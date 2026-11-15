package mp4todvd

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
// MPEG-2 video coder is deliberately basic and retains only block DC values;
// use it for pipeline integration and low-detail material, not quality
// transcoding. It converts available AAC-LC/AC-3 audio to LPCM and creates
// silent LPCM when the input has no audio track.
type InProcessEncoder struct{}

// NewInProcessEncoder returns the built-in Encoder implementation.
func NewInProcessEncoder() *InProcessEncoder { return &InProcessEncoder{} }

// Encode turns one MP4 into DVD-compatible elementary streams in workDir.
func (e *InProcessEncoder) Encode(ctx context.Context, inputPath, workDir string, profile Profile) (EncodedTitle, error) {
	if e == nil {
		return EncodedTitle{}, errors.New("mp4todvd: in-process encoder is nil")
	}
	if ctx == nil {
		return EncodedTitle{}, errors.New("mp4todvd: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return EncodedTitle{}, err
	}
	info, err := ProbeMP4(inputPath)
	if err != nil {
		return EncodedTitle{}, err
	}
	videoTrack, err := selectVideoTrack(info, nil)
	if err != nil {
		return EncodedTitle{}, err
	}
	if videoTrack.Width <= 0 || videoTrack.Height <= 0 {
		return EncodedTitle{}, errors.New("mp4todvd: video track has invalid dimensions")
	}
	duration := info.Duration
	if duration <= 0 {
		duration = videoTrack.Duration
	}
	if duration <= 0 {
		return EncodedTitle{}, errors.New("mp4todvd: MP4 has no usable duration")
	}
	if err := validateEncoderProfile(profile); err != nil {
		return EncodedTitle{}, err
	}
	if err := checkEstimatedTitleSize(duration, encoderOutputLimit(profile)); err != nil {
		return EncodedTitle{}, err
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return EncodedTitle{}, fmt.Errorf("mp4todvd: create encoder work directory: %w", err)
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
		return EncodedTitle{}, fmt.Errorf("mp4todvd: create MPEG-2 output: %w", err)
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
	prepareErr := PrepareMP4VideoForDVD(ctx, inputPath, &videoTrack.Index, profile, sourceAspectN, sourceAspectD, FitLetterbox, videoEncoder.WriteFrame)
	closeEncoderErr := videoEncoder.Close()
	closeVideoErr := videoFile.Close()
	if prepareErr != nil {
		return EncodedTitle{}, fmt.Errorf("mp4todvd: prepare DVD video: %w", prepareErr)
	}
	if closeEncoderErr != nil {
		return EncodedTitle{}, closeEncoderErr
	}
	if closeVideoErr != nil {
		return EncodedTitle{}, fmt.Errorf("mp4todvd: close MPEG-2 output: %w", closeVideoErr)
	}

	audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("mp4todvd: create LPCM output: %w", err)
	}
	audioCreated = true
	audioWriter := &sizeLimitedWriter{dst: audioFile, limit: encoderOutputLimit(profile)}
	audioTrack, hasAudio := firstSupportedAudioTrack(info)
	if hasAudio {
		err = PrepareMP4AudioForDVD(ctx, inputPath, &audioTrack.Index, audioWriter)
	} else if hasAnyAudioTrack(info) {
		err = errors.New("mp4todvd: input has audio tracks but none are supported by the built-in decoder")
	} else {
		err = writeSilentLPCM(audioWriter, duration)
	}
	closeAudioErr := audioFile.Close()
	if err != nil {
		return EncodedTitle{}, fmt.Errorf("mp4todvd: prepare DVD audio: %w", err)
	}
	if closeAudioErr != nil {
		return EncodedTitle{}, fmt.Errorf("mp4todvd: close LPCM output: %w", closeAudioErr)
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

func firstSupportedAudioTrack(info MP4Info) (TrackInfo, bool) {
	for _, track := range info.Tracks {
		if track.Kind == TrackKindAudio && (track.Codec == "mp4a" || track.Codec == "ac-3") {
			return track, true
		}
	}
	return TrackInfo{}, false
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
		return 0, 0, errors.New("mp4todvd: video track has invalid display dimensions")
	}
	horizontal, vertical := uint64(track.PixelAspectHorizontal), uint64(track.PixelAspectVertical)
	if horizontal == 0 && vertical == 0 {
		horizontal, vertical = 1, 1
	}
	if horizontal == 0 || vertical == 0 {
		return 0, 0, errors.New("mp4todvd: video track has an incomplete pixel aspect ratio")
	}
	numerator := uint64(track.Width) * horizontal
	denominator := uint64(track.Height) * vertical
	divisor := gcdUint64(numerator, denominator)
	numerator, denominator = numerator/divisor, denominator/divisor
	maxInt := uint64(^uint(0) >> 1)
	if numerator > maxInt || denominator > maxInt {
		return 0, 0, errors.New("mp4todvd: video display aspect ratio is too large")
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
	videoBytes, err := estimateRateBytes(duration, 625_000) // 5 Mbit/s stream header rate.
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
		return 0, errors.New("mp4todvd: invalid duration or byte rate")
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
		return 0, errors.New("mp4todvd: invalid size-limited writer")
	}
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, ErrDiscCapacity
	}
	if int64(len(data)) > remaining {
		count := int(remaining)
		n, err := w.dst.Write(data[:count])
		if n < 0 || n > count {
			return 0, errors.New("mp4todvd: writer returned an invalid byte count")
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
		return 0, errors.New("mp4todvd: writer returned an invalid byte count")
	}
	w.written += int64(n)
	if err == nil && n != len(data) {
		return n, io.ErrShortWrite
	}
	return n, err
}

func writeSilentLPCM(dst io.Writer, duration time.Duration) error {
	if dst == nil || duration <= 0 {
		return errors.New("mp4todvd: invalid silent LPCM output")
	}
	seconds := int64(duration / time.Second)
	remainder := int64(duration % time.Second)
	frames := seconds*48000 + remainder*48000/int64(time.Second)
	const framesPerChunk = 4096
	silence := make([]int16, framesPerChunk*2)
	for frames > 0 {
		count := int64(framesPerChunk)
		if frames < count {
			count = frames
		}
		chunk := PCMChunk{SampleRate: 48000, Channels: 2, Samples: silence[:int(count*2)]}
		if err := WriteDVDLPCM(dst, chunk); err != nil {
			return err
		}
		frames -= count
	}
	return nil
}
