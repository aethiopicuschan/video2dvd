package video2dvd_test

import (
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// exactFrameTime is frame index at rate numerator/denominator frames per
// second; millisecondFrameTime is the same rounded to WebM's tick.
func exactFrameTime(index int, numerator, denominator int64) time.Duration {
	return time.Duration(int64(index) * denominator * int64(time.Second) / numerator)
}

func millisecondFrameTime(index int, numerator, denominator int64) time.Duration {
	return (exactFrameTime(index, numerator, denominator) + time.Millisecond/2) / time.Millisecond * time.Millisecond
}

// convertFrameTimes pushes frames with the given timestamps through the NTSC
// converter and returns the source frame index shown by each output frame.
// regularize, when not nil, is applied first, as the WebM preparation path
// does.
func convertFrameTimes(t *testing.T, frames int, pts func(int) time.Duration, regularize func(pts, duration time.Duration) (time.Duration, time.Duration)) []int {
	t.Helper()
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC})
	if err != nil {
		t.Fatal(err)
	}
	var shown []int
	converter, err := NewDVDFrameRateConverter(profile, func(out DecodedVideoFrame) error {
		shown = append(shown, int(out.Y[0])|int(out.Y[1])<<8|int(out.Y[2])<<16)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range frames {
		start, duration := pts(i), pts(i+1)-pts(i)
		if regularize != nil {
			start, duration = regularize(start, duration)
		}
		frame := DecodedVideoFrame{PTS: start, Duration: duration, Width: 2, Height: 2, Y: []byte{byte(i), byte(i >> 8), byte(i >> 16), 0}, U: []byte{128}, V: []byte{128}}
		if err := converter.Push(frame); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := converter.Finish(); err != nil {
		t.Fatal(err)
	}
	return shown
}

// stepCounts tallies the source-index advance between consecutive output
// frames and the shortest distance, in output frames, between two advances
// that are not the regular one.
func stepCounts(shown []int, regular int) (counts map[int]int, closest int) {
	counts = make(map[int]int)
	closest, last := len(shown), -1
	for i := 1; i < len(shown); i++ {
		step := shown[i] - shown[i-1]
		counts[step]++
		if step != regular {
			if last >= 0 {
				closest = min(closest, i-last)
			}
			last = i
		}
	}
	return counts, closest
}

// Millisecond-rounded timestamps of sources slightly faster than a multiple
// of the output rate used to flip the frame choice back and forth for dozens
// of frames whenever the slow phase drift came near the decision point. The
// converter must make the one frame adjustment the rate difference requires,
// both on its own and behind the WebM timestamp regularizer.
func TestFrameRateConverterDropsOncePerDriftPeriod(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		rate                   int64
		frames, regular, extra int
	}{
		{"30 fps", 30, 6000, 1, 2},
		{"60 fps", 60, 12000, 2, 3},
	} {
		pts := func(i int) time.Duration { return millisecondFrameTime(i, tc.rate, 1) }
		for mode, regularize := range map[string]func(time.Duration, time.Duration) (time.Duration, time.Duration){
			"converter alone":        nil,
			"regularized timestamps": ExportWebMCadence(0),
			"regularized with hint":  ExportWebMCadence(time.Second / time.Duration(tc.rate)),
			"regularized, bad hint":  ExportWebMCadence(33 * time.Millisecond),
		} {
			shown := convertFrameTimes(t, tc.frames, pts, regularize)
			counts, closest := stepCounts(shown, tc.regular)
			// 200 s of video is 5994 output frames, and the source runs one
			// frame (30 fps) or two (60 fps) per 1001 output frames ahead.
			if len(shown) != 5994 {
				t.Errorf("%s, %s: %d output frames, want 5994", tc.name, mode, len(shown))
			}
			wantExtra := (tc.frames + 500) / 1001
			if len(counts) != 2 || counts[tc.extra] < wantExtra-1 || counts[tc.extra] > wantExtra+1 {
				t.Errorf("%s, %s: source steps %v, want only %d (regular) and about %d of %d", tc.name, mode, counts, tc.regular, wantExtra, tc.extra)
			}
			if closest < 400 {
				t.Errorf("%s, %s: two irregular steps only %d output frames apart", tc.name, mode, closest)
			}
		}
	}
}

func TestFrameRateConverterKeepsOtherCadences(t *testing.T) {
	for mode, regularize := range map[string]func() func(time.Duration, time.Duration) (time.Duration, time.Duration){
		"converter alone":        func() func(time.Duration, time.Duration) (time.Duration, time.Duration) { return nil },
		"regularized timestamps": func() func(time.Duration, time.Duration) (time.Duration, time.Duration) { return ExportWebMCadence(0) },
		"exact timestamps":       func() func(time.Duration, time.Duration) (time.Duration, time.Duration) { return nil },
	} {
		// 29.97 fps maps one to one.
		shown := convertFrameTimes(t, 6000, func(i int) time.Duration { return millisecondFrameTime(i, 30000, 1001) }, regularize())
		if counts, _ := stepCounts(shown, 1); len(shown) != 6000 || len(counts) != 1 || counts[1] != 5999 {
			t.Errorf("29.97 fps, %s: %d output frames with steps %v, want 6000 one-to-one", mode, len(shown), counts)
		}

		// 24 fps repeats every fourth frame: 2-1-1-1, five outputs per four
		// source frames, with nothing dropped.
		const filmFrames = 4800
		filmTime := func(i int) time.Duration { return millisecondFrameTime(i, 24, 1) }
		if mode == "exact timestamps" {
			filmTime = func(i int) time.Duration { return exactFrameTime(i, 24, 1) }
		}
		shown = convertFrameTimes(t, filmFrames, filmTime, regularize())
		counts, _ := stepCounts(shown, 1)
		if len(counts) != 2 || counts[1] != filmFrames-1 {
			t.Errorf("24 fps, %s: steps %v, want every source frame shown and only repeats besides", mode, counts)
		}
		if len(shown) != 5994 {
			t.Errorf("24 fps, %s: %d output frames, want 5994", mode, len(shown))
		}
		// Five outputs span four source frames, except where the 0.1%
		// difference between 24 fps and four fifths of 29.97 fps skips a
		// repeat.
		slips := 0
		for start := 0; start+5 < len(shown); start += 5 {
			switch shown[start+5] - shown[start] {
			case 4:
			case 5:
				slips++
			default:
				t.Fatalf("24 fps, %s: output frames %d..%d show source frames %v", mode, start, start+5, shown[start:start+6])
			}
		}
		if slips > 7 {
			t.Errorf("24 fps, %s: %d groups of five outputs without a repeat, want about 6", mode, slips)
		}
	}

}

func TestWebMCadenceRestoresExactFrameTimes(t *testing.T) {
	for _, rate := range [][2]int64{{24, 1}, {25, 1}, {30, 1}, {50, 1}, {60, 1}, {24000, 1001}, {30000, 1001}, {60000, 1001}, {15, 1}, {120, 1}} {
		for _, hint := range []time.Duration{0, exactFrameTime(1, rate[0], rate[1]), 33 * time.Millisecond} {
			for _, origin := range []time.Duration{0, 7 * time.Millisecond, 1234567 * time.Microsecond} {
				regularize := ExportWebMCadence(hint)
				previous := time.Duration(-1)
				for i := range 20000 {
					raw := origin + millisecondFrameTime(i, rate[0], rate[1])
					got, duration := regularize(raw, time.Millisecond)
					if i == 0 && got != raw {
						t.Fatalf("%d/%d fps: first timestamp %v became %v", rate[0], rate[1], raw, got)
					}
					if got < previous || duration <= 0 {
						t.Fatalf("%d/%d fps: frame %d at %v after %v, duration %v", rate[0], rate[1], i, got, previous, duration)
					}
					previous = got
					if difference := (got - raw).Abs(); difference > 1250*time.Microsecond {
						t.Fatalf("%d/%d fps: frame %d moved %v from its container timestamp", rate[0], rate[1], i, difference)
					}
					// Once the rate is pinned down, every frame is on the
					// exact grid through the first frame, which here has
					// no rounding of its own. The container timestamps
					// themselves are up to half a tick off that grid.
					if i >= 2000 {
						want := origin + exactFrameTime(i, rate[0], rate[1])
						if difference := (got - want).Abs(); difference > time.Microsecond {
							t.Fatalf("%d/%d fps, hint %v: frame %d at %v, want %v", rate[0], rate[1], hint, i, got, want)
						}
					}
				}
			}
		}
	}
}

// Timestamps that no constant frame period explains are left as they are.
func TestWebMCadenceFollowsVariableFrameRate(t *testing.T) {
	regularize := ExportWebMCadence(33 * time.Millisecond)
	pts := time.Duration(0)
	deltas := []time.Duration{33, 50, 33, 100, 17, 33, 67, 33, 34, 250, 16, 17, 33}
	for i := range 4000 {
		got, duration := regularize(pts, 20*time.Millisecond)
		if got != pts || duration != 20*time.Millisecond {
			t.Fatalf("variable-rate frame %d at %v became %v (duration %v)", i, pts, got, duration)
		}
		pts += deltas[i%len(deltas)] * time.Millisecond
	}

	// A constant-rate stream with a gap restarts at the gap and regains the
	// exact grid after it, anchored at the first frame behind the gap.
	regularize = ExportWebMCadence(0)
	const gapFrame = 500
	gap := 2500 * time.Millisecond
	for i := range 3000 {
		raw := millisecondFrameTime(i, 30, 1)
		if i >= gapFrame {
			raw += gap
		}
		got, _ := regularize(raw, time.Millisecond)
		if i == gapFrame && got != raw {
			t.Fatalf("frame after the gap at %v became %v", raw, got)
		}
		if difference := (got - raw).Abs(); difference > 1250*time.Microsecond {
			t.Fatalf("frame %d moved %v from its container timestamp", i, difference)
		}
		if i > gapFrame+ExportWebMCadenceMinFrames+200 {
			want := millisecondFrameTime(gapFrame, 30, 1) + gap + exactFrameTime(i-gapFrame, 30, 1)
			if difference := (got - want).Abs(); difference > time.Microsecond {
				t.Fatalf("frame %d after the gap at %v, want %v", i, got, want)
			}
		}
	}
}
