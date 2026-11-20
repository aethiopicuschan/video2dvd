package video2dvd

import (
	"io"
	"time"
)

// editHiddenReaderAt presents an MP4 with one track's "edts" box renamed to
// "free". The audio decoders refuse edit lists that are not sample-exact,
// which millisecond movie timescales rarely are, so the edit is applied here
// with rounding instead.
type editHiddenReaderAt struct {
	src        io.ReaderAt
	typeOffset int64
}

func hideMP4EditList(src io.ReaderAt, edit mp4EditList) io.ReaderAt {
	if edit.typeOffset == 0 {
		return src
	}
	return &editHiddenReaderAt{src: src, typeOffset: edit.typeOffset}
}

func (r *editHiddenReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.src.ReadAt(p, off)
	replacement := [4]byte{'f', 'r', 'e', 'e'}
	for i := range replacement {
		position := r.typeOffset + int64(i) - off
		if position >= 0 && position < int64(n) {
			p[position] = replacement[i]
		}
	}
	return n, err
}

// editedPCMWriter maps PCM decoded from a track's whole media timeline onto
// the movie timeline: it drops the samples before the edit's media time,
// prepends silence for a leading empty edit, and stops at the edit's end.
type editedPCMWriter struct {
	handler func(PCMChunk) error
	skip    int64
	leading int64
	limit   int64 // media frames to deliver; negative means unlimited
	emitted int64
}

func newEditedPCMWriter(edit mp4EditList, handler func(PCMChunk) error) *editedPCMWriter {
	w := &editedPCMWriter{handler: handler, limit: -1}
	if !edit.present {
		return w
	}
	w.skip = durationToSampleCount(edit.mediaTime)
	w.leading = durationToSampleCount(edit.empty)
	if edit.segment > 0 {
		w.limit = durationToSampleCount(edit.segment)
	}
	return w
}

func (w *editedPCMWriter) WriteChunk(chunk PCMChunk) error {
	samples := chunk.Samples
	if w.skip > 0 {
		drop := min(w.skip, int64(len(samples)/2))
		samples = samples[drop*2:]
		w.skip -= drop
	}
	if len(samples) == 0 {
		return nil
	}
	const silenceFrames = 4096
	for w.leading > 0 {
		count := min(w.leading, int64(silenceFrames))
		if err := w.emit(make([]int16, count*2)); err != nil {
			return err
		}
		w.leading -= count
	}
	if w.limit >= 0 {
		keep := min(w.limit, int64(len(samples)/2))
		samples = samples[:keep*2]
		w.limit -= keep
	}
	if len(samples) == 0 {
		return nil
	}
	return w.emit(samples)
}

func (w *editedPCMWriter) emit(samples []int16) error {
	chunk := PCMChunk{
		Start:      time.Duration(w.emitted) * time.Second / time.Duration(dvdLPCMSampleRate),
		SampleRate: int(dvdLPCMSampleRate),
		Channels:   2,
		Samples:    samples,
	}
	w.emitted += int64(len(samples) / 2)
	return w.handler(chunk)
}
