package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var ErrInvalidAVC = errors.New("video2dvd: invalid AVC sample or configuration")

// AVCNALLengthSize returns the size of the NAL unit length field stored in an
// avcC decoder configuration record.
func AVCNALLengthSize(config []byte) (int, error) {
	if len(config) < 5 || config[0] != 1 {
		return 0, fmt.Errorf("%w: invalid avcC record", ErrInvalidAVC)
	}
	size := int(config[4]&0x03) + 1
	if size == 3 {
		return 0, fmt.Errorf("%w: reserved NAL length size", ErrInvalidAVC)
	}
	return size, nil
}

// AVCParameterSets returns the SPS and PPS NAL units in an avcC record.
func AVCParameterSets(config []byte) ([][]byte, [][]byte, error) {
	if len(config) < 7 || config[0] != 1 {
		return nil, nil, fmt.Errorf("%w: invalid avcC record", ErrInvalidAVC)
	}
	position := 6
	spsCount := int(config[5] & 0x1f)
	sps, err := readAVCNALUnits(config, &position, spsCount)
	if err != nil {
		return nil, nil, err
	}
	if position >= len(config) {
		return nil, nil, fmt.Errorf("%w: missing PPS count", ErrInvalidAVC)
	}
	ppsCount := int(config[position])
	position++
	pps, err := readAVCNALUnits(config, &position, ppsCount)
	if err != nil {
		return nil, nil, err
	}
	if len(sps) == 0 || len(pps) == 0 {
		return nil, nil, fmt.Errorf("%w: avcC must contain SPS and PPS NAL units", ErrInvalidAVC)
	}
	return sps, pps, nil
}

func readAVCNALUnits(config []byte, position *int, count int) ([][]byte, error) {
	units := make([][]byte, 0, count)
	for range count {
		if len(config)-*position < 2 {
			return nil, fmt.Errorf("%w: truncated parameter-set length", ErrInvalidAVC)
		}
		size := int(binary.BigEndian.Uint16(config[*position : *position+2]))
		*position += 2
		if size == 0 || size > len(config)-*position {
			return nil, fmt.Errorf("%w: invalid parameter-set length", ErrInvalidAVC)
		}
		unit := append([]byte(nil), config[*position:*position+size]...)
		units = append(units, unit)
		*position += size
	}
	return units, nil
}

// AVCConfigToAnnexB writes the SPS and PPS units from avcC in Annex-B form.
func AVCConfigToAnnexB(config []byte) ([]byte, error) {
	sps, pps, err := AVCParameterSets(config)
	if err != nil {
		return nil, err
	}
	output := make([]byte, 0, len(config)+4*len(sps)+4*len(pps))
	for _, units := range [][][]byte{sps, pps} {
		for _, unit := range units {
			output = append(output, 0, 0, 0, 1)
			output = append(output, unit...)
		}
	}
	return output, nil
}

// AVCToAnnexB converts one length-prefixed AVC access unit from MP4 into
// Annex-B NAL units. nalLengthSize must come from the track's avcC record.
func AVCToAnnexB(sample []byte, nalLengthSize int) ([]byte, error) {
	if nalLengthSize != 1 && nalLengthSize != 2 && nalLengthSize != 4 {
		return nil, fmt.Errorf("%w: NAL length size must be 1, 2, or 4", ErrInvalidAVC)
	}
	if len(sample) == 0 {
		return nil, fmt.Errorf("%w: empty access unit", ErrInvalidAVC)
	}
	output := make([]byte, 0, len(sample)+16)
	for position := 0; position < len(sample); {
		if len(sample)-position < nalLengthSize {
			return nil, fmt.Errorf("%w: truncated NAL length", ErrInvalidAVC)
		}
		var naluLength uint32
		switch nalLengthSize {
		case 1:
			naluLength = uint32(sample[position])
		case 2:
			naluLength = uint32(binary.BigEndian.Uint16(sample[position : position+2]))
		case 4:
			naluLength = binary.BigEndian.Uint32(sample[position : position+4])
		}
		position += nalLengthSize
		if naluLength == 0 || uint64(naluLength) > uint64(len(sample)-position) {
			return nil, fmt.Errorf("%w: invalid NAL length %d", ErrInvalidAVC, naluLength)
		}
		output = append(output, 0, 0, 0, 1)
		output = append(output, sample[position:position+int(naluLength)]...)
		position += int(naluLength)
	}
	return output, nil
}

// avcSPSColor reads the colour description from the VUI of a sequence
// parameter set NAL unit. ok is false when the SPS has no
// video_signal_type or cannot be parsed; callers then fall back to container
// metadata.
func avcSPSColor(sps []byte) (color VideoColor, ok bool) {
	if len(sps) < 4 || sps[0]&0x1f != 7 {
		return VideoColor{}, false
	}
	// Strip emulation prevention bytes (00 00 03 -> 00 00).
	payload := make([]byte, 0, len(sps)-1)
	zeros := 0
	for _, value := range sps[1:] {
		if zeros >= 2 && value == 3 {
			zeros = 0
			continue
		}
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
		payload = append(payload, value)
	}
	reader := expGolombReader{bits: bitReader{data: payload}}
	profile := reader.bitsValue(8)
	reader.bitsValue(16) // constraint flags and level_idc
	reader.unsigned()    // seq_parameter_set_id
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chromaFormat := reader.unsigned()
		if chromaFormat == 3 {
			reader.bitsValue(1) // separate_colour_plane_flag
		}
		reader.unsigned()   // bit_depth_luma_minus8
		reader.unsigned()   // bit_depth_chroma_minus8
		reader.bitsValue(1) // qpprime_y_zero_transform_bypass_flag
		if reader.bitsValue(1) == 1 {
			lists := 8
			if chromaFormat == 3 {
				lists = 12
			}
			for list := range lists {
				if reader.bitsValue(1) == 0 {
					continue
				}
				size := 16
				if list >= 6 {
					size = 64
				}
				last, next := 8, 8
				for range size {
					if next != 0 {
						next = (last + reader.signed() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	reader.unsigned() // log2_max_frame_num_minus4
	switch reader.unsigned() {
	case 0:
		reader.unsigned() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		reader.bitsValue(1) // delta_pic_order_always_zero_flag
		reader.signed()     // offset_for_non_ref_pic
		reader.signed()     // offset_for_top_to_bottom_field
		cycle := reader.unsigned()
		if cycle > 255 {
			return VideoColor{}, false
		}
		for range cycle {
			reader.signed()
		}
	}
	reader.unsigned()   // max_num_ref_frames
	reader.bitsValue(1) // gaps_in_frame_num_value_allowed_flag
	reader.unsigned()   // pic_width_in_mbs_minus1
	reader.unsigned()   // pic_height_in_map_units_minus1
	if reader.bitsValue(1) == 0 {
		reader.bitsValue(1) // mb_adaptive_frame_field_flag
	}
	reader.bitsValue(1) // direct_8x8_inference_flag
	if reader.bitsValue(1) == 1 {
		for range 4 {
			reader.unsigned() // frame cropping offsets
		}
	}
	if reader.bitsValue(1) == 0 { // vui_parameters_present_flag
		return VideoColor{}, false
	}
	if reader.bitsValue(1) == 1 { // aspect_ratio_info_present_flag
		if reader.bitsValue(8) == 255 {
			reader.bitsValue(32) // sar_width, sar_height
		}
	}
	if reader.bitsValue(1) == 1 { // overscan_info_present_flag
		reader.bitsValue(1)
	}
	if reader.bitsValue(1) == 0 { // video_signal_type_present_flag
		return VideoColor{}, false
	}
	reader.bitsValue(3) // video_format
	color.FullRange = reader.bitsValue(1) == 1
	if reader.bitsValue(1) == 1 { // colour_description_present_flag
		reader.bitsValue(16) // colour_primaries, transfer_characteristics
		color.Matrix = colorMatrixFromCode(reader.bitsValue(8))
	}
	if reader.failed {
		return VideoColor{}, false
	}
	return color, true
}

// expGolombReader reads H.264 syntax elements; after the first read past the
// end every value is zero and failed is set.
type expGolombReader struct {
	bits   bitReader
	failed bool
}

func (r *expGolombReader) bitsValue(count int) int {
	value, err := r.bits.read(count)
	if err != nil {
		r.failed = true
		return 0
	}
	return int(value)
}

func (r *expGolombReader) unsigned() int {
	zeros := 0
	for !r.failed && r.bitsValue(1) == 0 {
		zeros++
		if zeros > 31 {
			r.failed = true
		}
	}
	if r.failed {
		return 0
	}
	return 1<<zeros - 1 + r.bitsValue(zeros)
}

func (r *expGolombReader) signed() int {
	code := r.unsigned()
	if code%2 == 1 {
		return (code + 1) / 2
	}
	return -(code / 2)
}
