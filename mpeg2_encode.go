package mp4todvd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
)

const mpeg2VideoBitrate = 7_000_000

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
	dst        io.Writer
	profile    Profile
	frameRate  int
	frameBytes int
	frameNo    int64
	reference  *DecodedVideoFrame
	closed     bool
	writeErr   error
}

// NewMPEG2VideoEncoder prepares an encoder for NTSC or PAL full-raster DVD
// frames. It writes the sequence header with the first GOP.
func NewMPEG2VideoEncoder(dst io.Writer, profile Profile) (*MPEG2VideoEncoder, error) {
	if dst == nil {
		return nil, errors.New("mp4todvd: MPEG-2 destination writer is required")
	}
	if err := validateEncoderProfile(profile); err != nil {
		return nil, err
	}
	encoder := &MPEG2VideoEncoder{dst: dst, profile: profile}
	switch profile.Standard {
	case VideoStandardNTSC:
		encoder.frameRate = 30
		encoder.frameBytes = mpeg2VideoBitrate * 1001 / (8 * 30000)
	case VideoStandardPAL:
		encoder.frameRate = 25
		encoder.frameBytes = mpeg2VideoBitrate / (8 * 25)
	default:
		return nil, ErrInvalidProfile
	}
	return encoder, nil
}

// WriteFrame encodes one full-resolution planar YUV 4:2:0 frame. Frames must
// already be fitted and timestamp-resampled to the selected DVD profile.
func (e *MPEG2VideoEncoder) WriteFrame(frame DecodedVideoFrame) error {
	if e == nil || e.dst == nil {
		return errors.New("mp4todvd: MPEG-2 encoder is not configured")
	}
	if e.closed {
		return errors.New("mp4todvd: MPEG-2 encoder is already closed")
	}
	if e.writeErr != nil {
		return e.writeErr
	}
	if err := validateDecodedFrame(frame); err != nil {
		return err
	}
	if frame.Width != e.profile.FrameWidth || frame.Height != e.profile.FrameHeight {
		return fmt.Errorf("mp4todvd: MPEG-2 encoder requires %dx%d frames", e.profile.FrameWidth, e.profile.FrameHeight)
	}
	mbWidth := frame.Width / 16
	mbHeight := frame.Height / 16
	isIntra := e.reference == nil || e.frameNo%15 == 0
	var intraBlocks []mpeg2IntraBlock
	var predictiveMacroblocks []mpeg2PredictiveMacroblock
	if isIntra {
		intraBlocks = collectMPEG2IntraBlocks(frame, mbWidth, mbHeight)
	} else {
		predictiveMacroblocks = analyzeMPEG2PredictiveMacroblocks(frame, *e.reference, mbWidth, mbHeight)
	}
	var encoded []byte
	selectedQuantizer := 1
	includeAC := true
	for quantizer := 1; quantizer <= 31; quantizer++ {
		if isIntra {
			encoded = e.encodeIntraPicture(frame, intraBlocks, mbWidth, mbHeight, quantizer, true)
		} else {
			encoded = e.encodePredictivePicture(predictiveMacroblocks, mbWidth, mbHeight, quantizer, true)
		}
		if len(encoded) <= e.frameBytes {
			selectedQuantizer = quantizer
			break
		}
	}
	if len(encoded) > e.frameBytes {
		// At the upper quantizer limit, drop AC coefficients as a lower-rate fallback.
		selectedQuantizer = 31
		includeAC = false
		if isIntra {
			encoded = e.encodeIntraPicture(frame, intraBlocks, mbWidth, mbHeight, selectedQuantizer, false)
		} else {
			encoded = e.encodePredictivePicture(predictiveMacroblocks, mbWidth, mbHeight, selectedQuantizer, false)
		}
	}
	if err := writeAll(e.dst, encoded); err != nil {
		e.writeErr = fmt.Errorf("mp4todvd: write MPEG-2 picture %d: %w", e.frameNo, err)
		return e.writeErr
	}
	var reference DecodedVideoFrame
	if isIntra {
		reference = reconstructMPEG2IntraFrame(frame, intraBlocks, mbWidth, mbHeight, selectedQuantizer, includeAC)
	} else {
		reference = reconstructMPEG2PredictiveFrame(frame, *e.reference, predictiveMacroblocks, mbWidth, mbHeight, selectedQuantizer, includeAC)
	}
	e.reference = &reference
	e.frameNo++
	return nil
}

func (e *MPEG2VideoEncoder) encodeIntraPicture(frame DecodedVideoFrame, blocks []mpeg2IntraBlock, mbWidth, mbHeight, quantizer int, includeAC bool) []byte {
	var bits mpeg2BitWriter
	if e.frameNo%15 == 0 {
		writeMPEG2SequenceHeader(&bits, e.profile)
		writeMPEG2GOPHeader(&bits, e.frameNo, e.frameRate)
	}
	writeMPEG2PictureHeader(&bits, uint16(e.frameNo%15), 1)
	for row := range mbHeight {
		writeMPEG2IntraSlice(&bits, blocks, row, mbWidth, quantizer, includeAC)
	}
	return bits.bytes()
}

func (e *MPEG2VideoEncoder) encodePredictivePicture(macroblocks []mpeg2PredictiveMacroblock, mbWidth, mbHeight, quantizer int, includeAC bool) []byte {
	var bits mpeg2BitWriter
	writeMPEG2PictureHeader(&bits, uint16(e.frameNo%15), 2)
	for row := range mbHeight {
		writeMPEG2PredictiveSlice(&bits, macroblocks, row, mbWidth, quantizer, includeAC)
	}
	return bits.bytes()
}

// Close finishes the elementary stream. It does not close the destination.
func (e *MPEG2VideoEncoder) Close() error {
	if e == nil || e.dst == nil {
		return errors.New("mp4todvd: MPEG-2 encoder is not configured")
	}
	if e.closed {
		return e.writeErr
	}
	e.closed = true
	if e.writeErr != nil {
		return e.writeErr
	}
	if e.frameNo == 0 {
		e.writeErr = errors.New("mp4todvd: MPEG-2 stream contains no video frames")
		return e.writeErr
	}
	var bits mpeg2BitWriter
	bits.startCode(0xb7)
	if err := writeAll(e.dst, bits.bytes()); err != nil {
		e.writeErr = fmt.Errorf("mp4todvd: finish MPEG-2 stream: %w", err)
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
	bits.write(mpeg2VideoBitrate/400, 18) // Maximum video rate in units of 400 bit/s.
	bits.write(1, 1)                      // marker_bit
	bits.write(112, 10)                   // 224 KiB video buffering verifier.
	bits.write(0, 1)                      // constrained_parameters_flag
	bits.write(0, 1)                      // load_intra_quantiser_matrix
	bits.write(0, 1)                      // load_non_intra_quantiser_matrix

	bits.startCode(0xb5)
	bits.write(1, 4)    // sequence_extension_identifier
	bits.write(0x48, 8) // Main Profile at Main Level
	bits.write(1, 1)    // progressive_sequence
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
	bits.write(0, 1) // top_field_first
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
	for row := range mbHeight {
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
	}
	return macroblocks
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
	var cost int64
	for row := range 16 {
		for column := range 16 {
			actual := int(frame.Y[(y+row)*frame.Width+x+column])
			predicted := int(mpeg2MotionSample(reference.Y, reference.Width, x+column, y+row, motionX, motionY))
			cost += int64(absInt(actual - predicted))
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
	if fractionX != 0 && fractionY == 0 {
		return byte((first + int(plane[top*stride+left+1])) / 2)
	}
	if fractionX == 0 && fractionY != 0 {
		return byte((first + int(plane[(top+1)*stride+left])) / 2)
	}
	return byte((first + int(plane[top*stride+left+1]) + int(plane[(top+1)*stride+left]) + int(plane[(top+1)*stride+left+1])) / 4)
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
	blocks := make([]mpeg2IntraBlock, 0, mbWidth*mbHeight*6)
	for mbRow := range mbHeight {
		for mbColumn := range mbWidth {
			baseX, baseY := mbColumn*16, mbRow*16
			for block := range 4 {
				x := baseX + (block%2)*8
				y := baseY + (block/2)*8
				blocks = append(blocks, analyzeMPEG2Block(frame.Y, frame.Width, x, y, false))
			}
			blocks = append(blocks,
				analyzeMPEG2Block(frame.U, frame.Width/2, mbColumn*8, mbRow*8, true),
				analyzeMPEG2Block(frame.V, frame.Width/2, mbColumn*8, mbRow*8, true),
			)
		}
	}
	return blocks
}

func reconstructMPEG2IntraFrame(frame DecodedVideoFrame, blocks []mpeg2IntraBlock, mbWidth, mbHeight, quantizer int, includeAC bool) DecodedVideoFrame {
	reconstructed := newMPEG2FrameLike(frame)
	for row := range mbHeight {
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
	}
	return reconstructed
}

func reconstructMPEG2IntraBlock(block mpeg2IntraBlock, quantizer int, includeAC bool) [64]byte {
	var coefficients [64]int
	coefficients[0] = block.dc * 8
	if includeAC {
		for scanPosition := 1; scanPosition < 64; scanPosition++ {
			index := mpeg2ZigZag[scanPosition]
			weight := mpeg2DefaultIntraMatrix[index]
			level := quantizeMPEG2IntraCoefficient(block.coeff[index], quantizer, weight)
			coefficients[index] = level * weight * quantizer / 8
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
	for row := range mbHeight {
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
	}
	return reconstructed
}

func reconstructMPEG2NonIntraBlock(levels [64]int, quantizer int) [64]int {
	var coefficients [64]int
	for index, level := range levels {
		if level == 0 {
			continue
		}
		sign := 1
		if level < 0 {
			sign = -1
		}
		value := (2*absInt(level) + 1) * sign * quantizer
		coefficients[index] = clampMPEG2Coefficient(value)
	}
	applyMPEG2MismatchControl(&coefficients)
	return inverseMPEG2DCT(coefficients)
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
	for y := range 8 {
		for x := range 8 {
			var value float64
			for vertical := range 8 {
				for horizontal := range 8 {
					value += float64(coefficients[vertical*8+horizontal]) * mpeg2DCTBasis[horizontal][x] * mpeg2DCTBasis[vertical][y]
				}
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
	block.dc = (sum+32)/64 - 128
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

func quantizeMPEG2ResidualMacroblock(macroblock mpeg2PredictiveMacroblock, quantizer int, includeAC bool) ([6][64]int, uint8) {
	var levels [6][64]int
	var pattern uint8
	coefficientLimit := 64
	if !includeAC {
		coefficientLimit = 1
	}
	for blockIndex, block := range macroblock.blocks {
		for scanPosition := range coefficientLimit {
			coefficientIndex := mpeg2ZigZag[scanPosition]
			value := block.coeff[coefficientIndex]
			magnitude := min(int(math.Floor((math.Abs(value)/float64(quantizer)+1)/2)), 2047)
			if value < 0 {
				magnitude = -magnitude
			}
			levels[blockIndex][coefficientIndex] = magnitude
			if magnitude != 0 {
				pattern |= 1 << (5 - blockIndex)
			}
		}
	}
	return levels, pattern
}

func writeMPEG2CodedBlockPattern(bits *mpeg2BitWriter, pattern uint8) {
	code := mpeg2CodedBlockPatternCodes[pattern]
	bits.write(uint64(code.bits), int(code.length))
}

func writeMPEG2NonIntraBlock(bits *mpeg2BitWriter, levels [64]int) {
	lastPosition := -1
	for scanPosition := range 64 {
		level := levels[mpeg2ZigZag[scanPosition]]
		if level == 0 {
			continue
		}
		run := scanPosition - lastPosition - 1
		bits.write(1, 6) // DCT escape code 000001.
		bits.write(uint64(run), 6)
		bits.write(uint64(level)&0x0fff, 12) // MPEG-2 signed_level.
		lastPosition = scanPosition
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
		bits.write(1, 6) // DCT escape code 000001.
		bits.write(uint64(scanPosition-lastPosition-1), 6)
		bits.write(uint64(level)&0x0fff, 12) // MPEG-2 signed_level.
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
	data    []byte
	current byte
	used    uint8
}

func (w *mpeg2BitWriter) write(value uint64, width int) {
	for bit := width - 1; bit >= 0; bit-- {
		w.current = w.current<<1 | byte((value>>bit)&1)
		w.used++
		if w.used == 8 {
			w.data = append(w.data, w.current)
			w.current, w.used = 0, 0
		}
	}
}

func (w *mpeg2BitWriter) align() {
	if w.used != 0 {
		w.current <<= 8 - w.used
		w.data = append(w.data, w.current)
		w.current, w.used = 0, 0
	}
}

func (w *mpeg2BitWriter) startCode(code byte) {
	w.align()
	w.data = append(w.data, 0, 0, 1, code)
}

func (w *mpeg2BitWriter) bytes() []byte {
	w.align()
	return w.data
}
