package video2dvd_test

import (
	"bytes"
	"context"
	"image"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
	"github.com/thesyncim/goav/container/webm"
	"github.com/thesyncim/govpx"
)

// writeTestCadenceWebM encodes frames of a moving gradient at the given
// rate, with timestamps on WebM's millisecond tick, and returns the path of
// the file. The first frame is at start.
func writeTestCadenceWebM(t *testing.T, frames int, numerator, denominator int64, start time.Duration) string {
	t.Helper()
	times := make([]time.Duration, frames)
	for index := range times {
		times[index] = start + millisecondFrameTime(index, numerator, denominator)
	}
	return writeTestTimedWebM(t, times)
}

// writeTestTimedWebM writes a small VP9 WebM file with a frame at each of
// the given timestamps.
func writeTestTimedWebM(t *testing.T, times []time.Duration) string {
	t.Helper()
	const width, height = 64, 64
	encoder, err := govpx.NewVP9Encoder(govpx.VP9EncoderOptions{Width: width, Height: height, FPS: 30})
	if err != nil {
		t.Fatalf("create VP9 encoder: %v", err)
	}
	defer encoder.Close()
	path := filepath.Join(t.TempDir(), "cadence.webm")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	muxer, err := webm.NewMuxer(file, webm.MuxerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	trackID, err := muxer.AddTrack(webm.Track{Type: webm.TrackVideo, Codec: webm.CodecVP9, Video: webm.VideoConfig{Width: width, Height: height}})
	if err != nil {
		t.Fatal(err)
	}
	src := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	for i := range src.Cb {
		src.Cb[i], src.Cr[i] = 128, 128
	}
	packet := make([]byte, 64<<10)
	for index := range times {
		for i := range src.Y {
			src.Y[i] = byte(40 + (i%width+2*index)%160)
		}
		n, err := encoder.EncodeInto(src, packet)
		if err != nil || n <= 0 {
			t.Fatalf("encode frame %d: %d bytes, %v", index, n, err)
		}
		if err := muxer.WritePacket(webm.Packet{TrackID: trackID, TimeNS: int64(times[index]), Keyframe: index == 0, Data: packet[:n]}); err != nil {
			t.Fatalf("write frame %d: %v", index, err)
		}
	}
	if err := muxer.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The built-in encoder codes a 24 fps source as film on NTSC, with a title
// and audio length counted in display fields, and as video everywhere else.
func TestInProcessEncoderSoftTelecine(t *testing.T) {
	const frames = 60
	film := writeTestCadenceWebM(t, frames, 24, 1, 0)
	video := writeTestCadenceWebM(t, frames, 30, 1, 0)
	var mixedTimes, heldTimes []time.Duration
	for index := range 48 + 60 {
		if index < 48 {
			mixedTimes = append(mixedTimes, millisecondFrameTime(index, 24, 1))
		} else {
			mixedTimes = append(mixedTimes, 2*time.Second+millisecondFrameTime(index-48, 30, 1))
		}
	}
	for index := range frames {
		heldTimes = append(heldTimes, millisecondFrameTime(index, 24, 1))
		if index > 0 {
			heldTimes[index] += 100 * time.Millisecond
		}
	}
	mixed, held := writeTestTimedWebM(t, mixedTimes), writeTestTimedWebM(t, heldTimes)
	if got, err := ExportWebMVideoFrameTimes(mixed, nil); err != nil || !slices.Equal(got, mixedTimes) {
		t.Fatalf("frame times read from the WebM file differ from those written: %v", err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		options Options
		// pictures and fields are the coded pictures and the display
		// fields expected; repeats is the number of pictures with
		// repeat_first_field.
		pictures, fields, repeats int
	}{
		// 60 film frames are 30 pulldown pairs of five fields: 2.5025 s.
		{"film on NTSC", film, Options{}, 60, 150, 30},
		// On the video grid every fourth frame is shown twice.
		{"film on NTSC, soft telecine off", film, Options{DisableSoftTelecine: true}, 75, 150, 0},
		{"film on PAL", film, Options{Standard: VideoStandardPAL}, 63, 126, 0},
		{"30 fps on NTSC", video, Options{}, 60, 120, 0},
		// Film only at its start is not film: all 60 frames of the 30 fps
		// part are coded, after 48 film frames on 60 video frames.
		{"film, then 30 fps", mixed, Options{}, 120, 240, 0},
		// A first frame held for 0.1 s, three video frames, is film still:
		// it is coded three times, and the frames after it once each. The
		// last picture is the second of a pulldown cycle and gets a third
		// field for the title to end on a whole frame.
		{"film after a held frame", held, Options{}, 62, 156, 32},
	} {
		profile, err := ExportProfile(tc.options)
		if err != nil {
			t.Fatal(err)
		}
		workDir := t.TempDir()
		title, err := NewInProcessEncoder().Encode(context.Background(), tc.path, workDir, profile)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// The frame rate is the display rate of the standard, film or not.
		wantN, wantD := 30000, 1001
		if tc.options.Standard == VideoStandardPAL {
			wantN, wantD = 25, 1
		}
		if title.FrameRateNumerator != wantN || title.FrameRateDenominator != wantD {
			t.Fatalf("%s: title frame rate %d/%d", tc.name, title.FrameRateNumerator, title.FrameRateDenominator)
		}
		wantDuration := time.Duration(tc.fields) * time.Second * time.Duration(wantD) / time.Duration(2*wantN)
		if title.Duration != wantDuration {
			t.Fatalf("%s: title lasts %v, want %v", tc.name, title.Duration, wantDuration)
		}
		stream, err := os.ReadFile(filepath.Join(workDir, title.VideoPath))
		if err != nil {
			t.Fatal(err)
		}
		reader := ExportNewMPEG2PictureReader(bytes.NewReader(stream))
		pictures, fields, repeats := 0, 0, 0
		for {
			picture, err := reader.Next()
			if err != nil {
				break
			}
			count, _, repeat, progressive, err := ExportMPEG2PictureDisplay(picture)
			if err != nil || !progressive {
				t.Fatalf("%s: picture %d: progressive_frame=%v, %v", tc.name, pictures, progressive, err)
			}
			pictures++
			fields += count
			if repeat {
				repeats++
			}
		}
		if pictures != tc.pictures || fields != tc.fields || repeats != tc.repeats {
			t.Fatalf("%s: %d pictures shown for %d fields, %d with a repeated field; want %d, %d, %d", tc.name, pictures, fields, repeats, tc.pictures, tc.fields, tc.repeats)
		}
		// Audio, silence here, is as long as the video is displayed.
		audio, err := os.Stat(filepath.Join(workDir, title.AudioPath))
		if err != nil {
			t.Fatal(err)
		}
		wantSamples := (int64(tc.fields)*int64(wantD)*48000 + int64(wantN)) / int64(2*wantN)
		if audio.Size() != wantSamples*4 {
			t.Fatalf("%s: %d audio samples for %d fields, want %d", tc.name, audio.Size()/4, tc.fields, wantSamples)
		}
		if err := MuxDVDTitleVOB(context.Background(), title, workDir, io.Discard); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

// A WebM whose video starts late keeps that start as time zero of the title,
// film or not.
func TestPrepareWebMTitleVideoReportsStartOfShortFilm(t *testing.T) {
	profile, err := ExportProfile(Options{})
	if err != nil {
		t.Fatal(err)
	}
	const start = 500 * time.Millisecond
	for frames, wantFilm := range map[int]bool{5: false, 20: true, 53: true} {
		path := writeTestCadenceWebM(t, frames, 24, 1, start)
		produced, got, film, err := ExportPrepareWebMTitleVideo(context.Background(), path, profile)
		if err != nil {
			t.Fatal(err)
		}
		if got != start || film != wantFilm || produced < frames {
			t.Fatalf("%d frames from %v: %d output frames from %v, film=%v, want film=%v", frames, start, produced, got, film, wantFilm)
		}
	}
}
