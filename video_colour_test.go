package video2dvd_test

import (
	"bytes"
	"math"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

// studioYCbCr encodes normalised R'G'B' as 8-bit studio-range Y'CbCr with the
// given luma coefficients; studioRGB decodes it to 8-bit R'G'B'.
func studioYCbCr(kr, kb, r, g, b float64) (y, cb, cr float64) {
	luma := kr*r + (1-kr-kb)*g + kb*b
	return 16 + 219*luma, 128 + 224*(b-luma)/(2*(1-kb)), 128 + 224*(r-luma)/(2*(1-kr))
}

func studioRGB(kr, kb, y, cb, cr float64) [3]float64 {
	luma, pb, pr := (y-16)/219, (cb-128)/224, (cr-128)/224
	r := luma + 2*(1-kr)*pr
	b := luma + 2*(1-kb)*pb
	g := (luma - kr*r - kb*b) / (1 - kr - kb)
	return [3]float64{255 * r, 255 * g, 255 * b}
}

func flatFrame(width, height int, y, cb, cr byte, color VideoColor) DecodedVideoFrame {
	return DecodedVideoFrame{
		Width: width, Height: height, Color: color,
		Y: bytes.Repeat([]byte{y}, width*height),
		U: bytes.Repeat([]byte{cb}, width*height/4),
		V: bytes.Repeat([]byte{cr}, width*height/4),
	}
}

func ntscWideProfile(t *testing.T) Profile {
	t.Helper()
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// A DVD player decodes with the BT.601 matrix, so BT.709 colour bars must be
// rewritten to show the colours the BT.709 source meant.
func TestFitVideoFrameForDVDConvertsBT709ColourBars(t *testing.T) {
	profile := ntscWideProfile(t)
	const kr709, kb709, kr601, kb601 = 0.2126, 0.0722, 0.299, 0.114
	for _, level := range []float64{1, 0.75, 0.4} {
		for bar := 1; bar < 7; bar++ {
			r, g, b := level*float64(bar>>2&1), level*float64(bar>>1&1), level*float64(bar&1)
			y, cb, cr := studioYCbCr(kr709, kb709, r, g, b)
			source := flatFrame(1280, 720, byte(math.Round(y)), byte(math.Round(cb)), byte(math.Round(cr)), VideoColor{Matrix: ColorMatrixBT709})
			want := studioRGB(kr709, kb709, float64(source.Y[0]), float64(source.U[0]), float64(source.V[0]))

			plain, err := FitVideoFrame(source, profile, 16, 9, FitLetterbox)
			if err != nil {
				t.Fatal(err)
			}
			if plain.Y[1000] != source.Y[0] || plain.U[1000] != source.U[0] || plain.V[1000] != source.V[0] || plain.Color != source.Color {
				t.Fatalf("FitVideoFrame changed colour samples or the tag: %d/%d/%d %+v", plain.Y[1000], plain.U[1000], plain.V[1000], plain.Color)
			}
			unconverted := studioRGB(kr601, kb601, float64(plain.Y[1000]), float64(plain.U[1000]), float64(plain.V[1000]))

			fitted, err := FitVideoFrameForDVD(source, profile, 16, 9, FitLetterbox)
			if err != nil {
				t.Fatal(err)
			}
			if fitted.Color != (VideoColor{Matrix: ColorMatrixBT601}) {
				t.Fatalf("converted frame is tagged %+v", fitted.Color)
			}
			got := studioRGB(kr601, kb601, float64(fitted.Y[1000]), float64(fitted.U[1000]), float64(fitted.V[1000]))
			worst, worstUnconverted := 0.0, 0.0
			for channel := range got {
				worst = max(worst, math.Abs(got[channel]-want[channel]))
				worstUnconverted = max(worstUnconverted, math.Abs(unconverted[channel]-want[channel]))
			}
			if worst > 2 {
				t.Errorf("bar %d at %.0f%%: BT.601 rendering %.1f differs from the BT.709 source colour %.1f by %.1f codes (unconverted: %.1f)", bar, level*100, got, want, worst, worstUnconverted)
			}
		}
	}
}

func TestFitVideoFrameForDVDKeepsGreysExactly(t *testing.T) {
	profile := ntscWideProfile(t)
	for _, luma := range []byte{16, 17, 64, 126, 180, 235} {
		fitted, err := FitVideoFrameForDVD(flatFrame(1920, 1080, luma, 128, 128, VideoColor{Matrix: ColorMatrixBT709}), profile, 16, 9, FitLetterbox)
		if err != nil {
			t.Fatal(err)
		}
		for i, value := range fitted.Y {
			if value != luma {
				t.Fatalf("grey %d became luma %d at sample %d", luma, value, i)
			}
		}
		for i := range fitted.U {
			if fitted.U[i] != 128 || fitted.V[i] != 128 {
				t.Fatalf("grey %d gained chroma %d/%d at sample %d", luma, fitted.U[i], fitted.V[i], i)
			}
		}
	}
}

// Untagged sources follow the size rule players use; BT.601 and encodings
// the library does not convert pass through.
func TestFitVideoFrameForDVDSelectsMatrixBySourceTagAndSize(t *testing.T) {
	profile := ntscWideProfile(t)
	for _, tc := range []struct {
		name          string
		width, height int
		color         VideoColor
		converted     bool
	}{
		{"untagged 1080p", 1920, 1080, VideoColor{}, true},
		{"untagged 720p", 1280, 720, VideoColor{}, true},
		{"untagged wide but short", 1280, 544, VideoColor{}, true},
		{"untagged 576 lines", 1024, 576, VideoColor{}, false},
		{"untagged 360p", 640, 360, VideoColor{}, false},
		{"BT.709 tagged 360p", 640, 360, VideoColor{Matrix: ColorMatrixBT709}, true},
		{"BT.601 tagged 1080p", 1920, 1080, VideoColor{Matrix: ColorMatrixBT601}, false},
		{"BT.2020 or RGB tagged 2160p", 3840, 2160, VideoColor{Matrix: ColorMatrixOther}, false},
		{"full-range RGB tagged", 1920, 1080, VideoColor{Matrix: ColorMatrixOther, FullRange: true}, false},
	} {
		source := flatFrame(tc.width, tc.height, 150, 70, 190, tc.color)
		plain, err := FitVideoFrame(source, profile, 16, 9, FitCrop)
		if err != nil {
			t.Fatal(err)
		}
		fitted, err := FitVideoFrameForDVD(source, profile, 16, 9, FitCrop)
		if err != nil {
			t.Fatal(err)
		}
		changed := !bytes.Equal(plain.Y, fitted.Y) || !bytes.Equal(plain.U, fitted.U) || !bytes.Equal(plain.V, fitted.V)
		if changed != tc.converted {
			t.Errorf("%s: converted = %v, want %v", tc.name, changed, tc.converted)
		}
		wantTag := VideoColor{Matrix: ColorMatrixBT601}
		if tc.color.Matrix == ColorMatrixOther {
			wantTag = tc.color
		}
		if fitted.Color != wantTag {
			t.Errorf("%s: result tagged %+v, want %+v", tc.name, fitted.Color, wantTag)
		}
	}
}

// Explicitly full-range sources are compressed to studio range, and the bars
// around the picture stay exactly black whatever is done to the picture.
func TestFitVideoFrameForDVDCompressesFullRangeInsideThePictureOnly(t *testing.T) {
	profile := ntscWideProfile(t)
	for _, tc := range []struct {
		matrix          ColorMatrix
		y, cb, cr       byte
		wantY, wantCb   byte
		wantCr          byte
		tolerance       int
		describeSamples string
	}{
		{ColorMatrixBT601, 255, 128, 128, 235, 128, 128, 0, "white"},
		{ColorMatrixBT601, 0, 128, 128, 16, 128, 128, 0, "black"},
		{ColorMatrixBT601, 128, 255, 0, 126, 240, 16, 1, "chroma extremes"},
		{ColorMatrixBT709, 255, 128, 128, 235, 128, 128, 0, "BT.709 white"},
		{ColorMatrixBT709, 0, 128, 128, 16, 128, 128, 0, "BT.709 black"},
	} {
		// A 4:3 picture in the 16:9 raster occupies columns 90..629.
		source := flatFrame(1440, 1080, tc.y, tc.cb, tc.cr, VideoColor{Matrix: tc.matrix, FullRange: true})
		fitted, err := FitVideoFrameForDVD(source, profile, 4, 3, FitLetterbox)
		if err != nil {
			t.Fatal(err)
		}
		near := func(got, want byte) bool {
			return int(got) >= int(want)-tc.tolerance && int(got) <= int(want)+tc.tolerance
		}
		for y := range fitted.Height {
			for x := range fitted.Width {
				luma := fitted.Y[y*fitted.Width+x]
				cb, cr := fitted.U[y/2*fitted.Width/2+x/2], fitted.V[y/2*fitted.Width/2+x/2]
				if x < 90 || x >= 630 {
					if luma != 16 || cb != 128 || cr != 128 {
						t.Fatalf("%s: bar sample (%d,%d) = %d/%d/%d, want 16/128/128", tc.describeSamples, x, y, luma, cb, cr)
					}
				} else if !near(luma, tc.wantY) || !near(cb, tc.wantCb) || !near(cr, tc.wantCr) {
					t.Fatalf("%s: picture sample (%d,%d) = %d/%d/%d, want %d/%d/%d", tc.describeSamples, x, y, luma, cb, cr, tc.wantY, tc.wantCb, tc.wantCr)
				}
			}
		}
	}

	// Studio-range BT.709 leaves the bars alone as well.
	fitted, err := FitVideoFrameForDVD(flatFrame(1440, 1080, 81, 90, 240, VideoColor{Matrix: ColorMatrixBT709}), profile, 4, 3, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	for y := range fitted.Height {
		for _, x := range []int{0, 45, 89, 630, 719} {
			if luma, cb, cr := fitted.Y[y*fitted.Width+x], fitted.U[y/2*fitted.Width/2+x/2], fitted.V[y/2*fitted.Width/2+x/2]; luma != 16 || cb != 128 || cr != 128 {
				t.Fatalf("bar sample (%d,%d) = %d/%d/%d after BT.709 conversion", x, y, luma, cb, cr)
			}
		}
	}
}

// The luma correction follows chroma interpolated to each luma position, so a
// saturated edge keeps its place: the BT.601 rendering through bilinear chroma
// upsampling matches the BT.709 rendering of the unconverted frame.
func TestFitVideoFrameForDVDTracksChromaEdges(t *testing.T) {
	profile := ntscWideProfile(t)
	const kr709, kb709, kr601, kb601 = 0.2126, 0.0722, 0.299, 0.114
	source := DecodedVideoFrame{Width: 720, Height: 480, Color: VideoColor{Matrix: ColorMatrixBT709},
		Y: make([]byte, 720*480), U: make([]byte, 360*240), V: make([]byte, 360*240)}
	for y := range 480 {
		for x := range 720 {
			source.Y[y*720+x] = byte(60 + (x*3+y*5)%120)
		}
	}
	for y := range 240 {
		for x := range 360 {
			// Saturated blocks with hard edges in both directions.
			source.U[y*360+x] = byte(40 + 170*((x/23+y/17)%2))
			source.V[y*360+x] = byte(220 - 180*((x/31+y/13)%2))
		}
	}
	fitted, err := FitVideoFrameForDVD(source, profile, 16, 9, FitLetterbox)
	if err != nil {
		t.Fatal(err)
	}
	upsampled := func(plane []byte, x, y int) float64 {
		column, row := x/2, y/2
		other := max(row-1, 0)
		if y%2 == 1 {
			other = min(row+1, 239)
		}
		next := column
		if x%2 == 1 {
			next = min(column+1, 359)
		}
		at := func(r, c int) float64 { return float64(plane[r*360+c]) }
		return (3*(at(row, column)+at(row, next)) + at(other, column) + at(other, next)) / 8
	}
	worst := 0.0
	for y := range 480 {
		for x := range 720 {
			want := studioRGB(kr709, kb709, float64(source.Y[y*720+x]), upsampled(source.U, x, y), upsampled(source.V, x, y))
			got := studioRGB(kr601, kb601, float64(fitted.Y[y*720+x]), upsampled(fitted.U, x, y), upsampled(fitted.V, x, y))
			for channel := range got {
				worst = max(worst, math.Abs(got[channel]-want[channel]))
			}
		}
	}
	if worst > 2.5 {
		t.Fatalf("converted picture is up to %.1f RGB codes from the BT.709 original", worst)
	}
}
