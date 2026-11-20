package video2dvd_test

import (
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// The horizontal pass of a signed kernel leaves values below zero beside a
// bright line, and they must survive until the vertical pass has run. Here
// every second source row is white, so the vertical low-pass lifts the
// picture to mid grey and the negative lobes of the lines on the other rows
// show as dips below it; clamping between the passes would flatten them.
func TestFitVideoFrameKeepsOvershootBetweenPasses(t *testing.T) {
	profile := ntscWideProfile(t)
	frame := fitTestFrame(3840, 2160, func(x, y int) byte {
		if y%2 == 1 || x%240 < 6 {
			return 255
		}
		return 0
	})
	fitted, err := FitVideoFrame(frame, profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	row := fitted.Y[100*fitted.Width:][:fitted.Width]
	if flat := row[22]; flat < 127 || flat > 128 {
		t.Fatalf("between the lines the picture is %d, want mid grey", flat)
	}
	lowest := row[0]
	for _, value := range row {
		lowest = min(lowest, value)
	}
	if lowest > 122 {
		t.Fatalf("darkest sample beside a line is %d: the negative lobes were clipped before the vertical pass", lowest)
	}
}

// Frames pass through the frame-rate converter with their colour tag, so a
// pipeline that fits after converting still selects the right matrix.
func TestFrameRateConverterKeepsFrameColor(t *testing.T) {
	profile := ntscWideProfile(t)
	want := VideoColor{Matrix: ColorMatrixBT709, FullRange: true}
	var got []VideoColor
	converter, err := NewDVDFrameRateConverter(profile, func(frame DecodedVideoFrame) error {
		got = append(got, frame.Color)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		frame := flatFrame(2, 2, 16, 128, 128, want)
		frame.PTS, frame.Duration = time.Duration(index)*40*time.Millisecond, 40*time.Millisecond
		if err := converter.Push(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := converter.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no output frames")
	}
	for index, color := range got {
		if color != want {
			t.Fatalf("output frame %d colour %+v, want %+v", index, color, want)
		}
	}
}
