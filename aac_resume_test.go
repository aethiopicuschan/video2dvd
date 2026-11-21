package video2dvd_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"

	"github.com/rcarmo/go-264/audio/pcm"
)

// errAlignmentPadding has the shape of the primary decoder's rejection as
// decodeMP4AudioPrimary returns it.
var errAlignmentPadding = fmt.Errorf("video2dvd: decode audio: %w", fmt.Errorf("%w: non-zero alignment padding", pcm.ErrMalformed))

type pcmDecoder = func(func(PCMChunk) error) error

// scriptedPCMDecoder delivers the first frames of a ramp in chunks of the given
// size and then returns result.
func scriptedPCMDecoder(calls *int, frames, chunkFrames int, result error) pcmDecoder {
	return func(handler func(PCMChunk) error) error {
		*calls++
		for frame := 0; frame < frames; frame += chunkFrames {
			count := min(chunkFrames, frames-frame)
			err := handler(PCMChunk{
				Start:      time.Duration(frame) * time.Second / 48000,
				SampleRate: 48000,
				Channels:   2,
				Samples:    rampPCM(frame, count),
			})
			if err != nil {
				return err
			}
		}
		return result
	}
}

// pcmCollector checks that chunks form one contiguous 48 kHz stereo stream.
type pcmCollector struct {
	t       *testing.T
	samples []int16
	chunks  int
}

func (c *pcmCollector) write(chunk PCMChunk) error {
	c.t.Helper()
	want := time.Duration(len(c.samples)/2) * time.Second / 48000
	if chunk.Start != want || chunk.SampleRate != 48000 || chunk.Channels != 2 || len(chunk.Samples) == 0 || len(chunk.Samples)%2 != 0 {
		c.t.Fatalf("chunk %d: start %s, rate %d, channels %d, %d samples; want a non-empty 48 kHz stereo chunk at %s",
			c.chunks, chunk.Start, chunk.SampleRate, chunk.Channels, len(chunk.Samples), want)
	}
	c.samples = append(c.samples, chunk.Samples...)
	c.chunks++
	return nil
}

func TestAudioFallbackResumesAfterDeliveredChunks(t *testing.T) {
	const total = 20000
	for _, test := range []struct {
		name                   string
		delivered, primarySize int
		fallbackSize           int
	}{
		{"nothing delivered", 0, 1089, 8192},
		{"one chunk", 1089, 1089, 8192},
		{"several chunks ending inside a fallback chunk", 3 * 4096, 4096, 5000},
		{"several chunks ending on a fallback chunk boundary", 2 * 4096, 4096, 4096},
		{"partial last chunk", 4096 + 123, 4096, 1000},
		{"everything delivered before the failure", total, 4096, 8192},
	} {
		t.Run(test.name, func(t *testing.T) {
			var primaryCalls, fallbackCalls int
			collector := &pcmCollector{t: t}
			err := ExportDecodePCMWithFallback(
				scriptedPCMDecoder(&primaryCalls, test.delivered, test.primarySize, errAlignmentPadding),
				scriptedPCMDecoder(&fallbackCalls, total, test.fallbackSize, nil),
				collector.write,
			)
			if err != nil {
				t.Fatal(err)
			}
			if primaryCalls != 1 || fallbackCalls != 1 {
				t.Fatalf("primary ran %d times and fallback %d times; want once each", primaryCalls, fallbackCalls)
			}
			if !slices.Equal(collector.samples, rampPCM(0, total)) {
				t.Fatalf("handler received %d frames that are not the %d-frame stream exactly once", len(collector.samples)/2, total)
			}
		})
	}
}

func TestAudioFallbackOutputIsUnchangedWhenNothingWasDelivered(t *testing.T) {
	var calls int
	var direct, switched []PCMChunk
	fallback := scriptedPCMDecoder(&calls, 20000, 8192, nil)
	if err := fallback(func(chunk PCMChunk) error { direct = append(direct, chunk); return nil }); err != nil {
		t.Fatal(err)
	}
	err := ExportDecodePCMWithFallback(
		scriptedPCMDecoder(&calls, 0, 1, errAlignmentPadding),
		fallback,
		func(chunk PCMChunk) error { switched = append(switched, chunk); return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(direct, switched, func(a, b PCMChunk) bool {
		return a.Start == b.Start && a.SampleRate == b.SampleRate && a.Channels == b.Channels && slices.Equal(a.Samples, b.Samples)
	}) {
		t.Fatal("chunks differ from the fallback decoder's own chunks")
	}
}

func TestAudioFallbackIsOnlyForThePrimaryAlignmentPaddingFailure(t *testing.T) {
	broken := fmt.Errorf("video2dvd: decode audio: %w", fmt.Errorf("%w: invalid scalefactor", pcm.ErrMalformed))
	for _, test := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"other malformed stream", broken},
		{"no progress", io.ErrNoProgress},
		{"cancellation", context.Canceled},
		{"message without the decoder's malformed sentinel", errors.New("non-zero alignment padding")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var primaryCalls, fallbackCalls int
			collector := &pcmCollector{t: t}
			err := ExportDecodePCMWithFallback(
				scriptedPCMDecoder(&primaryCalls, 5000, 4096, test.err),
				scriptedPCMDecoder(&fallbackCalls, 20000, 8192, nil),
				collector.write,
			)
			if err != test.err {
				t.Fatalf("error = %v; want the primary decoder's %v unchanged", err, test.err)
			}
			if fallbackCalls != 0 {
				t.Fatal("fallback decoder ran")
			}
			if !slices.Equal(collector.samples, rampPCM(0, 5000)) {
				t.Fatal("handler output is not the primary decoder's output")
			}
		})
	}
}

func TestAudioFallbackReturnsHandlerErrorsUnchanged(t *testing.T) {
	// Even an error that looks exactly like the decoder's rejection must not
	// start the fallback when it comes from the caller's handler.
	handlerErr := fmt.Errorf("sink: %w", errAlignmentPadding)
	t.Run("during the primary decoder", func(t *testing.T) {
		var primaryCalls, fallbackCalls, chunks int
		err := ExportDecodePCMWithFallback(
			scriptedPCMDecoder(&primaryCalls, 20000, 4096, nil),
			scriptedPCMDecoder(&fallbackCalls, 20000, 8192, nil),
			func(PCMChunk) error {
				chunks++
				if chunks == 3 {
					return handlerErr
				}
				return nil
			},
		)
		if err != handlerErr || fallbackCalls != 0 || chunks != 3 {
			t.Fatalf("error = %v after %d chunks with %d fallback runs; want the handler error after 3 chunks and no fallback", err, chunks, fallbackCalls)
		}
	})
	t.Run("swallowed by the primary decoder", func(t *testing.T) {
		var fallbackCalls int
		err := ExportDecodePCMWithFallback(
			func(handler func(PCMChunk) error) error {
				_ = handler(PCMChunk{SampleRate: 48000, Channels: 2, Samples: rampPCM(0, 16)})
				return errAlignmentPadding
			},
			scriptedPCMDecoder(&fallbackCalls, 20000, 8192, nil),
			func(PCMChunk) error { return handlerErr },
		)
		if err != handlerErr || fallbackCalls != 0 {
			t.Fatalf("error = %v with %d fallback runs; want the handler error and no fallback", err, fallbackCalls)
		}
	})
	t.Run("during the fallback decoder", func(t *testing.T) {
		var primaryCalls, fallbackCalls int
		collector := &pcmCollector{t: t}
		err := ExportDecodePCMWithFallback(
			scriptedPCMDecoder(&primaryCalls, 4096, 4096, errAlignmentPadding),
			scriptedPCMDecoder(&fallbackCalls, 20000, 4096, nil),
			func(chunk PCMChunk) error {
				if collector.chunks == 2 {
					return handlerErr
				}
				return collector.write(chunk)
			},
		)
		if err != handlerErr {
			t.Fatalf("error = %v; want the handler error unchanged", err)
		}
		if !slices.Equal(collector.samples, rampPCM(0, 2*4096)) {
			t.Fatal("handler output before its error is not the start of the stream exactly once")
		}
	})
}

func TestAudioFallbackFailureReportsBothDecoders(t *testing.T) {
	fallbackErr := fmt.Errorf("video2dvd: AAC fallback decode sample 7: %w", io.ErrUnexpectedEOF)
	var primaryCalls, fallbackCalls int
	collector := &pcmCollector{t: t}
	err := ExportDecodePCMWithFallback(
		scriptedPCMDecoder(&primaryCalls, 5000, 4096, errAlignmentPadding),
		scriptedPCMDecoder(&fallbackCalls, 9000, 4096, fallbackErr),
		collector.write,
	)
	if !errors.Is(err, fallbackErr) || !errors.Is(err, errAlignmentPadding) || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, pcm.ErrMalformed) {
		t.Fatalf("error %v does not wrap both decoders' errors", err)
	}
	for _, want := range []string{"AAC fallback decode sample 7", "non-zero alignment padding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if !slices.Equal(collector.samples, rampPCM(0, 9000)) {
		t.Fatal("handler output before the failure is not the start of the stream exactly once")
	}
}

func TestAudioFallbackReturnsCancellationUnwrapped(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		var primaryCalls, fallbackCalls int
		err := ExportDecodePCMWithFallback(
			scriptedPCMDecoder(&primaryCalls, 5000, 4096, errAlignmentPadding),
			scriptedPCMDecoder(&fallbackCalls, 9000, 4096, cause),
			func(PCMChunk) error { return nil },
		)
		if err != cause {
			t.Errorf("error = %v; want %v itself", err, cause)
		}
	}
}

func TestAudioFallbackShorterThanDeliveredOutputFails(t *testing.T) {
	var primaryCalls, fallbackCalls int
	collector := &pcmCollector{t: t}
	err := ExportDecodePCMWithFallback(
		scriptedPCMDecoder(&primaryCalls, 5000, 4096, errAlignmentPadding),
		scriptedPCMDecoder(&fallbackCalls, 4000, 4096, nil),
		collector.write,
	)
	if err == nil || !errors.Is(err, errAlignmentPadding) {
		t.Fatalf("error = %v; want a failure that wraps the primary decoder's error", err)
	}
	if len(collector.samples) != 5000*2 {
		t.Fatalf("handler received %d frames; want only the primary decoder's 5000", len(collector.samples)/2)
	}
}

// testdata/aac_lc_44100_stereo.mp4 is 0.75 s of synthesised tones (no
// third-party material) encoded by FFmpeg's AAC-LC encoder without perceptual
// noise substitution, with the usual 1024-sample priming edit:
//
//	ffmpeg -f lavfi -i "aevalsrc=0.25*sin(2*PI*(300+400*t)*t)+0.15*sin(2*PI*1700*t)*sin(2*PI*5*t)|0.25*sin(2*PI*(1100-300*t)*t)+0.1*sin(2*PI*2600*t):d=0.75:s=44100" \
//	  -c:a aac -aac_pns 0 -b:a 48k -map_metadata -1 -fflags +bitexact -flags:a +bitexact -movflags +faststart aac_lc_44100_stereo.mp4
//
// The primary decoder decodes it completely. Setting a byte-alignment padding
// bit in chosen access units reproduces what some encoders write and the
// primary decoder rejects, without changing the audio.
const aacFixturePath = "testdata/aac_lc_44100_stereo.mp4"

// writeAACWithPaddingBits copies the fixture with a padding bit set in every
// access unit for which pick returns true, and returns the copy's path and the
// number of access units changed.
func writeAACWithPaddingBits(t *testing.T, pick func(index, count int) bool) (string, int) {
	t.Helper()
	data, err := os.ReadFile(aacFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var lastBytes []int64
	if _, err := DemuxMP4(aacFixturePath, func(_ TrackInfo, sample SampleInfo, _ io.Reader) error {
		lastBytes = append(lastBytes, sample.Offset+int64(sample.Size)-1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for index, offset := range lastBytes {
		// An access unit ends with the three-bit END element (all ones) and
		// zero bits up to the byte boundary, so a zero last bit is padding.
		if data[offset]&1 == 0 && pick(index, len(lastBytes)) {
			data[offset] |= 1
			changed++
		}
	}
	path := filepath.Join(t.TempDir(), "padding.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, changed
}

func decodeAllPCM(t *testing.T, decode func(context.Context, string, *int, func(PCMChunk) error) error, path string) ([]int16, error) {
	t.Helper()
	collector := &pcmCollector{t: t}
	err := decode(context.Background(), path, nil, collector.write)
	return collector.samples, err
}

func TestDecodeMP4AudioResumesWithFallbackOnPaddingBits(t *testing.T) {
	primary, err := decodeAllPCM(t, ExportDecodeMP4AudioPrimary, aacFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := decodeAllPCM(t, ExportDecodeMP4AACFallback, aacFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	// 0.75 s at 48 kHz: the edit list removes the encoder priming.
	if len(primary) != 36000*2 || len(fallback) != len(primary) {
		t.Fatalf("decoders produced %d and %d frames; want 36000", len(primary)/2, len(fallback)/2)
	}
	clean, err := decodeAllPCM(t, DecodeMP4Audio, aacFixturePath)
	if err != nil || !slices.Equal(clean, primary) {
		t.Fatalf("DecodeMP4Audio of the clean fixture: error %v, equal to the primary decoder's output: %v", err, slices.Equal(clean, primary))
	}
	for _, test := range []struct {
		name string
		pick func(index, count int) bool
		// wantDelivered reports whether the primary decoder is expected to
		// have delivered audio before it failed.
		wantDelivered bool
	}{
		{"from the first access unit", func(int, int) bool { return true }, false},
		{"from a later access unit", func(index, _ int) bool { return index >= 12 }, true},
		{"in the last access unit only", func(index, count int) bool { return index == count-1 }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spoolDir := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(name, spoolDir)
			}
			path, changed := writeAACWithPaddingBits(t, test.pick)
			if changed == 0 {
				t.Fatal("no access unit with padding bits matched")
			}
			primaryOnly, err := decodeAllPCM(t, ExportDecodeMP4AudioPrimary, path)
			if err == nil || !strings.Contains(err.Error(), "non-zero alignment padding") || !errors.Is(err, pcm.ErrMalformed) {
				t.Fatalf("primary decoder error = %v; want the alignment padding rejection", err)
			}
			if delivered := len(primaryOnly) > 0; delivered != test.wantDelivered {
				t.Fatalf("primary decoder delivered %d frames before failing", len(primaryOnly)/2)
			}
			if len(primaryOnly) >= len(primary) || !slices.Equal(primaryOnly, primary[:len(primaryOnly)]) {
				t.Fatal("primary decoder's partial output is not a strict prefix of its output for the clean fixture")
			}
			// Padding bits carry no audio: the result is the primary decoder's
			// delivered part followed by the fallback decoder's output from
			// exactly that frame on.
			want := slices.Concat(primaryOnly, fallback[len(primaryOnly):])
			got, err := decodeAllPCM(t, DecodeMP4Audio, path)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("decoded %d frames that differ from the %d expected", len(got)/2, len(want)/2)
			}
			// The two decoders agree to the last bit or two on this fixture, so
			// repeated, missing or shifted audio at the switch would show up
			// against the clean decode as a large difference.
			for i := range got {
				if difference := int(got[i]) - int(primary[i]); difference < -2 || difference > 2 {
					t.Fatalf("sample %d differs from the clean decode by %d", i, difference)
				}
			}

			var lpcm bytes.Buffer
			if err := PrepareMP4AudioForDVD(context.Background(), path, nil, &lpcm); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(decodeBigEndianPCM(t, lpcm.Bytes()), want) {
				t.Fatal("PrepareMP4AudioForDVD wrote LPCM that differs from the decoded stream")
			}

			// The Converter's title timeline writer, here with a 100 ms audio
			// delay and a title longer than the audio.
			const delayFrames, titleFrames = 4800, 48000
			var title bytes.Buffer
			window, err := ExportNewPCMWindowWriter(&title, titleFrames, 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if err := DecodeMP4Audio(context.Background(), path, nil, window.WriteChunk); err != nil {
				t.Fatal(err)
			}
			if err := window.Finish(); err != nil {
				t.Fatal(err)
			}
			wantTitle := make([]int16, titleFrames*2)
			copy(wantTitle[delayFrames*2:], want)
			if !slices.Equal(decodeBigEndianPCM(t, title.Bytes()), wantTitle) {
				t.Fatal("title timeline LPCM differs from the delayed decoded stream")
			}

			assertEmptyDir(t, spoolDir)
		})
	}
}

func TestDecodeMP4AudioFallbackCancellationAndHandlerErrors(t *testing.T) {
	path, _ := writeAACWithPaddingBits(t, func(index, _ int) bool { return index >= 12 })
	primaryChunks := 0
	if err := ExportDecodeMP4AudioPrimary(context.Background(), path, nil, func(PCMChunk) error {
		primaryChunks++
		return nil
	}); err == nil || primaryChunks < 2 {
		t.Fatalf("primary decoder delivered %d chunks and returned %v; want several chunks and a failure", primaryChunks, err)
	}
	for _, test := range []struct {
		name   string
		failAt int // handler call that fails or cancels
	}{
		{"in the primary decoder's part", primaryChunks},
		{"in the fallback decoder's part", primaryChunks + 2},
	} {
		useSpoolDir := func(t *testing.T) string {
			dir := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(name, dir)
			}
			return dir
		}
		t.Run("handler error "+test.name, func(t *testing.T) {
			spoolDir := useSpoolDir(t)
			handlerErr := errors.New("sink is full")
			calls := 0
			err := DecodeMP4Audio(context.Background(), path, nil, func(PCMChunk) error {
				calls++
				if calls == test.failAt {
					return handlerErr
				}
				return nil
			})
			if err != handlerErr || calls != test.failAt {
				t.Fatalf("error = %v after %d handler calls; want the handler error at call %d", err, calls, test.failAt)
			}
			assertEmptyDir(t, spoolDir)
		})
		t.Run("cancellation "+test.name, func(t *testing.T) {
			spoolDir := useSpoolDir(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := DecodeMP4Audio(ctx, path, nil, func(PCMChunk) error {
				calls++
				if calls == test.failAt {
					cancel()
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) || calls != test.failAt {
				t.Fatalf("error = %v after %d handler calls; want context.Canceled at call %d", err, calls, test.failAt)
			}
			assertEmptyDir(t, spoolDir)
		})
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d temporary files left behind, first %q", len(entries), entries[0].Name())
	}
}
