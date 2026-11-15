package video2dvd

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const maxMPEG2HeaderScan = 4 << 20

// MPEG2VideoInfo describes the first MPEG-2 sequence in an elementary stream.
type MPEG2VideoInfo struct {
	Width                int
	Height               int
	Aspect               AspectRatio
	FrameRateNumerator   int
	FrameRateDenominator int
	Profile              string
	Level                string
}

// ProbeMPEG2Video parses sequence and sequence-extension headers without
// decoding picture data. It accepts Main Profile at Main Level streams used by
// DVD-Video and rejects MPEG-1 streams.
func ProbeMPEG2Video(path string) (MPEG2VideoInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return MPEG2VideoInfo{}, errors.New("video2dvd: MPEG-2 video stream is not a regular file")
	}
	scanSize := min(info.Size(), maxMPEG2HeaderScan)
	data := make([]byte, int(scanSize))
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, scanSize), data); err != nil && err != io.EOF {
		return MPEG2VideoInfo{}, err
	}
	return parseMPEG2VideoHeaders(data)
}

func parseMPEG2VideoHeaders(data []byte) (MPEG2VideoInfo, error) {
	sequence := findMPEGStartCode(data, 0xb3, 0)
	if sequence < 0 || len(data)-sequence < 8 {
		return MPEG2VideoInfo{}, errors.New("video2dvd: MPEG-2 sequence header not found")
	}
	sequenceBits := uint32(data[sequence+4])<<24 | uint32(data[sequence+5])<<16 | uint32(data[sequence+6])<<8 | uint32(data[sequence+7])
	width := int(sequenceBits >> 20)
	height := int((sequenceBits >> 8) & 0x0fff)
	aspectCode := int((sequenceBits >> 4) & 0x0f)
	frameRateCode := int(sequenceBits & 0x0f)
	if width == 0 || height == 0 {
		return MPEG2VideoInfo{}, errors.New("video2dvd: invalid MPEG-2 sequence dimensions")
	}
	var aspect AspectRatio
	switch aspectCode {
	case 2:
		aspect = AspectRatio4x3
	case 3:
		aspect = AspectRatio16x9
	default:
		return MPEG2VideoInfo{}, fmt.Errorf("video2dvd: unsupported MPEG-2 display aspect code %d", aspectCode)
	}
	numerator, denominator, ok := mpegFrameRate(frameRateCode)
	if !ok {
		return MPEG2VideoInfo{}, fmt.Errorf("video2dvd: unsupported MPEG-2 frame-rate code %d", frameRateCode)
	}

	extension := findMPEGStartCode(data, 0xb5, sequence+8)
	if extension < 0 || len(data)-extension < 10 {
		return MPEG2VideoInfo{}, errors.New("video2dvd: MPEG-2 sequence extension not found")
	}
	bits := bitReader{data: data[extension+4:]}
	extensionID, err := bits.read(4)
	if err != nil || extensionID != 1 {
		return MPEG2VideoInfo{}, errors.New("video2dvd: invalid MPEG-2 sequence extension")
	}
	profileLevel, err := bits.read(8)
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	if _, err = bits.read(1); err != nil { // progressive_sequence
		return MPEG2VideoInfo{}, err
	}
	chroma, err := bits.read(2)
	if err != nil || chroma != 1 {
		return MPEG2VideoInfo{}, errors.New("video2dvd: DVD video requires 4:2:0 MPEG-2 video")
	}
	widthExtension, err := bits.read(2)
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	heightExtension, err := bits.read(2)
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	width |= int(widthExtension) << 12
	height |= int(heightExtension) << 12
	if _, err = bits.read(12); err != nil { // bit_rate_extension
		return MPEG2VideoInfo{}, err
	}
	if marker, markerErr := bits.read(1); markerErr != nil || marker != 1 {
		return MPEG2VideoInfo{}, errors.New("video2dvd: invalid MPEG-2 sequence-extension marker")
	}
	if _, err = bits.read(8); err != nil { // vbv_buffer_size_extension
		return MPEG2VideoInfo{}, err
	}
	if _, err = bits.read(1); err != nil { // low_delay
		return MPEG2VideoInfo{}, err
	}
	frameRateN, err := bits.read(2)
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	frameRateD, err := bits.read(5)
	if err != nil {
		return MPEG2VideoInfo{}, err
	}
	numerator *= int(frameRateN + 1)
	denominator *= int(frameRateD + 1)
	numerator, denominator = reduceRatio(numerator, denominator)

	profileCode := int((profileLevel >> 4) & 0x07)
	levelCode := int(profileLevel & 0x0f)
	if profileCode != 4 || levelCode != 8 {
		return MPEG2VideoInfo{}, fmt.Errorf("video2dvd: DVD requires MPEG-2 Main Profile at Main Level, got profile %d level %d", profileCode, levelCode)
	}
	return MPEG2VideoInfo{
		Width:                width,
		Height:               height,
		Aspect:               aspect,
		FrameRateNumerator:   numerator,
		FrameRateDenominator: denominator,
		Profile:              "main",
		Level:                "main",
	}, nil
}

func findMPEGStartCode(data []byte, code byte, start int) int {
	for i := start; i+4 <= len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && data[i+3] == code {
			return i
		}
	}
	return -1
}

func mpegFrameRate(code int) (int, int, bool) {
	switch code {
	case 1:
		return 24000, 1001, true
	case 2:
		return 24, 1, true
	case 3:
		return 25, 1, true
	case 4:
		return 30000, 1001, true
	case 5:
		return 30, 1, true
	case 6:
		return 50, 1, true
	case 7:
		return 60000, 1001, true
	case 8:
		return 60, 1, true
	default:
		return 0, 0, false
	}
}

type bitReader struct {
	data []byte
	bit  int
}

func (r *bitReader) read(count int) (uint32, error) {
	if count < 0 || count > 32 || count > len(r.data)*8-r.bit {
		return 0, io.ErrUnexpectedEOF
	}
	var value uint32
	for i := range count {
		position := r.bit + i
		value = value<<1 | uint32((r.data[position/8]>>(7-position%8))&1)
	}
	r.bit += count
	return value, nil
}

func reduceRatio(numerator, denominator int) (int, int) {
	a, b := numerator, denominator
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return numerator, denominator
	}
	return numerator / a, denominator / a
}
