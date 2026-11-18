package video2dvd_test

import (
	"bytes"
	. "github.com/aethiopicuschan/video2dvd"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestMPEG2VideoEncoderWritesDVDSequence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		standard VideoStandard
		aspect   AspectRatio
		width    int
		height   int
		fpsN     int
		fpsD     int
	}{
		{name: "ntsc", standard: VideoStandardNTSC, aspect: AspectRatio16x9, width: 720, height: 480, fpsN: 30000, fpsD: 1001},
		{name: "pal", standard: VideoStandardPAL, aspect: AspectRatio4x3, width: 720, height: 576, fpsN: 25, fpsD: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: tc.aspect})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			encoder, err := NewMPEG2VideoEncoder(&output, profile)
			if err != nil {
				t.Fatal(err)
			}
			frame := DecodedVideoFrame{
				Width:    tc.width,
				Height:   tc.height,
				Duration: time.Second * time.Duration(tc.fpsD) / time.Duration(tc.fpsN),
				Y:        bytes.Repeat([]byte{16}, tc.width*tc.height),
				U:        bytes.Repeat([]byte{128}, tc.width*tc.height/4),
				V:        bytes.Repeat([]byte{128}, tc.width*tc.height/4),
			}
			if err := encoder.WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			if err := encoder.Close(); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(t.TempDir(), "video.m2v")
			if err := os.WriteFile(path, output.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := ProbeMPEG2Video(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Width != tc.width || info.Height != tc.height || info.Aspect != tc.aspect {
				t.Fatalf("unexpected sequence metadata: %+v", info)
			}
			if info.FrameRateNumerator != tc.fpsN || info.FrameRateDenominator != tc.fpsD {
				t.Fatalf("unexpected frame rate: %d/%d", info.FrameRateNumerator, info.FrameRateDenominator)
			}
		})
	}
}

func TestMPEG2VideoEncoderRejectsEmptyStream(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&output, profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err == nil {
		t.Fatal("expected an error for an empty MPEG-2 stream")
	}
}

func TestMPEG2VideoEncoderHonorsConfiguredBitrate(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, VideoBitrate: 4_000_000})
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(&bytes.Buffer{}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if want := 4_000_000 * 1001 / (8 * 30000); ExportMPEG2FrameBytes(encoder) != want {
		t.Fatalf("frame budget = %d, want %d", ExportMPEG2FrameBytes(encoder), want)
	}
	profile.VideoBitrate = 9_000_000
	if _, err := NewMPEG2VideoEncoder(&bytes.Buffer{}, profile); err == nil {
		t.Fatal("expected rejection of bitrate above DVD-safe limit")
	}
}

func TestMPEG2InverseQuantizationMatchesH262Scale(t *testing.T) {
	if got, want := ExportMPEG2InverseIntraAC(4, 16, 16), 64; got != want {
		t.Errorf("intra AC inverse quantization = %d, want %d", got, want)
	}
	if got, want := ExportMPEG2InverseNonIntra(4, 16), 72; got != want {
		t.Errorf("positive non-intra inverse quantization = %d, want %d", got, want)
	}
	if got, want := ExportMPEG2InverseNonIntra(-4, 16), -72; got != want {
		t.Errorf("negative non-intra inverse quantization = %d, want %d", got, want)
	}
}

func TestMPEG2NonIntraQuantizationMatchesInverseScale(t *testing.T) {
	for _, tc := range []struct {
		coefficient float64
		quantizer   int
		wantLevel   int
		wantDecoded int
	}{
		{coefficient: 20, quantizer: 5, wantLevel: 4, wantDecoded: 22},
		{coefficient: -17, quantizer: 5, wantLevel: -3, wantDecoded: -17},
	} {
		level := ExportMPEG2QuantizeNonIntra(tc.coefficient, tc.quantizer)
		if level != tc.wantLevel {
			t.Errorf("quantized coefficient %.1f = %d, want %d", tc.coefficient, level, tc.wantLevel)
		}
		if got := ExportMPEG2InverseNonIntra(level, tc.quantizer); got != tc.wantDecoded {
			t.Errorf("reconstructed coefficient %.1f = %d, want %d", tc.coefficient, got, tc.wantDecoded)
		}
	}
}

func TestMPEG2VideoEncoderCodesACAndStaysWithinFrameBudget(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{Width: 720, Height: 480, Duration: time.Second * 1001 / 30000}
	frame.Y = make([]byte, frame.Width*frame.Height)
	frame.U = make([]byte, frame.Width*frame.Height/4)
	frame.V = make([]byte, frame.Width*frame.Height/4)
	for y := 0; y < frame.Height; y++ {
		for x := 0; x < frame.Width; x++ {
			frame.Y[y*frame.Width+x] = byte(16 + (x*13+y*7)%220)
		}
	}
	for i := range frame.U {
		frame.U[i] = byte(16 + (i*3)%224)
		frame.V[i] = byte(16 + (i*5)%224)
	}

	blocks := ExportCollectMPEG2IntraBlocks(frame, frame.Width/16, frame.Height/16)
	encoder, err := NewMPEG2VideoEncoder(&bytes.Buffer{}, profile)
	if err != nil {
		t.Fatal(err)
	}
	withAC := ExportMPEG2IntraPicture(encoder, frame, blocks, frame.Width/16, frame.Height/16, 31, true)
	withoutAC := ExportMPEG2IntraPicture(encoder, frame, blocks, frame.Width/16, frame.Height/16, 31, false)
	if got := ExportMPEG2IntraPictureSize(encoder, frame, blocks, frame.Width/16, frame.Height/16, 31, true); got != len(withAC) {
		t.Fatalf("counted AC picture size = %d, encoded size = %d", got, len(withAC))
	}
	if got := ExportMPEG2IntraPictureSize(encoder, frame, blocks, frame.Width/16, frame.Height/16, 31, false); got != len(withoutAC) {
		t.Fatalf("counted DC-only picture size = %d, encoded size = %d", got, len(withoutAC))
	}
	if len(withAC) <= len(withoutAC) {
		t.Fatalf("AC-coded picture size %d did not exceed DC-only size %d", len(withAC), len(withoutAC))
	}

	var output bytes.Buffer
	encoder, err = NewMPEG2VideoEncoder(&output, profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if output.Len() > ExportMPEG2FrameBytes(encoder)+4 {
		t.Fatalf("output size %d exceeds one-frame budget %d plus sequence end", output.Len(), ExportMPEG2FrameBytes(encoder))
	}
}

func TestMPEG2VideoEncoderUsesPredictivePicturesForUnchangedMacroblocks(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Duration: time.Second * 1001 / 30000,
		Y:        bytes.Repeat([]byte{96}, profile.FrameWidth*profile.FrameHeight),
		U:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	var output bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&output, profile)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if i == 2 {
			frame = ExportCloneDecodedVideoFrame(frame)
			for y := 160; y < 176; y++ {
				for x := 320; x < 336; x++ {
					frame.Y[y*frame.Width+x]++
				}
			}
		}
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}

	types, sizes := mpeg2PictureTypesAndSizes(output.Bytes())
	if len(types) != 3 {
		t.Fatalf("found %d pictures, want 3", len(types))
	}
	if types[0] != 1 || types[1] != 2 || types[2] != 2 {
		t.Fatalf("picture types = %v, want I, P, P", types)
	}
	if sizes[1] >= sizes[0] {
		t.Fatalf("unchanged P picture size %d should be smaller than I picture size %d", sizes[1], sizes[0])
	}
	if sizes[2] <= sizes[1] {
		t.Fatalf("P picture with a changed macroblock size %d should exceed unchanged P picture size %d", sizes[2], sizes[1])
	}
}

func TestMPEG2PredictiveProcessingIsDeterministicAcrossWorkerCounts(t *testing.T) {
	frame := DecodedVideoFrame{Width: 64, Height: 64}
	frame.Y = make([]byte, frame.Width*frame.Height)
	frame.U = make([]byte, frame.Width*frame.Height/4)
	frame.V = make([]byte, frame.Width*frame.Height/4)
	reference := ExportCloneDecodedVideoFrame(frame)
	for y := range frame.Height {
		for x := range frame.Width {
			frame.Y[y*frame.Width+x] = byte((x*11 + y*7) % 220)
			reference.Y[y*frame.Width+x] = byte((x*9 + y*5) % 220)
		}
	}
	for i := range frame.U {
		frame.U[i], frame.V[i] = byte((i*3)%224), byte((i*5)%224)
		reference.U[i], reference.V[i] = byte((i*7)%224), byte((i*11)%224)
	}

	previous := runtime.GOMAXPROCS(1)
	serial := ExportAnalyzeMPEG2PredictiveMacroblocks(frame, reference, 4, 4)
	serialReconstruction := ExportReconstructMPEG2PredictiveFrame(frame, reference, serial, 4, 4, 8, true)
	runtime.GOMAXPROCS(max(2, previous))
	defer runtime.GOMAXPROCS(previous)
	parallel := ExportAnalyzeMPEG2PredictiveMacroblocks(frame, reference, 4, 4)
	parallelReconstruction := ExportReconstructMPEG2PredictiveFrame(frame, reference, parallel, 4, 4, 8, true)

	if !reflect.DeepEqual(parallel, serial) {
		t.Fatal("parallel macroblock analysis differs from serial analysis")
	}
	if !reflect.DeepEqual(parallelReconstruction, serialReconstruction) {
		t.Fatal("parallel reconstruction differs from serial reconstruction")
	}
}

func TestMPEG2ParallelPictureRowsMatchCountedSizesAndSerialOutput(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(&bytes.Buffer{}, profile)
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{Width: 32, Height: 32}
	frame.Y = make([]byte, frame.Width*frame.Height)
	frame.U = make([]byte, frame.Width*frame.Height/4)
	frame.V = make([]byte, frame.Width*frame.Height/4)
	reference := ExportCloneDecodedVideoFrame(frame)
	for y := range frame.Height {
		for x := range frame.Width {
			frame.Y[y*frame.Width+x] = byte((x*13 + y*7) % 220)
			reference.Y[y*frame.Width+x] = byte((x*9 + y*5) % 220)
		}
	}
	for i := range frame.U {
		frame.U[i], frame.V[i] = byte((i*3)%224), byte((i*5)%224)
		reference.U[i], reference.V[i] = byte((i*7)%224), byte((i*11)%224)
	}
	const mbWidth, mbHeight = 2, 2
	intraBlocks := ExportCollectMPEG2IntraBlocks(frame, mbWidth, mbHeight)
	predictive := ExportAnalyzeMPEG2PredictiveMacroblocks(frame, reference, mbWidth, mbHeight)

	previous := runtime.GOMAXPROCS(1)
	serialIntra := ExportMPEG2IntraPicture(encoder, frame, intraBlocks, mbWidth, mbHeight, 8, true)
	serialPredictive := ExportMPEG2PredictivePicture(encoder, predictive, mbWidth, mbHeight, 8, true)
	runtime.GOMAXPROCS(max(2, previous))
	defer runtime.GOMAXPROCS(previous)
	parallelIntra := ExportMPEG2IntraPicture(encoder, frame, intraBlocks, mbWidth, mbHeight, 8, true)
	parallelPredictive := ExportMPEG2PredictivePicture(encoder, predictive, mbWidth, mbHeight, 8, true)

	if !bytes.Equal(parallelIntra, serialIntra) {
		t.Fatal("parallel intra slice output differs from serial output")
	}
	if !bytes.Equal(parallelPredictive, serialPredictive) {
		t.Fatal("parallel predictive slice output differs from serial output")
	}
	if got := ExportMPEG2IntraPictureSize(encoder, frame, intraBlocks, mbWidth, mbHeight, 8, true); got != len(serialIntra) {
		t.Fatalf("counted intra picture size = %d, encoded size = %d", got, len(serialIntra))
	}
	if got := ExportMPEG2PredictivePictureSize(encoder, predictive, mbWidth, mbHeight, 8, true); got != len(serialPredictive) {
		t.Fatalf("counted predictive picture size = %d, encoded size = %d", got, len(serialPredictive))
	}
}

func mpeg2PictureTypesAndSizes(stream []byte) ([]uint32, []int) {
	var types []uint32
	var sizes []int
	for position := 0; position+4 <= len(stream); position++ {
		if !bytes.Equal(stream[position:position+4], []byte{0, 0, 1, 0}) {
			continue
		}
		if position+6 > len(stream) {
			break
		}
		reader := ExportNewBitReader(stream[position+4:])
		_, _ = ExportReadBits(reader, 10)
		pictureType, err := ExportReadBits(reader, 3)
		if err != nil {
			break
		}
		end := len(stream)
		for next := position + 4; next+4 <= len(stream); next++ {
			if bytes.Equal(stream[next:next+4], []byte{0, 0, 1, 0}) || bytes.Equal(stream[next:next+4], []byte{0, 0, 1, 0xb7}) {
				end = next
				break
			}
		}
		types = append(types, pictureType)
		sizes = append(sizes, end-position)
		position = end - 1
	}
	return types, sizes
}
