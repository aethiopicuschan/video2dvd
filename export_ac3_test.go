package video2dvd

// Test-only exports for the AC-3 encoder. They live apart from
// export_test.go so that unrelated shims do not collide.

import "io"

const (
	ExportAC3SamplesPerFrame = ac3SamplesPerFrame
	ExportAC3EncoderDelay    = ac3EncoderDelay
)

var (
	ExportAC3Bitrates     = ac3Bitrates
	ExportAC3FrameBytes   = ac3FrameBytes
	ExportAC3OutputFrames = ac3OutputFrames
	ExportAC3LogAdd       = ac3LogAdd
)

// ExportNewAC3Writer returns the encoder as a plain io.WriteCloser.
func ExportNewAC3Writer(dst io.Writer, bitrate int) (io.WriteCloser, error) {
	return newAC3Writer(dst, bitrate)
}

// ExportAC3MDCT runs the fast forward transform on 512 samples and returns
// its window too, so that a test can compare it with the textbook sum.
func ExportAC3MDCT(input []float64) (coefficients [256]float64, window [512]float64) {
	var transform ac3MDCT
	transform.init()
	transform.forward(input, &coefficients)
	return coefficients, transform.window
}
