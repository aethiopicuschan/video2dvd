package video2dvd_test

import (
	"bytes"
	"math"
	"runtime"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

func fitTestFrame(width, height int, luma func(x, y int) byte) DecodedVideoFrame {
	frame := DecodedVideoFrame{
		Width:  width,
		Height: height,
		Y:      make([]byte, width*height),
		U:      bytes.Repeat([]byte{128}, width*height/4),
		V:      bytes.Repeat([]byte{128}, width*height/4),
	}
	for y := range height {
		for x := range width {
			frame.Y[y*width+x] = luma(x, y)
		}
	}
	return frame
}

func TestFitVideoFrameLowPassesWhenDownscaling(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	// One-pixel stripes are far above what 720x480 can represent. Sampling two
	// neighbours per output pixel turns them into a strong false pattern
	// (52..199); a filtered reduction leaves a nearly flat mid grey.
	for _, tc := range []struct {
		name string
		luma func(x, y int) byte
	}{
		{name: "vertical stripes", luma: func(x, _ int) byte { return byte(16 + 219*(x%2)) }},
		{name: "horizontal stripes", luma: func(_, y int) byte { return byte(16 + 219*(y%2)) }},
	} {
		fitted, err := FitVideoFrame(fitTestFrame(1920, 1080, tc.luma), profile, 16, 9, FitLetterbox)
		if err != nil {
			t.Fatal(err)
		}
		lowest, highest := byte(255), byte(0)
		for _, value := range fitted.Y {
			lowest, highest = min(lowest, value), max(highest, value)
		}
		if lowest < 93 || highest > 158 {
			t.Errorf("%s: downscaled luma spans %d..%d, want within 93..158 around the 125.5 mean", tc.name, lowest, highest)
		}
	}

	flat, err := FitVideoFrame(fitTestFrame(1280, 720, func(int, int) byte { return 200 }), profile, 4, 3, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	for y := range flat.Height {
		for x := range flat.Width {
			want := byte(200)
			if x < 90 || x >= 630 {
				want = 16 // pillarbox bars of a 4:3 picture in a 16:9 raster.
			}
			if got := flat.Y[y*flat.Width+x]; got != want {
				t.Fatalf("flat picture luma at (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

func TestFitVideoFrameCopiesEqualSizeAndInterpolatesEnlargement(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	texture := func(x, y int) byte { return byte((x*7 + y*13) % 251) }
	source := fitTestFrame(720, 480, texture)
	same, err := FitVideoFrame(source, profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(same.Y, source.Y) || !bytes.Equal(same.U, source.U) || !bytes.Equal(same.V, source.V) {
		t.Fatal("a frame already in the DVD raster was not copied unchanged")
	}

	// Enlargement interpolates: a doubled vertical ramp stays the ramp y/2
	// (sample centres put the exact value at y/2 - 0.25).
	doubled, err := FitVideoFrame(fitTestFrame(360, 240, func(_, y int) byte { return byte(y) }), profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	for y := range doubled.Height {
		for x := range doubled.Width {
			want := min(max(float64(y)/2-0.25, 0), 239)
			if got := float64(doubled.Y[y*doubled.Width+x]); math.Abs(got-want) > 0.76 {
				t.Fatalf("upscaled ramp at (%d,%d) = %.0f, want about %.2f", x, y, got, want)
			}
		}
	}
}

func TestFitVideoFrameDownscaleKeepsSamplePositions(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	// A horizontal ramp must land where centre-aligned sampling puts it; a
	// filter with a phase error would shift every column.
	ramp, err := FitVideoFrame(fitTestFrame(1920, 1080, func(x, _ int) byte { return byte(x * 255 / 1919) }), profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, y := range []int{0, 240, 479} {
		for x := range ramp.Width {
			want := ((float64(x)+0.5)*1920/720 - 0.5) * 255 / 1919
			if got := float64(ramp.Y[y*ramp.Width+x]); got < want-1.5 || got > want+1.5 {
				t.Fatalf("downscaled ramp at (%d,%d) = %.0f, want about %.1f", x, y, got, want)
			}
		}
	}

	// 640x480 4:3 shrinks only horizontally (to 540 columns); the axis that
	// keeps its size must pass through untouched.
	rows, err := FitVideoFrame(fitTestFrame(640, 480, func(_, y int) byte { return byte(y * 37 % 251) }), profile, 4, 3, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	for y := range rows.Height {
		for x := 90; x < 630; x++ {
			if got, want := rows.Y[y*rows.Width+x], byte(y*37%251); got != want {
				t.Fatalf("row pattern at (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

// The reduction filter is a windowed sinc, not a triangle: detail the DVD
// raster can carry keeps its contrast. At a quarter cycle per output sample a
// triangle filter passes 81% and Lanczos-3 all of it.
func TestFitVideoFrameKeepsRepresentableDetail(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	const amplitude = 80.0
	wave := func(x, _ int) byte {
		// Four output columns per cycle on the 3840 -> 720 grid.
		outputColumn := (float64(x)+0.5)*720/3840 - 0.5
		return byte(math.Round(126 + amplitude*math.Sin(2*math.Pi*outputColumn/4)))
	}
	fitted, err := FitVideoFrame(fitTestFrame(3840, 2160, wave), profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	lowest, highest := byte(255), byte(0)
	for x := 40; x < 680; x++ {
		value := fitted.Y[240*fitted.Width+x]
		lowest, highest = min(lowest, value), max(highest, value)
	}
	if swing := float64(highest-lowest) / 2; swing < 0.95*amplitude || swing > 1.05*amplitude {
		t.Fatalf("quarter-cycle detail keeps a swing of %.1f, want within 5%% of %.0f", swing, amplitude)
	}
}

// A hard edge overshoots by the few percent inherent to Lanczos-3 and no
// more, and the scaler leaves the overshoot in place instead of clipping it
// to studio range.
func TestFitVideoFrameEdgeOvershootIsBounded(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	edge := func(x, _ int) byte {
		if x < 1920 {
			return 40
		}
		return 220
	}
	fitted, err := FitVideoFrame(fitTestFrame(3840, 2160, edge), profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	lowest, highest := byte(255), byte(0)
	for x := range fitted.Width {
		value := fitted.Y[100*fitted.Width+x]
		lowest, highest = min(lowest, value), max(highest, value)
	}
	if lowest >= 40 || highest <= 220 {
		t.Fatalf("edge spans %d..%d: a windowed-sinc reduction must overshoot the 40..220 step", lowest, highest)
	}
	if lowest < 40-18 || highest > 220+18 {
		t.Fatalf("edge spans %d..%d, overshoot above 10%% of the 180-code step", lowest, highest)
	}
	if left, right := fitted.Y[100*fitted.Width+340], fitted.Y[100*fitted.Width+380]; left != 40 || right != 220 {
		t.Fatalf("flat areas beside the edge = %d and %d, want 40 and 220", left, right)
	}
}

// 4:2:0 chroma is co-sited with even luma columns, so chroma sample j of the
// output sits at source chroma position (j+0.25)*ratio-0.25; treating it as
// centred would move colour 0.4 luma samples to the right at 3840 -> 720.
func TestFitVideoFrameKeepsLeftSitedChromaInPlace(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	source := fitTestFrame(3840, 2160, func(int, int) byte { return 128 })
	for y := range 1080 {
		for x := range 1920 {
			source.U[y*1920+x] = byte(min(max((x-900)*2, 0), 255))
		}
	}
	fitted, err := FitVideoFrame(source, profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	const ratio = 1920.0 / 360
	for x := 172; x < 190; x++ {
		want := ((float64(x)+0.25)*ratio - 0.25 - 900) * 2
		centred := ((float64(x)+0.5)*ratio - 0.5 - 900) * 2
		got := float64(fitted.U[120*360+x])
		if math.Abs(got-want) > 0.75 {
			t.Fatalf("chroma ramp at column %d = %.0f, want %.2f for left-sited chroma (%.2f if centred)", x, got, want, centred)
		}
	}
}

func TestResampleKernelIsNormalisedSignedAndIdentityAtEqualSizes(t *testing.T) {
	for _, phase := range []float64{ExportResamplePhaseCentre, ExportResamplePhaseLeft} {
		for _, sizes := range [][2]int{{3840, 720}, {1920, 720}, {1080, 480}, {640, 720}, {360, 480}, {180, 240}, {2, 720}, {720, 2}} {
			taps, first, weights := ExportResampleKernelWeights(ExportNewResampleKernel(sizes[0], sizes[1], phase))
			negative := false
			for index := range sizes[1] {
				var sum int32
				for _, weight := range weights[index*taps : (index+1)*taps] {
					sum += weight
					negative = negative || weight < 0
				}
				if sum != 1<<14 {
					t.Fatalf("%d -> %d phase %.2f: weights of output %d sum to %d", sizes[0], sizes[1], phase, index, sum)
				}
				if first[index] < 0 || first[index]+taps > sizes[0] {
					t.Fatalf("%d -> %d: output %d reads %d..%d", sizes[0], sizes[1], index, first[index], first[index]+taps-1)
				}
			}
			if sizes[0] >= 180 && !negative {
				t.Errorf("%d -> %d phase %.2f: kernel has no negative lobes", sizes[0], sizes[1], phase)
			}
		}
		taps, first, weights := ExportResampleKernelWeights(ExportNewResampleKernel(360, 360, phase))
		for index := range 360 {
			for tap, weight := range weights[index*taps : (index+1)*taps] {
				want := int32(0)
				if first[index]+tap == index {
					want = 1 << 14
				}
				if weight != want {
					t.Fatalf("equal-size kernel, phase %.2f: output %d weight for source %d = %d", phase, index, first[index]+tap, weight)
				}
			}
		}
	}
	centred := ExportCachedResampleKernel(1920, 720, ExportResamplePhaseCentre)
	if again := ExportCachedResampleKernel(1920, 720, ExportResamplePhaseCentre); again != centred {
		t.Fatal("kernel for the same sizes was rebuilt")
	}
	if leftSited := ExportCachedResampleKernel(1920, 720, ExportResamplePhaseLeft); leftSited == centred {
		t.Fatal("centre- and left-sited kernels share a cache entry")
	}
}

// Rows are resampled in parallel; the picture must not depend on how many
// goroutines share them.
func TestFitVideoFrameIsIdenticalForAnyGOMAXPROCS(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardPAL, Aspect: AspectRatio4x3})
	if err != nil {
		t.Fatal(err)
	}
	source := fitTestFrame(1920, 1080, func(x, y int) byte { return byte((x*x/7 + y*y/3 + x*y/11) % 256) })
	for i := range source.U {
		source.U[i], source.V[i] = byte(i*7%256), byte(i*13%256)
	}
	source.Color = VideoColor{Matrix: ColorMatrixBT709, FullRange: true}
	fit := func() DecodedVideoFrame {
		fitted, err := FitVideoFrameForDVD(source, profile, 16, 9, FitLetterbox)
		if err != nil {
			t.Fatal(err)
		}
		return fitted
	}
	previous := runtime.GOMAXPROCS(1)
	single := fit()
	runtime.GOMAXPROCS(7)
	parallel := fit()
	runtime.GOMAXPROCS(previous)
	if !bytes.Equal(single.Y, parallel.Y) || !bytes.Equal(single.U, parallel.U) || !bytes.Equal(single.V, parallel.V) {
		t.Fatal("fitted frame differs between GOMAXPROCS 1 and 7")
	}
}

func BenchmarkFitVideoFrameForDVD(b *testing.B) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range [][2]int{{3840, 2160}, {1920, 1080}, {640, 360}} {
		source := fitTestFrame(size[0], size[1], func(x, y int) byte { return byte((x*7 + y*13) % 251) })
		source.Color = VideoColor{Matrix: ColorMatrixBT709}
		b.Run(map[int]string{3840: "2160p", 1920: "1080p", 640: "360p"}[size[0]], func(b *testing.B) {
			for b.Loop() {
				if _, err := FitVideoFrameForDVD(source, profile, 16, 9, FitLetterbox); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
