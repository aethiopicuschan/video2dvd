package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	dvdNAVSystemHeaderSize   = 24
	dvdNAVPCIDataSize        = 979
	dvdNAVDSIDataSize        = 1017
	dvdNAVDSIVOBUSRIOffset   = 32 + 148 + 54
	dvdNAVDSINextVOBUOffset  = dvdNAVDSIVOBUSRIOffset + 4 + 19*4
	dvdNAVDSIPrevVOBUOffset  = dvdNAVDSINextVOBUOffset + 4
	dvdNAVDSIPrevVideoOffset = dvdNAVDSIPrevVOBUOffset + 4 + 19*4
	dvdNAVDSIAudioSyncOffset = dvdNAVDSIPrevVideoOffset + 4
	dvdNAVDSISubpictureSync  = dvdNAVDSIAudioSyncOffset + 8*2
	dvdNAVPCIHighlightOffset = 0x60
	dvdNAVMaxButtons         = 36
	dvdNAVMaxButtonGroups    = 3
	dvdNAVHighlightNoEnd     = 0xffffffff
	dvdNAVDSIVOBStartOffset  = 32 + 12
	dvdNAVSRIPointerValid    = uint32(1 << 31)
	dvdNAVSRISkipsVOBUs      = uint32(1 << 30)
	dvdNAVSRINoVideo         = uint32(0xbfffffff)
	dvdNAVSRIHalfSecond      = 45_000
)

// DVDNAVAudioSyncNone is the A_SYNCA value of an audio stream that has no
// pack for the VOBU.
const DVDNAVAudioSyncNone uint16 = 0x3fff

// DVDNAVSubpictureSyncCurrent is the SP_SYNCA value of a subpicture stream
// whose unit for the VOBU is stored in the VOBU itself.
const DVDNAVSubpictureSyncCurrent uint32 = 0x7fffffff

// dvdVOBUSearchSteps lists the VOBU_SRI distances in half seconds, nearest
// first. FWDA is stored in the opposite order.
var dvdVOBUSearchSteps = [19]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 20, 60, 120, 240}

// DVDNAVSRIEndOfCell marks the boundary of a DVD cell in a VOBU search
// pointer. It is used for a VOBU with no next or previous VOBU in its cell.
const DVDNAVSRIEndOfCell uint32 = 0x3fffffff

// DVDNAVPackInfo contains the navigation fields associated with one VOBU.
// Sector addresses are relative to the beginning of the VOB file. Reference
// end addresses are relative to the NAV pack, as required by DSI_GI.
type DVDNAVPackInfo struct {
	SCRBase uint64
	// SCRExtension is the 27 MHz remainder (0-299) of the pack's system clock
	// reference.
	SCRExtension       uint16
	LogicalBlockNumber uint32
	VOBUEndAddress     uint32
	// NextVOBU and PreviousVOBU are raw DSI VOBU_SRI pointers. Valid
	// pointers contain the relative sector offset and bit 31; use
	// DVDNAVSRIEndOfCell at a cell boundary. Zero defaults to end-of-cell.
	NextVOBU     uint32
	PreviousVOBU uint32
	// ForwardVOBUs and BackwardVOBUs are the raw FWDA and BWDA scan pointers
	// in DSI order: ForwardVOBUs runs from +120 s down to +0.5 s and
	// BackwardVOBUs from -0.5 s to -120 s. Zero entries default to
	// DVDNAVSRIEndOfCell.
	ForwardVOBUs  [19]uint32
	BackwardVOBUs [19]uint32
	// AudioSync holds A_SYNCA for audio streams 0-7: the sector offset from
	// this NAV pack to the audio pack presented at StartPTM, with bit 15 set
	// for a pack that precedes the NAV pack. Zero marks an absent stream.
	AudioSync [8]uint16
	// SubpictureSync holds SP_SYNCA for subpicture streams 0-31: the sector
	// offset to the VOBU that holds the subpicture unit shown during this
	// VOBU, with bit 31 set for an earlier VOBU, or
	// DVDNAVSubpictureSyncCurrent for this VOBU. Zero marks an absent stream.
	SubpictureSync            [32]uint32
	FirstReferenceEndAddress  uint32
	SecondReferenceEndAddress uint32
	ThirdReferenceEndAddress  uint32
	VOBID                     uint16
	CellID                    uint8
	StartPTM                  uint32
	EndPTM                    uint32
	// SequenceEndPTM is the end presentation time of the video when the VOBU
	// ends with a sequence_end_code and zero otherwise.
	SequenceEndPTM uint32
	// VOBStartPTM and VOBEndPTM are the presentation times of the first and
	// the end of the last video frame of the whole VOB.
	VOBStartPTM     uint32
	VOBEndPTM       uint32
	CellElapsedTime [4]byte
	// ButtonDisplayType tells which display mode the first button group,
	// Buttons, is laid out for. It is 0 for 4:3 video. For 16:9 video it is
	// 1 for a widescreen display, 2 for a letterboxed and 4 for a
	// pan-and-scan presentation on a 4:3 display; a group that serves more
	// than one of these modes has the sum of their values.
	ButtonDisplayType uint8
	// Buttons is the first button group. The highlight is valid from
	// StartPTM without an end time, as for a still menu, and recolours
	// subpicture pattern pixels: palette entry 1 marks the selected button
	// and palette entry 2 the activated one.
	Buttons []DVDNAVButton
	// ExtraButtonGroups holds up to two more layouts of the same buttons
	// for other display modes. Every group needs as many buttons as the
	// first one, and with n groups each may hold 36/n buttons.
	ExtraButtonGroups []DVDNAVButtonGroup
}

// DVDNAVButtonGroup is the button layout for one display mode. DisplayType
// has the meaning of DVDNAVPackInfo.ButtonDisplayType.
type DVDNAVButtonGroup struct {
	DisplayType uint8
	Buttons     []DVDNAVButton
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
	if info.SCRBase > maxMPEGTimestamp || info.SCRExtension >= mpegSCRExtensionRange {
		return nil, errors.New("video2dvd: DVD NAV SCR exceeds the MPEG 33-bit range")
	}
	if info.ButtonDisplayType > 7 {
		return nil, errors.New("video2dvd: DVD button display type exceeds the three-bit field")
	}
	if len(info.Buttons) == 0 && len(info.ExtraButtonGroups) != 0 {
		return nil, errors.New("video2dvd: additional DVD button groups require a first group")
	}
	if info.NextVOBU == 0 {
		info.NextVOBU = DVDNAVSRIEndOfCell
	}
	if info.PreviousVOBU == 0 {
		info.PreviousVOBU = DVDNAVSRIEndOfCell
	}
	if !validDVDNAVSRIPointer(info.NextVOBU) || !validDVDNAVSRIPointer(info.PreviousVOBU) {
		return nil, errors.New("video2dvd: DVD NAV VOBU search pointers are invalid")
	}
	pack := make([]byte, DVDVOBSectorSize)
	if err := putMPEG2PackHeader(pack, info.SCRBase*mpegSCRExtensionRange+uint64(info.SCRExtension), defaultProgramMuxRate/50); err != nil {
		return nil, err
	}
	copy(pack[14:], dvdNAVSystemHeader[:])

	const pciOffset = 14 + dvdNAVSystemHeaderSize
	writePrivateStream2Packet(pack[pciOffset:pciOffset+986], 0, dvdNAVPCIDataSize)
	pci := pack[pciOffset+7 : pciOffset+986]
	binary.BigEndian.PutUint32(pci[0:4], info.LogicalBlockNumber)
	binary.BigEndian.PutUint32(pci[12:16], info.StartPTM)
	binary.BigEndian.PutUint32(pci[16:20], info.EndPTM)
	binary.BigEndian.PutUint32(pci[20:24], info.SequenceEndPTM)
	copy(pci[24:28], info.CellElapsedTime[:])
	groups := append([]DVDNAVButtonGroup{{DisplayType: info.ButtonDisplayType, Buttons: info.Buttons}}, info.ExtraButtonGroups...)
	if err := writeDVDNAVButtons(pci[dvdNAVPCIHighlightOffset:], info.StartPTM, groups); err != nil {
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
	binary.BigEndian.PutUint32(dsi[dvdNAVDSIVOBStartOffset:], info.VOBStartPTM)
	binary.BigEndian.PutUint32(dsi[dvdNAVDSIVOBStartOffset+4:], info.VOBEndPTM)
	nextVideo, previousVideo := info.NextVOBU, info.PreviousVOBU
	if nextVideo == DVDNAVSRIEndOfCell {
		nextVideo = dvdNAVSRINoVideo
	}
	if previousVideo == DVDNAVSRIEndOfCell {
		previousVideo = dvdNAVSRINoVideo
	}
	binary.BigEndian.PutUint32(dsi[dvdNAVDSIVOBUSRIOffset:dvdNAVDSIVOBUSRIOffset+4], nextVideo)
	binary.BigEndian.PutUint32(dsi[dvdNAVDSINextVOBUOffset:dvdNAVDSINextVOBUOffset+4], info.NextVOBU)
	binary.BigEndian.PutUint32(dsi[dvdNAVDSIPrevVOBUOffset:dvdNAVDSIPrevVOBUOffset+4], info.PreviousVOBU)
	binary.BigEndian.PutUint32(dsi[dvdNAVDSIPrevVideoOffset:dvdNAVDSIPrevVideoOffset+4], previousVideo)
	for i := range info.ForwardVOBUs {
		forward, backward := info.ForwardVOBUs[i], info.BackwardVOBUs[i]
		if forward == 0 {
			forward = DVDNAVSRIEndOfCell
		}
		if backward == 0 {
			backward = DVDNAVSRIEndOfCell
		}
		binary.BigEndian.PutUint32(dsi[dvdNAVDSIVOBUSRIOffset+4+i*4:], forward)
		binary.BigEndian.PutUint32(dsi[dvdNAVDSIPrevVOBUOffset+4+i*4:], backward)
	}
	for i, sync := range info.AudioSync {
		binary.BigEndian.PutUint16(dsi[dvdNAVDSIAudioSyncOffset+i*2:], sync)
	}
	for i, sync := range info.SubpictureSync {
		binary.BigEndian.PutUint32(dsi[dvdNAVDSISubpictureSync+i*4:], sync)
	}
	return pack, nil
}

// dvdVOBUSearchEntry is one VOBU of the table that VOBU_SRI pointers are
// derived from. Every VOBU is assumed to contain video.
type dvdVOBUSearchEntry struct {
	Sector   uint32
	StartPTM uint32
	EndPTM   uint32
}

type dvdVOBUSearchInfo struct {
	NextVOBU      uint32
	PreviousVOBU  uint32
	ForwardVOBUs  [19]uint32
	BackwardVOBUs [19]uint32
}

// buildDVDVOBUSearchInfo computes the DSI search pointers of vobus[index].
// first and last bound the cell that contains it; pointers never leave the
// cell. The rules follow dvdauthor: each scan entry addresses the VOBU that
// is being presented the given time away, and the entries of 10 s and more
// carry the skip flag when they pass over VOBUs the previous entry did not.
func buildDVDVOBUSearchInfo(vobus []dvdVOBUSearchEntry, first, last, index int) dvdVOBUSearchInfo {
	find := func(pts int64) int {
		low, high := first, last
		if pts < int64(vobus[low].StartPTM) {
			return low - 1
		}
		if pts >= int64(vobus[high].EndPTM) {
			return high + 1
		}
		for low < high {
			middle := (low + high + 1) / 2
			if pts < int64(vobus[middle].StartPTM) {
				high = middle - 1
			} else {
				low = middle
			}
		}
		return low
	}
	pointer := func(target int, skip bool) uint32 {
		var flags uint32
		if skip && (target-index > 1 || index-target > 1) {
			flags = dvdNAVSRISkipsVOBUs
		}
		if target < first || target > last {
			return DVDNAVSRIEndOfCell | flags
		}
		from, to := vobus[index].Sector, vobus[target].Sector
		if to < from {
			from, to = to, from
		}
		return dvdNAVSRIPointerValid | (to - from) | flags
	}
	info := dvdVOBUSearchInfo{NextVOBU: pointer(index+1, false), PreviousVOBU: pointer(index-1, false)}
	start := int64(vobus[index].StartPTM)
	previousForward, previousBackward := index, index
	for i, step := range dvdVOBUSearchSteps {
		forward := find(start + step*dvdNAVSRIHalfSecond)
		backward := find(start - step*dvdNAVSRIHalfSecond)
		info.ForwardVOBUs[len(dvdVOBUSearchSteps)-1-i] = pointer(forward, i >= 15 && forward > previousForward+1)
		info.BackwardVOBUs[i] = pointer(backward, i >= 15 && backward < previousBackward-1)
		previousForward, previousBackward = forward, backward
	}
	return info
}

func validDVDNAVSRIPointer(pointer uint32) bool {
	if pointer == DVDNAVSRIEndOfCell {
		return true
	}
	offset := pointer & 0x3fffffff
	return pointer&dvdNAVSRIPointerValid != 0 && offset != 0 && offset != 0x3fffffff
}

// Highlight colours of the selected and the activated button. An entry holds
// four palette indexes and then four contrasts, each as emphasis-2,
// emphasis-1, pattern and background nibbles: only the pattern pixels of the
// subpicture become visible, in palette entry 1 or 2.
var (
	dvdNAVSelectionColor = [4]byte{0x00, 0x10, 0x00, 0x80}
	dvdNAVActionColor    = [4]byte{0x00, 0x20, 0x00, 0xc0}
)

// writeDVDNAVButtons fills the highlight information (HLI) of a PCI packet.
// The first group must be the layout for the display mode a player uses when
// it ignores button groups, which is the native one of the video.
func writeDVDNAVButtons(hli []byte, startPTM uint32, groups []DVDNAVButtonGroup) error {
	count := len(groups[0].Buttons)
	if count == 0 {
		return nil
	}
	if len(groups) > dvdNAVMaxButtonGroups {
		return errors.New("video2dvd: DVD menu supports at most three button groups")
	}
	// The button table is divided evenly among the groups.
	groupSize := dvdNAVMaxButtons / len(groups)
	if count > groupSize {
		return fmt.Errorf("video2dvd: DVD menu supports at most %d buttons per VOBU in %d button group(s)", groupSize, len(groups))
	}
	hli[1] = 1 // HLI_SS: all-new highlight information.
	// A still menu is highlighted from the start of its VOBU and neither the
	// highlight nor the button selection ever ends.
	binary.BigEndian.PutUint32(hli[2:6], startPTM)
	binary.BigEndian.PutUint32(hli[6:10], dvdNAVHighlightNoEnd)
	binary.BigEndian.PutUint32(hli[10:14], dvdNAVHighlightNoEnd)
	// BTNGR_NS and one display type nibble per group; reserved bits stay zero.
	groupInfo := uint16(len(groups)) << 12
	for i, group := range groups {
		if group.DisplayType > 7 {
			return errors.New("video2dvd: DVD button display type exceeds the three-bit field")
		}
		if len(group.Buttons) != count {
			return errors.New("video2dvd: DVD button groups must hold the same number of buttons")
		}
		groupInfo |= uint16(group.DisplayType) << (8 - 4*i)
	}
	binary.BigEndian.PutUint16(hli[0x0e:0x10], groupInfo)
	hli[0x10] = 0 // BTN_OFN: user button numbers start at 1.
	hli[0x11] = byte(count)
	hli[0x12] = byte(count)
	// Select the first button initially without forcing its action.
	hli[0x14] = 1
	hli[0x15] = 0
	// Colour scheme 1 of 3; every button uses it.
	copy(hli[0x16:0x1a], dvdNAVSelectionColor[:])
	copy(hli[0x1a:0x1e], dvdNAVActionColor[:])
	for groupIndex, group := range groups {
		for index, button := range group.Buttons {
			if button.XStart < 0 || button.XStart > 719 || button.XEnd < button.XStart || button.XEnd > 719 ||
				button.YStart < 0 || button.YStart > 575 || button.YEnd < button.YStart || button.YEnd > 575 {
				return errors.New("video2dvd: DVD menu button coordinates are outside the display raster")
			}
			if int(button.Up) > count || int(button.Down) > count || int(button.Left) > count || int(button.Right) > count {
				return errors.New("video2dvd: DVD menu button navigation target is out of range")
			}
			offset := 0x2e + (groupIndex*groupSize+index)*18
			hli[offset] = 0x40 | byte(button.XStart>>4)
			hli[offset+1] = byte(button.XStart<<4) | byte(button.XEnd>>8)
			hli[offset+2] = byte(button.XEnd)
			hli[offset+3] = byte(button.YStart >> 4)
			hli[offset+4] = byte(button.YStart<<4) | byte(button.YEnd>>8)
			hli[offset+5] = byte(button.YEnd)
			hli[offset+6] = button.Up
			hli[offset+7] = button.Down
			hli[offset+8] = button.Left
			hli[offset+9] = button.Right
			copy(hli[offset+10:offset+18], button.Command[:])
		}
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

// dvdNAVSystemHeader is the system header carried by every DVD NAV pack: a
// 10.08 Mbit/s rate bound, one video stream, and the buffer bounds of all
// video streams (232 KiB), all MPEG audio streams (4 KiB), private_stream_1
// (58 KiB) and private_stream_2 (2 KiB).
var dvdNAVSystemHeader = [dvdNAVSystemHeaderSize]byte{
	0x00, 0x00, 0x01, 0xbb, 0x00, 0x12,
	0x80 | defaultProgramMuxRate/50>>15, defaultProgramMuxRate / 50 >> 7 & 0xff, defaultProgramMuxRate/50<<1&0xff | 0x01,
	0x00, 0xe1, 0x7f,
	0xb9, 0xe0, 0xe8,
	0xb8, 0xc0, 0x20,
	0xbd, 0xe0, 0x3a,
	0xbf, 0xe0, 0x02,
}

func writePrivateStream2Packet(dst []byte, substreamID byte, dataSize int) {
	dst[0], dst[1], dst[2], dst[3] = 0, 0, 1, 0xbf
	binary.BigEndian.PutUint16(dst[4:6], uint16(dataSize+1))
	dst[6] = substreamID
}
