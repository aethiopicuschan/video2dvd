package mp4todvd

import (
	"bytes"
	"os"
	"path/filepath"
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
			profile, err := (Options{Standard: tc.standard, Aspect: tc.aspect}).profile()
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
	profile, err := (Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9}).profile()
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
