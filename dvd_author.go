package mp4todvd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

const (
	dvdMaxTitles         = 99
	dvdIFOAudioOffset    = 0x0200
	dvdVTSIFOBaseSectors = 5
)

// InProcessAuthorer writes a menu-less DVD-Video title set for each encoded
// title. It uses only Go code and currently accepts MPEG-2 video with LPCM
// audio, which are the streams supported by MuxDVDTitleVOB.
type InProcessAuthorer struct{}

// NewInProcessAuthorer returns the built-in IFO/BUP and VOB authorer.
func NewInProcessAuthorer() *InProcessAuthorer { return &InProcessAuthorer{} }

type authoredTitle struct {
	ifoSectors   uint32
	titleSetBase uint32
}

// Author writes a title set for every encoded title. The output directory must
// be empty. VTS numbers and title order match the input order.
func (a *InProcessAuthorer) Author(ctx context.Context, titles []EncodedTitle, outputDir string, profile Profile) error {
	if a == nil {
		return errors.New("mp4todvd: in-process authorer is nil")
	}
	if ctx == nil {
		return errors.New("mp4todvd: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(titles) == 0 {
		return ErrNoInputs
	}
	if len(titles) > dvdMaxTitles {
		return fmt.Errorf("mp4todvd: DVD-Video supports at most %d titles", dvdMaxTitles)
	}
	if err := validateAuthoringProfile(profile); err != nil {
		return err
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return fmt.Errorf("mp4todvd: inspect authoring directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("mp4todvd: authoring directory must be empty")
	}

	vmgPlaceholder, vmgSectors, err := buildDVDVMGI(titles, profile, nil)
	if err != nil {
		return err
	}
	if err := writeIFOAndBUP(outputDir, "VIDEO_TS", vmgPlaceholder); err != nil {
		return err
	}

	authored := make([]authoredTitle, len(titles))
	nextTitleSetSector := uint64(vmgSectors) * 2 // VMGI and its BUP; no VMGM VOB.
	for i, title := range titles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if title.VideoCodec != VideoCodecMPEG2 || title.AudioCodec != AudioCodecLPCM {
			return fmt.Errorf("mp4todvd: built-in authorer requires MPEG-2 video and LPCM audio for title %d", i+1)
		}
		if title.Aspect != profile.Aspect || title.Width != profile.FrameWidth || title.Height != profile.FrameHeight {
			return fmt.Errorf("mp4todvd: title %d metadata does not match the authoring profile", i+1)
		}
		if title.Duration <= 0 || title.FrameRateNumerator <= 0 || title.FrameRateDenominator <= 0 {
			return fmt.Errorf("mp4todvd: title %d has invalid timing metadata", i+1)
		}
		if !validFrameRate(profile.Standard, title.FrameRateNumerator, title.FrameRateDenominator) {
			return fmt.Errorf("mp4todvd: title %d frame rate does not match the authoring standard", i+1)
		}
		if nextTitleSetSector > math.MaxUint32 {
			return errors.New("mp4todvd: DVD title-set sector address exceeds 32 bits")
		}
		base := uint32(nextTitleSetSector)
		vtsName := fmt.Sprintf("VTS_%02d", i+1)
		vobPath := filepath.Join(outputDir, vtsName+"_1.VOB")
		vob, err := os.OpenFile(vobPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("mp4todvd: create %s: %w", filepath.Base(vobPath), err)
		}
		records := make([]dvdVOBURecord, 0, 64)
		workDir := filepath.Dir(title.VideoPath)
		muxErr := muxDVDTitleVOB(ctx, title, workDir, vob, func(record dvdVOBURecord) error {
			records = append(records, record)
			return nil
		})
		closeErr := vob.Close()
		if muxErr != nil {
			return fmt.Errorf("mp4todvd: mux title %d: %w", i+1, muxErr)
		}
		if closeErr != nil {
			return fmt.Errorf("mp4todvd: close %s: %w", filepath.Base(vobPath), closeErr)
		}
		vobInfo, err := os.Stat(vobPath)
		if err != nil {
			return err
		}
		if vobInfo.Size() == 0 || vobInfo.Size()%DVDVOBSectorSize != 0 || vobInfo.Size()/DVDVOBSectorSize > math.MaxUint32 {
			return fmt.Errorf("mp4todvd: title %d VOB has an invalid sector count", i+1)
		}
		vobSectors := uint32(vobInfo.Size() / DVDVOBSectorSize)
		ifo, ifoSectors, err := buildDVDVTSIFO(title, profile, records, vobSectors)
		if err != nil {
			return fmt.Errorf("mp4todvd: build title %d IFO: %w", i+1, err)
		}
		if err := writeIFOAndBUP(outputDir, vtsName+"_0", ifo); err != nil {
			return err
		}
		authored[i] = authoredTitle{ifoSectors: ifoSectors, titleSetBase: base}
		nextTitleSetSector += uint64(ifoSectors)*2 + uint64(vobSectors)
	}

	starts := make([]uint32, len(authored))
	for i := range authored {
		starts[i] = authored[i].titleSetBase
	}
	vmg, finalVMGSectors, err := buildDVDVMGI(titles, profile, starts)
	if err != nil {
		return err
	}
	if finalVMGSectors != vmgSectors {
		return errors.New("mp4todvd: VMGI size changed while writing title references")
	}
	if err := overwriteIFOAndBUP(outputDir, "VIDEO_TS", vmg); err != nil {
		return err
	}
	return ctx.Err()
}

func validateAuthoringProfile(profile Profile) error {
	if profile.Standard != VideoStandardNTSC && profile.Standard != VideoStandardPAL {
		return ErrInvalidProfile
	}
	if profile.Aspect != AspectRatio4x3 && profile.Aspect != AspectRatio16x9 {
		return ErrInvalidProfile
	}
	wantHeight := 480
	if profile.Standard == VideoStandardPAL {
		wantHeight = 576
	}
	if profile.FrameWidth != 720 || profile.FrameHeight != wantHeight {
		return ErrInvalidProfile
	}
	return nil
}

func buildDVDVTSIFO(title EncodedTitle, profile Profile, records []dvdVOBURecord, vobSectors uint32) ([]byte, uint32, error) {
	if len(records) == 0 || records[0].StartSector != 0 || records[len(records)-1].EndSector+1 != vobSectors {
		return nil, 0, errors.New("VOBU records do not cover the title VOB")
	}
	for i := 1; i < len(records); i++ {
		if records[i].StartSector != records[i-1].EndSector+1 || records[i].EndSector < records[i].StartSector {
			return nil, 0, errors.New("VOBU records are not contiguous and ordered")
		}
	}
	playbackTime, err := dvdIFOTime(title.Duration, profile.Standard)
	if err != nil {
		return nil, 0, err
	}
	admap := make([]byte, 4+4*len(records))
	binary.BigEndian.PutUint32(admap[0:4], uint32(len(admap)-1))
	for i, record := range records {
		binary.BigEndian.PutUint32(admap[4+i*4:8+i*4], record.StartSector)
	}
	ifoSectors := dvdVTSIFOBaseSectors + uint32((len(admap)+DVDVOBSectorSize-1)/DVDVOBSectorSize)
	lastTitleSetSector := uint64(ifoSectors)*2 + uint64(vobSectors) - 1
	if lastTitleSetSector > math.MaxUint32 {
		return nil, 0, errors.New("VTS last-sector address exceeds 32 bits")
	}
	ifo := make([]byte, int(ifoSectors)*DVDVOBSectorSize)
	copy(ifo[:12], []byte("DVDVIDEO-VTS"))
	binary.BigEndian.PutUint32(ifo[0x000C:0x0010], uint32(lastTitleSetSector))
	binary.BigEndian.PutUint32(ifo[0x001C:0x0020], ifoSectors-1)
	binary.BigEndian.PutUint16(ifo[0x0020:0x0022], 0x0011)
	binary.BigEndian.PutUint32(ifo[0x0080:0x0084], 0x01ff)
	binary.BigEndian.PutUint32(ifo[0x00C4:0x00C8], ifoSectors) // VTS_VOBS starts after VTSI.
	binary.BigEndian.PutUint32(ifo[0x00C8:0x00CC], 1)          // VTS_PTT_SRPT
	binary.BigEndian.PutUint32(ifo[0x00CC:0x00D0], 2)          // VTS_PGCI
	binary.BigEndian.PutUint32(ifo[0x00D4:0x00D8], 4)          // VTS_TMAPTI
	binary.BigEndian.PutUint32(ifo[0x00E0:0x00E4], 3)          // VTS_C_ADT
	binary.BigEndian.PutUint32(ifo[0x00E4:0x00E8], 5)          // VTS_VOBU_ADMAP
	videoAttr := dvdVideoAttribute(title, profile)
	binary.BigEndian.PutUint16(ifo[dvdIFOAudioOffset:dvdIFOAudioOffset+2], videoAttr)
	binary.BigEndian.PutUint16(ifo[dvdIFOAudioOffset+2:dvdIFOAudioOffset+4], 1)
	ifo[0x0204] = 0x80 // LPCM coding mode, unspecified language.
	ifo[0x0205] = 0x01 // 16-bit, 48 kHz, stereo.

	ptt := make([]byte, 16)
	binary.BigEndian.PutUint16(ptt[0:2], 1)
	binary.BigEndian.PutUint32(ptt[4:8], uint32(len(ptt)-1))
	binary.BigEndian.PutUint32(ptt[8:12], 12)
	binary.BigEndian.PutUint16(ptt[12:14], 1) // PGCN
	binary.BigEndian.PutUint16(ptt[14:16], 1) // PGN
	copy(ifo[DVDVOBSectorSize:], ptt)

	pgc := buildDVDVTSProgramChain(playbackTime, records, vobSectors)
	pgci := make([]byte, 16+len(pgc))
	binary.BigEndian.PutUint16(pgci[0:2], 1)
	binary.BigEndian.PutUint32(pgci[4:8], uint32(len(pgci)-1))
	binary.BigEndian.PutUint32(pgci[8:12], 0x81000000) // entry PGC for VTS title 1.
	binary.BigEndian.PutUint32(pgci[12:16], 16)
	copy(pgci[16:], pgc)
	copy(ifo[2*DVDVOBSectorSize:], pgci)

	cadt := make([]byte, 20)
	binary.BigEndian.PutUint16(cadt[0:2], 1)
	binary.BigEndian.PutUint32(cadt[4:8], uint32(len(cadt)-1))
	binary.BigEndian.PutUint16(cadt[8:10], 1) // VOB ID
	cadt[10] = 1                              // cell ID
	binary.BigEndian.PutUint32(cadt[12:16], 0)
	binary.BigEndian.PutUint32(cadt[16:20], vobSectors-1)
	copy(ifo[3*DVDVOBSectorSize:], cadt)
	tmap := make([]byte, 16)
	binary.BigEndian.PutUint16(tmap[0:2], 1) // one program chain.
	binary.BigEndian.PutUint32(tmap[4:8], uint32(len(tmap)-1))
	binary.BigEndian.PutUint32(tmap[8:12], 12)
	tmap[12] = 1 // one-second time unit; this PGC's map has no indexed points.
	copy(ifo[4*DVDVOBSectorSize:], tmap)
	copy(ifo[5*DVDVOBSectorSize:], admap)
	return ifo, ifoSectors, nil
}

func buildDVDVTSProgramChain(playbackTime [4]byte, records []dvdVOBURecord, vobSectors uint32) []byte {
	const headerSize = 0xec
	const programMapSize = 2 // one program cell number, padded to an even byte count.
	const cellPlaybackSize = 24
	const cellPositionSize = 4
	pgc := make([]byte, headerSize+programMapSize+cellPlaybackSize+cellPositionSize)
	pgc[2], pgc[3] = 1, 1
	copy(pgc[4:8], playbackTime[:])
	pgc[0x0c] = 0x80 // first LPCM stream is available on private_stream_1 substream 0.
	binary.BigEndian.PutUint16(pgc[0x00E6:0x00E8], headerSize)
	binary.BigEndian.PutUint16(pgc[0x00E8:0x00EA], headerSize+programMapSize)
	binary.BigEndian.PutUint16(pgc[0x00EA:0x00EC], headerSize+programMapSize+cellPlaybackSize)
	pgc[headerSize] = 1
	cell := pgc[headerSize+programMapSize:]
	copy(cell[4:8], playbackTime[:])
	binary.BigEndian.PutUint32(cell[8:12], records[0].StartSector)
	binary.BigEndian.PutUint32(cell[16:20], records[len(records)-1].StartSector)
	binary.BigEndian.PutUint32(cell[20:24], vobSectors-1)
	position := pgc[headerSize+programMapSize+cellPlaybackSize:]
	binary.BigEndian.PutUint16(position[0:2], 1)
	position[3] = 1
	return pgc
}

func dvdVideoAttribute(title EncodedTitle, profile Profile) uint16 {
	attribute := byte(0x40) // MPEG-2.
	if profile.Standard == VideoStandardPAL {
		attribute |= 0x10
	}
	if title.Aspect == AspectRatio16x9 {
		attribute |= 0x0c
	}
	return uint16(attribute) << 8
}

func dvdIFOTime(duration time.Duration, standard VideoStandard) ([4]byte, error) {
	if duration <= 0 || duration >= 100*time.Hour {
		return [4]byte{}, errors.New("DVD title duration must be between zero and 100 hours")
	}
	rate := int64(30)
	rateCode := byte(3)
	if standard == VideoStandardPAL {
		rate, rateCode = 25, 1
	}
	frames := int64(duration) * rate / int64(time.Second)
	wholeSeconds := frames / rate
	framePart := frames % rate
	if wholeSeconds >= 100*60*60 {
		return [4]byte{}, errors.New("DVD title duration exceeds 99:59:59")
	}
	hours := wholeSeconds / 3600
	minutes := wholeSeconds / 60 % 60
	seconds := wholeSeconds % 60
	return [4]byte{toPackedBCD(hours), toPackedBCD(minutes), toPackedBCD(seconds), rateCode<<6 | toPackedBCD(framePart)}, nil
}

func buildDVDVMGI(titles []EncodedTitle, profile Profile, titleSetStarts []uint32) ([]byte, uint32, error) {
	if len(titles) == 0 || len(titles) > dvdMaxTitles {
		return nil, 0, errors.New("mp4todvd: VMGI requires between 1 and 99 titles")
	}
	attributes := make([]byte, len(titles)*776)
	for i, title := range titles {
		vtsAttrs := make([]byte, 0x300)
		// VMG_VTS_ATRT mirrors VTS IFO bytes 0x100..0x3ff. The title
		// attributes begin at file offset 0x200, hence offset 0x100 here.
		vtsAttrs[0x100] = byte(dvdVideoAttribute(title, profile) >> 8)
		vtsAttrs[0x101] = byte(dvdVideoAttribute(title, profile))
		binary.BigEndian.PutUint16(vtsAttrs[0x102:0x104], 1)
		vtsAttrs[0x104], vtsAttrs[0x105] = 0x80, 0x01
		base := i * 776
		binary.BigEndian.PutUint32(attributes[base:base+4], 775)
		copy(attributes[base+8:base+776], vtsAttrs)
	}
	attrTable := make([]byte, 8+4*len(titles)+len(attributes))
	binary.BigEndian.PutUint16(attrTable[0:2], uint16(len(titles)))
	binary.BigEndian.PutUint32(attrTable[4:8], uint32(len(attrTable)-1))
	for i := range titles {
		entryOffset := uint32(8 + 4*len(titles) + i*776)
		binary.BigEndian.PutUint32(attrTable[8+i*4:12+i*4], entryOffset)
		entry := int(entryOffset)
		copy(attrTable[entry+8:entry+776], attributes[i*776+8:(i+1)*776])
	}
	tt := make([]byte, 8+12*len(titles))
	binary.BigEndian.PutUint16(tt[0:2], uint16(len(titles)))
	binary.BigEndian.PutUint32(tt[4:8], uint32(len(tt)-1))
	if titleSetStarts != nil && len(titleSetStarts) != len(titles) {
		return nil, 0, errors.New("VMGI title-set start list has the wrong length")
	}
	for i := range titles {
		base := 8 + i*12
		tt[base], tt[base+1] = 0x3f, 1                   // playable title, one angle.
		binary.BigEndian.PutUint16(tt[base+2:base+4], 1) // one chapter.
		tt[base+6], tt[base+7] = byte(i+1), 1
		if titleSetStarts != nil {
			binary.BigEndian.PutUint32(tt[base+8:base+12], titleSetStarts[i])
		}
	}
	vmgSectors := uint32(2 + (len(attrTable)+DVDVOBSectorSize-1)/DVDVOBSectorSize)
	image := make([]byte, int(vmgSectors)*DVDVOBSectorSize)
	copy(image[:12], []byte("DVDVIDEO-VMG"))
	lastVMGSector := uint64(vmgSectors)*2 - 1 // IFO and BUP; no VIDEO_TS.VOB.
	if lastVMGSector > math.MaxUint32 {
		return nil, 0, errors.New("VMG last-sector address exceeds 32 bits")
	}
	binary.BigEndian.PutUint32(image[0x000C:0x0010], uint32(lastVMGSector))
	binary.BigEndian.PutUint32(image[0x001C:0x0020], vmgSectors-1)
	binary.BigEndian.PutUint16(image[0x0020:0x0022], 0x0011)
	binary.BigEndian.PutUint16(image[0x0028:0x002A], 1) // number of volumes.
	binary.BigEndian.PutUint16(image[0x002A:0x002C], 1) // volume number.
	binary.BigEndian.PutUint16(image[0x003E:0x0040], uint16(len(titles)))
	copy(image[0x0040:0x0040+12], []byte("MP4TODVD-GO"))
	binary.BigEndian.PutUint32(image[0x0080:0x0084], 0x01ff)
	binary.BigEndian.PutUint32(image[0x00C4:0x00C8], 1) // TT_SRPT.
	binary.BigEndian.PutUint32(image[0x00D0:0x00D4], 2) // VMG_VTS_ATRT.
	copy(image[DVDVOBSectorSize:], tt)
	copy(image[2*DVDVOBSectorSize:], attrTable)
	return image, vmgSectors, nil
}

func writeIFOAndBUP(dir, stem string, contents []byte) error {
	for _, ext := range []string{".IFO", ".BUP"} {
		path := filepath.Join(dir, stem+ext)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return fmt.Errorf("mp4todvd: write %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func overwriteIFOAndBUP(dir, stem string, contents []byte) error {
	for _, ext := range []string{".IFO", ".BUP"} {
		path := filepath.Join(dir, stem+ext)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return fmt.Errorf("mp4todvd: open %s for finalization: %w", filepath.Base(path), err)
		}
		_, writeErr := file.Write(contents)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("mp4todvd: finalize %s: %w", filepath.Base(path), writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("mp4todvd: close %s: %w", filepath.Base(path), closeErr)
		}
	}
	return nil
}
