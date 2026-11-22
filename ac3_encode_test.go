package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rcarmo/go-264/audio/ac3"

	. "github.com/aethiopicuschan/video2dvd"
)

// ac3TestSignal is stereo PCM in [-1, 1) per channel.
type ac3TestSignal [][2]float64

// ac3TestPCM renders a signal in the DVD LPCM byte layout the encoder takes.
func ac3TestPCM(signal ac3TestSignal) []byte {
	pcm := make([]byte, 0, 4*len(signal))
	for _, sample := range signal {
		for _, value := range sample {
			quantized := int16(min(max(math.Round(value*32768), -32768), 32767))
			pcm = binary.BigEndian.AppendUint16(pcm, uint16(quantized))
		}
	}
	return pcm
}

// ac3TestQuantized returns the signal as the encoder sees it, after the
// 16-bit rounding of ac3TestPCM, so that SNR measures only the codec.
func ac3TestQuantized(signal ac3TestSignal) ac3TestSignal {
	out := make(ac3TestSignal, len(signal))
	for n, sample := range signal {
		for ch, value := range sample {
			out[n][ch] = min(max(math.Round(value*32768), -32768), 32767) / 32768
		}
	}
	return out
}

func ac3TestEncode(t testing.TB, pcm []byte, bitrate int) []byte {
	t.Helper()
	var stream bytes.Buffer
	writer, err := ExportNewAC3Writer(&stream, bitrate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(pcm); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return stream.Bytes()
}

// ac3TestSine is a tone at the given amplitude on both channels.
func ac3TestSine(samples int, frequency, amplitude float64) ac3TestSignal {
	signal := make(ac3TestSignal, samples)
	for n := range signal {
		value := amplitude * math.Sin(2*math.Pi*frequency*float64(n)/48000)
		signal[n] = [2]float64{value, value}
	}
	return signal
}

// ac3TestTones mixes tones from 100 Hz to 15 kHz with different phases on
// the two channels, so the stereo image is wide and rematrixing is mixed.
func ac3TestTones(samples int) ac3TestSignal {
	frequencies := []float64{100, 250, 440, 1000, 2000, 3150, 5000, 8000, 11000, 15000}
	signal := make(ac3TestSignal, samples)
	for n := range signal {
		var left, right float64
		for i, frequency := range frequencies {
			phase := 2 * math.Pi * frequency * float64(n) / 48000
			left += 0.07 * math.Sin(phase)
			right += 0.07 * math.Sin(phase+float64(i))
		}
		signal[n] = [2]float64{left, right}
	}
	return signal
}

// ac3TestMusic is a music-like signal: band-limited noise with a slow
// envelope, a melody of decaying harmonic notes, and a partly correlated
// stereo image.
func ac3TestMusic(samples int) ac3TestSignal {
	random := rand.New(rand.NewPCG(1, 2))
	signal := make(ac3TestSignal, samples)
	var lowLeft, lowRight, highLeft, highRight float64
	notes := []float64{220, 277.18, 329.63, 440, 369.99, 293.66}
	for n := range signal {
		common := random.NormFloat64()
		left := 0.6*common + 0.4*random.NormFloat64()
		right := 0.6*common + 0.4*random.NormFloat64()
		// One-pole low-pass at about 6 kHz minus one at about 150 Hz.
		lowLeft += 0.55 * (left - lowLeft)
		lowRight += 0.55 * (right - lowRight)
		highLeft += 0.02 * (lowLeft - highLeft)
		highRight += 0.02 * (lowRight - highRight)
		envelope := 0.5 + 0.4*math.Sin(2*math.Pi*0.7*float64(n)/48000)
		noiseLeft := 0.08 * envelope * (lowLeft - highLeft)
		noiseRight := 0.08 * envelope * (lowRight - highRight)
		note := notes[(n/12000)%len(notes)]
		age := float64(n%12000) / 48000
		var tone float64
		for harmonic := 1.0; harmonic <= 6; harmonic++ {
			tone += math.Sin(2*math.Pi*note*harmonic*float64(n)/48000) / harmonic
		}
		tone *= 0.25 * math.Exp(-4*age)
		signal[n] = [2]float64{noiseLeft + tone, noiseRight + 0.8*tone}
	}
	return signal
}

// ac3TestCRC is a bitwise A/52 CRC, independent of the encoder's table.
func ac3TestCRC(data []byte) uint16 {
	var crc uint16
	for _, value := range data {
		for bit := 7; bit >= 0; bit-- {
			feedback := crc>>15 ^ uint16(value>>bit&1)
			crc <<= 1
			if feedback != 0 {
				crc ^= 0x8005
			}
		}
	}
	return crc
}

// ac3TestFrames splits a stream into syncframes of the bitrate's size.
func ac3TestFrames(t testing.TB, stream []byte, bitrate int) [][]byte {
	t.Helper()
	size := ExportAC3FrameBytes(bitrate)
	if len(stream)%size != 0 {
		t.Fatalf("stream of %d bytes is not whole %d-byte frames", len(stream), size)
	}
	var frames [][]byte
	for len(stream) > 0 {
		frames = append(frames, stream[:size])
		stream = stream[size:]
	}
	return frames
}

// ac3TestDecodeGo decodes with the pure-Go AC-3 decoder of go-264, which
// verifies both CRCs of every frame.
func ac3TestDecodeGo(t testing.TB, stream []byte, bitrate int) ac3TestSignal {
	t.Helper()
	decoder, err := ac3.NewDecoder(ac3.Config{OutputChannels: 2})
	if err != nil {
		t.Fatal(err)
	}
	var decoded ac3TestSignal
	buffer := make([]float64, 2*ac3.FrameSamples)
	for i, frame := range ac3TestFrames(t, stream, bitrate) {
		samples, info, err := decoder.Decode(context.Background(), frame, buffer)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if info.SampleRate != 48000 || info.BitRateKbps != bitrate/1000 || info.ACMod != 2 || info.BSID != 8 || info.LFE || info.DialNorm != 31 {
			t.Fatalf("frame %d info = %+v", i, info)
		}
		for n := range samples {
			decoded = append(decoded, [2]float64{buffer[2*n], buffer[2*n+1]})
		}
	}
	return decoded
}

// ac3TestDecodeFFmpeg decodes with ffmpeg, failing on any decoder message.
func ac3TestDecodeFFmpeg(t *testing.T, ffmpeg string, stream []byte) ac3TestSignal {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.ac3")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := exec.Command(ffmpeg, "-v", "error", "-f", "ac3", "-i", path, "-f", "f32le", "-c:a", "pcm_f32le", "-")
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("ffmpeg reported: %s", stderr.String())
	}
	raw := stdout.Bytes()
	decoded := make(ac3TestSignal, len(raw)/8)
	for n := range decoded {
		decoded[n][0] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[8*n:])))
		decoded[n][1] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[8*n+4:])))
	}
	return decoded
}

// ac3TestSNR returns the signal-to-noise ratio in dB of decoded against
// reference over both channels, from the first sample: decoded audio is
// the reference delayed by the encoder's silent block.
func ac3TestSNR(reference, decoded ac3TestSignal) float64 {
	delay := ExportAC3EncoderDelay
	var signal, noise float64
	for n := 0; n < len(reference) && n+delay < len(decoded); n++ {
		for ch := range 2 {
			difference := decoded[n+delay][ch] - reference[n][ch]
			signal += reference[n][ch] * reference[n][ch]
			noise += difference * difference
		}
	}
	return 10 * math.Log10(signal/noise)
}

// ac3TestDelay returns the lag in samples at which decoded best matches
// reference by normalized cross-correlation.
func ac3TestDelay(reference, decoded ac3TestSignal, maxLag int) int {
	best, bestLag := math.Inf(-1), 0
	for lag := -maxLag; lag <= maxLag; lag++ {
		var correlation, energy float64
		for n := 4096; n+maxLag < len(reference) && n+maxLag < len(decoded); n++ {
			value := decoded[n+lag][0]
			correlation += reference[n][0] * value
			energy += value * value
		}
		if score := correlation / math.Sqrt(energy); score > best {
			best, bestLag = score, lag
		}
	}
	return bestLag
}

func TestAC3FrameSizesFollowTheBitrate(t *testing.T) {
	// A/52 Table 5.18 at 48 kHz: two bytes per kbit/s per 16-bit word pair.
	want := map[int]int{192000: 768, 224000: 896, 256000: 1024, 320000: 1280, 384000: 1536, 448000: 1792}
	if len(ExportAC3Bitrates) != len(want) {
		t.Fatalf("bitrates = %v", ExportAC3Bitrates)
	}
	for _, bitrate := range ExportAC3Bitrates {
		if got := ExportAC3FrameBytes(bitrate); got != want[bitrate] {
			t.Errorf("frame bytes at %d = %d, want %d", bitrate, got, want[bitrate])
		}
	}
	if ExportAC3FrameBytes(200000) != 0 {
		t.Errorf("an unlisted bitrate has a frame size")
	}
	if _, err := ExportNewAC3Writer(&bytes.Buffer{}, 200000); err == nil {
		t.Errorf("unsupported bitrate accepted")
	}
}

func TestAC3FramesHaveSyncwordHeaderAndValidCRCs(t *testing.T) {
	pcm := ac3TestPCM(ac3TestMusic(48000))
	for _, bitrate := range ExportAC3Bitrates {
		frames := ac3TestFrames(t, ac3TestEncode(t, pcm, bitrate), bitrate)
		words := len(frames[0]) / 2
		fiveEighths := 2 * (words>>1 + words>>3)
		wantCode := map[int]byte{192000: 20, 224000: 22, 256000: 24, 320000: 26, 384000: 28, 448000: 30}[bitrate]
		for i, frame := range frames {
			if frame[0] != 0x0b || frame[1] != 0x77 {
				t.Fatalf("%d: frame %d starts with %x", bitrate, i, frame[:2])
			}
			if fscod, code := frame[4]>>6, frame[4]&0x3f; fscod != 0 || code != wantCode {
				t.Fatalf("%d: frame %d fscod %d frmsizecod %d", bitrate, i, fscod, code)
			}
			// bsid 8, bsmod 0, acmod 2, dsurmod 0, lfeon 0, dialnorm 31,
			// no compression word, language code or production information,
			// copyrightb 0, origbs 1, no timecodes and no additional BSI.
			if got := mpeg2TestBits(frame[5:9])[:27]; got != "010000000100001111100001000" {
				t.Fatalf("%d: frame %d BSI %s", bitrate, i, got)
			}
			// Audio block 0: long blocks, dither on, no dynamic range word,
			// coupling strategy sent with coupling off, rematrixing sent;
			// then four rematrixing flags, two exponent strategies that must
			// not be reuse, and two bandwidth codes.
			block := mpeg2TestBits(frame[8:13])[3:]
			if block[:8] != "00110101" {
				t.Fatalf("%d: frame %d block 0 starts %s", bitrate, i, block[:8])
			}
			if block[12:14] == "00" || block[14:16] == "00" {
				t.Fatalf("%d: frame %d block 0 reuses exponents", bitrate, i)
			}
			wantBandwidth := map[bool]string{false: "101110", true: "110010"}[bitrate >= 320000]
			if block[16:22] != wantBandwidth || block[22:28] != wantBandwidth {
				t.Fatalf("%d: frame %d bandwidth codes %s %s", bitrate, i, block[16:22], block[22:28])
			}
			if crc := ac3TestCRC(frame[2:fiveEighths]); crc != 0 {
				t.Fatalf("%d: frame %d crc1 residue %04x", bitrate, i, crc)
			}
			if crc := ac3TestCRC(frame[2:]); crc != 0 {
				t.Fatalf("%d: frame %d crc2 residue %04x", bitrate, i, crc)
			}
		}
	}
}

func TestAC3FrameCountCoversTheInput(t *testing.T) {
	const frame = ExportAC3SamplesPerFrame
	if frame != 1536 || ExportAC3EncoderDelay != 256 {
		t.Fatalf("samples per frame %d, delay %d", frame, ExportAC3EncoderDelay)
	}
	// The writer codes the input behind its 256 silent samples.
	cases := map[int64]int64{0: 1, 1: 1, frame - 256: 1, frame - 255: 2, frame: 2, frame + 256: 2, 2 * frame: 3, 48000: 32, 48001: 32}
	for samples, want := range cases {
		if got := ExportAC3OutputFrames(samples + ExportAC3EncoderDelay); got != want {
			t.Errorf("ac3OutputFrames(%d+%d) = %d, want %d", samples, ExportAC3EncoderDelay, got, want)
		}
		stream := ac3TestEncode(t, make([]byte, 4*samples), 192000)
		if got := int64(len(stream) / ExportAC3FrameBytes(192000)); got != want || len(stream)%ExportAC3FrameBytes(192000) != 0 {
			t.Errorf("%d samples gave %d bytes, want %d frames", samples, len(stream), want)
		}
	}
}

func TestAC3WriteSplitsAndRepeatsGiveIdenticalOutput(t *testing.T) {
	pcm := ac3TestPCM(ac3TestMusic(20000))
	whole := ac3TestEncode(t, pcm, 256000)
	if again := ac3TestEncode(t, pcm, 256000); !bytes.Equal(again, whole) {
		t.Fatal("encoding the same input twice differs")
	}
	random := rand.New(rand.NewPCG(3, 4))
	for _, maxChunk := range []int{1, 3, 7, 4099} {
		var stream bytes.Buffer
		writer, err := ExportNewAC3Writer(&stream, 256000)
		if err != nil {
			t.Fatal(err)
		}
		for rest := pcm; len(rest) > 0; {
			size := min(1+random.IntN(maxChunk), len(rest))
			if n, err := writer.Write(rest[:size]); err != nil || n != size {
				t.Fatalf("Write(%d) = %d, %v", size, n, err)
			}
			rest = rest[size:]
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stream.Bytes(), whole) {
			t.Fatalf("chunks of up to %d bytes changed the output", maxChunk)
		}
	}
}

func TestAC3WriterRejectsMisuse(t *testing.T) {
	writer, err := ExportNewAC3Writer(&bytes.Buffer{}, 192000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err == nil {
		t.Error("a trailing partial sample was accepted")
	}
	if _, err := writer.Write([]byte{0, 0, 0, 0}); err == nil {
		t.Error("Write after Close succeeded")
	}
	if err := writer.Close(); err == nil {
		t.Error("second Close succeeded")
	}
	if _, err := ExportNewAC3Writer(nil, 192000); err == nil {
		t.Error("nil destination accepted")
	}
}

func TestAC3MDCTMatchesTheA52Definition(t *testing.T) {
	random := rand.New(rand.NewPCG(5, 6))
	input := make([]float64, 512)
	for n := range input {
		input[n] = 2*random.Float64() - 1
	}
	fast, window := ExportAC3MDCT(input)
	for n := range 256 {
		// Princen-Bradley: the overlapped window halves preserve power.
		if sum := window[n]*window[n] + window[n+256]*window[n+256]; math.Abs(sum-1) > 1e-12 || window[n] != window[511-n] {
			t.Fatalf("window is not a symmetric Princen-Bradley window at %d", n)
		}
	}
	for k := range 256 {
		var sum float64
		for n := range 512 {
			sum += input[n] * window[n] * math.Cos(2*math.Pi/(4*512)*float64(2*n+1)*float64(2*k+1)+math.Pi/4*float64(2*k+1))
		}
		if want := -2.0 / 512 * sum; math.Abs(fast[k]-want) > 1e-12 {
			t.Fatalf("coefficient %d = %g, want %g", k, fast[k], want)
		}
	}
}

func TestAC3LogAddTableMatchesItsDefinition(t *testing.T) {
	// latab adds powers given as 128 steps per factor of two in amplitude.
	// The standard rounds its entries its own way, so a transcription error
	// shows as a step away from the curve or a table that rises.
	for address, value := range ExportAC3LogAdd {
		ideal := 64 * math.Log2(1+math.Exp2(-float64(address)/32))
		if math.Abs(float64(value)-ideal) > 1 || address > 0 && value > ExportAC3LogAdd[address-1] {
			t.Fatalf("latab[%d] = %#x, curve %.2f", address, value, ideal)
		}
	}
}

// ac3TestCase is one signal of the round-trip quality tests, with the
// minimum SNR in dB required at 192 and 448 kbit/s.
type ac3TestCase struct {
	name   string
	signal ac3TestSignal
	min192 float64
	min448 float64
}

func ac3TestCases() []ac3TestCase {
	const samples = 3 * 48000
	return []ac3TestCase{
		// Measured: 85/89, 40/66 and 21.6/27.3 dB, on par with ffmpeg's own
		// AC-3 encoder on the music-like signal (22.1/27.1 dB). A perceptual
		// coder puts noise under the masking curve rather than minimizing
		// it, so broadband signals score far lower than tones.
		{"sine 1 kHz -6 dBFS", ac3TestSine(samples, 1000, 0.5), 75, 80},
		{"tones 100 Hz-15 kHz", ac3TestTones(samples), 35, 60},
		{"music-like", ac3TestMusic(samples), 19, 25},
	}
}

func ac3TestRoundTrip(t *testing.T, decode func(t *testing.T, stream []byte, bitrate int) ac3TestSignal) {
	for _, test := range ac3TestCases() {
		reference := ac3TestQuantized(test.signal)
		pcm := ac3TestPCM(test.signal)
		for _, bitrate := range []int{192000, 448000} {
			decoded := decode(t, ac3TestEncode(t, pcm, bitrate), bitrate)
			if want := int(ExportAC3OutputFrames(int64(len(reference)+ExportAC3EncoderDelay))) * ExportAC3SamplesPerFrame; len(decoded) != want {
				t.Fatalf("%s at %d: decoded %d samples, want %d", test.name, bitrate, len(decoded), want)
			}
			snr := ac3TestSNR(reference, decoded)
			minimum := map[int]float64{192000: test.min192, 448000: test.min448}[bitrate]
			t.Logf("%s at %d kbit/s: SNR %.1f dB", test.name, bitrate/1000, snr)
			if snr < minimum {
				t.Errorf("%s at %d kbit/s: SNR %.1f dB, want at least %.0f", test.name, bitrate/1000, snr, minimum)
			}
			if test.name == "music-like" {
				if delay := ac3TestDelay(reference, decoded, 600); delay != ExportAC3EncoderDelay {
					t.Errorf("%s at %d kbit/s: decoded audio is %d samples late", test.name, bitrate/1000, delay)
				}
			}
		}
	}
	for _, bitrate := range []int{192000, 448000} {
		decoded := decode(t, ac3TestEncode(t, make([]byte, 4*48000), bitrate), bitrate)
		peak := 0.0
		for _, sample := range decoded {
			peak = max(peak, math.Abs(sample[0]), math.Abs(sample[1]))
		}
		t.Logf("silence at %d kbit/s: peak %.2g", bitrate/1000, peak)
		if peak > 1.0/65536 {
			t.Errorf("silence at %d kbit/s decodes with peak %g", bitrate/1000, peak)
		}
	}
}

func TestAC3RoundTripThroughPureGoDecoder(t *testing.T) {
	if testing.Short() {
		t.Skip("round trip is slow in -short mode")
	}
	ac3TestRoundTrip(t, func(t *testing.T, stream []byte, bitrate int) ac3TestSignal {
		return ac3TestDecodeGo(t, stream, bitrate)
	})
}

func TestAC3RoundTripThroughFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if testing.Short() {
		t.Skip("round trip is slow in -short mode")
	}
	ac3TestRoundTrip(t, func(t *testing.T, stream []byte, _ int) ac3TestSignal {
		return ac3TestDecodeFFmpeg(t, ffmpeg, stream)
	})
}

func TestAC3StreamParametersSeenByFFprobe(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	path := filepath.Join(t.TempDir(), "stream.ac3")
	if err := os.WriteFile(path, ac3TestEncode(t, ac3TestPCM(ac3TestMusic(48000)), 384000), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_name,sample_rate,channels,channel_layout,bit_rate", "-of", "default=noprint_wrappers=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v: %s", err, output)
	}
	want := "codec_name=ac3\nsample_rate=48000\nchannels=2\nchannel_layout=stereo\nbit_rate=384000\n"
	if string(output) != want {
		t.Fatalf("ffprobe reported\n%s\nwant\n%s", output, want)
	}
}

func TestAC3SurvivesFullScaleNoise(t *testing.T) {
	// The worst case for the bit budget and for mantissa range: every block
	// sends new exponents and transform coefficients approach full scale.
	random := rand.New(rand.NewPCG(7, 8))
	signal := make(ac3TestSignal, 48000)
	for n := range signal {
		signal[n] = [2]float64{2*random.Float64() - 1, float64(int(n/40)%2*2 - 1)}
	}
	ac3TestDecodeGo(t, ac3TestEncode(t, ac3TestPCM(signal), 192000), 192000)
}

func BenchmarkAC3Encode(b *testing.B) {
	pcm := ac3TestPCM(ac3TestMusic(10 * 48000))
	b.SetBytes(int64(len(pcm)))
	for b.Loop() {
		ac3TestEncode(b, pcm, 448000)
	}
}
