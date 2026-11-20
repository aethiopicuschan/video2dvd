package video2dvd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
)

const (
	dvdImageSectorSize      = 2048
	dvdImageECCBlockSectors = 16
	isoPVDLBA               = 16
	isoTerminatorLBA        = 17
	udfVRSStart             = 18

	// Fixed layout of the UDF 1.02 writer.
	udfAnchorLBA         = 256
	udfMainVDSLBA        = 257
	dvdImagePartitionLBA = 291

	udfTagAnchor                 = 0x0002
	udfTagPartition              = 0x0005
	udfTagLogicalVolumeIntegrity = 0x0009
	udfTagFileSet                = 0x0100
	udfTagFileIdentifier         = 0x0101
	udfTagFileEntry              = 0x0105

	// A short allocation descriptor holds a 30-bit length, and every extent
	// but the last must be a whole number of blocks.
	udfMaxExtentBytes = 0x3ffff800
)

// DVDImageOptions configures a UDF 1.02 / ISO 9660 DVD-Video image.
type DVDImageOptions struct {
	// VolumeID is the ISO 9660 volume identifier. Empty selects VIDEO2DVD.
	VolumeID string
	// DiscCapacityBytes is the maximum image size. Zero selects DVD5 capacity.
	DiscCapacityBytes int64
}

// WriteDVDISO writes an authored VIDEO_TS directory as a DVD-Video UDF bridge
// image. It creates an ISO 9660 view and a UDF 1.02 view over the same file
// extents, and publishes outputISO only after the image has been completed.
func WriteDVDISO(ctx context.Context, videoTSDir, outputISO string) error {
	return WriteDVDISOWithOptions(ctx, videoTSDir, outputISO, DVDImageOptions{})
}

// WriteDVDISOWithOptions is WriteDVDISO with a volume identifier and capacity
// limit. The image is assembled in-process and does not invoke external tools.
func WriteDVDISOWithOptions(ctx context.Context, videoTSDir, outputISO string, options DVDImageOptions) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	capacity, volumeID, err := normalizeDVDImageOptions(options)
	if err != nil {
		return err
	}
	videoTSDir, err = filepath.Abs(videoTSDir)
	if err != nil {
		return fmt.Errorf("video2dvd: resolve VIDEO_TS path: %w", err)
	}
	outputISO, err = filepath.Abs(outputISO)
	if err != nil {
		return fmt.Errorf("video2dvd: resolve image path: %w", err)
	}
	if _, err := os.Lstat(outputISO); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, outputISO)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("video2dvd: inspect image path: %w", err)
	}
	if rel, err := filepath.Rel(videoTSDir, outputISO); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return errors.New("video2dvd: image output must not be inside VIDEO_TS")
	}
	parent := filepath.Dir(outputISO)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("video2dvd: inspect image parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("video2dvd: image parent %q is not a directory", parent)
	}

	sets, sizes, err := inspectDVDImageSource(videoTSDir)
	if err != nil {
		return err
	}
	usedBytes, err := directorySize(videoTSDir)
	if err != nil {
		return fmt.Errorf("video2dvd: measure VIDEO_TS directory: %w", err)
	}
	if usedBytes > capacity {
		return ErrDiscCapacity
	}
	stageRoot, err := os.MkdirTemp(parent, ".video2dvd-image-")
	if err != nil {
		return fmt.Errorf("video2dvd: create image staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	udfRoot := filepath.Join(stageRoot, "tree")
	if err := stageDVDImageTree(ctx, udfRoot, sets); err != nil {
		return err
	}
	imagePath := filepath.Join(stageRoot, "image.iso")
	image, err := os.OpenFile(imagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("video2dvd: create staged image: %w", err)
	}
	removeImage := true
	defer func() {
		_ = image.Close()
		if removeImage {
			_ = os.Remove(imagePath)
		}
	}()

	totalSectors, err := writeDVDImageContents(ctx, image, udfRoot, videoTSDir, sets, sizes, volumeID, capacity)
	if err != nil {
		return err
	}
	if err := image.Truncate(int64(totalSectors) * dvdImageSectorSize); err != nil {
		return fmt.Errorf("video2dvd: size DVD image: %w", err)
	}
	if err := image.Sync(); err != nil {
		return fmt.Errorf("video2dvd: sync DVD image: %w", err)
	}
	if err := image.Close(); err != nil {
		return fmt.Errorf("video2dvd: close DVD image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(imagePath, outputISO); err != nil {
		return fmt.Errorf("video2dvd: publish DVD image: %w", err)
	}
	removeImage = false
	return nil
}

func normalizeDVDImageOptions(options DVDImageOptions) (int64, string, error) {
	if options.DiscCapacityBytes < 0 {
		return 0, "", ErrInvalidProfile
	}
	capacity := options.DiscCapacityBytes
	if capacity == 0 {
		capacity = DVD5CapacityBytes
	}
	volumeID := strings.TrimSpace(options.VolumeID)
	if volumeID == "" {
		volumeID = "VIDEO2DVD"
	}
	volumeID = strings.ToUpper(volumeID)
	if len(volumeID) > 32 {
		return 0, "", errors.New("video2dvd: ISO volume identifier exceeds 32 bytes")
	}
	for _, r := range volumeID {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == ' ') {
			return 0, "", errors.New("video2dvd: ISO volume identifier may contain only ASCII letters, digits, underscore, and spaces")
		}
	}
	return capacity, volumeID, nil
}

// dvdImageFileSet lists the files of the VMG or of one title set in the
// order DVD-Video requires them on disc: IFO, menu VOB, title VOBs, BUP.
type dvdImageFileSet struct {
	ifo       string
	menuVOB   string // empty when the set has no menu VOB.
	titleVOBs []string
	bup       string
}

func (s dvdImageFileSet) names() []string {
	names := make([]string, 0, 3+len(s.titleVOBs))
	names = append(names, s.ifo)
	if s.menuVOB != "" {
		names = append(names, s.menuVOB)
	}
	names = append(names, s.titleVOBs...)
	return append(names, s.bup)
}

// inspectDVDImageSource returns the VMG file set followed by one file set per
// title, and the byte size of every file.
func inspectDVDImageSource(videoTS string) ([]dvdImageFileSet, map[string]int64, error) {
	entries, err := os.ReadDir(videoTS)
	if err != nil {
		return nil, nil, fmt.Errorf("video2dvd: inspect VIDEO_TS directory: %w", err)
	}
	sizes := make(map[string]int64, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, nil, fmt.Errorf("video2dvd: inspect %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, nil, fmt.Errorf("video2dvd: VIDEO_TS entry %q must be a non-empty regular file", entry.Name())
		}
		sizes[entry.Name()] = info.Size()
	}
	titleCount := 0
	for i := 1; i <= dvdMaxTitles; i++ {
		if _, ok := sizes[fmt.Sprintf("VTS_%02d_0.IFO", i)]; !ok {
			break
		}
		titleCount++
	}
	if titleCount == 0 {
		return nil, nil, errors.New("video2dvd: VIDEO_TS contains no title sets")
	}
	if err := validateDVDDirectory(videoTS, titleCount); err != nil {
		return nil, nil, err
	}
	sets := make([]dvdImageFileSet, 0, 1+titleCount)
	sets = append(sets, dvdImageFileSet{ifo: "VIDEO_TS.IFO", menuVOB: "VIDEO_TS.VOB", bup: "VIDEO_TS.BUP"})
	for i := 1; i <= titleCount; i++ {
		set := dvdImageFileSet{ifo: fmt.Sprintf("VTS_%02d_0.IFO", i), bup: fmt.Sprintf("VTS_%02d_0.BUP", i)}
		if name := fmt.Sprintf("VTS_%02d_0.VOB", i); sizes[name] != 0 {
			set.menuVOB = name
		}
		for part := 1; part <= dvdTitleVOBMaxFiles; part++ {
			name := fmt.Sprintf("VTS_%02d_%d.VOB", i, part)
			if sizes[name] == 0 {
				break
			}
			set.titleVOBs = append(set.titleVOBs, name)
		}
		sets = append(sets, set)
	}
	expected := 0
	for _, set := range sets {
		for _, name := range set.names() {
			size, ok := sizes[name]
			if !ok {
				return nil, nil, fmt.Errorf("video2dvd: VIDEO_TS is missing %s", name)
			}
			if size%dvdImageSectorSize != 0 {
				return nil, nil, fmt.Errorf("video2dvd: %s is not sector-aligned", name)
			}
			expected++
		}
	}
	if len(entries) != expected {
		return nil, nil, errors.New("video2dvd: image writer accepts the generated VIDEO_TS file layout only")
	}
	return sets, sizes, nil
}

// stageDVDImageTree creates the directory tree handed to the UDF writer. The
// files are empty placeholders: the writer then emits every UDF descriptor
// ahead of the data area, and the file extents are recorded afterwards so
// that each VMG/VTS file run is contiguous.
func stageDVDImageTree(ctx context.Context, root string, sets []dvdImageFileSet) error {
	if err := os.MkdirAll(filepath.Join(root, "VIDEO_TS"), 0o700); err != nil {
		return fmt.Errorf("video2dvd: create staged VIDEO_TS directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(root, "AUDIO_TS"), 0o700); err != nil {
		return fmt.Errorf("video2dvd: create staged AUDIO_TS directory: %w", err)
	}
	for _, set := range sets {
		for _, name := range set.names() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(root, "VIDEO_TS", name), nil, 0o600); err != nil {
				return fmt.Errorf("video2dvd: stage %s: %w", name, err)
			}
		}
	}
	return nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			total += int64(nw)
			if writeErr != nil {
				return total, writeErr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

type contextWriterAt struct {
	ctx context.Context
	w   io.WriterAt
}

func (w contextWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.WriteAt(p, off)
}

// layoutDVDImageFiles assigns absolute sectors to every file starting at
// first and returns the sector after the last file. Each file set is one run
// with no foreign sectors between its IFO and VOBs, because DVD-Video
// addresses VOB sectors relative to the IFO and reads the title VOB parts as
// one continuous stream. The BUP is moved to a later 16-sector ECC block than
// the end of its IFO so that one damaged block cannot destroy both copies.
func layoutDVDImageFiles(sets []dvdImageFileSet, sizes map[string]int64, first uint64) (map[string]udf.Location, uint64) {
	locations := make(map[string]udf.Location)
	next := first
	for _, set := range sets {
		ifoLast := next + sectorsForSize(sizes[set.ifo]) - 1
		for _, name := range set.names() {
			if name == set.bup && next/dvdImageECCBlockSectors == ifoLast/dvdImageECCBlockSectors {
				next = (ifoLast/dvdImageECCBlockSectors + 1) * dvdImageECCBlockSectors
			}
			locations[name] = udf.Location{Sector: next, Length: sizes[name]}
			next += sectorsForSize(sizes[name])
		}
	}
	return locations, next
}

func writeDVDImageContents(ctx context.Context, image *os.File, root, videoTS string, sets []dvdImageFileSet, sizes map[string]int64, volumeID string, capacity int64) (uint64, error) {
	result, err := udf.WriteTree(contextWriterAt{ctx: ctx, w: image}, root, udf.Options{VolumeID: volumeID, VRSStart: udfVRSStart})
	if err != nil {
		return 0, fmt.Errorf("video2dvd: write UDF 1.02 filesystem: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if result.TotalSectors <= dvdImagePartitionLBA+1 {
		return 0, errors.New("video2dvd: UDF writer reported an invalid metadata size")
	}
	// The placeholder tree has no file data, so the writer's last sector (its
	// backup anchor) is where the data area begins.
	firstDataSector := result.TotalSectors - 1
	locations, dataEnd := layoutDVDImageFiles(sets, sizes, firstDataSector)
	udfSectors := dataEnd + 1 // the backup anchor follows the partition.
	if udfSectors > math.MaxUint32 || udfSectors > math.MaxInt64/dvdImageSectorSize {
		return 0, errors.New("video2dvd: DVD image sector count overflows file size")
	}
	if int64(udfSectors)*dvdImageSectorSize > capacity {
		return 0, ErrDiscCapacity
	}
	fileEntries, err := locateUDFFileEntries(image)
	if err != nil {
		return 0, err
	}
	if err := writeAtFull(image, make([]byte, dvdImageSectorSize), int64(firstDataSector)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("video2dvd: clear UDF placeholder anchor: %w", err)
	}
	for _, set := range sets {
		for _, name := range set.names() {
			entrySector, ok := fileEntries[name]
			if !ok {
				return 0, fmt.Errorf("video2dvd: UDF writer did not create a file entry for %s", name)
			}
			loc := locations[name]
			if err := copyDVDImageFile(ctx, image, filepath.Join(videoTS, name), loc); err != nil {
				return 0, fmt.Errorf("video2dvd: write %s: %w", name, err)
			}
			if err := recordUDFFileExtent(image, entrySector, loc); err != nil {
				return 0, fmt.Errorf("video2dvd: record UDF extent for %s: %w", name, err)
			}
		}
	}
	if err := resizeUDFPartition(image, uint32(dataEnd-dvdImagePartitionLBA), dataEnd); err != nil {
		return 0, err
	}
	if err := validateUDFImage(image, sizes); err != nil {
		return 0, err
	}
	if err := patchDVDImageIFO(image, locations, sets); err != nil {
		return 0, err
	}
	return writeISO9660Bridge(image, udfSectors, locations, volumeID, capacity)
}

func copyDVDImageFile(ctx context.Context, image *os.File, source string, loc udf.Location) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	written, err := copyContext(ctx, io.NewOffsetWriter(image, int64(loc.Sector)*dvdImageSectorSize), io.LimitReader(in, loc.Length+1))
	if err != nil {
		return err
	}
	if written != loc.Length {
		return errors.New("file size changed while the image was written")
	}
	return nil
}

// locateUDFFileEntries returns the absolute sector of the UDF File Entry of
// every file in VIDEO_TS, keyed by file name.
func locateUDFFileEntries(image *os.File) (map[string]uint64, error) {
	fileSet, err := readUDFDescriptor(image, dvdImagePartitionLBA, udfTagFileSet)
	if err != nil {
		return nil, err
	}
	// The root directory ICB is a long allocation descriptor at offset 400.
	rootEntries, err := readUDFDirectory(image, dvdImagePartitionLBA+uint64(binary.LittleEndian.Uint32(fileSet[404:408])))
	if err != nil {
		return nil, err
	}
	videoTS, ok := rootEntries["VIDEO_TS"]
	if !ok {
		return nil, errors.New("video2dvd: UDF root directory has no VIDEO_TS entry")
	}
	return readUDFDirectory(image, videoTS)
}

func readUDFDescriptor(image *os.File, sector uint64, tag uint16) ([]byte, error) {
	descriptor := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(descriptor, int64(sector)*dvdImageSectorSize); err != nil {
		return nil, fmt.Errorf("video2dvd: read UDF descriptor at sector %d: %w", sector, err)
	}
	if got := binary.LittleEndian.Uint16(descriptor[0:2]); got != tag {
		return nil, fmt.Errorf("video2dvd: UDF descriptor at sector %d has tag %#x, want %#x", sector, got, tag)
	}
	return descriptor, nil
}

// readUDFDirectory parses the directory whose File Entry is at entrySector
// and returns the absolute File Entry sector of each child.
func readUDFDirectory(image *os.File, entrySector uint64) (map[string]uint64, error) {
	entry, err := readUDFDescriptor(image, entrySector, udfTagFileEntry)
	if err != nil {
		return nil, err
	}
	extent, err := udfAllocationOffset(entry)
	if err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(entry[172:176]) != 8 {
		return nil, errors.New("video2dvd: UDF directory is not recorded as one extent")
	}
	length := int64(binary.LittleEndian.Uint32(entry[extent:extent+4]) & 0x3fffffff)
	if length > 16*1024*1024 {
		return nil, errors.New("video2dvd: UDF directory is unexpectedly large")
	}
	data := make([]byte, length)
	dataSector := dvdImagePartitionLBA + uint64(binary.LittleEndian.Uint32(entry[extent+4:extent+8]))
	if _, err := image.ReadAt(data, int64(dataSector)*dvdImageSectorSize); err != nil {
		return nil, fmt.Errorf("video2dvd: read UDF directory: %w", err)
	}
	children := make(map[string]uint64)
	for offset := 0; offset+38 <= len(data); {
		if binary.LittleEndian.Uint16(data[offset:offset+2]) != udfTagFileIdentifier {
			return nil, errors.New("video2dvd: UDF directory contains an invalid file identifier")
		}
		identifierLength := int(data[offset+19])
		useLength := int(binary.LittleEndian.Uint16(data[offset+36 : offset+38]))
		nameStart := offset + 38 + useLength
		if nameStart+identifierLength > len(data) {
			return nil, errors.New("video2dvd: UDF file identifier overruns its directory")
		}
		// Generated names are ASCII, stored as one compression-ID byte
		// followed by 8-bit characters. The parent entry has no name.
		if data[offset+18]&0x08 == 0 && identifierLength > 1 && data[nameStart] == 8 {
			name := string(data[nameStart+1 : nameStart+identifierLength])
			children[name] = dvdImagePartitionLBA + uint64(binary.LittleEndian.Uint32(data[offset+24:offset+28]))
		}
		offset += (38 + useLength + identifierLength + 3) &^ 3
	}
	return children, nil
}

func udfAllocationOffset(fileEntry []byte) (int, error) {
	offset := 176 + int(binary.LittleEndian.Uint32(fileEntry[168:172]))
	if binary.LittleEndian.Uint16(fileEntry[34:36])&7 != 0 || offset+8 > len(fileEntry) {
		return 0, errors.New("video2dvd: UDF file entry does not use short allocation descriptors")
	}
	return offset, nil
}

// recordUDFFileExtent points a placeholder File Entry at the file data.
func recordUDFFileExtent(image *os.File, entrySector uint64, loc udf.Location) error {
	entry, err := readUDFDescriptor(image, entrySector, udfTagFileEntry)
	if err != nil {
		return err
	}
	offset, err := udfAllocationOffset(entry)
	if err != nil {
		return err
	}
	start := offset
	block := loc.Sector - dvdImagePartitionLBA
	for remaining := uint64(loc.Length); remaining > 0; {
		length := min(remaining, udfMaxExtentBytes)
		if offset+8 > len(entry) || block > math.MaxUint32 {
			return errors.New("file needs more extents than a UDF file entry can hold")
		}
		binary.LittleEndian.PutUint32(entry[offset:offset+4], uint32(length))
		binary.LittleEndian.PutUint32(entry[offset+4:offset+8], uint32(block))
		block += sectorsForSize(int64(length))
		remaining -= length
		offset += 8
	}
	binary.LittleEndian.PutUint64(entry[56:64], uint64(loc.Length))
	binary.LittleEndian.PutUint64(entry[64:72], sectorsForSize(loc.Length))
	binary.LittleEndian.PutUint32(entry[172:176], uint32(offset-start))
	binary.LittleEndian.PutUint16(entry[10:12], uint16(offset-16)) // descriptor CRC length.
	refreshUDFTag(entry)
	return writeAtFull(image, entry, int64(entrySector)*dvdImageSectorSize)
}

// resizeUDFPartition extends the partition over the recorded file data and
// writes the backup anchor in the sector that follows it.
func resizeUDFPartition(image *os.File, partitionBlocks uint32, backupAnchorSector uint64) error {
	patched := 0
	for sector := uint64(udfMainVDSLBA); sector < dvdImagePartitionLBA; sector++ {
		descriptor := make([]byte, dvdImageSectorSize)
		if _, err := image.ReadAt(descriptor, int64(sector)*dvdImageSectorSize); err != nil {
			return fmt.Errorf("video2dvd: read UDF volume descriptor: %w", err)
		}
		switch binary.LittleEndian.Uint16(descriptor[0:2]) {
		case udfTagPartition:
			binary.LittleEndian.PutUint32(descriptor[192:196], partitionBlocks)
		case udfTagLogicalVolumeIntegrity:
			binary.LittleEndian.PutUint32(descriptor[84:88], partitionBlocks)
		default:
			continue
		}
		refreshUDFTag(descriptor)
		if err := writeAtFull(image, descriptor, int64(sector)*dvdImageSectorSize); err != nil {
			return fmt.Errorf("video2dvd: update UDF partition size: %w", err)
		}
		patched++
	}
	// Main and reserve partition descriptors plus one integrity descriptor.
	if patched != 3 {
		return fmt.Errorf("video2dvd: updated %d UDF partition size fields, want 3", patched)
	}
	anchor, err := readUDFDescriptor(image, udfAnchorLBA, udfTagAnchor)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(anchor[12:16], uint32(backupAnchorSector))
	refreshUDFTag(anchor)
	if err := writeAtFull(image, anchor, int64(backupAnchorSector)*dvdImageSectorSize); err != nil {
		return fmt.Errorf("video2dvd: write UDF backup anchor: %w", err)
	}
	return nil
}

func refreshUDFTag(descriptor []byte) {
	crcLength := int(binary.LittleEndian.Uint16(descriptor[10:12]))
	if crcLength > len(descriptor)-16 {
		return
	}
	binary.LittleEndian.PutUint16(descriptor[8:10], crcCCITT(descriptor[16:16+crcLength]))
	descriptor[4] = 0
	var checksum byte
	for i := range 16 {
		checksum += descriptor[i]
	}
	descriptor[4] = checksum
}

func crcCCITT(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func validateUDFImage(image *os.File, sizes map[string]int64) error {
	volume, err := udf.Read(image)
	if err != nil {
		return fmt.Errorf("video2dvd: validate UDF filesystem: %w", err)
	}
	root, err := volume.ReadDir(nil)
	if err != nil {
		return fmt.Errorf("video2dvd: read UDF root directory: %w", err)
	}
	if len(root) != 2 || root[0].Name != "AUDIO_TS" || root[1].Name != "VIDEO_TS" {
		return errors.New("video2dvd: generated UDF root directory is invalid")
	}
	videoEntries, err := volume.ReadDir([]string{"VIDEO_TS"})
	if err != nil {
		return fmt.Errorf("video2dvd: read UDF VIDEO_TS directory: %w", err)
	}
	if len(videoEntries) != len(sizes) {
		return fmt.Errorf("video2dvd: UDF VIDEO_TS has %d entries, want %d", len(videoEntries), len(sizes))
	}
	for _, entry := range videoEntries {
		if size, ok := sizes[entry.Name]; !ok || entry.IsDir || entry.Size != size {
			return fmt.Errorf("video2dvd: unexpected UDF VIDEO_TS entry %q", entry.Name)
		}
	}
	return nil
}

// patchDVDImageIFO rewrites the IFO/BUP sector references that depend on the
// image layout: the last sector of each VMG/VTS, the menu and title VOB start
// sectors, and the title-set start sectors in the VMG title table.
func patchDVDImageIFO(image *os.File, locations map[string]udf.Location, sets []dvdImageFileSet) error {
	if len(sets) < 2 {
		return errors.New("video2dvd: DVD image has no title sets")
	}
	titleCount := len(sets) - 1
	vmgIFO, vmgBUP := locations[sets[0].ifo], locations[sets[0].bup]
	vmgHeader, err := patchIFOHeader(image, locations, sets[0])
	if err != nil {
		return fmt.Errorf("video2dvd: patch VMGI sector addresses: %w", err)
	}
	ttPointer := binary.BigEndian.Uint32(vmgHeader[0x00C4:0x00C8])
	ttOffset := int64(ttPointer) * dvdImageSectorSize
	if ttOffset+dvdImageSectorSize > vmgIFO.Length {
		return errors.New("video2dvd: VMGI title table exceeds IFO extent")
	}
	table := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(table, int64(vmgIFO.Sector)*dvdImageSectorSize+ttOffset); err != nil {
		return fmt.Errorf("video2dvd: read VMGI title table: %w", err)
	}
	if binary.BigEndian.Uint16(table[0:2]) != uint16(titleCount) {
		return errors.New("video2dvd: VMGI title count does not match VIDEO_TS")
	}
	for i, set := range sets[1:] {
		if _, err := patchIFOHeader(image, locations, set); err != nil {
			return fmt.Errorf("video2dvd: patch title set %d sector addresses: %w", i+1, err)
		}
		entry := 8 + i*12
		if entry+12 > len(table) {
			return errors.New("video2dvd: VMGI title table is too large")
		}
		// Title-set start sectors are relative to VIDEO_TS.IFO, like every
		// other IFO sector reference.
		if locations[set.ifo].Sector <= vmgIFO.Sector {
			return errors.New("video2dvd: title set precedes the video manager in the image")
		}
		binary.BigEndian.PutUint32(table[entry+8:entry+12], uint32(locations[set.ifo].Sector-vmgIFO.Sector))
	}
	for _, loc := range []udf.Location{vmgIFO, vmgBUP} {
		if err := writeAtFull(image, table, int64(loc.Sector)*dvdImageSectorSize+ttOffset); err != nil {
			return fmt.Errorf("video2dvd: update VMGI title table: %w", err)
		}
	}
	return nil
}

// patchIFOHeader adjusts the last-sector and VOB start-sector fields of one
// IFO and its BUP from the image extents and returns the updated first sector.
func patchIFOHeader(image *os.File, locations map[string]udf.Location, set dvdImageFileSet) ([]byte, error) {
	ifo, bup := locations[set.ifo], locations[set.bup]
	if ifo.Length < dvdImageSectorSize || bup.Length != ifo.Length || bup.Sector <= ifo.Sector {
		return nil, errors.New("IFO/BUP extents are inconsistent")
	}
	header := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(header, int64(ifo.Sector)*dvdImageSectorSize); err != nil {
		return nil, fmt.Errorf("read IFO header at sector %d: %w", ifo.Sector, err)
	}
	last := bup.Sector + sectorsForSize(bup.Length) - 1 - ifo.Sector
	if ifo.Sector > math.MaxUint32 || last > math.MaxUint32 {
		return nil, errors.New("IFO sector address exceeds 32 bits")
	}
	binary.BigEndian.PutUint32(header[0x000C:0x0010], uint32(last))
	// 0xC0 is VMGM_VOBS/VTSM_VOBS; zero means the set has no menu VOB.
	if (binary.BigEndian.Uint32(header[0x00C0:0x00C4]) != 0) != (set.menuVOB != "") {
		return nil, errors.New("IFO menu VOB pointer does not match the menu VOB file")
	}
	if set.menuVOB != "" {
		binary.BigEndian.PutUint32(header[0x00C0:0x00C4], uint32(locations[set.menuVOB].Sector-ifo.Sector))
	}
	// 0xC4 is VTSTT_VOBS in a VTSI. The VMGI has no title VOBs and keeps its
	// TT_SRPT pointer there.
	if len(set.titleVOBs) != 0 {
		next := locations[set.titleVOBs[0]].Sector
		binary.BigEndian.PutUint32(header[0x00C4:0x00C8], uint32(next-ifo.Sector))
		for _, name := range set.titleVOBs {
			if locations[name].Sector != next {
				return nil, errors.New("title VOB files are not contiguous")
			}
			next += sectorsForSize(locations[name].Length)
		}
	}
	for _, loc := range []udf.Location{ifo, bup} {
		if err := writeAtFull(image, header, int64(loc.Sector)*dvdImageSectorSize); err != nil {
			return nil, err
		}
	}
	return header, nil
}

func sectorsForSize(size int64) uint64 {
	if size <= 0 {
		return 0
	}
	return uint64((size + dvdImageSectorSize - 1) / dvdImageSectorSize)
}

type isoPathEntry struct {
	name   []byte
	extent uint32
	parent uint16
}

func writeISO9660Bridge(image *os.File, udfSectors uint64, locations map[string]udf.Location, volumeID string, capacity int64) (uint64, error) {
	if udfSectors == 0 || udfSectors > math.MaxUint32 {
		return 0, errors.New("video2dvd: ISO 9660 volume exceeds 32-bit sector count")
	}
	// The ECMA-119 metadata is appended after the UDF partition. This keeps
	// DVD-Video's reserved sectors (including 21-31 and 66-255) at zero.
	if udfSectors > math.MaxUint32-4 {
		return 0, errors.New("video2dvd: ISO metadata extent exceeds 32-bit sector addresses")
	}
	baseLBA := uint32(udfSectors - 1) // reuse the old backup-anchor sector.
	anchor := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(anchor, int64(baseLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("video2dvd: read UDF backup anchor: %w", err)
	}
	lPathLBA, mPathLBA := baseLBA, baseLBA+1
	rootLBA, audioLBA, videoLBA := baseLBA+2, baseLBA+3, baseLBA+4
	entries := make([]isoPathEntry, 0, 3)
	entries = append(entries,
		isoPathEntry{name: []byte{0}, extent: rootLBA, parent: 1},
		isoPathEntry{name: []byte("AUDIO_TS"), extent: audioLBA, parent: 1},
		isoPathEntry{name: []byte("VIDEO_TS"), extent: videoLBA, parent: 1},
	)
	lPath := buildISOPathTable(entries, binary.LittleEndian)
	mPath := buildISOPathTable(entries, binary.BigEndian)
	if len(lPath) > dvdImageSectorSize || len(mPath) > dvdImageSectorSize {
		return 0, errors.New("video2dvd: ISO path table exceeds one sector")
	}

	videoEntries := make([]string, 0, len(locations))
	for name := range locations {
		videoEntries = append(videoEntries, name)
	}
	sort.Strings(videoEntries)
	videoRecords := make([][]byte, 0, len(videoEntries)+2)
	videoRecords = append(videoRecords,
		isoDirectoryRecord([]byte{0}, videoLBA, dvdImageSectorSize, 0x02),
		isoDirectoryRecord([]byte{1}, rootLBA, dvdImageSectorSize, 0x02),
	)
	for _, name := range videoEntries {
		loc := locations[name]
		if loc.Length <= 0 || loc.Sector > math.MaxUint32 {
			return 0, fmt.Errorf("video2dvd: invalid ISO extent for %s", name)
		}
		records, err := isoFileRecords(name+";1", loc)
		if err != nil {
			return 0, fmt.Errorf("video2dvd: create ISO records for %s: %w", name, err)
		}
		videoRecords = append(videoRecords, records...)
	}
	videoData, err := buildISODirectory(videoRecords...)
	if err != nil {
		return 0, err
	}
	putISOBoth32(videoData[10:18], uint32(len(videoData)))
	videoSectors := len(videoData) / dvdImageSectorSize
	newAnchorLBA := uint64(videoLBA) + uint64(videoSectors)
	imageSectors := newAnchorLBA + 1
	if imageSectors > math.MaxUint32 || imageSectors > math.MaxInt64/dvdImageSectorSize {
		return 0, errors.New("video2dvd: ISO 9660 volume exceeds supported sector count")
	}
	imageBytes := int64(imageSectors) * dvdImageSectorSize
	if imageBytes > capacity {
		return 0, fmt.Errorf("%w: image is %d bytes, limit is %d", ErrDiscCapacity, imageBytes, capacity)
	}
	rootData, err := buildISODirectory(
		isoDirectoryRecord([]byte{0}, rootLBA, dvdImageSectorSize, 0x02),
		isoDirectoryRecord([]byte{1}, rootLBA, dvdImageSectorSize, 0x02),
		isoDirectoryRecord([]byte("AUDIO_TS"), audioLBA, dvdImageSectorSize, 0x02),
		isoDirectoryRecord([]byte("VIDEO_TS"), videoLBA, uint32(len(videoData)), 0x02),
	)
	if err != nil {
		return 0, err
	}
	audioData, err := buildISODirectory(
		isoDirectoryRecord([]byte{0}, audioLBA, dvdImageSectorSize, 0x02),
		isoDirectoryRecord([]byte{1}, rootLBA, dvdImageSectorSize, 0x02),
	)
	if err != nil {
		return 0, err
	}
	for _, table := range []struct {
		lba  uint32
		data []byte
		name string
	}{{lPathLBA, lPath, "little-endian"}, {mPathLBA, mPath, "big-endian"}} {
		if err := writeAtFull(image, padSector(table.data), int64(table.lba)*dvdImageSectorSize); err != nil {
			return 0, fmt.Errorf("video2dvd: write ISO %s path table: %w", table.name, err)
		}
	}
	for _, dir := range []struct {
		lba  uint32
		data []byte
	}{{rootLBA, rootData}, {audioLBA, audioData}, {videoLBA, videoData}} {
		if err := writeAtFull(image, dir.data, int64(dir.lba)*dvdImageSectorSize); err != nil {
			return 0, fmt.Errorf("video2dvd: write ISO directory at sector %d: %w", dir.lba, err)
		}
	}

	pvd := buildISOPVD(imageSectors, uint32(len(lPath)), lPathLBA, mPathLBA, rootLBA, volumeID, uint32(len(rootData)))
	if err := writeAtFull(image, pvd, int64(isoPVDLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("video2dvd: write ISO primary volume descriptor: %w", err)
	}
	terminator := make([]byte, dvdImageSectorSize)
	terminator[0] = 255
	copy(terminator[1:6], "CD001")
	terminator[6] = 1
	if err := writeAtFull(image, terminator, int64(isoTerminatorLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("video2dvd: write ISO volume descriptor terminator: %w", err)
	}
	binary.LittleEndian.PutUint32(anchor[12:16], uint32(newAnchorLBA))
	refreshUDFTag(anchor)
	if err := writeAtFull(image, anchor, int64(newAnchorLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("video2dvd: write final UDF backup anchor: %w", err)
	}
	return imageSectors, nil
}

func isoFileRecords(identifier string, loc udf.Location) ([][]byte, error) {
	const maxExtentBytes = uint64(math.MaxUint32) &^ uint64(dvdImageSectorSize-1)
	remaining := uint64(loc.Length)
	sector := loc.Sector
	var records [][]byte
	for remaining > 0 {
		length := min(remaining, maxExtentBytes)
		flags := byte(0)
		if remaining > length {
			flags = 0x80 // ISO 9660 multi-extent file record.
		}
		if sector > math.MaxUint32 || length > math.MaxUint32 {
			return nil, errors.New("extent exceeds ISO 9660 32-bit address space")
		}
		records = append(records, isoDirectoryRecord([]byte(identifier), uint32(sector), uint32(length), flags))
		sector += uint64((length + dvdImageSectorSize - 1) / dvdImageSectorSize)
		remaining -= length
	}
	return records, nil
}

func buildISODirectory(records ...[]byte) ([]byte, error) {
	var data []byte
	for _, record := range records {
		if len(record) == 0 || len(record) > 255 {
			return nil, errors.New("video2dvd: ISO directory record has invalid length")
		}
		withinSector := len(data) % dvdImageSectorSize
		if withinSector+len(record) > dvdImageSectorSize {
			data = append(data, make([]byte, dvdImageSectorSize-withinSector)...)
		}
		data = append(data, record...)
	}
	if padding := len(data) % dvdImageSectorSize; padding != 0 {
		data = append(data, make([]byte, dvdImageSectorSize-padding)...)
	}
	if len(data) == 0 {
		return nil, errors.New("video2dvd: empty ISO directory")
	}
	return data, nil
}

func isoDirectoryRecord(identifier []byte, extent, size uint32, flags byte) []byte {
	length := 33 + len(identifier)
	if len(identifier)%2 == 0 {
		length++
	}
	if length > 255 {
		return nil
	}
	record := make([]byte, length)
	record[0] = byte(length)
	putISOBoth32(record[2:10], extent)
	putISOBoth32(record[10:18], size)
	now := time.Now().UTC()
	record[18] = byte(now.Year() - 1900)
	record[19] = byte(now.Month())
	record[20] = byte(now.Day())
	record[21] = byte(now.Hour())
	record[22] = byte(now.Minute())
	record[23] = byte(now.Second())
	record[24] = 0
	record[25] = flags
	putISOBoth16(record[28:32], 1)
	record[32] = byte(len(identifier))
	copy(record[33:], identifier)
	return record
}

func buildISOPathTable(entries []isoPathEntry, order binary.ByteOrder) []byte {
	var data []byte
	for _, entry := range entries {
		length := 8 + len(entry.name)
		if len(entry.name)%2 != 0 {
			length++
		}
		record := make([]byte, length)
		record[0] = byte(len(entry.name))
		order.PutUint32(record[2:6], entry.extent)
		order.PutUint16(record[6:8], entry.parent)
		copy(record[8:], entry.name)
		data = append(data, record...)
	}
	return data
}

func buildISOPVD(totalSectors uint64, pathTableSize, lPath, mPath, rootExtent uint32, volumeID string, rootSize uint32) []byte {
	pvd := make([]byte, dvdImageSectorSize)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	for _, field := range [][2]int{{8, 40}, {72, 80}, {190, 318}, {318, 446}, {446, 574}, {574, 702}, {702, 739}, {739, 776}, {776, 813}} {
		for i := field[0]; i < field[1]; i++ {
			pvd[i] = ' '
		}
	}
	for i := 40; i < 72; i++ {
		pvd[i] = ' '
	}
	copy(pvd[40:72], volumeID)
	putISOBoth32(pvd[80:88], uint32(totalSectors))
	putISOBoth16(pvd[120:124], 1)
	putISOBoth16(pvd[124:128], 1)
	putISOBoth16(pvd[128:132], dvdImageSectorSize)
	putISOBoth32(pvd[132:140], pathTableSize)
	binary.LittleEndian.PutUint32(pvd[140:144], lPath)
	binary.LittleEndian.PutUint32(pvd[144:148], 0)
	binary.BigEndian.PutUint32(pvd[148:152], mPath)
	binary.BigEndian.PutUint32(pvd[152:156], 0)
	copy(pvd[156:190], isoDirectoryRecord([]byte{0}, rootExtent, rootSize, 0x02))
	now := time.Now().UTC().Format("20060102150405") + "00"
	for _, field := range [][2]int{{813, 830}, {830, 847}, {847, 864}, {864, 881}} {
		copy(pvd[field[0]:field[0]+16], now)
		pvd[field[1]-1] = 0 // GMT offset in 15-minute units; the digits are UTC.
	}
	pvd[881] = 1
	return pvd
}

func putISOBoth16(dst []byte, value uint16) {
	binary.LittleEndian.PutUint16(dst[:2], value)
	binary.BigEndian.PutUint16(dst[2:4], value)
}

func putISOBoth32(dst []byte, value uint32) {
	binary.LittleEndian.PutUint32(dst[:4], value)
	binary.BigEndian.PutUint32(dst[4:8], value)
}

func writeAtFull(w io.WriterAt, data []byte, off int64) error {
	n, err := w.WriteAt(data, off)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func padSector(data []byte) []byte {
	sector := make([]byte, dvdImageSectorSize)
	copy(sector, data)
	return sector
}
