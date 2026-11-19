package video2dvd_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

func TestMPEG2EncoderPreservesFineLumaDetail(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 quality regression sample")
	}
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Duration: time.Second * 1001 / 30000,
		Y:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight),
		U:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	for y := 224; y < 256; y++ {
		for x := 320; x < 352; x++ {
			value := byte(60)
			if (x/4+y/4)%2 != 0 {
				value = 196
			}
			frame.Y[y*frame.Width+x] = value
		}
	}
	streamPath := filepath.Join(t.TempDir(), "detail.m2v")
	stream, err := os.Create(streamPath)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(stream, profile)
	if err != nil {
		_ = stream.Close()
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(frame); err != nil {
		_ = stream.Close()
		t.Fatal(err)
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
		t.Fatalf("decode MPEG-2 detail sample: %v: %s", err, output)
	}
	decoded, err := os.ReadFile(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	lumaSize := frame.Width * frame.Height
	if len(decoded) < lumaSize {
		t.Fatalf("decoded frame has %d luma bytes, want at least %d", len(decoded), lumaSize)
	}
	var squaredError uint64
	var sampleCount uint64
	for y := 224; y < 256; y++ {
		for x := 320; x < 352; x++ {
			i := y*frame.Width + x
			delta := int(decoded[i]) - int(frame.Y[i])
			squaredError += uint64(delta * delta)
			sampleCount++
		}
	}
	mse := float64(squaredError) / float64(sampleCount)
	psnr := 10 * math.Log10(255*255/mse)
	if psnr < 30 {
		t.Fatalf("decoded luma PSNR = %.1f dB; fine detail was lost (want at least 30 dB)", psnr)
	}
}

func TestMPEG2EncoderPreservesFineLumaDetailInPredictiveFrame(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 predictive quality sample")
	}
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	blank := DecodedVideoFrame{
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Duration: time.Second * 1001 / 30000,
		Y:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight),
		U:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	detail := ExportCloneDecodedVideoFrame(blank)
	for y := 224; y < 256; y++ {
		for x := 320; x < 352; x++ {
			value := byte(60)
			if (x/4+y/4)%2 != 0 {
				value = 196
			}
			detail.Y[y*detail.Width+x] = value
		}
	}

	streamPath := filepath.Join(t.TempDir(), "predictive-detail.m2v")
	stream, err := os.Create(streamPath)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(stream, profile)
	if err != nil {
		_ = stream.Close()
		t.Fatal(err)
	}
	for _, frame := range []DecodedVideoFrame{blank, detail} {
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
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", streamPath, "-frames:v", "2", "-f", "rawvideo", "-pix_fmt", "yuv420p", decodedPath, "-y")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("decode MPEG-2 predictive detail sample: %v: %s", err, output)
	}
	decoded, err := os.ReadFile(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	frameSize := profile.FrameWidth * profile.FrameHeight * 3 / 2
	if len(decoded) < 2*frameSize {
		t.Fatalf("decoded sequence has %d bytes, want at least %d", len(decoded), 2*frameSize)
	}
	secondLuma := decoded[frameSize : frameSize+profile.FrameWidth*profile.FrameHeight]
	var squaredError uint64
	var sampleCount uint64
	for y := 224; y < 256; y++ {
		for x := 320; x < 352; x++ {
			i := y*detail.Width + x
			delta := int(secondLuma[i]) - int(detail.Y[i])
			squaredError += uint64(delta * delta)
			sampleCount++
		}
	}
	mse := float64(squaredError) / float64(sampleCount)
	psnr := 10 * math.Log10(255*255/mse)
	if psnr < 30 {
		t.Fatalf("predictive-frame luma PSNR = %.1f dB; fine detail was lost (want at least 30 dB)", psnr)
	}
}
