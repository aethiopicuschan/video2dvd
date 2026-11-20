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

func TestMPEG2HalfSampleMotionRoundsHalfUp(t *testing.T) {
	// H.262 7.6.4: half-sample predictions are (a+b+1)>>1 and (a+b+c+d+2)>>2.
	plane := []byte{
		1, 2, 9,
		2, 2, 9,
		9, 9, 9,
	}
	for _, tc := range []struct {
		name             string
		motionX, motionY int
		want             byte
	}{
		{name: "integer", motionX: 0, motionY: 0, want: 1},
		{name: "horizontal", motionX: 1, motionY: 0, want: 2},
		{name: "vertical", motionX: 0, motionY: 1, want: 2},
		{name: "diagonal", motionX: 1, motionY: 1, want: 2},
	} {
		if got := ExportMPEG2MotionSample(plane, 3, 0, 0, tc.motionX, tc.motionY); got != tc.want {
			t.Errorf("%s half-sample prediction = %d, want %d", tc.name, got, tc.want)
		}
	}
	// (1+2+2+2+2)>>2 is 2 but (2+9+2+9+2)>>2 must round 5.5 up to 6.
	if got := ExportMPEG2MotionSample(plane, 3, 1, 0, 1, 1); got != 6 {
		t.Errorf("diagonal half-sample prediction = %d, want 6", got)
	}
}

// movingMPEG2TestFrame draws a textured picture that slides by half a luma
// sample per frame, so the motion search has to pick half-sample vectors.
func movingMPEG2TestFrame(profile Profile, index int) DecodedVideoFrame {
	width, height := profile.FrameWidth, profile.FrameHeight
	frame := DecodedVideoFrame{
		Width:  width,
		Height: height,
		Y:      make([]byte, width*height),
		U:      make([]byte, width*height/4),
		V:      make([]byte, width*height/4),
	}
	shiftX, shiftY := 0.5*float64(index), 0.5*float64(index/2)
	for y := range height {
		for x := range width {
			fx, fy := float64(x)+shiftX, float64(y)+shiftY
			value := 126 + 60*math.Sin(fx/9)*math.Cos(fy/7) + 30*math.Sin((fx+fy)/3.1) + 12*math.Sin(fx/1.7)
			frame.Y[y*width+x] = byte(max(16, min(235, value)))
		}
	}
	for y := range height / 2 {
		for x := range width / 2 {
			fx, fy := float64(x)+shiftX/2, float64(y)+shiftY/2
			frame.U[y*width/2+x] = byte(128 + 50*math.Sin(fx/11)*math.Sin(fy/5))
			frame.V[y*width/2+x] = byte(128 + 50*math.Cos(fx/6)*math.Sin(fy/13))
		}
	}
	return frame
}

func TestMPEG2DecoderDoesNotDriftFromEncoderReference(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 drift regression sample")
	}
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, VideoBitrate: 4_000_000})
	if err != nil {
		t.Fatal(err)
	}
	const frameCount = 15 // one full GOP: an I picture and 14 chained P pictures.
	var stream bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&stream, profile)
	if err != nil {
		t.Fatal(err)
	}
	references := make([]DecodedVideoFrame, 0, frameCount)
	for index := range frameCount {
		if err := encoder.WriteFrame(movingMPEG2TestFrame(profile, index)); err != nil {
			t.Fatal(err)
		}
		references = append(references, ExportMPEG2EncoderReference(encoder))
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	streamPath := filepath.Join(t.TempDir(), "drift.m2v")
	if err := os.WriteFile(streamPath, stream.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	decodedPath := filepath.Join(t.TempDir(), "decoded.yuv")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// -xerror makes any bitstream error fail the decode instead of being concealed.
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-xerror", "-i", streamPath, "-f", "rawvideo", "-pix_fmt", "yuv420p", decodedPath, "-y")
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("decode MPEG-2 drift sample: %v: %s", err, output)
	}
	decoded, err := os.ReadFile(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	lumaSize := profile.FrameWidth * profile.FrameHeight
	frameSize := lumaSize * 3 / 2
	if len(decoded) != frameCount*frameSize {
		t.Fatalf("decoded %d bytes, want %d frames of %d bytes", len(decoded), frameCount, frameSize)
	}
	meanSquaredError := func(first, second []byte) float64 {
		var sum uint64
		for i := range first {
			delta := int(first[i]) - int(second[i])
			sum += uint64(delta * delta)
		}
		return float64(sum) / float64(len(first))
	}
	halfSampleVectors := false
	for index, reference := range references {
		picture := decoded[index*frameSize : (index+1)*frameSize]
		// The encoder's floating-point IDCT and the decoder's integer IDCT may
		// differ by one LSB on isolated samples (MSE about 0.04 to 0.1 by the
		// end of the GOP, depending on the decoder's IDCT); truncated
		// half-sample prediction instead accumulates to an MSE above 40.
		for _, plane := range []struct {
			name             string
			decoded, encoder []byte
		}{
			{"Y", picture[:lumaSize], reference.Y},
			{"U", picture[lumaSize : lumaSize*5/4], reference.U},
			{"V", picture[lumaSize*5/4:], reference.V},
		} {
			if mse := meanSquaredError(plane.decoded, plane.encoder); mse > 0.25 {
				t.Errorf("picture %d plane %s: decoder differs from encoder reference, MSE %.4f (want <= 0.25)", index, plane.name, mse)
			}
		}
		if index > 0 {
			current := movingMPEG2TestFrame(profile, index)
			for _, macroblock := range ExportAnalyzeMPEG2PredictiveMacroblocks(current, references[index-1], profile.FrameWidth/16, profile.FrameHeight/16) {
				motionX, motionY := ExportMPEG2MacroblockMotion(macroblock)
				if motionX%2 != 0 || motionY%2 != 0 {
					halfSampleVectors = true
				}
			}
		}
	}
	if !halfSampleVectors {
		t.Fatal("sample did not exercise half-sample motion vectors")
	}
}

func TestMPEG2SequenceIsInterlacedWithProgressiveFrames(t *testing.T) {
	// DVD-Video requires 525/60 and 625/50 streams to be interlaced sequences;
	// progressive material is flagged per picture instead.
	profile, err := ExportProfile(Options{Standard: VideoStandardPAL, Aspect: AspectRatio4x3})
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&stream, profile)
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width: profile.FrameWidth, Height: profile.FrameHeight,
		Y: bytes.Repeat([]byte{90}, profile.FrameWidth*profile.FrameHeight),
		U: bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V: bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	for range 2 {
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	data := stream.Bytes()
	sequenceExtensions, pictureExtensions := 0, 0
	for position := 0; position+9 <= len(data); position++ {
		if !bytes.Equal(data[position:position+4], []byte{0, 0, 1, 0xb5}) {
			continue
		}
		reader := ExportNewBitReader(data[position+4:])
		read := func(count int) uint32 {
			value, err := ExportReadBits(reader, count)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		switch read(4) {
		case 1:
			sequenceExtensions++
			read(8) // profile_and_level_indication
			if progressive := read(1); progressive != 0 {
				t.Errorf("progressive_sequence = %d, want 0", progressive)
			}
			if chroma := read(2); chroma != 1 {
				t.Errorf("chroma_format = %d, want 1 (4:2:0)", chroma)
			}
			read(2 + 2 + 12 + 1 + 8)
			if lowDelay := read(1); lowDelay != 0 {
				t.Errorf("low_delay = %d, want 0", lowDelay)
			}
		case 8:
			pictureExtensions++
			read(16) // f_code
			read(2)  // intra_dc_precision
			for _, field := range []struct {
				name string
				bits int
				want uint32
			}{
				{"picture_structure", 2, 3},
				{"top_field_first", 1, 1},
				{"frame_pred_frame_dct", 1, 1},
				{"concealment_motion_vectors", 1, 0},
				{"q_scale_type", 1, 0},
				{"intra_vlc_format", 1, 0},
				{"alternate_scan", 1, 0},
				{"repeat_first_field", 1, 0},
				{"chroma_420_type", 1, 1},
				{"progressive_frame", 1, 1},
				{"composite_display_flag", 1, 0},
			} {
				if got := read(field.bits); got != field.want {
					t.Errorf("picture %d %s = %d, want %d", pictureExtensions, field.name, got, field.want)
				}
			}
		}
	}
	if sequenceExtensions != 1 || pictureExtensions != 2 {
		t.Fatalf("found %d sequence and %d picture coding extensions, want 1 and 2", sequenceExtensions, pictureExtensions)
	}
}

func TestMPEG2RateControlKeepsDecoderBufferFromUnderflowing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		standard  VideoStandard
		requested int
		effective int
		frameRate float64
	}{
		{name: "ntsc-minimum", standard: VideoStandardNTSC, requested: 2_000_000, effective: 2_000_000, frameRate: 30000.0 / 1001},
		{name: "pal-maximum", standard: VideoStandardPAL, requested: 8_500_000, effective: ExportMPEG2MuxSafeVideoBitrate, frameRate: 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: AspectRatio16x9, VideoBitrate: tc.requested})
			if err != nil {
				t.Fatal(err)
			}
			var stream bytes.Buffer
			encoder, err := NewMPEG2VideoEncoder(&stream, profile)
			if err != nil {
				t.Fatal(err)
			}
			// Every frame is fresh noise over a moving ramp: the costliest
			// content for both picture types, and a scene change every frame.
			const frameCount = 34
			sizes := make([]int, 0, frameCount)
			seed := uint32(1)
			for index := range frameCount {
				frame := movingMPEG2TestFrame(profile, index*7)
				for _, plane := range [][]byte{frame.Y, frame.U, frame.V} {
					for i := range plane {
						seed = seed*1664525 + 1013904223
						plane[i] = byte(max(16, min(235, int(plane[i])+int(seed>>24)%96-48)))
					}
				}
				before := stream.Len()
				if err := encoder.WriteFrame(frame); err != nil {
					t.Fatal(err)
				}
				sizes = append(sizes, stream.Len()-before)
			}

			// H.262 Annex C with the stream delivered at the declared bit rate
			// and decoding started at the occupancy the encoder assumes.
			bitsPerPicture := float64(tc.effective) / tc.frameRate
			fullness := float64(ExportMPEG2VBVStartBits)
			total, intraTotal := 0, 0
			for index, size := range sizes {
				bits := float64(size * 8)
				if bits > fullness {
					t.Fatalf("picture %d (%d bytes) underflows the decoder buffer holding %.0f bits", index, size, fullness)
				}
				fullness = min(fullness-bits+bitsPerPicture, ExportMPEG2VBVBufferBits)
				total += size
				if index%15 == 0 {
					intraTotal += size
				}
			}
			if used := float64(total*8) / float64(frameCount) * tc.frameRate; used < 0.85*float64(tc.effective) {
				t.Fatalf("noise was coded at %.0f bit/s, want at least 85%% of %d", used, tc.effective)
			}
			intraMean := float64(intraTotal) / 3
			predictiveMean := float64(total-intraTotal) / float64(frameCount-3)
			if intraMean < 1.2*predictiveMean {
				t.Fatalf("I pictures average %.0f bytes against %.0f for P pictures; want a larger share", intraMean, predictiveMean)
			}
		})
	}
}

func TestMPEG2EncoderRepeatsReferenceRatherThanUnderflowDecoderBuffer(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, VideoBitrate: 2_000_000})
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&stream, profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(movingMPEG2TestFrame(profile, 0)); err != nil {
		t.Fatal(err)
	}
	intra := ExportMPEG2EncoderReference(encoder)

	// With almost nothing left in the decoder buffer even the motion vectors
	// of a changed picture would arrive too late to be decoded.
	const fullnessBits = 12_000
	ExportSetMPEG2VBVFullness(encoder, fullnessBits)
	before := stream.Len()
	if err := encoder.WriteFrame(movingMPEG2TestFrame(profile, 9)); err != nil {
		t.Fatal(err)
	}
	if bits := (stream.Len() - before) * 8; bits > fullnessBits {
		t.Fatalf("P picture has %d bits with only %d in the decoder buffer", bits, fullnessBits)
	}
	repeated := ExportMPEG2EncoderReference(encoder)
	if !bytes.Equal(repeated.Y, intra.Y) || !bytes.Equal(repeated.U, intra.U) || !bytes.Equal(repeated.V, intra.V) {
		t.Fatal("encoder reference changed although the picture was coded as a repeat")
	}
	types, _ := mpeg2PictureTypesAndSizes(stream.Bytes())
	if len(types) != 2 || types[0] != 1 || types[1] != 2 {
		t.Fatalf("picture types = %v, want I, P", types)
	}
}

func TestMPEG2IntraPictureFitsDrainedDecoderBuffer(t *testing.T) {
	// An I picture cannot be replaced by a repeat, so its smallest coding has
	// to fit whatever the P pictures before it may leave in the buffer: the
	// guard plus one picture period of delivery.
	for _, tc := range []struct {
		standard  VideoStandard
		frameRate float64
	}{
		{VideoStandardNTSC, 30000.0 / 1001},
		{VideoStandardPAL, 25},
	} {
		profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: AspectRatio16x9, VideoBitrate: 2_000_000})
		if err != nil {
			t.Fatal(err)
		}
		var stream bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&stream, profile)
		if err != nil {
			t.Fatal(err)
		}
		floorBits := float64(ExportMPEG2VBVGuardBits) + 2_000_000/tc.frameRate
		seed := uint32(7)
		for index := range 16 {
			frame := movingMPEG2TestFrame(profile, index)
			for _, plane := range [][]byte{frame.Y, frame.U, frame.V} {
				for i := range plane {
					seed = seed*1664525 + 1013904223
					plane[i] = byte(seed >> 24)
				}
			}
			if index > 0 {
				ExportSetMPEG2VBVFullness(encoder, floorBits)
			}
			before := stream.Len()
			if err := encoder.WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			if bits := float64((stream.Len() - before) * 8); index > 0 && bits > floorBits {
				t.Fatalf("standard %v picture %d has %.0f bits with %.0f in the decoder buffer", tc.standard, index, bits, floorBits)
			}
		}
		if types, _ := mpeg2PictureTypesAndSizes(stream.Bytes()); len(types) != 16 || types[15] != 1 {
			t.Fatalf("standard %v picture types = %v, want an I picture last", tc.standard, types)
		}
	}
}

func TestMPEG2SequenceHeaderDeclaresCodedBitrate(t *testing.T) {
	for requested, want := range map[int]uint32{0: ExportMPEG2MuxSafeVideoBitrate, 6_000_000: 6_000_000, 7_500_000: 7_500_000, 8_000_000: ExportMPEG2MuxSafeVideoBitrate} {
		profile, err := ExportProfile(Options{Standard: VideoStandardPAL, Aspect: AspectRatio4x3, VideoBitrate: requested})
		if err != nil {
			t.Fatal(err)
		}
		var stream bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&stream, profile)
		if err != nil {
			t.Fatal(err)
		}
		if err := encoder.WriteFrame(movingMPEG2TestFrame(profile, 0)); err != nil {
			t.Fatal(err)
		}
		bitRate, err := ExportReadBits(ExportNewBitReader(stream.Bytes()[8:]), 18)
		if err != nil {
			t.Fatal(err)
		}
		if bitRate*400 != want {
			t.Errorf("requested %d bit/s: sequence header bit_rate = %d, want %d", requested, bitRate*400, want)
		}
	}
}
