package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
	"testing"
	"time"
)

func TestBuildDVDNAVPackWritesPCIAndDSIAtFixedOffsets(t *testing.T) {
	info := DVDNAVPackInfo{
		SCRBase:                   90_000,
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
		CellElapsedTime:           [4]byte{0x00, 0x00, 0x10, 0x00},
	}
	pack, err := BuildDVDNAVPack(info)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) != DVDVOBSectorSize {
		t.Fatalf("NAV pack size = %d, want %d", len(pack), DVDVOBSectorSize)
	}
	if !bytes.Equal(pack[:4], []byte{0, 0, 1, 0xba}) {
		t.Fatal("NAV pack has no MPEG pack header")
	}
	if !bytes.Equal(pack[14:18], []byte{0, 0, 1, 0xbb}) || binary.BigEndian.Uint16(pack[18:20]) != 18 {
		t.Fatal("NAV pack has an invalid 24-byte system header")
	}
	if !bytes.Equal(pack[38:42], []byte{0, 0, 1, 0xbf}) || binary.BigEndian.Uint16(pack[42:44]) != 980 || pack[44] != 0 {
		t.Fatal("NAV pack PCI packet has invalid framing")
	}
	pci := pack[45:]
	if binary.BigEndian.Uint32(pci[0:4]) != info.LogicalBlockNumber ||
		binary.BigEndian.Uint32(pci[12:16]) != info.StartPTM ||
		binary.BigEndian.Uint32(pci[16:20]) != info.EndPTM ||
		!bytes.Equal(pci[20:24], info.CellElapsedTime[:]) {
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
}

func TestBuildDVDNAVPackRejectsMissingIDs(t *testing.T) {
	if _, err := BuildDVDNAVPack(DVDNAVPackInfo{}); err == nil {
		t.Fatal("expected a NAV pack without VOB/cell IDs to fail")
	}
}

func TestBuildDVDNAVPackWritesValidHighlightGroup(t *testing.T) {
	pack, err := BuildDVDNAVPack(DVDNAVPackInfo{
		VOBID: 1, CellID: 1, ButtonDisplayType: 0x07,
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
	if got := pci[highlightInfo+0x0e]; got != 0x17 {
		t.Fatalf("highlight group flags = %#x, want 0x17 (reserved bits clear, one group, all display modes)", got)
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
	ntsc, err := ExportDVDCellElapsedTime(time.Hour+2*time.Minute+3*time.Second+500*time.Millisecond, 30000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if ntsc != [4]byte{0x01, 0x02, 0x03, 0xcf} {
		t.Fatalf("NTSC cell time = % x, want 01 02 03 cf", ntsc)
	}
	pal, err := ExportDVDCellElapsedTime(time.Second+960*time.Millisecond, 25, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pal != [4]byte{0x00, 0x00, 0x01, 0x58} {
		t.Fatalf("PAL cell time = % x, want 00 00 01 58", pal)
	}
}

const (
	dvdNavTestNextVOBUOffset      = 0x13a
	dvdNavTestPreviousVOBUOffset  = 0x13e
	dvdNavTestPreviousVideoOffset = 0x18e
)
