package video2dvd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
)

const (
	mpeg2VideoBitrate    = 8_000_000
	mpeg2MinVideoBitrate = 2_000_000
	mpeg2MaxVideoBitrate = 8_500_000

	// mpeg2MuxSafeVideoBitrate is the highest rate the encoder actually codes
	// at. The DVD program stream carries 10.08 Mbit/s; 48 kHz 16-bit stereo
	// LPCM occupies 1.64 Mbit/s of it as packed, and NAV packs and pack/PES headers
	// take about 0.15 Mbit/s. The remainder is headroom for rate-control
	// overshoot and for delivering large I pictures ahead of their decode
	// time. Requests above this value are accepted for compatibility and
	// coded at this rate.
	mpeg2MuxSafeVideoBitrate = 7_500_000

	mpeg2GOPPictures = 15

	// mpeg2VBVBufferBits is the buffer declared in the sequence header
	// (vbv_buffer_size 112), the largest DVD-Video allows.
	mpeg2VBVBufferBits = 112 * 16384
	// mpeg2VBVStartBits is the occupancy rate control assumes at the first
	// picture and steers back to before every I picture; mpeg2VBVCeilingBits
	// is the most it ever counts on, so unspent bits carry over only up to
	// that level. Staying below the declared size means a multiplexer only has
	// to deliver the start occupancy ahead of the first decoding time, and
	// keeps the largest picture well inside the buffer.
	mpeg2VBVStartBits   = 100 * 1024 * 8
	mpeg2VBVCeilingBits = 150 * 1024 * 8
	// mpeg2VBVGuardBits is kept in the modelled buffer after every picture to
	// absorb pack granularity and audio/NAV interleaving in the multiplexer.
	mpeg2VBVGuardBits = 6 * DVDVOBSectorSize * 8

	// An I picture may take at most this fraction of the bits left for its
	// GOP, so that a scene change after it does not starve the P pictures.
	mpeg2MaxIntraShare = 0.5
	// Share of a GOP given to the first I picture, before any P picture has
	// shown how the content codes.
	mpeg2FirstIntraShare = 0.25
	// An I picture is coded at this multiple of the previous GOP's mean P
	// quantizer scale. The intra matrix quantizes high frequencies more
	// coarsely than the flat non-intra one, and every P picture of the GOP
	// predicts from the I picture, so it is given the finer scale.
	mpeg2IntraQuantizerRatio = 0.75
)

func profileVideoBitrate(profile Profile) int {
	if profile.VideoBitrate == 0 {
		return mpeg2VideoBitrate
	}
	return profile.VideoBitrate
}

// effectiveMPEG2VideoBitrate is the rate written to the sequence header and
// enforced by rate control for the requested profile bitrate.
func effectiveMPEG2VideoBitrate(profile Profile) int {
	return min(profileVideoBitrate(profile), mpeg2MuxSafeVideoBitrate)
}

// mpeg2RateControl models the video buffering verifier (H.262 Annex C) for a
// stream delivered at the declared bit rate and derives per-picture budgets
// from its occupancy.
type mpeg2RateControl struct {
	bitsPerPicture float64
	// fullness is the modelled occupancy in bits just before the next picture
	// is removed.
	fullness float64
	// Quantizer scales of the P pictures coded since the last I picture.
	predictiveQuantizerSum   int
	predictiveQuantizerCount int
}

func newMPEG2RateControl(bitsPerPicture float64) mpeg2RateControl {
	return mpeg2RateControl{bitsPerPicture: bitsPerPicture, fullness: mpeg2VBVStartBits}
}

// pictureBudget returns, in bytes, the size to aim for and the size that must
// not be exceeded for the next picture, and the finest quantizer scale worth
// trying. remaining counts the pictures left in the GOP including this one.
func (rc *mpeg2RateControl) pictureBudget(isIntra bool, remaining int) (targetBytes, limitBytes, minQuantizer int) {
	limit := max(rc.fullness-mpeg2VBVGuardBits, 0)
	// Spend what brings the buffer back to its start level by the next
	// I picture, which is the picture that needs the reserve.
	gopBits := rc.fullness + float64(remaining)*rc.bitsPerPicture - mpeg2VBVStartBits
	target := gopBits / float64(remaining)
	minQuantizer = 1
	if isIntra {
		if rc.predictiveQuantizerCount > 0 {
			mean := float64(rc.predictiveQuantizerSum) / float64(rc.predictiveQuantizerCount)
			minQuantizer = max(1, min(31, int(mean*mpeg2IntraQuantizerRatio+0.5)))
			target = gopBits * mpeg2MaxIntraShare
		} else {
			target = gopBits * mpeg2FirstIntraShare
		}
		rc.predictiveQuantizerSum, rc.predictiveQuantizerCount = 0, 0
	}
	target = min(max(target, rc.bitsPerPicture/8), limit)
	return int(target) / 8, int(limit) / 8, minQuantizer
}

func (rc *mpeg2RateControl) pictureCoded(isIntra bool, sizeBytes, quantizer int) {
	if !isIntra {
		rc.predictiveQuantizerSum += quantizer
		rc.predictiveQuantizerCount++
	}
	rc.fullness = min(rc.fullness-float64(sizeBytes)*8+rc.bitsPerPicture, mpeg2VBVCeilingBits)
}

var mpeg2DCTBasis = buildMPEG2DCTBasis()

var mpeg2DefaultIntraMatrix = [64]int{
	8, 16, 19, 22, 26, 27, 29, 34,
	16, 16, 22, 24, 27, 29, 34, 37,
	19, 22, 26, 27, 29, 34, 34, 38,
	22, 22, 26, 27, 29, 32, 36, 40,
	22, 26, 27, 29, 32, 35, 40, 48,
	26, 27, 29, 32, 35, 40, 48, 58,
	26, 27, 29, 34, 38, 46, 56, 69,
	27, 29, 35, 38, 46, 56, 69, 83,
}

var mpeg2ZigZag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

func buildMPEG2DCTBasis() [8][8]float64 {
	var basis [8][8]float64
	for frequency := range 8 {
		normalization := 1.0
		if frequency == 0 {
			normalization = 1 / math.Sqrt2
		}
		for position := range 8 {
			basis[frequency][position] = normalization * math.Cos(float64((2*position+1)*frequency)*math.Pi/16)
		}
	}
	return basis
}

// MPEG2VideoEncoder writes a DVD-raster MPEG-2 elementary stream from planar
// YUV 4:2:0 frames. It emits closed GOPs containing I pictures and P pictures.
// P pictures use half-sample forward motion vectors and quantized DCT residuals.
//
// The writer does not close dst. Call Close after the final frame to write the
// MPEG sequence-end code.
type MPEG2VideoEncoder struct {
	dst       io.Writer
	profile   Profile
	frameRate int
	rate      mpeg2RateControl
	frameNo   int64
	reference *DecodedVideoFrame
	closed    bool
	writeErr  error
}

// NewMPEG2VideoEncoder prepares an encoder for NTSC or PAL full-raster DVD
// frames. It writes the sequence header with the first GOP. Bitrates above
// 7.5 Mbit/s are coded at 7.5 Mbit/s so that the stream still fits the DVD
// multiplex next to LPCM audio.
func NewMPEG2VideoEncoder(dst io.Writer, profile Profile) (*MPEG2VideoEncoder, error) {
	if dst == nil {
		return nil, errors.New("video2dvd: MPEG-2 destination writer is required")
	}
	if err := validateEncoderProfile(profile); err != nil {
		return nil, err
	}
	encoder := &MPEG2VideoEncoder{dst: dst, profile: profile}
	bitrate := profileVideoBitrate(profile)
	if bitrate < mpeg2MinVideoBitrate || bitrate > mpeg2MaxVideoBitrate {
		return nil, fmt.Errorf("video2dvd: video bitrate must be between %d and %d bit/s", mpeg2MinVideoBitrate, mpeg2MaxVideoBitrate)
	}
	bitrate = effectiveMPEG2VideoBitrate(profile)
	switch profile.Standard {
	case VideoStandardNTSC:
		encoder.frameRate = 30
		encoder.rate = newMPEG2RateControl(float64(bitrate) * 1001 / 30000)
	case VideoStandardPAL:
		encoder.frameRate = 25
		encoder.rate = newMPEG2RateControl(float64(bitrate) / 25)
	default:
		return nil, ErrInvalidProfile
	}
	return encoder, nil
}

// WriteFrame encodes one full-resolution planar YUV 4:2:0 frame. Frames must
// already be fitted and timestamp-resampled to the selected DVD profile.
func (e *MPEG2VideoEncoder) WriteFrame(frame DecodedVideoFrame) error {
	if e == nil || e.dst == nil {
		return errors.New("video2dvd: MPEG-2 encoder is not configured")
	}
	if e.closed {
		return errors.New("video2dvd: MPEG-2 encoder is already closed")
	}
	if e.writeErr != nil {
		return e.writeErr
	}
	if err := validateDecodedFrame(frame); err != nil {
		return err
	}
	if frame.Width != e.profile.FrameWidth || frame.Height != e.profile.FrameHeight {
		return fmt.Errorf("video2dvd: MPEG-2 encoder requires %dx%d frames", e.profile.FrameWidth, e.profile.FrameHeight)
	}
	mbWidth := frame.Width / 16
	mbHeight := frame.Height / 16
	gopPosition := int(e.frameNo % mpeg2GOPPictures)
	isIntra := e.reference == nil || gopPosition == 0
	var intraBlocks []mpeg2IntraBlock
	var predictiveMacroblocks []mpeg2PredictiveMacroblock
	if isIntra {
		intraBlocks = collectMPEG2IntraBlocks(frame, mbWidth, mbHeight)
	} else {
		predictiveMacroblocks = analyzeMPEG2PredictiveMacroblocks(frame, *e.reference, mbWidth, mbHeight)
	}
	measure := func(quantizer int) int {
		if isIntra {
			return e.measureIntraPicture(frame, intraBlocks, mbWidth, mbHeight, quantizer, true)
		}
		return e.measurePredictivePicture(predictiveMacroblocks, mbWidth, mbHeight, quantizer, true)
	}
	targetBytes, limitBytes, minQuantizer := e.rate.pictureBudget(isIntra, mpeg2GOPPictures-gopPosition)
	selectedQuantizer := selectMPEG2Quantizer(measure, targetBytes, minQuantizer)
	if selectedQuantizer == 0 {
		// Preserve low-frequency detail when even the coarsest complete picture
		// exceeds its budget. Dropping every AC coefficient causes visible
		// macroblock blur, so keep the largest zig-zag prefix that fits instead.
		selectedQuantizer = 31
		limit := fitMPEG2ACLimit(func(limit int) int {
			if isIntra {
				return e.measureIntraPicture(frame, limitMPEG2IntraAC(intraBlocks, limit), mbWidth, mbHeight, 31, true)
			}
			return e.measurePredictivePicture(limitMPEG2PredictiveAC(predictiveMacroblocks, limit), mbWidth, mbHeight, 31, true)
		}, targetBytes)
		if isIntra {
			intraBlocks = limitMPEG2IntraAC(intraBlocks, limit)
		} else {
			predictiveMacroblocks = limitMPEG2PredictiveAC(predictiveMacroblocks, limit)
			if measure(31) > limitBytes {
				// Motion vectors alone would underflow the decoder buffer:
				// repeat the reference picture instead.
				predictiveMacroblocks = make([]mpeg2PredictiveMacroblock, len(predictiveMacroblocks))
			}
		}
	}
	var encoded []byte
	if isIntra {
		encoded = e.encodeIntraPicture(frame, intraBlocks, mbWidth, mbHeight, selectedQuantizer, true)
	} else {
		encoded = e.encodePredictivePicture(predictiveMacroblocks, mbWidth, mbHeight, selectedQuantizer, true)
	}
	if err := writeAll(e.dst, encoded); err != nil {
		e.writeErr = fmt.Errorf("video2dvd: write MPEG-2 picture %d: %w", e.frameNo, err)
		return e.writeErr
	}
	e.rate.pictureCoded(isIntra, len(encoded), selectedQuantizer)
	var reference DecodedVideoFrame
	if isIntra {
		reference = reconstructMPEG2IntraFrame(frame, intraBlocks, mbWidth, mbHeight, selectedQuantizer, true)
	} else {
		reference = reconstructMPEG2PredictiveFrame(frame, *e.reference, predictiveMacroblocks, mbWidth, mbHeight, selectedQuantizer, true)
	}
	e.reference = &reference
	e.frameNo++
	return nil
}

// selectMPEG2Quantizer returns the finest quantizer scale from minQuantizer up
// whose picture fits budgetBytes, or 0 when even the coarsest does not.
// Picture size falls as the scale rises, so a bisection needs five
// measurements instead of up to 31.
func selectMPEG2Quantizer(measure func(quantizer int) int, budgetBytes, minQuantizer int) int {
	low, high, best := minQuantizer, 31, 0
	for low <= high {
		quantizer := low + (high-low)/2
		if measure(quantizer) <= budgetBytes {
			best = quantizer
			high = quantizer - 1
		} else {
			low = quantizer + 1
		}
	}
	return best
}

// fitMPEG2ACLimit returns the longest zig-zag coefficient prefix whose picture
// fits budgetBytes at the coarsest quantizer, or 1 when none does.
func fitMPEG2ACLimit(measure func(coefficientLimit int) int, budgetBytes int) int {
	low, high, best := 1, 64, 1
	for low <= high {
		limit := low + (high-low)/2
		if measure(limit) <= budgetBytes {
			best = limit
			low = limit + 1
		} else {
			high = limit - 1
		}
	}
	return best
}

func limitMPEG2IntraAC(blocks []mpeg2IntraBlock, coefficientLimit int) []mpeg2IntraBlock {
	if coefficientLimit >= 64 {
		return blocks
	}
	limited := append([]mpeg2IntraBlock(nil), blocks...)
	for blockIndex := range limited {
		for scanPosition := max(1, coefficientLimit); scanPosition < 64; scanPosition++ {
			limited[blockIndex].coeff[mpeg2ZigZag[scanPosition]] = 0
		}
	}
	return limited
}

func limitMPEG2PredictiveAC(macroblocks []mpeg2PredictiveMacroblock, coefficientLimit int) []mpeg2PredictiveMacroblock {
	if coefficientLimit >= 64 {
		return macroblocks
	}
	limited := append([]mpeg2PredictiveMacroblock(nil), macroblocks...)
	for macroblockIndex := range limited {
		for blockIndex := range limited[macroblockIndex].blocks {
			for scanPosition := coefficientLimit; scanPosition < 64; scanPosition++ {
				limited[macroblockIndex].blocks[blockIndex].coeff[mpeg2ZigZag[scanPosition]] = 0
			}
		}
	}
	return limited
}

func (e *MPEG2VideoEncoder) encodeIntraPicture(frame DecodedVideoFrame, blocks []mpeg2IntraBlock, mbWidth, mbHeight, quantizer int, includeAC bool) []byte {
	var bits mpeg2BitWriter
	e.writeIntraPictureHeader(&bits)
	encoded, _ := encodeMPEG2PictureRows(&bits, mbHeight, func(rowBits *mpeg2BitWriter, row int) {
		writeMPEG2IntraSlice(rowBits, blocks, row, mbWidth, quantizer, includeAC)
	})
	return encoded
}

func (e *MPEG2VideoEncoder) measureIntraPicture(frame DecodedVideoFrame, blocks []mpeg2IntraBlock, mbWidth, mbHeight, quantizer int, includeAC bool) int {
	bits := mpeg2BitWriter{countOnly: true}
	e.writeIntraPictureHeader(&bits)
	_, size := encodeMPEG2PictureRows(&bits, mbHeight, func(rowBits *mpeg2BitWriter, row int) {
		writeMPEG2IntraSlice(rowBits, blocks, row, mbWidth, quantizer, includeAC)
	})
	return size
}

func (e *MPEG2VideoEncoder) writeIntraPictureHeader(bits *mpeg2BitWriter) {
	if e.frameNo%mpeg2GOPPictures == 0 {
		writeMPEG2SequenceHeader(bits, e.profile)
		writeMPEG2GOPHeader(bits, e.frameNo, e.frameRate)
	}
	writeMPEG2PictureHeader(bits, uint16(e.frameNo%mpeg2GOPPictures), 1)
}

func (e *MPEG2VideoEncoder) encodePredictivePicture(macroblocks []mpeg2PredictiveMacroblock, mbWidth, mbHeight, quantizer int, includeAC bool) []byte {
	var bits mpeg2BitWriter
	e.writePredictivePictureHeader(&bits)
	encoded, _ := encodeMPEG2PictureRows(&bits, mbHeight, func(rowBits *mpeg2BitWriter, row int) {
		writeMPEG2PredictiveSlice(rowBits, macroblocks, row, mbWidth, quantizer, includeAC)
	})
	return encoded
}

func (e *MPEG2VideoEncoder) measurePredictivePicture(macroblocks []mpeg2PredictiveMacroblock, mbWidth, mbHeight, quantizer int, includeAC bool) int {
	bits := mpeg2BitWriter{countOnly: true}
	e.writePredictivePictureHeader(&bits)
	_, size := encodeMPEG2PictureRows(&bits, mbHeight, func(rowBits *mpeg2BitWriter, row int) {
		writeMPEG2PredictiveSlice(rowBits, macroblocks, row, mbWidth, quantizer, includeAC)
	})
	return size
}

func (e *MPEG2VideoEncoder) writePredictivePictureHeader(bits *mpeg2BitWriter) {
	writeMPEG2PictureHeader(bits, uint16(e.frameNo%mpeg2GOPPictures), 2)
}

func encodeMPEG2PictureRows(header *mpeg2BitWriter, rowCount int, writeRow func(*mpeg2BitWriter, int)) ([]byte, int) {
	var prefix []byte
	var prefixSize int
	if header.countOnly {
		header.align()
		prefixSize = header.sizeBytes()
	} else {
		prefix = header.bytes()
		prefixSize = len(prefix)
	}

	rowData := make([][]byte, rowCount)
	rowSizes := make([]int, rowCount)
	parallelMPEG2Rows(rowCount, func(row int) {
		rowBits := mpeg2BitWriter{countOnly: header.countOnly}
		writeRow(&rowBits, row)
		if header.countOnly {
			rowBits.align()
			rowSizes[row] = rowBits.sizeBytes()
			return
		}
		rowData[row] = rowBits.bytes()
		rowSizes[row] = len(rowData[row])
	})

	totalSize := prefixSize
	for _, rowSize := range rowSizes {
		totalSize += rowSize
	}
	if header.countOnly {
		return nil, totalSize
	}

	encoded := make([]byte, 0, totalSize)
	encoded = append(encoded, prefix...)
	for _, row := range rowData {
		encoded = append(encoded, row...)
	}
	return encoded, totalSize
}

// Close finishes the elementary stream. It does not close the destination.
func (e *MPEG2VideoEncoder) Close() error {
	if e == nil || e.dst == nil {
		return errors.New("video2dvd: MPEG-2 encoder is not configured")
	}
	if e.closed {
		return e.writeErr
	}
	e.closed = true
	if e.writeErr != nil {
		return e.writeErr
	}
	if e.frameNo == 0 {
		e.writeErr = errors.New("video2dvd: MPEG-2 stream contains no video frames")
		return e.writeErr
	}
	var bits mpeg2BitWriter
	bits.startCode(0xb7)
	if err := writeAll(e.dst, bits.bytes()); err != nil {
		e.writeErr = fmt.Errorf("video2dvd: finish MPEG-2 stream: %w", err)
	}
	return e.writeErr
}

func writeMPEG2SequenceHeader(bits *mpeg2BitWriter, profile Profile) {
	bits.startCode(0xb3)
	bits.write(uint64(profile.FrameWidth), 12)
	bits.write(uint64(profile.FrameHeight), 12)
	aspectCode := uint64(3)
	if profile.Aspect == AspectRatio4x3 {
		aspectCode = 2
	}
	bits.write(aspectCode, 4)
	frameRateCode := uint64(4) // 30000/1001
	if profile.Standard == VideoStandardPAL {
		frameRateCode = 3 // 25/1
	}
	bits.write(frameRateCode, 4)
	bits.write(uint64((effectiveMPEG2VideoBitrate(profile)+399)/400), 18) // Maximum video rate in units of 400 bit/s.
	bits.write(1, 1)                                                      // marker_bit
	bits.write(mpeg2VBVBufferBits/16384, 10)                              // 224 KiB video buffering verifier.
	bits.write(0, 1)                                                      // constrained_parameters_flag
	bits.write(0, 1)                                                      // load_intra_quantiser_matrix
	bits.write(0, 1)                                                      // load_non_intra_quantiser_matrix

	bits.startCode(0xb5)
	bits.write(1, 4)    // sequence_extension_identifier
	bits.write(0x48, 8) // Main Profile at Main Level
	bits.write(0, 1)    // progressive_sequence: DVD-Video flags progressive material per picture.
	bits.write(1, 2)    // 4:2:0 chroma_format
	bits.write(0, 2)    // horizontal_size_extension
	bits.write(0, 2)    // vertical_size_extension
	bits.write(0, 12)   // bit_rate_extension
	bits.write(1, 1)    // marker_bit
	bits.write(0, 8)    // vbv_buffer_size_extension
	bits.write(0, 1)    // low_delay
	bits.write(0, 2)    // frame_rate_extension_n
	bits.write(0, 5)    // frame_rate_extension_d
}

func writeMPEG2GOPHeader(bits *mpeg2BitWriter, frameNo int64, nominalRate int) {
	seconds := frameNo / int64(nominalRate)
	frame := frameNo % int64(nominalRate)
	minutes := seconds / 60
	hours := minutes / 60
	bits.startCode(0xb8)
	bits.write(0, 1) // drop_frame_flag
	bits.write(uint64(hours%24), 5)
	bits.write(uint64(minutes%60), 6)
	bits.write(1, 1) // marker_bit
	bits.write(uint64(seconds%60), 6)
	bits.write(uint64(frame), 6)
	bits.write(1, 1) // closed_gop
	bits.write(0, 1) // broken_link
}

func writeMPEG2PictureHeader(bits *mpeg2BitWriter, temporalReference uint16, pictureType uint8) {
	bits.startCode(0x00)
	bits.write(uint64(temporalReference), 10)
	bits.write(uint64(pictureType), 3)
	bits.write(0xffff, 16) // vbv_delay
	if pictureType == 2 || pictureType == 3 {
		bits.write(0, 1) // full_pel_forward_vector
		bits.write(1, 3) // forward_f_code
	}
	if pictureType == 3 {
		bits.write(0, 1) // full_pel_backward_vector
		bits.write(1, 3) // backward_f_code
	}
	bits.write(0, 1) // extra_bit_picture

	bits.startCode(0xb5)
	bits.write(8, 4) // picture_coding_extension_identifier
	for range 4 {
		bits.write(1, 4) // f_code (unused for I pictures)
	}
	bits.write(0, 2) // intra_dc_precision
	bits.write(3, 2) // frame picture
	bits.write(1, 1) // top_field_first
	bits.write(1, 1) // frame_pred_frame_dct
	bits.write(0, 1) // concealment_motion_vectors
	bits.write(0, 1) // q_scale_type
	bits.write(0, 1) // intra_vlc_format
	bits.write(0, 1) // alternate_scan
	bits.write(0, 1) // repeat_first_field
	bits.write(1, 1) // chroma_420_type
	bits.write(1, 1) // progressive_frame
	bits.write(0, 1) // composite_display_flag
}

type mpeg2IntraBlock struct {
	dc     int
	coeff  [64]float64
	chroma bool
}

type mpeg2ResidualBlock struct {
	coeff [64]float64
}

type mpeg2PredictiveMacroblock struct {
	motionX int
	motionY int
	blocks  [6]mpeg2ResidualBlock
}

func analyzeMPEG2PredictiveMacroblocks(frame, reference DecodedVideoFrame, mbWidth, mbHeight int) []mpeg2PredictiveMacroblock {
	macroblocks := make([]mpeg2PredictiveMacroblock, mbWidth*mbHeight)
	parallelMPEG2Rows(mbHeight, func(row int) {
		for column := range mbWidth {
			motionX, motionY := estimateMPEG2Motion(frame, reference, column*16, row*16)
			macroblock := &macroblocks[row*mbWidth+column]
			macroblock.motionX, macroblock.motionY = motionX, motionY
			baseX, baseY := column*16, row*16
			for block := range 4 {
				x := baseX + (block%2)*8
				y := baseY + (block/2)*8
				macroblock.blocks[block] = analyzeMPEG2ResidualBlock(
					frame.Y, reference.Y, frame.Width, x, y, 8, 8, motionX, motionY,
				)
			}
			chromaMotionX, chromaMotionY := halfMotionVector(motionX), halfMotionVector(motionY)
			macroblock.blocks[4] = analyzeMPEG2ResidualBlock(
				frame.U, reference.U, frame.Width/2, column*8, row*8, 8, 8, chromaMotionX, chromaMotionY,
			)
			macroblock.blocks[5] = analyzeMPEG2ResidualBlock(
				frame.V, reference.V, frame.Width/2, column*8, row*8, 8, 8, chromaMotionX, chromaMotionY,
			)
		}
	})
	return macroblocks
}

func parallelMPEG2Rows(rows int, process func(int)) {
	workers := min(runtime.GOMAXPROCS(0), rows)
	if workers <= 1 {
		for row := range rows {
			process(row)
		}
		return
	}
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := range workers {
		start := rows * worker / workers
		end := rows * (worker + 1) / workers
		go func() {
			defer wait.Done()
			for row := start; row < end; row++ {
				process(row)
			}
		}()
	}
	wait.Wait()
}

func estimateMPEG2Motion(frame, reference DecodedVideoFrame, x, y int) (int, int) {
	if sameMPEG2LumaMacroblock(frame, reference, x, y) {
		return 0, 0
	}
	bestX, bestY := 0, 0
	bestCost := mpeg2MotionCost(frame, reference, x, y, 0, 0, math.MaxInt64)
	for motionY := -16; motionY <= 8; motionY += 8 {
		for motionX := -16; motionX <= 8; motionX += 8 {
			if motionX == 0 && motionY == 0 || !validMPEG2Motion(reference, x, y, motionX, motionY) {
				continue
			}
			cost := mpeg2MotionCost(frame, reference, x, y, motionX, motionY, bestCost)
			if cost < bestCost {
				bestX, bestY, bestCost = motionX, motionY, cost
			}
		}
	}
	coarseX, coarseY := bestX, bestY
	for motionY := max(-16, coarseY-4); motionY <= min(15, coarseY+4); motionY++ {
		for motionX := max(-16, coarseX-4); motionX <= min(15, coarseX+4); motionX++ {
			if motionX == coarseX && motionY == coarseY || !validMPEG2Motion(reference, x, y, motionX, motionY) {
				continue
			}
			cost := mpeg2MotionCost(frame, reference, x, y, motionX, motionY, bestCost)
			if cost < bestCost {
				bestX, bestY, bestCost = motionX, motionY, cost
			}
		}
	}
	return bestX, bestY
}

func validMPEG2Motion(reference DecodedVideoFrame, x, y, motionX, motionY int) bool {
	return validMPEG2MotionRegion(reference.Width, reference.Height, x, y, 16, 16, motionX, motionY) &&
		validMPEG2MotionRegion(reference.Width/2, reference.Height/2, x/2, y/2, 8, 8, halfMotionVector(motionX), halfMotionVector(motionY))
}

func validMPEG2MotionRegion(width, height, x, y, blockWidth, blockHeight, motionX, motionY int) bool {
	integerX, fractionX := halfMotionParts(motionX)
	integerY, fractionY := halfMotionParts(motionY)
	left, top := x+integerX, y+integerY
	right, bottom := left+blockWidth-1+fractionX, top+blockHeight-1+fractionY
	return left >= 0 && top >= 0 && right < width && bottom < height
}

func sameMPEG2LumaMacroblock(first, second DecodedVideoFrame, x, y int) bool {
	for row := range 16 {
		start := (y+row)*first.Width + x
		if !bytes.Equal(first.Y[start:start+16], second.Y[start:start+16]) {
			return false
		}
	}
	return true
}

func mpeg2MotionCost(frame, reference DecodedVideoFrame, x, y, motionX, motionY int, limit int64) int64 {
	integerX, fractionX := halfMotionParts(motionX)
	integerY, fractionY := halfMotionParts(motionY)
	left, top := x+integerX, y+integerY
	switch {
	case fractionX == 0 && fractionY == 0:
		return mpeg2IntegerMotionCost(frame.Y, reference.Y, frame.Width, x, y, left, top, limit)
	case fractionX != 0 && fractionY == 0:
		return mpeg2HorizontalHalfMotionCost(frame.Y, reference.Y, frame.Width, x, y, left, top, limit)
	case fractionX == 0 && fractionY != 0:
		return mpeg2VerticalHalfMotionCost(frame.Y, reference.Y, frame.Width, x, y, left, top, limit)
	default:
		return mpeg2DiagonalHalfMotionCost(frame.Y, reference.Y, frame.Width, x, y, left, top, limit)
	}
}

func mpeg2IntegerMotionCost(current, reference []byte, stride, x, y, left, top int, limit int64) int64 {
	var cost int64
	for row := range 16 {
		currentRow := (y+row)*stride + x
		referenceRow := (top+row)*stride + left
		for column := range 16 {
			cost += int64(absInt(int(current[currentRow+column]) - int(reference[referenceRow+column])))
			if cost > limit {
				return cost
			}
		}
	}
	return cost
}

func mpeg2HorizontalHalfMotionCost(current, reference []byte, stride, x, y, left, top int, limit int64) int64 {
	var cost int64
	for row := range 16 {
		currentRow := (y+row)*stride + x
		referenceRow := (top+row)*stride + left
		for column := range 16 {
			predicted := (int(reference[referenceRow+column]) + int(reference[referenceRow+column+1]) + 1) / 2
			cost += int64(absInt(int(current[currentRow+column]) - predicted))
			if cost > limit {
				return cost
			}
		}
	}
	return cost
}

func mpeg2VerticalHalfMotionCost(current, reference []byte, stride, x, y, left, top int, limit int64) int64 {
	var cost int64
	for row := range 16 {
		currentRow := (y+row)*stride + x
		referenceRow := (top+row)*stride + left
		nextReferenceRow := referenceRow + stride
		for column := range 16 {
			predicted := (int(reference[referenceRow+column]) + int(reference[nextReferenceRow+column]) + 1) / 2
			cost += int64(absInt(int(current[currentRow+column]) - predicted))
			if cost > limit {
				return cost
			}
		}
	}
	return cost
}

func mpeg2DiagonalHalfMotionCost(current, reference []byte, stride, x, y, left, top int, limit int64) int64 {
	var cost int64
	for row := range 16 {
		currentRow := (y+row)*stride + x
		referenceRow := (top+row)*stride + left
		nextReferenceRow := referenceRow + stride
		for column := range 16 {
			predicted := (int(reference[referenceRow+column]) + int(reference[referenceRow+column+1]) +
				int(reference[nextReferenceRow+column]) + int(reference[nextReferenceRow+column+1]) + 2) / 4
			cost += int64(absInt(int(current[currentRow+column]) - predicted))
			if cost > limit {
				return cost
			}
		}
	}
	return cost
}

func analyzeMPEG2ResidualBlock(current, reference []byte, stride, x, y, blockWidth, blockHeight, motionX, motionY int) mpeg2ResidualBlock {
	var block mpeg2ResidualBlock
	var source, temporary [8][8]float64
	for row := range blockHeight {
		for column := range blockWidth {
			position := (y+row)*stride + x + column
			predicted := mpeg2MotionSample(reference, stride, x+column, y+row, motionX, motionY)
			source[row][column] = float64(int(current[position]) - int(predicted))
		}
	}
	for row := range 8 {
		for horizontal := range 8 {
			var value float64
			for column := range 8 {
				value += source[row][column] * mpeg2DCTBasis[horizontal][column]
			}
			temporary[row][horizontal] = value
		}
	}
	for vertical := range 8 {
		for horizontal := range 8 {
			var value float64
			for row := range 8 {
				value += temporary[row][horizontal] * mpeg2DCTBasis[vertical][row]
			}
			block.coeff[vertical*8+horizontal] = value * 0.25
		}
	}
	return block
}

func mpeg2MotionSample(plane []byte, stride, x, y, motionX, motionY int) byte {
	integerX, fractionX := halfMotionParts(motionX)
	integerY, fractionY := halfMotionParts(motionY)
	left, top := x+integerX, y+integerY
	first := int(plane[top*stride+left])
	if fractionX == 0 && fractionY == 0 {
		return byte(first)
	}
	// H.262 7.6.4 rounds half-sample averages half up; truncating here would
	// make every decoder drift from the encoder's reference.
	if fractionX != 0 && fractionY == 0 {
		return byte((first + int(plane[top*stride+left+1]) + 1) / 2)
	}
	if fractionX == 0 && fractionY != 0 {
		return byte((first + int(plane[(top+1)*stride+left]) + 1) / 2)
	}
	return byte((first + int(plane[top*stride+left+1]) + int(plane[(top+1)*stride+left]) + int(plane[(top+1)*stride+left+1]) + 2) / 4)
}

func halfMotionParts(vector int) (integer, fraction int) {
	integer = vector / 2
	if vector < 0 && vector%2 != 0 {
		integer--
	}
	fraction = vector - integer*2
	return integer, fraction
}

func halfMotionVector(vector int) int {
	return vector / 2
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func collectMPEG2IntraBlocks(frame DecodedVideoFrame, mbWidth, mbHeight int) []mpeg2IntraBlock {
	blocks := make([]mpeg2IntraBlock, mbWidth*mbHeight*6)
	parallelMPEG2Rows(mbHeight, func(mbRow int) {
		for mbColumn := range mbWidth {
			baseX, baseY := mbColumn*16, mbRow*16
			blockIndex := (mbRow*mbWidth + mbColumn) * 6
			for block := range 4 {
				x := baseX + (block%2)*8
				y := baseY + (block/2)*8
				blocks[blockIndex+block] = analyzeMPEG2Block(frame.Y, frame.Width, x, y, false)
			}
			blocks[blockIndex+4] = analyzeMPEG2Block(frame.U, frame.Width/2, mbColumn*8, mbRow*8, true)
			blocks[blockIndex+5] = analyzeMPEG2Block(frame.V, frame.Width/2, mbColumn*8, mbRow*8, true)
		}
	})
	return blocks
}

func reconstructMPEG2IntraFrame(frame DecodedVideoFrame, blocks []mpeg2IntraBlock, mbWidth, mbHeight, quantizer int, includeAC bool) DecodedVideoFrame {
	reconstructed := newMPEG2FrameLike(frame)
	parallelMPEG2Rows(mbHeight, func(row int) {
		for column := range mbWidth {
			blockIndex := (row*mbWidth + column) * 6
			baseX, baseY := column*16, row*16
			for block := range 4 {
				x := baseX + (block%2)*8
				y := baseY + (block/2)*8
				pixels := reconstructMPEG2IntraBlock(blocks[blockIndex+block], quantizer, includeAC)
				copyMPEG2Block(reconstructed.Y, frame.Width, x, y, pixels)
			}
			pixelsU := reconstructMPEG2IntraBlock(blocks[blockIndex+4], quantizer, includeAC)
			pixelsV := reconstructMPEG2IntraBlock(blocks[blockIndex+5], quantizer, includeAC)
			copyMPEG2Block(reconstructed.U, frame.Width/2, column*8, row*8, pixelsU)
			copyMPEG2Block(reconstructed.V, frame.Width/2, column*8, row*8, pixelsV)
		}
	})
	return reconstructed
}

func reconstructMPEG2IntraBlock(block mpeg2IntraBlock, quantizer int, includeAC bool) [64]byte {
	var coefficients [64]int
	// MPEG-2's intra DC predictor is absolute (initialized to 128), while
	// the transform operates on samples centered around 128.
	coefficients[0] = (block.dc - 128) * 8
	if includeAC {
		for scanPosition := 1; scanPosition < 64; scanPosition++ {
			index := mpeg2ZigZag[scanPosition]
			weight := mpeg2DefaultIntraMatrix[index]
			level := quantizeMPEG2IntraCoefficient(block.coeff[index], quantizer, weight)
			coefficients[index] = inverseMPEG2IntraACCoefficient(level, weight, quantizer)
		}
	}
	applyMPEG2MismatchControl(&coefficients)
	pixels := inverseMPEG2DCT(coefficients)
	var result [64]byte
	for i, value := range pixels {
		result[i] = clampMPEG2Sample(value + 128)
	}
	return result
}

func reconstructMPEG2PredictiveFrame(frame, reference DecodedVideoFrame, macroblocks []mpeg2PredictiveMacroblock, mbWidth, mbHeight, quantizer int, includeAC bool) DecodedVideoFrame {
	reconstructed := newMPEG2FrameLike(frame)
	parallelMPEG2Rows(mbHeight, func(row int) {
		for column := range mbWidth {
			macroblock := macroblocks[row*mbWidth+column]
			levels, codedPattern := quantizeMPEG2ResidualMacroblock(macroblock, quantizer, includeAC)
			baseX, baseY := column*16, row*16
			for block := range 4 {
				x := baseX + (block%2)*8
				y := baseY + (block/2)*8
				var residual [64]int
				if codedPattern&(1<<(5-block)) != 0 {
					residual = reconstructMPEG2NonIntraBlock(levels[block], quantizer)
				}
				var pixels [64]byte
				for pixelY := range 8 {
					for pixelX := range 8 {
						prediction := mpeg2MotionSample(reference.Y, reference.Width, x+pixelX, y+pixelY, macroblock.motionX, macroblock.motionY)
						pixels[pixelY*8+pixelX] = clampMPEG2Sample(int(prediction) + residual[pixelY*8+pixelX])
					}
				}
				copyMPEG2Block(reconstructed.Y, frame.Width, x, y, pixels)
			}
			chromaMotionX, chromaMotionY := halfMotionVector(macroblock.motionX), halfMotionVector(macroblock.motionY)
			for planeIndex, plane := range [2]struct {
				source []byte
				target []byte
			}{{reference.U, reconstructed.U}, {reference.V, reconstructed.V}} {
				blockIndex := 4 + planeIndex
				var residual [64]int
				if codedPattern&(1<<(5-blockIndex)) != 0 {
					residual = reconstructMPEG2NonIntraBlock(levels[blockIndex], quantizer)
				}
				var pixels [64]byte
				for pixelY := range 8 {
					for pixelX := range 8 {
						prediction := mpeg2MotionSample(plane.source, frame.Width/2,
							column*8+pixelX, row*8+pixelY, chromaMotionX, chromaMotionY)
						pixels[pixelY*8+pixelX] = clampMPEG2Sample(int(prediction) + residual[pixelY*8+pixelX])
					}
				}
				copyMPEG2Block(plane.target, frame.Width/2, column*8, row*8, pixels)
			}
		}
	})
	return reconstructed
}

func reconstructMPEG2NonIntraBlock(levels [64]int16, quantizer int) [64]int {
	var coefficients [64]int
	for index, level := range levels {
		if level == 0 {
			continue
		}
		coefficients[index] = clampMPEG2Coefficient(inverseMPEG2NonIntraCoefficient(int(level), quantizer))
	}
	applyMPEG2MismatchControl(&coefficients)
	return inverseMPEG2DCT(coefficients)
}

func inverseMPEG2IntraACCoefficient(level, weight, quantizer int) int {
	// MPEG-2 doubles the linear quantizer scale code before inverse
	// quantization: qscale*2*W/16 = qscale*W/8.
	return level * weight * quantizer / 8
}

func inverseMPEG2NonIntraCoefficient(level, quantizer int) int {
	// MPEG-2 doubles the linear quantizer scale code before inverse
	// quantization. With the default non-intra matrix (16), this reduces to
	// sign(level)*(2*abs(level)+1)*qscale.
	sign := 1
	if level < 0 {
		sign = -1
	}
	return sign * ((2*absInt(level) + 1) * quantizer)
}

func newMPEG2FrameLike(frame DecodedVideoFrame) DecodedVideoFrame {
	return DecodedVideoFrame{
		PTS:      frame.PTS,
		Duration: frame.Duration,
		Width:    frame.Width,
		Height:   frame.Height,
		Y:        make([]byte, frame.Width*frame.Height),
		U:        make([]byte, frame.Width*frame.Height/4),
		V:        make([]byte, frame.Width*frame.Height/4),
	}
}

func copyMPEG2Block(plane []byte, stride, x, y int, block [64]byte) {
	for row := range 8 {
		copy(plane[(y+row)*stride+x:(y+row)*stride+x+8], block[row*8:(row+1)*8])
	}
}

func inverseMPEG2DCT(coefficients [64]int) [64]int {
	var pixels [64]int
	var temporary [8][8]float64
	for vertical := range 8 {
		for x := range 8 {
			var value float64
			for horizontal := range 8 {
				value += float64(coefficients[vertical*8+horizontal]) * mpeg2DCTBasis[horizontal][x]
			}
			temporary[vertical][x] = value
		}
	}
	for y := range 8 {
		for x := range 8 {
			var value float64
			for vertical := range 8 {
				value += temporary[vertical][x] * mpeg2DCTBasis[vertical][y]
			}
			pixels[y*8+x] = int(math.Round(value * 0.25))
		}
	}
	return pixels
}

func applyMPEG2MismatchControl(coefficients *[64]int) {
	sum := 0
	for index, value := range coefficients {
		coefficients[index] = clampMPEG2Coefficient(value)
		sum += coefficients[index]
	}
	if sum%2 == 0 {
		if coefficients[63]%2 != 0 {
			coefficients[63]--
		} else {
			coefficients[63]++
		}
	}
}

func clampMPEG2Coefficient(value int) int {
	return max(-2048, min(2047, value))
}

func clampMPEG2Sample(value int) byte {
	return byte(max(0, min(255, value)))
}

func analyzeMPEG2Block(plane []byte, stride, x, y int, chroma bool) mpeg2IntraBlock {
	var block mpeg2IntraBlock
	block.chroma = chroma
	var source, temporary [8][8]float64
	sum := 0
	for row := range 8 {
		for column := range 8 {
			value := int(plane[(y+row)*stride+x+column])
			sum += value
			source[row][column] = float64(value - 128)
		}
	}
	// The bitstream's DC predictor stores the absolute sample level. AC
	// coefficients below use samples centered around 128 for the DCT.
	block.dc = (sum + 32) / 64
	for row := range 8 {
		for horizontal := range 8 {
			var value float64
			for column := range 8 {
				value += source[row][column] * mpeg2DCTBasis[horizontal][column]
			}
			temporary[row][horizontal] = value
		}
	}
	for vertical := range 8 {
		for horizontal := range 8 {
			var value float64
			for row := range 8 {
				value += temporary[row][horizontal] * mpeg2DCTBasis[vertical][row]
			}
			block.coeff[vertical*8+horizontal] = value * 0.25
		}
	}
	return block
}

func writeMPEG2IntraSlice(bits *mpeg2BitWriter, blocks []mpeg2IntraBlock, row, mbWidth, quantizer int, includeAC bool) {
	bits.startCode(byte(row + 1))
	bits.write(uint64(quantizer), 5) // linear quantizer_scale_code.
	bits.write(0, 1)                 // extra_bit_slice
	lastY, lastCb, lastCr := 128, 128, 128
	for column := range mbWidth {
		bits.write(1, 1) // macroblock_address_increment = 1
		bits.write(1, 1) // I-picture macroblock_type = intra
		blockIndex := (row*mbWidth + column) * 6
		for block := range 4 {
			lastY = writeMPEG2Block(bits, blocks[blockIndex+block], lastY, quantizer, includeAC)
		}
		lastCb = writeMPEG2Block(bits, blocks[blockIndex+4], lastCb, quantizer, includeAC)
		lastCr = writeMPEG2Block(bits, blocks[blockIndex+5], lastCr, quantizer, includeAC)
	}
	bits.align()
}

func writeMPEG2PredictiveSlice(bits *mpeg2BitWriter, macroblocks []mpeg2PredictiveMacroblock, row, mbWidth, quantizer int, includeAC bool) {
	bits.startCode(byte(row + 1))
	bits.write(uint64(quantizer), 5)
	bits.write(0, 1) // extra_bit_slice
	lastColumn := -1
	lastMotionX, lastMotionY := 0, 0
	for column := range mbWidth {
		macroblock := macroblocks[row*mbWidth+column]
		levels, codedPattern := quantizeMPEG2ResidualMacroblock(macroblock, quantizer, includeAC)
		hasResidual := codedPattern != 0
		hasMotion := macroblock.motionX != 0 || macroblock.motionY != 0
		mustCode := column == 0 || column == mbWidth-1 || hasResidual || hasMotion
		if !mustCode {
			lastMotionX, lastMotionY = 0, 0
			continue
		}
		writeMPEG2MacroblockAddressIncrement(bits, column-lastColumn)
		if hasMotion {
			if hasResidual {
				bits.write(1, 1) // P-picture MC, coded macroblock.
			} else {
				bits.write(0b001, 3) // P-picture MC, not-coded macroblock.
			}
			writeMPEG2MotionVector(bits, macroblock.motionX, lastMotionX)
			writeMPEG2MotionVector(bits, macroblock.motionY, lastMotionY)
			lastMotionX, lastMotionY = macroblock.motionX, macroblock.motionY
		} else if hasResidual {
			bits.write(0b01, 2) // P-picture no-MC, coded macroblock.
			lastMotionX, lastMotionY = 0, 0
		} else {
			// A boundary macroblock cannot be left as an implicit slice-edge skip.
			bits.write(0b001, 3) // P-picture MC, not-coded macroblock.
			writeMPEG2MotionVector(bits, 0, lastMotionX)
			writeMPEG2MotionVector(bits, 0, lastMotionY)
			lastMotionX, lastMotionY = 0, 0
		}
		if hasResidual {
			writeMPEG2CodedBlockPattern(bits, codedPattern)
			for block := range 6 {
				if codedPattern&(1<<(5-block)) != 0 {
					writeMPEG2NonIntraBlock(bits, levels[block])
				}
			}
		}
		lastColumn = column
	}
	bits.align()
}

func quantizeMPEG2ResidualMacroblock(macroblock mpeg2PredictiveMacroblock, quantizer int, includeAC bool) ([6][64]int16, uint8) {
	var levels [6][64]int16
	var pattern uint8
	coefficientLimit := 64
	if !includeAC {
		coefficientLimit = 1
	}
	for blockIndex, block := range macroblock.blocks {
		for scanPosition := range coefficientLimit {
			coefficientIndex := mpeg2ZigZag[scanPosition]
			value := block.coeff[coefficientIndex]
			magnitude := quantizeMPEG2NonIntraCoefficient(value, quantizer)
			levels[blockIndex][coefficientIndex] = int16(magnitude)
			if magnitude != 0 {
				pattern |= 1 << (5 - blockIndex)
			}
		}
	}
	return levels, pattern
}

func quantizeMPEG2NonIntraCoefficient(value float64, quantizer int) int {
	// The decoder reconstructs level L at (2L+1)*qscale, the midpoint of
	// [2L*qscale, (2L+2)*qscale), so the matching quantizer truncates.
	level := min(int(math.Abs(value)/float64(2*quantizer)), 2047)
	if value < 0 {
		return -level
	}
	return level
}

func writeMPEG2CodedBlockPattern(bits *mpeg2BitWriter, pattern uint8) {
	code := mpeg2CodedBlockPatternCodes[pattern]
	bits.write(uint64(code.bits), int(code.length))
}

func writeMPEG2NonIntraBlock(bits *mpeg2BitWriter, levels [64]int16) {
	lastPosition := -1
	firstCoefficient := true
	for scanPosition := range 64 {
		level := int(levels[mpeg2ZigZag[scanPosition]])
		if level == 0 {
			continue
		}
		run := scanPosition - lastPosition - 1
		writeMPEG2DCTRunLevel(bits, run, level, firstCoefficient)
		lastPosition = scanPosition
		firstCoefficient = false
	}
	bits.write(0b10, 2) // End of block.
}

func writeMPEG2MotionVector(bits *mpeg2BitWriter, motion, predictor int) {
	delta := motion - predictor
	for delta < -16 {
		delta += 32
	}
	for delta > 15 {
		delta -= 32
	}
	code := mpeg2MotionCodeCodes[delta+16]
	for _, bit := range code {
		bits.write(uint64(bit-'0'), 1)
	}
}

type mpeg2VLC struct {
	bits   uint16
	length uint8
}

var mpeg2CodedBlockPatternCodes = [64]mpeg2VLC{
	60: {0b111, 3}, 35: {0b00011100, 8},
	4: {0b1101, 4}, 13: {0b00011011, 8},
	8: {0b1100, 4}, 49: {0b00011010, 8},
	16: {0b1011, 4}, 21: {0b00011001, 8},
	32: {0b1010, 4}, 41: {0b00011000, 8},
	12: {0b10011, 5}, 14: {0b00010111, 8},
	48: {0b10010, 5}, 50: {0b00010110, 8},
	20: {0b10001, 5}, 22: {0b00010101, 8},
	40: {0b10000, 5}, 42: {0b00010100, 8},
	28: {0b01111, 5}, 15: {0b00010011, 8},
	44: {0b01110, 5}, 51: {0b00010010, 8},
	52: {0b01101, 5}, 23: {0b00010001, 8},
	56: {0b01100, 5}, 43: {0b00010000, 8},
	1: {0b01011, 5}, 25: {0b00001111, 8},
	61: {0b01010, 5}, 37: {0b00001110, 8},
	2: {0b01001, 5}, 26: {0b00001101, 8},
	62: {0b01000, 5}, 38: {0b00001100, 8},
	24: {0b001111, 6}, 29: {0b00001011, 8},
	36: {0b001110, 6}, 45: {0b00001010, 8},
	3: {0b001101, 6}, 53: {0b00001001, 8},
	63: {0b001100, 6}, 57: {0b00001000, 8},
	5: {0b0010111, 7}, 30: {0b00000111, 8},
	9: {0b0010110, 7}, 46: {0b00000110, 8},
	17: {0b0010101, 7}, 54: {0b00000101, 8},
	33: {0b0010100, 7}, 58: {0b00000100, 8},
	6: {0b0010011, 7}, 31: {0b000000111, 9},
	10: {0b0010010, 7}, 47: {0b000000110, 9},
	18: {0b0010001, 7}, 55: {0b000000101, 9},
	34: {0b0010000, 7}, 59: {0b000000100, 9},
	7: {0b00011111, 8}, 27: {0b000000011, 9},
	11: {0b00011110, 8}, 39: {0b000000010, 9},
	19: {0b00011101, 8},
}

var mpeg2MotionCodeCodes = [...]string{
	"00000011001", "00000011011", "00000011101", "00000011111",
	"00000100001", "00000100011", "0000010011", "0000010101",
	"0000010111", "00000111", "00001001", "00001011", "0000111",
	"00011", "0011", "011", "1", "010", "0010", "00010",
	"0000110", "00001010", "00001000", "00000110", "0000010110",
	"0000010100", "0000010010", "00000100010", "00000100000",
	"00000011110", "00000011100", "00000011010", "00000011000",
}

var mpeg2MacroblockAddressCodes = [...]string{
	"", "1", "011", "010", "0011", "0010", "00011", "00010",
	"0000111", "0000110", "00001011", "00001010", "00001001", "00001000",
	"00000111", "00000110", "0000010111", "0000010110", "0000010101", "0000010100",
	"0000010011", "0000010010", "00000100011", "00000100010", "00000100001", "00000100000",
	"00000011111", "00000011110", "00000011101", "00000011100", "00000011011", "00000011010",
	"00000011001", "00000011000",
}

func writeMPEG2MacroblockAddressIncrement(bits *mpeg2BitWriter, increment int) {
	for increment > 33 {
		bits.write(0b00000001000, 11) // macroblock_escape adds 33.
		increment -= 33
	}
	code := mpeg2MacroblockAddressCodes[increment]
	for _, bit := range code {
		bits.write(uint64(bit-'0'), 1)
	}
}

func writeMPEG2Block(bits *mpeg2BitWriter, block mpeg2IntraBlock, predictor, quantizer int, includeAC bool) int {
	difference := block.dc - predictor
	size := mpeg2MagnitudeBits(difference)
	writeMPEG2DCSize(bits, size, block.chroma)
	if size != 0 {
		value := difference
		if difference < 0 {
			value += (1 << size) - 1
		}
		bits.write(uint64(value), size)
	}
	if includeAC {
		writeMPEG2ACCoefficients(bits, block.coeff, quantizer)
	} else {
		bits.write(0b10, 2) // end_of_block
	}
	return block.dc
}

func writeMPEG2ACCoefficients(bits *mpeg2BitWriter, coefficients [64]float64, quantizer int) {
	lastPosition := 0
	for scanPosition := 1; scanPosition < 64; scanPosition++ {
		coefficientIndex := mpeg2ZigZag[scanPosition]
		value := coefficients[coefficientIndex]
		weight := mpeg2DefaultIntraMatrix[coefficientIndex]
		level := quantizeMPEG2IntraCoefficient(value, quantizer, weight)
		if level == 0 {
			continue
		}
		writeMPEG2DCTRunLevel(bits, scanPosition-lastPosition-1, level, false)
		lastPosition = scanPosition
	}
	bits.write(0b10, 2) // end_of_block
}

func quantizeMPEG2IntraCoefficient(value float64, quantizer, weight int) int {
	level := min(int(math.Floor(math.Abs(value)*8/(float64(quantizer*weight))+0.5)), 2047)
	if value < 0 {
		return -level
	}
	return level
}

func mpeg2MagnitudeBits(value int) int {
	if value < 0 {
		value = -value
	}
	for size := range 12 {
		if value < 1<<size {
			return size
		}
	}
	return 11
}

func writeMPEG2DCSize(bits *mpeg2BitWriter, size int, chroma bool) {
	if chroma {
		codes := [...]struct {
			value uint16
			width uint8
		}{
			{0b00, 2}, {0b01, 2}, {0b10, 2}, {0b110, 3}, {0b1110, 4},
			{0b11110, 5}, {0b111110, 6}, {0b1111110, 7}, {0b11111110, 8}, {0b111111110, 9},
		}
		bits.write(uint64(codes[size].value), int(codes[size].width))
		return
	}
	codes := [...]struct {
		value uint16
		width uint8
	}{
		{0b100, 3}, {0b00, 2}, {0b01, 2}, {0b101, 3}, {0b110, 3},
		{0b1110, 4}, {0b11110, 5}, {0b111110, 6}, {0b1111110, 7}, {0b11111110, 8},
	}
	bits.write(uint64(codes[size].value), int(codes[size].width))
}

type mpeg2BitWriter struct {
	data      []byte
	current   byte
	used      uint8
	countOnly bool
	counted   int
}

func (w *mpeg2BitWriter) write(value uint64, width int) {
	if w.countOnly {
		if width <= 0 {
			return
		}
		buffered := int(w.used) + width
		w.counted += buffered / 8
		w.used = uint8(buffered % 8)
		return
	}
	for width > 0 {
		available := 8 - int(w.used)
		take := min(width, available)
		shift := width - take
		mask := uint64(1<<take) - 1
		w.current = w.current<<take | byte((value>>shift)&mask)
		w.used += uint8(take)
		width -= take
		if w.used == 8 {
			w.data = append(w.data, w.current)
			w.current, w.used = 0, 0
		}
	}
}

func (w *mpeg2BitWriter) align() {
	if w.used != 0 {
		if w.countOnly {
			w.counted++
		} else {
			w.current <<= 8 - w.used
			w.data = append(w.data, w.current)
		}
		w.current, w.used = 0, 0
	}
}

func (w *mpeg2BitWriter) startCode(code byte) {
	w.align()
	if w.countOnly {
		w.counted += 4
	} else {
		w.data = append(w.data, 0, 0, 1, code)
	}
}

func (w *mpeg2BitWriter) bytes() []byte {
	w.align()
	return w.data
}

func (w *mpeg2BitWriter) sizeBytes() int {
	if w.countOnly {
		size := w.counted
		if w.used != 0 {
			size++
		}
		return size
	}
	size := len(w.data)
	if w.used != 0 {
		size++
	}
	return size
}
