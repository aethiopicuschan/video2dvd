package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

const (
	// dvdMaxAudioStreams is the number of audio streams of a title.
	dvdMaxAudioStreams = 8
	// dvdLPCMBitrate is the rate of a 16-bit 48 kHz stereo LPCM stream.
	dvdLPCMBitrate = 1_536_000
	// dvdMuxMediaBitrate is what video and audio together may take of the
	// 10.08 Mbit/s multiplex, leaving the rest to pack and PES headers, NAV
	// packs and subtitles: the share of the 7.5 Mbit/s video and the one
	// LPCM stream that a title has always had.
	dvdMuxMediaBitrate = mpeg2MuxSafeVideoBitrate + dvdLPCMBitrate

	// ac3SyncFrameSamples is the number of samples of an AC-3 syncframe, and
	// ac3SyncFrameTicks its duration at 48 kHz on the 90 kHz clock.
	ac3SyncFrameSamples = 1536
	ac3SyncFrameTicks   = ac3SyncFrameSamples * 90_000 / 48_000
	// dvdAC3PrivateHeaderLen is the private_stream_1 header of an AC-3
	// packet: the substream number, the number of syncframes that start in
	// the packet and the pointer to the first of them.
	dvdAC3PrivateHeaderLen = 4
	dvdAC3SubstreamID      = 0x80
	// dvdAC3BufferBytes is the decoder's input buffer for an AC-3 stream,
	// the size ffmpeg's DVD multiplexer assumes, and dvdAC3PSTDBuffer the
	// P-STD_buffer field that states it in 128-byte units.
	dvdAC3BufferBytes = 4 * 1024
	dvdAC3PSTDBuffer  = 0x4000 | dvdAC3BufferBytes/128
	// dvdAC3PresentationLead is the encoder delay of AC-3 streams, 256
	// samples, on the 90 kHz clock: the built-in encoder and common others
	// such as ffmpeg's decode input sample n as output sample n+256.
	dvdAC3PresentationLead = ac3EncoderDelay * 90_000 / 48_000
)

// titleAudioCodec returns the codec of every audio stream of a title with
// the given number of streams.
func titleAudioCodec(format AudioFormat, streams int) string {
	switch {
	case format == AudioFormatLPCM:
		return AudioCodecLPCM
	case format == AudioFormatAC3 || streams > 1:
		return AudioCodecAC3
	}
	return AudioCodecLPCM
}

// dvdAudioStreamBitrate returns the rate an audio stream takes of the
// multiplex.
func dvdAudioStreamBitrate(codec string, ac3Bitrate int) int {
	if codec == AudioCodecAC3 {
		return ac3Bitrate
	}
	return dvdLPCMBitrate
}

// titleVideoBitrate returns the video bitrate a title is coded at: the
// profile's, lowered when its audio streams leave less of dvdMuxMediaBitrate.
// One LPCM stream leaves exactly the 7.5 Mbit/s video always had.
func titleVideoBitrate(profile Profile, codecs []string) (int, error) {
	ac3Bitrate := profile.AC3Bitrate
	if ac3Bitrate == 0 {
		ac3Bitrate = DefaultAC3Bitrate
	}
	room := dvdMuxMediaBitrate
	for _, codec := range codecs {
		room -= dvdAudioStreamBitrate(codec, ac3Bitrate)
	}
	video := min(effectiveMPEG2VideoBitrate(profile), room)
	if video < mpeg2MinVideoBitrate {
		return 0, fmt.Errorf("video2dvd: %d %s audio streams leave %d bit/s to the video, below the minimum of %d bit/s; use AC-3 or fewer streams", len(codecs), codecs[0], max(room, 0), mpeg2MinVideoBitrate)
	}
	return video, nil
}

// selectInputAudioTracks returns the audio tracks to record for an input:
// the requested ones, or with requested nil and includeAll set every
// supported audio track, the default-marked one first, up to the streams of
// a title. It returns nil when neither applies, which records one track as
// before.
func selectInputAudioTracks(info MP4Info, requested []int, includeAll bool) ([]int, error) {
	supported := func(track TrackInfo) bool {
		return track.Kind == TrackKindAudio && supportedInputAudioCodec(track.Codec)
	}
	if requested == nil {
		if !includeAll {
			return nil, nil
		}
		// Tracks the decoders cannot turn into stereo, such as multichannel
		// AAC, are skipped like tracks of other codecs; if none is left the
		// single-track path reports why.
		decodable := func(track TrackInfo) bool {
			return supported(track) && validateInputAudioChannels(track) == nil
		}
		first, found := selectDefaultTrack(info, decodable)
		if !found {
			return nil, nil
		}
		all := []int{first.Index}
		for _, track := range info.Tracks {
			if decodable(track) && track.Index != first.Index && len(all) < dvdMaxAudioStreams {
				all = append(all, track.Index)
			}
		}
		return all, nil
	}
	if len(requested) == 0 {
		return nil, errors.New("audio track list is empty")
	}
	if len(requested) > dvdMaxAudioStreams {
		return nil, fmt.Errorf("a title has at most %d audio streams, %d were requested", dvdMaxAudioStreams, len(requested))
	}
	for i, index := range requested {
		if slices.Contains(requested[:i], index) {
			return nil, fmt.Errorf("audio track %d is requested twice", index)
		}
		if index < 0 || index >= len(info.Tracks) {
			return nil, fmt.Errorf("audio track index %d is out of range", index)
		}
		track := info.Tracks[index]
		if !supported(track) {
			return nil, fmt.Errorf("track %d is not a supported audio track (%s %q)", index, track.Kind, track.Codec)
		}
		if err := validateInputAudioChannels(track); err != nil {
			return nil, err
		}
	}
	return slices.Clone(requested), nil
}

// ac3FrameInfo is what the header of an AC-3 syncframe tells.
type ac3FrameInfo struct {
	size     int // bytes.
	channels int // full-bandwidth channels plus LFE.
}

// ac3FrameSizes48k is the syncframe size in 16-bit words at 48 kHz per
// frmsizecod/2.
var ac3FrameSizes48k = [19]int{64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 640, 768, 896, 1024, 1152, 1280}

// parseAC3FrameHeader reads the syncinfo and the start of the bit stream
// information of an AC-3 syncframe at 48 kHz.
func parseAC3FrameHeader(header []byte) (ac3FrameInfo, error) {
	if len(header) < 8 || header[0] != 0x0b || header[1] != 0x77 {
		return ac3FrameInfo{}, errors.New("video2dvd: AC-3 stream is not at a syncframe")
	}
	fscod, frmsizecod := header[4]>>6, int(header[4]&0x3f)
	if fscod != 0 {
		return ac3FrameInfo{}, errors.New("video2dvd: AC-3 stream is not at 48 kHz")
	}
	if frmsizecod/2 >= len(ac3FrameSizes48k) {
		return ac3FrameInfo{}, errors.New("video2dvd: AC-3 syncframe has an invalid frame size code")
	}
	// The table's 16th rate class is 448 kbit/s, the most DVD-Video allows.
	if frmsizecod/2 > 15 {
		return ac3FrameInfo{}, fmt.Errorf("video2dvd: AC-3 bitrate of %d kbit/s exceeds the 448 kbit/s DVD-Video allows", ac3FrameSizes48k[frmsizecod/2]*2*8*48/1536)
	}
	bsid := header[5] >> 3
	if bsid > 10 {
		return ac3FrameInfo{}, fmt.Errorf("video2dvd: AC-3 bit stream id %d is not AC-3 (E-AC-3 is not DVD-Video audio)", bsid)
	}
	if bsid > 8 {
		return ac3FrameInfo{}, fmt.Errorf("video2dvd: AC-3 bit stream id %d is a reduced sample rate stream, not 48 kHz", bsid)
	}
	// acmod is the top three bits of the byte after bsid and bsmod; lfeon
	// follows the mix levels and the surround mode that some modes carry.
	acmod := int(header[6] >> 5)
	bits := uint16(header[6])<<8 | uint16(header[7])
	position := 3
	if acmod&1 != 0 && acmod != 1 {
		position += 2 // cmixlev.
	}
	if acmod&4 != 0 {
		position += 2 // surmixlev.
	}
	if acmod == 2 {
		position += 2 // dsurmod.
	}
	lfe := int(bits>>(15-position)) & 1
	return ac3FrameInfo{
		size:     ac3FrameSizes48k[frmsizecod/2] * 2,
		channels: [8]int{2, 1, 2, 3, 3, 4, 4, 5}[acmod] + lfe,
	}, nil
}

// ac3StreamReader reads whole syncframes of an AC-3 elementary stream.
type ac3StreamReader struct {
	r      io.Reader
	frames int64
}

// next returns the next syncframe, or io.EOF after the last.
func (s *ac3StreamReader) next() ([]byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(s.r, header); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("video2dvd: AC-3 stream ends inside syncframe %d", s.frames)
	}
	info, err := parseAC3FrameHeader(header)
	if err != nil {
		return nil, fmt.Errorf("%w (syncframe %d)", err, s.frames)
	}
	frame := make([]byte, info.size)
	copy(frame, header)
	if _, err := io.ReadFull(s.r, frame[len(header):]); err != nil {
		return nil, fmt.Errorf("video2dvd: AC-3 stream ends inside syncframe %d", s.frames)
	}
	s.frames++
	return frame, nil
}

// probeAC3Stream returns the channels of the first syncframe of an AC-3
// stream and the number of syncframes, checking that all are 48 kHz.
func probeAC3Stream(r io.Reader) (channels int, frames int64, err error) {
	reader := &ac3StreamReader{r: r}
	for {
		frame, err := reader.next()
		if err == io.EOF {
			if reader.frames == 0 {
				return 0, 0, errors.New("video2dvd: AC-3 stream has no syncframes")
			}
			return channels, reader.frames, nil
		}
		if err != nil {
			return 0, 0, err
		}
		if reader.frames == 1 {
			info, _ := parseAC3FrameHeader(frame)
			channels = info.channels
		}
	}
}

// dvdAudioAttribute describes one audio stream of a title for its IFO.
type dvdAudioAttribute struct {
	codec    string
	channels int
	language string
	// number is the stream's number within its codec, from which its
	// substream ID follows.
	number byte
}

// dvdTitleAudioAttributes describes the audio streams of a title, reading
// the channel count of AC-3 streams from their first syncframe.
func dvdTitleAudioAttributes(title EncodedTitle, workDir string) ([]dvdAudioAttribute, error) {
	streams := titleAudioStreams(title)
	if len(streams) == 0 || len(streams) > dvdMaxAudioStreams {
		return nil, fmt.Errorf("a title has 1 to %d audio streams, not %d", dvdMaxAudioStreams, len(streams))
	}
	attributes := make([]dvdAudioAttribute, len(streams))
	var lpcm, ac3 byte
	for i, stream := range streams {
		if !validDVDLanguage(stream.Language) {
			return nil, fmt.Errorf("audio stream %d language %q is not an ISO 639-1 code", i+1, stream.Language)
		}
		attribute := dvdAudioAttribute{codec: stream.Codec, channels: 2, language: stream.Language}
		switch stream.Codec {
		case AudioCodecLPCM:
			attribute.number = lpcm
			lpcm++
		case AudioCodecAC3:
			path, err := validateGeneratedFile(stream.Path, workDir, "audio")
			if err != nil {
				return nil, err
			}
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			channels, _, err := probeAC3Stream(f)
			_ = f.Close()
			if err != nil {
				return nil, fmt.Errorf("audio stream %d: %w", i+1, err)
			}
			attribute.channels = channels
			attribute.number = ac3
			ac3++
		default:
			return nil, fmt.Errorf("built-in authorer requires LPCM or AC-3 audio, stream %d is %q", i+1, stream.Codec)
		}
		attributes[i] = attribute
	}
	return attributes, nil
}

// putDVDAudioAttributes writes the number of audio streams of a title and
// their 8-byte attributes, as a VTSI stores them from 0x202 on: the coding
// mode (AC-3 0, LPCM 4) and whether a language code is set, 48 kHz and the
// channel count, and the language code. A single LPCM stream without a
// language gives the bytes every title had before titles could have
// several streams.
func putDVDAudioAttributes(dst []byte, audio []dvdAudioAttribute) {
	binary.BigEndian.PutUint16(dst[0:2], uint16(len(audio)))
	for i, stream := range audio {
		attribute := dst[2+i*8 : 10+i*8]
		if stream.codec == AudioCodecLPCM {
			attribute[0] = dvdLPCMAudioAttribute0
		}
		if stream.language != "" {
			attribute[0] |= 0x04 // the language code is set.
			copy(attribute[2:4], stream.language)
		}
		attribute[1] = byte(min(max(stream.channels, 1), 8) - 1) // 48 kHz.
	}
}

// TitleEstimate describes a title for EstimateDVDBytes.
type TitleEstimate struct {
	// Duration is the length of the title's video.
	Duration time.Duration
	// AudioStreams is the number of audio streams; zero counts as one, the
	// stream of silence a title without audio gets.
	AudioStreams int
}

const (
	// dvdEstimateOverheadDivisor: pack and PES headers, NAV packs and
	// partly filled packs add about 1/32 to the streams once they are
	// multiplexed, as checkEstimatedTitleSize assumes.
	dvdEstimateOverheadDivisor = 32
	// dvdEstimateFixedBytes covers the title menu, the IFO and BUP files
	// and the ISO 9660 and UDF structures of a disc with few titles, and
	// dvdEstimateTitleBytes what each title adds to them.
	dvdEstimateFixedBytes = 2 << 20
	dvdEstimateTitleBytes = 256 << 10
)

// EstimateDVDBytes estimates the size in bytes of the ISO image that
// ConvertSourcesISO writes for titles with options, before anything is
// encoded, so that an application can tell whether its inputs fit a disc.
// It assumes the rates the built-in encoder codes at: the video at its
// bitrate (Options.VideoBitrate, at most 7.5 Mbit/s, lowered for several
// audio streams as Options.AudioFormat describes), every audio stream at
// 1.536 Mbit/s for LPCM or Options.AC3Bitrate for AC-3, about 3% for the
// multiplex, and a few megabytes for menus and the file system. The video
// encoder never exceeds its bitrate but spends less on simple pictures, so
// this is an upper estimate: close for detailed video, far above the result
// for a still or mostly black picture. Subtitles, which take little room,
// are not included. It returns an error for invalid options or durations and for
// audio streams that leave the video too little room.
func EstimateDVDBytes(titles []TitleEstimate, options Options) (int64, error) {
	profile, err := options.profile()
	if err != nil {
		return 0, err
	}
	total := int64(dvdEstimateFixedBytes)
	for i, title := range titles {
		if title.Duration <= 0 {
			return 0, fmt.Errorf("video2dvd: title %d has no duration", i+1)
		}
		streams := max(title.AudioStreams, 1)
		if streams > dvdMaxAudioStreams {
			return 0, fmt.Errorf("video2dvd: title %d has more than %d audio streams", i+1, dvdMaxAudioStreams)
		}
		codecs := slices.Repeat([]string{titleAudioCodec(profile.AudioFormat, streams)}, streams)
		video, err := titleVideoBitrate(profile, codecs)
		if err != nil {
			return 0, fmt.Errorf("video2dvd: title %d: %w", i+1, err)
		}
		rate := int64(video)
		for _, codec := range codecs {
			rate += int64(dvdAudioStreamBitrate(codec, profile.AC3Bitrate))
		}
		bytes, err := estimateRateBytes(title.Duration, rate/8)
		if err != nil {
			return 0, err
		}
		total += bytes + bytes/dvdEstimateOverheadDivisor + dvdEstimateTitleBytes
	}
	return total, nil
}
