package video2dvd

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Timing model of a title VOB, in 90 kHz ticks unless noted otherwise. The
// system clock starts at zero and advances with the bytes written. The first
// picture is decoded dvdMuxStartDelay later and presented one frame period
// after that, which is also where the audio timeline starts.
//
// Video time is counted in display fields. A frame picture lasts two of
// them, or three with repeat_first_field set, as the pictures of
// soft-telecined film have in turn; an NTSC field is 1501.5 ticks. Every
// timestamp is the exact time of its field rounded down to a whole tick, as
// mplex does, so that nothing accumulates: a film picture's PTS is 4504 or
// 4505 ticks after that of a three-field predecessor.
const (
	dvdVOBUMaxTicks  = 90_000
	dvdMuxStartDelay = 27_000
	// dvdMuxVideoLead is how long before its decode time a picture may start
	// to be sent. Together with the start delay it lets a picture of the
	// largest size MP@ML allows (224 KiB) arrive in time at the video share
	// of the mux rate.
	dvdMuxVideoLead = dvdMuxStartDelay
	// dvdMuxAudioLead is how long before its presentation time an audio pack
	// is sent. 0.2 s of LPCM plus one pack is 40320 bytes, inside the 58 KiB
	// DVD audio buffer.
	dvdMuxAudioLead = 18_000
	// dvdMuxSubtitleLead is how long before its presentation time a
	// subtitle unit starts to be sent, if the subpicture buffer has room.
	dvdMuxSubtitleLead = 45_000
	// dvdSubpictureBufferBytes is the subpicture decoder buffer: a unit is
	// kept in it until it is presented.
	dvdSubpictureBufferBytes = 52 * 1024

	dvdVideoPSTDBufferBytes = 232 * 1024
	dvdVideoPSTDBuffer      = 0x6000 | 232 // 232 KiB in 1024-byte units.
	dvdAudioPSTDBuffer      = 0x6000 | 58  // 58 KiB in 1024-byte units.
	dvdSubpicturePSTDBuffer = 0x6000 | 52  // 52 KiB in 1024-byte units.
	dvdVideoStreamID        = 0xe0
	dvdPrivateStream1ID     = 0xbd
	dvdLPCMSubstreamID      = 0xa0
	dvdLPCMPacketBytes      = dvdLPCMFramesPerPacket * dvdLPCMFrameBytes
	dvdLPCMFrameTicks       = dvdLPCMSamplesPerFrame * 90_000 / 48_000
	// VOBU search pointers hold a 30-bit sector offset whose all-ones value
	// is reserved.
	dvdVOBMaxSectorCount = 0x3fffffff
	dvdMuxNever          = math.MaxUint64
)

// MuxDVDTitleVOB combines one MPEG-2 elementary stream and one raw DVD LPCM
// stream into VOBUs with NAV packs and fixed-size MPEG program-stream sectors.
// The result does not include IFO/BUP files or complete DVD navigation tables.
//
// The output is one continuous sector stream whose NAV addresses are relative
// to its first sector; it is not split at the 1 GiB DVD file size limit.
func MuxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer) error {
	return muxDVDTitleVOB(ctx, title, workDir, dst, nil, nil)
}

// dvdVOBURecord locates one written VOBU. Sectors are relative to the start
// of the VOB set, which is the start of the VOB unless the VOB was placed
// after others (dvdVOBPlacement), and EndSector is the last sector of the
// VOBU. The presentation
// times are those stored in the VOBU's NAV pack.
type dvdVOBURecord struct {
	StartSector uint32
	EndSector   uint32
	StartPTM    uint32
	EndPTM      uint32
}

// dvdVOBPlacement locates a VOB inside its VOB set: the sector of the set at
// which the VOB starts, which NAV packs and VOBU records count from, and the
// VOB ID that its NAV packs carry.
type dvdVOBPlacement struct {
	firstSector uint32
	vobID       uint16
}

// dvdFirstVOB is the placement of a VOB that starts its VOB set.
var dvdFirstVOB = dvdVOBPlacement{firstSector: 0, vobID: 1}

func muxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer, recordVOBU func(dvdVOBURecord) error, buttonPages [][]DVDNAVButton) error {
	return muxDVDVOB(ctx, title, workDir, dst, recordVOBU, buttonPages, dvdFirstVOB)
}

func muxDVDVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer, recordVOBU func(dvdVOBURecord) error, buttonPages [][]DVDNAVButton, placement dvdVOBPlacement) error {
	if placement.vobID == 0 {
		return errors.New("video2dvd: VOB ID must not be zero")
	}
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if dst == nil {
		return errors.New("video2dvd: VOB destination writer is required")
	}
	audioStreams := titleAudioStreams(title)
	if title.VideoCodec != VideoCodecMPEG2 {
		return errors.New("video2dvd: VOB muxer requires MPEG-2 video")
	}
	if len(audioStreams) == 0 || len(audioStreams) > dvdMaxAudioStreams {
		return fmt.Errorf("video2dvd: a VOB has 1 to %d audio streams, not %d", dvdMaxAudioStreams, len(audioStreams))
	}
	for i, audio := range audioStreams {
		if audio.Codec != AudioCodecLPCM && audio.Codec != AudioCodecAC3 {
			return fmt.Errorf("video2dvd: VOB muxer requires LPCM or AC-3 audio, stream %d is %q", i+1, audio.Codec)
		}
	}
	if err := checkDVDFrameRate(title.FrameRateNumerator, title.FrameRateDenominator); err != nil {
		return fmt.Errorf("video2dvd: %w", err)
	}
	videoPath, err := validateGeneratedFile(title.VideoPath, workDir, "video")
	if err != nil {
		return err
	}
	audioPaths := make([]string, len(audioStreams))
	for i, audio := range audioStreams {
		if audioPaths[i], err = validateGeneratedFile(audio.Path, workDir, "audio"); err != nil {
			return err
		}
	}
	videoInfo, err := ProbeMPEG2Video(videoPath)
	if err != nil {
		return err
	}
	if videoInfo.Width != title.Width || videoInfo.Height != title.Height || videoInfo.Aspect != title.Aspect ||
		videoInfo.FrameRateNumerator*title.FrameRateDenominator != title.FrameRateNumerator*videoInfo.FrameRateDenominator {
		return errors.New("video2dvd: MPEG-2 stream metadata does not match the encoded title")
	}
	videoFile, err := os.Open(videoPath)
	if err != nil {
		return err
	}
	defer videoFile.Close()
	audioFiles := make([]*os.File, len(audioPaths))
	for i, path := range audioPaths {
		if audioFiles[i], err = os.Open(path); err != nil {
			return err
		}
		defer audioFiles[i].Close()
	}
	audioSources := func() ([]dvdMuxAudioSource, error) {
		sources := make([]dvdMuxAudioSource, len(audioFiles))
		for i, file := range audioFiles {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			sources[i] = dvdMuxAudioSource{codec: audioStreams[i].Codec, reader: file}
		}
		return sources, nil
	}
	if len(title.Subtitles) > dvdSubtitleMaxStreams {
		return fmt.Errorf("video2dvd: a title has at most %d subtitle streams", dvdSubtitleMaxStreams)
	}
	if len(title.Subtitles) > 0 && len(buttonPages) > 0 {
		return errors.New("video2dvd: a menu VOB cannot carry subtitles")
	}
	subtitleFiles := make([]*os.File, len(title.Subtitles))
	for i, subtitle := range title.Subtitles {
		path := subtitle.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		if subtitleFiles[i], err = os.Open(path); err != nil {
			return fmt.Errorf("video2dvd: open subtitle stream %d: %w", i+1, err)
		}
		defer subtitleFiles[i].Close()
	}
	subtitleReaders := func() ([]io.Reader, error) {
		readers := make([]io.Reader, len(subtitleFiles))
		for i, file := range subtitleFiles {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			readers[i] = file
		}
		return readers, nil
	}

	// A NAV pack addresses VOBUs up to two minutes ahead and stores the end
	// time of the VOB, none of which is known while the stream is produced.
	// The schedule depends only on the inputs, so it is computed once without
	// output and then replayed with the complete VOBU table at hand.
	highlights, err := buildDVDMenuHighlights(buttonPages, title.Aspect, title.Width, title.Height)
	if err != nil {
		return err
	}
	readers, err := subtitleReaders()
	if err != nil {
		return err
	}
	sources, err := audioSources()
	if err != nil {
		return err
	}
	planner := newDVDVOBMuxer(ctx, title, highlights, videoFile, sources, placement)
	planner.addSubtitles(readers)
	if err := planner.run(); err != nil {
		return err
	}
	if _, err := videoFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if sources, err = audioSources(); err != nil {
		return err
	}
	if readers, err = subtitleReaders(); err != nil {
		return err
	}
	writer := newDVDVOBMuxer(ctx, title, highlights, videoFile, sources, placement)
	writer.addSubtitles(readers)
	writer.dst = dst
	writer.recordVOBU = recordVOBU
	writer.plan = planner.vobus
	writer.spuPlan = planner.spus
	writer.search = make([]dvdVOBUSearchEntry, len(planner.vobus))
	for i, vobu := range planner.vobus {
		writer.search[i] = dvdVOBUSearchEntry{Sector: vobu.StartSector, StartPTM: vobu.StartPTM, EndPTM: vobu.EndPTM}
	}
	return writer.run()
}

// dvdMuxVOBU holds everything a VOBU's NAV pack states about the VOBU itself.
type dvdMuxVOBU struct {
	StartSector    uint32
	EndAddress     uint32 // last sector, relative to the NAV pack.
	SCR            uint64 // 27 MHz clock of the NAV pack.
	StartPTM       uint32
	EndPTM         uint32
	SequenceEndPTM uint32
	CellID         uint8
	CellElapsed    [4]byte
	ReferenceEnds  [3]uint32
	ReferenceCount int
	// AudioSync holds A_SYNCA of every audio stream.
	AudioSync [8]uint16
}

type dvdMuxPicture struct {
	data       []byte
	offset     int // bytes already sent.
	frame      int64
	startsVOBU bool
	// dts and pts are the picture's timestamps and end the time its display
	// ends, which is the next picture's pts.
	dts, pts, end uint64
}

// dvdMuxBufferedPicture is the part of a picture that has been sent and still
// waits in the decoder's input buffer for its decode time. Its size includes
// the PES headers of the packets whose last picture it is.
type dvdMuxBufferedPicture struct {
	frame int64
	dts   uint64 // 27 MHz.
	bytes int
}

type dvdMuxAudioPack struct {
	pts    uint64
	end    uint64
	sector uint32
}

// dvdVOBMuxer schedules the packs of one VOB against the system clock. With a
// nil dst it only plans: it runs the identical schedule and collects the VOBU
// table without building any pack.
type dvdVOBMuxer struct {
	ctx   context.Context
	title EncodedTitle
	// highlights holds the button highlight of each leading VOBU of a menu
	// VOB, one menu page per VOBU; it is empty for a title.
	highlights []dvdMenuHighlight
	audio      []*dvdMuxAudio
	pictures   mpeg2PictureReader

	dst        io.Writer
	recordVOBU func(dvdVOBURecord) error
	plan       []dvdMuxVOBU
	search     []dvdVOBUSearchEntry

	clock mpegSystemClock
	// sector is the VOB set sector of the next pack: the VOB's first sector
	// plus the sectors produced so far.
	sector uint64
	vobID  uint16
	vobus  []dvdMuxVOBU

	current    *dvdMuxPicture // next picture data to send.
	following  *dvdMuxPicture
	framesRead int64
	// fieldsRead counts the display fields of the pictures read so far and
	// lastFields those of the latest one.
	fieldsRead int64
	lastFields int64
	payload    []byte
	// videoBuffer models the decoder's video input buffer.
	videoBuffer   []dvdMuxBufferedPicture
	videoBuffered int
	vobuHasVideo  bool

	// subtitles holds the subtitle streams, stream n on substream 0x20+n.
	subtitles []*dvdMuxSubtitle
	// spus lists the subtitle units sent, per stream in presentation
	// order, and spuPlan those of the planning run, from which NAV packs
	// take SP_SYNCA.
	spus    [][]dvdMuxSPU
	spuPlan [][]dvdMuxSPU
}

// dvdMuxSubtitle is the state of one subtitle stream.
type dvdMuxSubtitle struct {
	reader *dvdSubtitleStreamReader
	// unit is the next unit to send, with its presentation time on the VOB
	// clock, and offset the bytes of it already sent; unit is nil once the
	// stream has ended.
	unit   *dvdSubtitleUnit
	offset int
	// buffer lists the units in the decoder's subpicture buffer, which
	// leave it at their presentation time.
	buffer   []dvdMuxBufferedPicture
	buffered int
	inVOBU   bool
}

// dvdMuxSPU records a subtitle unit: when it is shown and hidden, and the
// VOBU in which its first pack is.
type dvdMuxSPU struct {
	pts, end uint64
	vobu     int
}

func newDVDVOBMuxer(ctx context.Context, title EncodedTitle, highlights []dvdMenuHighlight, video io.Reader, audio []dvdMuxAudioSource, placement dvdVOBPlacement) *dvdVOBMuxer {
	m := &dvdVOBMuxer{
		ctx:        ctx,
		title:      title,
		highlights: highlights,
		pictures:   mpeg2PictureReader{ctx: ctx, src: video},
		clock:      newMPEGSystemClock(defaultProgramMuxRate),
		sector:     uint64(placement.firstSector),
		vobID:      placement.vobID,
	}
	// Streams are numbered within their codec: LPCM from substream 0xa0 and
	// AC-3 from 0x80 on, which is also the number the PGC's audio control
	// gives each of them.
	var lpcm, ac3 byte
	for _, source := range audio {
		stream := &dvdMuxAudio{codec: source.codec, reader: source.reader}
		if source.codec == AudioCodecAC3 {
			stream.substream = dvdAC3SubstreamID + ac3
			stream.ac3 = &ac3StreamReader{r: source.reader}
			ac3++
		} else {
			stream.substream = dvdLPCMSubstreamID + lpcm
			lpcm++
		}
		m.audio = append(m.audio, stream)
	}
	return m
}

// titleAudioStreams returns the audio streams of a title: its Audio list, or
// the single stream of AudioPath and AudioCodec.
func titleAudioStreams(title EncodedTitle) []EncodedAudio {
	if len(title.Audio) > 0 {
		return title.Audio
	}
	return []EncodedAudio{{Path: title.AudioPath, Codec: title.AudioCodec}}
}

// dvdMuxAudioSource is the data of one audio stream to multiplex.
type dvdMuxAudioSource struct {
	codec  string
	reader io.Reader
}

// dvdMuxAudio is the state of one audio stream of a VOB.
type dvdMuxAudio struct {
	codec     string
	substream byte
	reader    io.Reader
	inVOBU    bool

	// LPCM: chunk is the next packet payload behind room for its private
	// header, nil once the stream has ended; frames counts the LPCM frames
	// already sent.
	chunk   []byte
	storage [dvdLPCMPrivateHeaderLen + dvdLPCMPacketBytes]byte
	frames  int64

	// AC-3: queue holds the syncframes read ahead, the first of them
	// syncframe number first; offset bytes of it are sent. buffer models
	// the decoder's input buffer, which a syncframe leaves at its
	// presentation time.
	ac3      *ac3StreamReader
	queue    [][]byte
	first    int64
	offset   int
	ended    bool
	buffer   []dvdMuxBufferedPicture
	buffered int

	// recent lists sent packs that may still be the A_SYNCA target of a
	// VOBU that has not started; syncPending is the first VOBU whose target
	// has not been sent yet.
	recent      []dvdMuxAudioPack
	syncPending int
}

// pending reports whether data of the stream remains to be sent.
func (a *dvdMuxAudio) pending() bool {
	if a.codec == AudioCodecAC3 {
		return len(a.queue) > 0
	}
	return a.chunk != nil
}

// addSubtitles adds subtitle streams read from stream files.
func (m *dvdVOBMuxer) addSubtitles(readers []io.Reader) {
	for _, reader := range readers {
		m.subtitles = append(m.subtitles, &dvdMuxSubtitle{reader: newDVDSubtitleStreamReader(reader)})
	}
	m.spus = make([][]dvdMuxSPU, len(readers))
}

// readSubtitle loads the next unit of a subtitle stream.
func (m *dvdVOBMuxer) readSubtitle(subtitle *dvdMuxSubtitle) error {
	unit, err := subtitle.reader.next()
	if err == io.EOF {
		subtitle.unit = nil
		return nil
	}
	if err != nil {
		return err
	}
	unit.pts += m.startTime()
	subtitle.unit, subtitle.offset = &unit, 0
	return nil
}

// subtitlesPending reports whether a subtitle unit remains to be sent.
func (m *dvdVOBMuxer) subtitlesPending() bool {
	for _, subtitle := range m.subtitles {
		if subtitle.unit != nil {
			return true
		}
	}
	return false
}

// subtitleReadyAt returns the stream whose next pack may be sent first and
// the clock value from which it may.
func (m *dvdVOBMuxer) subtitleReadyAt(now uint64) (int, uint64) {
	best, bestAt := -1, uint64(dvdMuxNever)
	for stream, subtitle := range m.subtitles {
		released := 0
		for released < len(subtitle.buffer) && subtitle.buffer[released].dts <= now {
			subtitle.buffered -= subtitle.buffer[released].bytes
			released++
		}
		subtitle.buffer = slices.Delete(subtitle.buffer, 0, released)
		if subtitle.unit == nil {
			continue
		}
		pts := subtitle.unit.pts
		at := (pts - min(pts, dvdMuxSubtitleLead)) * mpegSCRExtensionRange
		if subtitle.offset == 0 && subtitle.buffered+len(subtitle.unit.data) > dvdSubpictureBufferBytes {
			at = max(at, subtitle.buffer[0].dts)
		}
		if subtitle.offset > 0 {
			at = 0 // a unit that has started is finished first.
		}
		if at < bestAt {
			best, bestAt = stream, at
		}
	}
	return best, bestAt
}

func (m *dvdVOBMuxer) run() error {
	var err error
	if m.current, err = m.readPicture(); err != nil {
		return err
	}
	if m.current == nil {
		return errors.New("video2dvd: MPEG-2 stream contains no pictures")
	}
	if m.following, err = m.readPicture(); err != nil {
		return err
	}
	for _, audio := range m.audio {
		if err := m.readAudio(audio); err != nil {
			return err
		}
	}
	for _, subtitle := range m.subtitles {
		if err := m.readSubtitle(subtitle); err != nil {
			return err
		}
	}
	// The VOB must open with a NAV pack, so the first picture goes first.
	if err := m.writeVideoPack(); err != nil {
		return err
	}
	for m.current != nil || m.audioPending() || m.subtitlesPending() {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		now := m.clock.now()
		m.releaseVideo(now)
		subtitleStream, subtitleAt := m.subtitleReadyAt(now)
		audioStream, audioAt := m.audioReadyAt(now)
		videoAt := uint64(dvdMuxNever)
		if m.current != nil {
			if videoAt, err = m.videoReadyAt(now); err != nil {
				return err
			}
		}
		// Audio goes first whenever it is due: its packs are small and
		// evenly spaced, so they interleave with a large picture instead
		// of waiting behind it. Subtitles are rare and small and come next.
		switch {
		case audioAt <= now:
			err = m.writeAudioPack(audioStream)
		case subtitleAt <= now:
			err = m.writeSubtitlePack(subtitleStream)
		case videoAt <= now:
			err = m.writeVideoPack()
		case audioAt == dvdMuxNever && videoAt == dvdMuxNever && subtitleAt == dvdMuxNever:
			return errors.New("video2dvd: subtitle units cannot be scheduled")
		default:
			m.clock.wait(min(audioAt, videoAt, subtitleAt))
		}
		if err != nil {
			return err
		}
	}
	if err := m.writePack(func(scr uint64) ([]byte, error) {
		return buildMPEGProgramEndPack(scr, defaultProgramMuxRate/50)
	}); err != nil {
		return err
	}
	if err := m.finishVOBU(); err != nil {
		return err
	}
	if m.dst != nil && !slices.Equal(m.vobus, m.plan) {
		return errDVDMuxInputChanged
	}
	return nil
}

var errDVDMuxInputChanged = errors.New("video2dvd: the encoded title changed while it was being multiplexed")

// writePack accounts for one sector and, unless planning, builds and writes it.
func (m *dvdVOBMuxer) writePack(build func(scr uint64) ([]byte, error)) error {
	scr := m.clock.now()
	if m.sector >= dvdVOBMaxSectorCount {
		return errors.New("video2dvd: VOB exceeds the DVD sector address range")
	}
	if scr/mpegSCRExtensionRange > maxMPEGTimestamp {
		return errors.New("video2dvd: VOB exceeds the MPEG 33-bit clock range")
	}
	if m.dst != nil {
		pack, err := build(scr)
		if err != nil {
			return err
		}
		if err := writeAll(m.dst, pack); err != nil {
			return err
		}
	}
	m.sector++
	m.clock.advance(DVDVOBSectorSize)
	return nil
}

// fieldTime is the time at which display field number field of the VOB
// starts. The first picture is decoded at field zero and, one frame period
// later, displayed from field two on.
func (m *dvdVOBMuxer) fieldTime(field int64) uint64 {
	return dvdMuxStartDelay + dvdVideoFieldTicks(field, m.title.FrameRateNumerator, m.title.FrameRateDenominator)
}

// startTime is the presentation time of the first picture.
func (m *dvdVOBMuxer) startTime() uint64 {
	return m.fieldTime(2)
}

// timePicture sets a picture's timestamps from the number of fields it is
// displayed for. The stream has no B pictures, so a picture is decoded when
// its predecessor is presented and stays undisplayed for as long as that one
// is shown: PTS minus DTS is one frame period, or a period and a half behind
// a picture with repeat_first_field. The first picture waits one frame
// period.
func (m *dvdVOBMuxer) timePicture(picture *dvdMuxPicture, fields int64) {
	const firstField = 2
	previous := m.lastFields
	if m.framesRead == 0 {
		previous = firstField
	}
	picture.dts = m.fieldTime(firstField + m.fieldsRead - previous)
	picture.pts = m.fieldTime(firstField + m.fieldsRead)
	picture.end = m.fieldTime(firstField + m.fieldsRead + fields)
	m.fieldsRead += fields
	m.lastFields = fields
}

// audioPTS is the presentation time of the next LPCM packet of a stream. An
// LPCM frame of 80 samples lasts exactly 150 ticks.
func (m *dvdVOBMuxer) audioPTS(audio *dvdMuxAudio) uint64 {
	return m.startTime() + uint64(audio.frames)*dvdLPCMFrameTicks
}

// ac3PTS is the presentation time of AC-3 syncframe number frame. A
// syncframe of 1536 samples lasts exactly 2880 ticks, and the stream starts
// dvdAC3PresentationLead before the first picture: its first 256 samples
// are the encoder's silent block, so that the audio itself starts with the
// first picture, like LPCM.
func (m *dvdVOBMuxer) ac3PTS(frame int64) uint64 {
	return m.startTime() - dvdAC3PresentationLead + uint64(frame)*ac3SyncFrameTicks
}

// audioPending reports whether any audio remains to be sent.
func (m *dvdVOBMuxer) audioPending() bool {
	for _, audio := range m.audio {
		if audio.pending() {
			return true
		}
	}
	return false
}

// audioReadyAt returns the audio stream whose next pack may be sent first
// and the clock value from which it may. A pack is sent dvdMuxAudioLead
// before the audio it starts with is presented; an AC-3 pack also waits
// until the decoder's buffer has room for it.
func (m *dvdVOBMuxer) audioReadyAt(now uint64) (int, uint64) {
	best, bestAt := -1, uint64(dvdMuxNever)
	for index, audio := range m.audio {
		if !audio.pending() {
			continue
		}
		var at uint64
		if audio.codec == AudioCodecAC3 {
			released := 0
			for released < len(audio.buffer) && audio.buffer[released].dts <= now {
				audio.buffered -= audio.buffer[released].bytes
				released++
			}
			audio.buffer = slices.Delete(audio.buffer, 0, released)
			pts := m.ac3PTS(audio.first)
			at = (pts - min(pts, dvdMuxAudioLead)) * mpegSCRExtensionRange
			if size := m.ac3PackLayout(audio).size; audio.buffered+size > dvdAC3BufferBytes && len(audio.buffer) > 0 {
				at = max(at, audio.buffer[0].dts)
			}
		} else {
			pts := m.audioPTS(audio)
			at = (pts - min(pts, dvdMuxAudioLead)) * mpegSCRExtensionRange
		}
		if at < bestAt {
			best, bestAt = index, at
		}
	}
	return best, bestAt
}

func (m *dvdVOBMuxer) readPicture() (*dvdMuxPicture, error) {
	data, err := m.pictures.Next()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pictureType, err := mpeg2PictureType(data)
	if m.framesRead == 0 && (err != nil || pictureType != 1 || !mpeg2PictureHasSequenceHeader(data)) {
		return nil, errors.New("video2dvd: the first MPEG-2 picture must be an I picture with a sequence header")
	}
	if err != nil {
		return nil, err
	}
	if pictureType == 3 {
		return nil, errors.New("video2dvd: B pictures are not supported by the DVD VOB muxer")
	}
	if pictureType != 1 && pictureType != 2 {
		return nil, fmt.Errorf("video2dvd: unsupported MPEG-2 picture type %d", pictureType)
	}
	if pictureType == 1 && !mpeg2PictureHasSequenceHeader(data) {
		return nil, errors.New("video2dvd: each VOBU I picture must include a sequence header")
	}
	display, err := mpeg2PictureDisplayOf(data)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: MPEG-2 picture %d: %w", m.framesRead, err)
	}
	picture := &dvdMuxPicture{data: data, frame: m.framesRead, startsVOBU: pictureType == 1}
	m.timePicture(picture, int64(display.fields))
	m.framesRead++
	return picture, nil
}

// readAudio loads the next LPCM packet payload behind room for its private
// header, padding a final partial audio frame with silence, or for AC-3
// reads syncframes ahead until a pack's worth is queued.
func (m *dvdVOBMuxer) readAudio(audio *dvdMuxAudio) error {
	if audio.codec == AudioCodecAC3 {
		queued := -audio.offset
		for _, frame := range audio.queue {
			queued += len(frame)
		}
		for !audio.ended && queued < DVDVOBSectorSize {
			frame, err := audio.ac3.next()
			if err == io.EOF {
				audio.ended = true
				break
			}
			if err != nil {
				return err
			}
			audio.queue = append(audio.queue, frame)
			queued += len(frame)
		}
		return nil
	}
	data := audio.storage[dvdLPCMPrivateHeaderLen:]
	n, err := io.ReadFull(audio.reader, data)
	if err == io.EOF {
		audio.chunk = nil
		return nil
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	if n%4 != 0 {
		return errors.New("video2dvd: LPCM input ends in an incomplete stereo sample")
	}
	padded := (n + dvdLPCMFrameBytes - 1) / dvdLPCMFrameBytes * dvdLPCMFrameBytes
	clear(data[n:padded])
	audio.chunk = audio.storage[:dvdLPCMPrivateHeaderLen+padded]
	return nil
}

// releaseVideo removes the pictures decoded by now from the buffer model.
func (m *dvdVOBMuxer) releaseVideo(now uint64) {
	released := 0
	for released < len(m.videoBuffer) && m.videoBuffer[released].dts <= now {
		m.videoBuffered -= m.videoBuffer[released].bytes
		released++
	}
	m.videoBuffer = slices.Delete(m.videoBuffer, 0, released)
}

// videoReadyAt returns the earliest clock value at which the next video pack
// may be sent: now if it may go immediately.
func (m *dvdVOBMuxer) videoReadyAt(now uint64) (uint64, error) {
	dts := m.current.dts
	if earliest := (dts - min(dts, dvdMuxVideoLead)) * mpegSCRExtensionRange; earliest > now {
		return earliest, nil
	}
	if m.videoBuffered+DVDVOBSectorSize > dvdVideoPSTDBufferBytes {
		head := m.videoBuffer[0]
		if head.frame == m.current.frame {
			return 0, fmt.Errorf("video2dvd: MPEG-2 picture %d does not fit the %d KiB DVD video buffer", head.frame, dvdVideoPSTDBufferBytes/1024)
		}
		return head.dts, nil
	}
	return now, nil
}

// writeVideoPack sends the next sector of video. A pack carries the rest of
// the current picture and, when room remains, the start of the next one; at
// most one picture starts in a pack so that each has its own timestamps. A
// VOBU's first picture always starts a pack, directly after the NAV pack.
func (m *dvdVOBMuxer) writeVideoPack() error {
	picture := m.current
	if picture.offset == 0 && picture.startsVOBU {
		if err := m.startVOBU(picture); err != nil {
			return err
		}
	}
	// Every byte of a picture has to reach the decoder before the picture is
	// decoded. The first picture in the pack is the earliest one.
	if m.clock.after(DVDVOBSectorSize) > picture.dts*mpegSCRExtensionRange {
		return fmt.Errorf("video2dvd: MPEG-2 video near frame %d exceeds the DVD multiplex rate: it cannot reach the decoder before its decode time (beside LPCM audio the video must stay below about 8.2 Mbit/s)", picture.frame)
	}
	header := mpegPESHeader{streamID: dvdVideoStreamID}
	if !m.vobuHasVideo {
		header.pstdBuffer = dvdVideoPSTDBuffer
		m.vobuHasVideo = true
	}
	// Decide first whether a picture starts in this pack: its timestamps
	// take room that the payload could otherwise use. The next picture only
	// joins when its whole start code fits, so a demultiplexer finds the
	// picture the timestamps belong to inside the packet that carries them.
	const timestampBytes, startCodeBytes = 10, 4
	starting := picture
	if picture.offset != 0 {
		starting = nil
		joinable := m.following != nil && !m.following.startsVOBU
		if joinable && len(picture.data)-picture.offset <= header.capacity()-timestampBytes-startCodeBytes {
			starting = m.following
		}
	}
	if starting != nil {
		header.hasPTS, header.hasDTS = true, true
		header.pts = starting.pts
		header.dts = starting.dts
	}
	capacity := header.capacity()
	payload := m.payload[:0]
	for len(payload) < capacity {
		count := min(capacity-len(payload), len(picture.data)-picture.offset)
		payload = append(payload, picture.data[picture.offset:picture.offset+count]...)
		picture.offset += count
		m.bufferVideo(picture, count)
		if picture.offset < len(picture.data) {
			break
		}
		m.finishPicture(picture)
		var err error
		m.current = m.following
		if m.following != nil {
			if m.following, err = m.readPicture(); err != nil {
				return err
			}
		}
		if starting == nil || m.current != starting || picture == starting {
			break
		}
		picture = m.current
	}
	m.payload = payload
	// The PES header, including any stuffing that completes the pack, enters
	// the decoder buffer with the data and leaves with the last picture.
	overhead := mpegPESFixedHeaderSize + header.dataLength()
	if spare := capacity - len(payload); spare < mpegMinPaddingPacketSize {
		overhead += spare
	}
	m.videoBuffer[len(m.videoBuffer)-1].bytes += overhead
	m.videoBuffered += overhead
	return m.writePack(func(scr uint64) ([]byte, error) {
		return buildMPEGPESPack(scr, defaultProgramMuxRate/50, header, payload)
	})
}

func (m *dvdVOBMuxer) bufferVideo(picture *dvdMuxPicture, count int) {
	m.videoBuffered += count
	if last := len(m.videoBuffer) - 1; last >= 0 && m.videoBuffer[last].frame == picture.frame {
		m.videoBuffer[last].bytes += count
		return
	}
	m.videoBuffer = append(m.videoBuffer, dvdMuxBufferedPicture{
		frame: picture.frame,
		dts:   picture.dts * mpegSCRExtensionRange,
		bytes: count,
	})
}

// finishPicture records a picture whose last byte is in the sector about to
// be written.
func (m *dvdVOBMuxer) finishPicture(picture *dvdMuxPicture) {
	vobu := &m.vobus[len(m.vobus)-1]
	if vobu.ReferenceCount < len(vobu.ReferenceEnds) {
		vobu.ReferenceEnds[vobu.ReferenceCount] = uint32(m.sector) - vobu.StartSector
		vobu.ReferenceCount++
	}
	vobu.EndPTM = uint32(picture.end)
	if findMPEGStartCode(picture.data, 0xb7, max(0, len(picture.data)-4)) >= 0 {
		vobu.SequenceEndPTM = vobu.EndPTM
	}
}

func (m *dvdVOBMuxer) writeAudioPack(index int) error {
	audio := m.audio[index]
	if audio.codec == AudioCodecAC3 {
		return m.writeAC3Pack(index)
	}
	pts := m.audioPTS(audio)
	frames := (len(audio.chunk) - dvdLPCMPrivateHeaderLen) / dvdLPCMFrameBytes
	end := pts + uint64(frames)*dvdLPCMFrameTicks
	if m.clock.after(DVDVOBSectorSize) > pts*mpegSCRExtensionRange {
		return errors.New("video2dvd: LPCM audio cannot reach the decoder before its presentation time")
	}
	header := mpegPESHeader{streamID: dvdPrivateStream1ID, hasPTS: true, pts: pts}
	if !audio.inVOBU {
		header.pstdBuffer = dvdAudioPSTDBuffer
		audio.inVOBU = true
	}
	putDVDLPCMPrivateHeader(audio.chunk, audio.substream, frames, audio.frames)
	sector := uint32(m.sector)
	chunk := audio.chunk
	if err := m.writePack(func(scr uint64) ([]byte, error) {
		return buildMPEGPESPack(scr, defaultProgramMuxRate/50, header, chunk)
	}); err != nil {
		return err
	}
	audio.frames += int64(frames)
	m.recordAudioPack(index, pts, end, sector)
	return m.readAudio(audio)
}

// recordAudioPack notes a sent pack of an audio stream that starts the
// audio presented from pts until end, for the A_SYNCA of the VOBUs.
func (m *dvdVOBMuxer) recordAudioPack(index int, pts, end uint64, sector uint32) {
	audio := m.audio[index]
	for audio.syncPending < len(m.vobus) {
		vobu := &m.vobus[audio.syncPending]
		if vobu.AudioSync[index] == DVDNAVAudioSyncNone {
			if uint64(vobu.StartPTM) >= end {
				break
			}
			vobu.AudioSync[index] = dvdAudioSyncOffset(sector, vobu.StartSector)
		}
		audio.syncPending++
	}
	// A later VOBU starts at most a few packs behind the audio, so a short
	// history is enough to find the pack presented at its start.
	const history = 1024
	if len(audio.recent) >= history {
		audio.recent = slices.Delete(audio.recent, 0, history/2)
	}
	audio.recent = append(audio.recent, dvdMuxAudioPack{pts: pts, end: end, sector: sector})
}

// dvdAC3PackLayout describes the next pack of an AC-3 stream.
type dvdAC3PackLayout struct {
	header mpegPESHeader
	// size is the audio data it carries, starts the number of syncframes
	// that begin in it and lead the bytes before the first of them.
	size, starts, lead int
}

// ac3PackLayout plans the next pack of an AC-3 stream. A pack in which a
// syncframe begins carries the PTS of that syncframe; a pack in which none
// does carries none, and is cut short instead of letting a syncframe begin
// in the five bytes the PTS would have taken.
func (m *dvdVOBMuxer) ac3PackLayout(audio *dvdMuxAudio) dvdAC3PackLayout {
	layout := dvdAC3PackLayout{header: mpegPESHeader{streamID: dvdPrivateStream1ID}}
	if !audio.inVOBU {
		layout.header.pstdBuffer = dvdAC3PSTDBuffer
	}
	remaining := -audio.offset
	for _, frame := range audio.queue {
		remaining += len(frame)
	}
	// The first syncframe that begins in the pack, relative to its data.
	firstStart := 0
	if audio.offset > 0 {
		firstStart = len(audio.queue[0]) - audio.offset
	}
	withPTS := layout.header
	withPTS.hasPTS = true
	capacity := withPTS.capacity() - dvdAC3PrivateHeaderLen
	if firstStart < min(capacity, remaining) {
		layout.header.hasPTS = true
		layout.header.pts = m.ac3PTS(audio.first + int64(min(audio.offset, 1)))
	} else {
		capacity = min(layout.header.capacity()-dvdAC3PrivateHeaderLen, firstStart)
	}
	layout.size = min(capacity, remaining)
	layout.lead = firstStart
	for position, frame := firstStart, int64(0); position < layout.size && int(frame)+min(audio.offset, 1) < len(audio.queue); frame++ {
		layout.starts++
		position += len(audio.queue[int(frame)+min(audio.offset, 1)])
	}
	return layout
}

// writeAC3Pack sends the next pack of an AC-3 stream. Its private header
// counts the syncframes that begin in it and points at the first: the
// pointer counts from its own last byte, so a syncframe that directly
// follows the header is at 1.
func (m *dvdVOBMuxer) writeAC3Pack(index int) error {
	audio := m.audio[index]
	layout := m.ac3PackLayout(audio)
	audio.inVOBU = true
	if m.clock.after(DVDVOBSectorSize) > m.ac3PTS(audio.first)*mpegSCRExtensionRange {
		return fmt.Errorf("video2dvd: AC-3 audio stream %d cannot reach the decoder before its presentation time", index+1)
	}
	payload := make([]byte, dvdAC3PrivateHeaderLen, dvdAC3PrivateHeaderLen+layout.size)
	payload[0] = audio.substream
	payload[1] = byte(layout.starts)
	if layout.starts > 0 {
		binary.BigEndian.PutUint16(payload[2:4], uint16(layout.lead+1))
	}
	firstPTS := m.ac3PTS(audio.first + int64(min(audio.offset, 1)))
	lastStart := audio.first + int64(min(audio.offset, 1)) + int64(layout.starts) - 1
	for need := layout.size; need > 0; {
		frame := audio.queue[0]
		count := min(need, len(frame)-audio.offset)
		payload = append(payload, frame[audio.offset:audio.offset+count]...)
		// The bytes stay in the decoder's buffer until their syncframe is
		// presented.
		audio.buffer = append(audio.buffer, dvdMuxBufferedPicture{frame: audio.first, dts: m.ac3PTS(audio.first) * mpegSCRExtensionRange, bytes: count})
		audio.buffered += count
		audio.offset += count
		need -= count
		if audio.offset == len(frame) {
			audio.queue = audio.queue[1:]
			audio.first++
			audio.offset = 0
		}
	}
	header := layout.header
	sector := uint32(m.sector)
	if err := m.writePack(func(scr uint64) ([]byte, error) {
		return buildMPEGPESPack(scr, defaultProgramMuxRate/50, header, payload)
	}); err != nil {
		return err
	}
	if layout.starts > 0 {
		m.recordAudioPack(index, firstPTS, m.ac3PTS(lastStart)+ac3SyncFrameTicks, sector)
	}
	return m.readAudio(audio)
}

// writeSubtitlePack sends the next pack of a subtitle stream. The first pack
// of a unit carries its presentation time.
func (m *dvdVOBMuxer) writeSubtitlePack(stream int) error {
	subtitle := m.subtitles[stream]
	unit := subtitle.unit
	header := mpegPESHeader{streamID: dvdPrivateStream1ID}
	if subtitle.offset == 0 {
		header.hasPTS, header.pts = true, unit.pts
	}
	if !subtitle.inVOBU {
		header.pstdBuffer = dvdSubpicturePSTDBuffer
		subtitle.inVOBU = true
	}
	if m.clock.after(DVDVOBSectorSize) > unit.pts*mpegSCRExtensionRange {
		return fmt.Errorf("video2dvd: subtitle stream %d cannot reach the decoder before its presentation time", stream+1)
	}
	count := min(header.capacity()-1, len(unit.data)-subtitle.offset)
	payload := append(append(m.payload[:0], dvdMenuSubpictureSubstream+byte(stream)), unit.data[subtitle.offset:subtitle.offset+count]...)
	m.payload = payload
	if subtitle.offset == 0 {
		end, ok := dvdSubtitleUnitEnd(unit.data)
		spu := dvdMuxSPU{pts: unit.pts, end: unit.pts + end, vobu: len(m.vobus) - 1}
		if !ok {
			spu.end = dvdMuxNever
		}
		m.spus[stream] = append(m.spus[stream], spu)
		subtitle.buffer = append(subtitle.buffer, dvdMuxBufferedPicture{dts: unit.pts * mpegSCRExtensionRange, bytes: len(unit.data)})
		subtitle.buffered += len(unit.data)
	}
	if err := m.writePack(func(scr uint64) ([]byte, error) {
		return buildMPEGPESPack(scr, defaultProgramMuxRate/50, header, payload)
	}); err != nil {
		return err
	}
	subtitle.offset += count
	if subtitle.offset < len(unit.data) {
		return nil
	}
	return m.readSubtitle(subtitle)
}

// dvdSubtitleUnitEnd returns how long after its presentation time a
// subtitle unit hides its picture: the delay of the control sequence that
// holds STP_DSP. It reports false for a unit that never does.
func dvdSubtitleUnitEnd(unit []byte) (uint64, bool) {
	offset, visited := int(binary.BigEndian.Uint16(unit[2:4])), 0
	for offset+4 <= len(unit) && visited < 64 {
		visited++
		delay := uint64(binary.BigEndian.Uint16(unit[offset:])) * dvdSubtitleDelayTicks
		next := int(binary.BigEndian.Uint16(unit[offset+2:]))
		for position := offset + 4; position < len(unit); {
			command := unit[position]
			if command == 0x02 {
				return delay, true
			}
			size, ok := map[byte]int{0x00: 1, 0x01: 1, 0x03: 3, 0x04: 3, 0x05: 7, 0x06: 5}[command]
			if !ok {
				break // CMD_END, or a command whose size is not fixed.
			}
			position += size
		}
		if next == offset {
			break
		}
		offset = next
	}
	return 0, false
}

// dvdSubpictureSync returns SP_SYNCA of one subtitle stream for VOBU index,
// following dvdauthor: the offset to the VOBU holding the first pack of the
// unit shown at the start of the VOBU, or else of the unit that comes up
// next during the VOBU, with bit 31 set for an earlier VOBU. A unit in this
// VOBU, or none to point at, is DVDNAVSubpictureSyncCurrent.
func (m *dvdVOBMuxer) dvdSubpictureSync(stream, index int) uint32 {
	vobu := m.plan[index]
	start, end := uint64(vobu.StartPTM), uint64(vobu.EndPTM)
	spus := m.spuPlan[stream]
	// spus[following] is the first unit presented after the VOBU starts.
	following, _ := slices.BinarySearchFunc(spus, start+1, func(spu dvdMuxSPU, pts uint64) int {
		return cmp.Compare(spu.pts, pts)
	})
	target := -1
	switch {
	case following > 0 && spus[following-1].end > start:
		target = spus[following-1].vobu
	case following < len(spus) && spus[following].pts < end:
		target = spus[following].vobu
	}
	if target < 0 || target == index {
		return DVDNAVSubpictureSyncCurrent
	}
	here, there := m.plan[index].StartSector, m.plan[target].StartSector
	if there < here {
		return (here - there) | 0x80000000
	}
	return there - here
}

// dvdAudioSyncOffset encodes A_SYNCA for an audio pack relative to a NAV pack.
func dvdAudioSyncOffset(audioSector, navSector uint32) uint16 {
	const maxOffset = 0x1fff
	switch {
	case audioSector >= navSector && audioSector-navSector <= maxOffset:
		return uint16(audioSector - navSector)
	case audioSector < navSector && navSector-audioSector <= maxOffset:
		return uint16(navSector-audioSector) | 0x8000
	default:
		return 0
	}
}

// startVOBU closes the previous VOBU and emits the NAV pack of the one that
// begins with picture.
func (m *dvdVOBMuxer) startVOBU(picture *dvdMuxPicture) error {
	if len(m.vobus) > 0 {
		if err := m.finishVOBU(); err != nil {
			return err
		}
	}
	index := len(m.vobus)
	start := picture.pts
	if start+dvdVOBUMaxTicks > math.MaxUint32 {
		return errors.New("video2dvd: title exceeds the DVD navigation timestamp range")
	}
	vobu := dvdMuxVOBU{
		StartSector:   uint32(m.sector),
		SCR:           m.clock.now(),
		StartPTM:      uint32(start),
		EndPTM:        uint32(start),
		CellID:        1,
		ReferenceEnds: [3]uint32{math.MaxUint32, math.MaxUint32, math.MaxUint32},
	}
	elapsed := start - m.startTime()
	if index < len(m.highlights) {
		if index >= math.MaxUint8 {
			return errors.New("video2dvd: menu contains more cells than a DVD VOB can address")
		}
		// Each menu page is a cell of its own.
		vobu.CellID = uint8(index + 1)
		elapsed = 0
	}
	var err error
	vobu.CellElapsed, err = dvdCellElapsedTime(time.Duration(elapsed)*time.Second/90_000, m.title.FrameRateNumerator, m.title.FrameRateDenominator)
	if err != nil {
		return err
	}
	for index, audio := range m.audio {
		vobu.AudioSync[index] = DVDNAVAudioSyncNone
		for len(audio.recent) > 0 && audio.recent[0].end <= start {
			audio.recent = audio.recent[1:]
		}
		if len(audio.recent) > 0 && audio.recent[0].pts <= start {
			vobu.AudioSync[index] = dvdAudioSyncOffset(audio.recent[0].sector, vobu.StartSector)
		}
		audio.inVOBU = false
	}
	m.vobus = append(m.vobus, vobu)
	m.vobuHasVideo = false
	for _, subtitle := range m.subtitles {
		subtitle.inVOBU = false
	}
	if err := m.writePack(func(uint64) ([]byte, error) { return m.buildNAVPack(index) }); err != nil {
		return err
	}
	if index < len(m.highlights) {
		return m.writeSubpicturePacks(m.highlights[index], start)
	}
	return nil
}

// writeSubpicturePacks sends the highlight subpictures of a menu VOBU right
// after its NAV pack, one pack per stream. They are presented with the first
// picture of the VOBU, which is when the highlight of the NAV pack starts.
func (m *dvdVOBMuxer) writeSubpicturePacks(highlight dvdMenuHighlight, pts uint64) error {
	for _, payload := range highlight.subpictures {
		if m.clock.after(DVDVOBSectorSize) > pts*mpegSCRExtensionRange {
			return errors.New("video2dvd: menu subpicture cannot reach the decoder before its presentation time")
		}
		// Each pack is the only one of its stream in the VOBU, so each
		// states the subpicture decoder's buffer size.
		header := mpegPESHeader{streamID: dvdPrivateStream1ID, hasPTS: true, pts: pts, pstdBuffer: dvdSubpicturePSTDBuffer}
		if err := m.writePack(func(scr uint64) ([]byte, error) {
			return buildMPEGPESPack(scr, defaultProgramMuxRate/50, header, payload)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m *dvdVOBMuxer) finishVOBU() error {
	vobu := &m.vobus[len(m.vobus)-1]
	if vobu.EndPTM-vobu.StartPTM > dvdVOBUMaxTicks {
		return errors.New("video2dvd: MPEG-2 GOP exceeds the DVD VOBU duration limit")
	}
	vobu.EndAddress = uint32(m.sector) - vobu.StartSector - 1
	if vobu.EndAddress == 0 {
		return errors.New("video2dvd: VOBU media is empty")
	}
	if m.dst == nil || m.recordVOBU == nil {
		return nil
	}
	return m.recordVOBU(dvdVOBURecord{
		StartSector: vobu.StartSector,
		EndSector:   vobu.StartSector + vobu.EndAddress,
		StartPTM:    vobu.StartPTM,
		EndPTM:      vobu.EndPTM,
	})
}

// buildNAVPack builds the NAV pack of a planned VOBU.
func (m *dvdVOBMuxer) buildNAVPack(index int) ([]byte, error) {
	if index >= len(m.plan) || m.plan[index].StartSector != m.vobus[index].StartSector || m.plan[index].SCR != m.vobus[index].SCR {
		return nil, errDVDMuxInputChanged
	}
	vobu := m.plan[index]
	// A VOBU with fewer than three reference pictures repeats the end of its
	// last one, as dvdauthor does; players read up to these addresses during
	// trick play.
	for i := max(vobu.ReferenceCount, 1); i < len(vobu.ReferenceEnds); i++ {
		vobu.ReferenceEnds[i] = vobu.ReferenceEnds[i-1]
	}
	info := DVDNAVPackInfo{
		SCRBase:                   vobu.SCR / mpegSCRExtensionRange,
		SCRExtension:              uint16(vobu.SCR % mpegSCRExtensionRange),
		LogicalBlockNumber:        vobu.StartSector,
		VOBUEndAddress:            vobu.EndAddress,
		NextVOBU:                  DVDNAVSRIEndOfCell,
		PreviousVOBU:              DVDNAVSRIEndOfCell,
		FirstReferenceEndAddress:  vobu.ReferenceEnds[0],
		SecondReferenceEndAddress: vobu.ReferenceEnds[1],
		ThirdReferenceEndAddress:  vobu.ReferenceEnds[2],
		VOBID:                     m.vobID,
		CellID:                    vobu.CellID,
		StartPTM:                  vobu.StartPTM,
		EndPTM:                    vobu.EndPTM,
		SequenceEndPTM:            vobu.SequenceEndPTM,
		VOBStartPTM:               m.plan[0].StartPTM,
		VOBEndPTM:                 m.plan[len(m.plan)-1].EndPTM,
		CellElapsedTime:           vobu.CellElapsed,
		AudioSync:                 vobu.AudioSync,
	}
	if index < len(m.highlights) {
		highlight := m.highlights[index]
		info.ButtonDisplayType = highlight.groups[0].DisplayType
		info.Buttons = highlight.groups[0].Buttons
		info.ExtraButtonGroups = highlight.groups[1:]
		for stream := range highlight.subpictures {
			info.SubpictureSync[stream] = DVDNAVSubpictureSyncCurrent
		}
	}
	for stream := range m.subtitles {
		info.SubpictureSync[stream] = m.dvdSubpictureSync(stream, index)
	}
	// A title is a single cell. Menu VOBUs are separate cells, and search
	// pointers never cross a cell boundary.
	if len(m.highlights) == 0 {
		search := buildDVDVOBUSearchInfo(m.search, 0, len(m.search)-1, index)
		info.NextVOBU, info.PreviousVOBU = search.NextVOBU, search.PreviousVOBU
		info.ForwardVOBUs, info.BackwardVOBUs = search.ForwardVOBUs, search.BackwardVOBUs
	}
	return BuildDVDNAVPack(info)
}

// validDVDFrameRate reports whether a rate is one of the two sequence frame
// rates of DVD-Video.
func validDVDFrameRate(numerator, denominator int) bool {
	return denominator == 1 && numerator == 25 || denominator == 1001 && numerator == 30000
}

// checkDVDFrameRate explains why a title's frame rate cannot go on a DVD.
// Film is no exception: a 24000/1001 sequence (frame_rate_code 1) is not
// DVD-Video, and its pictures would be timed as if they were.
func checkDVDFrameRate(numerator, denominator int) error {
	if validDVDFrameRate(numerator, denominator) {
		return nil
	}
	if numerator == 24000 && denominator == 1001 {
		return errors.New("frame rate 24000/1001 is not a DVD-Video sequence rate: film is coded as a 30000/1001 stream whose pictures carry repeat_first_field (soft telecine), as NewMPEG2SoftTelecineEncoder writes it")
	}
	return fmt.Errorf("unsupported DVD frame rate %d/%d", numerator, denominator)
}

// dvdVideoFieldTicks returns the start of a display field in 90 kHz ticks,
// computed directly from the field number and rounded down, so that nothing
// accumulates: a frame of two fields is exactly 3003 ticks at 30000/1001 and
// 3600 at 25 frames per second, and a field 1501.5 or 1800.
func dvdVideoFieldTicks(field int64, numerator, denominator int) uint64 {
	return uint64(field) * 90_000 * uint64(denominator) / (2 * uint64(numerator))
}

func dvdCellElapsedTime(elapsed time.Duration, numerator, denominator int) ([4]byte, error) {
	if elapsed < 0 || !validDVDFrameRate(numerator, denominator) {
		return [4]byte{}, errors.New("video2dvd: invalid DVD cell elapsed time")
	}
	return dvdTimeCode(elapsed, numerator == 25 && denominator == 1)
}

// dvdTimeCode converts a duration to the BCD hh:mm:ss:ff form of IFO and NAV
// time fields. DVD time codes count frames: an NTSC "second" is 30 frames
// (1.001 s), the same unit the VTS time map uses.
func dvdTimeCode(duration time.Duration, pal bool) ([4]byte, error) {
	if duration < 0 {
		return [4]byte{}, errors.New("video2dvd: DVD time code cannot be negative")
	}
	rate, rateCode := int64(30), byte(3)
	if pal {
		rate, rateCode = 25, 1
	}
	// frames = round(duration * fps), computed without overflowing int64.
	numerator, denominator := int64(30000), int64(1001)
	if pal {
		numerator, denominator = 25, 1
	}
	seconds := int64(duration / time.Second)
	remainder := int64(duration % time.Second)
	whole := seconds * numerator
	scale := denominator * int64(time.Second)
	frames := whole/denominator + ((whole%denominator)*int64(time.Second)+remainder*numerator+scale/2)/scale
	totalSeconds := frames / rate
	if totalSeconds >= 100*60*60 {
		return [4]byte{}, errors.New("video2dvd: DVD time code exceeds 99:59:59")
	}
	return [4]byte{
		toPackedBCD(totalSeconds / 3600),
		toPackedBCD(totalSeconds / 60 % 60),
		toPackedBCD(totalSeconds % 60),
		rateCode<<6 | toPackedBCD(frames%rate),
	}, nil
}

func toPackedBCD(value int64) byte {
	return byte(value/10<<4 | value%10)
}

func mpeg2PictureType(picture []byte) (uint32, error) {
	start := findMPEGStartCode(picture, 0x00, 0)
	if start < 0 {
		return 0, errors.New("video2dvd: MPEG-2 picture start code not found")
	}
	reader := bitReader{data: picture[start+4:]}
	if _, err := reader.read(10); err != nil {
		return 0, err
	}
	return reader.read(3)
}

func mpeg2PictureHasSequenceHeader(picture []byte) bool {
	pictureStart := findMPEGStartCode(picture, 0x00, 0)
	sequenceStart := findMPEGStartCode(picture, 0xb3, 0)
	return sequenceStart >= 0 && pictureStart > sequenceStart
}

type mpeg2PictureReader struct {
	ctx                 context.Context
	src                 io.Reader
	buffer              []byte
	searchOffset        int
	currentPictureStart int
	foundPicture        bool
	finished            bool
}

func (r *mpeg2PictureReader) Next() ([]byte, error) {
	chunk := make([]byte, 32*1024)
	for {
		if r.ctx != nil {
			if err := r.ctx.Err(); err != nil {
				return nil, err
			}
		}
		if start := findMPEGStartCode(r.buffer, 0x00, r.searchOffset); start >= 0 {
			if r.foundPicture {
				boundary := start
				for _, code := range [...]byte{0xb3, 0xb8} {
					preamble := findMPEGStartCode(r.buffer, code, r.currentPictureStart+4)
					if preamble >= 0 && preamble < boundary {
						boundary = preamble
					}
				}
				picture := append([]byte(nil), r.buffer[:boundary]...)
				r.buffer = append(r.buffer[:0], r.buffer[boundary:]...)
				if boundary < start {
					r.foundPicture = false
					r.searchOffset = 0
					r.currentPictureStart = 0
				} else {
					r.searchOffset = start - boundary + 4
					r.currentPictureStart = 0
				}
				return picture, nil
			}
			r.foundPicture = true
			r.currentPictureStart = start
			r.searchOffset = start + 4
			continue
		}
		if r.finished {
			if r.foundPicture && len(r.buffer) != 0 {
				picture := append([]byte(nil), r.buffer...)
				r.buffer = nil
				return picture, nil
			}
			return nil, io.EOF
		}
		if len(r.buffer) > 3 {
			r.searchOffset = max(r.searchOffset, len(r.buffer)-3)
		}
		n, err := r.src.Read(chunk)
		if n > 0 {
			r.buffer = append(r.buffer, chunk[:n]...)
		}
		if err == io.EOF {
			r.finished = true
		} else if err != nil {
			return nil, err
		} else if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}
