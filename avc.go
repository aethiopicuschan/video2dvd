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
