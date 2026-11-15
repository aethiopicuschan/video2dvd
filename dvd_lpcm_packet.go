package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

const (
	dvdLPCMSamplesPerFrame  = 80
	dvdLPCMFramesPerPacket  = 6
	dvdLPCMPrivateHeaderLen = 7
)

// DVDLPCMPacketWriter packetizes 48 kHz, 16-bit stereo PCM into DVD LPCM
// private_stream_1 PES packets and writes them as 2048-byte program-stream
// sectors. It does not add NAV packs or DVD navigation files.
type DVDLPCMPacketWriter struct {
	program   *MPEGProgramStreamWriter
	substream byte
	start     time.Duration
	haveStart bool
	input     int64 // stereo sample frames received
	emitted   int64 // stereo sample frames encoded, including final padding
	pending   []int16
	closed    bool
	writeErr  error
}

// NewDVDLPCMPacketWriter creates a writer for LPCM audio stream 0 through 7.
func NewDVDLPCMPacketWriter(dst io.Writer, streamNumber uint8) (*DVDLPCMPacketWriter, error) {
	if streamNumber > 7 {
		return nil, errors.New("video2dvd: DVD LPCM stream number must be between 0 and 7")
	}
	program, err := NewMPEGProgramStreamWriter(dst)
	if err != nil {
		return nil, err
	}
	return &DVDLPCMPacketWriter{program: program, substream: 0xa0 + streamNumber}, nil
}

// WriteChunk adds contiguous interleaved stereo samples. Chunk timestamps must
// continue the same 48 kHz sample timeline.
func (w *DVDLPCMPacketWriter) WriteChunk(chunk PCMChunk) error {
	if w == nil || w.program == nil {
		return errors.New("video2dvd: DVD LPCM packet writer is not configured")
	}
	if w.closed {
		return errors.New("video2dvd: DVD LPCM packet writer is already closed")
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	if chunk.SampleRate != 48000 || chunk.Channels != 2 {
		return errors.New("video2dvd: DVD LPCM packet writer requires 48 kHz stereo PCM")
	}
	if chunk.Start < 0 {
		return errors.New("video2dvd: DVD LPCM chunk timestamp cannot be negative")
	}
	if len(chunk.Samples)%2 != 0 {
		return errors.New("video2dvd: interleaved stereo PCM must contain complete sample pairs")
	}
	if len(chunk.Samples) == 0 {
		return nil
	}
	if !w.haveStart {
		w.start = chunk.Start
		w.haveStart = true
	} else {
		expectedOffset, err := dvdLPCMSamplesDuration(w.input)
		if err != nil {
			return err
		}
		if expectedOffset > time.Duration(math.MaxInt64)-w.start {
			return errors.New("video2dvd: DVD LPCM timeline overflows duration")
		}
		expected := w.start + expectedOffset
		if absDuration(chunk.Start-expected) > time.Second/48000 {
			return fmt.Errorf("video2dvd: DVD LPCM chunks are not contiguous at sample %d", w.input)
		}
	}

	added := int64(len(chunk.Samples) / 2)
	if added > math.MaxInt64-w.input {
		return errors.New("video2dvd: DVD LPCM sample count overflows")
	}
	packetSamples := dvdLPCMFramesPerPacket * dvdLPCMSamplesPerFrame * 2
	for offset := 0; offset < len(chunk.Samples); {
		needed := packetSamples - len(w.pending)
		count := min(len(chunk.Samples)-offset, needed)
		w.pending = append(w.pending, chunk.Samples[offset:offset+count]...)
		offset += count
		if len(w.pending) == packetSamples {
			if err := w.writePacket(dvdLPCMFramesPerPacket); err != nil {
				w.writeErr = fmt.Errorf("video2dvd: write DVD LPCM packet: %w", err)
				return w.writeErr
			}
		}
	}
	w.input += added
	return nil
}

// Close pads a final partial 80-sample LPCM frame with silence and flushes it.
// It does not close the destination writer.
func (w *DVDLPCMPacketWriter) Close() error {
	if w == nil || w.program == nil {
		return errors.New("video2dvd: DVD LPCM packet writer is not configured")
	}
	if w.closed {
		return w.writeErr
	}
	w.closed = true
	if w.writeErr != nil {
		return w.writeErr
	}
	if len(w.pending) == 0 {
		return nil
	}
	sampleFrames := len(w.pending) / 2
	frames := (sampleFrames + dvdLPCMSamplesPerFrame - 1) / dvdLPCMSamplesPerFrame
	paddedSamples := frames * dvdLPCMSamplesPerFrame * 2
	w.pending = append(w.pending, make([]int16, paddedSamples-len(w.pending))...)
	if err := w.writePacket(frames); err != nil {
		w.writeErr = fmt.Errorf("video2dvd: flush DVD LPCM packet: %w", err)
	}
	return w.writeErr
}

func (w *DVDLPCMPacketWriter) writePacket(frameCount int) error {
	if frameCount < 1 || frameCount > dvdLPCMFramesPerPacket || len(w.pending) != frameCount*dvdLPCMSamplesPerFrame*2 {
		return errors.New("video2dvd: invalid DVD LPCM packet frame count")
	}
	if w.emitted%dvdLPCMSamplesPerFrame != 0 {
		return errors.New("video2dvd: DVD LPCM packet is not aligned to an audio frame")
	}
	offset, err := dvdLPCMSamplesDuration(w.emitted)
	if err != nil || offset > time.Duration(math.MaxInt64)-w.start {
		return errors.New("video2dvd: DVD LPCM packet timestamp overflows duration")
	}
	pts := w.start + offset
	payload := make([]byte, dvdLPCMPrivateHeaderLen+len(w.pending)*2)
	payload[0] = w.substream
	payload[1] = byte(frameCount)
	binary.BigEndian.PutUint16(payload[2:4], 0) // first audio frame starts after this header.
	payload[4] = byte((w.emitted / dvdLPCMSamplesPerFrame) % 32)
	payload[5] = 0x01 // 16-bit, 48 kHz, two channels.
	payload[6] = 0x80 // neutral dynamic range control.
	for i, sample := range w.pending {
		binary.BigEndian.PutUint16(payload[dvdLPCMPrivateHeaderLen+i*2:], uint16(sample))
	}
	if int64(frameCount*dvdLPCMSamplesPerFrame) > math.MaxInt64-w.emitted {
		return errors.New("video2dvd: DVD LPCM sample count overflows")
	}
	if err := w.program.WritePES(0xbd, pts, payload); err != nil {
		return err
	}
	w.emitted += int64(frameCount * dvdLPCMSamplesPerFrame)
	w.pending = w.pending[:0]
	return nil
}

func dvdLPCMSamplesDuration(samples int64) (time.Duration, error) {
	if samples < 0 {
		return 0, errors.New("video2dvd: DVD LPCM sample count cannot be negative")
	}
	seconds := samples / 48000
	remainder := samples % 48000
	if seconds > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("video2dvd: DVD LPCM timeline overflows duration")
	}
	wholeSeconds := seconds * int64(time.Second)
	fractionalSecond := remainder * int64(time.Second) / 48000
	if fractionalSecond > math.MaxInt64-wholeSeconds {
		return 0, errors.New("video2dvd: DVD LPCM timeline overflows duration")
	}
	return time.Duration(wholeSeconds + fractionalSecond), nil
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
