package video2dvd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	aac "github.com/arabian9ts/aac-go"
	"github.com/rcarmo/go-264/audio/convert"
	mp4audio "github.com/rcarmo/go-264/audio/mp4"
	"github.com/rcarmo/go-264/audio/pcm"
	"github.com/rcarmo/go-264/audio/resample"
)

const aacFallbackDecodedFramesPerSample = 1024

func decodeMP4AACFallback(ctx context.Context, path string, trackIndex *int, handler func(PCMChunk) error) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if handler == nil {
		return errors.New("video2dvd: PCM handler is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	probed, err := ProbeMedia(path)
	if err != nil {
		return fmt.Errorf("video2dvd: inspect MP4 before AAC fallback: %w", err)
	}
	selected, err := selectAudioTrack(probed, trackIndex)
	if err != nil {
		return err
	}
	selectedIndex := &selected.Index
	edited := newEditedPCMWriter(selected.edit, handler)
	source, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("video2dvd: open MP4 for AAC fallback: %w", err)
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil {
		return fmt.Errorf("video2dvd: stat MP4 for AAC fallback: %w", err)
	}
	reader, err := mp4audio.OpenTrack(ctx, hideMP4EditList(source, selected.edit), stat.Size(), mp4audio.Limits{
		MaxBytes: stat.Size(), MaxDurationSeconds: 24 * 60 * 60,
	}, *selectedIndex)
	if err != nil {
		return fmt.Errorf("video2dvd: inspect MP4 AAC track for fallback: %w", err)
	}
	track := reader.Track()
	if track.Codec != "mp4a" || (track.Channels != 1 && track.Channels != 2) {
		return fmt.Errorf("video2dvd: AAC fallback supports AAC-LC mono/stereo tracks, got codec %q with %d channels", track.Codec, track.Channels)
	}
	if track.SampleRate < 8000 || track.SampleRate > 96000 {
		return fmt.Errorf("video2dvd: AAC fallback does not support sample rate %d Hz", track.SampleRate)
	}
	if track.SampleCount > math.MaxInt/aacFallbackDecodedFramesPerSample {
		return errors.New("video2dvd: AAC decoded sample count overflows int")
	}
	expectedFrames := int64(track.SampleCount) * aacFallbackDecodedFramesPerSample
	timing, err := track.TimingPlan(expectedFrames)
	if err != nil {
		return fmt.Errorf("video2dvd: AAC edit/priming timing is unsupported by fallback: %w", err)
	}
	for index := 0; index < track.SampleCount; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, packetInfo, err := reader.ReadPacket(ctx, index, nil)
		if !errors.Is(err, io.ErrShortBuffer) {
			return fmt.Errorf("video2dvd: inspect MP4 AAC sample %d timing: %w", index+1, err)
		}
		if packetInfo.CompositionOffset != 0 {
			return fmt.Errorf("video2dvd: AAC fallback does not support audio composition offsets in sample %d", index+1)
		}
		ticks := uint64(packetInfo.Duration) * uint64(track.SampleRate)
		if track.MediaTimescale == 0 || ticks%uint64(track.MediaTimescale) != 0 {
			return fmt.Errorf("video2dvd: AAC sample %d has a fractional-sample duration", index+1)
		}
		frames := ticks / uint64(track.MediaTimescale)
		if frames == 0 || frames > aacFallbackDecodedFramesPerSample || (index < track.SampleCount-1 && frames != aacFallbackDecodedFramesPerSample) {
			return fmt.Errorf("video2dvd: AAC sample %d has unsupported duration %d source frames", index+1, frames)
		}
	}
	decoder, err := aac.NewDecoder()
	if err != nil {
		return fmt.Errorf("video2dvd: create AAC fallback decoder: %w", err)
	}
	spool, err := os.CreateTemp("", "video2dvd-aac-*.pcm")
	if err != nil {
		return fmt.Errorf("video2dvd: create AAC fallback spool: %w", err)
	}
	spoolPath := spool.Name()
	defer os.Remove(spoolPath)
	defer spool.Close()
	packet := make([]byte, 0, 64<<10)
	decodedFrames := int64(0)
	for index := 0; index < track.SampleCount; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, packetInfo, err := reader.ReadPacket(ctx, index, nil)
		if !errors.Is(err, io.ErrShortBuffer) {
			return fmt.Errorf("video2dvd: read MP4 AAC sample %d: %w", index+1, err)
		}
		if packetInfo.Size > cap(packet) {
			packet = make([]byte, packetInfo.Size)
		}
		n, _, err := reader.ReadPacket(ctx, index, packet[:packetInfo.Size])
		if err != nil {
			return fmt.Errorf("video2dvd: read MP4 AAC sample %d: %w", index+1, err)
		}
		adts, err := makeAACADTSHeader(track.AudioSpecificConfig, n)
		if err != nil {
			return fmt.Errorf("video2dvd: AAC fallback configuration: %w", err)
		}
		adts = append(adts, packet[:n]...)
		pcmSamples, err := decoder.Decode(adts)
		if err != nil {
			return fmt.Errorf("video2dvd: AAC fallback decode sample %d: %w", index+1, err)
		}
		if len(pcmSamples)%track.Channels != 0 {
			return fmt.Errorf("video2dvd: AAC fallback returned an incomplete channel frame at sample %d", index+1)
		}
		frames := int64(len(pcmSamples) / track.Channels)
		if frames != aacFallbackDecodedFramesPerSample {
			return fmt.Errorf("video2dvd: AAC fallback decoded %d frames from sample %d; want %d", frames, index+1, aacFallbackDecodedFramesPerSample)
		}
		if err := writePCM16(spool, pcmSamples); err != nil {
			return fmt.Errorf("video2dvd: spool AAC fallback PCM: %w", err)
		}
		decodedFrames += frames
	}
	if decodedFrames != timing.DecodedFrames {
		return fmt.Errorf("video2dvd: AAC fallback decoded %d frames; timing metadata requires %d", decodedFrames, timing.DecodedFrames)
	}
	if err := spool.Sync(); err != nil {
		return fmt.Errorf("video2dvd: flush AAC fallback PCM: %w", err)
	}
	pcmSource := &aacPCMFileSource{
		file:    spool,
		info:    pcm.Info{SampleRate: track.SampleRate, Channels: track.Channels, Frames: timing.OutputFrames},
		priming: timing.PrimingFrames,
		leading: timing.LeadingSilenceFrames,
	}
	converted, err := convert.New(pcmSource, 2)
	if err != nil {
		return fmt.Errorf("video2dvd: configure AAC fallback channels: %w", err)
	}
	resampled, err := resample.New(converted, 48000)
	if err != nil {
		return fmt.Errorf("video2dvd: configure AAC fallback resampler: %w", err)
	}
	floatPCM := make([]float64, 8192*2)
	shortPCM := make([]int16, len(floatPCM))
	var outputFrame int64
	expectedOutputFrames := resampled.Info().Frames
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frames, readErr := resampled.ReadFrames(ctx, floatPCM)
		if frames > 0 {
			values := floatPCM[:frames*2]
			if err := convert.S16(shortPCM[:len(values)], values); err != nil {
				return fmt.Errorf("video2dvd: convert AAC fallback PCM: %w", err)
			}
			chunk := PCMChunk{
				Start:      time.Duration(outputFrame) * time.Second / 48000,
				SampleRate: 48000,
				Channels:   2,
				Samples:    append([]int16(nil), shortPCM[:len(values)]...),
			}
			if err := edited.WriteChunk(chunk); err != nil {
				return err
			}
			outputFrame += int64(frames)
		}
		if readErr == io.EOF {
			if outputFrame != expectedOutputFrames {
				return fmt.Errorf("video2dvd: AAC fallback produced %d output frames; timing requires %d", outputFrame, expectedOutputFrames)
			}
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("video2dvd: resample AAC fallback PCM: %w", readErr)
		}
		if frames == 0 {
			return io.ErrNoProgress
		}
	}
}

func makeAACADTSHeader(audioSpecificConfig []byte, payloadSize int) ([]byte, error) {
	if len(audioSpecificConfig) < 2 || payloadSize < 0 {
		return nil, errors.New("invalid AudioSpecificConfig or access unit")
	}
	bits := binary.BigEndian.Uint16(audioSpecificConfig[:2])
	objectType := int(bits >> 11)
	frequencyIndex := int(bits>>7) & 0x0f
	channelConfig := int(bits>>3) & 0x0f
	if objectType != 2 {
		return nil, fmt.Errorf("AAC object type %d is not AAC-LC", objectType)
	}
	if frequencyIndex > 12 {
		return nil, fmt.Errorf("AAC sampling frequency index %d is not supported by ADTS", frequencyIndex)
	}
	if channelConfig != 1 && channelConfig != 2 {
		return nil, fmt.Errorf("AAC channel configuration %d is not mono/stereo", channelConfig)
	}
	frameLength := payloadSize + 7
	if frameLength > 0x1fff {
		return nil, fmt.Errorf("AAC access unit length %d exceeds the ADTS limit", payloadSize)
	}
	header := []byte{
		0xff,
		0xf1,
		byte((objectType-1)<<6 | frequencyIndex<<2 | channelConfig>>2),
		byte((channelConfig&3)<<6 | frameLength>>11),
		byte(frameLength >> 3),
		byte((frameLength&7)<<5 | 0x1f),
		0xfc,
	}
	return header, nil
}

func writePCM16(dst io.Writer, samples []int16) error {
	var buffer [8192]byte
	for len(samples) > 0 {
		count := min(len(samples), len(buffer)/2)
		for i, sample := range samples[:count] {
			binary.LittleEndian.PutUint16(buffer[i*2:], uint16(sample))
		}
		if err := writeAll(dst, buffer[:count*2]); err != nil {
			return err
		}
		samples = samples[count:]
	}
	return nil
}

type aacPCMFileSource struct {
	file     *os.File
	info     pcm.Info
	priming  int64
	leading  int64
	position int64
}

func (s *aacPCMFileSource) Info() pcm.Info { return s.info }

func (s *aacPCMFileSource) SeekFrame(_ context.Context, frame int64) error {
	if frame < 0 || frame > s.info.Frames {
		return fmt.Errorf("%w: AAC PCM seek frame", pcm.ErrMalformed)
	}
	s.position = frame
	return nil
}

func (s *aacPCMFileSource) ReadFrames(ctx context.Context, dst []float64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(dst)%s.info.Channels != 0 {
		return 0, fmt.Errorf("%w: AAC PCM buffer shape", pcm.ErrMalformed)
	}
	wantFrames := min(int64(len(dst)/s.info.Channels), s.info.Frames-s.position)
	want := int(wantFrames)
	if want == 0 {
		return 0, io.EOF
	}
	channels := s.info.Channels
	framesDone := 0
	if s.position < s.leading {
		silence := min(want, int(s.leading-s.position))
		clear(dst[:silence*channels])
		framesDone += silence
		s.position += int64(silence)
	}
	if framesDone < want {
		mediaFrames := want - framesDone
		mediaFrameOffset := s.position - s.leading + s.priming
		byteOffset := mediaFrameOffset * int64(channels*2)
		byteCount := mediaFrames * channels * 2
		data := make([]byte, byteCount)
		if _, err := s.file.ReadAt(data, byteOffset); err != nil {
			return framesDone, fmt.Errorf("read AAC PCM spool: %w", err)
		}
		for i := range mediaFrames * channels {
			dst[(framesDone*channels)+i] = float64(int16(binary.LittleEndian.Uint16(data[i*2:]))) / 32768
		}
		framesDone += mediaFrames
		s.position += int64(mediaFrames)
	}
	return framesDone, nil
}
