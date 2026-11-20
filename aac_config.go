package video2dvd

import (
	"errors"
	"fmt"
)

// extractAACAudioSpecificConfig reads the DecoderSpecificInfo descriptor from
// an ISO BMFF esds full box.
func extractAACAudioSpecificConfig(esds []byte) ([]byte, error) {
	if len(esds) < 5 {
		return nil, errors.New("video2dvd: truncated esds box")
	}
	config, found, err := findAACConfigDescriptor(esds[4:], 0)
	if err != nil {
		return nil, err
	}
	if !found || len(config) == 0 {
		return nil, errors.New("video2dvd: esds has no AAC AudioSpecificConfig")
	}
	return config, nil
}

func findAACConfigDescriptor(data []byte, depth int) ([]byte, bool, error) {
	if depth > 4 {
		return nil, false, errors.New("video2dvd: esds descriptor nesting is too deep")
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
				return nil, false, errors.New("video2dvd: truncated decoder configuration descriptor")
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
		return nil, errors.New("video2dvd: truncated ES descriptor")
	}
	flags := payload[2]
	position := 3
	if flags&0x80 != 0 {
		position += 2
	}
	if flags&0x40 != 0 {
		if position >= len(payload) {
			return nil, errors.New("video2dvd: truncated ES descriptor URL")
		}
		urlLength := int(payload[position])
		position++
		position += urlLength
	}
	if flags&0x20 != 0 {
		position += 2
	}
	if position > len(payload) {
		return nil, errors.New("video2dvd: truncated optional ES descriptor fields")
	}
	return payload[position:], nil
}

func readDescriptor(data []byte) ([]byte, []byte, error) {
	length := 0
	position := 0
	for range 4 {
		if position >= len(data) {
			return nil, nil, errors.New("video2dvd: truncated descriptor length")
		}
		value := data[position]
		position++
		if length > (int(^uint(0)>>1) >> 7) {
			return nil, nil, errors.New("video2dvd: descriptor length overflows int")
		}
		length = (length << 7) | int(value&0x7f)
		if value&0x80 == 0 {
			if length > len(data)-position {
				return nil, nil, fmt.Errorf("video2dvd: descriptor length %d exceeds esds data", length)
			}
			return data[position : position+length], data[position+length:], nil
		}
	}
	return nil, nil, errors.New("video2dvd: descriptor length uses more than four bytes")
}

// esdsObjectTypeIndication returns the DecoderConfigDescriptor's
// objectTypeIndication from an esds full box.
func esdsObjectTypeIndication(esds []byte) (byte, bool) {
	if len(esds) < 5 {
		return 0, false
	}
	return findObjectTypeIndication(esds[4:], 0)
}

func findObjectTypeIndication(data []byte, depth int) (byte, bool) {
	if depth > 4 {
		return 0, false
	}
	for len(data) > 0 {
		tag := data[0]
		payload, rest, err := readDescriptor(data[1:])
		if err != nil {
			return 0, false
		}
		data = rest
		switch tag {
		case 0x03:
			children, err := esDescriptorChildren(payload)
			if err != nil {
				return 0, false
			}
			if value, ok := findObjectTypeIndication(children, depth+1); ok {
				return value, true
			}
		case 0x04:
			if len(payload) < 1 {
				return 0, false
			}
			return payload[0], true
		}
	}
	return 0, false
}

// isAACObjectTypeIndication reports MPEG-4 AAC (0x40) and MPEG-2 AAC
// Main/LC/SSR (0x66-0x68).
func isAACObjectTypeIndication(value byte) bool {
	return value == 0x40 || value >= 0x66 && value <= 0x68
}
