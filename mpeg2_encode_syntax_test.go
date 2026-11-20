package video2dvd_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// mpeg2TestBits renders bytes as a string of binary digits.
func mpeg2TestBits(data []byte) string {
	var text strings.Builder
	for _, value := range data {
		for bit := 7; bit >= 0; bit-- {
			text.WriteByte('0' + value>>bit&1)
		}
	}
	return text.String()
}

func TestMPEG2DefaultIntraMatrixIsTheStandardOne(t *testing.T) {
	// H.262 6.3.11. The stream never loads a matrix, so a decoder
	// dequantizes with exactly these weights.
	want := [64]int{
		8, 16, 19, 22, 26, 27, 29, 34,
		16, 16, 22, 24, 27, 29, 34, 37,
		19, 22, 26, 27, 29, 34, 34, 38,
		22, 22, 26, 27, 29, 34, 37, 40,
		22, 26, 27, 29, 32, 35, 40, 48,
		26, 27, 29, 32, 35, 40, 48, 58,
		26, 27, 29, 34, 38, 46, 56, 69,
		27, 29, 35, 38, 46, 56, 69, 83,
	}
	if ExportMPEG2DefaultIntraMatrix != want {
		t.Fatalf("default intra matrix = %v, want %v", ExportMPEG2DefaultIntraMatrix, want)
	}
}

func TestMPEG2MotionVectorCodesDecodeToTheSameVector(t *testing.T) {
	// H.262 Table B-10, by magnitude of motion_code; a sign bit follows
	// every code but the first.
	motionCodes := []string{
		"1", "01", "001", "0001", "000011", "0000101", "0000100", "0000011",
		"000001011", "000001010", "000001001", "0000010001", "0000010000",
		"0000001111", "0000001110", "0000001101", "0000001100",
	}
	const rSize = ExportMPEG2MotionRSize
	const low, high = ExportMPEG2MotionLow, ExportMPEG2MotionHigh
	if low != -16<<rSize || high != 16<<rSize-1 {
		t.Fatalf("vector range %d..%d does not match r_size %d", low, high, rSize)
	}
	// decode follows H.262 7.6.3.1.
	decode := func(code string, predictor int) (vector int, rest string) {
		magnitude := -1
		for candidate, prefix := range motionCodes {
			if strings.HasPrefix(code, prefix) {
				magnitude, code = candidate, code[len(prefix):]
				break
			}
		}
		if magnitude < 0 {
			t.Fatalf("no motion_code at the start of %q", code)
		}
		delta := 0
		if magnitude != 0 {
			negative := code[0] == '1'
			residual := 0
			for _, bit := range code[1 : 1+rSize] {
				residual = residual<<1 | int(bit-'0')
			}
			code = code[1+rSize:]
			delta = (magnitude-1)<<rSize + residual + 1
			if negative {
				delta = -delta
			}
		}
		vector = predictor + delta
		if vector < low {
			vector += high - low + 1
		}
		if vector > high {
			vector -= high - low + 1
		}
		return vector, code
	}
	for predictor := low; predictor <= high; predictor++ {
		for motion := low; motion <= high; motion++ {
			code := mpeg2TestBits(ExportMPEG2MotionVectorCode(motion, predictor))
			vector, rest := decode(code, predictor)
			if vector != motion {
				t.Fatalf("vector %d coded against predictor %d as %q decodes to %d", motion, predictor, code, vector)
			}
			if strings.TrimRight(rest, "0") != "1" {
				t.Fatalf("vector %d coded against predictor %d as %q leaves %q undecoded", motion, predictor, code, rest)
			}
		}
	}
}

func TestMPEG2MacroblockAddressIncrementUsesEscapes(t *testing.T) {
	// H.262 Table B-1: increments above 33 are a macroblock_escape per 33
	// macroblocks and then the code of the remainder.
	const escape = "00000001000"
	for increment, want := range map[int]string{
		1:  "1",
		2:  "011",
		33: "00000011000",
		34: escape + "1",
		44: escape + "00001010",
		66: escape + "00000011000",
		67: escape + escape + "1",
	} {
		got := strings.TrimRight(mpeg2TestBits(ExportMPEG2MacroblockAddressIncrement(increment)), "0")
		if got != want+"1" {
			t.Errorf("address increment %d coded as %q, want %q", increment, strings.TrimSuffix(got, "1"), want)
		}
	}
}

func TestMPEG2MotionBoundsKeepPredictionsInsideThePicture(t *testing.T) {
	// H.262 7.6.3.8: a vector must not reference samples outside the coded
	// picture. inside checks one dimension of a prediction block the way a
	// decoder forms it, including the extra sample of a half-sample vector.
	inside := func(position, blockSize, size, vector int) bool {
		first := position + vector>>1
		last := first + blockSize - 1 + vector&1
		return first >= 0 && last < size
	}
	for _, size := range []int{720, 480, 576, 64, 32} {
		for position := 0; position+16 <= size; position += 16 {
			low, high := ExportMPEG2MotionBounds(position, size)
			if low > 0 || high < 0 {
				t.Fatalf("position %d of %d: bounds %d..%d exclude the zero vector", position, size, low, high)
			}
			for vector := ExportMPEG2MotionLow; vector <= ExportMPEG2MotionHigh; vector++ {
				// The chroma vector of 4:2:0 is the luma vector halved
				// towards zero.
				fits := inside(position, 16, size, vector) && inside(position/2, 8, size/2, vector/2)
				if allowed := vector >= low && vector <= high; allowed != fits {
					t.Fatalf("position %d of %d, vector %d: allowed %v but prediction inside the picture %v", position, size, vector, allowed, fits)
				}
			}
		}
	}
}

func TestMPEG2SequenceDisplayExtensionDescribesTheVideoSystem(t *testing.T) {
	for _, tc := range []struct {
		standard    VideoStandard
		videoFormat uint32
		colour      uint32
		height      uint32
		colourSpace string
	}{
		{VideoStandardNTSC, 2, 6, 480, "smpte170m"},
		{VideoStandardPAL, 1, 5, 576, "bt470bg"},
	} {
		profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: AspectRatio16x9})
		if err != nil {
			t.Fatal(err)
		}
		header := ExportMPEG2SequenceHeader(profile)
		// sequence_header (12 bytes without matrices), sequence_extension
		// (10 bytes), then the display extension as the last 12 bytes.
		if len(header) != 34 || !bytes.Equal(header[12:16], []byte{0, 0, 1, 0xb5}) || !bytes.Equal(header[22:26], []byte{0, 0, 1, 0xb5}) {
			t.Fatalf("standard %v: unexpected sequence header layout % x", tc.standard, header)
		}
		reader := ExportNewBitReader(header[26:])
		for _, field := range []struct {
			name string
			bits int
			want uint32
		}{
			{"extension_start_code_identifier", 4, 2},
			{"video_format", 3, tc.videoFormat},
			{"colour_description", 1, 1},
			{"colour_primaries", 8, tc.colour},
			{"transfer_characteristics", 8, tc.colour},
			{"matrix_coefficients", 8, tc.colour},
			{"display_horizontal_size", 14, 720},
			{"marker_bit", 1, 1},
			{"display_vertical_size", 14, tc.height},
			{"stuffing", 3, 0},
		} {
			got, err := ExportReadBits(reader, field.bits)
			if err != nil {
				t.Fatal(err)
			}
			if got != field.want {
				t.Errorf("standard %v: %s = %d, want %d", tc.standard, field.name, got, field.want)
			}
		}
		if bytes.Contains(header[26:], []byte{0, 0, 1}) || bytes.Contains(header[16:22], []byte{0, 0, 1}) {
			t.Errorf("standard %v: extension payload emulates a start code: % x", tc.standard, header)
		}

		ffprobe, err := exec.LookPath("ffprobe")
		if err != nil {
			continue
		}
		var stream bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&stream, profile)
		if err != nil {
			t.Fatal(err)
		}
		for index := range 2 {
			if err := encoder.WriteFrame(movingMPEG2TestFrame(profile, index)); err != nil {
				t.Fatal(err)
			}
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "video.m2v")
		if err := os.WriteFile(path, stream.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=color_space,color_primaries,color_transfer", "-of", "default=nw=1:nk=1", path).Output()
		cancel()
		if err != nil {
			t.Fatalf("ffprobe: %v", err)
		}
		if fields := strings.Fields(string(output)); len(fields) != 3 || fields[0] != tc.colourSpace {
			t.Errorf("standard %v: ffprobe reports colour description %q, want colour space %s", tc.standard, fields, tc.colourSpace)
		}
	}
}

// sceneChangeMPEG2TestFrames returns a sequence with a scene change early in
// the first GOP, a window of unrelated content appearing inside the second
// scene, a scene change once the GOP may end, and a nearly static last scene
// that runs past the end of the GOP the second change started.
func sceneChangeMPEG2TestFrames(profile Profile, minGOP int) (frames []DecodedVideoFrame, earlyCut, window, lateCut int) {
	earlyCut, window, lateCut = 5, 8, minGOP+1
	for index := range lateCut + ExportMPEG2GOPPictures + 2 {
		drift := 0.5 * float64(index)
		var frame DecodedVideoFrame
		switch {
		case index < earlyCut:
			frame = flatShadedMPEG2TestFrame(profile, 0, drift, 0)
		case index < lateCut:
			frame = flatShadedMPEG2TestFrame(profile, 1, 0, drift)
			if index >= window {
				// Fresh content every picture, off the macroblock grid
				// and at levels of its own.
				patch := flatShadedMPEG2TestFrame(profile, 2, 37*float64(index), 0)
				for y := 100; y < 260; y++ {
					for x := 200; x < 424; x++ {
						frame.Y[y*frame.Width+x] = patch.Y[y*frame.Width+x] + byte(index%4*9)
					}
				}
			}
		default:
			// A flat picture with a small moving object: most macroblocks
			// are skipped, in runs longer than one macroblock_escape
			// covers.
			frame = newMPEG2TestFrame(profile)
			for i := range frame.Y {
				frame.Y[i] = 90
			}
			for i := range frame.U {
				frame.U[i], frame.V[i] = 128, 128
			}
			sprite := texturedMPEG2TestFrame(profile, 2, 0, 0)
			left, top := 40+7*(index-lateCut), 150+3*(index-lateCut)
			for y := range 40 {
				copy(frame.Y[(top+y)*frame.Width+left:(top+y)*frame.Width+left+56], sprite.Y[y*frame.Width:y*frame.Width+56])
			}
		}
		frames = append(frames, frame)
	}
	return frames, earlyCut, window, lateCut
}

func TestMPEG2SceneChangeStartsGOPOnlyWhereAVOBUMayEnd(t *testing.T) {
	for _, tc := range []struct {
		standard   VideoStandard
		frameTicks int
	}{
		{VideoStandardNTSC, 3003},
		{VideoStandardPAL, 3600},
	} {
		profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: AspectRatio16x9})
		if err != nil {
			t.Fatal(err)
		}
		var stream bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&stream, profile)
		if err != nil {
			t.Fatal(err)
		}
		minGOP := ExportMPEG2MinGOPPictures(encoder)
		// A DVD VOBU, which the multiplexer makes of each GOP, lasts 0.4 s
		// to 1 s.
		if minGOP*tc.frameTicks < 36_000 || (minGOP-1)*tc.frameTicks >= 36_000 || ExportMPEG2GOPPictures*tc.frameTicks > 90_000 {
			t.Fatalf("standard %v: GOPs of %d to %d pictures do not fit the VOBU duration limits", tc.standard, minGOP, ExportMPEG2GOPPictures)
		}
		frames, earlyCut, _, lateCut := sceneChangeMPEG2TestFrames(profile, minGOP)
		for _, frame := range frames {
			if err := encoder.WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}

		// The multiplexer's own picture splitter has to see one picture per
		// frame, each I picture behind its sequence and GOP headers.
		reader := ExportNewMPEG2PictureReader(bytes.NewReader(stream.Bytes()))
		gopStart := 0
		for index := range frames {
			picture, err := reader.Next()
			if err != nil {
				t.Fatalf("standard %v picture %d: %v", tc.standard, index, err)
			}
			start := bytes.Index(picture, []byte{0, 0, 1, 0x00})
			if start < 0 {
				t.Fatalf("standard %v picture %d has no picture start code", tc.standard, index)
			}
			bits := ExportNewBitReader(picture[start+4:])
			temporalReference, _ := ExportReadBits(bits, 10)
			pictureType, _ := ExportReadBits(bits, 3)
			_, _ = ExportReadBits(bits, 16) // vbv_delay
			wantType := uint32(2)
			if index == 0 || index == lateCut || index == lateCut+ExportMPEG2GOPPictures {
				wantType, gopStart = 1, index
			}
			if pictureType != wantType {
				t.Fatalf("standard %v picture %d has type %d, want %d (scene changes at %d and %d)", tc.standard, index, pictureType, wantType, earlyCut, lateCut)
			}
			if int(temporalReference) != index-gopStart {
				t.Errorf("standard %v picture %d: temporal_reference %d, want %d", tc.standard, index, temporalReference, index-gopStart)
			}
			var startCodes []byte
			for position := 0; position+4 <= start; position++ {
				if bytes.Equal(picture[position:position+3], []byte{0, 0, 1}) {
					startCodes = append(startCodes, picture[position+3])
				}
			}
			extension := start + 4 + bytes.Index(picture[start+4:], []byte{0, 0, 1, 0xb5})
			fCodes := picture[extension+4 : extension+7]
			if pictureType == 1 {
				// sequence_header, sequence_extension,
				// sequence_display_extension, group_of_pictures_header.
				if !bytes.Equal(startCodes, []byte{0xb3, 0xb5, 0xb5, 0xb8}) {
					t.Errorf("standard %v I picture %d is preceded by start codes % x", tc.standard, index, startCodes)
				}
				if !bytes.Equal(fCodes, []byte{0x8f, 0xff, 0xf0 | fCodes[2]&0x0f}) {
					t.Errorf("standard %v I picture %d: f_codes % x, want all 15", tc.standard, index, fCodes)
				}
				continue
			}
			if len(startCodes) != 0 {
				t.Errorf("standard %v P picture %d is preceded by start codes % x", tc.standard, index, startCodes)
			}
			// full_pel_forward_vector 0 and forward_f_code 7 follow
			// vbv_delay; the vector range is in the coding extension.
			if forward, _ := ExportReadBits(bits, 4); forward != 7 {
				t.Errorf("standard %v P picture %d: full_pel_forward_vector and forward_f_code = %04b, want 0111", tc.standard, index, forward)
			}
			wantFCode := byte(ExportMPEG2MotionRSize + 1)
			if !bytes.Equal(fCodes, []byte{0x80 | wantFCode, wantFCode<<4 | 0x0f, 0xf0 | fCodes[2]&0x0f}) {
				t.Errorf("standard %v P picture %d: f_codes % x, want forward %d and backward 15", tc.standard, index, fCodes, wantFCode)
			}
		}
		if _, err := reader.Next(); err != io.EOF {
			t.Fatalf("standard %v: picture reader found more pictures than frames: %v", tc.standard, err)
		}
	}
}

func TestMPEG2HardContentStaysDeliverableByTheDVDMultiplexer(t *testing.T) {
	// Rate control may fill the decoder buffer up to its ceiling before an
	// I picture. This runs the costliest content, with a scene change
	// every few pictures, at the highest rate through the multiplexer, which
	// rejects a stream it cannot deliver ahead of the decoding times.
	for _, tc := range []struct {
		standard               VideoStandard
		numerator, denominator int
	}{
		{VideoStandardNTSC, 30000, 1001},
		{VideoStandardPAL, 25, 1},
	} {
		profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: AspectRatio16x9, VideoBitrate: 8_500_000})
		if err != nil {
			t.Fatal(err)
		}
		workDir := t.TempDir()
		var stream bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&stream, profile)
		if err != nil {
			t.Fatal(err)
		}
		const frameCount = 96
		sizes := make([]int, 0, frameCount)
		seed := uint32(3)
		var held DecodedVideoFrame
		for index := range frameCount {
			var frame DecodedVideoFrame
			switch scene := index / 7; {
			case scene >= 6 && scene < 11:
				// One noisy picture held still: the P pictures cost
				// nothing, the buffer fills to its ceiling, and the next
				// I picture takes all that rate control allows.
				if held.Y == nil {
					held = noisyMPEG2TestFrame(profile, index, &seed)
				}
				frame = held
			case scene%3 == 0:
				frame = noisyMPEG2TestFrame(profile, index, &seed)
			case scene%3 == 1:
				frame = flatShadedMPEG2TestFrame(profile, 1, float64(index), 0)
			default:
				frame = texturedMPEG2TestFrame(profile, 2, 3*float64(index), float64(index))
			}
			before := stream.Len()
			if err := encoder.WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, stream.Len()-before)
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}

		// H.262 Annex C at the declared rate, from the occupancy the
		// encoder assumes at the first picture.
		bitsPerPicture := float64(ExportMPEG2MuxSafeVideoBitrate) * float64(tc.denominator) / float64(tc.numerator)
		fullness := float64(ExportMPEG2VBVStartBits)
		largest := 0
		for index, size := range sizes {
			if bits := float64(size * 8); bits > fullness-ExportMPEG2VBVGuardBits+8 {
				t.Fatalf("standard %v picture %d (%d bytes) leaves less than the guard in a decoder buffer holding %.0f bits", tc.standard, index, size, fullness)
			}
			fullness = min(fullness-float64(size*8)+bitsPerPicture, ExportMPEG2VBVCeilingBits)
			largest = max(largest, size)
		}
		if ExportMPEG2VBVCeilingBits > ExportMPEG2VBVBufferBits {
			t.Fatalf("rate control counts on %d bits in a decoder buffer of %d", ExportMPEG2VBVCeilingBits, ExportMPEG2VBVBufferBits)
		}
		// The sample has to contain a picture that only fits a buffer
		// filled beyond its start level, or it proves nothing about the
		// ceiling.
		if largest*8 <= ExportMPEG2VBVStartBits-ExportMPEG2VBVGuardBits {
			t.Fatalf("standard %v: largest picture is %d bytes; the sample does not load the decoder buffer", tc.standard, largest)
		}

		if err := os.WriteFile(filepath.Join(workDir, "video.m2v"), stream.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		duration := time.Duration(frameCount) * time.Second * time.Duration(tc.denominator) / time.Duration(tc.numerator)
		samples := make([]int16, int(duration*48000/time.Second)*2)
		var audio bytes.Buffer
		if err := WriteDVDLPCM(&audio, PCMChunk{SampleRate: 48000, Channels: 2, Samples: samples}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "audio.lpcm"), audio.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		title := EncodedTitle{
			VideoPath: "video.m2v", AudioPath: "audio.lpcm",
			VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
			Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
			FrameRateNumerator: tc.numerator, FrameRateDenominator: tc.denominator,
			Duration: duration,
		}
		var vobuTicks []uint32
		err = ExportMuxDVDTitleVOB(context.Background(), title, workDir, io.Discard, func(record TestDVDVOBURecord) error {
			vobuTicks = append(vobuTicks, record.EndPTM-record.StartPTM)
			return nil
		}, nil)
		if err != nil {
			t.Fatalf("standard %v: %v", tc.standard, err)
		}
		if len(vobuTicks) <= frameCount/ExportMPEG2GOPPictures {
			t.Fatalf("standard %v: %d VOBUs for %d pictures; no scene change started a GOP", tc.standard, len(vobuTicks), frameCount)
		}
		// A VOBU lasts 0.4 s to 1 s; only the last one of a cell may be
		// shorter.
		for index, ticks := range vobuTicks {
			if ticks > 90_000 || ticks < 36_000 && index != len(vobuTicks)-1 {
				t.Errorf("standard %v: VOBU %d of %d lasts %d ticks", tc.standard, index, len(vobuTicks), ticks)
			}
		}
	}
}
