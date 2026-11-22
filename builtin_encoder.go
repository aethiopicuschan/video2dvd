package video2dvd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// InProcessEncoder is a built-in Encoder that uses only Go code. Its MPEG-2
// video coder emits closed GOPs of I and P pictures with half-sample motion
// compensation and VBV rate control, without B pictures, so it needs more
// bits than a dedicated encoder for the same quality. Pictures are fitted to
// the DVD raster with FitVideoFrameForDVD, so BT.709 and full-range sources
// are converted to the BT.601 studio-range encoding players assume. It
// converts MP4 AAC-LC/AC-3 and WebM Opus audio to LPCM, padded or trimmed to
// the encoded video length, and creates silent LPCM when the input has no
// audio track.
//
// An NTSC title whose video track runs at 24000/1001 or 24 frames per second
// throughout is coded as film with 3:2 pulldown flags (soft telecine, see
// NewMPEG2SoftTelecineEncoder) unless Profile.DisableSoftTelecine is set.
//
// The text subtitle tracks that MediaInput.SubtitleTracks selects are read
// before any video is encoded and written as subtitle streams with
// WriteDVDSubtitleStream, on the title timeline: a cue keeps its distance
// from the first video frame of its file.
type InProcessEncoder struct{}

// NewInProcessEncoder returns the built-in Encoder implementation.
func NewInProcessEncoder() *InProcessEncoder { return &InProcessEncoder{} }

// Encode turns one MP4 or WebM into DVD-compatible elementary streams in workDir.
func (e *InProcessEncoder) Encode(ctx context.Context, inputPath, workDir string, profile Profile) (EncodedTitle, error) {
	return e.EncodeStreams(ctx, MediaInput{VideoPath: inputPath, AudioPath: inputPath}, workDir, profile)
}

// EncodeStreams encodes one DVD title from independent video/audio files or
// selected tracks in a shared file.
func (e *InProcessEncoder) EncodeStreams(ctx context.Context, input MediaInput, workDir string, profile Profile) (EncodedTitle, error) {
	if e == nil {
		return EncodedTitle{}, errors.New("video2dvd: in-process encoder is nil")
	}
	if ctx == nil {
		return EncodedTitle{}, errors.New("video2dvd: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return EncodedTitle{}, err
	}
	if input.VideoPath == "" {
		return EncodedTitle{}, errors.New("video2dvd: video input path is required")
	}
	if input.AudioPath == "" {
		input.AudioPath = input.VideoPath
	}
	info, err := ProbeMedia(input.VideoPath)
	if err != nil {
		return EncodedTitle{}, err
	}
	videoTrack, err := selectInputVideoTrack(info, input.VideoTrackIndex)
	if err != nil {
		return EncodedTitle{}, err
	}
	if videoTrack.Width <= 0 || videoTrack.Height <= 0 {
		return EncodedTitle{}, errors.New("video2dvd: video track has invalid dimensions")
	}
	if err := validateEncoderProfile(profile); err != nil {
		return EncodedTitle{}, err
	}
	if rate := profileVideoBitrate(profile); rate < mpeg2MinVideoBitrate || rate > mpeg2MaxVideoBitrate {
		return EncodedTitle{}, fmt.Errorf("video2dvd: video bitrate must be between %d and %d bit/s", mpeg2MinVideoBitrate, mpeg2MaxVideoBitrate)
	}
	// Subtitles are read and drawn before any video is encoded, so that a
	// track that cannot be read fails the title early and their size counts
	// towards the estimate below.
	subtitleTracks, err := selectInputSubtitleTracks(info, input.SubtitleTracks, false)
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: prepare DVD subtitles: %w", err)
	}
	subtitles, subtitleSectors, err := readInputSubtitles(input.VideoPath, info, subtitleTracks, profile)
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: prepare DVD subtitles: %w", err)
	}
	audioInfo := info
	if input.AudioPath != input.VideoPath {
		audioInfo, err = ProbeMedia(input.AudioPath)
		if err != nil {
			return EncodedTitle{}, fmt.Errorf("video2dvd: inspect audio source: %w", err)
		}
	}
	// audioTracks lists the tracks of the title's audio streams; a nil
	// entry is a stream of silence, for an input without audio.
	var audioTracks []*TrackInfo
	if input.AudioTracks != nil {
		if input.AudioTrackIndex != nil {
			return EncodedTitle{}, errors.New("video2dvd: AudioTrackIndex and AudioTracks cannot both be set")
		}
		indexes, err := selectInputAudioTracks(audioInfo, input.AudioTracks, false)
		if err != nil {
			return EncodedTitle{}, fmt.Errorf("video2dvd: prepare DVD audio: %w", err)
		}
		for _, index := range indexes {
			audioTracks = append(audioTracks, &audioInfo.Tracks[index])
		}
	} else {
		audioTrack, hasAudio := firstSupportedAudioTrack(audioInfo, input.AudioTrackIndex)
		if !hasAudio && hasAnyAudioTrack(audioInfo) {
			return EncodedTitle{}, errors.New("video2dvd: prepare DVD audio: input has audio tracks but none are supported by the built-in decoder (MP4 AAC-LC/AC-3, WebM Opus)")
		}
		if hasAudio {
			if err := validateInputAudioChannels(audioTrack); err != nil {
				return EncodedTitle{}, err
			}
			audioTracks = []*TrackInfo{&audioTrack}
		} else {
			audioTracks = []*TrackInfo{nil}
		}
	}
	audioCodec := titleAudioCodec(profile.AudioFormat, len(audioTracks))
	audioCodecs := slices.Repeat([]string{audioCodec}, len(audioTracks))
	ac3Bitrate := profile.AC3Bitrate
	if ac3Bitrate == 0 {
		ac3Bitrate = DefaultAC3Bitrate
	}
	if !slices.Contains(ac3Bitrates, ac3Bitrate) {
		return EncodedTitle{}, fmt.Errorf("%w: AC-3 bitrate %d is not one of %v", ErrInvalidProfile, ac3Bitrate, ac3Bitrates)
	}
	// Audio streams beyond one LPCM stream may leave the video less room.
	videoBitrate, err := titleVideoBitrate(profile, audioCodecs)
	if err != nil {
		return EncodedTitle{}, err
	}
	videoProfile := profile
	if videoBitrate < effectiveMPEG2VideoBitrate(profile) {
		videoProfile.VideoBitrate = videoBitrate
	}
	// The container duration only feeds the up-front capacity estimate; live
	// WebM has none, and the title length comes from the frames encoded below.
	estimatedDuration := info.Duration
	if estimatedDuration <= 0 {
		estimatedDuration = videoTrack.Duration
	}
	if estimatedDuration > 0 {
		var audioBytesPerSecond int64
		for _, codec := range audioCodecs {
			audioBytesPerSecond += int64(dvdAudioStreamBitrate(codec, ac3Bitrate) / 8)
		}
		if err := checkEstimatedTitleSize(estimatedDuration, encoderOutputLimit(profile)-subtitleSectors*DVDVOBSectorSize, videoBitrate, audioBytesPerSecond); err != nil {
			return EncodedTitle{}, err
		}
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: create encoder work directory: %w", err)
	}
	videoPath := filepath.Join(workDir, "video.m2v")
	videoCreated := false
	var audioPaths, subtitlePaths []string
	cleanup := true
	defer func() {
		if cleanup {
			if videoCreated {
				_ = os.Remove(videoPath)
			}
			for _, path := range slices.Concat(audioPaths, subtitlePaths) {
				_ = os.Remove(path)
			}
		}
	}()

	videoFile, err := os.OpenFile(videoPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: create MPEG-2 output: %w", err)
	}
	videoCreated = true
	sourceAspectN, sourceAspectD, err := trackDisplayAspect(videoTrack)
	if err != nil {
		_ = videoFile.Close()
		return EncodedTitle{}, err
	}
	webMVideo := videoTrack.Codec == "vp9" || videoTrack.Codec == "vp8" || videoTrack.Codec == "av1"
	// Whether the title is film is settled before the first frame is coded,
	// from the timestamps of the whole video track. If they cannot be read
	// the title is not film, and decoding reports what is wrong.
	var frameTimes []time.Duration
	if profile.Standard == VideoStandardNTSC && !profile.DisableSoftTelecine {
		if webMVideo {
			frameTimes, _ = webMVideoFrameTimes(input.VideoPath, &videoTrack.Index)
		} else {
			frameTimes, _ = mp4VideoFrameTimes(input.VideoPath, &videoTrack.Index)
		}
	}
	var videoEncoder *MPEG2VideoEncoder
	rateConverter, film, err := newDVDTitleFrameRateConverter(profile, frameTimes, func(frame DecodedVideoFrame) error {
		return videoEncoder.WriteFrame(frame)
	})
	if err != nil {
		_ = videoFile.Close()
		return EncodedTitle{}, err
	}
	videoWriter := &sizeLimitedWriter{dst: videoFile, limit: encoderOutputLimit(profile)}
	if film {
		videoEncoder, err = NewMPEG2SoftTelecineEncoder(videoWriter, videoProfile)
	} else {
		videoEncoder, err = NewMPEG2VideoEncoder(videoWriter, videoProfile)
	}
	if err != nil {
		_ = videoFile.Close()
		return EncodedTitle{}, err
	}
	var prepareErr error
	// audioOffset places the audio on the title timeline, whose zero is the
	// first encoded video frame. subtitleOffset does the same for the
	// subtitles, which share the timeline of the video file.
	audioOffset := input.AudioOffset
	var subtitleOffset time.Duration
	if webMVideo {
		var videoStart time.Duration
		videoStart, prepareErr = prepareWebMVideoForDVD(ctx, input.VideoPath, &videoTrack.Index, profile, sourceAspectN, sourceAspectD, FitLetterbox, rateConverter)
		if input.AudioPath == input.VideoPath {
			// Both tracks share the WebM timeline, so audio moves with the
			// video's start instead of staying on container time.
			shifted, ok := addMediaOffsets(audioOffset, -videoStart)
			if prepareErr == nil && !ok {
				prepareErr = errors.New("video2dvd: audio offset overflows")
			}
			audioOffset = shifted
		}
		subtitleOffset = -videoStart
	} else {
		prepareErr = prepareMP4VideoForDVD(ctx, input.VideoPath, &videoTrack.Index, profile, sourceAspectN, sourceAspectD, FitLetterbox, rateConverter)
		// The subtitle cues are on the movie timeline, to which an edit
		// list maps the video's media time; title time zero is the media
		// time of the converter's origin.
		subtitleOffset = -rateConverter.origin
		if videoTrack.edit.present {
			subtitleOffset += videoTrack.edit.mediaTime - videoTrack.edit.empty
		}
	}
	closeEncoderErr := videoEncoder.Close()
	closeVideoErr := videoFile.Close()
	if prepareErr != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: prepare DVD video: %w", prepareErr)
	}
	if closeEncoderErr != nil {
		return EncodedTitle{}, closeEncoderErr
	}
	if closeVideoErr != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: close MPEG-2 output: %w", closeVideoErr)
	}

	// The title is exactly as long as the encoded video is displayed, which
	// for film is not a whole number of frames; audio is padded or trimmed
	// to the same length in whole samples. The frame rate stays the display
	// rate of the standard: the pictures themselves say how long each lasts.
	fpsN, fpsD := profileFrameRate(profile.Standard)
	duration, audioSamples, err := titleLengthFromFields(videoEncoder.DisplayFields(), fpsN, fpsD)
	if err != nil {
		return EncodedTitle{}, err
	}

	// Every stream is placed on the title timeline like the single stream
	// of a title always was, and cut or padded to the title's length.
	encodedAudio := make([]EncodedAudio, 0, len(audioTracks))
	for i, track := range audioTracks {
		name := "audio.lpcm"
		if audioCodec == AudioCodecAC3 {
			name = fmt.Sprintf("audio-%02d.ac3", i+1)
		} else if i > 0 {
			name = fmt.Sprintf("audio-%02d.lpcm", i+1)
		}
		path := filepath.Join(workDir, name)
		if err := encodeTitleAudioStream(ctx, path, input.AudioPath, track, audioCodec, ac3Bitrate, audioSamples, audioOffset, encoderOutputLimit(profile)); err != nil {
			if !errors.Is(err, os.ErrExist) {
				audioPaths = append(audioPaths, path)
			}
			return EncodedTitle{}, err
		}
		audioPaths = append(audioPaths, path)
		stream := EncodedAudio{Path: name, Codec: audioCodec}
		if track != nil && input.AudioTracks != nil {
			stream.Language = DVDLanguageCode(track.Language)
		}
		encodedAudio = append(encodedAudio, stream)
	}
	if err := ctx.Err(); err != nil {
		return EncodedTitle{}, err
	}
	encodedSubtitles := make([]EncodedSubtitle, 0, len(subtitles))
	for i, subtitle := range subtitles {
		name := fmt.Sprintf("subtitle-%02d.sub", i+1)
		path := filepath.Join(workDir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return EncodedTitle{}, fmt.Errorf("video2dvd: create subtitle stream: %w", err)
		}
		subtitlePaths = append(subtitlePaths, path)
		err = writeDVDSubtitleStream(file, shiftDVDSubtitleEvents(subtitle.events, subtitleOffset), duration, subtitle.renderer)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return EncodedTitle{}, fmt.Errorf("video2dvd: write subtitle stream %d: %w", i+1, err)
		}
		encodedSubtitles = append(encodedSubtitles, EncodedSubtitle{Path: name, Language: subtitle.language})
	}
	if len(encodedSubtitles) == 0 {
		encodedSubtitles = nil
	}
	cleanup = false
	return EncodedTitle{
		VideoPath:            "video.m2v",
		AudioPath:            encodedAudio[0].Path,
		VideoCodec:           VideoCodecMPEG2,
		AudioCodec:           audioCodec,
		Aspect:               profile.Aspect,
		Width:                profile.FrameWidth,
		Height:               profile.FrameHeight,
		FrameRateNumerator:   fpsN,
		FrameRateDenominator: fpsD,
		Duration:             duration,
		Subtitles:            encodedSubtitles,
		Audio:                titleAudioList(encodedAudio, input.AudioTracks != nil),
	}, nil
}

// titleAudioList returns the Audio list of an encoded title: nil for the
// single stream of a title whose tracks were not chosen with
// MediaInput.AudioTracks or Options.IncludeAllAudio, which keeps such a
// title as it always was.
func titleAudioList(streams []EncodedAudio, chosen bool) []EncodedAudio {
	if !chosen && len(streams) == 1 {
		return nil
	}
	return streams
}

// encodeTitleAudioStream decodes one audio track, or makes silence for a nil
// track, places it on the title timeline (offset, length in samples) and
// writes it to path as LPCM or AC-3.
func encodeTitleAudioStream(ctx context.Context, path, source string, track *TrackInfo, codec string, ac3Bitrate int, samples int64, offset time.Duration, limit int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("video2dvd: create audio output: %w", err)
	}
	limited := &sizeLimitedWriter{dst: file, limit: limit}
	var pcm io.Writer = limited
	var ac3 *ac3Writer
	if codec == AudioCodecAC3 {
		if ac3, err = newAC3Writer(limited, ac3Bitrate); err != nil {
			_ = file.Close()
			return err
		}
		pcm = ac3
	}
	timeline, err := newPCMWindowWriterSamples(pcm, samples, offset)
	if err != nil {
		_ = file.Close()
		return err
	}
	if track != nil {
		if track.Codec == "opus" {
			err = DecodeWebMAudio(ctx, source, &track.Index, timeline.WriteChunk)
		} else {
			err = DecodeMP4Audio(ctx, source, &track.Index, timeline.WriteChunk)
		}
	}
	if err == nil {
		err = timeline.Finish()
	}
	if err == nil && ac3 != nil {
		err = ac3.Close()
	}
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("video2dvd: prepare DVD audio: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("video2dvd: close audio output: %w", closeErr)
	}
	return nil
}

// inputSubtitle is a subtitle track read for a title: its events on the
// timeline of the source file and its DVD language code.
type inputSubtitle struct {
	events   []dvdSubtitleEvent
	language string
	renderer *dvdSubtitleRenderer
}

// readInputSubtitles reads the subtitle tracks of a title and draws every
// text once. It returns the tracks and the sectors that their units will
// take at most, when every subtitle is shown within the title.
func readInputSubtitles(path string, info MP4Info, tracks []int, profile Profile) ([]inputSubtitle, int64, error) {
	if len(tracks) == 0 {
		return nil, 0, nil
	}
	renderer := newDVDSubtitleRenderer(profile)
	subtitles := make([]inputSubtitle, 0, len(tracks))
	var sectors int64
	for _, index := range tracks {
		cues, err := ReadSubtitleCues(path, index)
		if err != nil {
			return nil, 0, err
		}
		events := flattenSubtitleCues(cues)
		for _, event := range events {
			picture, err := renderer.image(event.text)
			if err != nil {
				return nil, 0, err
			}
			if picture == nil {
				continue
			}
			// The largest delay a unit holds; a longer event is split.
			units := int64((dvdSubtitleTicks(event.end-event.start) + dvdSubtitleMaxDelay*dvdSubtitleDelayTicks - 1) / (dvdSubtitleMaxDelay * dvdSubtitleDelayTicks))
			unit, err := encodeDVDSubtitleUnit(picture, 1)
			if err != nil {
				return nil, 0, err
			}
			sectors += max(units, 1) * dvdSubtitleUnitSectors(len(unit))
		}
		subtitles = append(subtitles, inputSubtitle{events: events, language: DVDLanguageCode(info.Tracks[index].Language), renderer: renderer})
	}
	return subtitles, sectors, nil
}

// shiftDVDSubtitleEvents moves events by offset.
func shiftDVDSubtitleEvents(events []dvdSubtitleEvent, offset time.Duration) []dvdSubtitleEvent {
	shifted := make([]dvdSubtitleEvent, len(events))
	for i, event := range events {
		shifted[i] = dvdSubtitleEvent{start: event.start + offset, end: event.end + offset, text: event.text}
	}
	return shifted
}

// titleLengthFromFields returns the display time of a number of fields at
// the frame rate fpsN/fpsD, two fields to the frame, and the number of audio
// samples nearest to it.
func titleLengthFromFields(fields int64, fpsN, fpsD int) (time.Duration, int64, error) {
	if fields <= 0 {
		return 0, 0, errors.New("video2dvd: input produced no video frames")
	}
	if fields > 2*24*60*60*60 {
		return 0, 0, errors.New("video2dvd: title is too long")
	}
	scaled, rate := fields*int64(fpsD), 2*int64(fpsN)
	duration := time.Duration(scaled/rate)*time.Second + time.Duration(scaled%rate)*time.Second/time.Duration(rate)
	samples := (scaled*dvdLPCMSampleRate + rate/2) / rate
	return duration, samples, nil
}

// validateInputAudioChannels rejects layouts the built-in decoders cannot
// decode to stereo before any video is encoded. AC-3 is decoded and
// downmixed by go-264 for every acmod, with or without LFE, and so is AAC-LC
// with channel configurations 3 to 7 (from 3.0 to 7.1). AAC whose channels
// are described by a program config element (channel configuration 0, which
// ffmpeg writes for layouts such as 5.1(side)) and multichannel HE-AAC are
// refused, as go-264 cannot decode them. Opus with more than two channels is
// downmixed when it uses channel mapping family 1.
func validateInputAudioChannels(track TrackInfo) error {
	switch track.Codec {
	case "mp4a":
		channels, objectType, configuration := track.Channels, 0, -1
		if len(track.CodecConfig) >= 2 {
			// The AudioSpecificConfig is authoritative; the sample entry's
			// channel count is often left at 2.
			objectType = int(track.CodecConfig[0] >> 3)
			frequencyIndex := (track.CodecConfig[0]&0x07)<<1 | track.CodecConfig[1]>>7
			if objectType != 31 && frequencyIndex != 15 {
				configuration = int(track.CodecConfig[1]>>3) & 0x0f
				if configuration != 0 {
					channels = map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 8}[configuration]
				}
			}
		}
		switch {
		case configuration > 7:
			return fmt.Errorf("video2dvd: prepare DVD audio: AAC channel configuration %d is not supported by the built-in AAC decoder (1 to 7 are)", configuration)
		case channels <= 2:
		case configuration == 0:
			return fmt.Errorf("video2dvd: prepare DVD audio: %d-channel AAC audio described by a program config element is not supported by the built-in AAC decoder (channel configurations 1 to 7 are)", channels)
		case objectType != 2:
			return fmt.Errorf("video2dvd: prepare DVD audio: %d-channel AAC audio of object type %d is not supported by the built-in AAC decoder (multichannel AAC must be AAC-LC)", channels, objectType)
		}
	case "opus":
		if opusTrackChannels(track) <= 2 {
			return nil
		}
		head, err := parseOpusHead(track.CodecConfig)
		if err != nil {
			return fmt.Errorf("video2dvd: prepare DVD audio: %d-channel Opus audio cannot be downmixed: %s", track.Channels, strings.TrimPrefix(err.Error(), "video2dvd: "))
		}
		if _, err := opusDownmixLayout(head); err != nil {
			return fmt.Errorf("video2dvd: prepare DVD audio: %s", strings.TrimPrefix(err.Error(), "video2dvd: "))
		}
	}
	return nil
}

func validateEncoderProfile(profile Profile) error {
	wantWidth, wantHeight := 0, 0
	switch profile.Standard {
	case VideoStandardNTSC:
		wantWidth, wantHeight = 720, 480
	case VideoStandardPAL:
		wantWidth, wantHeight = 720, 576
	default:
		return ErrInvalidProfile
	}
	if profile.FrameWidth != wantWidth || profile.FrameHeight != wantHeight || (profile.Aspect != AspectRatio4x3 && profile.Aspect != AspectRatio16x9) || profile.DiscCapacityBytes < 0 {
		return ErrInvalidProfile
	}
	return nil
}

func profileFrameRate(standard VideoStandard) (int, int) {
	if standard == VideoStandardPAL {
		return 25, 1
	}
	return 30000, 1001
}

func firstSupportedAudioTrack(info MP4Info, requested *int) (TrackInfo, bool) {
	if requested != nil {
		for _, track := range info.Tracks {
			if track.Index == *requested && track.Kind == TrackKindAudio && supportedInputAudioCodec(track.Codec) {
				return track, true
			}
			if track.Index == *requested {
				return TrackInfo{}, false
			}
		}
		return TrackInfo{}, false
	}
	// The default-marked track is only preferred when it can be decoded,
	// so that a file whose marked track cannot be downmixed still converts
	// with another one; when no track can, the first supported one is
	// returned and its validation explains why.
	if track, ok := selectDefaultTrack(info, func(track TrackInfo) bool {
		return track.Kind == TrackKindAudio && supportedInputAudioCodec(track.Codec) && validateInputAudioChannels(track) == nil
	}); ok {
		return track, true
	}
	return selectDefaultTrack(info, func(track TrackInfo) bool {
		return track.Kind == TrackKindAudio && supportedInputAudioCodec(track.Codec)
	})
}

// hasDefaultMark reports whether a track of info is marked as a default
// track: an MP4 track that is enabled, or a WebM track whose FlagDefault is
// 1. A WebM track without the element, which Matroska makes a default
// track, counts only when another track of the same kind is marked as not
// default: ffmpeg writes FlagDefault only for the tracks that are not, while
// in files from tools that write no FlagDefault at all every track would
// otherwise be marked.
func hasDefaultMark(info MP4Info, track TrackInfo) bool {
	if !track.Default {
		return false
	}
	if !track.defaultImplied {
		return true
	}
	for _, other := range info.Tracks {
		if other.Kind == track.Kind && !other.Default {
			return true
		}
	}
	return false
}

// selectDefaultTrack returns the first track that matches and carries a
// default mark, or else the first track that matches.
func selectDefaultTrack(info MP4Info, matches func(TrackInfo) bool) (TrackInfo, bool) {
	first, found := TrackInfo{}, false
	for _, track := range info.Tracks {
		if !matches(track) {
			continue
		}
		if hasDefaultMark(info, track) {
			return track, true
		}
		if !found {
			first, found = track, true
		}
	}
	return first, found
}

func selectInputVideoTrack(info MP4Info, requested *int) (TrackInfo, error) {
	if requested != nil {
		for _, track := range info.Tracks {
			if track.Index != *requested {
				continue
			}
			if track.Kind != TrackKindVideo || !supportedInputVideoCodec(track.Codec) {
				return TrackInfo{}, fmt.Errorf("video2dvd: track %d is not a supported video track", *requested)
			}
			return track, nil
		}
		return TrackInfo{}, fmt.Errorf("video2dvd: video track index %d is out of range", *requested)
	}
	for _, track := range info.Tracks {
		if track.Kind == TrackKindVideo && supportedInputVideoCodec(track.Codec) {
			return track, nil
		}
	}
	return TrackInfo{}, errors.New("video2dvd: input has no video track supported by the built-in decoder (MP4 H.264, WebM VP8/VP9/AV1)")
}

func selectInputAudioTrack(info MP4Info, requested *int) (TrackInfo, error) {
	track, ok := firstSupportedAudioTrack(info, requested)
	if ok {
		return track, nil
	}
	if requested != nil {
		for _, track := range info.Tracks {
			if track.Index == *requested {
				return TrackInfo{}, fmt.Errorf("video2dvd: track %d is not a supported audio track", *requested)
			}
		}
		return TrackInfo{}, fmt.Errorf("video2dvd: audio track index %d is out of range", *requested)
	}
	return TrackInfo{}, errors.New("video2dvd: no supported audio track (MP4 AAC-LC/AC-3, WebM Opus)")
}

func supportedInputVideoCodec(codec string) bool {
	switch codec {
	case "avc1", "avc3", "vp9", "vp8", "av1":
		return true
	default:
		return false
	}
}

func supportedInputAudioCodec(codec string) bool {
	switch codec {
	case "mp4a", "ac-3", "opus":
		return true
	default:
		return false
	}
}

func hasAnyAudioTrack(info MP4Info) bool {
	for _, track := range info.Tracks {
		if track.Kind == TrackKindAudio {
			return true
		}
	}
	return false
}

func trackDisplayAspect(track TrackInfo) (int, int, error) {
	if track.Width <= 0 || track.Height <= 0 {
		return 0, 0, errors.New("video2dvd: video track has invalid display dimensions")
	}
	horizontal, vertical := uint64(track.PixelAspectHorizontal), uint64(track.PixelAspectVertical)
	if horizontal == 0 && vertical == 0 {
		horizontal, vertical = 1, 1
	}
	if horizontal == 0 || vertical == 0 {
		return 0, 0, errors.New("video2dvd: video track has an incomplete pixel aspect ratio")
	}
	numerator := uint64(track.Width) * horizontal
	denominator := uint64(track.Height) * vertical
	divisor := gcdUint64(numerator, denominator)
	numerator, denominator = numerator/divisor, denominator/divisor
	maxInt := uint64(^uint(0) >> 1)
	if numerator > maxInt || denominator > maxInt {
		return 0, 0, errors.New("video2dvd: video display aspect ratio is too large")
	}
	return int(numerator), int(denominator), nil
}

func gcdUint64(left, right uint64) uint64 {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}

func encoderOutputLimit(profile Profile) int64 {
	if profile.DiscCapacityBytes > 0 {
		return profile.DiscCapacityBytes
	}
	return DVD5CapacityBytes
}

// checkEstimatedTitleSize refuses a title whose video and audio, at the
// given rates, would not fit capacity. One LPCM stream is 192000 bytes a
// second.
func checkEstimatedTitleSize(duration time.Duration, capacity int64, videoBitrate int, audioBytesPerSecond int64) error {
	videoBytes, err := estimateRateBytes(duration, int64(videoBitrate)/8)
	if err != nil {
		return err
	}
	audioBytes, err := estimateRateBytes(duration, audioBytesPerSecond)
	if err != nil {
		return err
	}
	if capacity < 0 || videoBytes > capacity || audioBytes > capacity-videoBytes {
		return ErrDiscCapacity
	}
	// Pack and PES headers, NAV packs, and LPCM pack padding add about 3% to
	// the elementary streams once they are multiplexed.
	total := videoBytes + audioBytes
	if total/32 > capacity-total {
		return ErrDiscCapacity
	}
	return nil
}

func estimateRateBytes(duration time.Duration, bytesPerSecond int64) (int64, error) {
	if duration <= 0 || bytesPerSecond <= 0 {
		return 0, errors.New("video2dvd: invalid duration or byte rate")
	}
	seconds := int64(duration / time.Second)
	remainder := int64(duration % time.Second)
	maxInt64 := int64(^uint64(0) >> 1)
	if seconds > maxInt64/bytesPerSecond {
		return 0, ErrDiscCapacity
	}
	whole := seconds * bytesPerSecond
	fraction := (remainder*bytesPerSecond + int64(time.Second) - 1) / int64(time.Second)
	if fraction > maxInt64-whole {
		return 0, ErrDiscCapacity
	}
	return whole + fraction, nil
}

type sizeLimitedWriter struct {
	dst     io.Writer
	limit   int64
	written int64
}

func (w *sizeLimitedWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if w == nil || w.dst == nil || w.limit < 0 {
		return 0, errors.New("video2dvd: invalid size-limited writer")
	}
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, ErrDiscCapacity
	}
	if int64(len(data)) > remaining {
		count := int(remaining)
		n, err := w.dst.Write(data[:count])
		if n < 0 || n > count {
			return 0, errors.New("video2dvd: writer returned an invalid byte count")
		}
		if n > 0 {
			w.written += int64(n)
		}
		if err != nil {
			return n, err
		}
		return n, ErrDiscCapacity
	}
	n, err := w.dst.Write(data)
	if n < 0 || n > len(data) {
		return 0, errors.New("video2dvd: writer returned an invalid byte count")
	}
	w.written += int64(n)
	if err == nil && n != len(data) {
		return n, io.ErrShortWrite
	}
	return n, err
}
