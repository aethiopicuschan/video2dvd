package mp4todvd

import (
	"errors"
	"io"
	"math"
	"time"
)

const dvdLPCMSampleRate = int64(48_000)

// pcmWindowWriter places decoded PCM on a fixed video timeline, inserting
// silence for positive offsets and gaps and trimming samples outside the title.
type pcmWindowWriter struct {
	dst       io.Writer
	offset    time.Duration
	endSample int64
	next      int64
}

func newPCMWindowWriter(dst io.Writer, duration, offset time.Duration) (*pcmWindowWriter, error) {
	if dst == nil {
		return nil, errors.New("mp4todvd: LPCM destination writer is required")
	}
	if duration <= 0 {
		return nil, errors.New("mp4todvd: audio window duration must be positive")
	}
	return &pcmWindowWriter{dst: dst, offset: offset, endSample: durationToSampleCount(duration)}, nil
}

func (w *pcmWindowWriter) WriteChunk(chunk PCMChunk) error {
	if w == nil || w.dst == nil {
		return errors.New("mp4todvd: LPCM timeline writer is not configured")
	}
	if chunk.SampleRate != int(dvdLPCMSampleRate) || chunk.Channels != 2 || len(chunk.Samples)%2 != 0 {
		return errors.New("mp4todvd: LPCM timeline requires 48 kHz stereo PCM")
	}
	if len(chunk.Samples) == 0 || w.next >= w.endSample {
		return nil
	}
	start, ok := addMediaOffsets(w.offset, chunk.Start)
	if !ok {
		return errors.New("mp4todvd: audio timestamp overflows")
	}
	startSample := durationToSampleCount(start)
	frameCount := int64(len(chunk.Samples) / 2)
	if startSample < 0 {
		trim := min(-startSample, frameCount)
		chunk.Samples = chunk.Samples[trim*2:]
		frameCount -= trim
		startSample = 0
	}
	if frameCount == 0 || startSample >= w.endSample {
		return nil
	}
	if startSample > w.next {
		if err := w.writeSilence(min(startSample, w.endSample) - w.next); err != nil {
			return err
		}
	}
	if startSample < w.next {
		skip := min(w.next-startSample, frameCount)
		chunk.Samples = chunk.Samples[skip*2:]
		frameCount -= skip
		startSample += skip
	}
	if frameCount == 0 || startSample >= w.endSample {
		return nil
	}
	frameCount = min(frameCount, w.endSample-startSample)
	if err := WriteDVDLPCM(w.dst, PCMChunk{
		SampleRate: int(dvdLPCMSampleRate),
		Channels:   2,
		Samples:    chunk.Samples[:frameCount*2],
	}); err != nil {
		return err
	}
	w.next = startSample + frameCount
	return nil
}

func (w *pcmWindowWriter) Finish() error {
	if w == nil || w.dst == nil {
		return errors.New("mp4todvd: LPCM timeline writer is not configured")
	}
	return w.writeSilence(w.endSample - w.next)
}

func (w *pcmWindowWriter) writeSilence(frames int64) error {
	if frames <= 0 {
		return nil
	}
	const chunkFrames = 4096
	silence := make([]int16, chunkFrames*2)
	for frames > 0 {
		count := min(frames, int64(chunkFrames))
		if err := WriteDVDLPCM(w.dst, PCMChunk{
			SampleRate: int(dvdLPCMSampleRate),
			Channels:   2,
			Samples:    silence[:count*2],
		}); err != nil {
			return err
		}
		w.next += count
		frames -= count
	}
	return nil
}

func durationToSampleCount(duration time.Duration) int64 {
	seconds := int64(duration / time.Second)
	remainder := int64(duration % time.Second)
	return seconds*dvdLPCMSampleRate + remainder*dvdLPCMSampleRate/int64(time.Second)
}

func addMediaOffsets(left, right time.Duration) (time.Duration, bool) {
	maxDuration := time.Duration(math.MaxInt64)
	minDuration := time.Duration(math.MinInt64)
	if right > 0 && left > maxDuration-right || right < 0 && left < minDuration-right {
		return 0, false
	}
	return left + right, true
}
