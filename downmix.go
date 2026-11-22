package video2dvd

import (
	"errors"
	"fmt"
	"math"
)

// speakerPosition names the loudspeaker a decoded channel is meant for. The
// downmix matrix is defined on positions rather than on channel indices, so
// that every codec only has to state the order in which it delivers them.
type speakerPosition uint8

const (
	speakerFrontLeft speakerPosition = iota
	speakerFrontRight
	speakerFrontCentre
	speakerLFE
	speakerSideLeft
	speakerSideRight
	speakerBackLeft
	speakerBackRight
	speakerBackCentre
)

// downmixMinus3dB is the ITU-R BS.775 coefficient (1/sqrt 2, about 0.7071)
// with which the centre and the surround channels enter the stereo pair.
const downmixMinus3dB = math.Sqrt2 / 2

// vorbisChannelOrder lists, by channel count, the order in which Opus channel
// mapping family 1 delivers its channels (RFC 7845 section 5.1.1.2, which
// uses the Vorbis I order). Only the multichannel layouts are listed: mono and
// stereo never go through the downmix.
var vorbisChannelOrder = map[int][]speakerPosition{
	3: {speakerFrontLeft, speakerFrontCentre, speakerFrontRight},
	4: {speakerFrontLeft, speakerFrontRight, speakerBackLeft, speakerBackRight},
	5: {speakerFrontLeft, speakerFrontCentre, speakerFrontRight, speakerBackLeft, speakerBackRight},
	6: {speakerFrontLeft, speakerFrontCentre, speakerFrontRight, speakerBackLeft, speakerBackRight, speakerLFE},
	7: {speakerFrontLeft, speakerFrontCentre, speakerFrontRight, speakerSideLeft, speakerSideRight, speakerBackCentre, speakerLFE},
	8: {speakerFrontLeft, speakerFrontCentre, speakerFrontRight, speakerSideLeft, speakerSideRight, speakerBackLeft, speakerBackRight, speakerLFE},
}

// stereoDownmix holds the per-channel gains that fold one multichannel layout
// into a stereo pair. left[i] and right[i] weight input channel i.
type stereoDownmix struct {
	left, right []float64
}

// newStereoDownmix builds the ITU-R BS.775 downmix for layout:
//
//	Lo = L + 0.7071 C + 0.7071 Ls + 0.7071 Lb + 0.5 Cb
//	Ro = R + 0.7071 C + 0.7071 Rs + 0.7071 Rb + 0.5 Cb
//
// LFE is discarded, as BS.775 recommends, because a stereo pair has no
// low-frequency channel and adding it would only make bass louder than the
// mix intended. A single back centre is split equally into both outputs at
// -3 dB below a side surround (0.5 each), so it keeps the power a surround
// channel has. The gains are then divided by the larger of the two outputs'
// coefficient sums, so that inputs at full scale on every channel stay at or
// below full scale: the DVD's 16-bit LPCM cannot carry an overload and a
// clipped downmix would distort exactly the loudest passages. A layout
// without centre or surrounds is normalised by its own, smaller sum.
func newStereoDownmix(layout []speakerPosition) (stereoDownmix, error) {
	if len(layout) == 0 {
		return stereoDownmix{}, errors.New("video2dvd: downmix layout is empty")
	}
	mix := stereoDownmix{left: make([]float64, len(layout)), right: make([]float64, len(layout))}
	var sumLeft, sumRight float64
	for i, position := range layout {
		var left, right float64
		switch position {
		case speakerFrontLeft:
			left = 1
		case speakerFrontRight:
			right = 1
		case speakerFrontCentre:
			left, right = downmixMinus3dB, downmixMinus3dB
		case speakerLFE:
		case speakerSideLeft, speakerBackLeft:
			left = downmixMinus3dB
		case speakerSideRight, speakerBackRight:
			right = downmixMinus3dB
		case speakerBackCentre:
			left, right = downmixMinus3dB*downmixMinus3dB, downmixMinus3dB*downmixMinus3dB
		default:
			return stereoDownmix{}, fmt.Errorf("video2dvd: downmix layout has unknown speaker position %d", position)
		}
		mix.left[i], mix.right[i] = left, right
		sumLeft += left
		sumRight += right
	}
	scale := max(sumLeft, sumRight)
	if scale == 0 {
		return stereoDownmix{}, errors.New("video2dvd: downmix layout has no channel that reaches the stereo pair")
	}
	for i := range layout {
		mix.left[i] /= scale
		mix.right[i] /= scale
	}
	return mix, nil
}

// apply folds interleaved float PCM with len(m.left) channels, nominally in
// [-1, 1], into interleaved 16-bit stereo in dst, which must hold two samples
// for every input frame. Mixing in floating point and rounding once keeps the
// downmix from adding a second quantisation step to the decoder's output.
func (m stereoDownmix) apply(dst []int16, src []float32) error {
	channels := len(m.left)
	if channels == 0 || len(src)%channels != 0 {
		return errors.New("video2dvd: downmix input does not hold whole frames")
	}
	frames := len(src) / channels
	if len(dst) < frames*2 {
		return errors.New("video2dvd: downmix output buffer is too small")
	}
	for frame := range frames {
		input := src[frame*channels : (frame+1)*channels]
		var left, right float64
		for i, sample := range input {
			left += float64(sample) * m.left[i]
			right += float64(sample) * m.right[i]
		}
		dst[frame*2] = pcmFloatToS16(left)
		dst[frame*2+1] = pcmFloatToS16(right)
	}
	return nil
}

// pcmFloatToS16 converts one sample from the [-1, 1] float range to 16-bit
// PCM with rounding. A lossy decoder can overshoot full scale slightly, so the
// result saturates instead of wrapping; NaN, which no valid stream produces,
// becomes silence.
func pcmFloatToS16(value float64) int16 {
	scaled := math.Round(value * 32768)
	switch {
	case math.IsNaN(scaled):
		return 0
	case scaled > math.MaxInt16:
		return math.MaxInt16
	case scaled < math.MinInt16:
		return math.MinInt16
	}
	return int16(scaled)
}
