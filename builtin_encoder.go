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

// InProcessEncoder is a built-in Encoder that uses only Go code. Its current
// MPEG-2 video coder uses I pictures and P-picture macroblock skips with a
// simple intra quantizer and no motion estimation; use it as an experimental
// backend rather than a quality transcoder. It converts MP4 AAC-LC/AC-3 and
// WebM Opus audio to LPCM and creates silent LPCM when the input has no audio
// track.
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
	duration := info.Duration
	if duration <= 0 {
		duration = videoTrack.Duration
	}
	if duration <= 0 {
		return EncodedTitle{}, errors.New("video2dvd: input has no usable duration")
	}
	if err := validateEncoderProfile(profile); err != nil {
		return EncodedTitle{}, err
	}
	if err := checkEstimatedTitleSize(duration, encoderOutputLimit(profile)); err != nil {
		return EncodedTitle{}, err
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
	videoEncoder, err := NewMPEG2VideoEncoder(&sizeLimitedWriter{dst: videoFile, limit: encoderOutputLimit(profile)}, profile)
	if err != nil {
		_ = videoFile.Close()
		return EncodedTitle{}, err
	}
	sourceAspectN, sourceAspectD, err := trackDisplayAspect(videoTrack)
	if err != nil {
		_ = videoFile.Close()
		return EncodedTitle{}, err
	}
	var prepareErr error
	if videoTrack.Codec == "vp9" || videoTrack.Codec == "vp8" || videoTrack.Codec == "av1" {
		prepareErr = PrepareWebMVideoForDVD(ctx, input.VideoPath, &videoTrack.Index, profile, sourceAspectN, sourceAspectD, FitLetterbox, videoEncoder.WriteFrame)
	} else {
		prepareErr = PrepareMP4VideoForDVD(ctx, input.VideoPath, &videoTrack.Index, profile, sourceAspectN, sourceAspectD, FitLetterbox, videoEncoder.WriteFrame)
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

	audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("video2dvd: create LPCM output: %w", err)
	}
	audioCreated = true
	audioWriter := &sizeLimitedWriter{dst: audioFile, limit: encoderOutputLimit(profile)}
	audioInfo := info
	if input.AudioPath != input.VideoPath {
		audioInfo, err = ProbeMedia(input.AudioPath)
		if err != nil {
			_ = audioFile.Close()
			return EncodedTitle{}, fmt.Errorf("video2dvd: inspect audio source: %w", err)
		}
	}
	audioTrack, hasAudio := firstSupportedAudioTrack(audioInfo, input.AudioTrackIndex)
	audioTimeline, err := newPCMWindowWriter(audioWriter, duration, input.AudioOffset)
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
	} else if hasAnyAudioTrack(audioInfo) {
		err = errors.New("video2dvd: input has audio tracks but none are supported by the built-in decoder (MP4 AAC-LC/AC-3, WebM Opus)")
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
	fpsN, fpsD := profileFrameRate(profile.Standard)
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

func checkEstimatedTitleSize(duration time.Duration, capacity int64) error {
	videoBytes, err := estimateRateBytes(duration, mpeg2VideoBitrate/8)
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
