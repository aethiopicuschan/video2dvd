package video2dvd_test

import (
	"bytes"
	"context"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
	"github.com/thesyncim/goav/container/webm"
	"github.com/thesyncim/govpx"
)

// writeTestVP9WebM encodes a few saturated frames at 30 fps, with timestamps
// on WebM's millisecond tick, and returns the path of the file.
func writeTestVP9WebM(t *testing.T, colorSpace govpx.VP9ColorSpace, colorRange govpx.VP9ColorRange, colour webm.VideoColourConfig) string {
	t.Helper()
	const width, height, frames = 64, 64, 12
	encoder, err := govpx.NewVP9Encoder(govpx.VP9EncoderOptions{Width: width, Height: height, FPS: 30, ColorSpace: colorSpace, ColorRange: colorRange})
	if err != nil {
		t.Fatalf("create VP9 encoder: %v", err)
	}
	defer encoder.Close()
	path := filepath.Join(t.TempDir(), "colour.webm")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	muxer, err := webm.NewMuxer(file, webm.MuxerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	trackID, err := muxer.AddTrack(webm.Track{Type: webm.TrackVideo, Codec: webm.CodecVP9, Video: webm.VideoConfig{Width: width, Height: height, Colour: colour}})
	if err != nil {
		t.Fatal(err)
	}
	src := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	for i := range src.Cb {
		src.Cb[i], src.Cr[i] = 90, 200
	}
	packet := make([]byte, 64<<10)
	for index := range frames {
		for i := range src.Y {
			src.Y[i] = byte(60 + 8*index)
		}
		n, err := encoder.EncodeInto(src, packet)
		if err != nil || n <= 0 {
			t.Fatalf("encode frame %d: %d bytes, %v", index, n, err)
		}
		timestamp := time.Duration(index) * time.Second / 30 / time.Millisecond * time.Millisecond
		if err := muxer.WritePacket(webm.Packet{TrackID: trackID, TimeNS: int64(timestamp), Keyframe: index == 0, Data: packet[:n]}); err != nil {
			t.Fatalf("write frame %d: %v", index, err)
		}
	}
	if err := muxer.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The colour tag has to travel from the container and the VP9 key frame
// through decoding into the preparation path, which is where every Converter
// user gets the BT.601 studio-range conversion.
func TestPrepareWebMVideoForDVDAppliesTheSourceColour(t *testing.T) {
	profile := ntscWideProfile(t)
	for _, tc := range []struct {
		name       string
		colorSpace govpx.VP9ColorSpace
		colorRange govpx.VP9ColorRange
		colour     webm.VideoColourConfig
		probed     VideoColor
		decoded    VideoColor
		converts   bool
	}{
		{"BT.709 in the bitstream only", govpx.VP9ColorSpaceBT709, govpx.VP9ColorRangeStudio, webm.VideoColourConfig{},
			VideoColor{}, VideoColor{Matrix: ColorMatrixBT709}, true},
		{"container BT.601 overrides the bitstream", govpx.VP9ColorSpaceBT709, govpx.VP9ColorRangeStudio, webm.VideoColourConfig{MatrixCoefficients: 6, MatrixCoefficientsSet: true},
			VideoColor{Matrix: ColorMatrixBT601}, VideoColor{Matrix: ColorMatrixBT601}, false},
		{"container BT.709", govpx.VP9ColorSpaceUnknown, govpx.VP9ColorRangeStudio, webm.VideoColourConfig{MatrixCoefficients: 1, MatrixCoefficientsSet: true},
			VideoColor{Matrix: ColorMatrixBT709}, VideoColor{Matrix: ColorMatrixBT709}, true},
		{"untagged small picture", govpx.VP9ColorSpaceUnknown, govpx.VP9ColorRangeStudio, webm.VideoColourConfig{},
			VideoColor{}, VideoColor{}, false},
		{"full range in the bitstream", govpx.VP9ColorSpaceBT601, govpx.VP9ColorRangeFull, webm.VideoColourConfig{},
			VideoColor{}, VideoColor{Matrix: ColorMatrixBT601, FullRange: true}, true},
	} {
		path := writeTestVP9WebM(t, tc.colorSpace, tc.colorRange, tc.colour)
		info, err := ProbeWebM(path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := info.Tracks[0].Color; got != tc.probed {
			t.Errorf("%s: probed colour %+v, want %+v", tc.name, got, tc.probed)
		}
		var decoded []DecodedVideoFrame
		if err := DecodeWebMVideo(context.Background(), path, nil, func(frame DecodedVideoFrame) error {
			decoded = append(decoded, frame)
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(decoded) != 12 {
			t.Fatalf("%s: decoded %d frames, want 12", tc.name, len(decoded))
		}
		for index, frame := range decoded {
			if frame.Color != tc.decoded {
				t.Fatalf("%s: decoded frame %d colour %+v, want %+v", tc.name, index, frame.Color, tc.decoded)
			}
			// Decoding reports the container's own timestamps.
			if want := time.Duration(index) * time.Second / 30 / time.Millisecond * time.Millisecond; frame.PTS != want {
				t.Fatalf("%s: decoded frame %d at %v, want %v", tc.name, index, frame.PTS, want)
			}
		}
		var prepared []DecodedVideoFrame
		if err := PrepareWebMVideoForDVD(context.Background(), path, nil, profile, 16, 9, FitLetterbox, func(frame DecodedVideoFrame) error {
			prepared = append(prepared, frame)
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(prepared) != len(decoded) {
			t.Fatalf("%s: prepared %d frames from %d at an equal cadence", tc.name, len(prepared), len(decoded))
		}
		for index, frame := range prepared {
			converted, err := FitVideoFrameForDVD(decoded[index], profile, 16, 9, FitLetterbox)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frame.Y, converted.Y) || !bytes.Equal(frame.U, converted.U) || !bytes.Equal(frame.V, converted.V) {
				t.Fatalf("%s: prepared frame %d is not FitVideoFrameForDVD of decoded frame %d", tc.name, index, index)
			}
			resampled, err := FitVideoFrame(decoded[index], profile, 16, 9, FitLetterbox)
			if err != nil {
				t.Fatal(err)
			}
			if changed := !bytes.Equal(frame.Y, resampled.Y) || !bytes.Equal(frame.U, resampled.U); changed != tc.converts {
				t.Fatalf("%s: prepared frame %d differs from the resampled source: %v, want %v", tc.name, index, changed, tc.converts)
			}
		}
	}
}
