package video2dvd_test

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

// bs775 is the ITU-R BS.775 -3 dB coefficient.
const bs775 = math.Sqrt2 / 2

// The matrix for every Opus channel mapping family 1 layout puts front left
// only into Lo, centre into both at -3 dB, surrounds on their own side at
// -3 dB, a back centre into both at -6 dB and LFE nowhere, scaled by one over
// the coefficient sum of an output.
func TestStereoDownmixCoefficients(t *testing.T) {
	cases := []struct {
		channels    int
		left, right []float64
	}{
		// L C R
		{3, []float64{1, bs775, 0}, []float64{0, bs775, 1}},
		// L R Lb Rb: no centre, so the sum is 1 + 0.7071.
		{4, []float64{1, 0, bs775, 0}, []float64{0, 1, 0, bs775}},
		// L C R Lb Rb
		{5, []float64{1, bs775, 0, bs775, 0}, []float64{0, bs775, 1, 0, bs775}},
		// L C R Lb Rb LFE
		{6, []float64{1, bs775, 0, bs775, 0, 0}, []float64{0, bs775, 1, 0, bs775, 0}},
		// L C R Ls Rs Cb LFE
		{7, []float64{1, bs775, 0, bs775, 0, 0.5, 0}, []float64{0, bs775, 1, 0, bs775, 0.5, 0}},
		// L C R Ls Rs Lb Rb LFE
		{8, []float64{1, bs775, 0, bs775, 0, bs775, 0, 0}, []float64{0, bs775, 1, 0, bs775, 0, bs775, 0}},
	}
	for _, c := range cases {
		left, right, err := ExportVorbisStereoDownmix(c.channels)
		if err != nil {
			t.Fatalf("%d channels: %v", c.channels, err)
		}
		sum := 0.0
		for _, gain := range c.left {
			sum += gain
		}
		for i := range c.left {
			if math.Abs(left[i]-c.left[i]/sum) > 1e-12 || math.Abs(right[i]-c.right[i]/sum) > 1e-12 {
				t.Fatalf("%d channels: gains %v / %v, want %v / %v divided by %.4f", c.channels, left, right, c.left, c.right, sum)
			}
		}
		gotLeft, gotRight := 0.0, 0.0
		for i := range left {
			gotLeft += left[i]
			gotRight += right[i]
		}
		if math.Abs(gotLeft-1) > 1e-12 || math.Abs(gotRight-1) > 1e-12 {
			t.Fatalf("%d channels: coefficient sums %.6f / %.6f, want 1", c.channels, gotLeft, gotRight)
		}
	}
	if _, _, err := ExportVorbisStereoDownmix(2); err == nil {
		t.Fatal("stereo has no downmix layout and must be refused")
	}
}

// Synthetic 5.1 frames in Vorbis order with one channel active at a time
// land in Lo/Ro with exactly the BS.775 gains, and full scale on every
// channel reaches but never exceeds full scale.
func TestStereoDownmixSamples(t *testing.T) {
	g := 1 / (1 + 2*bs775)
	want := [6][2]float64{{g, 0}, {bs775 * g, bs775 * g}, {0, g}, {bs775 * g, 0}, {0, bs775 * g}, {0, 0}}
	for channel := range 6 {
		frame := make([]float32, 6)
		frame[channel] = 0.5
		got, err := ExportApplyVorbisStereoDownmix(6, frame)
		if err != nil {
			t.Fatal(err)
		}
		for side := range 2 {
			if expected := int16(math.Round(0.5 * want[channel][side] * 32768)); got[side] != expected {
				t.Fatalf("channel %d: output %v, want %d on side %d", channel, got, expected, side)
			}
		}
	}
	for _, level := range []float32{1, -1, 0.999} {
		frame := []float32{level, level, level, level, level, level}
		got, err := ExportApplyVorbisStereoDownmix(6, frame)
		if err != nil {
			t.Fatal(err)
		}
		expected := int16(max(min(math.Round(float64(level)*32768), 32767), -32768))
		if got[0] != expected || got[1] != expected {
			t.Fatalf("full scale %v: output %v, want %d on both sides", level, got, expected)
		}
	}
	// A lossy decoder can overshoot; the result saturates instead of wrapping.
	got, err := ExportApplyVorbisStereoDownmix(6, []float32{1.5, 1.5, 1.5, 1.5, 1.5, 1.5, -1.5, -1.5, -1.5, -1.5, -1.5, -1.5})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 32767 || got[1] != 32767 || got[2] != -32768 || got[3] != -32768 {
		t.Fatalf("overshoot output %v, want saturation", got)
	}
	if _, err := ExportApplyVorbisStereoDownmix(6, make([]float32, 7)); err == nil {
		t.Fatal("a partial frame must be refused")
	}
}

// testMultichannelOpusHead builds an RFC 7845 identification header.
func testMultichannelOpusHead(channels, family, streams, coupled int, mapping []byte) []byte {
	head := []byte("OpusHead")
	head = append(head, 1, byte(channels))
	head = binary.LittleEndian.AppendUint16(head, 312)
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = append(head, 0, 0, byte(family))
	if family != 0 {
		head = append(head, byte(streams), byte(coupled))
		head = append(head, mapping...)
	}
	return head
}

// Only what the built-in decoders cannot turn into stereo is refused before
// encoding, and the message says why.
func TestValidateInputAudioChannelsMultichannel(t *testing.T) {
	cases := []struct {
		name  string
		track TrackInfo
		want  string // empty when the track is accepted
	}{
		{"stereo Opus", TrackInfo{Codec: "opus", Channels: 2, CodecConfig: testMultichannelOpusHead(2, 0, 0, 0, nil)}, ""},
		{"5.1 Opus family 1", TrackInfo{Codec: "opus", Channels: 6, CodecConfig: testMultichannelOpusHead(6, 1, 4, 2, []byte{0, 4, 1, 2, 3, 5})}, ""},
		{"7.1 Opus family 1", TrackInfo{Codec: "opus", Channels: 8, CodecConfig: testMultichannelOpusHead(8, 1, 5, 3, []byte{0, 6, 1, 2, 3, 4, 5, 7})}, ""},
		{"Opus head overrides the container", TrackInfo{Codec: "opus", Channels: 2, CodecConfig: testMultichannelOpusHead(6, 255, 6, 0, []byte{0, 1, 2, 3, 4, 5})}, "mapping family 255"},
		{"ambisonic Opus", TrackInfo{Codec: "opus", Channels: 4, CodecConfig: testMultichannelOpusHead(4, 2, 4, 0, []byte{0, 1, 2, 3})}, "mapping family 2"},
		{"Opus without a header", TrackInfo{Codec: "opus", Channels: 6}, "6-channel Opus audio cannot be downmixed"},
		{"5.1 AAC", TrackInfo{Codec: "mp4a", Channels: 2, CodecConfig: []byte{0x11, 0xb0}}, "6-channel AAC audio is not supported"},
		{"5.1 AC-3", TrackInfo{Codec: "ac-3", Channels: 6}, ""},
	}
	for _, c := range cases {
		err := ExportValidateInputAudioChannels(c.track)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want one containing %q", c.name, err, c.want)
		}
	}
}
