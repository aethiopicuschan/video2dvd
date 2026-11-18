package video2dvd_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

func TestMPEG2EncoderPreservesYUV420ColorWhenDecoded(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 color regression sample")
	}

	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Duration: time.Second * 1001 / 30000,
		Y:        bytes.Repeat([]byte{80}, profile.FrameWidth*profile.FrameHeight),
		U:        bytes.Repeat([]byte{90}, profile.FrameWidth*profile.FrameHeight/4),
		V:        bytes.Repeat([]byte{200}, profile.FrameWidth*profile.FrameHeight/4),
	}
	streamPath := filepath.Join(t.TempDir(), "video.m2v")
	stream, err := os.Create(streamPath)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(stream, profile)
	if err != nil {
		_ = stream.Close()
		t.Fatal(err)
	}
	for range 16 {
		if err := encoder.WriteFrame(frame); err != nil {
			_ = stream.Close()
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		_ = stream.Close()
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	decodedPath := filepath.Join(t.TempDir(), "decoded.yuv")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", streamPath, "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "yuv420p", decodedPath, "-y")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("decode MPEG-2 regression sample: %v: %s", err, output)
	}
	decoded, err := os.ReadFile(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	lumaSize := profile.FrameWidth * profile.FrameHeight
	chromaSize := lumaSize / 4
	if want := lumaSize + 2*chromaSize; len(decoded) != want {
		t.Fatalf("decoded frame size = %d, want %d", len(decoded), want)
	}
	for _, plane := range []struct {
		name string
		data []byte
		want float64
	}{
		{name: "Y", data: decoded[:lumaSize], want: 80},
		{name: "U", data: decoded[lumaSize : lumaSize+chromaSize], want: 90},
		{name: "V", data: decoded[lumaSize+chromaSize:], want: 200},
	} {
		if got := meanByte(plane.data); got < plane.want-1 || got > plane.want+1 {
			t.Errorf("decoded %s mean = %.2f, want %.0f ± 1", plane.name, got, plane.want)
		}
	}
}

func meanByte(values []byte) float64 {
	var sum uint64
	for _, value := range values {
		sum += uint64(value)
	}
	return float64(sum) / float64(len(values))
}
