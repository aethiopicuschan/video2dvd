package video2dvd_test

import (
	"bytes"
	"math"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

// mpeg2TestLumaPSNR compares the encoder's reconstruction with its input.
func mpeg2TestLumaPSNR(source, reconstruction DecodedVideoFrame) float64 {
	var squared uint64
	for i := range source.Y {
		delta := int(source.Y[i]) - int(reconstruction.Y[i])
		squared += uint64(delta * delta)
	}
	if squared == 0 {
		return 100
	}
	return 10 * math.Log10(255*255*float64(len(source.Y))/float64(squared))
}

// noisyMPEG2TestFrame returns content that predicts badly and codes
// expensively in every picture.
func noisyMPEG2TestFrame(profile Profile, index int, seed *uint32) DecodedVideoFrame {
	frame := movingMPEG2TestFrame(profile, index*7)
	for _, plane := range [][]byte{frame.Y, frame.U, frame.V} {
		for i := range plane {
			*seed = *seed*1664525 + 1013904223
			plane[i] = byte(max(16, min(235, int(plane[i])+int(*seed>>24)%96-48)))
		}
	}
	return frame
}

func TestMPEG2RateControlFollowsContentItDidNotExpect(t *testing.T) {
	// Budgets rest on the complexity of earlier P pictures. The estimate is
	// wrong whenever the content changes, and neither direction may cost a
	// picture its quality for the rest of the GOP.
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, VideoBitrate: 3_000_000})
	if err != nil {
		t.Fatal(err)
	}
	const gop = ExportMPEG2GOPPictures
	encode := func(frames []DecodedVideoFrame) (psnr []float64, sizes []int) {
		var stream bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&stream, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			before := stream.Len()
			if err := encoder.WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			psnr = append(psnr, mpeg2TestLumaPSNR(frame, ExportMPEG2EncoderReference(encoder)))
			sizes = append(sizes, stream.Len()-before)
		}
		return psnr, sizes
	}
	lowest := func(values []float64) float64 {
		result := values[0]
		for _, value := range values {
			result = min(result, value)
		}
		return result
	}

	t.Run("motion starts in a still scene", func(t *testing.T) {
		// Two still GOPs make every P picture nearly free. Then the
		// picture starts to pan in the middle of a GOP.
		const start = 2*gop + 4
		var frames []DecodedVideoFrame
		for index := range 4 * gop {
			frames = append(frames, texturedMPEG2TestFrame(profile, 0, 3.5*float64(max(0, index-start)), 0))
		}
		psnr, sizes := encode(frames)
		// The first moving picture must leave the rest of its GOP enough to
		// follow the motion as well as the next GOP does.
		first, rest, next := psnr[start], lowest(psnr[start+1:3*gop]), lowest(psnr[3*gop:])
		if rest < next-2 || first < next-2 {
			t.Fatalf("PSNR after motion starts: %.1f dB, then at least %.1f dB to the end of the GOP, against %.1f dB in the next GOP (sizes %v)",
				first, rest, next, sizes[start-1:3*gop+2])
		}
	})

	t.Run("unrelated pictures follow each other", func(t *testing.T) {
		// A still GOP makes P pictures look nearly free. Then every
		// picture is a scene change, which is what flashes, strobes and
		// very fast editing look like to the encoder.
		var frames []DecodedVideoFrame
		for index := range 4 * gop {
			if index < gop {
				frames = append(frames, flatShadedMPEG2TestFrame(profile, 0, 0, 0))
			} else {
				frames = append(frames, flatShadedMPEG2TestFrame(profile, index%3, float64(index*37%200), float64(index*53%160)))
			}
		}
		psnr, sizes := encode(frames)
		// The first unrelated pictures are budgeted as single scene
		// changes with cheap pictures after them. Once a GOP has shown
		// otherwise, no picture may take the bits of the ones behind it:
		// measured, the largest is 1.7 times the mean, against 2.7 times
		// when every scene change is budgeted as if it were the only one.
		settled := sizes[2*gop+gop/2:]
		largest, total := 0, 0
		for _, size := range settled {
			largest, total = max(largest, size), total+size
		}
		if mean := float64(total) / float64(len(settled)); float64(largest) > 2*mean {
			t.Fatalf("largest picture in a run of scene changes has %d bytes, %.1f times the mean (PSNR %.1f, sizes %v)",
				largest, float64(largest)/mean, psnr[gop:], sizes[gop:])
		}
	})

	t.Run("a still scene follows a busy one", func(t *testing.T) {
		// Two GOPs of noise raise the complexity estimate far above what
		// the flat scene after them needs, once at a GOP boundary and once
		// inside a GOP.
		for _, cut := range []int{2 * gop, 2*gop + 5} {
			seed := uint32(11)
			var frames []DecodedVideoFrame
			for index := range 3 * gop {
				if index < cut {
					frames = append(frames, noisyMPEG2TestFrame(profile, index, &seed))
				} else {
					frames = append(frames, flatShadedMPEG2TestFrame(profile, 1, 0, 0))
				}
			}
			psnr, sizes := encode(frames)
			// The first picture of the scene is budgeted before any
			// picture has shown how cheap the scene is, from what the
			// noise left of the GOP. It must still be a usable picture,
			// and every picture after it must build on it with the bits
			// the scene leaves over.
			scene := psnr[cut:]
			improving := true
			for index := 1; index < len(scene); index++ {
				improving = improving && scene[index] > scene[index-1]-0.5
			}
			if first, last := scene[0], scene[len(scene)-1]; first < 27 || last < first+5 || !improving {
				t.Errorf("cut at picture %d: flat scene starts at %.1f dB and ends at %.1f dB (PSNR %.1f, sizes %v)", cut, first, last, scene, sizes[cut:])
			}
		}
	})
}
