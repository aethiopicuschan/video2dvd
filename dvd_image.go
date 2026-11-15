package mp4todvd

import (
	"bytes"
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
	dvdImageSectorSize   = 2048
	dvdImagePartitionLBA = 291 // UDF 1.02 writer's fixed partition start.
	isoPVDLBA            = 16
	isoTerminatorLBA     = 17
	udfVRSStart          = 18
)

// DVDImageOptions configures a UDF 1.02 / ISO 9660 DVD-Video image.
type DVDImageOptions struct {
	// VolumeID is the ISO 9660 volume identifier. Empty selects MP4TODVD.
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
		return errors.New("mp4todvd: context must not be nil")
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
		return fmt.Errorf("mp4todvd: resolve VIDEO_TS path: %w", err)
	}
	outputISO, err = filepath.Abs(outputISO)
	if err != nil {
		return fmt.Errorf("mp4todvd: resolve image path: %w", err)
	}
	if _, err := os.Lstat(outputISO); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, outputISO)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mp4todvd: inspect image path: %w", err)
	}
	if rel, err := filepath.Rel(videoTSDir, outputISO); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return errors.New("mp4todvd: image output must not be inside VIDEO_TS")
	}
	parent := filepath.Dir(outputISO)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("mp4todvd: inspect image parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("mp4todvd: image parent %q is not a directory", parent)
	}

	files, titleCount, err := inspectDVDImageSource(videoTSDir)
	if err != nil {
		return err
	}
	usedBytes, err := directorySize(videoTSDir)
	if err != nil {
		return fmt.Errorf("mp4todvd: measure VIDEO_TS directory: %w", err)
	}
	if usedBytes > capacity {
		return ErrDiscCapacity
	}
	stageRoot, err := os.MkdirTemp(parent, ".mp4todvd-image-")
	if err != nil {
		return fmt.Errorf("mp4todvd: create image staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	udfRoot := filepath.Join(stageRoot, "tree")
	if err := stageDVDImageTree(ctx, udfRoot, videoTSDir, files); err != nil {
		return err
	}
	imagePath := filepath.Join(stageRoot, "image.iso")
	image, err := os.OpenFile(imagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("mp4todvd: create staged image: %w", err)
	}
	removeImage := true
	defer func() {
		_ = image.Close()
		if removeImage {
			_ = os.Remove(imagePath)
		}
	}()

	totalSectors, err := writeDVDImageContents(ctx, image, udfRoot, files, titleCount, volumeID, capacity)
	if err != nil {
		return err
	}
	if err := image.Truncate(int64(totalSectors) * dvdImageSectorSize); err != nil {
		return fmt.Errorf("mp4todvd: size DVD image: %w", err)
	}
	if err := image.Sync(); err != nil {
		return fmt.Errorf("mp4todvd: sync DVD image: %w", err)
	}
	if err := image.Close(); err != nil {
		return fmt.Errorf("mp4todvd: close DVD image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(imagePath, outputISO); err != nil {
		return fmt.Errorf("mp4todvd: publish DVD image: %w", err)
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
		volumeID = "MP4TODVD"
	}
	volumeID = strings.ToUpper(volumeID)
	if len(volumeID) > 32 {
		return 0, "", errors.New("mp4todvd: ISO volume identifier exceeds 32 bytes")
	}
	for _, r := range volumeID {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == ' ') {
			return 0, "", errors.New("mp4todvd: ISO volume identifier may contain only ASCII letters, digits, underscore, and spaces")
		}
	}
	return capacity, volumeID, nil
}

type dvdImageFile struct {
	name     string
	fakeName string
}

func inspectDVDImageSource(videoTS string) ([]dvdImageFile, int, error) {
	entries, err := os.ReadDir(videoTS)
	if err != nil {
		return nil, 0, fmt.Errorf("mp4todvd: inspect VIDEO_TS directory: %w", err)
	}
	entryNames := make(map[string]bool, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, 0, fmt.Errorf("mp4todvd: inspect %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, 0, fmt.Errorf("mp4todvd: VIDEO_TS entry %q must be a non-empty regular file", entry.Name())
		}
		entryNames[entry.Name()] = true
	}
	titleCount := 0
	for i := 1; i <= dvdMaxTitles; i++ {
		name := fmt.Sprintf("VTS_%02d_0.IFO", i)
		if !entryNames[name] {
			break
		}
		titleCount++
	}
	if titleCount == 0 {
		return nil, 0, errors.New("mp4todvd: VIDEO_TS contains no title sets")
	}
	if err := validateDVDDirectory(videoTS, titleCount); err != nil {
		return nil, 0, err
	}
	expected := make([]dvdImageFile, 0, 2+3*titleCount)
	expected = append(expected,
		dvdImageFile{name: "VIDEO_TS.IFO", fakeName: "VIDEO_TS.AAA"},
		dvdImageFile{name: "VIDEO_TS.BUP", fakeName: "VIDEO_TS.ZZZ"},
	)
	for i := 1; i <= titleCount; i++ {
		expected = append(expected,
			dvdImageFile{name: fmt.Sprintf("VTS_%02d_0.IFO", i), fakeName: fmt.Sprintf("VTS_%02d_0.AAA", i)},
			dvdImageFile{name: fmt.Sprintf("VTS_%02d_1.VOB", i), fakeName: fmt.Sprintf("VTS_%02d_0.MMM", i)},
			dvdImageFile{name: fmt.Sprintf("VTS_%02d_0.BUP", i), fakeName: fmt.Sprintf("VTS_%02d_0.ZZZ", i)},
		)
	}
	if len(entries) != len(expected) {
		return nil, 0, errors.New("mp4todvd: image writer accepts the current menu-less VIDEO_TS file layout only")
	}
	for _, file := range expected {
		if !entryNames[file.name] {
			return nil, 0, fmt.Errorf("mp4todvd: VIDEO_TS is missing %s", file.name)
		}
	}
	return expected, titleCount, nil
}

func stageDVDImageTree(ctx context.Context, root, videoTS string, files []dvdImageFile) error {
	if err := os.MkdirAll(filepath.Join(root, "VIDEO_TS"), 0o700); err != nil {
		return fmt.Errorf("mp4todvd: create staged VIDEO_TS directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(root, "AUDIO_TS"), 0o700); err != nil {
		return fmt.Errorf("mp4todvd: create staged AUDIO_TS directory: %w", err)
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := filepath.Join(videoTS, file.name)
		target := filepath.Join(root, "VIDEO_TS", file.fakeName)
		if err := os.Link(source, target); err == nil {
			continue
		}
		if err := copyFileContext(ctx, source, target); err != nil {
			return fmt.Errorf("mp4todvd: stage %s: %w", file.name, err)
		}
	}
	return nil
}

func copyFileContext(ctx context.Context, source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := copyContext(ctx, out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return closeErr
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

func writeDVDImageContents(ctx context.Context, image *os.File, root string, files []dvdImageFile, titleCount int, volumeID string, capacity int64) (uint64, error) {
	result, err := udf.WriteTree(contextWriterAt{ctx: ctx, w: image}, root, udf.Options{VolumeID: volumeID, VRSStart: udfVRSStart})
	if err != nil {
		return 0, fmt.Errorf("mp4todvd: write UDF 1.02 filesystem: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if result.TotalSectors == 0 || result.TotalSectors > math.MaxInt64/dvdImageSectorSize {
		return 0, errors.New("mp4todvd: DVD image sector count overflows file size")
	}
	imageBytes := int64(result.TotalSectors) * dvdImageSectorSize
	if imageBytes > capacity {
		return 0, ErrDiscCapacity
	}
	locations := make(map[string]udf.Location, len(files))
	minDataSector := uint64(math.MaxUint64)
	for i := range files {
		key := "VIDEO_TS/" + files[i].fakeName
		loc, ok := result.Files[key]
		if !ok {
			return 0, fmt.Errorf("mp4todvd: UDF writer did not report extent for %s", files[i].name)
		}
		locations[files[i].name] = loc
		if loc.Sector < minDataSector {
			minDataSector = loc.Sector
		}
	}
	if err := patchUDFIdentifiers(image, minDataSector, files); err != nil {
		return 0, err
	}
	if err := validateUDFImage(image, files, titleCount); err != nil {
		return 0, err
	}
	if err := patchDVDImageIFO(image, locations, titleCount); err != nil {
		return 0, err
	}
	imageSectors, err := writeISO9660Bridge(image, result.TotalSectors, locations, files, volumeID, capacity)
	if err != nil {
		return 0, err
	}
	return imageSectors, nil
}

func patchUDFIdentifiers(image *os.File, firstFileSector uint64, files []dvdImageFile) error {
	if firstFileSector <= dvdImagePartitionLBA {
		return errors.New("mp4todvd: invalid UDF file-data location")
	}
	start := int64(dvdImagePartitionLBA) * dvdImageSectorSize
	end := int64(firstFileSector) * dvdImageSectorSize
	if end-start > 64*1024*1024 {
		return errors.New("mp4todvd: UDF metadata area is unexpectedly large")
	}
	metadata := make([]byte, end-start)
	if _, err := image.ReadAt(metadata, start); err != nil {
		return fmt.Errorf("mp4todvd: read UDF metadata: %w", err)
	}
	for _, file := range files {
		oldID := append([]byte{8}, []byte(file.fakeName)...)
		newID := []byte(file.name)
		if len(oldID) != len(newID)+1 {
			return fmt.Errorf("mp4todvd: internal UDF identifier length mismatch for %s", file.name)
		}
		searchAt, replacements := 0, 0
		for searchAt < len(metadata) {
			rel := bytes.Index(metadata[searchAt:], oldID)
			if rel < 0 {
				break
			}
			idAt := searchAt + rel
			fidAt := idAt - 38
			if fidAt >= 0 && fidAt+38 <= len(metadata) && binary.LittleEndian.Uint16(metadata[fidAt:fidAt+2]) == 0x0101 &&
				metadata[fidAt+19] == byte(len(oldID)) && binary.LittleEndian.Uint16(metadata[fidAt+36:fidAt+38]) == 0 &&
				idAt+len(oldID) <= len(metadata) {
				fidLen := (38 + int(metadata[fidAt+19]) + 3) &^ 3
				if fidAt+fidLen <= len(metadata) {
					copy(metadata[idAt+1:idAt+len(oldID)], newID)
					refreshUDFTag(metadata[fidAt : fidAt+fidLen])
					replacements++
				}
			}
			searchAt = idAt + len(oldID)
		}
		if replacements != 1 {
			return fmt.Errorf("mp4todvd: expected one UDF directory entry for %s, found %d", file.name, replacements)
		}
	}
	if err := writeAtFull(image, metadata, start); err != nil {
		return fmt.Errorf("mp4todvd: update UDF directory entries: %w", err)
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

func validateUDFImage(image *os.File, files []dvdImageFile, titleCount int) error {
	volume, err := udf.Read(image)
	if err != nil {
		return fmt.Errorf("mp4todvd: validate UDF filesystem: %w", err)
	}
	root, err := volume.ReadDir(nil)
	if err != nil {
		return fmt.Errorf("mp4todvd: read UDF root directory: %w", err)
	}
	if len(root) != 2 || root[0].Name != "AUDIO_TS" || root[1].Name != "VIDEO_TS" {
		return errors.New("mp4todvd: generated UDF root directory is invalid")
	}
	videoEntries, err := volume.ReadDir([]string{"VIDEO_TS"})
	if err != nil {
		return fmt.Errorf("mp4todvd: read UDF VIDEO_TS directory: %w", err)
	}
	if len(videoEntries) != len(files) {
		return fmt.Errorf("mp4todvd: UDF VIDEO_TS has %d entries, want %d", len(videoEntries), len(files))
	}
	want := make(map[string]bool, len(files))
	for _, file := range files {
		want[file.name] = true
	}
	for _, entry := range videoEntries {
		if entry.IsDir || !want[entry.Name] {
			return fmt.Errorf("mp4todvd: unexpected UDF VIDEO_TS entry %q", entry.Name)
		}
		delete(want, entry.Name)
	}
	if len(want) != 0 || titleCount == 0 {
		return errors.New("mp4todvd: generated UDF VIDEO_TS directory is incomplete")
	}
	return nil
}

func patchDVDImageIFO(image *os.File, locations map[string]udf.Location, titleCount int) error {
	vmgIFO, ok := locations["VIDEO_TS.IFO"]
	if !ok {
		return errors.New("mp4todvd: VMGI extent is missing")
	}
	vmgBUP := locations["VIDEO_TS.BUP"]
	vmgBytes, err := patchIFOHeader(image, vmgIFO, vmgBUP)
	if err != nil {
		return fmt.Errorf("mp4todvd: patch VMGI sector addresses: %w", err)
	}
	ttPointer := binary.BigEndian.Uint32(vmgBytes[0x00C4:0x00C8])
	ttOffset := int64(ttPointer) * dvdImageSectorSize
	if ttOffset+dvdImageSectorSize > vmgIFO.Length {
		return errors.New("mp4todvd: VMGI title table exceeds IFO extent")
	}
	table := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(table, int64(vmgIFO.Sector)*dvdImageSectorSize+ttOffset); err != nil {
		return fmt.Errorf("mp4todvd: read VMGI title table: %w", err)
	}
	if binary.BigEndian.Uint16(table[0:2]) != uint16(titleCount) {
		return errors.New("mp4todvd: VMGI title count does not match VIDEO_TS")
	}
	for i := range titleCount {
		stem := fmt.Sprintf("VTS_%02d", i+1)
		ifo := locations[stem+"_0.IFO"]
		vob := locations[stem+"_1.VOB"]
		bup := locations[stem+"_0.BUP"]
		if ifo.Length == 0 || vob.Length == 0 || bup.Length != ifo.Length {
			return fmt.Errorf("mp4todvd: title set %d has inconsistent file extents", i+1)
		}
		if !(ifo.Sector < vob.Sector && vob.Sector < bup.Sector) {
			return fmt.Errorf("mp4todvd: title set %d files are not in DVD-Video sector order", i+1)
		}
		start := ifo.Sector
		end := bup.Sector + uint64(sectorsForSize(bup.Length)) - 1
		if start > math.MaxUint32 || end-start > math.MaxUint32 || vob.Sector-start > math.MaxUint32 {
			return fmt.Errorf("mp4todvd: title set %d sector address exceeds 32 bits", i+1)
		}
		titleHeader, err := readImageHeader(image, ifo)
		if err != nil {
			return err
		}
		binary.BigEndian.PutUint32(titleHeader[0x000C:0x0010], uint32(end-start))
		binary.BigEndian.PutUint32(titleHeader[0x00C4:0x00C8], uint32(vob.Sector-start))
		if err := writeImageHeader(image, ifo, bup, titleHeader); err != nil {
			return fmt.Errorf("mp4todvd: patch title set %d sector addresses: %w", i+1, err)
		}
		entry := 8 + i*12
		if entry+12 > len(table) {
			return errors.New("mp4todvd: VMGI title table is too large")
		}
		binary.BigEndian.PutUint32(table[entry+8:entry+12], uint32(start))
	}
	for _, loc := range []udf.Location{vmgIFO, vmgBUP} {
		if err := writeAtFull(image, table, int64(loc.Sector)*dvdImageSectorSize+ttOffset); err != nil {
			return fmt.Errorf("mp4todvd: update VMGI title table: %w", err)
		}
	}
	return nil
}

// patchIFOHeader adjusts an IFO/BUP last-sector field from the actual image
// extents and returns the updated first sector.
func patchIFOHeader(image *os.File, ifo, bup udf.Location) ([]byte, error) {
	if ifo.Length < dvdImageSectorSize || bup.Length != ifo.Length || bup.Sector <= ifo.Sector {
		return nil, errors.New("IFO/BUP extents are inconsistent")
	}
	header, err := readImageHeader(image, ifo)
	if err != nil {
		return nil, err
	}
	last := bup.Sector + uint64(sectorsForSize(bup.Length)) - 1 - ifo.Sector
	if last > math.MaxUint32 {
		return nil, errors.New("IFO last-sector address exceeds 32 bits")
	}
	binary.BigEndian.PutUint32(header[0x000C:0x0010], uint32(last))
	if err := writeImageHeader(image, ifo, bup, header); err != nil {
		return nil, err
	}
	return header, nil
}

func readImageHeader(image *os.File, loc udf.Location) ([]byte, error) {
	if loc.Length < dvdImageSectorSize {
		return nil, errors.New("IFO extent is shorter than one sector")
	}
	header := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(header, int64(loc.Sector)*dvdImageSectorSize); err != nil {
		return nil, fmt.Errorf("read IFO header at sector %d: %w", loc.Sector, err)
	}
	return header, nil
}

func writeImageHeader(image *os.File, ifo, bup udf.Location, header []byte) error {
	for _, loc := range []udf.Location{ifo, bup} {
		if err := writeAtFull(image, header, int64(loc.Sector)*dvdImageSectorSize); err != nil {
			return err
		}
	}
	return nil
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

func writeISO9660Bridge(image *os.File, udfSectors uint64, locations map[string]udf.Location, files []dvdImageFile, volumeID string, capacity int64) (uint64, error) {
	if udfSectors == 0 || udfSectors > math.MaxUint32 {
		return 0, errors.New("mp4todvd: ISO 9660 volume exceeds 32-bit sector count")
	}
	// The ECMA-119 metadata is appended after the UDF partition. This keeps
	// DVD-Video's reserved sectors (including 21-31 and 66-255) at zero.
	if udfSectors > math.MaxUint32-4 {
		return 0, errors.New("mp4todvd: ISO metadata extent exceeds 32-bit sector addresses")
	}
	baseLBA := uint32(udfSectors - 1) // reuse the old backup-anchor sector.
	anchor := make([]byte, dvdImageSectorSize)
	if _, err := image.ReadAt(anchor, int64(baseLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("mp4todvd: read UDF backup anchor: %w", err)
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
		return 0, errors.New("mp4todvd: ISO path table exceeds one sector")
	}

	videoEntries := make([]dvdImageFile, len(files))
	copy(videoEntries, files)
	sort.Slice(videoEntries, func(i, j int) bool { return videoEntries[i].name < videoEntries[j].name })
	videoRecords := make([][]byte, 0, len(videoEntries)+2)
	videoRecords = append(videoRecords,
		isoDirectoryRecord([]byte{0}, videoLBA, dvdImageSectorSize, 0x02),
		isoDirectoryRecord([]byte{1}, rootLBA, dvdImageSectorSize, 0x02),
	)
	for _, file := range videoEntries {
		loc := locations[file.name]
		if loc.Length <= 0 || loc.Sector > math.MaxUint32 {
			return 0, fmt.Errorf("mp4todvd: invalid ISO extent for %s", file.name)
		}
		records, err := isoFileRecords(file.name+";1", loc)
		if err != nil {
			return 0, fmt.Errorf("mp4todvd: create ISO records for %s: %w", file.name, err)
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
		return 0, errors.New("mp4todvd: ISO 9660 volume exceeds supported sector count")
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
			return 0, fmt.Errorf("mp4todvd: write ISO %s path table: %w", table.name, err)
		}
	}
	for _, dir := range []struct {
		lba  uint32
		data []byte
	}{{rootLBA, rootData}, {audioLBA, audioData}, {videoLBA, videoData}} {
		if err := writeAtFull(image, dir.data, int64(dir.lba)*dvdImageSectorSize); err != nil {
			return 0, fmt.Errorf("mp4todvd: write ISO directory at sector %d: %w", dir.lba, err)
		}
	}

	pvd := buildISOPVD(imageSectors, uint32(len(lPath)), lPathLBA, mPathLBA, rootLBA, volumeID, uint32(len(rootData)))
	if err := writeAtFull(image, pvd, int64(isoPVDLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("mp4todvd: write ISO primary volume descriptor: %w", err)
	}
	terminator := make([]byte, dvdImageSectorSize)
	terminator[0] = 255
	copy(terminator[1:6], "CD001")
	terminator[6] = 1
	if err := writeAtFull(image, terminator, int64(isoTerminatorLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("mp4todvd: write ISO volume descriptor terminator: %w", err)
	}
	binary.LittleEndian.PutUint32(anchor[12:16], uint32(newAnchorLBA))
	refreshUDFTag(anchor)
	if err := writeAtFull(image, anchor, int64(newAnchorLBA)*dvdImageSectorSize); err != nil {
		return 0, fmt.Errorf("mp4todvd: write final UDF backup anchor: %w", err)
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
			return nil, errors.New("mp4todvd: ISO directory record has invalid length")
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
		return nil, errors.New("mp4todvd: empty ISO directory")
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
		for i := field[0]; i < field[1]; i++ {
			pvd[i] = '0'
		}
		copy(pvd[field[0]:field[0]+16], now)
		pvd[field[1]-1] = '0'
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
