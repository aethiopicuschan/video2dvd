package video2dvd_test

import (
	"context"
	. "github.com/aethiopicuschan/video2dvd"
	"os"
	"testing"
	"time"

	mp4audio "github.com/rcarmo/go-264/audio/mp4"
)

// These optional full-file regressions use user-supplied media and keep those
// files out of the repository. Set the corresponding environment variable to
// rerun a local reproducer; ordinary go test runs remain hermetic.
func TestMP4AACAlignmentPaddingRegression(t *testing.T) {
	path := os.Getenv("VIDEO2DVD_TEST_AAC_MP4")
	if path == "" {
		t.Skip("set VIDEO2DVD_TEST_AAC_MP4 to run the full-file AAC regression")
	}
	info, err := ProbeMedia(path)
	if err != nil {
		t.Fatal(err)
	}
	track, err := ExportSelectAudioTrack(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	var frames int64
	var chunks int
	err = DecodeMP4Audio(context.Background(), path, &track.Index, func(chunk PCMChunk) error {
		wantStart := time.Duration(frames) * time.Second / time.Duration(chunk.SampleRate)
		if chunk.Start != wantStart {
			t.Fatalf("chunk %d starts at %s; want %s", chunks, chunk.Start, wantStart)
		}
		if chunk.SampleRate != 48000 || chunk.Channels != 2 || len(chunk.Samples)%2 != 0 {
			t.Fatalf("chunk %d has invalid output format: %+v", chunks, chunk)
		}
		frames += int64(len(chunk.Samples) / chunk.Channels)
		chunks++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	demux, err := mp4audio.OpenTrack(context.Background(), file, stat.Size(), mp4audio.Limits{MaxBytes: stat.Size(), MaxDurationSeconds: 24 * 60 * 60}, track.Index)
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	audioTrack := demux.Track()
	timing, err := audioTrack.TimingPlan(int64(audioTrack.SampleCount * ExportAACFallbackDecodedFramesPerSample))
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := (timing.OutputFrames*48000 + int64(audioTrack.SampleRate) - 1) / int64(audioTrack.SampleRate)
	if chunks == 0 || frames != wantFrames {
		t.Fatalf("decoded %d chunks and %d frames; want non-empty output of %d frames", chunks, frames, wantFrames)
	}
}

func TestWebMVP9FullFileRegression(t *testing.T) {
	path := os.Getenv("VIDEO2DVD_TEST_VP9_WEBM")
	if path == "" {
		t.Skip("set VIDEO2DVD_TEST_VP9_WEBM to run the full-file VP9 regression")
	}
	info, err := ProbeMedia(path)
	if err != nil {
		t.Fatal(err)
	}
	track, err := ExportSelectWebMVideoTrack(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var lastPTS time.Duration
	err = DecodeWebMVideo(context.Background(), path, &track.Index, func(frame DecodedVideoFrame) error {
		if frame.Width <= 0 || frame.Height <= 0 || len(frame.Y) != frame.Width*frame.Height {
			t.Fatalf("frame %d has invalid dimensions or luma plane", count)
		}
		if count > 0 && frame.PTS <= lastPTS {
			t.Fatalf("frame %d PTS %s does not follow %s", count, frame.PTS, lastPTS)
		}
		lastPTS = frame.PTS
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || lastPTS >= info.Duration {
		t.Fatalf("decoded %d frames through %s; track duration is %s", count, lastPTS, info.Duration)
	}
}
