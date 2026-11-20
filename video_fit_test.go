package video2dvd_test

import (
	"bytes"
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

func TestFitVideoFrameKeepsBilinearResultWithoutReduction(t *testing.T) {
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

	// Doubling a vertical ramp with bilinear interpolation gives y/2 exactly.
	doubled, err := FitVideoFrame(fitTestFrame(360, 240, func(_, y int) byte { return byte(y) }), profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	for y := range doubled.Height {
		for x := range doubled.Width {
			if got := doubled.Y[y*doubled.Width+x]; got != byte(y/2) {
				t.Fatalf("upscaled ramp at (%d,%d) = %d, want %d", x, y, got, y/2)
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
