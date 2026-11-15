package mp4todvd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
)

const mpeg2VideoBitrate = 5_000_000

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
// YUV 4:2:0 frames. It emits closed GOPs containing I pictures and P pictures;
// P pictures skip macroblocks that exactly match the previous input frame and
// code changed macroblocks intra. Its simple quantizer and lack of motion
// estimation limit quality and compression efficiency.
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
	blocks := collectMPEG2IntraBlocks(frame, mbWidth, mbHeight)
	isIntra := e.reference == nil || e.frameNo%15 == 0
	var encoded []byte
	for quantizer := 1; quantizer <= 31; quantizer++ {
		if isIntra {
			encoded = e.encodeIntraPicture(frame, blocks, mbWidth, mbHeight, quantizer, true)
		} else {
			encoded = e.encodePredictivePicture(frame, *e.reference, blocks, mbWidth, mbHeight, quantizer, true)
		}
		if len(encoded) <= e.frameBytes {
			break
		}
	}
	if len(encoded) > e.frameBytes {
		// At the upper quantizer limit, retain the DC image as a bounded fallback.
		if isIntra {
			encoded = e.encodeIntraPicture(frame, blocks, mbWidth, mbHeight, 31, false)
		} else {
			encoded = e.encodePredictivePicture(frame, *e.reference, blocks, mbWidth, mbHeight, 31, false)
		}
	}
	if err := writeAll(e.dst, encoded); err != nil {
		e.writeErr = fmt.Errorf("mp4todvd: write MPEG-2 picture %d: %w", e.frameNo, err)
		return e.writeErr
	}
	reference := cloneDecodedVideoFrame(frame)
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

func (e *MPEG2VideoEncoder) encodePredictivePicture(frame, reference DecodedVideoFrame, blocks []mpeg2IntraBlock, mbWidth, mbHeight, quantizer int, includeAC bool) []byte {
	var bits mpeg2BitWriter
	writeMPEG2PictureHeader(&bits, uint16(e.frameNo%15), 2)
	for row := range mbHeight {
		writeMPEG2PredictiveSlice(&bits, frame, reference, blocks, row, mbWidth, quantizer, includeAC)
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
	bits.write(12500, 18) // 5 Mbit/s in units of 400 bit/s.
	bits.write(1, 1)      // marker_bit
	bits.write(112, 10)   // 224 KiB video buffering verifier.
	bits.write(0, 1)      // constrained_parameters_flag
	bits.write(0, 1)      // load_intra_quantiser_matrix
	bits.write(0, 1)      // load_non_intra_quantiser_matrix

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

func writeMPEG2PredictiveSlice(bits *mpeg2BitWriter, frame, reference DecodedVideoFrame, blocks []mpeg2IntraBlock,
	row, mbWidth, quantizer int, includeAC bool) {
	bits.startCode(byte(row + 1))
	bits.write(uint64(quantizer), 5)
	bits.write(0, 1) // extra_bit_slice
	lastY, lastCb, lastCr := 128, 128, 128
	lastColumn := -1
	for column := range mbWidth {
		// Keep both slice edges coded so skipped macroblocks are represented
		// between coded addresses rather than left implicit at the slice end.
		changed := column == 0 || column == mbWidth-1 || !sameMPEG2Macroblock(frame, reference, row, column)
		if !changed {
			continue
		}
		writeMPEG2MacroblockAddressIncrement(bits, column-lastColumn)
		if column-lastColumn > 1 {
			lastY, lastCb, lastCr = 128, 128, 128
		}
		bits.write(0b00011, 5) // P-picture macroblock_type = intra.
		blockIndex := (row*mbWidth + column) * 6
		for block := range 4 {
			lastY = writeMPEG2Block(bits, blocks[blockIndex+block], lastY, quantizer, includeAC)
		}
		lastCb = writeMPEG2Block(bits, blocks[blockIndex+4], lastCb, quantizer, includeAC)
		lastCr = writeMPEG2Block(bits, blocks[blockIndex+5], lastCr, quantizer, includeAC)
		lastColumn = column
	}
	bits.align()
}

func sameMPEG2Macroblock(first, second DecodedVideoFrame, row, column int) bool {
	left, top := column*16, row*16
	if !samePlaneRectangle(first.Y, second.Y, first.Width, left, top, 16, 16) {
		return false
	}
	left, top = column*8, row*8
	return samePlaneRectangle(first.U, second.U, first.Width/2, left, top, 8, 8) &&
		samePlaneRectangle(first.V, second.V, first.Width/2, left, top, 8, 8)
}

func samePlaneRectangle(first, second []byte, stride, x, y, width, height int) bool {
	for row := range height {
		start := (y+row)*stride + x
		if !bytes.Equal(first[start:start+width], second[start:start+width]) {
			return false
		}
	}
	return true
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
	qScale := float64(quantizer * 2)
	for scanPosition := 1; scanPosition < 64; scanPosition++ {
		coefficientIndex := mpeg2ZigZag[scanPosition]
		value := coefficients[coefficientIndex]
		weight := float64(mpeg2DefaultIntraMatrix[coefficientIndex])
		level := int(math.Floor(math.Abs(value)*16/(qScale*weight) + 0.5))
		if level == 0 {
			continue
		}
		if level > 127 {
			level = 127
		}
		bits.write(1, 6) // DCT escape code 000001.
		bits.write(uint64(scanPosition-lastPosition-1), 6)
		if value < 0 {
			bits.write(uint64(byte(-level)), 8)
		} else {
			bits.write(uint64(level), 8)
		}
		lastPosition = scanPosition
	}
	bits.write(0b10, 2) // end_of_block
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
