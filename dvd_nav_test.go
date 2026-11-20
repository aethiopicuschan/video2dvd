package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
	"slices"
	"testing"
	"time"
)

func TestBuildDVDNAVPackWritesPCIAndDSIAtFixedOffsets(t *testing.T) {
	info := DVDNAVPackInfo{
		SCRBase:                   90_000,
		SCRExtension:              123,
		LogicalBlockNumber:        321,
		VOBUEndAddress:            14,
		NextVOBU:                  0x8000000f,
		PreviousVOBU:              0x80000004,
		FirstReferenceEndAddress:  4,
		SecondReferenceEndAddress: 8,
		ThirdReferenceEndAddress:  12,
		VOBID:                     1,
		CellID:                    2,
		StartPTM:                  180_000,
		EndPTM:                    225_000,
		SequenceEndPTM:            225_000,
		VOBStartPTM:               30_003,
		VOBEndPTM:                 900_000,
		ForwardVOBUs:              [19]uint32{17: 0x8000001e, 18: 0x8000000f},
		BackwardVOBUs:             [19]uint32{0: 0x80000004},
		AudioSync:                 [8]uint16{0x8003},
		CellElapsedTime:           [4]byte{0x00, 0x00, 0x10, 0x00},
	}
	pack, err := BuildDVDNAVPack(info)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) != DVDVOBSectorSize {
		t.Fatalf("NAV pack size = %d, want %d", len(pack), DVDVOBSectorSize)
	}
	systemHeader := pack[14:]
	if got := uint32(systemHeader[6]&0x7f)<<15 | uint32(systemHeader[7])<<7 | uint32(systemHeader[8]>>1); got != 25_200 {
		t.Fatalf("DVD system-header rate_bound = %d units of 50 bytes/s, want 25200 (10.08 Mbit/s)", got)
	}
	if !bytes.Equal(pack[:4], []byte{0, 0, 1, 0xba}) {
		t.Fatal("NAV pack has no MPEG pack header")
	}
	if !bytes.Equal(pack[14:18], []byte{0, 0, 1, 0xbb}) || binary.BigEndian.Uint16(pack[18:20]) != 18 {
		t.Fatal("NAV pack has an invalid 24-byte system header")
	}
	// The header must hold exactly the stream entries its length announces:
	// all video 232 KiB, all MPEG audio 4 KiB, private_stream_1 58 KiB and
	// private_stream_2 2 KiB.
	wantSystemHeader := []byte{
		0x00, 0x00, 0x01, 0xbb, 0x00, 0x12, 0x80, 0xc4, 0xe1, 0x00, 0xe1, 0x7f,
		0xb9, 0xe0, 0xe8, 0xb8, 0xc0, 0x20, 0xbd, 0xe0, 0x3a, 0xbf, 0xe0, 0x02,
	}
	if !bytes.Equal(pack[14:38], wantSystemHeader) {
		t.Fatalf("NAV system header = % x, want % x", pack[14:38], wantSystemHeader)
	}
	if got := decodeTestMPEG2SCR(pack[4:14])*300 + decodeTestMPEG2SCRExtension(pack[4:14]); got != 90_000*300+123 {
		t.Fatalf("NAV pack SCR = %d (27 MHz), want %d", got, 90_000*300+123)
	}
	if !bytes.Equal(pack[38:42], []byte{0, 0, 1, 0xbf}) || binary.BigEndian.Uint16(pack[42:44]) != 980 || pack[44] != 0 {
		t.Fatal("NAV pack PCI packet has invalid framing")
	}
	pci := pack[45:]
	if binary.BigEndian.Uint32(pci[0:4]) != info.LogicalBlockNumber ||
		binary.BigEndian.Uint32(pci[12:16]) != info.StartPTM ||
		binary.BigEndian.Uint32(pci[16:20]) != info.EndPTM ||
		binary.BigEndian.Uint32(pci[20:24]) != info.SequenceEndPTM ||
		!bytes.Equal(pci[24:28], info.CellElapsedTime[:]) {
		t.Fatal("NAV pack PCI general information does not match the input")
	}
	if !bytes.Equal(pack[0x400:0x404], []byte{0, 0, 1, 0xbf}) || binary.BigEndian.Uint16(pack[0x404:0x406]) != 1018 || pack[0x406] != 1 {
		t.Fatal("NAV pack DSI packet has invalid framing")
	}
	dsi := pack[0x407:]
	if binary.BigEndian.Uint32(dsi[0:4]) != uint32(info.SCRBase) ||
		binary.BigEndian.Uint32(dsi[4:8]) != info.LogicalBlockNumber ||
		binary.BigEndian.Uint32(dsi[8:12]) != info.VOBUEndAddress ||
		binary.BigEndian.Uint32(dsi[12:16]) != info.FirstReferenceEndAddress ||
		binary.BigEndian.Uint32(dsi[16:20]) != info.SecondReferenceEndAddress ||
		binary.BigEndian.Uint32(dsi[20:24]) != info.ThirdReferenceEndAddress ||
		binary.BigEndian.Uint16(dsi[24:26]) != info.VOBID || dsi[27] != info.CellID ||
		!bytes.Equal(dsi[28:32], info.CellElapsedTime[:]) {
		t.Fatal("NAV pack DSI general information does not match the input")
	}
	const vobuSriOffset = 32 + 148 + 54
	if binary.BigEndian.Uint32(dsi[vobuSriOffset:vobuSriOffset+4]) != info.NextVOBU ||
		binary.BigEndian.Uint32(dsi[dvdNavTestNextVOBUOffset:dvdNavTestNextVOBUOffset+4]) != info.NextVOBU ||
		binary.BigEndian.Uint32(dsi[dvdNavTestPreviousVOBUOffset:dvdNavTestPreviousVOBUOffset+4]) != info.PreviousVOBU ||
		binary.BigEndian.Uint32(dsi[dvdNavTestPreviousVideoOffset:dvdNavTestPreviousVideoOffset+4]) != info.PreviousVOBU {
		t.Fatal("NAV pack DSI VOBU search pointers do not match the input")
	}
	if binary.BigEndian.Uint32(dsi[44:48]) != info.VOBStartPTM || binary.BigEndian.Uint32(dsi[48:52]) != info.VOBEndPTM {
		t.Fatal("NAV pack DSI seamless playback information does not carry the VOB presentation span")
	}
	for i := range 19 {
		wantForward, wantBackward := info.ForwardVOBUs[i], info.BackwardVOBUs[i]
		// Unset scan entries must read as "no such VOBU", never as zero.
		if wantForward == 0 {
			wantForward = DVDNAVSRIEndOfCell
		}
		if wantBackward == 0 {
			wantBackward = DVDNAVSRIEndOfCell
		}
		if got := binary.BigEndian.Uint32(dsi[vobuSriOffset+4+i*4:]); got != wantForward {
			t.Fatalf("FWDA[%d] = %#x, want %#x", i, got, wantForward)
		}
		if got := binary.BigEndian.Uint32(dsi[dvdNavTestPreviousVOBUOffset+4+i*4:]); got != wantBackward {
			t.Fatalf("BWDA[%d] = %#x, want %#x", i, got, wantBackward)
		}
	}
	if got := binary.BigEndian.Uint16(dsi[dvdNavTestAudioSyncOffset:]); got != 0x8003 {
		t.Fatalf("A_SYNCA[0] = %#x, want 0x8003", got)
	}
}

func TestDVDVOBUSearchInfoFollowsPresentationTime(t *testing.T) {
	// 600 VOBUs of 15 NTSC frames (0.5005 s), 100 sectors apart, in one cell.
	const count, duration, sectors = 600, 45_045, 100
	vobus := make([]TestDVDVOBUSearchEntry, count)
	for i := range vobus {
		vobus[i] = TestDVDVOBUSearchEntry{Sector: uint32(i * sectors), StartPTM: uint32(30_003 + i*duration), EndPTM: uint32(30_003 + (i+1)*duration)}
	}
	steps := [19]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 20, 60, 120, 240}
	for _, index := range []int{0, 1, 14, 300, 598, 599} {
		info := ExportDVDVOBUSearchInfo(vobus, 0, count-1, index)
		pointer := func(target int, skip bool) uint32 {
			var flags uint32
			if skip {
				flags = 0x40000000
			}
			if target < 0 || target >= count {
				return DVDNAVSRIEndOfCell | flags
			}
			return 0x80000000 | flags | uint32(max(target-index, index-target)*sectors)
		}
		if info.NextVOBU != pointer(index+1, false) || info.PreviousVOBU != pointer(index-1, false) {
			t.Fatalf("VOBU %d next/previous = %#x/%#x", index, info.NextVOBU, info.PreviousVOBU)
		}
		for i, step := range steps {
			// n half seconds ahead is still inside the VOBU n-1 further on,
			// because each VOBU lasts slightly more than half a second;
			// n half seconds back is exactly n VOBUs back.
			forward := min(index+step-1, count)
			backward := max(index-step, -1)
			// The 10 s and longer entries are flagged when they pass over
			// VOBUs that the previous, shorter entry did not reach.
			skipForward := i >= 15 && forward-index > 1 && forward > min(index+steps[i-1]-1, count)+1
			skipBackward := i >= 15 && index-backward > 1 && backward < max(index-steps[i-1], -1)-1
			if got, want := info.ForwardVOBUs[18-i], pointer(forward, skipForward); got != want {
				t.Fatalf("VOBU %d +%d half seconds = %#x, want %#x", index, step, got, want)
			}
			if got, want := info.BackwardVOBUs[i], pointer(backward, skipBackward); got != want {
				t.Fatalf("VOBU %d -%d half seconds = %#x, want %#x", index, step, got, want)
			}
		}
	}
	// Pointers never leave the cell, even when the neighbours exist.
	inner := ExportDVDVOBUSearchInfo(vobus, 100, 102, 101)
	if inner.NextVOBU != 0x80000000|sectors || inner.PreviousVOBU != 0x80000000|sectors ||
		inner.ForwardVOBUs[16]&0x3fffffff != DVDNAVSRIEndOfCell || inner.BackwardVOBUs[1]&0x3fffffff != DVDNAVSRIEndOfCell {
		t.Fatalf("search pointers cross the cell boundary: %+v", inner)
	}
}

func TestBuildDVDNAVPackRejectsMissingIDs(t *testing.T) {
	if _, err := BuildDVDNAVPack(DVDNAVPackInfo{}); err == nil {
		t.Fatal("expected a NAV pack without VOB/cell IDs to fail")
	}
}

func TestBuildDVDNAVPackWritesValidHighlightGroup(t *testing.T) {
	pack, err := BuildDVDNAVPack(DVDNAVPackInfo{
		VOBID: 1, CellID: 1, ButtonDisplayType: 0x07, StartPTM: 30_003, EndPTM: 33_006,
		Buttons: []DVDNAVButton{{
			XStart: 10, YStart: 10, XEnd: 100, YEnd: 30,
			Up: 1, Down: 1, Left: 1, Right: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pci := pack[45:]
	const highlightInfo = 0x60
	if got := binary.BigEndian.Uint16(pci[highlightInfo+0x0e:]); got != 0x1700 {
		t.Fatalf("highlight group flags = %#x, want 0x1700 (reserved bits clear, one group, all display modes)", got)
	}
	// HLI_SS 1: new highlight information, valid from the start of the VOBU
	// (HLI_S_PTM) with no end of the highlight or of the button selection.
	if got := binary.BigEndian.Uint16(pci[highlightInfo:]); got != 1 {
		t.Fatalf("highlight status = %d, want 1", got)
	}
	if !bytes.Equal(pci[highlightInfo+2:highlightInfo+6], pci[12:16]) || binary.BigEndian.Uint32(pci[12:16]) != 30_003 {
		t.Fatalf("highlight start time = % x, want the VOBU start time % x", pci[highlightInfo+2:highlightInfo+6], pci[12:16])
	}
	if !bytes.Equal(pci[highlightInfo+6:highlightInfo+14], bytes.Repeat([]byte{0xff}, 8)) {
		t.Fatalf("highlight and button-selection end times = % x, want no end", pci[highlightInfo+6:highlightInfo+14])
	}
	// BTN_OFN 0: the user's button numbers are the button numbers. One
	// button, which is also selectable by number.
	if !bytes.Equal(pci[highlightInfo+0x10:highlightInfo+0x14], []byte{0, 1, 1, 0}) {
		t.Fatalf("button offset/count/numeric count/reserved = % x, want 00 01 01 00", pci[highlightInfo+0x10:highlightInfo+0x14])
	}
	// Selection and action recolour pattern pixels only: palette entries 1
	// and 2 with a nonzero contrast, everything else transparent.
	if !bytes.Equal(pci[highlightInfo+0x16:highlightInfo+0x1e], []byte{0x00, 0x10, 0x00, 0x80, 0x00, 0x20, 0x00, 0xc0}) {
		t.Fatalf("highlight colours = % x", pci[highlightInfo+0x16:highlightInfo+0x1e])
	}
	if !bytes.Equal(pci[highlightInfo+0x1e:highlightInfo+0x2e], make([]byte, 16)) {
		t.Fatalf("unused colour schemes = % x, want zero", pci[highlightInfo+0x1e:highlightInfo+0x2e])
	}
	// Button 1 uses colour scheme 1 and spans (10,10)-(100,30).
	if !bytes.Equal(pci[highlightInfo+0x2e:highlightInfo+0x38], []byte{0x40, 0xa0, 0x64, 0x00, 0xa0, 0x1e, 1, 1, 1, 1}) {
		t.Fatalf("button 1 = % x", pci[highlightInfo+0x2e:highlightInfo+0x38])
	}
	// No subpicture stream is declared unless the caller names one.
	if !bytes.Equal(pack[0x407+dvdNavTestSubpictureSyncOffset:][:32*4], make([]byte, 32*4)) {
		t.Fatal("SP_SYNCA is not zero for a VOBU without subpicture streams")
	}
	if pci[highlightInfo+0x15] != 0 {
		t.Fatalf("forced-action button = %d, want 0", pci[highlightInfo+0x15])
	}
	if pci[highlightInfo+0x14] != 1 {
		t.Fatalf("forced-selection button = %d, want 1", pci[highlightInfo+0x14])
	}
	dsi := pack[0x407:]
	if got := binary.BigEndian.Uint32(dsi[dvdNavTestNextVOBUOffset : dvdNavTestNextVOBUOffset+4]); got != DVDNAVSRIEndOfCell {
		t.Fatalf("default next VOBU pointer = %#x, want end-of-cell %#x", got, DVDNAVSRIEndOfCell)
	}
	if got := binary.BigEndian.Uint32(dsi[dvdNavTestPreviousVOBUOffset : dvdNavTestPreviousVOBUOffset+4]); got != DVDNAVSRIEndOfCell {
		t.Fatalf("default previous VOBU pointer = %#x, want end-of-cell %#x", got, DVDNAVSRIEndOfCell)
	}
}

func TestBuildDVDNAVPackWritesButtonGroupsAndSubpictureSync(t *testing.T) {
	wide := []DVDNAVButton{
		{XStart: 48, YStart: 92, XEnd: 671, YEnd: 109, Up: 2, Down: 2, Left: 1, Right: 1, Command: [8]byte{0x20, 0x04, 0, 0, 0, 0, 0, 3}},
		{XStart: 48, YStart: 112, XEnd: 671, YEnd: 129, Up: 1, Down: 1, Left: 2, Right: 2, Command: [8]byte{0x20, 0x04, 0, 0, 0, 0, 0, 4}},
	}
	letterbox := []DVDNAVButton{wide[0], wide[1]}
	letterbox[0].YStart, letterbox[0].YEnd = 128, 141
	letterbox[1].YStart, letterbox[1].YEnd = 144, 157
	info := DVDNAVPackInfo{
		VOBID: 1, CellID: 1, StartPTM: 30_003, EndPTM: 33_006,
		ButtonDisplayType: 1, Buttons: wide,
		ExtraButtonGroups: []DVDNAVButtonGroup{{DisplayType: 2, Buttons: letterbox}},
		SubpictureSync:    [32]uint32{DVDNAVSubpictureSyncCurrent, DVDNAVSubpictureSyncCurrent},
	}
	pack, err := BuildDVDNAVPack(info)
	if err != nil {
		t.Fatal(err)
	}
	hli := pack[45+0x60:]
	// Two groups: the first for a widescreen display, the second for the
	// letterboxed picture, no third.
	if got := binary.BigEndian.Uint16(hli[0x0e:]); got != 0x2120 {
		t.Fatalf("button group information = %#04x, want 0x2120", got)
	}
	if hli[0x11] != 2 || hli[0x12] != 2 {
		t.Fatalf("button counts = %d/%d, want the per-group count 2", hli[0x11], hli[0x12])
	}
	// With two groups the 36 table entries are split in halves.
	for group, buttons := range [][]DVDNAVButton{wide, letterbox} {
		for index, button := range buttons {
			entry := hli[0x2e+(group*18+index)*18:][:18]
			yStart := int(entry[3]&0x3f)<<4 | int(entry[4]>>4)
			yEnd := int(entry[4]&0x03)<<8 | int(entry[5])
			if yStart != button.YStart || yEnd != button.YEnd || entry[0]>>6 != 1 || !bytes.Equal(entry[10:], button.Command[:]) ||
				!bytes.Equal(entry[6:10], []byte{button.Up, button.Down, button.Left, button.Right}) {
				t.Errorf("group %d button %d entry = % x, want lines %d-%d", group+1, index+1, entry, button.YStart, button.YEnd)
			}
		}
	}
	if !bytes.Equal(hli[0x2e+2*18:0x2e+18*18], make([]byte, 16*18)) {
		t.Error("unused entries of the first button group are not zero")
	}
	sync := pack[0x407+dvdNavTestSubpictureSyncOffset:]
	for stream := range 32 {
		want := uint32(0)
		if stream < 2 {
			want = 0x7fffffff
		}
		if got := binary.BigEndian.Uint32(sync[stream*4:]); got != want {
			t.Errorf("SP_SYNCA of stream %d = %#x, want %#x", stream, got, want)
		}
	}
	// SP_SYNCA ends where the DSI data ends.
	if dvdNavTestSubpictureSyncOffset+32*4 > 1017 {
		t.Fatal("SP_SYNCA does not fit the DSI packet")
	}

	unequal := info
	unequal.ExtraButtonGroups = []DVDNAVButtonGroup{{DisplayType: 2, Buttons: letterbox[:1]}}
	if _, err := BuildDVDNAVPack(unequal); err == nil {
		t.Error("expected button groups of different sizes to fail")
	}
	many := info
	many.Buttons = slices.Repeat(wide, 10)
	many.ExtraButtonGroups = []DVDNAVButtonGroup{{DisplayType: 2, Buttons: slices.Repeat(letterbox, 10)}}
	if _, err := BuildDVDNAVPack(many); err == nil {
		t.Error("expected 20 buttons in each of two groups to fail: a group holds 18")
	}
	many.ExtraButtonGroups = nil
	if _, err := BuildDVDNAVPack(many); err != nil {
		t.Errorf("20 buttons in a single group: %v", err)
	}
	orphan := info
	orphan.Buttons = nil
	if _, err := BuildDVDNAVPack(orphan); err == nil {
		t.Error("expected an additional button group without a first group to fail")
	}
	dangling := info
	dangling.ExtraButtonGroups = nil
	dangling.Buttons = []DVDNAVButton{{XEnd: 10, YEnd: 10, Up: 1, Down: 2, Left: 1, Right: 1}}
	if _, err := BuildDVDNAVPack(dangling); err == nil {
		t.Error("expected a navigation target beyond the last button to fail")
	}
}

func TestBuildDVDNAVPackRejectsInvalidVOBUSearchPointers(t *testing.T) {
	if _, err := BuildDVDNAVPack(DVDNAVPackInfo{VOBID: 1, CellID: 1, NextVOBU: 1}); err == nil {
		t.Fatal("expected a VOBU pointer without the valid bit to fail")
	}
	if _, err := BuildDVDNAVPack(DVDNAVPackInfo{VOBID: 1, CellID: 1, PreviousVOBU: 0x80000000}); err == nil {
		t.Fatal("expected a valid-bit VOBU pointer with a zero offset to fail")
	}
}

func TestBuildDVDNAVPackRejectsInvalidButtonDisplayType(t *testing.T) {
	if _, err := BuildDVDNAVPack(DVDNAVPackInfo{VOBID: 1, CellID: 1, ButtonDisplayType: 8}); err == nil {
		t.Fatal("expected an out-of-range DVD button display mask to fail")
	}
}

func TestDVDCellElapsedTimeUsesDVDBCDAndFrameRateCode(t *testing.T) {
	// NTSC time codes count 30 frames per second: 111,705 frames of
	// 1001/30000 s are 01:02:03:15, although 3727.2 s of wall time passed.
	ntsc, err := ExportDVDCellElapsedTime(time.Duration(111_705)*time.Second*1001/30000, 30000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if ntsc != [4]byte{0x01, 0x02, 0x03, 0xd5} {
		t.Fatalf("NTSC cell time = % x, want 01 02 03 d5 (frame 15 in BCD)", ntsc)
	}
	pal, err := ExportDVDCellElapsedTime(time.Second+960*time.Millisecond, 25, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pal != [4]byte{0x00, 0x00, 0x01, 0x64} {
		t.Fatalf("PAL cell time = % x, want 00 00 01 64 (frame 24 in BCD)", pal)
	}
}

const (
	dvdNavTestNextVOBUOffset      = 0x13a
	dvdNavTestPreviousVOBUOffset  = 0x13e
	dvdNavTestPreviousVideoOffset = 0x18e
	dvdNavTestAudioSyncOffset     = 0x192
	// SP_SYNCA follows the eight 16-bit A_SYNCA entries.
	dvdNavTestSubpictureSyncOffset = 0x1a2
)
