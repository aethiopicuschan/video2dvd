package video2dvd

// Test-only exports let external-package tests exercise internal helpers
// without adding them to the library API.
import (
	"io"
	"os"

	"github.com/rcarmo/go-264/audio/pcm"
)

type ExportAACPCMFileSource = aacPCMFileSource
type TestDVDVOBURecord = dvdVOBURecord
type ExportMPEG2PictureReader = mpeg2PictureReader
type TestMP4Box = mp4Box
type TestBitReader = bitReader

var (
	TestMakeAACADTSHeader                   = makeAACADTSHeader
	ExportTrackDisplayAspect                = trackDisplayAspect
	ExportCheckEstimatedTitleSize           = checkEstimatedTitleSize
	ExportBuildDVDVMGMenuPGCI               = buildDVDVMGMenuPGCI
	ExportDVDCellElapsedTime                = dvdCellElapsedTime
	ExportSelectAudioTrack                  = selectAudioTrack
	ExportSelectWebMVideoTrack              = selectWebMVideoTrack
	ExportParseMediaDuration                = parseMediaDuration
	ExportCopyVP9Frame                      = copyVP9Frame
	ExportCloneDecodedVideoFrame            = cloneDecodedVideoFrame
	ExportCollectMPEG2IntraBlocks           = collectMPEG2IntraBlocks
	ExportAnalyzeMPEG2PredictiveMacroblocks = analyzeMPEG2PredictiveMacroblocks
	ExportReconstructMPEG2PredictiveFrame   = reconstructMPEG2PredictiveFrame
	TestNewWebMFrameDurationResolver        = newWebMFrameDurationResolver
	ExportMPEG2VideoBitrate                 = mpeg2VideoBitrate
	ExportAACFallbackDecodedFramesPerSample = aacFallbackDecodedFramesPerSample
)

func ExportProfile(options Options) (Profile, error) { return options.profile() }
func ExportAACSource(file *os.File, info pcm.Info, priming, leading int64) *aacPCMFileSource {
	return &aacPCMFileSource{file: file, info: info, priming: priming, leading: leading}
}
func ExportWriteAll(dst io.Writer, data []byte) error { return writeAll(dst, data) }
func ExportNewMPEG2PictureReader(src io.Reader) *mpeg2PictureReader {
	return &mpeg2PictureReader{src: src}
}
func ExportNewSizeLimitedWriter(dst io.Writer, limit int64) *sizeLimitedWriter {
	return &sizeLimitedWriter{dst: dst, limit: limit}
}
func ExportNewBitReader(data []byte) *bitReader              { return &bitReader{data: data} }
func ExportReadBits(r *bitReader, count int) (uint32, error) { return r.read(count) }
func ExportMP4Box(data, end int64) mp4Box                    { return mp4Box{data: data, end: end} }
func ExportMPEG2FrameBytes(e *MPEG2VideoEncoder) int         { return e.frameBytes }
func ExportMPEG2InverseIntraAC(level, weight, quantizer int) int {
	return inverseMPEG2IntraACCoefficient(level, weight, quantizer)
}
func ExportMPEG2InverseNonIntra(level, quantizer int) int {
	return inverseMPEG2NonIntraCoefficient(level, quantizer)
}
func ExportMPEG2QuantizeNonIntra(value float64, quantizer int) int {
	return quantizeMPEG2NonIntraCoefficient(value, quantizer)
}
func ExportMPEG2IntraPicture(e *MPEG2VideoEncoder, f DecodedVideoFrame, blocks []mpeg2IntraBlock, w, h, q int, ac bool) []byte {
	return e.encodeIntraPicture(f, blocks, w, h, q, ac)
}
func ExportProgramStreamClosedError(w *MPEGProgramStreamWriter) error { return w.closedErr }
