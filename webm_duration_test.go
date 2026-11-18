package video2dvd_test

import (
	. "github.com/aethiopicuschan/video2dvd"
	"strings"
	"testing"
	"time"
)

func TestWebMFrameDurationResolverUsesNextPTSWithoutBlockOrDefaultDuration(t *testing.T) {
	var got []DecodedVideoFrame
	resolver := TestNewWebMFrameDurationResolver(0, func(frame DecodedVideoFrame) error {
		got = append(got, frame)
		return nil
	})
	first := testWebMDurationFrame(0, 16*time.Millisecond)
	first.Duration = 0
	second := testWebMDurationFrame(16*time.Millisecond, 0)
	second.Duration = 0
	if err := resolver.Push(first); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Push(second); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Duration != 16*time.Millisecond || got[1].Duration <= 0 {
		var durations []time.Duration
		for _, frame := range got {
			durations = append(durations, frame.Duration)
		}
		t.Fatalf("resolved durations = %#v", durations)
	}
}

func TestWebMFrameDurationResolverRejectsUnrecoverableFinalDuration(t *testing.T) {
	resolver := TestNewWebMFrameDurationResolver(0, func(DecodedVideoFrame) error { return nil })
	frame := testWebMDurationFrame(0, 0)
	if err := resolver.Push(frame); err != nil {
		t.Fatal(err)
	}
	err := resolver.Finish()
	if err == nil || !strings.Contains(err.Error(), "final WebM frame duration") {
		t.Fatalf("error = %v, want an explanatory final-duration error", err)
	}
}

func TestWebMFrameDurationResolverPreservesPacketDuration(t *testing.T) {
	var got []DecodedVideoFrame
	resolver := TestNewWebMFrameDurationResolver(40*time.Millisecond, func(frame DecodedVideoFrame) error {
		got = append(got, frame)
		return nil
	})
	frame := testWebMDurationFrame(0, 33*time.Millisecond)
	if err := resolver.Push(frame); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Duration != 33*time.Millisecond {
		if len(got) == 0 {
			t.Fatal("no frame emitted")
		}
		t.Fatalf("duration = %v, want packet duration", got[0].Duration)
	}
}

func TestWebMFrameDurationResolverUsesTrackDefaultDuration(t *testing.T) {
	var got []DecodedVideoFrame
	resolver := TestNewWebMFrameDurationResolver(40*time.Millisecond, func(frame DecodedVideoFrame) error {
		got = append(got, frame)
		return nil
	})
	if err := resolver.Push(testWebMDurationFrame(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Duration != 40*time.Millisecond {
		t.Fatalf("duration = %v, want track default duration", got[0].Duration)
	}
}

func testWebMDurationFrame(pts, duration time.Duration) DecodedVideoFrame {
	return DecodedVideoFrame{PTS: pts, Duration: duration, Width: 2, Height: 2, Y: []byte{16, 16, 16, 16}, U: []byte{128}, V: []byte{128}}
}
