package video2dvd

import "time"

// Test-only exports for the decode, fit, colour and frame-rate stages.

var (
	ExportAVCSPSColor          = avcSPSColor
	ExportVP9ColorMatrix       = vp9ColorMatrix
	ExportCopyVP9HighBitDepth  = copyVP9HighBitDepthFrame
	ExportWebMContainerColor   = webMContainerColor
	ExportNewResampleKernel    = newResampleKernel
	ExportCachedResampleKernel = cachedResampleKernel
)

const (
	ExportResamplePhaseCentre  = resamplePhaseCentre
	ExportResamplePhaseLeft    = resamplePhaseLeft
	ExportWebMCadenceMinFrames = webMCadenceMinFrames
)

func ExportResampleKernelWeights(k *resampleKernel) (taps int, first []int, weights []int32) {
	return k.taps, k.first, k.weights
}

// ExportWebMCadence returns the timestamp regularizer of the WebM preparation
// path for a track with the given DefaultDuration.
func ExportWebMCadence(defaultDuration time.Duration) func(pts, duration time.Duration) (time.Duration, time.Duration) {
	cadence := &webMCadence{hint: defaultDuration}
	return cadence.regularize
}
