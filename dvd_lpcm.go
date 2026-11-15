package video2dvd

import (
	"encoding/binary"
	"errors"
	"io"
)

// WriteDVDLPCM writes the audio essence for a DVD 48 kHz, 16-bit, two-channel
// LPCM stream. Samples are signed PCM values interleaved left/right and are
// written big-endian as required by DVD LPCM. A DVD muxer must still wrap this
// essence in private_stream_1 PES packets and add each packet's LPCM headers.
func WriteDVDLPCM(dst io.Writer, chunk PCMChunk) error {
	if dst == nil {
		return errors.New("video2dvd: LPCM writer is required")
	}
	if chunk.SampleRate != 48000 || chunk.Channels != 2 {
		return errors.New("video2dvd: DVD LPCM requires 48 kHz stereo PCM")
	}
	if len(chunk.Samples)%2 != 0 {
		return errors.New("video2dvd: interleaved stereo PCM must contain complete sample pairs")
	}
	const samplesPerBuffer = 4096
	buffer := make([]byte, samplesPerBuffer*2)
	for offset := 0; offset < len(chunk.Samples); {
		count := min(len(chunk.Samples)-offset, samplesPerBuffer)
		for i, sample := range chunk.Samples[offset : offset+count] {
			binary.BigEndian.PutUint16(buffer[i*2:], uint16(sample))
		}
		if err := writeAll(dst, buffer[:count*2]); err != nil {
			return err
		}
		offset += count
	}
	return nil
}

func writeAll(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("video2dvd: writer returned an invalid byte count")
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
