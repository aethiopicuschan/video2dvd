package video2dvd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	pcmdecoder "github.com/rcarmo/go-264/audio"
	"github.com/rcarmo/go-264/audio/pcm"
	h264decode "github.com/rcarmo/go-264/decode"
	h264frame "github.com/rcarmo/go-264/frame"
)

const maxCompressedAccessUnit = 64 << 20

// DecodedVideoFrame is an owned, planar 8-bit YUV 4:2:0 frame. Chroma planes
// have half the luma width and height. PTS and Duration use the MP4 media
// timeline; edit-list offsets are not applied.
type DecodedVideoFrame struct {
	PTS      time.Duration
	Duration time.Duration
	Width    int
	Height   int
	Y        []byte
	U        []byte
	V        []byte
}

// PCMChunk contains interleaved signed 16-bit PCM in DVD's 48 kHz rate. The
// decoder outputs stereo by default, suitable for later LPCM multiplexing.
type PCMChunk struct {
	Start      time.Duration
	SampleRate int
	Channels   int
	Samples    []int16
}

// DecodeMP4Video decodes one progressive 8-bit 4:2:0 H.264 track. trackIndex
// is a zero-based trak index; nil selects the first H.264 video track. Frames
// are delivered in presentation order and own their pixel buffers.
func DecodeMP4Video(ctx context.Context, path string, trackIndex *int, handler func(DecodedVideoFrame) error) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if handler == nil {
		return errors.New("video2dvd: video frame handler is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("video2dvd: open MP4: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("video2dvd: stat MP4: %w", err)
	}
	info, err := ProbeMP4Reader(f, stat.Size())
	if err != nil {
		return err
	}
	track, err := selectVideoTrack(info, trackIndex)
	if err != nil {
		return err
	}
	if track.Codec != "avc1" && track.Codec != "avc3" {
		return fmt.Errorf("video2dvd: video codec %q is not supported by the built-in decoder", track.Codec)
	}
	if len(track.CodecConfig) == 0 || track.NALLengthSize == 0 {
		return errors.New("video2dvd: AVC track has no decoder configuration")
	}
	parameterSets, err := AVCConfigToAnnexB(track.CodecConfig)
	if err != nil {
		return err
	}
	var decoder *h264decode.StreamDecoder
	var pendingPTS map[uint64]videoSampleTiming
	decoder, err = h264decode.NewStreamDecoder(h264decode.StreamConfig{OutputOrder: true}, func(frame *h264frame.Frame) error {
		timing, ok := pendingPTS[frame.Tag]
		if !ok {
			return fmt.Errorf("video2dvd: decoder returned unknown sample tag %d", frame.Tag)
		}
		delete(pendingPTS, frame.Tag)
		ptsTime, err := ticksToDuration(timing.pts, track.TimeScale)
		if err != nil {
			return err
		}
		durationTime, err := ticksToDuration(int64(timing.duration), track.TimeScale)
		if err != nil {
			return err
		}
		decoded, err := copyDecodedFrame(frame, ptsTime, durationTime)
		if err != nil {
			return err
		}
		return handler(decoded)
	})
	if err != nil {
		return fmt.Errorf("video2dvd: create H.264 decoder: %w", err)
	}
	pendingPTS = make(map[uint64]videoSampleTiming, 16)
	firstAccessUnit := true
	_, err = demuxMP4Reader(f, stat.Size(), func(sampleTrack TrackInfo, sample SampleInfo, data io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sampleTrack.Index != track.Index {
			return nil
		}
		if sample.Size == 0 || sample.Size > maxCompressedAccessUnit {
			return fmt.Errorf("sample %d exceeds the compressed access-unit limit", sample.SampleNumber)
		}
		encoded := make([]byte, int(sample.Size))
		if _, err := io.ReadFull(data, encoded); err != nil {
			return fmt.Errorf("read sample %d: %w", sample.SampleNumber, err)
		}
		annexB, err := AVCToAnnexB(encoded, track.NALLengthSize)
		if err != nil {
			return fmt.Errorf("sample %d: %w", sample.SampleNumber, err)
		}
		if firstAccessUnit {
			annexB = append(parameterSets, annexB...)
			firstAccessUnit = false
		}
		tag := uint64(sample.SampleNumber)
		pendingPTS[tag] = videoSampleTiming{pts: sample.PTS, duration: sample.Duration}
		if err := decoder.DecodeAccessUnit(annexB, tag); err != nil {
			delete(pendingPTS, tag)
			return fmt.Errorf("decode sample %d: %w", sample.SampleNumber, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if firstAccessUnit {
		return errors.New("video2dvd: H.264 track contains no samples")
	}
	if err := decoder.Drain(); err != nil {
		return fmt.Errorf("video2dvd: finish H.264 stream: %w", err)
	}
	return ctx.Err()
}

// DecodeMP4Audio decodes an AAC-LC or AC-3 MP4 audio track to 48 kHz stereo
// S16 PCM. trackIndex selects the zero-based trak; nil selects a supported
// track. Audio payloads and edit-list timing are validated by the decoder.
func DecodeMP4Audio(ctx context.Context, path string, trackIndex *int, handler func(PCMChunk) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if ctx != nil && ctx.Err() != nil {
				err = ctx.Err()
				return
			}
			err = fmt.Errorf("video2dvd: decode MP4 audio %q panicked: %v", path, recovered)
		}
	}()
	if handler == nil {
		return errors.New("video2dvd: PCM handler is required")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	var delivered bool
	err = decodeMP4AudioPrimary(ctx, path, trackIndex, func(chunk PCMChunk) error {
		delivered = true
		return handler(chunk)
	})
	if err == nil || delivered || !strings.Contains(err.Error(), "non-zero alignment padding") {
		return err
	}
	return decodeMP4AACFallback(ctx, path, trackIndex, handler)
}

func decodeMP4AudioPrimary(ctx context.Context, path string, trackIndex *int, handler func(PCMChunk) error) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if handler == nil {
		return errors.New("video2dvd: PCM handler is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("video2dvd: open MP4: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("video2dvd: stat MP4: %w", err)
	}
	info, err := ProbeMP4Reader(f, stat.Size())
	if err != nil {
		return err
	}
	selected, err := selectAudioTrack(info, trackIndex)
	if err != nil {
		return err
	}
	decoder, err := pcmdecoder.Open(ctx, f, stat.Size(), pcmdecoder.Options{
		TargetRate:     48000,
		TargetChannels: 2,
		TrackIndex:     &selected.Index,
		Limits: pcm.Limits{
			MaxBytes:           stat.Size(),
			MaxDurationSeconds: 24 * 60 * 60,
		},
	})
	if err != nil {
		return fmt.Errorf("video2dvd: open audio decoder: %w", err)
	}
	defer decoder.Close()
	buffer := make([]int16, 8192)
	for {
		n, span, err := decoder.ReadPCM(ctx, buffer)
		if n > 0 {
			chunk := PCMChunk{
				Start:      time.Duration(span.StartFrame) * time.Second / 48000,
				SampleRate: 48000,
				Channels:   2,
				Samples:    append([]int16(nil), buffer[:n]...),
			}
			if callbackErr := handler(chunk); callbackErr != nil {
				return callbackErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("video2dvd: decode audio: %w", err)
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func selectVideoTrack(info MP4Info, requested *int) (TrackInfo, error) {
	if requested != nil {
		for _, track := range info.Tracks {
			if track.Index == *requested {
				if track.Kind != TrackKindVideo {
					return TrackInfo{}, fmt.Errorf("video2dvd: track %d is not video", *requested)
				}
				return track, nil
			}
		}
		return TrackInfo{}, fmt.Errorf("video2dvd: video track index %d is out of range", *requested)
	}
	for _, track := range info.Tracks {
		if track.Kind == TrackKindVideo && (track.Codec == "avc1" || track.Codec == "avc3") {
			return track, nil
		}
	}
	return TrackInfo{}, errors.New("video2dvd: no supported H.264 video track")
}

func selectAudioTrack(info MP4Info, requested *int) (TrackInfo, error) {
	if requested != nil {
		for _, track := range info.Tracks {
			if track.Index == *requested {
				if track.Kind != TrackKindAudio {
					return TrackInfo{}, fmt.Errorf("video2dvd: track %d is not audio", *requested)
				}
				return track, nil
			}
		}
		return TrackInfo{}, fmt.Errorf("video2dvd: audio track index %d is out of range", *requested)
	}
	for _, track := range info.Tracks {
		if track.Kind == TrackKindAudio && (track.Codec == "mp4a" || track.Codec == "ac-3") {
			return track, nil
		}
	}
	return TrackInfo{}, errors.New("video2dvd: no supported AAC or AC-3 audio track")
}

type videoSampleTiming struct {
	pts      int64
	duration uint32
}

func copyDecodedFrame(frame *h264frame.Frame, pts, duration time.Duration) (DecodedVideoFrame, error) {
	if frame == nil || frame.Width <= 0 || frame.Height <= 0 || frame.Width%2 != 0 || frame.Height%2 != 0 {
		return DecodedVideoFrame{}, errors.New("video2dvd: decoder returned an invalid YUV 4:2:0 frame")
	}
	if frame.StrideY < frame.Width || frame.StrideC < frame.Width/2 {
		return DecodedVideoFrame{}, errors.New("video2dvd: decoder returned invalid plane strides")
	}
	result := DecodedVideoFrame{
		PTS:      pts,
		Duration: duration,
		Width:    frame.Width,
		Height:   frame.Height,
		Y:        make([]byte, frame.Width*frame.Height),
		U:        make([]byte, frame.Width*frame.Height/4),
		V:        make([]byte, frame.Width*frame.Height/4),
	}
	if err := copyPlane(result.Y, frame.Y, frame.Width, frame.Height, frame.StrideY); err != nil {
		return DecodedVideoFrame{}, err
	}
	if err := copyPlane(result.U, frame.U, frame.Width/2, frame.Height/2, frame.StrideC); err != nil {
		return DecodedVideoFrame{}, err
	}
	if err := copyPlane(result.V, frame.V, frame.Width/2, frame.Height/2, frame.StrideC); err != nil {
		return DecodedVideoFrame{}, err
	}
	return result, nil
}

func copyPlane(dst, src []byte, width, height, stride int) error {
	if width <= 0 || height <= 0 || stride < width || len(dst) != width*height || len(src) < (height-1)*stride+width {
		return errors.New("video2dvd: decoder returned a truncated frame plane")
	}
	for y := range height {
		copy(dst[y*width:(y+1)*width], src[y*stride:y*stride+width])
	}
	return nil
}

func ticksToDuration(ticks int64, timeScale uint32) (time.Duration, error) {
	if timeScale == 0 {
		return 0, errors.New("video2dvd: zero media time scale")
	}
	negative := ticks < 0
	absolute := uint64(ticks)
	if negative {
		absolute = uint64(-(ticks + 1)) + 1
	}
	seconds := absolute / uint64(timeScale)
	remainder := absolute % uint64(timeScale)
	maxInt64 := uint64(^uint64(0) >> 1)
	if seconds > maxInt64/uint64(time.Second) {
		return 0, errors.New("video2dvd: presentation timestamp overflows duration")
	}
	nanoseconds := seconds*uint64(time.Second) + remainder*uint64(time.Second)/uint64(timeScale)
	if nanoseconds > maxInt64 {
		return 0, errors.New("video2dvd: presentation timestamp overflows duration")
	}
	duration := time.Duration(nanoseconds)
	if negative {
		duration = -duration
	}
	return duration, nil
}
