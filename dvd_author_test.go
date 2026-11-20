package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
	"os"
	"path/filepath"
	"slices"
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
	// The management table is the first sector; the first-play PGC must lie
	// inside it for libdvdread's first_play_pgc < vmgi_last_byte check.
	if got := binary.BigEndian.Uint32(vmg[0x0080:0x0084]); got != 0x7ff {
		t.Fatalf("VMGI management table last byte = %#x, want 0x7ff", got)
	}
	firstPlayOffset := int(binary.BigEndian.Uint32(vmg[0x0084:0x0088]))
	if firstPlayOffset < 0x400 || firstPlayOffset+0x104 > 0x800 {
		t.Fatalf("first-play PGC at %#x does not lie inside the management table sector", firstPlayOffset)
	}
	if got := binary.BigEndian.Uint16(vmg[0x0102:0x0104]); got != 1 || vmg[0x0104] != 0x80 || vmg[0x0105] != 0x01 {
		t.Fatalf("VMGM audio attributes = %d stream(s) % x, want one 16-bit 48 kHz stereo LPCM stream", got, vmg[0x0104:0x010c])
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
	for title := range 2 {
		if got := tt[8+title*12]; got != 0x3c {
			t.Fatalf("title %d playback type = %#x, want 0x3c (no user-operation prohibitions)", title+1, got)
		}
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
	// The menu VOB carries the highlight subpicture, without which the
	// selected button is invisible: substream 0x20 for a widescreen display
	// and 0x21 for the letterboxed picture. The IFO declares it as one
	// stream, and the NAV pack addresses its palette.
	if got := authorerTestSubpictureStreams(menuVOB); !bytes.Equal(got, []byte{0x20, 0x21}) {
		t.Fatalf("menu VOB subpicture substreams = % x, want 20 21", got)
	}
	menuHighlight := menuVOB[45+0x60:]
	if !bytes.Equal(menuHighlight[2:6], menuVOB[45+12:45+16]) || !bytes.Equal(menuHighlight[6:14], bytes.Repeat([]byte{0xff}, 8)) {
		t.Fatalf("menu highlight times = % x, want the VOBU start time and no end", menuHighlight[2:14])
	}
	if got := binary.BigEndian.Uint16(vmg[0x0154:0x0156]); got != 1 || !bytes.Equal(vmg[0x0156:0x015c], make([]byte, 6)) {
		t.Fatalf("VMGM declares %d subpicture stream(s) with attributes % x, want one plain stream", got, vmg[0x0156:0x015c])
	}
	if !bytes.Equal(menuPGC[0x1c:0x20], []byte{0x80, 0, 1, 0}) || !bytes.Equal(menuHighlight[0x16:0x1a], ExportDVDMenuSelectionColor[:]) {
		t.Fatalf("menu PGC subpicture control % x / selection colour % x, want 80 00 01 00 / % x",
			menuPGC[0x1c:0x20], menuHighlight[0x16:0x1a], ExportDVDMenuSelectionColor)
	}
	// 16:9 NTSC MPEG-2 video that a 4:3 display may only letterbox: there
	// is no highlight for pan-and-scan.
	if got := binary.BigEndian.Uint16(vmg[0x0100:0x0102]); got != 0x4e00 {
		t.Fatalf("VMGM video attributes = %#04x, want 0x4e00", got)
	}
	if got := authorerTestSubpictureStreams(readAuthorerTestFile(t, videoTS, "VTS_01_1.VOB")); len(got) != 0 {
		t.Fatalf("title VOB carries subpicture substreams % x", got)
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

	// A title set has no menu VOB. Its root menu is a command-only PGC that
	// forwards to the VMG title menu.
	firstIFO := readAuthorerTestFile(t, videoTS, "VTS_01_0.IFO")
	if _, err := os.Stat(filepath.Join(videoTS, "VTS_01_0.VOB")); !os.IsNotExist(err) {
		t.Fatalf("VTS_01_0.VOB exists although the title set has no menu: %v", err)
	}
	for _, offset := range []int{0x00c0, 0x00d8, 0x00dc} {
		if got := binary.BigEndian.Uint32(firstIFO[offset : offset+4]); got != 0 {
			t.Fatalf("VTSI menu VOB pointer at %#x = %d, want 0", offset, got)
		}
	}
	if !bytes.Equal(firstIFO[0x100:0x200], make([]byte, 0x100)) {
		t.Fatal("VTSM attributes are set although the title set has no menu VOB")
	}
	vtsMenuTableSector := binary.BigEndian.Uint32(firstIFO[0x00d0:0x00d4])
	vtsMenuTable := firstIFO[int(vtsMenuTableSector)*DVDVOBSectorSize:]
	if vtsMenuTable[11] != 0x80 {
		t.Fatalf("VTSM language unit menu existence = %#x, want 0x80 (root menu)", vtsMenuTable[11])
	}
	vtsMenuUnit := vtsMenuTable[int(binary.BigEndian.Uint32(vtsMenuTable[12:16])):]
	if got := binary.BigEndian.Uint16(vtsMenuUnit[0:2]); got != 1 || vtsMenuUnit[8] != 0x83 {
		t.Fatalf("VTSM has %d PGC(s) with entry ID %#x, want one root-menu PGC (0x83)", got, vtsMenuUnit[8])
	}
	vtsMenuPGC := vtsMenuUnit[int(binary.BigEndian.Uint32(vtsMenuUnit[12:16])):]
	if vtsMenuPGC[2] != 0 || vtsMenuPGC[3] != 0 || binary.BigEndian.Uint16(vtsMenuPGC[0xec:0xee]) != 1 ||
		!bytes.Equal(vtsMenuPGC[0xf4:0xfc], []byte{0x30, 0x06, 0, 0, 0, 0x42, 0, 0}) {
		t.Fatal("VTSM root menu is not a command-only PGC that jumps to the VMG title menu")
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
	if secondVTSStart != firstVTSStart+firstIFOSectors*2+firstVOBSectors {
		t.Fatalf("second VTS sector = %d, want %d", secondVTSStart, firstVTSStart+firstIFOSectors*2+firstVOBSectors)
	}
	if got, want := binary.BigEndian.Uint32(firstIFO[0x000c:0x0010]), firstIFOSectors*2+firstVOBSectors-1; got != want {
		t.Fatalf("VTS last sector = %d, want %d", got, want)
	}

	if string(firstIFO[:12]) != "DVDVIDEO-VTS" {
		t.Fatalf("VTSI signature = %q", firstIFO[:12])
	}
	if got := binary.BigEndian.Uint32(firstIFO[0x0080:0x0084]); got != 0x7ff {
		t.Fatalf("VTSI management table last byte = %#x, want 0x7ff", got)
	}
	if got := binary.BigEndian.Uint32(firstIFO[0x00C4:0x00C8]); got != firstIFOSectors {
		t.Fatalf("title VOB starts at sector %d, want %d", got, firstIFOSectors)
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
	records := []TestDVDVOBURecord{{StartSector: 0, EndSector: 9}, {StartSector: 10, EndSector: 19}}
	menu := ExportBuildDVDVMGMenuPGCI(20, [4]byte{0, 0, 1, 0xc0}, records, 0, [4]byte{})
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
		cell := pgc[cellPlaybackOffset:]
		first, lastVOBU, last := binary.BigEndian.Uint32(cell[8:12]), binary.BigEndian.Uint32(cell[16:20]), binary.BigEndian.Uint32(cell[20:24])
		if first != records[page].StartSector || lastVOBU != first || last != records[page].EndSector {
			t.Fatalf("menu page %d cell sectors first/last VOBU/last = %d/%d/%d, want %d/%d/%d",
				page+1, first, lastVOBU, last, records[page].StartSector, records[page].StartSector, records[page].EndSector)
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
	// A command-only PGC has no programs or cells, so its table offsets
	// must be zero instead of pointing past its end.
	if playAll[2] != 0 || playAll[3] != 0 || !bytes.Equal(playAll[0xe6:0xec], make([]byte, 6)) {
		t.Fatalf("PLAY ALL PGC declares %d program(s), %d cell(s) and table offsets % x", playAll[2], playAll[3], playAll[0xe6:0xec])
	}
	if !bytes.Equal(playAll[0xf4:0x104], []byte{0x71, 0, 0, 0, 0, 1, 0, 0, 0x30, 0x02, 0, 0, 0, 1, 0, 0}) {
		t.Fatalf("PLAY ALL commands = % x, want set GPRM0=1 then JumpTT 1", playAll[0xf4:0x104])
	}
}

func TestDVDMenuPGCDeclaresSubpictureOnlyWhenPresent(t *testing.T) {
	records := []TestDVDVOBURecord{{StartSector: 0, EndSector: 9}}
	// A 4:3 menu decodes substream 0x20. A 16:9 menu decodes 0x20 on a
	// widescreen display and 0x21 when letterboxed.
	narrow, wide := ExportDVDMenuSubpictureControl(AspectRatio4x3), ExportDVDMenuSubpictureControl(AspectRatio16x9)
	if narrow != [4]byte{0x80, 0, 0, 0} || wide != [4]byte{0x80, 0, 1, 0} {
		t.Fatalf("subpicture stream control = % x (4:3) and % x (16:9), want 80 00 00 00 and 80 00 01 00", narrow, wide)
	}
	for _, control := range [][4]byte{{}, narrow, wide} {
		subpicture := control != [4]byte{}
		unit := ExportBuildDVDVMGMenuPGCI(10, [4]byte{0, 0, 1, 0xc0}, records, 1, control)[16:]
		pgc := unit[int(binary.BigEndian.Uint32(unit[12:16])):]
		wantControl := control[:]
		var wantPalette [16][4]byte
		if subpicture {
			wantPalette = ExportDVDMenuSubpicturePalette
		}
		if !bytes.Equal(pgc[0x1c:0x20], wantControl) {
			t.Errorf("subpicture=%v: PGC subpicture stream control = % x, want % x", subpicture, pgc[0x1c:0x20], wantControl)
		}
		for i, color := range wantPalette {
			if !bytes.Equal(pgc[0xa4+i*4:0xa8+i*4], color[:]) {
				t.Errorf("subpicture=%v: PGC palette entry %d = % x, want % x", subpicture, i, pgc[0xa4+i*4:0xa8+i*4], color)
			}
		}
	}
	profile, err := ExportProfile(Options{Standard: VideoStandardPAL, Aspect: AspectRatio4x3})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		aspect    AspectRatio
		control   [4]byte
		wantCount uint16
		wantVideo uint16
	}{
		{aspect: AspectRatio4x3, wantVideo: 0x5000},
		{aspect: AspectRatio4x3, control: narrow, wantCount: 1, wantVideo: 0x5000},
		{aspect: AspectRatio16x9, wantVideo: 0x5c00},
		// Only a 16:9 menu with a highlight restricts a 4:3 display to
		// letterboxing.
		{aspect: AspectRatio16x9, control: wide, wantCount: 1, wantVideo: 0x5e00},
	} {
		vmg, err := ExportBuildDVDVMGI([]EncodedTitle{{Aspect: tc.aspect}}, profile, 10, records, tc.control)
		if err != nil {
			t.Fatal(err)
		}
		if got := binary.BigEndian.Uint16(vmg[0x154:0x156]); got != tc.wantCount {
			t.Errorf("aspect %v, control % x: VMGM subpicture stream count = %d, want %d", tc.aspect, tc.control, got, tc.wantCount)
		}
		if got := binary.BigEndian.Uint16(vmg[0x100:0x102]); got != tc.wantVideo {
			t.Errorf("aspect %v, control % x: VMGM video attributes = %#04x, want %#04x", tc.aspect, tc.control, got, tc.wantVideo)
		}
	}
}

func TestDVDVTSTimeMapIndexesVOBUsByElapsedTime(t *testing.T) {
	entries := func(tmap []byte) []uint32 {
		t.Helper()
		if binary.BigEndian.Uint16(tmap[0:2]) != 1 || binary.BigEndian.Uint16(tmap[2:4]) != 0 ||
			int(binary.BigEndian.Uint32(tmap[4:8])) != len(tmap)-1 || binary.BigEndian.Uint32(tmap[8:12]) != 12 || tmap[13] != 0 {
			t.Fatalf("time map table header = % x for %d bytes", tmap[:14], len(tmap))
		}
		count := int(binary.BigEndian.Uint16(tmap[14:16]))
		if len(tmap) != 16+4*count {
			t.Fatalf("time map declares %d entries in %d bytes", count, len(tmap))
		}
		values := make([]uint32, count)
		for i := range values {
			values[i] = binary.BigEndian.Uint32(tmap[16+4*i:])
		}
		return values
	}
	// VOBUs of 0.5 s whose clock starts at 30003 ticks, as the muxer's does,
	// each ten sectors long.
	makeRecords := func(count int, ticks uint32) []TestDVDVOBURecord {
		records := make([]TestDVDVOBURecord, count)
		for i := range records {
			records[i] = TestDVDVOBURecord{
				StartSector: uint32(i) * 10, EndSector: uint32(i)*10 + 9,
				StartPTM: 30_003 + uint32(i)*ticks, EndPTM: 30_003 + uint32(i+1)*ticks,
			}
		}
		return records
	}

	// 3.5 s of PAL video: entries for 1 s, 2 s and 3 s, which lie in the
	// third, fifth and seventh VOBU. No VOBU is presented at 4 s, so the
	// last entry carries the discontinuity bit.
	tmap := ExportBuildDVDVTSTimeMap(makeRecords(7, 45_000), 90_000)
	if tmap[12] != 1 {
		t.Fatalf("time unit = %d s, want 1", tmap[12])
	}
	if got, want := entries(tmap), []uint32{20, 40, 60 | 1<<31}; !slices.Equal(got, want) {
		t.Fatalf("time map entries = %#x, want %#x", got, want)
	}
	// A title of exactly three seconds ends at 3 s: that instant is not
	// part of it and gets no entry.
	if got, want := entries(ExportBuildDVDVTSTimeMap(makeRecords(6, 45_000), 90_000)), []uint32{20, 40 | 1<<31}; !slices.Equal(got, want) {
		t.Fatalf("time map entries of a three-second title = %#x, want %#x", got, want)
	}
	// NTSC time code counts 30 frames, 90090 ticks, as one second.
	if got, want := entries(ExportBuildDVDVTSTimeMap(makeRecords(5, 45_045), 90_090)), []uint32{20, 40 | 1<<31}; !slices.Equal(got, want) {
		t.Fatalf("NTSC time map entries = %#x, want %#x", got, want)
	}
	// Shorter than a second: an empty map.
	if tmap := ExportBuildDVDVTSTimeMap(makeRecords(1, 45_000), 90_000); len(entries(tmap)) != 0 || tmap[12] != 1 {
		t.Fatalf("time map of a half-second title = % x, want no entries and a one-second unit", tmap)
	}
	// 2 h 30 min would need 9000 one-second entries; the unit grows so that
	// at most 2040 remain.
	long := ExportBuildDVDVTSTimeMap(makeRecords(18_000, 45_000), 90_000)
	longEntries := entries(long)
	if long[12] != 5 || len(longEntries) != 1799 {
		t.Fatalf("long title time map: unit %d s with %d entries, want 5 s and 1799", long[12], len(longEntries))
	}
	for i, entry := range longEntries {
		want := uint32(i+1) * 10 * 10 // ten VOBUs of ten sectors per 5 s.
		if i == len(longEntries)-1 {
			want |= 1 << 31
		}
		if entry != want {
			t.Fatalf("long title time map entry %d = %#x, want %#x", i, entry, want)
		}
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

// authorerTestSubpictureStreams returns the distinct subpicture substream
// IDs (0x20-0x3f) of the private_stream_1 packs of vob, in ascending order.
func authorerTestSubpictureStreams(vob []byte) []byte {
	var streams []byte
	for sector := 0; sector+DVDVOBSectorSize <= len(vob); sector += DVDVOBSectorSize {
		pack := vob[sector : sector+DVDVOBSectorSize]
		if !bytes.Equal(pack[14:18], []byte{0, 0, 1, 0xbd}) {
			continue
		}
		if id := pack[23+int(pack[22])]; id&0xe0 == 0x20 && !slices.Contains(streams, id) {
			streams = append(streams, id)
		}
	}
	slices.Sort(streams)
	return streams
}

func readAuthorerTestFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
