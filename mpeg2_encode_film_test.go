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

// testFilmPicture is what a coded picture of a test stream says about
// itself.
type testFilmPicture struct {
	intra             bool
	temporalReference int
	// timeCode is the GOP header's time code in frames, for I pictures.
	timeCode                        int
	topFieldFirst, repeatFirstField bool
	size                            int
}

// readTestFilmPictures splits a stream with the multiplexer's picture
// reader and checks what every picture of a DVD stream has in common:
// progressive frame pictures in a 30000/1001 sequence that is not
// progressive.
func readTestFilmPictures(t *testing.T, stream []byte) []testFilmPicture {
	t.Helper()
	var pictures []testFilmPicture
	reader := ExportNewMPEG2PictureReader(bytes.NewReader(stream))
	for {
		data, err := reader.Next()
		if err == io.EOF {
			return pictures
		}
		if err != nil {
			t.Fatal(err)
		}
		index := len(pictures)
		start := bytes.Index(data, []byte{0, 0, 1, 0x00})
		if start < 0 {
			t.Fatalf("picture %d has no picture start code", index)
		}
		picture := testFilmPicture{size: len(data)}
		picture.temporalReference = int(data[start+4])<<2 | int(data[start+5])>>6
		pictureType := data[start+5] >> 3 & 7
		if pictureType != 1 && pictureType != 2 {
			t.Fatalf("picture %d has type %d", index, pictureType)
		}
		picture.intra = pictureType == 1
		if vbvDelay := int(data[start+5]&7)<<13 | int(data[start+6])<<5 | int(data[start+7])>>3; vbvDelay != 0xffff {
			t.Fatalf("picture %d has vbv_delay %#x", index, vbvDelay)
		}
		if picture.intra {
			sequence := bytes.Index(data[:start], []byte{0, 0, 1, 0xb3})
			gop := bytes.Index(data[:start], []byte{0, 0, 1, 0xb8})
			if sequence != 0 || gop < 0 {
				t.Fatalf("I picture %d is not behind a sequence and a GOP header", index)
			}
			if code := data[sequence+7] & 0x0f; code != 4 {
				t.Fatalf("I picture %d: frame_rate_code %d, want 4 (30000/1001)", index, code)
			}
			extension := bytes.Index(data[:start], []byte{0, 0, 1, 0xb5})
			if data[extension+4]>>4 != 1 || data[extension+5]&0x08 != 0 {
				t.Fatalf("I picture %d: the sequence extension is missing or marks the sequence progressive", index)
			}
			bits := ExportNewBitReader(data[gop+4:])
			dropFrame, _ := ExportReadBits(bits, 1)
			hours, _ := ExportReadBits(bits, 5)
			minutes, _ := ExportReadBits(bits, 6)
			marker, _ := ExportReadBits(bits, 1)
			seconds, _ := ExportReadBits(bits, 6)
			frames, _ := ExportReadBits(bits, 6)
			closed, _ := ExportReadBits(bits, 1)
			if dropFrame != 0 || marker != 1 || closed != 1 || frames >= 30 {
				t.Fatalf("I picture %d has a malformed GOP header", index)
			}
			picture.timeCode = int(((hours*60+minutes)*60+seconds)*30 + frames)
		}
		extension := start + bytes.Index(data[start:], []byte{0, 0, 1, 0xb5})
		if extension < start || data[extension+4]>>4 != 8 {
			t.Fatalf("picture %d has no picture coding extension", index)
		}
		// picture_structure 3 is a frame picture; frame_pred_frame_dct and
		// progressive_frame are set on every picture.
		if data[extension+6]&3 != 3 || data[extension+7]&0x40 == 0 || data[extension+8]&0x80 == 0 {
			t.Fatalf("picture %d is not a progressive frame picture with frame prediction: % x", index, data[extension+4:extension+9])
		}
		picture.topFieldFirst = data[extension+7]&0x80 != 0
		picture.repeatFirstField = data[extension+7]&0x02 != 0
		fields, topFieldFirst, repeatFirstField, progressive, err := ExportMPEG2PictureDisplay(data)
		wantFields := 2
		if picture.repeatFirstField {
			wantFields = 3
		}
		if err != nil || fields != wantFields || topFieldFirst != picture.topFieldFirst || repeatFirstField != picture.repeatFirstField || !progressive {
			t.Fatalf("picture %d: the multiplexer reads %d fields, flags %v/%v/%v, %v", index, fields, topFieldFirst, repeatFirstField, progressive, err)
		}
		pictures = append(pictures, picture)
	}
}

// requireFilmCadence checks the pulldown flags, the field parity and the GOP
// structure of a soft-telecined stream, and returns the pictures that start
// its GOPs.
func requireFilmCadence(t *testing.T, pictures []testFilmPicture) map[int]bool {
	t.Helper()
	gopStarts := make(map[int]bool)
	fields, gopFields, gopStart := 0, 0, 0
	nextTop := true // the parity the next field shown has to have.
	for index, picture := range pictures {
		wantTop, wantRepeat := filmPictureFlags(index)
		if index == len(pictures)-1 {
			// The stream ends on a whole frame: see filmStreamFields.
			wantRepeat = byte(filmStreamFields(len(pictures)) - filmFieldsBefore(index) - 2)
		}
		if picture.topFieldFirst != (wantTop == 1) || picture.repeatFirstField != (wantRepeat == 1) {
			t.Fatalf("picture %d has top_field_first=%v repeat_first_field=%v, want %d/%d", index, picture.topFieldFirst, picture.repeatFirstField, wantTop, wantRepeat)
		}
		// The parity alternates from field to field without regard to
		// pictures and GOPs. A three-field picture ends on the parity it
		// started with, so the next one starts with the other.
		if picture.topFieldFirst != nextTop {
			t.Fatalf("picture %d starts with the wrong field: the one before it ended on the same parity", index)
		}
		if picture.repeatFirstField {
			nextTop = !nextTop
		}
		if picture.intra {
			if index > 0 {
				// A VOBU lasts at least 0.4 s, 24 fields, and a GOP at
				// most 36 fields on an NTSC DVD.
				if gopFields < 24 || gopFields > 36 {
					t.Fatalf("the GOP before picture %d is shown for %d fields", index, gopFields)
				}
			}
			// Every GOP starts with a top field on a frame of the display
			// raster, which its time code numbers.
			if fields%2 != 0 || !picture.topFieldFirst {
				t.Fatalf("GOP at picture %d starts after %d fields, top_field_first=%v", index, fields, picture.topFieldFirst)
			}
			if picture.timeCode != fields/2 {
				t.Fatalf("GOP at picture %d has time code %d frames, want %d (%d fields)", index, picture.timeCode, fields/2, fields)
			}
			gopStarts[index] = true
			gopFields, gopStart = 0, index
		}
		if picture.temporalReference != index-gopStart {
			t.Fatalf("picture %d has temporal_reference %d, want %d", index, picture.temporalReference, index-gopStart)
		}
		// A GOP that starts with the pulldown cycle holds up to 12 pictures
		// and one that starts on the cycle's last picture 13.
		if limit := ExportMPEG2FilmGOPPictures + min(gopStart%4, 1); index-gopStart >= limit {
			t.Fatalf("picture %d is number %d of its GOP", index, index-gopStart+1)
		}
		count := 2
		if picture.repeatFirstField {
			count = 3
		}
		fields += count
		gopFields += count
	}
	if gopFields > 36 || fields != filmStreamFields(len(pictures)) || fields%2 != 0 {
		t.Fatalf("last GOP is shown for %d fields, the stream for %d", gopFields, fields)
	}
	return gopStarts
}

func newTestFilmEncoder(t *testing.T, dst io.Writer, bitrate int) (*MPEG2VideoEncoder, Profile) {
	t.Helper()
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, VideoBitrate: bitrate})
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2SoftTelecineEncoder(dst, profile)
	if err != nil {
		t.Fatal(err)
	}
	return encoder, profile
}

// The pulldown cadence runs through the whole stream: GOPs of 12 pictures
// start every 30 fields, and their time codes count display frames.
func TestMPEG2SoftTelecineCadenceAndTimeCodes(t *testing.T) {
	var stream bytes.Buffer
	encoder, profile := newTestFilmEncoder(t, &stream, 0)
	const frames = 53
	for index := range frames {
		if got, want := encoder.DisplayFields(), int64(filmFieldsBefore(index)); got != want {
			t.Fatalf("%d fields before picture %d, want %d", got, index, want)
		}
		if err := encoder.WriteFrame(flatShadedMPEG2TestFrame(profile, 1, float64(index), 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	// 53 pictures: 26 pairs of five fields and one more picture, which is
	// the first of a pulldown cycle and would have three fields. As the last
	// picture it has two, so that the stream ends on a whole frame.
	if got := encoder.DisplayFields(); got != 132 {
		t.Fatalf("stream is shown for %d fields, want 132", got)
	}
	pictures := readTestFilmPictures(t, stream.Bytes())
	if len(pictures) != frames {
		t.Fatalf("stream holds %d pictures, want %d", len(pictures), frames)
	}
	gopStarts := requireFilmCadence(t, pictures)
	// GOPs at pictures 0, 12, 24, 36 and 48 start at display frames 0, 15,
	// 30, 45 and 60: time codes 0:00, 0:15, 1:00, 1:15 and 2:00.
	for index, picture := range pictures {
		if picture.intra != (index%12 == 0) || gopStarts[index] != picture.intra {
			t.Fatalf("picture %d: intra=%v", index, picture.intra)
		}
		if picture.intra && picture.timeCode != index/12*15 {
			t.Fatalf("GOP at picture %d has time code %d frames, want %d", index, picture.timeCode, index/12*15)
		}
	}
	info, err := ExportParseMPEG2VideoHeaders(stream.Bytes())
	if err != nil || info.FrameRateNumerator != 30000 || info.FrameRateDenominator != 1001 {
		t.Fatalf("sequence frame rate %d/%d, %v", info.FrameRateNumerator, info.FrameRateDenominator, err)
	}

	if _, err := NewMPEG2SoftTelecineEncoder(&bytes.Buffer{}, Profile{Standard: VideoStandardPAL, Aspect: AspectRatio16x9, FrameWidth: 720, FrameHeight: 576}); err == nil {
		t.Fatal("a PAL soft telecine encoder was created")
	}
}

// filmStreamFields is the number of fields a soft-telecined stream of the
// given number of pictures is shown for. The pulldown cycle of four pictures
// is ten fields; a stream that stops after the first or second picture of a
// cycle would end on a top field, half a frame into the display raster, so
// its last picture has two fields instead of three or three instead of two.
func filmStreamFields(pictures int) int {
	return 10*(pictures/4) + [4]int{0, 2, 6, 8}[pictures%4]
}

// A stream of any length ends with a bottom field after a whole number of
// frames, and every picture but the last keeps to the pulldown cycle.
func TestMPEG2SoftTelecineStreamEndsOnWholeFrame(t *testing.T) {
	for frames := 1; frames <= 9; frames++ {
		var stream bytes.Buffer
		encoder, profile := newTestFilmEncoder(t, &stream, 0)
		for index := range frames {
			if err := encoder.WriteFrame(flatShadedMPEG2TestFrame(profile, 1, float64(index), 0)); err != nil {
				t.Fatal(err)
			}
			// A picture is written once it is known not to be the last.
			if got := len(readTestFilmPictures(t, stream.Bytes())); got != index {
				t.Fatalf("%d pictures written after frame %d", got, index)
			}
		}
		if got, want := encoder.DisplayFields(), int64(filmFieldsBefore(frames)); got != want {
			t.Fatalf("%d pictures: %d fields before Close, want %d", frames, got, want)
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}
		pictures := readTestFilmPictures(t, stream.Bytes())
		if len(pictures) != frames {
			t.Fatalf("stream holds %d pictures, want %d", len(pictures), frames)
		}
		requireFilmCadence(t, pictures)
		fields, lastTop := 0, false
		for _, picture := range pictures {
			count := 2
			if picture.repeatFirstField {
				count = 3
			}
			fields += count
			// The last field has the parity of the first for three fields
			// and the other one for two.
			lastTop = picture.topFieldFirst == picture.repeatFirstField
		}
		if fields%2 != 0 || lastTop || int64(fields) != encoder.DisplayFields() || fields != filmStreamFields(frames) {
			t.Fatalf("%d pictures: shown for %d fields, DisplayFields %d, last field top=%v", frames, fields, encoder.DisplayFields(), lastTop)
		}
		if difference := fields - filmFieldsBefore(frames); difference < -1 || difference > 1 {
			t.Fatalf("%d pictures: %d fields, %d off the pulldown cycle", frames, fields, difference)
		}
		if !bytes.HasSuffix(stream.Bytes(), []byte{0, 0, 1, 0xb7}) {
			t.Fatalf("%d pictures: the stream does not end with a sequence end code", frames)
		}
	}
}

// A scene change starts a GOP early only where the GOP before it has lasted
// 0.4 s and ends with a whole frame, so that every GOP starts with a top
// field on a frame of the display raster.
func TestMPEG2SoftTelecineSceneChangeKeepsFieldParity(t *testing.T) {
	var stream bytes.Buffer
	encoder, profile := newTestFilmEncoder(t, &stream, 0)
	minGOP := ExportMPEG2MinGOPPictures(encoder)
	if minGOP != ExportMPEG2FilmMinGOPPictures || filmFieldsBefore(minGOP)-1 < 24 || filmFieldsBefore(ExportMPEG2FilmGOPPictures+1) > 36 {
		t.Fatalf("film GOPs of %d to %d pictures do not fit the VOBU duration limits", minGOP, ExportMPEG2FilmGOPPictures+1)
	}
	// Cuts at pictures 5, 11, 22, 35 and 47.
	cuts := []int{5, 11, 22, 35, 47}
	scene, frames := 0, 0
	for index := range 64 {
		if scene < len(cuts) && index == cuts[scene] {
			scene++
		}
		frame := flatShadedMPEG2TestFrame(profile, scene%3, float64(131*scene)+0.5*float64(index), float64(57*scene))
		for i := range frame.Y {
			frame.Y[i] += byte(scene * 23)
		}
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		frames++
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	pictures := readTestFilmPictures(t, stream.Bytes())
	if len(pictures) != frames {
		t.Fatalf("stream holds %d pictures, want %d", len(pictures), frames)
	}
	gopStarts := requireFilmCadence(t, pictures)
	// The cut at 5 comes too early in its GOP and stays a P picture. The one
	// at 11 opens a GOP after 28 fields, on the last picture of a pulldown
	// cycle; that GOP may hold 13 pictures. The cut at 22 comes after 11 of
	// them, 27 fields, half a frame into the display raster, so the GOP runs
	// its 13 pictures, 32 fields, to picture 24, back at the start of a
	// cycle. The cut at 35 ends that GOP after 11 pictures again, and the
	// one at 47 the GOP after it after 12, 30 fields; the last GOP starts
	// 13 pictures later at 60.
	want := map[int]bool{0: true, 11: true, 24: true, 35: true, 47: true, 60: true}
	for index := range pictures {
		if gopStarts[index] != want[index] {
			t.Fatalf("picture %d: GOP start %v, want %v", index, gopStarts[index], want[index])
		}
	}
	// Time codes in display frames: 28, 60, 88, 118 and 150 fields.
	for index, timeCode := range map[int]int{11: 14, 24: 30, 35: 44, 47: 59, 60: 75} {
		if !pictures[index].topFieldFirst || pictures[index].timeCode != timeCode {
			t.Fatalf("GOP at picture %d: %+v, want time code %d", index, pictures[index], timeCode)
		}
	}
}

// Rate control follows the decoder's real schedule: a film picture is taken
// out of the buffer two or three field periods after the one before it, and
// the stream uses the bit rate over its display time, a quarter more per
// picture than a 30000/1001 stream. The multiplexer, which refuses what it
// cannot deliver ahead of the decoding times, has to carry the result.
func TestMPEG2SoftTelecineRateControlFollowsFieldTiming(t *testing.T) {
	var stream bytes.Buffer
	encoder, profile := newTestFilmEncoder(t, &stream, 8_500_000)
	if got, want := ExportMPEG2FrameBytes(encoder), ExportMPEG2MuxSafeVideoBitrate*1001/24000/8; got != want {
		t.Fatalf("average picture budget %d bytes, want %d", got, want)
	}
	const frameCount = 96
	sizes := make([]int, 0, frameCount)
	seed := uint32(3)
	var held DecodedVideoFrame
	for index := range frameCount {
		var frame DecodedVideoFrame
		switch scene := index / 11; {
		case scene >= 4 && scene < 7:
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
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	pictures := readTestFilmPictures(t, stream.Bytes())
	gopStarts := requireFilmCadence(t, pictures)
	if !gopStarts[11] {
		t.Fatalf("GOPs at %v; the scene change at picture 11 did not start one", gopStarts)
	}
	for _, picture := range pictures {
		sizes = append(sizes, picture.size)
	}
	// The last picture is read together with the sequence end code.
	sizes[len(sizes)-1] -= 4

	// H.262 Annex C at the declared rate. Without B pictures a picture is
	// decoded when the one before it is displayed, so the buffer fills for
	// the display time of the previous picture: 2, 3, 2, 3, ... fields.
	bitsPerField := float64(ExportMPEG2MuxSafeVideoBitrate) * 1001 / 60000
	fullness := float64(ExportMPEG2VBVStartBits)
	total, largest := 0, 0
	for index, size := range sizes {
		if bits := float64(size * 8); bits > fullness-ExportMPEG2VBVGuardBits+8 {
			t.Fatalf("picture %d (%d bytes) leaves less than the guard in a decoder buffer holding %.0f bits", index, size, fullness)
		}
		wantFields := 2
		if index > 0 && (index-1)%2 == 0 {
			wantFields = 3
		}
		if got := ExportMPEG2FilmDecodeFields(int64(index)); got != wantFields {
			t.Fatalf("picture %d is followed by %d field periods, want %d", index, got, wantFields)
		}
		fullness = min(fullness-float64(size*8)+float64(wantFields)*bitsPerField, ExportMPEG2VBVCeilingBits)
		total += size
		largest = max(largest, size)
	}
	if largest*8 <= ExportMPEG2VBVStartBits-ExportMPEG2VBVGuardBits {
		t.Fatalf("largest picture is %d bytes; the sample does not load the decoder buffer", largest)
	}
	// A third of the pictures are held stills that cost nothing; the rest
	// has to use what the display time brings.
	seconds := float64(encoder.DisplayFields()) * 1001 / 60000
	if rate := float64(total*8) / seconds; rate > ExportMPEG2MuxSafeVideoBitrate || rate < 0.5*ExportMPEG2MuxSafeVideoBitrate {
		t.Fatalf("stream runs at %.0f bit/s over %.2f s", rate, seconds)
	}

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "video.m2v"), stream.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	duration, sampleCount, err := ExportTitleLengthFromFields(encoder.DisplayFields(), 30000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var audio bytes.Buffer
	if err := WriteDVDLPCM(&audio, PCMChunk{SampleRate: 48000, Channels: 2, Samples: make([]int16, sampleCount*2)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "audio.lpcm"), audio.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	input := testMuxInput{
		dir: workDir, video: stream.Bytes(), audio: audio.Bytes(),
		title: EncodedTitle{
			VideoPath: "video.m2v", AudioPath: "audio.lpcm",
			VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
			Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
			FrameRateNumerator: 30000, FrameRateDenominator: 1001,
			Duration: duration,
		},
	}
	offset := 0
	for _, size := range sizes {
		input.starts = append(input.starts, offset)
		offset += size
	}
	var vob bytes.Buffer
	var records []TestDVDVOBURecord
	if err := ExportMuxDVDTitleVOB(context.Background(), input.title, workDir, &vob, func(record TestDVDVOBURecord) error {
		records = append(records, record)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	videoEnd := requireFilmVOBTiming(t, input, vob.Bytes(), records, gopStarts)
	for index, record := range records {
		if ticks := record.EndPTM - record.StartPTM; ticks > 90_000 || ticks < 36_000 && index != len(records)-1 {
			t.Errorf("VOBU %d of %d lasts %d ticks", index, len(records), ticks)
		}
	}
	// The audio ends with the video, to within the LPCM frame of 80
	// samples (150 ticks) its last packet is padded to.
	const firstPTS = testMuxStartDelay + 3003
	audioEnd := uint64(firstPTS) + uint64((sampleCount+79)/80)*150
	if audioEnd+1 < videoEnd || audioEnd > videoEnd+150 {
		t.Fatalf("audio ends at %d, video at %d", audioEnd, videoEnd)
	}
	if exact := firstPTS + filmFieldTicks(int(encoder.DisplayFields())); videoEnd != exact {
		t.Fatalf("video ends at %d, want %d", videoEnd, exact)
	}
}

// The encoder's own account of the decoder buffer is the one H.262 Annex C
// gives for the stream it wrote, and its budgets are those of the display
// time: on pictures that take whatever they are given, the stream runs at
// the declared bit rate, every GOP hands the buffer on as full as it found
// it, and no picture takes what the rest of its GOP needs.
func TestMPEG2SoftTelecineRateControlBudgetsByDisplayTime(t *testing.T) {
	var stream bytes.Buffer
	encoder, profile := newTestFilmEncoder(t, &stream, 0)
	const frameCount = 60
	seed := uint32(7)
	// model is the occupancy rate control expects before the next picture.
	model := make([]float64, 0, frameCount)
	for index := range frameCount {
		if err := encoder.WriteFrame(noisyMPEG2TestFrame(profile, index, &seed)); err != nil {
			t.Fatal(err)
		}
		model = append(model, ExportMPEG2RateFullness(encoder))
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	pictures := readTestFilmPictures(t, stream.Bytes())
	if len(pictures) != frameCount {
		t.Fatalf("stream holds %d pictures, want %d", len(pictures), frameCount)
	}
	requireFilmCadence(t, pictures)
	// Without B pictures a picture is decoded when the one before it is
	// displayed: the buffer fills for the two or three fields that one is
	// shown for, as its own flags say, and for a frame period at the start.
	bitsPerField := float64(ExportMPEG2MuxSafeVideoBitrate) * 1001 / 60000
	fullness := float64(ExportMPEG2VBVStartBits)
	total, lowest := 0, fullness
	for index, picture := range pictures {
		size := picture.size
		if index == len(pictures)-1 {
			size -= 4 // the sequence end code.
		}
		if picture.intra && index > 0 {
			if drift := fullness - ExportMPEG2VBVStartBits; drift < -0.05*ExportMPEG2VBVStartBits || drift > 0.05*ExportMPEG2VBVStartBits {
				t.Errorf("the GOP at picture %d starts with %.0f bits in the decoder buffer, want about %d", index, fullness, ExportMPEG2VBVStartBits)
			}
		}
		fields := 2
		if index > 0 && pictures[index-1].repeatFirstField {
			fields = 3
		}
		lowest = min(lowest, fullness-float64(size*8))
		fullness = min(fullness-float64(size*8)+float64(fields)*bitsPerField, ExportMPEG2VBVCeilingBits)
		if difference := fullness - model[index]; difference < -1 || difference > 1 {
			t.Fatalf("after picture %d the decoder buffer holds %.0f bits, rate control expects %.0f", index, fullness, model[index])
		}
		total += size
	}
	seconds := float64(encoder.DisplayFields()) * 1001 / 60000
	if rate := float64(total*8) / seconds; rate < 0.995*ExportMPEG2MuxSafeVideoBitrate || rate > 1.005*ExportMPEG2MuxSafeVideoBitrate {
		t.Errorf("stream runs at %.0f bit/s over %.3f s, want %d", rate, seconds, ExportMPEG2MuxSafeVideoBitrate)
	}
	// All pictures being alike, none has reason to take the buffer down to
	// the guard, as a GOP budgeted for more fields than it lasts would.
	if lowest < 2*ExportMPEG2VBVGuardBits {
		t.Errorf("the decoder buffer goes down to %.0f bits", lowest)
	}

	// What the buffer cannot hold until the next picture is free to spend:
	// the more the longer that is away.
	almostFull := ExportMPEG2VBVCeilingBits - bitsPerField
	for fields := 1; fields <= 3; fields++ {
		if got, want := ExportMPEG2PredictiveSpareBytes(encoder, almostFull, fields), int(float64(fields-1)*bitsPerField)/8; got != want {
			t.Errorf("a P picture %d fields before the next may spend %d spare bytes, want %d", fields, got, want)
		}
	}
}

// A decoder has to see what a player sees: 23.976 coded pictures per second
// in a 29.97 fps stream, every other one with a repeated field, and nothing
// to complain about.
func TestMPEG2SoftTelecineStreamDecodesAsFilm(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to decode the MPEG-2 soft telecine sample")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is required to inspect the MPEG-2 soft telecine sample")
	}
	var stream bytes.Buffer
	encoder, profile := newTestFilmEncoder(t, &stream, 4_000_000)
	const frameCount = 48
	for index := range frameCount {
		if err := encoder.WriteFrame(movingMPEG2TestFrame(profile, index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	streamPath := filepath.Join(t.TempDir(), "film.m2v")
	if err := os.WriteFile(streamPath, stream.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// One line per coded picture.
	output, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "frame=interlaced_frame,top_field_first,repeat_pict", "-of", "default=noprint_wrappers=1:nokey=0", streamPath).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect MPEG-2 soft telecine sample: %v: %s", err, output)
	}
	var interlaced, topFieldFirst, repeat []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "interlaced_frame":
			interlaced = append(interlaced, value)
		case "top_field_first":
			topFieldFirst = append(topFieldFirst, value)
		case "repeat_pict":
			repeat = append(repeat, value)
		default:
			t.Fatalf("unexpected ffprobe output %q", line)
		}
	}
	if len(interlaced) != frameCount || len(topFieldFirst) != frameCount || len(repeat) != frameCount {
		t.Fatalf("ffprobe reports %d/%d/%d frame entries for %d coded pictures", len(interlaced), len(topFieldFirst), len(repeat), frameCount)
	}
	for index := range frameCount {
		wantTop, wantRepeat := filmPictureFlags(index)
		if interlaced[index] != "0" || topFieldFirst[index] != string('0'+wantTop) || repeat[index] != string('0'+wantRepeat) {
			t.Fatalf("picture %d: interlaced_frame=%s top_field_first=%s repeat_pict=%s, want 0/%d/%d", index, interlaced[index], topFieldFirst[index], repeat[index], wantTop, wantRepeat)
		}
	}

	// In a program stream with field-based timestamps the pictures come out
	// at the film rate.
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "video.m2v"), stream.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	duration, sampleCount, err := ExportTitleLengthFromFields(encoder.DisplayFields(), 30000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var audio bytes.Buffer
	if err := WriteDVDLPCM(&audio, PCMChunk{SampleRate: 48000, Channels: 2, Samples: make([]int16, sampleCount*2)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "audio.lpcm"), audio.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	vobPath := filepath.Join(workDir, "title.vob")
	vob, err := os.Create(vobPath)
	if err != nil {
		t.Fatal(err)
	}
	err = MuxDVDTitleVOB(ctx, EncodedTitle{
		VideoPath: "video.m2v", AudioPath: "audio.lpcm",
		VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
		Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
		FrameRateNumerator: 30000, FrameRateDenominator: 1001,
		Duration: duration,
	}, workDir, vob)
	if closeErr := vob.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	output, err = exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "frame=pts", "-of", "csv=p=0", vobPath).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect soft telecine VOB: %v: %s", err, output)
	}
	lines := strings.Fields(string(output))
	if len(lines) != frameCount {
		t.Fatalf("ffprobe reads %d frames from the VOB, want %d: %s", len(lines), frameCount, output)
	}
	for index, line := range lines {
		// 48 pictures in 120 fields: 23.976 per second, each at the time
		// of its first field.
		if want := testMuxStartDelay + filmFieldTicks(2+filmFieldsBefore(index)); strings.TrimRight(line, ",") != itoa(want) {
			t.Fatalf("picture %d is presented at %s, want %d", index, line, want)
		}
	}

	// -xerror makes any bitstream error fail the decode instead of being
	// concealed; a clean decode prints nothing at this log level.
	decodedPath := filepath.Join(workDir, "decoded.yuv")
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "warning", "-xerror", "-err_detect", "explode", "-i", vobPath, "-map", "0:v:0", "-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "yuv420p", decodedPath, "-y")
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("decode soft telecine VOB: %v: %s", err, output)
	}
	decoded, err := os.Stat(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(frameCount * profile.FrameWidth * profile.FrameHeight * 3 / 2); decoded.Size() != want {
		t.Fatalf("decoded %d bytes, want %d coded pictures of %d bytes", decoded.Size(), frameCount, want/frameCount)
	}
}

func itoa(value uint64) string {
	digits := []byte{}
	for {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		if value /= 10; value == 0 {
			return string(digits)
		}
	}
}
