package mp4todvd

import (
	"errors"
	"fmt"
	"io"
)

// MPEG2VideoEncoder writes a DVD-raster MPEG-2 elementary stream from planar
// YUV 4:2:0 frames. This first in-process encoder emits closed GOPs containing
// I pictures and codes only each 8x8 block's DC coefficient. The stream is
// standards-shaped and small, but the omitted AC coefficients make image
// detail very coarse; this is not yet a quality encoder for general use.
//
// The writer does not close dst. Call Close after the final frame to write the
// MPEG sequence-end code.
type MPEG2VideoEncoder struct {
	dst       io.Writer
	profile   Profile
	frameRate int
	frameNo   int64
	closed    bool
	writeErr  error
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
	case VideoStandardPAL:
		encoder.frameRate = 25
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
	var bits mpeg2BitWriter
	if e.frameNo%15 == 0 {
		writeMPEG2SequenceHeader(&bits, e.profile)
		writeMPEG2GOPHeader(&bits, e.frameNo, e.frameRate)
	}
	writeMPEG2PictureHeader(&bits, uint16(e.frameNo%15))
	mbWidth := frame.Width / 16
	mbHeight := frame.Height / 16
	for row := 0; row < mbHeight; row++ {
		writeMPEG2DCOnlySlice(&bits, frame, row, mbWidth)
	}
	if err := writeAll(e.dst, bits.bytes()); err != nil {
		e.writeErr = fmt.Errorf("mp4todvd: write MPEG-2 picture %d: %w", e.frameNo, err)
		return e.writeErr
	}
	e.frameNo++
	return nil
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

func writeMPEG2PictureHeader(bits *mpeg2BitWriter, temporalReference uint16) {
	bits.startCode(0x00)
	bits.write(uint64(temporalReference), 10)
	bits.write(1, 3)       // I picture
	bits.write(0xffff, 16) // vbv_delay
	bits.write(0, 1)       // extra_bit_picture

	bits.startCode(0xb5)
	bits.write(8, 4) // picture_coding_extension_identifier
	for i := 0; i < 4; i++ {
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

func writeMPEG2DCOnlySlice(bits *mpeg2BitWriter, frame DecodedVideoFrame, row, mbWidth int) {
	bits.startCode(byte(row + 1))
	bits.write(8, 5) // quantizer_scale_code; AC coefficients are omitted.
	bits.write(0, 1) // extra_bit_slice
	lastY, lastCb, lastCr := 128, 128, 128
	for column := 0; column < mbWidth; column++ {
		bits.write(1, 1) // macroblock_address_increment = 1
		bits.write(1, 1) // I-picture macroblock_type = intra
		baseX, baseY := column*16, row*16
		for block := 0; block < 4; block++ {
			x := baseX + (block%2)*8
			y := baseY + (block/2)*8
			lastY = writeMPEG2DCAverage(bits, frame.Y, frame.Width, x, y, lastY, false)
		}
		lastCb = writeMPEG2DCAverage(bits, frame.U, frame.Width/2, column*8, row*8, lastCb, true)
		lastCr = writeMPEG2DCAverage(bits, frame.V, frame.Width/2, column*8, row*8, lastCr, true)
	}
	bits.align()
}

func writeMPEG2DCAverage(bits *mpeg2BitWriter, plane []byte, stride, x, y, predictor int, chroma bool) int {
	sum := 0
	for row := 0; row < 8; row++ {
		for column := 0; column < 8; column++ {
			sum += int(plane[(y+row)*stride+x+column])
		}
	}
	level := (sum+32)/64 - 128
	difference := level - predictor
	size := mpeg2MagnitudeBits(difference)
	writeMPEG2DCSize(bits, size, chroma)
	if size != 0 {
		value := difference
		if difference < 0 {
			value += (1 << size) - 1
		}
		bits.write(uint64(value), size)
	}
	bits.write(0b10, 2) // end_of_block; all AC coefficients are zero.
	return level
}

func mpeg2MagnitudeBits(value int) int {
	if value < 0 {
		value = -value
	}
	for size := 0; size < 12; size++ {
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
