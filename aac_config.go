package mp4todvd

import (
	"errors"
	"fmt"
)

// extractAACAudioSpecificConfig reads the DecoderSpecificInfo descriptor from
// an ISO BMFF esds full box.
func extractAACAudioSpecificConfig(esds []byte) ([]byte, error) {
	if len(esds) < 5 {
		return nil, errors.New("mp4todvd: truncated esds box")
	}
	config, found, err := findAACConfigDescriptor(esds[4:], 0)
	if err != nil {
		return nil, err
	}
	if !found || len(config) == 0 {
		return nil, errors.New("mp4todvd: esds has no AAC AudioSpecificConfig")
	}
	return config, nil
}

func findAACConfigDescriptor(data []byte, depth int) ([]byte, bool, error) {
	if depth > 4 {
		return nil, false, errors.New("mp4todvd: esds descriptor nesting is too deep")
	}
	for len(data) > 0 {
		tag := data[0]
		payload, rest, err := readDescriptor(data[1:])
		if err != nil {
			return nil, false, err
		}
		data = rest
		switch tag {
		case 0x05:
			return payload, true, nil
		case 0x03:
			children, err := esDescriptorChildren(payload)
			if err != nil {
				return nil, false, err
			}
			if config, found, err := findAACConfigDescriptor(children, depth+1); found || err != nil {
				return config, found, err
			}
		case 0x04:
			if len(payload) < 13 {
				return nil, false, errors.New("mp4todvd: truncated decoder configuration descriptor")
			}
			if config, found, err := findAACConfigDescriptor(payload[13:], depth+1); found || err != nil {
				return config, found, err
			}
		}
	}
	return nil, false, nil
}

func esDescriptorChildren(payload []byte) ([]byte, error) {
	if len(payload) < 3 {
		return nil, errors.New("mp4todvd: truncated ES descriptor")
	}
	flags := payload[2]
	position := 3
	if flags&0x80 != 0 {
		position += 2
	}
	if flags&0x40 != 0 {
		if position >= len(payload) {
			return nil, errors.New("mp4todvd: truncated ES descriptor URL")
		}
		urlLength := int(payload[position])
		position++
		position += urlLength
	}
	if flags&0x20 != 0 {
		position += 2
	}
	if position > len(payload) {
		return nil, errors.New("mp4todvd: truncated optional ES descriptor fields")
	}
	return payload[position:], nil
}

func readDescriptor(data []byte) ([]byte, []byte, error) {
	length := 0
	position := 0
	for bytesRead := 0; bytesRead < 4; bytesRead++ {
		if position >= len(data) {
			return nil, nil, errors.New("mp4todvd: truncated descriptor length")
		}
		value := data[position]
		position++
		if length > (int(^uint(0)>>1) >> 7) {
			return nil, nil, errors.New("mp4todvd: descriptor length overflows int")
		}
		length = (length << 7) | int(value&0x7f)
		if value&0x80 == 0 {
			if length > len(data)-position {
				return nil, nil, fmt.Errorf("mp4todvd: descriptor length %d exceeds esds data", length)
			}
			return data[position : position+length], data[position+length:], nil
		}
	}
	return nil, nil, errors.New("mp4todvd: descriptor length uses more than four bytes")
}
