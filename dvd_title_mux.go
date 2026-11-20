package video2dvd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"time"
)

// Timing model of a title VOB, in 90 kHz ticks unless noted otherwise. The
// system clock starts at zero and advances with the bytes written. The first
// picture is decoded dvdMuxStartDelay later and presented one frame period
// after that, which is also where the audio timeline starts.
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
// of the VOB and EndSector is the last sector of the VOBU. The presentation
// times are those stored in the VOBU's NAV pack.
type dvdVOBURecord struct {
	StartSector uint32
	EndSector   uint32
	StartPTM    uint32
	EndPTM      uint32
}

func muxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer, recordVOBU func(dvdVOBURecord) error, buttonPages [][]DVDNAVButton) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if dst == nil {
		return errors.New("video2dvd: VOB destination writer is required")
	}
	if title.VideoCodec != VideoCodecMPEG2 || title.AudioCodec != AudioCodecLPCM {
		return errors.New("video2dvd: VOB muxer requires MPEG-2 video and LPCM audio")
	}
	if !validDVDFrameRate(title.FrameRateNumerator, title.FrameRateDenominator) {
		return fmt.Errorf("video2dvd: unsupported DVD frame rate %d/%d", title.FrameRateNumerator, title.FrameRateDenominator)
	}
	videoPath, err := validateGeneratedFile(title.VideoPath, workDir, "video")
	if err != nil {
		return err
	}
	audioPath, err := validateGeneratedFile(title.AudioPath, workDir, "audio")
	if err != nil {
		return err
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
	audioFile, err := os.Open(audioPath)
	if err != nil {
		return err
	}
	defer audioFile.Close()

	// A NAV pack addresses VOBUs up to two minutes ahead and stores the end
	// time of the VOB, none of which is known while the stream is produced.
	// The schedule depends only on the inputs, so it is computed once without
	// output and then replayed with the complete VOBU table at hand.
	highlights, err := buildDVDMenuHighlights(buttonPages, title.Aspect, title.Width, title.Height)
	if err != nil {
		return err
	}
	planner := newDVDVOBMuxer(ctx, title, highlights, videoFile, audioFile)
	if err := planner.run(); err != nil {
		return err
	}
	if _, err := videoFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := audioFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	writer := newDVDVOBMuxer(ctx, title, highlights, videoFile, audioFile)
	writer.dst = dst
	writer.recordVOBU = recordVOBU
	writer.plan = planner.vobus
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
	AudioSync      uint16
}

type dvdMuxPicture struct {
	data       []byte
	offset     int // bytes already sent.
	frame      int64
	startsVOBU bool
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
	audio      io.Reader
	pictures   mpeg2PictureReader

	dst        io.Writer
	recordVOBU func(dvdVOBURecord) error
	plan       []dvdMuxVOBU
	search     []dvdVOBUSearchEntry

	clock  mpegSystemClock
	sector uint64 // sectors produced so far.
	vobus  []dvdMuxVOBU

	current    *dvdMuxPicture // next picture data to send.
	following  *dvdMuxPicture
	framesRead int64
	payload    []byte
	// videoBuffer models the decoder's video input buffer.
	videoBuffer   []dvdMuxBufferedPicture
	videoBuffered int
	vobuHasVideo  bool
	vobuHasAudio  bool

	audioChunk   []byte // next LPCM packet payload; nil once audio has ended.
	audioStorage [dvdLPCMPrivateHeaderLen + dvdLPCMPacketBytes]byte
	audioFrames  int64 // LPCM frames already sent.
	// recentAudio lists sent audio packs that may still be the A_SYNCA target
	// of a VOBU that has not started; syncPending is the first VOBU whose
	// target has not been sent yet.
	recentAudio []dvdMuxAudioPack
	syncPending int
}

func newDVDVOBMuxer(ctx context.Context, title EncodedTitle, highlights []dvdMenuHighlight, video, audio io.Reader) *dvdVOBMuxer {
	return &dvdVOBMuxer{
		ctx:        ctx,
		title:      title,
		highlights: highlights,
		audio:      audio,
		pictures:   mpeg2PictureReader{ctx: ctx, src: video},
		clock:      newMPEGSystemClock(defaultProgramMuxRate),
	}
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
	if err := m.readAudio(); err != nil {
		return err
	}
	// The VOB must open with a NAV pack, so the first picture goes first.
	if err := m.writeVideoPack(); err != nil {
		return err
	}
	for m.current != nil || m.audioChunk != nil {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		now := m.clock.now()
		m.releaseVideo(now)
		audioAt, videoAt := uint64(dvdMuxNever), uint64(dvdMuxNever)
		if m.audioChunk != nil {
			pts := m.audioPTS()
			audioAt = (pts - min(pts, dvdMuxAudioLead)) * mpegSCRExtensionRange
		}
		if m.current != nil {
			if videoAt, err = m.videoReadyAt(now); err != nil {
				return err
			}
		}
		// Audio goes first whenever it is due: its packs are small and
		// evenly spaced, so they interleave with a large picture instead
		// of waiting behind it.
		switch {
		case audioAt <= now:
			err = m.writeAudioPack()
		case videoAt <= now:
			err = m.writeVideoPack()
		default:
			m.clock.wait(min(audioAt, videoAt))
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

func (m *dvdVOBMuxer) frameTicks(frame int64) uint64 {
	return dvdVideoFrameTicks(frame, m.title.FrameRateNumerator, m.title.FrameRateDenominator)
}

// decodeTime and presentationTime give a frame's DTS and PTS. The stream has
// no B pictures, so every picture is presented one frame period after it is
// decoded.
func (m *dvdVOBMuxer) decodeTime(frame int64) uint64 {
	return dvdMuxStartDelay + m.frameTicks(frame)
}

func (m *dvdVOBMuxer) presentationTime(frame int64) uint64 {
	return dvdMuxStartDelay + m.frameTicks(frame+1)
}

// audioPTS is the presentation time of the next audio packet. An LPCM frame
// of 80 samples lasts exactly 150 ticks.
func (m *dvdVOBMuxer) audioPTS() uint64 {
	return m.presentationTime(0) + uint64(m.audioFrames)*dvdLPCMFrameTicks
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
	picture := &dvdMuxPicture{data: data, frame: m.framesRead, startsVOBU: pictureType == 1}
	m.framesRead++
	return picture, nil
}

// readAudio loads the next LPCM packet payload behind room for its private
// header, padding a final partial audio frame with silence.
func (m *dvdVOBMuxer) readAudio() error {
	data := m.audioStorage[dvdLPCMPrivateHeaderLen:]
	n, err := io.ReadFull(m.audio, data)
	if err == io.EOF {
		m.audioChunk = nil
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
	m.audioChunk = m.audioStorage[:dvdLPCMPrivateHeaderLen+padded]
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
	dts := m.decodeTime(m.current.frame)
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
	if m.clock.after(DVDVOBSectorSize) > m.decodeTime(picture.frame)*mpegSCRExtensionRange {
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
		header.pts = m.presentationTime(starting.frame)
		header.dts = m.decodeTime(starting.frame)
	}
	capacity := header.capacity()
	payload := m.payload[:0]
	for len(payload) < capacity {
		count := min(capacity-len(payload), len(picture.data)-picture.offset)
		payload = append(payload, picture.data[picture.offset:picture.offset+count]...)
		picture.offset += count
		m.bufferVideo(picture.frame, count)
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

func (m *dvdVOBMuxer) bufferVideo(frame int64, count int) {
	m.videoBuffered += count
	if last := len(m.videoBuffer) - 1; last >= 0 && m.videoBuffer[last].frame == frame {
		m.videoBuffer[last].bytes += count
		return
	}
	m.videoBuffer = append(m.videoBuffer, dvdMuxBufferedPicture{
		frame: frame,
		dts:   m.decodeTime(frame) * mpegSCRExtensionRange,
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
	vobu.EndPTM = uint32(m.presentationTime(picture.frame + 1))
	if findMPEGStartCode(picture.data, 0xb7, max(0, len(picture.data)-4)) >= 0 {
		vobu.SequenceEndPTM = vobu.EndPTM
	}
}

func (m *dvdVOBMuxer) writeAudioPack() error {
	pts := m.audioPTS()
	frames := (len(m.audioChunk) - dvdLPCMPrivateHeaderLen) / dvdLPCMFrameBytes
	end := pts + uint64(frames)*dvdLPCMFrameTicks
	if m.clock.after(DVDVOBSectorSize) > pts*mpegSCRExtensionRange {
		return errors.New("video2dvd: LPCM audio cannot reach the decoder before its presentation time")
	}
	header := mpegPESHeader{streamID: dvdPrivateStream1ID, hasPTS: true, pts: pts}
	if !m.vobuHasAudio {
		header.pstdBuffer = dvdAudioPSTDBuffer
		m.vobuHasAudio = true
	}
	putDVDLPCMPrivateHeader(m.audioChunk, dvdLPCMSubstreamID, frames, m.audioFrames)
	sector := uint32(m.sector)
	if err := m.writePack(func(scr uint64) ([]byte, error) {
		return buildMPEGPESPack(scr, defaultProgramMuxRate/50, header, m.audioChunk)
	}); err != nil {
		return err
	}
	m.audioFrames += int64(frames)

	for m.syncPending < len(m.vobus) {
		vobu := &m.vobus[m.syncPending]
		if vobu.AudioSync == DVDNAVAudioSyncNone {
			if uint64(vobu.StartPTM) >= end {
				break
			}
			vobu.AudioSync = dvdAudioSyncOffset(sector, vobu.StartSector)
		}
		m.syncPending++
	}
	// A later VOBU starts at most a few packs behind the audio, so a short
	// history is enough to find the pack presented at its start.
	const history = 1024
	if len(m.recentAudio) >= history {
		m.recentAudio = slices.Delete(m.recentAudio, 0, history/2)
	}
	m.recentAudio = append(m.recentAudio, dvdMuxAudioPack{pts: pts, end: end, sector: sector})
	return m.readAudio()
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
	start := m.presentationTime(picture.frame)
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
		AudioSync:     DVDNAVAudioSyncNone,
	}
	elapsed := start - m.presentationTime(0)
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
	for len(m.recentAudio) > 0 && m.recentAudio[0].end <= start {
		m.recentAudio = m.recentAudio[1:]
	}
	if len(m.recentAudio) > 0 && m.recentAudio[0].pts <= start {
		vobu.AudioSync = dvdAudioSyncOffset(m.recentAudio[0].sector, vobu.StartSector)
	}
	m.vobus = append(m.vobus, vobu)
	m.vobuHasVideo, m.vobuHasAudio = false, false
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
		VOBID:                     1,
		CellID:                    vobu.CellID,
		StartPTM:                  vobu.StartPTM,
		EndPTM:                    vobu.EndPTM,
		SequenceEndPTM:            vobu.SequenceEndPTM,
		VOBStartPTM:               m.plan[0].StartPTM,
		VOBEndPTM:                 m.plan[len(m.plan)-1].EndPTM,
		CellElapsedTime:           vobu.CellElapsed,
		AudioSync:                 [8]uint16{vobu.AudioSync},
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
	// A title is a single cell. Menu VOBUs are separate cells, and search
	// pointers never cross a cell boundary.
	if len(m.highlights) == 0 {
		search := buildDVDVOBUSearchInfo(m.search, 0, len(m.search)-1, index)
		info.NextVOBU, info.PreviousVOBU = search.NextVOBU, search.PreviousVOBU
		info.ForwardVOBUs, info.BackwardVOBUs = search.ForwardVOBUs, search.BackwardVOBUs
	}
	return BuildDVDNAVPack(info)
}

func validDVDFrameRate(numerator, denominator int) bool {
	return denominator == 1 && numerator == 25 || denominator == 1001 && (numerator == 24000 || numerator == 30000)
}

// dvdVideoFrameTicks returns the start of a frame in 90 kHz ticks, computed
// directly from the frame number so that frame periods stay exact (3003 ticks
// at 30000/1001 and 3600 at 25 frames per second).
func dvdVideoFrameTicks(frame int64, numerator, denominator int) uint64 {
	return uint64(frame) * 90_000 * uint64(denominator) / uint64(numerator)
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
