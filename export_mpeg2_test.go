package video2dvd

// Test-only exports for the MPEG-2 encoder's motion search, macroblock modes
// and GOP structure. They live apart from export_test.go so that unrelated
// shims do not collide.

type TestMPEG2MotionVector = mpeg2MotionVector
type TestMPEG2PredictiveMacroblock = mpeg2PredictiveMacroblock

const (
	ExportMPEG2MotionLow      = mpeg2MotionLow
	ExportMPEG2MotionHigh     = mpeg2MotionHigh
	ExportMPEG2MotionRSize    = mpeg2MotionRSize
	ExportMPEG2GOPPictures    = mpeg2GOPPictures
	ExportMPEG2VBVCeilingBits = mpeg2VBVCeilingBits
)

var (
	ExportMPEG2MotionBounds             = mpeg2MotionBounds
	ExportMPEG2DefaultIntraMatrix       = mpeg2DefaultIntraMatrix
	ExportAnalyzeMPEG2PredictivePicture = analyzeMPEG2PredictivePicture
)

func ExportMPEG2MacroblockIsIntra(m mpeg2PredictiveMacroblock) bool { return m.intra }

// ExportMPEG2SearchState returns what the encoder seeds its next motion
// search with: the remembered vector field and the vector-bit weight.
func ExportMPEG2SearchState(e *MPEG2VideoEncoder) ([]mpeg2MotionVector, int) {
	return append([]mpeg2MotionVector(nil), e.motion...), e.motionLambda
}

func ExportMPEG2MinGOPPictures(e *MPEG2VideoEncoder) int { return e.minGOPPictures }

// ExportMPEG2MotionVectorCode returns one coded vector component followed by
// a one bit, so that a reader can tell where the code ends.
func ExportMPEG2MotionVectorCode(motion, predictor int) []byte {
	var bits mpeg2BitWriter
	writeMPEG2MotionVector(&bits, motion, predictor)
	bits.write(1, 1)
	return bits.bytes()
}

// ExportMPEG2MacroblockAddressIncrement returns a coded address increment
// followed by a one bit.
func ExportMPEG2MacroblockAddressIncrement(increment int) []byte {
	var bits mpeg2BitWriter
	writeMPEG2MacroblockAddressIncrement(&bits, increment)
	bits.write(1, 1)
	return bits.bytes()
}

func ExportMPEG2SequenceHeader(profile Profile) []byte {
	var bits mpeg2BitWriter
	writeMPEG2SequenceHeader(&bits, profile)
	return bits.bytes()
}
