package video2dvd

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestBuildDVDNAVPackWritesPCIAndDSIAtFixedOffsets(t *testing.T) {
	info := DVDNAVPackInfo{
		SCRBase:                   90_000,
		LogicalBlockNumber:        321,
		VOBUEndAddress:            14,
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
}

func TestBuildDVDNAVPackRejectsMissingIDs(t *testing.T) {
	if _, err := BuildDVDNAVPack(DVDNAVPackInfo{}); err == nil {
		t.Fatal("expected a NAV pack without VOB/cell IDs to fail")
	}
}

func TestDVDCellElapsedTimeUsesDVDBCDAndFrameRateCode(t *testing.T) {
	ntsc, err := dvdCellElapsedTime(time.Hour+2*time.Minute+3*time.Second+500*time.Millisecond, 30000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if ntsc != [4]byte{0x01, 0x02, 0x03, 0xcf} {
		t.Fatalf("NTSC cell time = % x, want 01 02 03 cf", ntsc)
	}
	pal, err := dvdCellElapsedTime(time.Second+960*time.Millisecond, 25, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pal != [4]byte{0x00, 0x00, 0x01, 0x58} {
		t.Fatalf("PAL cell time = % x, want 00 00 01 58", pal)
	}
}
