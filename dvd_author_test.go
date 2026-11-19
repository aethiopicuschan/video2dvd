package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInProcessAuthorerWritesNavigableMultiTitleDVD(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
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
	if got := binary.BigEndian.Uint16(vmg[0x0026:0x0028]); got != 1 {
		t.Fatalf("VMGI volume count = %d, want 1", got)
	}
	if got := binary.BigEndian.Uint16(vmg[0x0028:0x002a]); got != 1 {
		t.Fatalf("VMGI volume number = %d, want 1", got)
	}
	if got := vmg[0x002a]; got != 1 {
		t.Fatalf("VMGI disc side = %d, want 1", got)
	}
	if !bytes.Equal(vmg[0x002b:0x003e], make([]byte, 0x13)) {
		t.Fatalf("VMGI reserved bytes are not zero: % x", vmg[0x002b:0x003e])
	}
	if got, want := binary.BigEndian.Uint32(vmg[0x0080:0x0084]), uint32(len(vmg)-1); got != want {
		t.Fatalf("VMGI last byte = %#x, want %#x", got, want)
	}
	firstPlayOffset := int(binary.BigEndian.Uint32(vmg[0x0084:0x0088]))
	if firstPlayOffset >= int(binary.BigEndian.Uint32(vmg[0x0080:0x0084])) {
		t.Fatalf("first-play PGC offset %#x is beyond VMGI last byte", firstPlayOffset)
	}
	firstPlay := vmg[firstPlayOffset:]
	if firstPlay[2] != 0 || firstPlay[3] != 0 || binary.BigEndian.Uint16(firstPlay[0x00e4:0x00e6]) != 0x00ec {
		t.Fatal("first-play PGC does not contain an isolated command-only chain")
	}
	if binary.BigEndian.Uint16(firstPlay[0x00ec:0x00ee]) != 2 ||
		!bytes.Equal(firstPlay[0x00f4:0x00fc], []byte{0x71, 0, 0, 1, 0, 0, 0, 0}) ||
		!bytes.Equal(firstPlay[0x00fc:0x0104], []byte{0x30, 0x06, 0, 0, 0, 0x42, 0, 0}) {
		t.Fatal("first-play PGC does not initialize navigation state and jump to the VMG menu")
	}
	if got := binary.BigEndian.Uint16(firstPlay[0x00f2:0x00f4]); got != 23 {
		t.Fatalf("first-play command table last byte = %d, want 23", got)
	}
	vmgSectors := uint32(len(vmg) / DVDVOBSectorSize)
	vmgMenuCadtSector := binary.BigEndian.Uint32(vmg[0x00d8:0x00dc])
	vmgMenuAdmapSector := binary.BigEndian.Uint32(vmg[0x00dc:0x00e0])
	vmgMenuCadt := vmg[int(vmgMenuCadtSector)*DVDVOBSectorSize:]
	vmgMenuAdmap := vmg[int(vmgMenuAdmapSector)*DVDVOBSectorSize:]
	menuVOB := readAuthorerTestFile(t, videoTS, "VIDEO_TS.VOB")
	menuDSI := menuVOB[0x407:]
	if got := binary.BigEndian.Uint32(menuDSI[0x13a:0x13e]); got != DVDNAVSRIEndOfCell {
		t.Fatalf("single-cell menu next VOBU pointer = %#x, want end-of-cell %#x", got, DVDNAVSRIEndOfCell)
	}
	if got := binary.BigEndian.Uint32(menuDSI[0x13e:0x142]); got != DVDNAVSRIEndOfCell {
		t.Fatalf("single-cell menu previous VOBU pointer = %#x, want end-of-cell %#x", got, DVDNAVSRIEndOfCell)
	}
	menuSectors := uint32(len(menuVOB) / DVDVOBSectorSize)
	if vmgiCount := binary.BigEndian.Uint16(vmgMenuCadt[0:2]); vmgiCount != 1 ||
		binary.BigEndian.Uint16(vmgMenuCadt[8:10]) != 1 || vmgMenuCadt[10] != 1 ||
		binary.BigEndian.Uint32(vmgMenuCadt[12:16]) != 0 ||
		binary.BigEndian.Uint32(vmgMenuCadt[16:20]) != menuSectors-1 ||
		binary.BigEndian.Uint32(vmgMenuAdmap[0:4]) != 7 ||
		binary.BigEndian.Uint32(vmgMenuAdmap[4:8]) != 0 {
		t.Fatal("VMG menu cell/VOBU address tables do not cover the menu VOB")
	}
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
	vmgMenuTable := vmg[int(binary.BigEndian.Uint32(vmg[0x00c8:0x00cc]))*DVDVOBSectorSize:]
	menuUnitOffset := int(binary.BigEndian.Uint32(vmgMenuTable[12:16]))
	menuUnit := vmgMenuTable[menuUnitOffset:]
	if got := binary.BigEndian.Uint16(menuUnit[0:2]); got != 6 {
		t.Fatalf("VMG menu PGCI PGC count = %d, want 6 (menu, PLAY ALL, two launchers, two continuations)", got)
	}
	menuPGCOffset := int(binary.BigEndian.Uint32(menuUnit[12:16]))
	menuPGC := menuUnit[menuPGCOffset:]
	menuCellPlaybackOffset := int(binary.BigEndian.Uint16(menuPGC[0x00e8:0x00ea]))
	if !bytes.Equal(menuPGC[4:8], []byte{0, 0, 1, 0xc0}) ||
		!bytes.Equal(menuPGC[4:8], menuPGC[menuCellPlaybackOffset+4:menuCellPlaybackOffset+8]) {
		t.Fatal("VMG menu PGC duration does not match its cell playback duration")
	}
	for pgcIndex, want := range []byte{0x82, 0, 0, 0, 0, 0} {
		entry := 8 + pgcIndex*8
		if got := menuUnit[entry]; got != want {
			t.Fatalf("VMG menu PGC %d entry ID = %#x, want %#x", pgcIndex+1, got, want)
		}
	}
	advanceEntry := 8 + 4*8
	advanceOffset := int(binary.BigEndian.Uint32(menuUnit[advanceEntry+4 : advanceEntry+8]))
	advancePGC := menuUnit[advanceOffset:]
	if got := binary.BigEndian.Uint16(advancePGC[0xec:0x00ee]); got != 1 {
		t.Fatalf("title continuation PGC pre-command count = %d, want 1", got)
	}
	if !bytes.Equal(advancePGC[0xf4:0xfc], []byte{0x30, 0x02, 0, 0, 0, 2, 0, 0}) {
		t.Fatalf("title continuation does not jump to title 2: % x", advancePGC[0xf4:0xfc])
	}
	firstVTSStart := binary.BigEndian.Uint32(tt[8+8 : 8+12])
	secondVTSStart := binary.BigEndian.Uint32(tt[8+12+8 : 8+12+12])
	menuSectors = uint32(len(menuVOB) / DVDVOBSectorSize)
	if firstVTSStart != vmgSectors*2+menuSectors {
		t.Fatalf("first VTS sector = %d, want after VMGI/menu/BUP at %d", firstVTSStart, vmgSectors*2+menuSectors)
	}

	firstIFO := readAuthorerTestFile(t, videoTS, "VTS_01_0.IFO")
	vtsMenuTableSector := binary.BigEndian.Uint32(firstIFO[0x00d0:0x00d4])
	vtsMenuTable := firstIFO[int(vtsMenuTableSector)*DVDVOBSectorSize:]
	vtsMenuUnit := vtsMenuTable[int(binary.BigEndian.Uint32(vtsMenuTable[12:16])):]
	vtsMenuPGC := vtsMenuUnit[int(binary.BigEndian.Uint32(vtsMenuUnit[12:16])):]
	vtsMenuCellPlaybackOffset := int(binary.BigEndian.Uint16(vtsMenuPGC[0x00e8:0x00ea]))
	if !bytes.Equal(vtsMenuPGC[4:8], []byte{0, 0, 1, 0xc0}) ||
		!bytes.Equal(vtsMenuPGC[4:8], vtsMenuPGC[vtsMenuCellPlaybackOffset+4:vtsMenuCellPlaybackOffset+8]) {
		t.Fatal("VTS menu PGC duration does not match its one-second cell playback duration")
	}
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
	if got, want := binary.BigEndian.Uint32(firstIFO[0x0080:0x0084]), uint32(len(firstIFO)-1); got != want {
		t.Fatalf("VTSI last byte = %#x, want %#x", got, want)
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
	if got := binary.BigEndian.Uint32(vtsAtrt[atrEntry : atrEntry+4]); got != 775 {
		t.Fatalf("VMG VTS attribute record last byte = %d, want 775", got)
	}
	if !bytes.Equal(vtsAtrt[atrEntry+8:atrEntry+8+0x300], firstIFO[0x100:0x400]) {
		t.Fatal("VMG VTS_ATRT attributes do not mirror the VTS IFO attributes")
	}
	if firstIFO[0x0204]>>5 != 4 || firstIFO[0x0205]&7 != 1 {
		t.Fatalf("LPCM stream attributes = % x, want 16-bit 48 kHz stereo", firstIFO[0x0204:0x020c])
	}
	vtsMenuCadtSector := binary.BigEndian.Uint32(firstIFO[0x00d8:0x00dc])
	vtsMenuAdmapSector := binary.BigEndian.Uint32(firstIFO[0x00dc:0x00e0])
	vtsMenuCadt := firstIFO[int(vtsMenuCadtSector)*DVDVOBSectorSize:]
	vtsMenuAdmap := firstIFO[int(vtsMenuAdmapSector)*DVDVOBSectorSize:]
	if binary.BigEndian.Uint16(vtsMenuCadt[0:2]) != 1 || binary.BigEndian.Uint16(vtsMenuCadt[8:10]) != 1 ||
		vtsMenuCadt[10] != 1 || binary.BigEndian.Uint32(vtsMenuCadt[16:20]) != menuSectors-1 ||
		binary.BigEndian.Uint32(vtsMenuAdmap[4:8]) != 0 {
		t.Fatal("VTS menu cell/VOBU address tables do not cover the menu VOB")
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
	if got := binary.BigEndian.Uint16(pgc[0x00ec+2 : 0x00ec+4]); got != 2 {
		t.Fatalf("first title post-command count = %d, want 2", got)
	}
	if got := binary.BigEndian.Uint16(pgc[0x00ec+6 : 0x00ec+8]); got != 23 {
		t.Fatalf("title PGC command table last byte = %d, want 23", got)
	}
	if !bytes.Equal(pgc[0xf4:0x00fc], []byte{0x30, 0x38, 0, 5, 0, 0xc0, 0, 1}) ||
		!bytes.Equal(pgc[0x00fc:0x0104], []byte{0x30, 0x08, 0, 0, 0, 0x42, 0, 0}) {
		t.Fatalf("first title post-commands are not a conditional continuation followed by a VMG menu call: % x", pgc[0xf4:0x0104])
	}
	cellPlaybackOffset := int(binary.BigEndian.Uint16(pgc[0x00e8:0x00ea]))
	cellPositionOffset := int(binary.BigEndian.Uint16(pgc[0x00ea:0x00ec]))
	if got := binary.BigEndian.Uint32(pgc[cellPlaybackOffset+8 : cellPlaybackOffset+12]); got != 0 {
		t.Fatalf("title cell starts at VOB sector %d, want sector 0", got)
	}
	if got := binary.BigEndian.Uint16(pgc[cellPositionOffset : cellPositionOffset+2]); got != 1 || pgc[cellPositionOffset+3] != 1 {
		t.Fatalf("title cell position is VOB %d/cell %d, want VOB 1/cell 1", got, pgc[cellPositionOffset+3])
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

	isoPath := filepath.Join(t.TempDir(), "authored.iso")
	if err := WriteDVDISO(context.Background(), videoTS, isoPath); err != nil {
		t.Fatalf("write ISO for titled DVD with a menu VOB: %v", err)
	}
	if info, err := os.Stat(isoPath); err != nil || info.Size() == 0 {
		t.Fatalf("authored ISO is missing or empty: info=%v err=%v", info, err)
	}
}

func TestDVDMenuPGCIContainsPagePGCs(t *testing.T) {
	menu := ExportBuildDVDVMGMenuPGCI(20, [4]byte{0, 0, 1, 0xc0}, []TestDVDVOBURecord{
		{StartSector: 0, EndSector: 9}, {StartSector: 10, EndSector: 19},
	})
	if len(menu) < 32 || binary.BigEndian.Uint16(menu[16:18]) != 3 {
		t.Fatalf("menu language unit page count is invalid")
	}
	unit := menu[16:]
	for page := range 2 {
		searchEntry := 8 + page*8
		wantEntryID := byte(0)
		if page == 0 {
			wantEntryID = 0x82
		}
		if got := unit[searchEntry]; got != wantEntryID {
			t.Fatalf("menu page %d entry ID = %#x, want %#x", page+1, got, wantEntryID)
		}
		if !bytes.Equal(unit[searchEntry+1:searchEntry+4], []byte{0, 0, 0}) {
			t.Fatalf("menu page %d reserved search bytes are nonzero: % x", page+1, unit[searchEntry+1:searchEntry+4])
		}
		pgcOffset := int(binary.BigEndian.Uint32(unit[searchEntry+4 : searchEntry+8]))
		pgc := unit[pgcOffset:]
		cellCommandCount := binary.BigEndian.Uint16(pgc[0xec+4 : 0xec+6])
		cellPlaybackOffset := int(binary.BigEndian.Uint16(pgc[0x00e8:0x00ea]))
		if cellCommandCount != 0 || pgc[cellPlaybackOffset+3] != 0 {
			t.Fatalf("menu page %d references a cell command without a cell command table", page+1)
		}
		if pgc[cellPlaybackOffset+2] != 0xff {
			t.Fatalf("menu page %d still time = %d, want 255 (indefinite)", page+1, pgc[cellPlaybackOffset+2])
		}
		if !bytes.Equal(pgc[4:8], []byte{0, 0, 1, 0xc0}) ||
			!bytes.Equal(pgc[4:8], pgc[cellPlaybackOffset+4:cellPlaybackOffset+8]) {
			t.Fatalf("menu page %d PGC duration does not match its cell playback duration", page+1)
		}
		if got := binary.BigEndian.Uint16(pgc[0xec+6 : 0xec+8]); got != 23 {
			t.Fatalf("menu page %d command table last byte = %d, want 23", page+1, got)
		}
		positionOffset := int(binary.BigEndian.Uint16(pgc[0x00ea:0x00ec]))
		if got, want := pgc[positionOffset+3], byte(page+1); got != want {
			t.Fatalf("menu page %d cell ID = %d, want %d", page+1, got, want)
		}
	}
	if got := unit[8+2*8]; got != 0 {
		t.Fatalf("PLAY ALL entry ID = %#x, want an internal PGC", got)
	}
	playAllOffset := int(binary.BigEndian.Uint32(unit[8+2*8+4 : 8+3*8]))
	playAll := unit[playAllOffset:]
	if got := binary.BigEndian.Uint16(playAll[0xec+6 : 0xec+8]); got != 23 {
		t.Fatalf("PLAY ALL command table last byte = %d, want 23", got)
	}
	vtsMenu := ExportBuildDVDVMGMenuPGCI(20, [4]byte{0, 0, 1, 0xc0}, []TestDVDVOBURecord{
		{StartSector: 0, EndSector: 9},
	}, 2, 1)
	vtsUnit := vtsMenu[16:]
	vtsPlayAllOffset := int(binary.BigEndian.Uint32(vtsUnit[8+1*8+4 : 8+2*8]))
	vtsPlayAll := vtsUnit[vtsPlayAllOffset:]
	if got := vtsPlayAll[0xfc+1]; got != 0x03 {
		t.Fatalf("VTS menu PLAY ALL does not use JumpVTS_TT: opcode byte = %#x, want %#x", got, 0x03)
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
			frame = ExportCloneDecodedVideoFrame(frame)
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
