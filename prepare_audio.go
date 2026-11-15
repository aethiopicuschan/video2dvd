package mp4todvd

import (
	"context"
	"errors"
	"io"
)

// PrepareMP4AudioForDVD decodes the selected AAC-LC or AC-3 track to 48 kHz
// stereo and writes big-endian 16-bit DVD LPCM audio essence. The destination
// is not a VOB audio PES stream; an authorer still needs to packetize it and
// add the DVD LPCM private-stream headers.
func PrepareMP4AudioForDVD(ctx context.Context, path string, trackIndex *int, dst io.Writer) error {
	if dst == nil {
		return errors.New("mp4todvd: DVD LPCM writer is required")
	}
	return DecodeMP4Audio(ctx, path, trackIndex, func(chunk PCMChunk) error {
		return WriteDVDLPCM(dst, chunk)
	})
}
