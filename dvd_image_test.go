package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
)

type isoTestExtent struct {
	sector  uint32
	sectors uint32
}

type isoTestImage struct {
	t     *testing.T
	data  []byte
	files map[string]isoTestExtent
}

func (image isoTestImage) sector(lba uint32) []byte {
	image.t.Helper()
	start := int(lba) * DVDVOBSectorSize
	if start+DVDVOBSectorSize > len(image.data) {
		image.t.Fatalf("sector %d is outside the %d-sector image", lba, len(image.data)/DVDVOBSectorSize)
	}
	return image.data[start : start+DVDVOBSectorSize]
}

// readISOTestImage reads the ISO 9660 view of an image and returns the extent
// of every file in VIDEO_TS.
func readISOTestImage(t *testing.T, path string) isoTestImage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	image := isoTestImage{t: t, data: data, files: map[string]isoTestExtent{}}
	pvd := image.sector(16)
	if string(pvd[1:6]) != "CD001" {
		t.Fatal("image has no ISO 9660 primary volume descriptor")
	}
	readDirectory := func(record []byte) map[string][]byte {
		lba, length := binary.LittleEndian.Uint32(record[2:6]), int(binary.LittleEndian.Uint32(record[10:14]))
		directory := data[int(lba)*DVDVOBSectorSize:][:length]
		records := map[string][]byte{}
		for offset := 0; offset < length; {
			size := int(directory[offset])
			if size == 0 {
				offset = (offset/DVDVOBSectorSize + 1) * DVDVOBSectorSize
				continue
			}
			name := string(directory[offset+33 : offset+33+int(directory[offset+32])])
			records[strings.TrimSuffix(name, ";1")] = directory[offset : offset+size]
			offset += size
		}
		return records
	}
	root := readDirectory(pvd[156:190])
	videoTS, ok := root["VIDEO_TS"]
	if !ok {
		t.Fatal("ISO 9660 root directory has no VIDEO_TS")
	}
	if _, ok := root["AUDIO_TS"]; !ok {
		t.Fatal("ISO 9660 root directory has no AUDIO_TS")
	}
	for name, record := range readDirectory(videoTS) {
		if len(name) < 2 {
			continue // "." and ".."
		}
		size := binary.LittleEndian.Uint32(record[10:14])
		image.files[name] = isoTestExtent{
			sector:  binary.LittleEndian.Uint32(record[2:6]),
			sectors: (size + DVDVOBSectorSize - 1) / DVDVOBSectorSize,
		}
	}
	return image
}

// checkISOTestFileSet verifies every layout-dependent IFO field of one VMG or
// VTS against the real extents, and the properties DVD-Video needs from the
// layout itself.
func (image isoTestImage) checkFileSet(stem string, menuVOB string, titleVOBs []string) {
	t := image.t
	t.Helper()
	ifo, bup := image.files[stem+".IFO"], image.files[stem+".BUP"]
	if ifo.sectors == 0 || bup.sectors != ifo.sectors {
		t.Fatalf("%s: IFO/BUP extents are %+v / %+v", stem, ifo, bup)
	}
	ifoData := image.data[int(ifo.sector)*DVDVOBSectorSize:][:int(ifo.sectors)*DVDVOBSectorSize]
	if !bytes.Equal(ifoData, image.data[int(bup.sector)*DVDVOBSectorSize:][:int(bup.sectors)*DVDVOBSectorSize]) {
		t.Errorf("%s: BUP differs from IFO in the image", stem)
	}
	if got, want := binary.BigEndian.Uint32(ifoData[0x0c:0x10]), bup.sector+bup.sectors-1-ifo.sector; got != want {
		t.Errorf("%s: last sector = %d, want %d", stem, got, want)
	}
	if got, want := binary.BigEndian.Uint32(ifoData[0x1c:0x20]), ifo.sectors-1; got != want {
		t.Errorf("%s: IFO last sector = %d, want %d", stem, got, want)
	}
	// One damaged 16-sector ECC block must not be able to destroy both the
	// IFO and its backup.
	if (ifo.sector+ifo.sectors-1)/16 >= bup.sector/16 {
		t.Errorf("%s: IFO ends at sector %d and BUP starts at sector %d, in the same ECC block", stem, ifo.sector+ifo.sectors-1, bup.sector)
	}
	next := ifo.sector + ifo.sectors
	menuPointer := binary.BigEndian.Uint32(ifoData[0xc0:0xc4])
	if menuVOB == "" {
		if menuPointer != 0 {
			t.Errorf("%s: menu VOB pointer = %d without a menu VOB", stem, menuPointer)
		}
	} else {
		menu := image.files[menuVOB]
		if menu.sector != next {
			t.Errorf("%s: %s starts at sector %d, want %d directly after the IFO", stem, menuVOB, menu.sector, next)
		}
		if ifo.sector+menuPointer != menu.sector {
			t.Errorf("%s: menu VOB pointer = %d, want %d", stem, menuPointer, menu.sector-ifo.sector)
		}
		if pack := image.sector(ifo.sector + menuPointer); !bytes.Equal(pack[:4], []byte{0, 0, 1, 0xba}) {
			t.Errorf("%s: menu VOB pointer leads to % x, want a pack header", stem, pack[:4])
		}
		next = menu.sector + menu.sectors
	}
	if len(titleVOBs) == 0 {
		return
	}
	titlePointer := binary.BigEndian.Uint32(ifoData[0xc4:0xc8])
	var titleSectors uint32
	for _, name := range titleVOBs {
		vob := image.files[name]
		if vob.sector != next {
			t.Errorf("%s: %s starts at sector %d, want %d (title VOB files must be contiguous)", stem, name, vob.sector, next)
		}
		next = vob.sector + vob.sectors
		titleSectors += vob.sectors
	}
	if first := image.files[titleVOBs[0]].sector; ifo.sector+titlePointer != first {
		t.Fatalf("%s: VTSTT_VOBS pointer = %d, want %d", stem, titlePointer, first-ifo.sector)
	}
	// Every VOBU address in the IFO is relative to VTSTT_VOBS and must lead
	// to a NAV pack that records the same address, including VOBUs stored
	// in the second and later VOB files.
	admap := ifoData[int(binary.BigEndian.Uint32(ifoData[0xe4:0xe8]))*DVDVOBSectorSize:]
	vobuCount := (int(binary.BigEndian.Uint32(admap[0:4])) + 1 - 4) / 4
	lastVOBU := uint32(0)
	for i := range vobuCount {
		address := binary.BigEndian.Uint32(admap[4+i*4 : 8+i*4])
		pack := image.sector(ifo.sector + titlePointer + address)
		if !bytes.Equal(pack[:4], []byte{0, 0, 1, 0xba}) || !bytes.Equal(pack[38:42], []byte{0, 0, 1, 0xbf}) ||
			binary.BigEndian.Uint32(pack[45:49]) != address {
			t.Fatalf("%s: VOBU %d at title sector %d is not a NAV pack recording that address", stem, i+1, address)
		}
		lastVOBU = address
	}
	if lastVOBU < image.files[titleVOBs[0]].sectors && len(titleVOBs) > 1 {
		t.Errorf("%s: no VOBU lies beyond the first of %d VOB files", stem, len(titleVOBs))
	}
	cellTable := ifoData[int(binary.BigEndian.Uint32(ifoData[0xe0:0xe4]))*DVDVOBSectorSize:]
	if got := binary.BigEndian.Uint32(cellTable[16:20]); got != titleSectors-1 {
		t.Errorf("%s: cell last sector = %d, want %d (the sum of all VOB files)", stem, got, titleSectors-1)
	}
}

func TestWriteDVDISOLayoutMatchesIFOSectorPointers(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	title := makeAuthorerTestTitle(t, profile)

	// Author once unsplit to learn the title VOB size, then with a split
	// size that needs three VOB files.
	plainDir := t.TempDir()
	if err := NewInProcessAuthorer().Author(context.Background(), []EncodedTitle{title, title}, plainDir, profile); err != nil {
		t.Fatal(err)
	}
	plainVOB := readAuthorerTestFile(t, plainDir, "VTS_01_1.VOB")
	titleSectors := len(plainVOB) / DVDVOBSectorSize
	if titleSectors < 6 {
		t.Fatalf("test title VOB has only %d sectors", titleSectors)
	}
	splitSectors := (titleSectors + 2) / 3
	splitDir := t.TempDir()
	if err := ExportNewSplitAuthorer(uint32(splitSectors)).Author(context.Background(), []EncodedTitle{title, title}, splitDir, profile); err != nil {
		t.Fatal(err)
	}
	var parts []string
	var joined []byte
	for part := 1; ; part++ {
		name := fmt.Sprintf("VTS_01_%d.VOB", part)
		data, err := os.ReadFile(filepath.Join(splitDir, name))
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > splitSectors*DVDVOBSectorSize || len(data)%DVDVOBSectorSize != 0 {
			t.Fatalf("%s has %d bytes, want at most %d whole sectors", name, len(data), splitSectors)
		}
		parts = append(parts, name)
		joined = append(joined, data...)
	}
	if len(parts) != 3 {
		t.Fatalf("title was written as %d VOB files, want 3", len(parts))
	}
	// Splitting changes only where the stream is cut: NAV addresses and the
	// IFO stay relative to the first file.
	if !bytes.Equal(joined, plainVOB) {
		t.Fatal("concatenated VOB files differ from the unsplit title VOB")
	}
	if !bytes.Equal(readAuthorerTestFile(t, splitDir, "VTS_01_0.IFO"), readAuthorerTestFile(t, plainDir, "VTS_01_0.IFO")) ||
		!bytes.Equal(readAuthorerTestFile(t, splitDir, "VIDEO_TS.IFO"), readAuthorerTestFile(t, plainDir, "VIDEO_TS.IFO")) {
		t.Fatal("IFO files of the split title set differ from the unsplit ones")
	}

	for _, tc := range []struct {
		name, dir string
		parts     int
	}{{"single VOB file", plainDir, 1}, {"three VOB files", splitDir, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			isoPath := filepath.Join(t.TempDir(), "disc.iso")
			if err := WriteDVDISO(context.Background(), tc.dir, isoPath); err != nil {
				t.Fatal(err)
			}
			image := readISOTestImage(t, isoPath)
			wantFiles := 3 + 2*(2+tc.parts)
			if len(image.files) != wantFiles {
				t.Fatalf("ISO 9660 VIDEO_TS has %d files, want %d", len(image.files), wantFiles)
			}
			image.checkFileSet("VIDEO_TS", "VIDEO_TS.VOB", nil)
			vmg := image.files["VIDEO_TS.IFO"]
			titleTable := image.sector(vmg.sector + binary.BigEndian.Uint32(image.sector(vmg.sector)[0xc4:0xc8]))
			for number := 1; number <= 2; number++ {
				stem := fmt.Sprintf("VTS_%02d_0", number)
				var vobs []string
				for part := 1; part <= tc.parts; part++ {
					vobs = append(vobs, fmt.Sprintf("VTS_%02d_%d.VOB", number, part))
				}
				image.checkFileSet(stem, "", vobs)
				if got, want := binary.BigEndian.Uint32(titleTable[8+(number-1)*12+8:]), image.files[stem+".IFO"].sector-vmg.sector; got != want {
					t.Errorf("TT_SRPT start sector of title set %d = %d, want %d relative to VIDEO_TS.IFO", number, got, want)
				}
			}
			// The 17th byte of each volume timestamp is a binary GMT
			// offset; the digits before it are UTC.
			for _, offset := range []int{829, 846, 863, 880} {
				if got := image.sector(16)[offset]; got != 0 {
					t.Errorf("PVD timestamp GMT offset at byte %d = %d, want 0", offset, got)
				}
			}

			// The UDF view must list the same files over the same data.
			file, err := os.Open(isoPath)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			volume, err := udf.Read(file)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := volume.ReadDir([]string{"VIDEO_TS"})
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != wantFiles {
				t.Fatalf("UDF VIDEO_TS has %d files, want %d", len(entries), wantFiles)
			}
			for _, entry := range entries {
				extent, ok := image.files[entry.Name]
				if !ok {
					t.Fatalf("UDF file %s is missing from the ISO 9660 view", entry.Name)
				}
				contents, err := volume.ReadFile([]string{"VIDEO_TS", entry.Name})
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(contents, image.data[int(extent.sector)*DVDVOBSectorSize:][:int(extent.sectors)*DVDVOBSectorSize]) {
					t.Errorf("UDF and ISO 9660 views of %s differ", entry.Name)
				}
				if strings.HasSuffix(entry.Name, ".VOB") && !bytes.Equal(contents, readAuthorerTestFile(t, tc.dir, entry.Name)) {
					t.Errorf("%s in the image differs from the authored file", entry.Name)
				}
			}
		})
	}

	// A hole in the VOB numbering would break the continuous title address
	// space, so the image writer refuses it.
	if err := os.Rename(filepath.Join(splitDir, "VTS_01_3.VOB"), filepath.Join(splitDir, "VTS_01_4.VOB")); err != nil {
		t.Fatal(err)
	}
	if err := WriteDVDISO(context.Background(), splitDir, filepath.Join(t.TempDir(), "gap.iso")); err == nil {
		t.Fatal("expected an image of a title set with a gap in its VOB numbering to fail")
	}
}

// A VIDEO_TS directory may come from an authorer that gives every title set
// its own menu VOB. The image writer must then point VTSM_VOBS at it.
func TestWriteDVDISOPointsAtTitleSetMenuVOB(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio4x3})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := NewInProcessAuthorer().Author(context.Background(), []EncodedTitle{makeAuthorerTestTitle(t, profile)}, dir, profile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VTS_01_0.VOB"), readAuthorerTestFile(t, dir, "VIDEO_TS.VOB"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The IFO still says that the title set has no menu VOB.
	if err := WriteDVDISO(context.Background(), dir, filepath.Join(t.TempDir(), "mismatch.iso")); err == nil {
		t.Fatal("expected a menu VOB file without a VTSM_VOBS pointer to fail")
	}
	ifo := readAuthorerTestFile(t, dir, "VTS_01_0.IFO")
	// Any non-zero value declares the menu VOB; the directory-relative value
	// is replaced by the image extent.
	binary.BigEndian.PutUint32(ifo[0xc0:0xc4], 1)
	for _, name := range []string{"VTS_01_0.IFO", "VTS_01_0.BUP"} {
		if err := os.WriteFile(filepath.Join(dir, name), ifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	isoPath := filepath.Join(t.TempDir(), "disc.iso")
	if err := WriteDVDISO(context.Background(), dir, isoPath); err != nil {
		t.Fatal(err)
	}
	image := readISOTestImage(t, isoPath)
	image.checkFileSet("VIDEO_TS", "VIDEO_TS.VOB", nil)
	image.checkFileSet("VTS_01_0", "VTS_01_0.VOB", []string{"VTS_01_1.VOB"})
}

func TestTitleVOBWriterSplitsAtFileLimit(t *testing.T) {
	if TestDVDTitleVOBFileMaxSectors != 524287 || TestDVDTitleVOBMaxFiles != 9 {
		t.Fatalf("title VOB limits = %d sectors x %d files, want 524287 x 9", TestDVDTitleVOBFileMaxSectors, TestDVDTitleVOBMaxFiles)
	}
	const fileBytes = 4 * DVDVOBSectorSize
	data := make([]byte, 10*DVDVOBSectorSize)
	for i := range data {
		data[i] = byte(i / DVDVOBSectorSize * 17)
	}
	// The cut positions must not depend on how the muxer chunks its writes.
	for _, chunkSize := range []int{DVDVOBSectorSize, 3000, 5 * DVDVOBSectorSize, len(data)} {
		dir := t.TempDir()
		files, err := ExportWriteSplitTitleVOB(dir, "VTS_01", fileBytes, data, chunkSize)
		if err != nil {
			t.Fatal(err)
		}
		if files != 3 {
			t.Fatalf("chunk size %d: wrote %d files, want 3", chunkSize, files)
		}
		for part, want := range [][]byte{data[:fileBytes], data[fileBytes : 2*fileBytes], data[2*fileBytes:]} {
			if got := readAuthorerTestFile(t, dir, fmt.Sprintf("VTS_01_%d.VOB", part+1)); !bytes.Equal(got, want) {
				t.Fatalf("chunk size %d: VTS_01_%d.VOB has %d bytes, want the next %d bytes of the stream", chunkSize, part+1, len(got), len(want))
			}
		}
	}

	full := make([]byte, 9*DVDVOBSectorSize)
	if files, err := ExportWriteSplitTitleVOB(t.TempDir(), "VTS_01", DVDVOBSectorSize, full, len(full)); err != nil || files != 9 {
		t.Fatalf("nine full VOB files: files=%d err=%v", files, err)
	}
	if _, err := ExportWriteSplitTitleVOB(t.TempDir(), "VTS_01", DVDVOBSectorSize, append(full, 0), len(full)+1); err == nil {
		t.Fatal("expected a title that needs a tenth VOB file to fail")
	}
}
