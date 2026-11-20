package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/aethiopicuschan/go-vp9/refbuf"
	. "github.com/aethiopicuschan/video2dvd"
	"github.com/thesyncim/goav/container/webm"
)

// spsBits assembles H.264 SPS syntax elements.
type spsBits struct {
	data []byte
	used int
}

func (b *spsBits) bits(value uint32, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.used%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte(value>>i&1) << (7 - b.used%8)
		b.used++
	}
}

func (b *spsBits) expGolomb(value uint32) {
	length := 0
	for (value+1)>>length > 1 {
		length++
	}
	b.bits(0, length)
	b.bits(value+1, length+1)
}

// testSPS builds a sequence parameter set NAL unit for a 16x16 picture. With
// signal set, the VUI carries video_signal_type with the given range flag
// and, when matrix is not negative, a colour description.
func testSPS(profile uint32, signal, fullRange bool, matrix int) []byte {
	var b spsBits
	b.bits(profile, 8)
	b.bits(0, 8)
	b.bits(31, 8)
	b.expGolomb(0) // seq_parameter_set_id
	if profile == 100 {
		b.expGolomb(1) // chroma_format_idc
		b.expGolomb(0) // bit_depth_luma_minus8
		b.expGolomb(0) // bit_depth_chroma_minus8
		b.bits(0, 1)   // qpprime_y_zero_transform_bypass_flag
		b.bits(1, 1)   // seq_scaling_matrix_present_flag
		for list := range 8 {
			// One explicit flat 4x4 list (sixteen zero deltas), the rest
			// absent.
			if list == 0 {
				b.bits(1, 1)
				for range 16 {
					b.expGolomb(0)
				}
			} else {
				b.bits(0, 1)
			}
		}
	}
	b.expGolomb(0) // log2_max_frame_num_minus4
	b.expGolomb(0) // pic_order_cnt_type
	b.expGolomb(2) // log2_max_pic_order_cnt_lsb_minus4
	b.expGolomb(1) // max_num_ref_frames
	b.bits(0, 1)   // gaps_in_frame_num_value_allowed_flag
	b.expGolomb(0) // pic_width_in_mbs_minus1
	b.expGolomb(0) // pic_height_in_map_units_minus1
	b.bits(1, 1)   // frame_mbs_only_flag
	b.bits(1, 1)   // direct_8x8_inference_flag
	b.bits(0, 1)   // frame_cropping_flag
	b.bits(1, 1)   // vui_parameters_present_flag
	b.bits(1, 1)   // aspect_ratio_info_present_flag
	b.bits(255, 8)
	b.bits(4, 16)
	b.bits(3, 16)
	b.bits(0, 1) // overscan_info_present_flag
	if signal {
		b.bits(1, 1)
		b.bits(5, 3) // video_format
		if fullRange {
			b.bits(1, 1)
		} else {
			b.bits(0, 1)
		}
		if matrix >= 0 {
			b.bits(1, 1)
			b.bits(1, 8)              // colour_primaries
			b.bits(0, 8)              // transfer_characteristics: zero forces emulation prevention
			b.bits(uint32(matrix), 8) // matrix_coefficients
		} else {
			b.bits(0, 1)
		}
	} else {
		b.bits(0, 1)
	}
	b.bits(0, 1) // chroma_loc_info_present_flag
	b.bits(1, 1) // stop bit
	// Escape 00 00 0x sequences as an encoder would.
	nal := []byte{0x67}
	zeros := 0
	for _, value := range b.data {
		if zeros >= 2 && value <= 3 {
			nal = append(nal, 3)
			zeros = 0
		}
		nal = append(nal, value)
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return nal
}

func TestAVCSPSColor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sps       []byte
		want      VideoColor
		wantFound bool
	}{
		{"BT.709 studio", testSPS(66, true, false, 1), VideoColor{Matrix: ColorMatrixBT709}, true},
		{"BT.709 high profile with scaling lists", testSPS(100, true, false, 1), VideoColor{Matrix: ColorMatrixBT709}, true},
		{"SMPTE 170M full range", testSPS(100, true, true, 6), VideoColor{Matrix: ColorMatrixBT601, FullRange: true}, true},
		{"BT.470BG", testSPS(66, true, false, 5), VideoColor{Matrix: ColorMatrixBT601}, true},
		{"BT.2020", testSPS(100, true, false, 9), VideoColor{Matrix: ColorMatrixOther}, true},
		{"RGB", testSPS(100, true, true, 0), VideoColor{Matrix: ColorMatrixOther, FullRange: true}, true},
		{"explicitly unspecified matrix", testSPS(66, true, false, 2), VideoColor{}, true},
		{"range only", testSPS(66, true, true, -1), VideoColor{FullRange: true}, true},
		{"no video_signal_type", testSPS(66, false, false, -1), VideoColor{}, false},
		{"truncated", testSPS(100, true, false, 1)[:9], VideoColor{}, false},
		{"not an SPS", []byte{0x68, 1, 2, 3, 4}, VideoColor{}, false},
	} {
		got, found := ExportAVCSPSColor(tc.sps)
		if got != tc.want || found != tc.wantFound {
			t.Errorf("%s: colour %+v found %v, want %+v found %v", tc.name, got, found, tc.want, tc.wantFound)
		}
	}
}

// probeTestAVCTrack wraps an SPS and optional colr payload into the smallest
// MP4 the prober accepts and returns the probed colour.
func probeTestAVCTrack(t *testing.T, sps []byte, colr []byte) VideoColor {
	t.Helper()
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 30000)
	binary.BigEndian.PutUint32(mdhd[16:20], 30000)
	hdlr := make([]byte, 12)
	copy(hdlr[8:12], "vide")
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	avcC := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	avcC = append(avcC, sps...)
	avcC = append(avcC, 1, 0, byte(len(pps)))
	avcC = append(avcC, pps...)
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:26], 1920)
	binary.BigEndian.PutUint16(entry[26:28], 1080)
	children := mp4TestBox("avcC", avcC)
	if colr != nil {
		children = append(children, mp4TestBox("colr", colr)...)
	}
	stsd := mp4TestBox("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, mp4TestBox("avc1", entry, children))
	mdia := mp4TestBox("mdia", mp4TestBox("mdhd", mdhd), mp4TestBox("hdlr", hdlr), mp4TestBox("minf", mp4TestBox("stbl", stsd)))
	data := append(mp4TestBox("ftyp", []byte("isom\x00\x00\x02\x00")), mp4TestBox("moov", mp4TestBox("trak", mdia))...)
	info, err := ProbeMP4Reader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Tracks) != 1 || info.Tracks[0].Codec != "avc1" {
		t.Fatalf("tracks = %+v", info.Tracks)
	}
	return info.Tracks[0].Color
}

func TestProbeMP4ReportsTrackColor(t *testing.T) {
	nclx := func(matrix uint16, fullRange bool) []byte {
		payload := []byte{'n', 'c', 'l', 'x', 0, 1, 0, 1, byte(matrix >> 8), byte(matrix), 0}
		if fullRange {
			payload[10] = 0x80
		}
		return payload
	}
	untaggedSPS := testSPS(66, false, false, -1)
	for _, tc := range []struct {
		name string
		sps  []byte
		colr []byte
		want VideoColor
	}{
		{"nothing tagged", untaggedSPS, nil, VideoColor{}},
		{"VUI only", testSPS(100, true, false, 1), nil, VideoColor{Matrix: ColorMatrixBT709}},
		{"colr only", untaggedSPS, nclx(1, false), VideoColor{Matrix: ColorMatrixBT709}},
		{"colr full range", untaggedSPS, nclx(6, true), VideoColor{Matrix: ColorMatrixBT601, FullRange: true}},
		{"VUI overrides colr", testSPS(66, true, false, 6), nclx(1, true), VideoColor{Matrix: ColorMatrixBT601}},
		{"VUI range with colr matrix", testSPS(66, true, true, -1), nclx(1, false), VideoColor{Matrix: ColorMatrixBT709, FullRange: true}},
		{"QuickTime nclc colr", untaggedSPS, append([]byte("nclc"), 0, 1, 0, 1, 0, 1), VideoColor{Matrix: ColorMatrixBT709}},
		{"truncated nclx colr is ignored", untaggedSPS, append([]byte("nclx"), 0, 1, 0, 1, 0, 1), VideoColor{}},
		{"ICC profile colr is ignored", untaggedSPS, append([]byte("prof"), make([]byte, 16)...), VideoColor{}},
	} {
		if got := probeTestAVCTrack(t, tc.sps, tc.colr); got != tc.want {
			t.Errorf("%s: track colour %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestWebMAndVP9ColorTags(t *testing.T) {
	for _, tc := range []struct {
		colour webm.VideoColourConfig
		want   VideoColor
	}{
		{webm.VideoColourConfig{}, VideoColor{}},
		{webm.VideoColourConfig{MatrixCoefficients: 1, MatrixCoefficientsSet: true, Range: 1, RangeSet: true}, VideoColor{Matrix: ColorMatrixBT709}},
		{webm.VideoColourConfig{MatrixCoefficients: 6, MatrixCoefficientsSet: true, Range: 2, RangeSet: true}, VideoColor{Matrix: ColorMatrixBT601, FullRange: true}},
		{webm.VideoColourConfig{MatrixCoefficients: 2, MatrixCoefficientsSet: true}, VideoColor{}},
		{webm.VideoColourConfig{MatrixCoefficients: 9, MatrixCoefficientsSet: true}, VideoColor{Matrix: ColorMatrixOther}},
		// Range 3 defers to the matrix and transfer; it is not an explicit
		// full-range signal.
		{webm.VideoColourConfig{MatrixCoefficients: 1, MatrixCoefficientsSet: true, Range: 3, RangeSet: true}, VideoColor{Matrix: ColorMatrixBT709}},
	} {
		if got := ExportWebMContainerColor(tc.colour); got != tc.want {
			t.Errorf("Colour %+v maps to %+v, want %+v", tc.colour, got, tc.want)
		}
	}
	// VP9 color_space: 0 unknown, 1 BT.601, 2 BT.709, 3 SMPTE 170M,
	// 4 SMPTE 240M, 5 BT.2020, 7 sRGB.
	for colorSpace, want := range map[int]ColorMatrix{0: ColorMatrixUnspecified, 1: ColorMatrixBT601, 2: ColorMatrixBT709, 3: ColorMatrixBT601, 4: ColorMatrixOther, 5: ColorMatrixOther, 7: ColorMatrixOther} {
		if got := ExportVP9ColorMatrix(colorSpace); got != want {
			t.Errorf("VP9 color_space %d maps to %d, want %d", colorSpace, got, want)
		}
	}
}

// Studio-range levels of high-bit-depth video must land on their 8-bit code
// values; scaling by 255/1023 put 10-bit white (940) on 234.
func TestCopyVP9HighBitDepthFrameKeepsStudioLevels(t *testing.T) {
	for _, tc := range []struct {
		bitDepth int
		sample   uint16
		want     byte
	}{
		{10, 64, 16}, {10, 940, 235}, {10, 960, 240}, {10, 512, 128}, {10, 1023, 255}, {10, 0, 0}, {10, 2, 1}, {10, 1, 0},
		{12, 256, 16}, {12, 3760, 235}, {12, 4095, 255}, {12, 2048, 128},
	} {
		frame := refbuf.NewFrame16(4, 4, tc.bitDepth)
		for i := range frame.Y {
			frame.Y[i] = tc.sample
		}
		for i := range frame.U {
			frame.U[i], frame.V[i] = tc.sample, tc.sample
		}
		decoded, err := ExportCopyVP9HighBitDepth(frame, tc.bitDepth, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, plane := range [][]byte{decoded.Y, decoded.U, decoded.V} {
			for _, value := range plane {
				if value != tc.want {
					t.Fatalf("%d-bit sample %d became %d, want %d", tc.bitDepth, tc.sample, value, tc.want)
				}
			}
		}
	}
}
