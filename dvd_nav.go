package video2dvd

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	dvdNAVSystemHeaderSize = 24
	dvdNAVPCIDataSize      = 979
	dvdNAVDSIDataSize      = 1017
)

// DVDNAVPackInfo contains the navigation fields associated with one VOBU.
// Sector addresses are relative to the beginning of the VOB file. Reference
// end addresses are relative to the NAV pack, as required by DSI_GI.
type DVDNAVPackInfo struct {
	SCRBase                   uint64
	LogicalBlockNumber        uint32
	VOBUEndAddress            uint32
	FirstReferenceEndAddress  uint32
	SecondReferenceEndAddress uint32
	ThirdReferenceEndAddress  uint32
	VOBID                     uint16
	CellID                    uint8
	StartPTM                  uint32
	EndPTM                    uint32
	CellElapsedTime           [4]byte
	Buttons                   []DVDNAVButton
}

// DVDNAVButton describes one rectangular menu button. Coordinates use the
// DVD 720x480/576 display raster and commands are eight-byte DVD VM words.
type DVDNAVButton struct {
	XStart, YStart int
	XEnd, YEnd     int
	Up, Down       uint8
	Left, Right    uint8
	Command        [8]byte
}

// BuildDVDNAVPack builds one 2048-byte DVD navigation pack containing the
// MPEG-2 pack and system headers, followed by PCI and DSI private-stream-2
// packets. It is a pack primitive; callers still need to arrange VOBUs and
// generate the corresponding IFO navigation tables.
func BuildDVDNAVPack(info DVDNAVPackInfo) ([]byte, error) {
	if info.VOBID == 0 || info.CellID == 0 {
		return nil, errors.New("video2dvd: DVD NAV pack requires nonzero VOB and cell IDs")
	}
	if info.SCRBase > maxMPEGTimestamp {
		return nil, errors.New("video2dvd: DVD NAV SCR exceeds the MPEG 33-bit range")
	}
	pack := make([]byte, DVDVOBSectorSize)
	copy(pack, buildMPEG2PackHeader(info.SCRBase, defaultProgramMuxRate/50))
	systemHeader := buildDVDNAVSystemHeader()
	copy(pack[14:], systemHeader)

	const pciOffset = 14 + dvdNAVSystemHeaderSize
	writePrivateStream2Packet(pack[pciOffset:pciOffset+986], 0, dvdNAVPCIDataSize)
	pci := pack[pciOffset+7 : pciOffset+986]
	binary.BigEndian.PutUint32(pci[0:4], info.LogicalBlockNumber)
	binary.BigEndian.PutUint32(pci[12:16], info.StartPTM)
	binary.BigEndian.PutUint32(pci[16:20], info.EndPTM)
	copy(pci[20:24], info.CellElapsedTime[:])
	if err := writeDVDNAVButtons(pci, info.Buttons); err != nil {
		return nil, err
	}

	const dsiOffset = 0x400
	writePrivateStream2Packet(pack[dsiOffset:], 1, dvdNAVDSIDataSize)
	dsi := pack[dsiOffset+7:]
	binary.BigEndian.PutUint32(dsi[0:4], uint32(info.SCRBase))
	binary.BigEndian.PutUint32(dsi[4:8], info.LogicalBlockNumber)
	binary.BigEndian.PutUint32(dsi[8:12], info.VOBUEndAddress)
	binary.BigEndian.PutUint32(dsi[12:16], info.FirstReferenceEndAddress)
	binary.BigEndian.PutUint32(dsi[16:20], info.SecondReferenceEndAddress)
	binary.BigEndian.PutUint32(dsi[20:24], info.ThirdReferenceEndAddress)
	binary.BigEndian.PutUint16(dsi[24:26], info.VOBID)
	dsi[27] = info.CellID
	copy(dsi[28:32], info.CellElapsedTime[:])
	return pack, nil
}

func writeDVDNAVButtons(pci []byte, buttons []DVDNAVButton) error {
	if len(buttons) == 0 {
		return nil
	}
	if len(buttons) > 36 {
		return errors.New("video2dvd: DVD menu supports at most 36 buttons per VOBU")
	}
	base := 0x60
	pci[base+1] = 1       // all-new highlight information.
	pci[base+0x0e] = 0x40 // one normal button group.
	pci[base+0x10] = 1
	pci[base+0x11] = byte(len(buttons))
	pci[base+0x12] = byte(len(buttons))
	pci[base+0x14] = 1
	pci[base+0x15] = 1
	// White selection and yellow action emphasis with full contrast.
	copy(pci[base+0x16:base+0x1e], []byte{0x03, 0x01, 0xff, 0xff, 0x30, 0x10, 0xff, 0xff})
	for index, button := range buttons {
		if button.XStart < 0 || button.XStart > 719 || button.XEnd < button.XStart || button.XEnd > 719 ||
			button.YStart < 0 || button.YStart > 575 || button.YEnd < button.YStart || button.YEnd > 575 {
			return errors.New("video2dvd: DVD menu button coordinates are outside the display raster")
		}
		if button.Up > 36 || button.Down > 36 || button.Left > 36 || button.Right > 36 {
			return errors.New("video2dvd: DVD menu button navigation target is out of range")
		}
		offset := base + 0x2e + index*18
		pci[offset] = 0x40 | byte(button.XStart>>4)
		pci[offset+1] = byte(button.XStart<<4) | byte(button.XEnd>>8)
		pci[offset+2] = byte(button.XEnd)
		pci[offset+3] = byte(button.YStart >> 4)
		pci[offset+4] = byte(button.YStart<<4) | byte(button.YEnd>>8)
		pci[offset+5] = byte(button.YEnd)
		pci[offset+6] = button.Up
		pci[offset+7] = button.Down
		pci[offset+8] = button.Left
		pci[offset+9] = button.Right
		copy(pci[offset+10:offset+18], button.Command[:])
	}
	return nil
}

// WriteDVDNAVPack writes one fixed-size navigation pack without closing dst.
func WriteDVDNAVPack(dst io.Writer, info DVDNAVPackInfo) error {
	if dst == nil {
		return errors.New("video2dvd: DVD NAV destination writer is required")
	}
	pack, err := BuildDVDNAVPack(info)
	if err != nil {
		return err
	}
	return writeAll(dst, pack)
}

func buildDVDNAVSystemHeader() []byte {
	var bits mpeg2BitWriter
	bits.startCode(0xbb)
	bits.write(18, 16)                       // header_length
	bits.write(1, 1)                         // marker_bit
	bits.write(defaultProgramMuxRate/50, 22) // rate_bound
	bits.write(1, 1)                         // marker_bit
	bits.write(1, 6)                         // audio_bound
	bits.write(0, 1)                         // fixed_flag
	bits.write(0, 1)                         // CSPS_flag
	bits.write(0, 1)                         // system_audio_lock_flag
	bits.write(0, 1)                         // system_video_lock_flag
	bits.write(1, 1)                         // marker_bit
	bits.write(1, 5)                         // video_bound
	bits.write(0, 1)                         // packet_rate_restriction_flag
	bits.write(0x7f, 7)                      // reserved_bits
	bits.write(0xe0, 8)                      // video stream_id
	bits.write(0b11, 2)                      // marker bits
	bits.write(1, 1)                         // video P-STD_buffer_bound_scale
	bits.write(112, 13)                      // 112 KiB video buffer bound
	bits.write(0xbd, 8)                      // private_stream_1 audio stream_id
	bits.write(0b11, 2)                      // marker bits
	bits.write(0, 1)                         // audio P-STD_buffer_bound_scale
	bits.write(32, 13)                       // 4 KiB audio buffer bound
	return bits.bytes()
}

func writePrivateStream2Packet(dst []byte, substreamID byte, dataSize int) {
	dst[0], dst[1], dst[2], dst[3] = 0, 0, 1, 0xbf
	binary.BigEndian.PutUint16(dst[4:6], uint16(dataSize+1))
	dst[6] = substreamID
}
