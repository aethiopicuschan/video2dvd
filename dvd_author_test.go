package video2dvd

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInProcessAuthorerWritesNavigableMultiTitleDVD(t *testing.T) {
	profile, err := (Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9}).profile()
	if err != nil {
		t.Fatal(err)
	}
	title := makeAuthorerTestTitle(t, profile)
	videoTS := t.TempDir()
	if err := NewInProcessAuthorer().Author(context.Background(), []EncodedTitle{title, title}, videoTS, profile); err != nil {
		t.Fatal(err)
	}

	vmg := readAuthorerTestFile(t, videoTS, "VIDEO_TS.IFO")
	if len(vmg)%DVDVOBSectorSize != 0 || string(vmg[:12]) != "DVDVIDEO-VMG" {
		t.Fatalf("VMGI header/size invalid: %q, %d bytes", vmg[:12], len(vmg))
	}
	vmgSectors := uint32(len(vmg) / DVDVOBSectorSize)
	if got := binary.BigEndian.Uint32(vmg[0x00C4:0x00C8]); got != 1 {
		t.Fatalf("TT_SRPT sector = %d, want 1", got)
	}
	if got := binary.BigEndian.Uint32(vmg[0x00D0:0x00D4]); got != 2 {
		t.Fatalf("VTS_ATRT sector = %d, want 2", got)
	}
	tt := vmg[DVDVOBSectorSize:]
	if got := binary.BigEndian.Uint16(tt[0:2]); got != 2 {
		t.Fatalf("VMG title count = %d, want 2", got)
	}
	firstVTSStart := binary.BigEndian.Uint32(tt[8+8 : 8+12])
	secondVTSStart := binary.BigEndian.Uint32(tt[8+12+8 : 8+12+12])
	menuVOB := readAuthorerTestFile(t, videoTS, "VIDEO_TS.VOB")
	menuSectors := uint32(len(menuVOB) / DVDVOBSectorSize)
	if firstVTSStart != vmgSectors*2+menuSectors {
		t.Fatalf("first VTS sector = %d, want after VMGI/menu/BUP at %d", firstVTSStart, vmgSectors*2+menuSectors)
	}

	firstIFO := readAuthorerTestFile(t, videoTS, "VTS_01_0.IFO")
	if menuVTS := readAuthorerTestFile(t, videoTS, "VTS_01_0.VOB"); !bytes.Equal(menuVTS, menuVOB) {
		t.Fatal("VTS menu VOB does not mirror the title menu")
	}
	firstVOB := readAuthorerTestFile(t, videoTS, "VTS_01_1.VOB")
	secondIFO := readAuthorerTestFile(t, videoTS, "VTS_02_0.IFO")
	if !bytes.Equal(firstIFO, readAuthorerTestFile(t, videoTS, "VTS_01_0.BUP")) ||
		!bytes.Equal(secondIFO, readAuthorerTestFile(t, videoTS, "VTS_02_0.BUP")) ||
		!bytes.Equal(vmg, readAuthorerTestFile(t, videoTS, "VIDEO_TS.BUP")) {
		t.Fatal("IFO and BUP pairs differ")
	}
	firstIFOSectors := uint32(len(firstIFO) / DVDVOBSectorSize)
	firstVOBSectors := uint32(len(firstVOB) / DVDVOBSectorSize)
	if secondVTSStart != firstVTSStart+firstIFOSectors*2+menuSectors+firstVOBSectors {
		t.Fatalf("second VTS sector = %d, want %d", secondVTSStart, firstVTSStart+firstIFOSectors*2+menuSectors+firstVOBSectors)
	}

	if string(firstIFO[:12]) != "DVDVIDEO-VTS" {
		t.Fatalf("VTSI signature = %q", firstIFO[:12])
	}
	if got := binary.BigEndian.Uint32(firstIFO[0x00C4:0x00C8]); got != firstIFOSectors+menuSectors {
		t.Fatalf("title VOB starts at sector %d, want %d", got, firstIFOSectors+menuSectors)
	}
	for offset, want := range map[int]uint32{0x00C8: 1, 0x00CC: 2, 0x00D4: 4, 0x00E0: 3, 0x00E4: 5} {
		if got := binary.BigEndian.Uint32(firstIFO[offset : offset+4]); got != want {
			t.Errorf("VTSI pointer at %#x = %d, want %d", offset, got, want)
		}
	}
	if got := binary.BigEndian.Uint16(firstIFO[0x0202:0x0204]); got != 1 {
		t.Fatalf("title audio stream count = %d, want 1", got)
	}
	vtsAtrt := vmg[2*DVDVOBSectorSize:]
	atrEntry := int(binary.BigEndian.Uint32(vtsAtrt[8:12]))
	if !bytes.Equal(vtsAtrt[atrEntry+8:atrEntry+8+0x300], firstIFO[0x100:0x400]) {
		t.Fatal("VMG VTS_ATRT attributes do not mirror the VTS IFO attributes")
	}
	if firstIFO[0x0204]>>5 != 4 || firstIFO[0x0205]&7 != 1 {
		t.Fatalf("LPCM stream attributes = % x, want 16-bit 48 kHz stereo", firstIFO[0x0204:0x020c])
	}

	ptt := firstIFO[DVDVOBSectorSize:]
	if binary.BigEndian.Uint16(ptt[0:2]) != 1 || binary.BigEndian.Uint16(ptt[12:14]) != 1 || binary.BigEndian.Uint16(ptt[14:16]) != 1 {
		t.Fatal("PTT table does not map title 1, chapter 1 to PGC 1/program 1")
	}
	pgci := firstIFO[2*DVDVOBSectorSize:]
	if binary.BigEndian.Uint16(pgci[0:2]) != 1 || binary.BigEndian.Uint32(pgci[8:12]) != 0x81000000 {
		t.Fatal("PGCI does not contain the entry PGC for title 1")
	}
	pgc := pgci[16:]
	if pgc[2] != 1 || pgc[3] != 1 || pgc[0x0c] != 0x80 {
		t.Fatalf("PGC program/cell/audio controls are invalid: % x", pgc[:0x1c])
	}
	cadt := firstIFO[3*DVDVOBSectorSize:]
	if binary.BigEndian.Uint16(cadt[0:2]) != 1 || binary.BigEndian.Uint32(cadt[16:20]) != firstVOBSectors-1 {
		t.Fatal("cell address table does not cover the title VOB")
	}
	tmap := firstIFO[4*DVDVOBSectorSize:]
	if binary.BigEndian.Uint16(tmap[0:2]) != 1 || binary.BigEndian.Uint32(tmap[8:12]) != 12 || tmap[12] != 1 {
		t.Fatal("time-map table does not include an empty map for the PGC")
	}
	if binary.BigEndian.Uint16(tmap[14:16]) != 0 {
		t.Fatalf("empty VTS_TMAP has %d entries, want 0", binary.BigEndian.Uint16(tmap[14:16]))
	}
	admap := firstIFO[5*DVDVOBSectorSize:]
	if got := binary.BigEndian.Uint32(admap[4:8]); got != 0 {
		t.Fatalf("first VOBU sector = %d, want 0", got)
	}
	endAddress := binary.BigEndian.Uint32(admap[0:4])
	if endAddress < 7 || endAddress > uint32(len(admap)-1) || (endAddress+1-4)%4 != 0 {
		t.Fatalf("VOBU address-map end address = %d, invalid for %d-byte table storage", endAddress, len(admap))
	}
	if entries := (endAddress + 1 - 4) / 4; entries != 2 {
		t.Fatalf("VOBU address-map entry count = %d, want two VOBUs", entries)
	}
	if secondVOBU := binary.BigEndian.Uint32(admap[8:12]); secondVOBU == 0 || secondVOBU >= firstVOBSectors {
		t.Fatalf("second VOBU sector = %d, outside the VOB", secondVOBU)
	}
}

func TestDVDMenuPGCIContainsPagePGCs(t *testing.T) {
	menu := buildDVDVMGMenuPGCI(20, [4]byte{0, 0, 1, 0xc0}, []dvdVOBURecord{
		{StartSector: 0, EndSector: 9}, {StartSector: 10, EndSector: 19},
	})
	if len(menu) < 32 || binary.BigEndian.Uint16(menu[16:18]) != 2 {
		t.Fatalf("menu language unit page count is invalid")
	}
}

func makeAuthorerTestTitle(t *testing.T, profile Profile) EncodedTitle {
	t.Helper()
	workDir := t.TempDir()
	videoPath := filepath.Join(workDir, "video.m2v")
	videoFile, err := os.Create(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(videoFile, profile)
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Duration: time.Second * 1001 / 30000,
		Y:        bytes.Repeat([]byte{80}, profile.FrameWidth*profile.FrameHeight),
		U:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	for i := range 16 {
		if i == 1 {
			frame = cloneDecodedVideoFrame(frame)
			frame.Y[100*frame.Width+100] = 140
		}
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := videoFile.Close(); err != nil {
		t.Fatal(err)
	}

	audioPath := filepath.Join(workDir, "audio.lpcm")
	audioFile, err := os.Create(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteDVDLPCM(audioFile, PCMChunk{SampleRate: 48000, Channels: 2, Samples: make([]int16, 960*2)}); err != nil {
		_ = audioFile.Close()
		t.Fatal(err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatal(err)
	}
	return EncodedTitle{
		VideoPath:            videoPath,
		AudioPath:            audioPath,
		VideoCodec:           VideoCodecMPEG2,
		AudioCodec:           AudioCodecLPCM,
		Aspect:               profile.Aspect,
		Width:                profile.FrameWidth,
		Height:               profile.FrameHeight,
		FrameRateNumerator:   30000,
		FrameRateDenominator: 1001,
		Duration:             16 * time.Second * 1001 / 30000,
	}
}

func readAuthorerTestFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
