package video2dvd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// The container duration only feeds the up-front capacity estimate; live
	// WebM has none, and the title length comes from the frames encoded below.
	estimatedDuration := info.Duration
	if estimatedDuration <= 0 {
		estimatedDuration = videoTrack.Duration
	}
	if estimatedDuration > 0 {
		if err := checkEstimatedTitleSize(estimatedDuration, encoderOutputLimit(profile), effectiveMPEG2VideoBitrate(profile)); err != nil {
			return EncodedTitle{}, err
		}
	}
	audioInfo := info
	if input.AudioPath != input.VideoPath {
		audioInfo, err = ProbeMedia(input.AudioPath)
		if err != nil {
			return EncodedTitle{}, fmt.Errorf("video2dvd: inspect audio source: %w", err)
		}
	}
	audioTrack, hasAudio := firstSupportedAudioTrack(audioInfo, input.AudioTrackIndex)
	if !hasAudio && hasAnyAudioTrack(audioInfo) {
		return EncodedTitle{}, errors.New("video2dvd: prepare DVD audio: input has audio tracks but none are supported by the built-in decoder (MP4 AAC-LC/AC-3, WebM Opus)")
	}
	if hasAudio {
		if err := validateInputAudioChannels(audioTrack); err != nil {
			return EncodedTitle{}, err
		}
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: create encoder work directory: %w", err)
	}
	videoPath := filepath.Join(workDir, "video.m2v")
	audioPath := filepath.Join(workDir, "audio.lpcm")
	videoCreated, audioCreated := false, false
	cleanup := true
	defer func() {
		if cleanup {
			if videoCreated {
				_ = os.Remove(videoPath)
			}
			if audioCreated {
				_ = os.Remove(audioPath)
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
		videoEncoder, err = NewMPEG2SoftTelecineEncoder(videoWriter, profile)
	} else {
		videoEncoder, err = NewMPEG2VideoEncoder(videoWriter, profile)
	}
	if err != nil {
		_ = videoFile.Close()
		return EncodedTitle{}, err
	}
	var prepareErr error
	// audioOffset places the audio on the title timeline, whose zero is the
	// first encoded video frame.
	audioOffset := input.AudioOffset
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
	} else {
		prepareErr = prepareMP4VideoForDVD(ctx, input.VideoPath, &videoTrack.Index, profile, sourceAspectN, sourceAspectD, FitLetterbox, rateConverter)
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

	audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: create LPCM output: %w", err)
	}
	audioCreated = true
	audioWriter := &sizeLimitedWriter{dst: audioFile, limit: encoderOutputLimit(profile)}
	audioTimeline, err := newPCMWindowWriterSamples(audioWriter, audioSamples, audioOffset)
	if err != nil {
		_ = audioFile.Close()
		return EncodedTitle{}, err
	}
	if hasAudio {
		if audioTrack.Codec == "opus" {
			err = DecodeWebMAudio(ctx, input.AudioPath, &audioTrack.Index, audioTimeline.WriteChunk)
		} else {
			err = DecodeMP4Audio(ctx, input.AudioPath, &audioTrack.Index, audioTimeline.WriteChunk)
		}
	}
	if err == nil {
		err = audioTimeline.Finish()
	}
	closeAudioErr := audioFile.Close()
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: prepare DVD audio: %w", err)
	}
	if closeAudioErr != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: close LPCM output: %w", closeAudioErr)
	}
	if err := ctx.Err(); err != nil {
		return EncodedTitle{}, err
	}
	cleanup = false
	return EncodedTitle{
		VideoPath:            "video.m2v",
		AudioPath:            "audio.lpcm",
		VideoCodec:           VideoCodecMPEG2,
		AudioCodec:           AudioCodecLPCM,
		Aspect:               profile.Aspect,
		Width:                profile.FrameWidth,
		Height:               profile.FrameHeight,
		FrameRateNumerator:   fpsN,
		FrameRateDenominator: fpsD,
		Duration:             duration,
	}, nil
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
// downmix before any video is encoded.
func validateInputAudioChannels(track TrackInfo) error {
	channels := track.Channels
	if track.Codec == "mp4a" && len(track.CodecConfig) >= 2 {
		// The AudioSpecificConfig is authoritative; the sample entry's
		// channel count is often left at 2.
		objectType := track.CodecConfig[0] >> 3
		frequencyIndex := (track.CodecConfig[0]&0x07)<<1 | track.CodecConfig[1]>>7
		if configured := int(track.CodecConfig[1]>>3) & 0x0f; configured != 0 && objectType != 31 && frequencyIndex != 15 {
			channels = configured
		}
	}
	if (track.Codec == "mp4a" || track.Codec == "opus") && channels > 2 {
		return fmt.Errorf("video2dvd: prepare DVD audio: %d-channel %s audio is not supported by the built-in decoder (mono or stereo only)", channels, track.Codec)
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
	for _, track := range info.Tracks {
		if track.Kind == TrackKindAudio && supportedInputAudioCodec(track.Codec) {
			return track, true
		}
	}
	return TrackInfo{}, false
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

func checkEstimatedTitleSize(duration time.Duration, capacity int64, videoBitrate int) error {
	videoBytes, err := estimateRateBytes(duration, int64(videoBitrate)/8)
	if err != nil {
		return err
	}
	audioBytes, err := estimateRateBytes(duration, 192_000) // 48 kHz, 16-bit stereo LPCM.
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
