package video2dvd_test

import (
	"slices"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

type filmConversion struct {
	// film reports whether the title was taken for film.
	film bool
	// shown is the source frame index of each output frame, and pts and
	// duration its output timing.
	shown    []int
	pts      []time.Duration
	duration []time.Duration
}

// convertTitleFrameTimes pushes frames with the given timestamps through the
// converter the built-in encoder uses for a title, which it chooses by the
// timestamps of the whole track as they are stored.
func convertTitleFrameTimes(t *testing.T, options Options, frames int, pts func(int) time.Duration, regularize func(pts, duration time.Duration) (time.Duration, time.Duration)) filmConversion {
	t.Helper()
	profile, err := ExportProfile(options)
	if err != nil {
		t.Fatal(err)
	}
	var result filmConversion
	times := make([]time.Duration, frames)
	for i := range times {
		times[i] = pts(i)
	}
	converter, film, err := ExportNewDVDTitleFrameRateConverter(profile, times, func(out DecodedVideoFrame) error {
		result.shown = append(result.shown, int(out.Y[0])|int(out.Y[1])<<8|int(out.Y[2])<<16)
		result.pts = append(result.pts, out.PTS)
		result.duration = append(result.duration, out.Duration)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result.film = film
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
	return result
}

// requireFilmGrid checks that output frames sit on the 24000/1001 grid.
func requireFilmGrid(t *testing.T, result filmConversion) {
	t.Helper()
	for index, pts := range result.pts {
		want := exactFrameTime(index, 24000, 1001)
		if pts != want || result.duration[index] != exactFrameTime(index+1, 24000, 1001)-want {
			t.Fatalf("output frame %d is at %v for %v, want %v on the film grid", index, pts, result.duration[index], want)
		}
	}
}

func regularizers(rate time.Duration) map[string]func() func(pts, duration time.Duration) (time.Duration, time.Duration) {
	type regularizer = func(pts, duration time.Duration) (time.Duration, time.Duration)
	return map[string]func() regularizer{
		"as stored":             func() regularizer { return nil },
		"regularized":           func() regularizer { return ExportWebMCadence(0) },
		"regularized with hint": func() regularizer { return ExportWebMCadence(rate) },
	}
}

// A 24000/1001 source is film: every frame goes out once, in order.
func TestTitleFrameRateConverterPassesFilmThrough(t *testing.T) {
	const frames = 6000
	for name, times := range map[string]func(int) time.Duration{
		"exact":       func(i int) time.Duration { return exactFrameTime(i, 24000, 1001) },
		"millisecond": func(i int) time.Duration { return millisecondFrameTime(i, 24000, 1001) },
		"offset":      func(i int) time.Duration { return 7*time.Second + exactFrameTime(i, 24000, 1001) },
	} {
		for regName, regularize := range regularizers(time.Second * 1001 / 24000) {
			result := convertTitleFrameTimes(t, Options{}, frames, times, regularize())
			if !result.film {
				t.Fatalf("%s, %s: not taken for film", name, regName)
			}
			if len(result.shown) != frames {
				t.Fatalf("%s, %s: %d output frames for %d film frames", name, regName, len(result.shown), frames)
			}
			for index, source := range result.shown {
				if source != index {
					t.Fatalf("%s, %s: output frame %d shows source frame %d", name, regName, index, source)
				}
			}
			requireFilmGrid(t, result)
		}
	}
}

// A source at exactly 24 frames per second keeps real time: one frame in
// 1001 is dropped, none is repeated, and the output never drifts from the
// source timestamps.
func TestTitleFrameRateConverterDropsOneFrameIn1001At24(t *testing.T) {
	const frames = 6 * 1001
	for name, times := range map[string]func(int) time.Duration{
		"exact":       func(i int) time.Duration { return exactFrameTime(i, 24, 1) },
		"millisecond": func(i int) time.Duration { return millisecondFrameTime(i, 24, 1) },
	} {
		for regName, regularize := range regularizers(time.Second / 24) {
			result := convertTitleFrameTimes(t, Options{}, frames, times, regularize())
			if !result.film {
				t.Fatalf("%s, %s: not taken for film", name, regName)
			}
			requireFilmGrid(t, result)
			// 6006 frames last 250.25 s, which is 6000 film periods.
			if len(result.shown) != frames-6 {
				t.Fatalf("%s, %s: %d output frames, want %d", name, regName, len(result.shown), frames-6)
			}
			counts, closest := stepCounts(result.shown, 1)
			if counts[2] != 6 && counts[2] != 5 || counts[1]+counts[2] != len(result.shown)-1 {
				t.Fatalf("%s, %s: source steps %v, want only single steps and 5 or 6 drops", name, regName, counts)
			}
			if closest < 900 {
				t.Fatalf("%s, %s: two drops only %d frames apart", name, regName, closest)
			}
			// The first frame is left out when the picture is half a period
			// behind, not when it is most of one.
			first := 0
			for first < len(result.shown) && result.shown[first] == first {
				first++
			}
			if first < 440 || first > 560 {
				t.Fatalf("%s, %s: the first frame is dropped at output frame %d, want about 500", name, regName, first)
			}
			for index, source := range result.shown {
				// The frame shown is the source frame nearest in time: the
				// picture is never more than half a film period, 20.9 ms,
				// from its time in the source, early or late, plus the
				// millisecond the timestamps are rounded by.
				offset := result.pts[index] - exactFrameTime(source, 24, 1)
				if offset < -22*time.Millisecond || offset > 22*time.Millisecond {
					t.Fatalf("%s, %s: output frame %d at %v shows source frame %d from %v", name, regName, index, result.pts[index], source, exactFrameTime(source, 24, 1))
				}
			}
		}
	}
}

// Every other rate, PAL titles and titles with soft telecine turned off take
// the plain converter's path and produce exactly what it does.
func TestTitleFrameRateConverterLeavesOtherRatesAlone(t *testing.T) {
	const frames = 700
	for _, rate := range [][2]int64{{25, 1}, {30000, 1001}, {30, 1}, {50, 1}, {60000, 1001}, {60, 1}, {15, 1}, {12, 1}, {48, 1}, {23, 1}} {
		for name, times := range map[string]func(int) time.Duration{
			"exact":       func(i int) time.Duration { return exactFrameTime(i, rate[0], rate[1]) },
			"millisecond": func(i int) time.Duration { return millisecondFrameTime(i, rate[0], rate[1]) },
		} {
			result := convertTitleFrameTimes(t, Options{}, frames, times, nil)
			if result.film {
				t.Fatalf("%d/%d %s: taken for film", rate[0], rate[1], name)
			}
			if want := convertFrameTimes(t, frames, times, nil); !slices.Equal(result.shown, want) {
				t.Fatalf("%d/%d %s: output differs from the video-rate converter's", rate[0], rate[1], name)
			}
			for index, pts := range result.pts {
				if pts != exactFrameTime(index, 30000, 1001) {
					t.Fatalf("%d/%d %s: output frame %d at %v is off the video grid", rate[0], rate[1], name, index, pts)
				}
			}
		}
	}

	film := func(i int) time.Duration { return exactFrameTime(i, 24000, 1001) }
	disabled := convertTitleFrameTimes(t, Options{DisableSoftTelecine: true}, frames, film, nil)
	if disabled.film || !slices.Equal(disabled.shown, convertFrameTimes(t, frames, film, nil)) {
		t.Fatal("soft telecine turned off: taken for film, or output differs from the video-rate converter's")
	}
	pal := convertTitleFrameTimes(t, Options{Standard: VideoStandardPAL}, frames, film, nil)
	if pal.film || len(pal.shown) == 0 {
		t.Fatalf("PAL: film=%v, %d output frames", pal.film, len(pal.shown))
	}
	for index, pts := range pal.pts {
		if pts != exactFrameTime(index, 25, 1) {
			t.Fatalf("PAL output frame %d at %v is off the 25 fps grid", index, pts)
		}
	}
	if want := int(exactFrameTime(frames, 24000, 1001) * 25 / time.Second); len(pal.shown) < want-1 || len(pal.shown) > want+1 {
		t.Fatalf("PAL: %d output frames, want about %d", len(pal.shown), want)
	}
}

// timesOf lists the timestamps of count frames.
func timesOf(count int, at func(int) time.Duration) []time.Duration {
	times := make([]time.Duration, count)
	for i := range times {
		times[i] = at(i)
	}
	return times
}

// cadenceParts strings passages of constant frame rates together and returns
// the frame times and the end of the last frame.
func cadenceParts(parts ...[3]int64) ([]time.Duration, time.Duration) {
	var starts []time.Duration
	now := time.Duration(0)
	for _, part := range parts {
		for i := range int(part[0]) {
			starts = append(starts, now+exactFrameTime(i, part[1], part[2]))
		}
		now += exactFrameTime(int(part[0]), part[1], part[2])
	}
	return starts, now
}

// A track is film only if it is film throughout: a passage at a higher rate
// anywhere in it would lose frames on the film grid, so such a track is
// converted at the video rate like before.
func TestFilmCadenceNeedsTheWholeTrack(t *testing.T) {
	filmThenVideo, end := cadenceParts([3]int64{240, 24, 1}, [3]int64{300, 30, 1})
	videoThenFilm, _ := cadenceParts([3]int64{300, 30, 1}, [3]int64{240, 24, 1})
	lateBurst, _ := cadenceParts([3]int64{5000, 24000, 1001}, [3]int64{6, 60, 1}, [3]int64{5000, 24000, 1001})
	for name, times := range map[string][]time.Duration{
		"film, then 30 fps": filmThenVideo,
		"30 fps, then film": videoThenFilm,
		// A tenth of a second at 60 fps in seven minutes of film.
		"short burst of 60 fps": lateBurst,
		// Alternating 33 and 50 ms: 24 frames per second on average.
		"alternating": timesOf(300, func(i int) time.Duration {
			return time.Duration(i/2)*83*time.Millisecond + time.Duration(i%2)*33*time.Millisecond
		}),
		// As many frames as film, a quarter of them a frame late.
		"jitter": timesOf(300, func(i int) time.Duration {
			return exactFrameTime(i-i%4/3, 24, 1) + time.Duration(i%4/3)*40*time.Millisecond
		}),
		"24.39 fps":     timesOf(300, func(i int) time.Duration { return time.Duration(i) * 41 * time.Millisecond }),
		"23.5 fps":      timesOf(300, func(i int) time.Duration { return exactFrameTime(i, 47, 2) }),
		"12 fps":        timesOf(300, func(i int) time.Duration { return exactFrameTime(i, 12, 1) }),
		"repeated time": timesOf(300, func(i int) time.Duration { return exactFrameTime(i-i/150, 24, 1) }),
	} {
		if ExportIsFilmCadence(times) {
			t.Errorf("%s: taken for film", name)
		}
	}

	// Converted, the mixed track shows every one of its 30 fps frames, as
	// the video-rate converter does.
	at := func(i int) time.Duration {
		if i >= len(filmThenVideo) {
			return end
		}
		return filmThenVideo[i]
	}
	result := convertTitleFrameTimes(t, Options{}, len(filmThenVideo), at, nil)
	if result.film || !slices.Equal(result.shown, convertFrameTimes(t, len(filmThenVideo), at, nil)) {
		t.Fatalf("film followed by 30 fps: film=%v, or output differs from the video-rate converter's", result.film)
	}
	for source := 240; source < len(filmThenVideo); source++ {
		if !slices.Contains(result.shown, source) {
			t.Fatalf("frame %d of the 30 fps passage is not shown", source)
		}
	}
}

// only returns value if the condition holds and zero otherwise.
func only(condition bool, value time.Duration) time.Duration {
	if condition {
		return value
	}
	return 0
}

// Frames that stay longer than a film period do not make a film track
// something else, nor does a timestamp that is a little off.
func TestFilmCadenceAllowsHeldFrames(t *testing.T) {
	film := func(i int) time.Duration { return exactFrameTime(i, 24, 1) }
	shifted := func(from int, by time.Duration) func(int) time.Duration {
		return func(i int) time.Duration {
			if i >= from {
				return film(i) + by
			}
			return film(i)
		}
	}
	for name, at := range map[string]func(int) time.Duration{
		"first frame held":     shifted(1, 100*time.Millisecond),
		"gap of two seconds":   shifted(20, 2*time.Second),
		"gap, millisecond":     func(i int) time.Duration { return shifted(20, 2*time.Second)(i).Truncate(time.Millisecond) },
		"one frame 5 ms late":  func(i int) time.Duration { return film(i) + only(i == 5, 5*time.Millisecond) },
		"one frame 5 ms early": func(i int) time.Duration { return film(i) - only(i == 5, 5*time.Millisecond) },
	} {
		const frames = 240
		times := timesOf(frames, at)
		if !ExportIsFilmCadence(times) {
			t.Errorf("%s: not taken for film", name)
			continue
		}
		result := convertTitleFrameTimes(t, Options{}, frames, at, nil)
		if !result.film {
			t.Fatalf("%s: the title converter did not choose film", name)
		}
		requireFilmGrid(t, result)
		// Every frame is shown, in order, and while it is on screen in the
		// source, give or take half a film period.
		period := exactFrameTime(1, 24000, 1001)
		for source := range frames {
			if !slices.Contains(result.shown, source) {
				t.Fatalf("%s: source frame %d is not shown", name, source)
			}
		}
		for index, source := range result.shown {
			if index > 0 && source < result.shown[index-1] {
				t.Fatalf("%s: output frame %d goes back to source frame %d", name, index, source)
			}
			if early, late := at(source)-result.pts[index], result.pts[index]-at(source+1); early > period*5/8 || late > -period*3/8 {
				t.Fatalf("%s: output frame %d at %v shows source frame %d of %v..%v", name, index, result.pts[index], source, at(source), at(source+1))
			}
		}
	}
}

// Tracks too short to tell are not film.
func TestFilmCadenceOfShortTracks(t *testing.T) {
	film := func(i int) time.Duration { return millisecondFrameTime(i, 24, 1) }
	for frames, want := range map[int]bool{0: false, 1: false, ExportDVDFilmMinFrames - 1: false, ExportDVDFilmMinFrames: true, 20: true, 49: true} {
		if got := ExportIsFilmCadence(timesOf(frames, film)); got != want {
			t.Errorf("%d frames: film=%v, want %v", frames, got, want)
		}
		result := convertTitleFrameTimes(t, Options{}, frames, film, nil)
		wantFrames := frames
		if !want {
			wantFrames = len(convertFrameTimes(t, frames, film, nil))
		}
		if result.film != want || len(result.shown) != wantFrames {
			t.Errorf("%d frames: film=%v with %d output frames, want %v with %d", frames, result.film, len(result.shown), want, wantFrames)
		}
	}
}

// The film-rate converter itself follows any cadence: at every output time
// it shows the source frame nearest to it, repeating and dropping frames,
// and keeps time.
func TestFilmFrameRateConverterKeepsTimeThroughCadenceChanges(t *testing.T) {
	// 10 s of film, a frame held for 0.7 s, 10 s at 30 fps, 10 s at 15 fps,
	// then film again.
	starts, end := cadenceParts([3]int64{240, 24, 1})
	more, moreEnd := cadenceParts([3]int64{300, 30, 1}, [3]int64{150, 15, 1}, [3]int64{240, 24000, 1001})
	for _, start := range more {
		starts = append(starts, end+700*time.Millisecond+start)
	}
	end += 700*time.Millisecond + moreEnd
	times := func(i int) time.Duration {
		if i >= len(starts) {
			return end
		}
		return starts[i]
	}
	profile, err := ExportProfile(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var result filmConversion
	converter, err := NewDVDFilmFrameRateConverter(profile, func(out DecodedVideoFrame) error {
		result.shown = append(result.shown, int(out.Y[0])|int(out.Y[1])<<8)
		result.pts = append(result.pts, out.PTS)
		result.duration = append(result.duration, out.Duration)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range starts {
		frame := DecodedVideoFrame{PTS: times(i), Duration: times(i+1) - times(i), Width: 2, Height: 2, Y: []byte{byte(i), byte(i >> 8), 0, 0}, U: []byte{128}, V: []byte{128}}
		if err := converter.Push(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := converter.Finish(); err != nil {
		t.Fatal(err)
	}
	requireFilmGrid(t, result)
	period := exactFrameTime(1, 24000, 1001)
	if want := int(end / period); len(result.shown) < want-1 || len(result.shown) > want+1 {
		t.Fatalf("%d output frames for %v of video, want about %d", len(result.shown), end, want)
	}
	repeats, drops := 0, 0
	for index, source := range result.shown {
		if index > 0 {
			switch step := source - result.shown[index-1]; {
			case step < 0:
				t.Fatalf("output frame %d goes back to source frame %d", index, source)
			case step == 0:
				repeats++
			case step > 1:
				drops += step - 1
			}
		}
		if early, late := times(source)-result.pts[index], result.pts[index]-times(source+1); early > period*5/8 || late >= 0 {
			t.Fatalf("output frame %d at %v shows source frame %d of %v..%v", index, result.pts[index], source, times(source), times(source+1))
		}
	}
	// The held frame is shown 17 times, each 15 fps frame once or twice, and
	// a fifth of the 30 fps frames is left out.
	if repeats < 16+60 || drops < 55 {
		t.Fatalf("%d repeated and %d dropped frames do not follow the cadence changes", repeats, drops)
	}
}
