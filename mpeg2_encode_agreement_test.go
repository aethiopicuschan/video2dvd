package video2dvd_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// The tests in this file hold the encoder to the one criterion that matters
// for a predictive codec: an independent decoder must rebuild the pictures the
// encoder predicted from. A wrong predictor reset or vector code leaves the
// stream decodable and shifts or smears whole macroblocks, so the comparison
// is per sample.

// mpeg2IDCTDisagreementLimit is the share of samples on which a decoder may
// differ from the encoder, by one level each. The two inverse DCTs are
// independent implementations of a real-valued transform and round a few
// samples per picture differently, which H.262 Annex A allows; measured with
// ffmpeg's floating-point IDCT it is at most 9 luma samples of 345600. A
// wrong predictor or vector changes most samples of at least one macroblock,
// and a wrong inverse quantization a fifth of the samples of every block it
// touches.
const mpeg2IDCTDisagreementLimit = 1e-4

// mpeg2TestTexture is a deterministic picture content that can be sampled at
// fractional positions, so that it can be moved by half samples. The chirp
// term keeps it from repeating within the motion search range.
func mpeg2TestTexture(variant int, fx, fy float64) float64 {
	switch variant {
	case 0:
		return 126 + 50*math.Sin(fx/9)*math.Cos(fy/7) + 25*math.Sin((fx+fy)/3.1) + 20*math.Sin((fx*fx+fy*fy)/2900)
	case 1:
		return 120 + 55*math.Cos(fx/5.3)*math.Sin(fy/11) + 30*math.Sin((fx-2*fy)/4.3) + 15*math.Cos((fx*fy)/1700)
	default:
		return 130 + 45*math.Sin(fx/3.7+fy/6.1) + 40*math.Cos(fx/13)*math.Cos(fy/4.1) + 12*math.Sin((fx*fx)/900)
	}
}

func newMPEG2TestFrame(profile Profile) DecodedVideoFrame {
	return DecodedVideoFrame{
		Width:  profile.FrameWidth,
		Height: profile.FrameHeight,
		Y:      make([]byte, profile.FrameWidth*profile.FrameHeight),
		U:      make([]byte, profile.FrameWidth*profile.FrameHeight/4),
		V:      make([]byte, profile.FrameWidth*profile.FrameHeight/4),
	}
}

// texturedMPEG2TestFrame draws a texture displaced by (offsetX, offsetY) luma
// samples.
func texturedMPEG2TestFrame(profile Profile, variant int, offsetX, offsetY float64) DecodedVideoFrame {
	return drawMPEG2TestFrame(profile, variant, offsetX, offsetY, 1, 1)
}

// flatShadedMPEG2TestFrame draws the texture enlarged and reduced to a few
// luma levels: large flat areas with sharp outlines, like cel animation. Each
// variant has its own set of levels, so that, unlike the smooth textures, two
// scenes do not predict each other at any displacement.
func flatShadedMPEG2TestFrame(profile Profile, variant int, offsetX, offsetY float64) DecodedVideoFrame {
	return drawMPEG2TestFrame(profile, variant, offsetX, offsetY, 48, 4)
}

func drawMPEG2TestFrame(profile Profile, variant int, offsetX, offsetY, lumaStep, zoom float64) DecodedVideoFrame {
	frame := newMPEG2TestFrame(profile)
	for y := range frame.Height {
		for x := range frame.Width {
			value := mpeg2TestTexture(variant, (float64(x)-offsetX)/zoom, (float64(y)-offsetY)/zoom)
			if lumaStep > 1 {
				value = math.Floor(value/lumaStep)*lumaStep + lumaStep/3*float64(variant)
			}
			frame.Y[y*frame.Width+x] = byte(max(16, min(235, value)))
		}
	}
	for y := range frame.Height / 2 {
		for x := range frame.Width / 2 {
			fx, fy := float64(x)-offsetX/2, float64(y)-offsetY/2
			frame.U[y*frame.Width/2+x] = byte(128 + 50*math.Sin(fx/11+float64(variant))*math.Sin(fy/5))
			frame.V[y*frame.Width/2+x] = byte(128 + 50*math.Cos(fx/6)*math.Sin(fy/13+float64(variant)))
		}
	}
	return frame
}

// mpeg2CodedTestSequence is an encoded sequence with what the encoder knew
// about each picture.
type mpeg2CodedTestSequence struct {
	stream []byte
	// types holds the picture_coding_type of each picture: 1 for I, 2 for P.
	types []uint32
	// references holds the encoder's own reconstruction of each picture.
	references []DecodedVideoFrame
	// macroblocks holds the analysis the encoder made of each picture as a
	// P picture. It is nil for the first picture, and unused where the
	// encoder coded an I picture.
	macroblocks [][]TestMPEG2PredictiveMacroblock
}

func encodeMPEG2TestSequence(t *testing.T, profile Profile, frames []DecodedVideoFrame) mpeg2CodedTestSequence {
	t.Helper()
	var stream bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&stream, profile)
	if err != nil {
		t.Fatal(err)
	}
	var coded mpeg2CodedTestSequence
	for index, frame := range frames {
		var macroblocks []TestMPEG2PredictiveMacroblock
		if index > 0 {
			motion, lambda := ExportMPEG2SearchState(encoder)
			macroblocks = ExportAnalyzeMPEG2PredictivePicture(frame, ExportMPEG2EncoderReference(encoder),
				profile.FrameWidth/16, profile.FrameHeight/16, motion, lambda)
		}
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		coded.macroblocks = append(coded.macroblocks, macroblocks)
		coded.references = append(coded.references, ExportMPEG2EncoderReference(encoder))
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	coded.stream = stream.Bytes()
	coded.types, _ = mpeg2PictureTypesAndSizes(coded.stream)
	if len(coded.types) != len(frames) {
		t.Fatalf("stream holds %d pictures, want %d", len(coded.types), len(frames))
	}
	return coded
}

// decodeMPEG2TestStream decodes an elementary stream with ffmpeg into planar
// 4:2:0 frames and fails on any decoder complaint.
func decodeMPEG2TestStream(t *testing.T, ffmpeg string, stream []byte) []byte {
	t.Helper()
	directory := t.TempDir()
	streamPath := filepath.Join(directory, "stream.m2v")
	if err := os.WriteFile(streamPath, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	decodedPath := filepath.Join(directory, "decoded.yuv")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// -xerror and explode turn any bitstream error into a failure instead of
	// concealing it. The floating-point IDCT is the decoder's closest match
	// to the encoder's reference IDCT.
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "warning", "-xerror", "-err_detect", "explode", "-idct", "faani",
		"-i", streamPath, "-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "yuv420p", decodedPath, "-y")
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("decode MPEG-2 stream: %v: %s", err, output)
	}
	decoded, err := os.ReadFile(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// requireMPEG2DecoderAgreement compares a decoder's output with the
// encoder's reconstruction of every picture.
func requireMPEG2DecoderAgreement(t *testing.T, profile Profile, coded mpeg2CodedTestSequence, decoded []byte) {
	t.Helper()
	lumaSize := profile.FrameWidth * profile.FrameHeight
	frameSize := lumaSize * 3 / 2
	if len(decoded) != len(coded.references)*frameSize {
		t.Fatalf("decoded %d bytes, want %d pictures of %d bytes", len(decoded), len(coded.references), frameSize)
	}
	for index, reference := range coded.references {
		picture := decoded[index*frameSize : (index+1)*frameSize]
		for _, plane := range []struct {
			name             string
			decoded, encoder []byte
		}{
			{"Y", picture[:lumaSize], reference.Y},
			{"U", picture[lumaSize : lumaSize*5/4], reference.U},
			{"V", picture[lumaSize*5/4:], reference.V},
		} {
			differing, largest := 0, 0
			for i := range plane.decoded {
				if delta := int(plane.decoded[i]) - int(plane.encoder[i]); delta != 0 {
					differing++
					largest = max(largest, max(delta, -delta))
				}
			}
			if largest > 1 || float64(differing) > mpeg2IDCTDisagreementLimit*float64(len(plane.decoded)) {
				t.Errorf("picture %d (type %d) plane %s: decoder differs from the encoder's reconstruction on %d of %d samples, by up to %d",
					index, coded.types[index], plane.name, differing, len(plane.decoded), largest)
			}
		}
	}
}

func TestMPEG2DecoderAgreesOnWideMotionVectors(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 motion vector sample")
	}
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	// Displacements per picture in luma samples. A displacement d needs the
	// vector -2d in half samples, so +32 and -31.5 are the two ends of the
	// vector range. One GOP: every picture after the first is a P picture.
	steps := [][2]float64{
		{32, 0}, {-31.5, 0}, {0, 32}, {0, -31.5},
		{32, 32}, {-31.5, -31.5}, {32, -31.5}, {-31.5, 32},
		{16.5, -8}, {-0.5, 0.5}, {3, -21.5}, {-32, -32}, {31.5, 31.5}, {0.5, 0},
	}
	frames := []DecodedVideoFrame{texturedMPEG2TestFrame(profile, 0, 0, 0)}
	offsetX, offsetY := 0.0, 0.0
	for _, step := range steps {
		offsetX, offsetY = offsetX+step[0], offsetY+step[1]
		frames = append(frames, texturedMPEG2TestFrame(profile, 0, offsetX, offsetY))
	}
	coded := encodeMPEG2TestSequence(t, profile, frames)
	requireMPEG2DecoderAgreement(t, profile, coded, decodeMPEG2TestStream(t, ffmpeg, coded.stream))

	mbWidth, mbHeight := profile.FrameWidth/16, profile.FrameHeight/16
	// Whether a vector at each end of the range was used in each direction,
	// and by a macroblock on the picture edge the vector points away from.
	var used, usedAtEdge [2][2]bool
	for index, macroblocks := range coded.macroblocks {
		if index == 0 {
			continue
		}
		if coded.types[index] != 2 {
			t.Fatalf("picture %d has type %d, want a P picture", index, coded.types[index])
		}
		for position, macroblock := range macroblocks {
			column, row := position%mbWidth, position/mbWidth
			motionX, motionY := ExportMPEG2MacroblockMotion(macroblock)
			for axis, component := range [2]struct{ motion, cell, cells, size int }{
				{motionX, column, mbWidth, profile.FrameWidth},
				{motionY, row, mbHeight, profile.FrameHeight},
			} {
				low, high := ExportMPEG2MotionBounds(component.cell*16, component.size)
				if component.motion < low || component.motion > high {
					t.Fatalf("picture %d macroblock %d,%d: vector component %d outside %d..%d", index, column, row, component.motion, low, high)
				}
				if component.motion == ExportMPEG2MotionLow {
					used[axis][0] = true
					usedAtEdge[axis][0] = usedAtEdge[axis][0] || component.cell == component.cells-1
				}
				if component.motion == ExportMPEG2MotionHigh {
					used[axis][1] = true
					usedAtEdge[axis][1] = usedAtEdge[axis][1] || component.cell == 0
				}
			}
		}
	}
	if used != [2][2]bool{{true, true}, {true, true}} || usedAtEdge != used {
		t.Fatalf("vector range ends used (x low/high, y low/high): %v, on the facing picture edge: %v; want all", used, usedAtEdge)
	}
}

func TestMPEG2DecoderAgreesOnIntraMacroblocksAndSceneChanges(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 scene change sample")
	}
	for _, standard := range []VideoStandard{VideoStandardNTSC, VideoStandardPAL} {
		profile, err := ExportProfile(Options{Standard: standard, Aspect: AspectRatio16x9, VideoBitrate: 5_000_000})
		if err != nil {
			t.Fatal(err)
		}
		probe, err := NewMPEG2VideoEncoder(&bytes.Buffer{}, profile)
		if err != nil {
			t.Fatal(err)
		}
		frames, earlyCut, window, lateCut := sceneChangeMPEG2TestFrames(profile, ExportMPEG2MinGOPPictures(probe))
		coded := encodeMPEG2TestSequence(t, profile, frames)
		requireMPEG2DecoderAgreement(t, profile, coded, decodeMPEG2TestStream(t, ffmpeg, coded.stream))

		for index, pictureType := range coded.types {
			want := uint32(2)
			if index == 0 || index == lateCut || index == lateCut+ExportMPEG2GOPPictures {
				want = 1
			}
			if pictureType != want {
				t.Errorf("standard %v picture %d has type %d, want %d (types %v)", standard, index, pictureType, want, coded.types)
			}
		}
		intraShare := func(index int) float64 {
			intra := 0
			for _, macroblock := range coded.macroblocks[index] {
				if ExportMPEG2MacroblockIsIntra(macroblock) {
					intra++
				}
			}
			return float64(intra) / float64(len(coded.macroblocks[index]))
		}
		// Macroblocks on an outline may still find a usable prediction in
		// an unrelated picture; the flat ones cannot.
		if share := intraShare(earlyCut); share < 0.6 {
			t.Errorf("standard %v: %.0f%% of the early scene change is intra coded, want most of it", standard, 100*share)
		}
		for index := window; index < lateCut; index++ {
			if share := intraShare(index); share < 0.02 || share > 0.2 {
				t.Errorf("standard %v picture %d: %.0f%% intra macroblocks, want only the replaced window", standard, index, 100*share)
			}
		}
		if share := intraShare(lateCut + 3); share > 0.05 {
			t.Errorf("standard %v: %.0f%% intra macroblocks in a nearly static picture", standard, 100*share)
		}
	}
}
