package video2dvd_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

// downmixSource is a multichannel test signal: an FFmpeg channel layout and
// a whole-hertz tone for every channel in FFmpeg's order, so one second of
// output shows how much of each channel reached Lo and Ro.
type downmixSource struct {
	layout      string
	names       []string
	frequencies []float64
}

var (
	downmixSource51 = downmixSource{"5.1", []string{"FL", "FR", "FC", "LFE", "BL", "BR"}, []float64{440, 620, 830, 70, 1050, 1290}}
	downmixSource71 = downmixSource{"7.1", []string{"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"}, []float64{440, 620, 830, 70, 1050, 1290, 1530, 1730}}
)

// gains returns the Lo/Ro coefficient of every channel before normalisation
// for a downmix with the given centre and surround coefficients.
func (s downmixSource) gains(centre, surround float64) [][2]float64 {
	gains := make([][2]float64, len(s.names))
	for i, name := range s.names {
		switch name {
		case "FL":
			gains[i] = [2]float64{1, 0}
		case "FR":
			gains[i] = [2]float64{0, 1}
		case "FC":
			gains[i] = [2]float64{centre, centre}
		case "BL", "SL":
			gains[i] = [2]float64{surround, 0}
		case "BR", "SR":
			gains[i] = [2]float64{0, surround}
		}
	}
	return gains
}

// downmixToneAmplitude is the level of every source tone.
const downmixToneAmplitude = 0.3

// makeDownmixInput writes a two-second file with ffmpeg. tones selects the
// per-channel tones; otherwise every channel carries the same in-phase 440 Hz
// tone at level. The test skips when ffmpeg or the encoder is missing.
func makeDownmixInput(t *testing.T, name string, source downmixSource, tones bool, level float64, extra ...string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	expressions := make([]string, len(source.frequencies))
	for i, frequency := range source.frequencies {
		if !tones {
			frequency = 440
		}
		expressions[i] = fmt.Sprintf("%g*sin(2*PI*%g*t)", level, frequency)
	}
	path := filepath.Join(t.TempDir(), name)
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
		"aevalsrc=" + strings.Join(expressions, "|") + ":s=48000:c=" + source.layout + ":d=2"}
	args = append(args, extra...)
	args = append(args, path)
	if output, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot write the %s input: %v: %s", source.layout, err, output)
	}
	return path
}

// decodeDownmixStereo collects every chunk of a decode into one interleaved
// stereo buffer.
func decodeDownmixStereo(t *testing.T, path string, webM bool) []int16 {
	t.Helper()
	var samples []int16
	handler := func(chunk PCMChunk) error {
		if chunk.SampleRate != 48000 || chunk.Channels != 2 {
			return fmt.Errorf("chunk is %d Hz %d channels", chunk.SampleRate, chunk.Channels)
		}
		samples = append(samples, chunk.Samples...)
		return nil
	}
	var err error
	if webM {
		err = DecodeWebMAudio(context.Background(), path, nil, handler)
	} else {
		err = DecodeMP4Audio(context.Background(), path, nil, handler)
	}
	if err != nil {
		t.Fatal(err)
	}
	return samples
}

// toneLevel is the amplitude of the frequency component of one channel of
// interleaved stereo over the second from 0.5 s to 1.5 s, in full-scale units.
// Every tone completes whole cycles in that window, so the single-bin DFT
// separates them exactly.
func toneLevel(t *testing.T, samples []int16, side int, frequency float64) float64 {
	t.Helper()
	const first, count = 24000, 48000
	if len(samples) < (first+count)*2 {
		t.Fatalf("decoded %d frames, want at least %d", len(samples)/2, first+count)
	}
	var re, im float64
	for i := range count {
		value := float64(samples[(first+i)*2+side]) / 32768
		phase := 2 * math.Pi * frequency * float64(i) / 48000
		re += value * math.Cos(phase)
		im += value * math.Sin(phase)
	}
	return 2 * math.Hypot(re, im) / count
}

// checkDownmixTones verifies that each tone reaches Lo and Ro with the gains
// of a downmix whose centre and surround coefficients are centre and
// surround, normalised by the coefficient sum of one output: front channels
// stay on their side and LFE is absent. Gains match within 0.5 dB; an absent
// tone is at least 40 dB below the front tone.
func checkDownmixTones(t *testing.T, samples []int16, source downmixSource, centre, surround float64) {
	t.Helper()
	gains := source.gains(centre, surround)
	sum := 0.0
	for _, gain := range gains {
		sum += gain[0]
	}
	reference := downmixToneAmplitude / sum
	for channel, frequency := range source.frequencies {
		name := source.names[channel]
		for side, sideName := range []string{"Lo", "Ro"} {
			got := toneLevel(t, samples, side, frequency)
			want := reference * gains[channel][side]
			if want == 0 {
				t.Logf("%s in %s: %.6f (%.1f dB below the front tone)", name, sideName, got, 20*math.Log10(reference/got))
				if got > reference/100 {
					t.Errorf("%s in %s at %.5f, want below %.5f (-40 dB)", name, sideName, got, reference/100)
				}
				continue
			}
			diff := 20 * math.Log10(got/want)
			t.Logf("%s in %s: %.5f, want %.5f (%+.3f dB)", name, sideName, got, want, diff)
			if math.Abs(diff) > 0.5 {
				t.Errorf("%s in %s at %.5f, want %.5f (%+.2f dB)", name, sideName, got, want, diff)
			}
		}
	}
}

// checkDownmixHeadroom verifies that loud in-phase input on every channel
// neither saturates nor loses more than 1 dB: without normalisation Lo would
// be 2.4 (5.1) or 3.1 (7.1) times the source level.
func checkDownmixHeadroom(t *testing.T, samples []int16, level float64) {
	t.Helper()
	peak := 0
	for _, sample := range samples {
		peak = max(peak, int(math.Abs(float64(sample))))
	}
	if peak >= 32767 {
		t.Fatalf("downmix peak %d reaches full scale", peak)
	}
	if want := level * 32768; float64(peak) < want*0.89 || float64(peak) > want*1.06 {
		t.Fatalf("downmix peak %d, want about %.0f", peak, want)
	}
	t.Logf("peak %d for a %.2f full-scale source", peak, level)
}

// 5.1 Opus with channel mapping family 1 is decoded with the multistream
// decoder and downmixed with the BS.775 coefficients.
func TestDecodeWebMAudioDownmixes51Opus(t *testing.T) {
	opus := []string{"-c:a", "libopus", "-mapping_family", "1", "-b:a", "512k"}
	samples := decodeDownmixStereo(t, makeDownmixInput(t, "tones.webm", downmixSource51, true, downmixToneAmplitude, opus...), true)
	if frames := len(samples) / 2; frames < 95_000 || frames > 97_000 {
		t.Fatalf("decoded %d frames, want about two seconds", frames)
	}
	checkDownmixTones(t, samples, downmixSource51, bs775, bs775)
	checkDownmixHeadroom(t, decodeDownmixStereo(t, makeDownmixInput(t, "loud.webm", downmixSource51, false, 0.9, opus...), true), 0.9)
}

// 7.1 Opus (eight channels in Vorbis order L C R Ls Rs Lb Rb LFE) adds both
// surround pairs on their side at 0.7071.
func TestDecodeWebMAudioDownmixes71Opus(t *testing.T) {
	opus := []string{"-c:a", "libopus", "-mapping_family", "1", "-b:a", "640k"}
	checkDownmixTones(t, decodeDownmixStereo(t, makeDownmixInput(t, "tones.webm", downmixSource71, true, downmixToneAmplitude, opus...), true), downmixSource71, bs775, bs775)
	checkDownmixHeadroom(t, decodeDownmixStereo(t, makeDownmixInput(t, "loud.webm", downmixSource71, false, 0.9, opus...), true), 0.9)
}

// 5.1 AC-3 is downmixed by the go-264 decoder with the mix levels the
// stream carries: at -3 dB that is the BS.775 downmix, and FFmpeg's default
// levels (centre -4.5 dB, surround -6 dB) are honoured as coded.
func TestDecodeMP4AudioDownmixes51AC3(t *testing.T) {
	ac3 := []string{"-c:a", "ac3", "-b:a", "640k"}
	minus3dB := append([]string{"-center_mixlev", "0.707", "-surround_mixlev", "0.707"}, ac3...)
	checkDownmixTones(t, decodeDownmixStereo(t, makeDownmixInput(t, "tones.mp4", downmixSource51, true, downmixToneAmplitude, minus3dB...), false), downmixSource51, bs775, bs775)
	checkDownmixTones(t, decodeDownmixStereo(t, makeDownmixInput(t, "default.mp4", downmixSource51, true, downmixToneAmplitude, ac3...), false), downmixSource51, math.Pow(2, -0.75), 0.5)
	checkDownmixHeadroom(t, decodeDownmixStereo(t, makeDownmixInput(t, "loud.mp4", downmixSource51, false, 0.9, minus3dB...), false), 0.9)
}

// The other AC-3 channel modes with front and side channels, with and
// without LFE, fold with the same coefficients at -3 dB mix levels; layouts
// without centre or surrounds are normalised by their own coefficient sum.
func TestDecodeMP4AudioDownmixesAC3Layouts(t *testing.T) {
	sources := []downmixSource{
		{"2.1", []string{"FL", "FR", "LFE"}, []float64{440, 620, 70}},
		{"3.0", []string{"FL", "FR", "FC"}, []float64{440, 620, 830}},
		{"quad", []string{"FL", "FR", "BL", "BR"}, []float64{440, 620, 1050, 1290}},
		{"5.0", []string{"FL", "FR", "FC", "BL", "BR"}, []float64{440, 620, 830, 1050, 1290}},
	}
	for _, source := range sources {
		args := []string{"-center_mixlev", "0.707", "-surround_mixlev", "0.707", "-c:a", "ac3", "-b:a", "640k"}
		path := makeDownmixInput(t, "tones-"+source.layout+".mp4", source, true, downmixToneAmplitude, args...)
		checkDownmixTones(t, decodeDownmixStereo(t, path, false), source, bs775, bs775)
	}
}

// AAC-LC with channel configurations 3 to 7 is downmixed by the go-264
// decoder with the BS.775 coefficients; 5.1(side), which ffmpeg describes
// with a program config element, is refused before encoding.
func TestDecodeMP4AudioDownmixesMultichannelAAC(t *testing.T) {
	aac := []string{"-c:a", "aac", "-b:a", "512k"}
	sources := []downmixSource{
		downmixSource51,
		{"5.0", []string{"FL", "FR", "FC", "BL", "BR"}, []float64{440, 620, 830, 1050, 1290}},
		{"3.0", []string{"FL", "FR", "FC"}, []float64{440, 620, 830}},
	}
	for _, source := range sources {
		path := makeDownmixInput(t, "tones-"+source.layout+".mp4", source, true, downmixToneAmplitude, aac...)
		checkDownmixTones(t, decodeDownmixStereo(t, path, false), source, bs775, bs775)
	}
	// AAC's own coding error lets the loud in-phase source overshoot its
	// level by a few percent, so this only checks that nothing clips.
	peak := 0
	for _, sample := range decodeDownmixStereo(t, makeDownmixInput(t, "loud.mp4", downmixSource51, false, 0.9, aac...), false) {
		peak = max(peak, int(math.Abs(float64(sample))))
	}
	if peak >= 32767 || float64(peak) < 0.85*0.9*32768 {
		t.Fatalf("downmix peak %d, want about %.0f without clipping", peak, 0.9*32768)
	}
	side := downmixSource{"5.1(side)", []string{"FL", "FR", "FC", "LFE", "SL", "SR"}, []float64{440, 620, 830, 70, 1050, 1290}}
	info, err := ProbeMedia(makeDownmixInput(t, "side.mp4", side, true, downmixToneAmplitude, aac...))
	if err != nil {
		t.Fatal(err)
	}
	if err := ExportValidateInputAudioChannels(info.Tracks[0]); err == nil || !strings.Contains(err.Error(), "program config element") {
		t.Fatalf("5.1(side) AAC error %v, want the program config element rejection", err)
	}
}

// The encoder turns a WebM with 5.1 Opus and an MP4 with 5.1 AAC into DVD
// LPCM carrying the same downmix.
func TestEncodeStreamsDownmixes51Opus(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "video.webm")
	command := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x48:rate=25:duration=2,format=yuv420p",
		"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", video)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot write the VP9 input: %v: %s", err, output)
	}
	audio := makeDownmixInput(t, "audio.webm", downmixSource51, true, downmixToneAmplitude, "-c:a", "libopus", "-mapping_family", "1", "-b:a", "512k")
	profile, err := ExportProfile(Options{Standard: VideoStandardPAL})
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(dir, "work")
	title, err := NewInProcessEncoder().EncodeStreams(context.Background(), MediaInput{VideoPath: video, AudioPath: audio}, workDir, profile)
	if err != nil {
		t.Fatal(err)
	}
	audioPath := title.AudioPath
	if !filepath.IsAbs(audioPath) {
		audioPath = filepath.Join(workDir, audioPath)
	}
	data, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.BigEndian.Uint16(data[i*2:]))
	}
	checkDownmixTones(t, samples, downmixSource51, bs775, bs775)

	aac := makeDownmixInput(t, "audio.mp4", downmixSource51, true, downmixToneAmplitude, "-c:a", "aac", "-b:a", "384k")
	workAAC := filepath.Join(dir, "work-aac")
	title, err = NewInProcessEncoder().EncodeStreams(context.Background(), MediaInput{VideoPath: video, AudioPath: aac}, workAAC, profile)
	if err != nil {
		t.Fatalf("5.1 AAC: %v", err)
	}
	if data, err = os.ReadFile(filepath.Join(workAAC, title.AudioPath)); err != nil {
		t.Fatal(err)
	}
	samples = make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.BigEndian.Uint16(data[i*2:]))
	}
	checkDownmixTones(t, samples, downmixSource51, bs775, bs775)
}
