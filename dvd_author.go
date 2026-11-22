package video2dvd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	dvdMaxTitles      = 99
	dvdIFOAudioOffset = 0x0200
	// The VTSI management table, VTS_PTT_SRPT, VTS_PGCI and VTS_C_ADT take
	// one sector each; the tables after them vary in size.
	dvdVTSIFOTimeMapSector = 4
	// A time map holds at most this many entries; longer titles use a
	// coarser time unit.
	dvdVTSTimeMapMaxEntries = 2040
	// The management table occupies the first IFO sector. The VMGI keeps its
	// first-play PGC in the second half of that sector, inside the table's
	// declared extent.
	dvdIFOMATLastByte        = 0x07ff
	dvdVMGFirstPlayPGCOffset = 0x0400
	// A title set stores its title VOB stream in at most nine files of at
	// most 1 GiB minus one sector each.
	dvdTitleVOBFileMaxSectors = (1<<30)/DVDVOBSectorSize - 1
	dvdTitleVOBMaxFiles       = 9
)

// InProcessAuthorer writes a DVD-Video title set with an automatically
// generated title menu. Each encoded title is one DVD title and one chapter.
type InProcessAuthorer struct {
	// titleVOBFileSectors overrides the title VOB split size; zero selects
	// the DVD-Video limit.
	titleVOBFileSectors uint32
}

// NewInProcessAuthorer returns the built-in IFO/BUP and VOB authorer.
func NewInProcessAuthorer() *InProcessAuthorer { return &InProcessAuthorer{} }

type authoredTitle struct {
	ifoSectors   uint32
	titleSetBase uint32
}

// dvdAuthoredMenu describes the muxed title menu VOB for the IFO builders.
type dvdAuthoredMenu struct {
	sectors  uint32
	pageTime [4]byte
	// records holds one VOBU, and therefore one cell and one PGC, per page.
	records []dvdVOBURecord
	// subpictureControl is the PGC subpicture stream control entry of the
	// highlight subpicture stream that the VOB carries, and zero when it
	// carries none; the IFOs declare the stream only when it is set.
	subpictureControl [4]byte
}

// dvdTitleVOBWriter stores one title's VOB stream as VTS_nn_1.VOB,
// VTS_nn_2.VOB, and so on. Sector addresses in NAV packs and IFO tables are
// relative to the first file and continue across the files, so the stream is
// cut wherever a file is full; the muxer writes whole sectors, which keeps
// every cut on a sector boundary.
type dvdTitleVOBWriter struct {
	dir, stem string
	fileBytes int64
	file      *os.File
	files     int
	written   int64 // bytes in the current file.
	total     int64
}

func (w *dvdTitleVOBWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.file == nil || w.written == w.fileBytes {
			if err := w.nextFile(); err != nil {
				return written, err
			}
		}
		count := int(min(int64(len(p)), w.fileBytes-w.written))
		n, err := w.file.Write(p[:count])
		written += n
		w.written += int64(n)
		w.total += int64(n)
		if err != nil {
			return written, err
		}
		p = p[count:]
	}
	return written, nil
}

func (w *dvdTitleVOBWriter) nextFile() error {
	if err := w.Close(); err != nil {
		return err
	}
	if w.files == dvdTitleVOBMaxFiles {
		return fmt.Errorf("video2dvd: title exceeds the %d VOB files of a DVD title set", dvdTitleVOBMaxFiles)
	}
	name := fmt.Sprintf("%s_%d.VOB", w.stem, w.files+1)
	file, err := os.OpenFile(filepath.Join(w.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("video2dvd: create %s: %w", name, err)
	}
	w.file, w.files, w.written = file, w.files+1, 0
	return nil
}

// Close closes the current file. The writer stays usable for nextFile.
func (w *dvdTitleVOBWriter) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("video2dvd: close %s_%d.VOB: %w", w.stem, w.files, err)
	}
	return nil
}

// Author writes a title set for every encoded title. The output directory must
// be empty. VTS numbers and title order match the input order. A title larger
// than one VOB file continues in VTS_nn_2.VOB and following files.
func (a *InProcessAuthorer) Author(ctx context.Context, titles []EncodedTitle, outputDir string, profile Profile) error {
	if a == nil {
		return errors.New("video2dvd: in-process authorer is nil")
	}
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(titles) == 0 {
		return ErrNoInputs
	}
	// Generated stream paths are joined onto the work directory again when
	// they are validated, which only works for absolute paths.
	outputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("video2dvd: resolve output directory: %w", err)
	}
	if len(titles) > dvdMaxTitles {
		return fmt.Errorf("video2dvd: DVD-Video supports at most %d titles", dvdMaxTitles)
	}
	if err := validateAuthoringProfile(profile); err != nil {
		return err
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return fmt.Errorf("video2dvd: inspect authoring directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("video2dvd: authoring directory must be empty")
	}
	titleVOBFileSectors := a.titleVOBFileSectors
	if titleVOBFileSectors == 0 {
		titleVOBFileSectors = dvdTitleVOBFileMaxSectors
	}

	menuWorkDir, err := os.MkdirTemp(outputDir, ".video2dvd-menu-")
	if err != nil {
		return fmt.Errorf("video2dvd: create menu work directory: %w", err)
	}
	defer os.RemoveAll(menuWorkDir)
	menuPages, menuButtonPages, err := createDVDMenuPages(ctx, titles, profile, menuWorkDir)
	if err != nil {
		return err
	}
	menuPath := filepath.Join(outputDir, "VIDEO_TS.VOB")
	menuFile, err := os.OpenFile(menuPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("video2dvd: create VIDEO_TS.VOB: %w", err)
	}
	menu := dvdAuthoredMenu{records: make([]dvdVOBURecord, 0, len(menuButtonPages))}
	if err := muxDVDMenuVOB(ctx, menuPages, menuButtonPages, menuWorkDir, menuFile, func(record dvdVOBURecord) error {
		menu.records = append(menu.records, record)
		return nil
	}); err != nil {
		_ = menuFile.Close()
		return fmt.Errorf("video2dvd: mux title menu: %w", err)
	}
	if err := menuFile.Close(); err != nil {
		return fmt.Errorf("video2dvd: close VIDEO_TS.VOB: %w", err)
	}
	menuInfo, err := os.Stat(menuPath)
	if err != nil || menuInfo.Size() == 0 || menuInfo.Size()%DVDVOBSectorSize != 0 {
		return errors.New("video2dvd: generated title menu has an invalid sector count")
	}
	if len(menu.records) != len(menuButtonPages) {
		return errors.New("video2dvd: generated title menu does not have one VOBU per page")
	}
	menu.sectors = uint32(menuInfo.Size() / DVDVOBSectorSize)
	menu.pageTime, err = dvdMenuPageTime(menu.records, profile.Standard)
	if err != nil {
		return err
	}
	// The muxer stores a highlight subpicture in the VOBU of every page.
	menu.subpictureControl = dvdMenuSubpictureStreamControl(profile.Aspect)
	vmgPlaceholder, vmgSectors, err := buildDVDVMGI(titles, profile, nil, menu)
	if err != nil {
		return err
	}
	if err := writeIFOAndBUP(outputDir, "VIDEO_TS", vmgPlaceholder); err != nil {
		return err
	}

	authored := make([]authoredTitle, len(titles))
	nextTitleSetSector := uint64(vmgSectors)*2 + uint64(menu.sectors)
	for i, title := range titles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if title.VideoCodec != VideoCodecMPEG2 || title.AudioCodec != AudioCodecLPCM {
			return fmt.Errorf("video2dvd: built-in authorer requires MPEG-2 video and LPCM audio for title %d", i+1)
		}
		if title.Aspect != profile.Aspect || title.Width != profile.FrameWidth || title.Height != profile.FrameHeight {
			return fmt.Errorf("video2dvd: title %d metadata does not match the authoring profile", i+1)
		}
		if title.Duration <= 0 || title.FrameRateNumerator <= 0 || title.FrameRateDenominator <= 0 {
			return fmt.Errorf("video2dvd: title %d has invalid timing metadata", i+1)
		}
		if !validFrameRate(profile.Standard, title.FrameRateNumerator, title.FrameRateDenominator) {
			return fmt.Errorf("video2dvd: title %d frame rate does not match the authoring standard", i+1)
		}
		if nextTitleSetSector > math.MaxUint32 {
			return errors.New("video2dvd: DVD title-set sector address exceeds 32 bits")
		}
		base := uint32(nextTitleSetSector)
		vtsName := fmt.Sprintf("VTS_%02d", i+1)
		vob := &dvdTitleVOBWriter{dir: outputDir, stem: vtsName, fileBytes: int64(titleVOBFileSectors) * DVDVOBSectorSize}
		records := make([]dvdVOBURecord, 0, 64)
		workDir := filepath.Dir(title.VideoPath)
		muxErr := muxDVDTitleVOB(ctx, title, workDir, vob, func(record dvdVOBURecord) error {
			records = append(records, record)
			return nil
		}, nil)
		closeErr := vob.Close()
		if muxErr != nil {
			return fmt.Errorf("video2dvd: mux title %d: %w", i+1, muxErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if vob.total == 0 || vob.total%DVDVOBSectorSize != 0 || vob.total/DVDVOBSectorSize > math.MaxUint32 {
			return fmt.Errorf("video2dvd: title %d VOB has an invalid sector count", i+1)
		}
		vobSectors := uint32(vob.total / DVDVOBSectorSize)
		ifo, ifoSectors, err := buildDVDVTSIFO(title, profile, records, vobSectors, len(menu.records), i, len(titles))
		if err != nil {
			return fmt.Errorf("video2dvd: build title %d IFO: %w", i+1, err)
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
	vmg, finalVMGSectors, err := buildDVDVMGI(titles, profile, starts, menu)
	if err != nil {
		return err
	}
	if finalVMGSectors != vmgSectors {
		return errors.New("video2dvd: VMGI size changed while writing title references")
	}
	if err := overwriteIFOAndBUP(outputDir, "VIDEO_TS", vmg); err != nil {
		return err
	}
	return ctx.Err()
}

// dvdMenuPageTime returns the playback time of a menu page cell: the time
// its VOBU presents, which must be the same for every page. A cell whose
// playback time disagrees with its VOBUs is not something players are
// required to cope with.
func dvdMenuPageTime(records []dvdVOBURecord, standard VideoStandard) ([4]byte, error) {
	if len(records) == 0 {
		return [4]byte{}, errors.New("video2dvd: title menu has no VOBU")
	}
	span := records[0].EndPTM - records[0].StartPTM
	for _, record := range records {
		if record.EndPTM < record.StartPTM || record.EndPTM-record.StartPTM != span {
			return [4]byte{}, errors.New("video2dvd: title menu pages differ in length")
		}
	}
	return dvdIFOTime(time.Duration(span)*time.Second/90_000, standard)
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

// buildDVDVTSIFO builds the VTSI for one title. vobSectors is the size of the
// whole title VOB stream, summed over its VOB files.
func buildDVDVTSIFO(title EncodedTitle, profile Profile, records []dvdVOBURecord, vobSectors uint32, menuPageCount, titleIndex, titleCount int) ([]byte, uint32, error) {
	if len(records) == 0 || records[0].StartSector != 0 || records[len(records)-1].EndSector+1 != vobSectors {
		return nil, 0, errors.New("VOBU records do not cover the title VOB")
	}
	for i := 1; i < len(records); i++ {
		if records[i].StartSector != records[i-1].EndSector+1 || records[i].EndSector < records[i].StartSector {
			return nil, 0, errors.New("VOBU records are not contiguous and ordered")
		}
	}
	// The muxed stream, not the encoder's declared duration, defines how long
	// the cell plays.
	span := records[len(records)-1].EndPTM - records[0].StartPTM
	if span == 0 || records[len(records)-1].EndPTM < records[0].StartPTM {
		return nil, 0, errors.New("VOBU records have no presentation time span")
	}
	playbackTime, err := dvdIFOTime(time.Duration(span/90_000)*time.Second+time.Duration(span%90_000)*time.Second/90_000, profile.Standard)
	if err != nil {
		return nil, 0, err
	}
	admap := make([]byte, 4+4*len(records))
	binary.BigEndian.PutUint32(admap[0:4], uint32(len(admap)-1))
	for i, record := range records {
		binary.BigEndian.PutUint32(admap[4+i*4:8+i*4], record.StartSector)
	}
	// The title set has no menu VOB of its own. Its root menu is a
	// command-only PGC that sends the remote's menu key to the VMG title menu.
	menuPGCI := buildDVDMenuPGCITable(dvdVTSRootMenuEntryID, buildDVDCommandOnlyPGC(dvdJumpMenuCommand()))
	// A second of DVD time code is 30 frames of NTSC video, which last
	// 90090 ticks, or 25 frames of PAL video.
	ticksPerSecond := uint32(90_000)
	if profile.Standard == VideoStandardNTSC {
		ticksPerSecond = 90_090
	}
	tmap := buildDVDVTSTimeMap(records, ticksPerSecond)
	admapSector := dvdVTSIFOTimeMapSector + uint32((len(tmap)+DVDVOBSectorSize-1)/DVDVOBSectorSize)
	admapSectors := uint32((len(admap) + DVDVOBSectorSize - 1) / DVDVOBSectorSize)
	menuPGCISector := admapSector + admapSectors
	ifoSectors := menuPGCISector + uint32((len(menuPGCI)+DVDVOBSectorSize-1)/DVDVOBSectorSize)
	lastTitleSetSector := uint64(ifoSectors)*2 + uint64(vobSectors) - 1
	if lastTitleSetSector > math.MaxUint32 {
		return nil, 0, errors.New("VTS last-sector address exceeds 32 bits")
	}
	ifo := make([]byte, int(ifoSectors)*DVDVOBSectorSize)
	copy(ifo[:12], []byte("DVDVIDEO-VTS"))
	binary.BigEndian.PutUint32(ifo[0x000C:0x0010], uint32(lastTitleSetSector))
	binary.BigEndian.PutUint32(ifo[0x001C:0x0020], ifoSectors-1)
	binary.BigEndian.PutUint16(ifo[0x0020:0x0022], 0x0011)
	binary.BigEndian.PutUint32(ifo[0x0080:0x0084], dvdIFOMATLastByte)
	// VTSM_VOBS (0xC0), VTSM_C_ADT (0xD8) and VTSM_VOBU_ADMAP (0xDC) stay
	// zero, and so do the VTSM attributes at 0x100: there is no menu VOB.
	binary.BigEndian.PutUint32(ifo[0x00C4:0x00C8], ifoSectors)     // VTSTT_VOBS follows the VTSI.
	binary.BigEndian.PutUint32(ifo[0x00C8:0x00CC], 1)              // VTS_PTT_SRPT
	binary.BigEndian.PutUint32(ifo[0x00CC:0x00D0], 2)              // VTS_PGCI
	binary.BigEndian.PutUint32(ifo[0x00D0:0x00D4], menuPGCISector) // VTSM_PGCI_UT
	binary.BigEndian.PutUint32(ifo[0x00D4:0x00D8], dvdVTSIFOTimeMapSector)
	binary.BigEndian.PutUint32(ifo[0x00E0:0x00E4], 3)           // VTS_C_ADT
	binary.BigEndian.PutUint32(ifo[0x00E4:0x00E8], admapSector) // VTS_VOBU_ADMAP
	videoAttr := dvdVideoAttribute(title, profile)
	binary.BigEndian.PutUint16(ifo[dvdIFOAudioOffset:dvdIFOAudioOffset+2], videoAttr)
	binary.BigEndian.PutUint16(ifo[dvdIFOAudioOffset+2:dvdIFOAudioOffset+4], 1)
	ifo[0x0204] = dvdLPCMAudioAttribute0 // LPCM coding mode, unspecified language.
	ifo[0x0205] = dvdLPCMAudioAttribute1 // 16-bit, 48 kHz, stereo.
	putDVDSubtitleAttributes(ifo[dvdIFOAudioOffset+0x54:], title.Subtitles)

	ptt := make([]byte, 16)
	binary.BigEndian.PutUint16(ptt[0:2], 1)
	binary.BigEndian.PutUint32(ptt[4:8], uint32(len(ptt)-1))
	binary.BigEndian.PutUint32(ptt[8:12], 12)
	binary.BigEndian.PutUint16(ptt[12:14], 1) // PGCN
	binary.BigEndian.PutUint16(ptt[14:16], 1) // PGN
	copy(ifo[DVDVOBSectorSize:], ptt)

	post := make([][8]byte, 0, 2)
	if titleIndex < titleCount-1 {
		post = append(post, dvdConditionalCallVMGPGCCommand(dvdVMGTitleAdvancePGCN(menuPageCount, titleCount, titleIndex)))
	}
	post = append(post, dvdCallMenuCommand())
	var pre [][8]byte
	if len(title.Subtitles) > 0 {
		// Every start of the title selects the first subtitle stream, shown
		// or hidden; without the command a player could start with whatever
		// stream and display state it had.
		pre = append(pre, dvdSetSubtitleStreamCommand(0, profile.ShowSubtitles))
	}
	pgc := buildDVDVTSProgramChain(playbackTime, records, vobSectors, pre, post, dvdTitleSubpictureStreamControl(profile.Aspect, len(title.Subtitles)))
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
	copy(ifo[dvdVTSIFOTimeMapSector*DVDVOBSectorSize:], tmap)
	copy(ifo[int(admapSector)*DVDVOBSectorSize:], admap)
	copy(ifo[int(menuPGCISector)*DVDVOBSectorSize:], menuPGCI)
	return ifo, ifoSectors, nil
}

// buildDVDVTSTimeMap builds the VTS_TMAPTI of a title set whose only program
// chain plays the VOBUs of records as one cell. Entry n of the map is the
// sector of the VOBU presented n+1 time units after the start, where a unit
// is a whole number of seconds of ticksPerSecond ticks. The layout and the
// rules follow dvdauthor: no entry is made for a time at or after the end of
// the title, and an entry that is not followed by a VOBU one more unit ahead
// in the same cell has bit 31 set.
func buildDVDVTSTimeMap(records []dvdVOBURecord, ticksPerSecond uint32) []byte {
	// VOBU times are compared as differences from the first one: the
	// presentation clock of a VOB does not start at zero.
	base := records[0].StartPTM
	span := records[len(records)-1].EndPTM - base
	seconds := uint32(0)
	if span > 0 {
		seconds = (span - 1) / ticksPerSecond
	}
	unit := max((seconds+dvdVTSTimeMapMaxEntries-1)/dvdVTSTimeMapMaxEntries, 1)
	entries := int(seconds / unit)
	const headerSize, mapOffset = 8, 12
	tmap := make([]byte, mapOffset+4+4*entries)
	binary.BigEndian.PutUint16(tmap[0:2], 1) // one program chain.
	binary.BigEndian.PutUint32(tmap[4:8], uint32(len(tmap)-1))
	binary.BigEndian.PutUint32(tmap[headerSize:], mapOffset)
	tmap[mapOffset] = byte(unit)
	binary.BigEndian.PutUint16(tmap[mapOffset+2:], uint16(entries))
	index := 0
	for entry := range entries {
		elapsed := uint64(entry+1) * uint64(unit) * uint64(ticksPerSecond)
		for index < len(records)-1 && uint64(records[index].EndPTM-base) <= elapsed {
			index++
		}
		sector := records[index].StartSector
		if elapsed+uint64(unit)*uint64(ticksPerSecond) >= uint64(span) {
			sector |= 1 << 31
		}
		binary.BigEndian.PutUint32(tmap[mapOffset+4+4*entry:], sector)
	}
	return tmap
}

// buildDVDVTSProgramChain builds the PGC of a title: one program of one
// cell that plays every VOBU of records. subpictureControl holds the stream
// control entry of each subtitle stream; a title with subtitles also gets
// their colour table.
func buildDVDVTSProgramChain(playbackTime [4]byte, records []dvdVOBURecord, vobSectors uint32, preCommands, postCommands [][8]byte, subpictureControl [][4]byte) []byte {
	const headerSize = 0xec
	commands := slices.Concat(preCommands, postCommands)
	commandTableSize := 8 + len(commands)*8
	const programMapSize = 2 // one program cell number, padded to an even byte count.
	const cellPlaybackSize = 24
	const cellPositionSize = 4
	pgc := make([]byte, headerSize+commandTableSize+programMapSize+cellPlaybackSize+cellPositionSize)
	pgc[2], pgc[3] = 1, 1
	copy(pgc[4:8], playbackTime[:])
	pgc[0x0c] = 0x80 // first LPCM stream is available on private_stream_1 substream 0.
	for i, control := range subpictureControl {
		copy(pgc[0x1c+i*4:0x20+i*4], control[:])
	}
	if len(subpictureControl) > 0 {
		for i, color := range dvdSubtitlePalette {
			copy(pgc[0xa4+i*4:0xa8+i*4], color[:])
		}
	}
	binary.BigEndian.PutUint16(pgc[0x00e4:0x00e6], headerSize)
	binary.BigEndian.PutUint16(pgc[0x00e6:0x00e8], uint16(headerSize+commandTableSize))
	binary.BigEndian.PutUint16(pgc[0x00e8:0x00ea], uint16(headerSize+commandTableSize+programMapSize))
	binary.BigEndian.PutUint16(pgc[0x00ea:0x00ec], uint16(headerSize+commandTableSize+programMapSize+cellPlaybackSize))
	commandTable := pgc[headerSize : headerSize+commandTableSize]
	binary.BigEndian.PutUint16(commandTable[0:2], uint16(len(preCommands)))
	binary.BigEndian.PutUint16(commandTable[2:4], uint16(len(postCommands)))
	binary.BigEndian.PutUint16(commandTable[6:8], uint16(commandTableSize-1))
	for i, command := range commands {
		copy(commandTable[8+i*8:16+i*8], command[:])
	}
	pgc[headerSize+commandTableSize] = 1
	cell := pgc[headerSize+commandTableSize+programMapSize:]
	copy(cell[4:8], playbackTime[:])
	binary.BigEndian.PutUint32(cell[8:12], records[0].StartSector)
	binary.BigEndian.PutUint32(cell[16:20], records[len(records)-1].StartSector)
	binary.BigEndian.PutUint32(cell[20:24], vobSectors-1)
	position := pgc[headerSize+commandTableSize+programMapSize+cellPlaybackSize:]
	binary.BigEndian.PutUint16(position[0:2], 1)
	position[3] = 1
	return pgc
}

// putDVDSubtitleAttributes writes the number of subtitle streams of a title
// and their attributes, as a VTSI stores them from 0x254 on: a 2-bit RLE
// stream, with its language code when it has one.
func putDVDSubtitleAttributes(dst []byte, subtitles []EncodedSubtitle) {
	if len(subtitles) == 0 {
		return
	}
	binary.BigEndian.PutUint16(dst[0:2], uint16(len(subtitles)))
	for i, subtitle := range subtitles {
		attribute := dst[2+i*6 : 8+i*6]
		if subtitle.Language != "" {
			attribute[0] = 0x01 // the language code is set.
			copy(attribute[2:4], subtitle.Language)
		}
	}
}

// dvdTitleSubpictureStreamControl returns the PGC subpicture stream control
// entries of a title's subtitle streams: stream n is decoded from substream
// 0x20+n in every display mode, as one picture serves 4:3 and 16:9 displays.
func dvdTitleSubpictureStreamControl(aspect AspectRatio, streams int) [][4]byte {
	controls := make([][4]byte, streams)
	for i := range controls {
		n := byte(i)
		controls[i] = [4]byte{0x80 | n, 0, 0, 0}
		if aspect == AspectRatio16x9 {
			controls[i] = [4]byte{0x80 | n, n, n, n}
		}
	}
	return controls
}

// dvdSetSubtitleStreamCommand is SetSTN for the subpicture stream: it selects
// stream (zero-based) and shows or hides it.
func dvdSetSubtitleStreamCommand(stream int, show bool) [8]byte {
	value := byte(stream)
	if show {
		value |= 0x40
	}
	return [8]byte{0x51, 0, 0, 0, 0x80 | value, 0, 0, 0}
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
	return dvdTimeCode(duration, standard == VideoStandardPAL)
}

func buildDVDVMGI(titles []EncodedTitle, profile Profile, titleSetStarts []uint32, menu dvdAuthoredMenu) ([]byte, uint32, error) {
	if len(titles) == 0 || len(titles) > dvdMaxTitles {
		return nil, 0, errors.New("video2dvd: VMGI requires between 1 and 99 titles")
	}
	if menu.sectors == 0 {
		return nil, 0, errors.New("video2dvd: VMGI requires a title menu VOB")
	}
	attributes := make([]byte, len(titles)*776)
	for i, title := range titles {
		vtsAttrs := make([]byte, 0x300)
		// VMG_VTS_ATRT mirrors VTS IFO bytes 0x100..0x3ff. The title
		// attributes begin at file offset 0x200, hence offset 0x100 here.
		vtsAttrs[0x100] = byte(dvdVideoAttribute(title, profile) >> 8)
		vtsAttrs[0x101] = byte(dvdVideoAttribute(title, profile))
		binary.BigEndian.PutUint16(vtsAttrs[0x102:0x104], 1)
		vtsAttrs[0x104], vtsAttrs[0x105] = dvdLPCMAudioAttribute0, dvdLPCMAudioAttribute1
		putDVDSubtitleAttributes(vtsAttrs[0x154:], title.Subtitles)
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
		copy(attrTable[entry:entry+776], attributes[i*776:(i+1)*776])
	}
	tt := make([]byte, 8+12*len(titles))
	binary.BigEndian.PutUint16(tt[0:2], uint16(len(titles)))
	binary.BigEndian.PutUint32(tt[4:8], uint32(len(tt)-1))
	if titleSetStarts != nil && len(titleSetStarts) != len(titles) {
		return nil, 0, errors.New("VMGI title-set start list has the wrong length")
	}
	for i := range titles {
		base := 8 + i*12
		// One sequential PGC title. The two low bits would prohibit time
		// and chapter search by the user, so they stay clear.
		tt[base], tt[base+1] = 0x3c, 1                   // one angle.
		binary.BigEndian.PutUint16(tt[base+2:base+4], 1) // one chapter.
		tt[base+6], tt[base+7] = byte(i+1), 1
		if titleSetStarts != nil {
			binary.BigEndian.PutUint32(tt[base+8:base+12], titleSetStarts[i])
		}
	}
	menuTable := buildDVDVMGMenuPGCI(menu, len(titles))
	menuCellAddress := buildDVDCellAddressTable(menu.records, menu.sectors)
	menuVOBUAddress := buildDVDVOBUAddressMap(menu.records, menu.sectors)
	attrSectors := uint32((len(attrTable) + DVDVOBSectorSize - 1) / DVDVOBSectorSize)
	menuTableSector := uint32(2) + attrSectors
	menuTableSectors := uint32((len(menuTable) + DVDVOBSectorSize - 1) / DVDVOBSectorSize)
	menuCellAddressSector := menuTableSector + menuTableSectors
	menuCellAddressSectors := uint32((len(menuCellAddress) + DVDVOBSectorSize - 1) / DVDVOBSectorSize)
	menuVOBUAddressSector := menuCellAddressSector + menuCellAddressSectors
	menuVOBUAddressSectors := uint32((len(menuVOBUAddress) + DVDVOBSectorSize - 1) / DVDVOBSectorSize)
	vmgSectors := menuVOBUAddressSector + menuVOBUAddressSectors
	image := make([]byte, int(vmgSectors)*DVDVOBSectorSize)
	copy(image[:12], []byte("DVDVIDEO-VMG"))
	lastVMGSector := uint64(vmgSectors)*2 + uint64(menu.sectors) - 1
	if lastVMGSector > math.MaxUint32 {
		return nil, 0, errors.New("VMG last-sector address exceeds 32 bits")
	}
	binary.BigEndian.PutUint32(image[0x000C:0x0010], uint32(lastVMGSector))
	binary.BigEndian.PutUint32(image[0x001C:0x0020], vmgSectors-1)
	binary.BigEndian.PutUint16(image[0x0020:0x0022], 0x0011)
	binary.BigEndian.PutUint16(image[0x0026:0x0028], 1) // number of volumes.
	binary.BigEndian.PutUint16(image[0x0028:0x002A], 1) // volume number.
	image[0x002A] = 1                                   // disc side.
	binary.BigEndian.PutUint16(image[0x003E:0x0040], uint16(len(titles)))
	copy(image[0x0040:0x0040+12], []byte("VIDEO2DVD-GO"))
	binary.BigEndian.PutUint32(image[0x0080:0x0084], dvdIFOMATLastByte)
	binary.BigEndian.PutUint32(image[0x0084:0x0088], dvdVMGFirstPlayPGCOffset)
	binary.BigEndian.PutUint32(image[0x00C0:0x00C4], vmgSectors) // VMGM_VOBS follows the VMGI.
	binary.BigEndian.PutUint32(image[0x00C4:0x00C8], 1)          // TT_SRPT.
	binary.BigEndian.PutUint32(image[0x00C8:0x00CC], menuTableSector)
	binary.BigEndian.PutUint32(image[0x00D0:0x00D4], 2) // VMG_VTS_ATRT.
	binary.BigEndian.PutUint32(image[0x00D8:0x00DC], menuCellAddressSector)
	binary.BigEndian.PutUint32(image[0x00DC:0x00E0], menuVOBUAddressSector)
	// VMGM attributes describe the streams of the menu VOB.
	menuVideoAttr := dvdVideoAttribute(titles[0], profile)
	binary.BigEndian.PutUint16(image[0x0102:0x0104], 1)
	image[0x0104], image[0x0105] = dvdLPCMAudioAttribute0, dvdLPCMAudioAttribute1
	if menu.subpictureControl != [4]byte{} {
		// One 2-bit RLE subpicture stream of unspecified language; its
		// six attribute bytes at 0x156 are all zero.
		binary.BigEndian.PutUint16(image[0x0154:0x0156], 1)
		if titles[0].Aspect == AspectRatio16x9 {
			// The highlight exists for a widescreen display and for
			// letterboxing, so a 4:3 display must not pan and scan.
			menuVideoAttr |= dvdVideoAttrLetterboxOnly
		}
	}
	binary.BigEndian.PutUint16(image[0x0100:0x0102], menuVideoAttr)
	copy(image[dvdVMGFirstPlayPGCOffset:DVDVOBSectorSize], buildDVDFirstPlayPGC())
	copy(image[DVDVOBSectorSize:], tt)
	copy(image[2*DVDVOBSectorSize:], attrTable)
	copy(image[int(menuTableSector)*DVDVOBSectorSize:], menuTable)
	copy(image[int(menuCellAddressSector)*DVDVOBSectorSize:], menuCellAddress)
	copy(image[int(menuVOBUAddressSector)*DVDVOBSectorSize:], menuVOBUAddress)
	return image, vmgSectors, nil
}

// buildDVDCellAddressTable builds the C_ADT of the title menu, whose page n
// is cell 1 of VOB n.
func buildDVDCellAddressTable(records []dvdVOBURecord, vobSectors uint32) []byte {
	if len(records) == 0 {
		records = []dvdVOBURecord{{StartSector: 0, EndSector: vobSectors - 1}}
	}
	table := make([]byte, 8+12*len(records))
	// The first field counts the VOBs of the VOB set.
	binary.BigEndian.PutUint16(table[0:2], uint16(len(records)))
	binary.BigEndian.PutUint32(table[4:8], uint32(len(table)-1))
	for i, record := range records {
		entry := table[8+i*12 : 20+i*12]
		binary.BigEndian.PutUint16(entry[0:2], uint16(i+1)) // VOB ID.
		entry[2] = 1                                        // cell ID.
		binary.BigEndian.PutUint32(entry[4:8], record.StartSector)
		binary.BigEndian.PutUint32(entry[8:12], record.EndSector)
	}
	return table
}

func buildDVDVOBUAddressMap(records []dvdVOBURecord, vobSectors uint32) []byte {
	if len(records) == 0 {
		records = []dvdVOBURecord{{StartSector: 0, EndSector: vobSectors - 1}}
	}
	table := make([]byte, 4+4*len(records))
	binary.BigEndian.PutUint32(table[0:4], uint32(len(table)-1))
	for i, record := range records {
		binary.BigEndian.PutUint32(table[4+i*4:8+i*4], record.StartSector)
	}
	return table
}

func buildDVDFirstPlayPGC() []byte {
	const pgcHeaderSize = 0xec
	pgc := make([]byte, pgcHeaderSize+24)
	binary.BigEndian.PutUint16(pgc[0x00e4:0x00e6], pgcHeaderSize)
	commands := pgc[pgcHeaderSize:]
	binary.BigEndian.PutUint16(commands[0:2], 2) // set play-all sentinel, then enter the title menu.
	binary.BigEndian.PutUint16(commands[6:8], uint16(len(commands)-1))
	setMenuMode := dvdSetGPRMCommand(1, 0)
	jumpMenu := dvdJumpMenuCommand()
	copy(commands[8:16], setMenuMode[:])
	copy(commands[16:24], jumpMenu[:])
	return pgc
}

// buildDVDVMGMenuPGCI builds the VMGM_PGCI_UT. The PGCs are, in order: one
// per menu page, PLAY ALL, one launcher per title, and one PLAY ALL
// continuation per title.
func buildDVDVMGMenuPGCI(menu dvdAuthoredMenu, titleCount int) []byte {
	// Each encoded menu page is a separate VMG PGC/cell. This lets the
	// highlight buttons use LinkPGCN for deterministic page navigation.
	menuRecords := menu.records
	if len(menuRecords) == 0 {
		menuRecords = []dvdVOBURecord{{StartSector: 0, EndSector: menu.sectors - 1}}
	}
	pageCount := len(menuRecords)
	pgcs := make([][]byte, 0, pageCount+1+2*titleCount)
	for page := range pageCount {
		const commandTableSize = 24
		const programMapSize = 2
		const cellPlaybackSize = 24
		pgc := make([]byte, 0xec+commandTableSize+programMapSize+cellPlaybackSize+4)
		pgc[2], pgc[3] = 1, 1
		copy(pgc[4:8], menu.pageTime[:])
		pgc[0x0c] = 0x80
		if menu.subpictureControl != [4]byte{} {
			copy(pgc[0x1c:0x20], menu.subpictureControl[:])
			for i, color := range dvdMenuSubpicturePalette {
				copy(pgc[0xa4+i*4:0xa8+i*4], color[:])
			}
		}
		binary.BigEndian.PutUint16(pgc[0x00e4:0x00e6], 0xec)
		programMapOffset := 0xec + commandTableSize
		cellPlaybackOffset := programMapOffset + programMapSize
		cellPositionOffset := cellPlaybackOffset + cellPlaybackSize
		binary.BigEndian.PutUint16(pgc[0x00e6:0x00e8], uint16(programMapOffset))
		binary.BigEndian.PutUint16(pgc[0x00e8:0x00ea], uint16(cellPlaybackOffset))
		binary.BigEndian.PutUint16(pgc[0x00ea:0x00ec], uint16(cellPositionOffset))
		cmd := pgc[0xec:programMapOffset]
		binary.BigEndian.PutUint16(cmd[0:2], 1) // clear the PLAY ALL flag when returning to the menu.
		binary.BigEndian.PutUint16(cmd[2:4], 1)
		binary.BigEndian.PutUint16(cmd[6:8], uint16(len(cmd)-1))
		resetPlayAll := dvdSetGPRMCommand(0, 0)
		loop := linkPGCNCommand((page + 1) % pageCount)
		copy(cmd[8:16], resetPlayAll[:])
		copy(cmd[16:24], loop[:])
		pgc[programMapOffset] = 1
		cell := pgc[cellPlaybackOffset:]
		// A menu page is a still cell: after its 15 pictures the last one
		// stays visible until the user chooses a button, instead of
		// navigation executing the PGC post command and looping the cell.
		cell[2] = 0xff
		copy(cell[4:8], menu.pageTime[:])
		binary.BigEndian.PutUint32(cell[8:12], menuRecords[page].StartSector)
		binary.BigEndian.PutUint32(cell[16:20], menuRecords[page].StartSector) // the page's only VOBU.
		binary.BigEndian.PutUint32(cell[20:24], menuRecords[page].EndSector)
		// The cell position: the page is cell 1 of VOB page+1.
		binary.BigEndian.PutUint16(cell[24:26], uint16(page+1))
		cell[27] = 1
		pgcs = append(pgcs, pgc)
	}
	// Dedicated PLAY ALL PGC: set GPRM0 and jump to title 1.
	pgcs = append(pgcs, buildDVDCommandOnlyPGC(dvdSetPlayAllCommand(), dvdJumpTitleCommand(1)))
	for title := 1; title <= titleCount; title++ {
		pgcs = append(pgcs, buildDVDCommandOnlyPGC(dvdSetGPRMCommand(0, 0), dvdJumpTitleCommand(title)))
	}
	for title := 1; title <= titleCount; title++ {
		command := dvdJumpMenuCommand()
		if title < titleCount {
			command = dvdJumpTitleCommand(title + 1)
		}
		pgcs = append(pgcs, buildDVDCommandOnlyPGC(command))
	}
	return buildDVDMenuPGCITable(dvdVMGTitleMenuEntryID, pgcs...)
}

const (
	// dvdVideoAttrLetterboxOnly is the permitted display mode field of the
	// video attributes for 16:9 video that a 4:3 display may only letterbox.
	dvdVideoAttrLetterboxOnly = 0x0200
	dvdVMGTitleMenuEntryID    = 0x82
	dvdVTSRootMenuEntryID     = 0x83
	// First two bytes of the audio attributes of a 16-bit, 48 kHz, stereo
	// LPCM stream of unspecified language.
	dvdLPCMAudioAttribute0 = 0x80
	dvdLPCMAudioAttribute1 = 0x01
)

// buildDVDMenuPGCITable builds a menu PGCI unit table with one English
// language unit. The first PGC is the entry PGC of the menu named by entryID;
// the others are reachable by PGC number only.
func buildDVDMenuPGCITable(entryID byte, pgcs ...[]byte) []byte {
	pgcCount := len(pgcs)
	luSize := 8 + 8*pgcCount
	for _, pgc := range pgcs {
		luSize += len(pgc)
	}
	lu := make([]byte, luSize)
	binary.BigEndian.PutUint16(lu[0:2], uint16(pgcCount))
	binary.BigEndian.PutUint32(lu[4:8], uint32(len(lu)-1))
	pos := 8 + 8*pgcCount
	for i, pgc := range pgcs {
		entry := 8 + i*8
		if i == 0 {
			lu[entry] = entryID
		}
		binary.BigEndian.PutUint32(lu[entry+4:entry+8], uint32(pos))
		copy(lu[pos:], pgc)
		pos += len(pgc)
	}
	table := make([]byte, 8+8+len(lu))
	binary.BigEndian.PutUint16(table[0:2], 1)
	binary.BigEndian.PutUint32(table[4:8], uint32(len(table)-1))
	copy(table[8:10], []byte("en"))
	// Menu existence flags: bit 7 is the title menu in the VMGM and the root
	// menu in a VTSM, which is the one menu each table defines.
	table[11] = 0x80
	binary.BigEndian.PutUint32(table[12:16], 16)
	copy(table[16:], lu)
	return table
}

func buildDVDCommandOnlyPGC(commands ...[8]byte) []byte {
	const pgcHeaderSize = 0xec
	commandTableSize := 8 + len(commands)*8
	pgc := make([]byte, pgcHeaderSize+commandTableSize)
	binary.BigEndian.PutUint16(pgc[0x00e4:0x00e6], pgcHeaderSize)
	commandTable := pgc[pgcHeaderSize:]
	binary.BigEndian.PutUint16(commandTable[0:2], uint16(len(commands)))
	binary.BigEndian.PutUint16(commandTable[6:8], uint16(commandTableSize-1))
	for i, command := range commands {
		copy(commandTable[8+i*8:16+i*8], command[:])
	}
	return pgc
}

func dvdVMGTitleAdvancePGCN(pageCount, titleCount, titleIndex int) int {
	return pageCount + 2 + titleCount + titleIndex
}

func writeIFOAndBUP(dir, stem string, contents []byte) error {
	for _, ext := range []string{".IFO", ".BUP"} {
		path := filepath.Join(dir, stem+ext)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return fmt.Errorf("video2dvd: write %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func overwriteIFOAndBUP(dir, stem string, contents []byte) error {
	for _, ext := range []string{".IFO", ".BUP"} {
		path := filepath.Join(dir, stem+ext)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return fmt.Errorf("video2dvd: open %s for finalization: %w", filepath.Base(path), err)
		}
		_, writeErr := file.Write(contents)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("video2dvd: finalize %s: %w", filepath.Base(path), writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("video2dvd: close %s: %w", filepath.Base(path), closeErr)
		}
	}
	return nil
}
