package video2dvd

// Test-only exports let external-package tests exercise internal helpers
// without adding them to the library API.
import (
	"io"
	"os"
	"time"

	"github.com/aethiopicuschan/go-264/audio/pcm"
)

type ExportAACPCMFileSource = aacPCMFileSource
type TestDVDVOBURecord = dvdVOBURecord
type ExportMPEG2PictureReader = mpeg2PictureReader
type TestMP4Box = mp4Box
type TestBitReader = bitReader

var (
	TestMakeAACADTSHeader         = makeAACADTSHeader
	ExportTrackDisplayAspect      = trackDisplayAspect
	ExportCheckEstimatedTitleSize = func(duration time.Duration, capacity int64, videoBitrate int) error {
		return checkEstimatedTitleSize(duration, capacity, videoBitrate, dvdLPCMBitrate/8)
	}
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
func ExportMakeDVDMenuButtons(start, end int, profile Profile, page, pageCount, titleLaunchBase int) []DVDNAVButton {
	return makeDVDMenuButtons(start, end, profile, page, pageCount, titleLaunchBase)
}
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
func ExportMPEG2FrameBytes(e *MPEG2VideoEncoder) int         { return int(e.rate.bitsPerPicture / 8) }
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
func ExportMPEG2IntraPictureSize(e *MPEG2VideoEncoder, f DecodedVideoFrame, blocks []mpeg2IntraBlock, w, h, q int, ac bool) int {
	return e.measureIntraPicture(f, blocks, w, h, q, ac)
}
func ExportMPEG2PredictivePicture(e *MPEG2VideoEncoder, macroblocks []mpeg2PredictiveMacroblock, w, h, q int, ac bool) []byte {
	return e.encodePredictivePicture(macroblocks, w, h, q, ac)
}
func ExportMPEG2PredictivePictureSize(e *MPEG2VideoEncoder, macroblocks []mpeg2PredictiveMacroblock, w, h, q int, ac bool) int {
	return e.measurePredictivePicture(macroblocks, w, h, q, ac)
}
func ExportMPEG2DCTRunLevel(run, level int, firstNonIntra bool) []byte {
	var bits mpeg2BitWriter
	writeMPEG2DCTRunLevel(&bits, run, level, firstNonIntra)
	return bits.bytes()
}
func ExportMPEG2MotionSample(plane []byte, stride, x, y, motionX, motionY int) byte {
	return mpeg2MotionSample(plane, stride, x, y, motionX, motionY)
}
func ExportMPEG2EncoderReference(e *MPEG2VideoEncoder) DecodedVideoFrame { return *e.reference }
func ExportSetMPEG2VBVFullness(e *MPEG2VideoEncoder, bits float64)       { e.rate.fullness = bits }
func ExportMPEG2MacroblockMotion(m mpeg2PredictiveMacroblock) (int, int) { return m.motionX, m.motionY }

const (
	ExportMPEG2MuxSafeVideoBitrate = mpeg2MuxSafeVideoBitrate
	ExportMPEG2VBVStartBits        = mpeg2VBVStartBits
	ExportMPEG2VBVBufferBits       = mpeg2VBVBufferBits
	ExportMPEG2VBVGuardBits        = mpeg2VBVGuardBits
)

func ExportProgramStreamClosedError(w *MPEGProgramStreamWriter) error { return w.closedErr }

type ExportPCMWindowWriter = pcmWindowWriter

func ExportNewPCMWindowWriter(dst io.Writer, endSample int64, offset time.Duration) (*pcmWindowWriter, error) {
	return newPCMWindowWriterSamples(dst, endSample, offset)
}

// ExportEditPCM runs chunks through the MP4 edit-list mapper.
func ExportEditPCM(empty, mediaTime, segment time.Duration, handler func(PCMChunk) error) func(PCMChunk) error {
	edit := mp4EditList{present: true, empty: empty, mediaTime: mediaTime, segment: segment}
	return newEditedPCMWriter(edit, handler).WriteChunk
}

func ExportHideMP4EditList(src io.ReaderAt, typeOffset int64) io.ReaderAt {
	return hideMP4EditList(src, mp4EditList{typeOffset: typeOffset})
}

func ExportTrackEdit(track TrackInfo) (present bool, typeOffset int64, empty, mediaTime, segment time.Duration) {
	return track.edit.present, track.edit.typeOffset, track.edit.empty, track.edit.mediaTime, track.edit.segment
}

func ExportSetFrameRateOrigin(c *DVDFrameRateConverter, origin time.Duration) { c.setOrigin(origin) }

// ExportTitleLengthFromFrames is the title and audio length of a number of
// whole frames.
func ExportTitleLengthFromFrames(frames int64, fpsN, fpsD int) (time.Duration, int64, error) {
	return titleLengthFromFields(2*frames, fpsN, fpsD)
}

var (
	ExportValidateInputAudioChannels = validateInputAudioChannels
)

// ExportDecodePCMWithFallback is DecodeMP4Audio's decoder switch with both
// decoders supplied by the test.
func ExportDecodePCMWithFallback(primary, fallback func(func(PCMChunk) error) error, handler func(PCMChunk) error) error {
	return decodePCMWithFallback(primary, fallback, handler)
}

// ExportDecodeMP4AudioPrimary and ExportDecodeMP4AACFallback run one of
// DecodeMP4Audio's two decoders alone.
var (
	ExportDecodeMP4AudioPrimary = decodeMP4AudioPrimary
	ExportDecodeMP4AACFallback  = decodeMP4AACFallback
)
