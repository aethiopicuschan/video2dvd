package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// filmFieldsBefore is the number of fields shown before film picture number
// picture of a soft-telecined stream: 3, 2, 3, 2, ... fields per picture.
func filmFieldsBefore(picture int) int {
	return 5*(picture/2) + 3*(picture%2)
}

// filmPictureFlags returns top_field_first and repeat_first_field of film
// picture number picture.
func filmPictureFlags(picture int) (topFieldFirst, repeatFirstField byte) {
	cycle := [4][2]byte{{1, 1}, {0, 0}, {0, 1}, {1, 0}}[picture%4]
	return cycle[0], cycle[1]
}

// filmFieldTicks is the time of a display field in 90 kHz ticks, rounded
// down from its exact 1501.5 ticks per field.
func filmFieldTicks(field int) uint64 {
	return uint64(field) * 3003 / 2
}

// makeTestFilmStream builds a soft-telecined elementary stream of pictures
// with the given sizes, the pictures listed in gopStarts being I pictures
// behind a sequence prefix. Each picture has a picture_coding_extension with
// the pulldown flags of its place in the cadence. It returns the stream and
// the offset at which each picture starts.
func makeTestFilmStream(prefix []byte, sizes []int, gopStarts map[int]bool) ([]byte, []int) {
	var stream []byte
	starts := make([]int, len(sizes))
	reference := 0
	for index, size := range sizes {
		starts[index] = len(stream)
		begin := len(stream)
		pictureType := byte(2)
		reference++
		if gopStarts[index] {
			stream = append(stream, prefix...)
			pictureType, reference = 1, 0
		}
		topFieldFirst, repeatFirstField := filmPictureFlags(index)
		stream = append(stream, 0, 0, 1, 0x00, byte(reference>>2), byte(reference<<6)|pictureType<<3|0x07, 0xff, 0xf8)
		stream = append(stream, 0, 0, 1, 0xb5,
			0x8f, 0xff, // extension id 8 and f_codes.
			0xf3, // f_code, intra_dc_precision 0, frame picture.
			topFieldFirst<<7|0x40|repeatFirstField<<1|1, // frame_pred_frame_dct and chroma_420_type set.
			0x80, // progressive_frame.
			0, 0, 1, 0x01)
		for len(stream)-begin < size {
			stream = append(stream, 0xa5)
		}
	}
	return append(stream, 0, 0, 1, 0xb7), starts
}

func newTestFilmMuxInput(t *testing.T, sizes []int, gopStarts map[int]bool, audioSamples int) testMuxInput {
	t.Helper()
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	input := testMuxInput{dir: t.TempDir()}
	input.video, input.starts = makeTestFilmStream(testMPEG2StreamPrefix(t, profile), sizes, gopStarts)
	input.audio = make([]byte, audioSamples*4)
	for i := range input.audio {
		input.audio[i] = byte(i*7 + i/251)
	}
	if err := os.WriteFile(filepath.Join(input.dir, "video.m2v"), input.video, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.dir, "audio.lpcm"), input.audio, 0o600); err != nil {
		t.Fatal(err)
	}
	input.title = EncodedTitle{
		VideoPath: "video.m2v", AudioPath: "audio.lpcm",
		VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
		Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
		FrameRateNumerator: 30000, FrameRateDenominator: 1001,
		Duration: time.Duration(filmFieldsBefore(len(sizes))) * time.Second * 1001 / 60000,
	}
	return input
}

// requireFilmVOBTiming checks a multiplexed soft-telecined title against the
// timing model: every picture stamped with the times of its display fields,
// every byte delivered before its picture is decoded, decoder buffers within
// their sizes, audio on the video's timeline, and NAV packs and VOBU records
// that state the presentation times of the pictures they hold. It returns
// the end time of the video.
func requireFilmVOBTiming(t *testing.T, input testMuxInput, vob []byte, records []TestDVDVOBURecord, gopStarts map[int]bool) uint64 {
	t.Helper()
	pictures := len(input.starts)
	// A picture is presented from the first field after those of its
	// predecessors, one frame period after the first decoding, and decoded
	// when its predecessor is presented.
	pts := func(picture int) uint64 { return testMuxStartDelay + filmFieldTicks(2+filmFieldsBefore(picture)) }
	dts := func(picture int) uint64 {
		if picture == 0 {
			return testMuxStartDelay
		}
		return pts(picture - 1)
	}
	firstPTS := pts(0)
	pictureOf := func(offset int) int {
		low, high := 0, pictures-1
		for low < high {
			middle := (low + high + 1) / 2
			if input.starts[middle] <= offset {
				low = middle
			} else {
				high = middle - 1
			}
		}
		return low
	}
	type buffered struct {
		release uint64
		bytes   int
	}
	var videoBuffer, audioBuffer []buffered
	videoOccupancy, audioOccupancy := 0, 0
	var navs, audioPacks []testVOBPack
	videoBytes, audioBytes, stamped := 0, 0, 0
	for index, pack := range parseTestVOB(t, vob) {
		packEnd := pack.scr + testMuxPackTicks
		for len(videoBuffer) > 0 && videoBuffer[0].release <= pack.scr {
			videoOccupancy -= videoBuffer[0].bytes
			videoBuffer = videoBuffer[1:]
		}
		for len(audioBuffer) > 0 && audioBuffer[0].release <= pack.scr {
			audioOccupancy -= audioBuffer[0].bytes
			audioBuffer = audioBuffer[1:]
		}
		switch {
		case pack.nav:
			navs = append(navs, pack)
		case pack.streamID == 0xe0:
			if !bytes.Equal(pack.payload, input.video[videoBytes:videoBytes+len(pack.payload)]) {
				t.Fatalf("sector %d does not continue the elementary stream", index)
			}
			first := pictureOf(videoBytes)
			last := pictureOf(videoBytes + len(pack.payload) - 1)
			starting := -1
			if input.starts[first] == videoBytes {
				starting = first
			} else if last != first {
				starting = last
			}
			if last-first > 1 || starting >= 0 && last != starting {
				t.Fatalf("sector %d carries the start of more than one picture", index)
			}
			if pack.hasPTS != (starting >= 0) || pack.hasDTS != pack.hasPTS {
				t.Fatalf("sector %d timestamps present=%v/%v, picture starting=%d", index, pack.hasPTS, pack.hasDTS, starting)
			}
			if starting >= 0 {
				if pack.pts != pts(starting) || pack.dts != dts(starting) {
					t.Fatalf("picture %d PTS/DTS = %d/%d, want %d/%d", starting, pack.pts, pack.dts, pts(starting), dts(starting))
				}
				if starting != stamped {
					t.Fatalf("picture %d is stamped out of order, expected %d", starting, stamped)
				}
				if gopStarts[starting] != (len(navs) > 0 && navs[len(navs)-1].sector == index-1) {
					t.Fatalf("picture %d: GOP start %v does not match its place behind a NAV pack", starting, gopStarts[starting])
				}
				stamped++
			}
			if deadline := dts(first) * 300; packEnd > deadline || pack.scr >= deadline {
				t.Fatalf("sector %d delivers picture %d at %d, after its decode time %d", index, first, packEnd, deadline)
			}
			for offset := videoBytes; offset < videoBytes+len(pack.payload); {
				picture := pictureOf(offset)
				end := len(input.video)
				if picture+1 < pictures {
					end = input.starts[picture+1]
				}
				end = min(end, videoBytes+len(pack.payload))
				videoBuffer = append(videoBuffer, buffered{release: dts(picture) * 300, bytes: end - offset})
				videoOccupancy += end - offset
				offset = end
			}
			header := 9 + int(pack.raw[22])
			videoBuffer[len(videoBuffer)-1].bytes += header
			videoOccupancy += header
			if videoOccupancy > testMuxVideoBuffer {
				t.Fatalf("sector %d fills the video decoder buffer to %d bytes, above %d", index, videoOccupancy, testMuxVideoBuffer)
			}
			videoBytes += len(pack.payload)
		default:
			samples := pack.payload[7:]
			wantPTS := firstPTS + uint64(audioBytes/320)*150
			if !pack.hasPTS || pack.hasDTS || pack.pts != wantPTS {
				t.Fatalf("sector %d audio PTS = %d, want %d", index, pack.pts, wantPTS)
			}
			if packEnd > wantPTS*300 {
				t.Fatalf("sector %d delivers audio at %d, after its presentation time %d", index, packEnd, wantPTS*300)
			}
			audioBuffer = append(audioBuffer, buffered{release: (wantPTS + uint64(len(samples)/320)*150) * 300, bytes: len(pack.payload)})
			audioOccupancy += len(pack.payload)
			if audioOccupancy > testMuxAudioBuffer {
				t.Fatalf("sector %d fills the audio decoder buffer to %d bytes, above %d", index, audioOccupancy, testMuxAudioBuffer)
			}
			audioBytes += len(samples)
			audioPacks = append(audioPacks, pack)
		}
	}
	if videoBytes != len(input.video) || stamped != pictures {
		t.Fatalf("demultiplexed %d of %d video bytes and %d of %d picture timestamps", videoBytes, len(input.video), stamped, pictures)
	}
	if audioBytes < len(input.audio) || audioBytes >= len(input.audio)+320 {
		t.Fatalf("demultiplexed %d audio bytes of %d", audioBytes, len(input.audio))
	}

	var gops []int
	for picture := range pictures {
		if gopStarts[picture] {
			gops = append(gops, picture)
		}
	}
	if len(navs) != len(gops) || len(records) != len(gops) {
		t.Fatalf("found %d NAV packs and %d VOBU records for %d GOPs", len(navs), len(records), len(gops))
	}
	vobEnd := pts(pictures)
	for index, nav := range navs {
		pci, dsi := nav.raw[45:], nav.raw[0x407:]
		endPicture := pictures
		if index+1 < len(gops) {
			endPicture = gops[index+1]
		}
		startPTM, endPTM := uint32(pts(gops[index])), uint32(pts(endPicture))
		if got := [2]uint32{binary.BigEndian.Uint32(pci[12:16]), binary.BigEndian.Uint32(pci[16:20])}; got != [2]uint32{startPTM, endPTM} {
			t.Fatalf("NAV %d presentation span = %d..%d, want %d..%d", index, got[0], got[1], startPTM, endPTM)
		}
		if uint64(startPTM)*300 <= nav.scr {
			t.Fatalf("NAV %d arrives at %d, not before its VOBU is presented at %d", index, nav.scr, uint64(startPTM)*300)
		}
		wantSequenceEnd := uint32(0)
		if index == len(navs)-1 {
			wantSequenceEnd = endPTM
		}
		if got := binary.BigEndian.Uint32(pci[20:24]); got != wantSequenceEnd {
			t.Fatalf("NAV %d vobu_se_e_ptm = %d, want %d", index, got, wantSequenceEnd)
		}
		// The cell elapsed time is a 30 fps time code of the time since the
		// first picture: the display frame the VOBU starts in or next to.
		elapsed := pci[24:28]
		frame := int(elapsed[0]>>4*10+elapsed[0]&15)*108000 + int(elapsed[1]>>4*10+elapsed[1]&15)*1800 +
			int(elapsed[2]>>4*10+elapsed[2]&15)*30 + int(elapsed[3]>>4&3*10+elapsed[3]&15)
		if fields := filmFieldsBefore(gops[index]); elapsed[3]>>6 != 3 || frame != fields/2 && frame != (fields+1)/2 || !bytes.Equal(elapsed, dsi[28:32]) {
			t.Fatalf("NAV %d cell elapsed time % x / % x is not display frame %d", index, elapsed, dsi[28:32], fields/2)
		}
		if binary.BigEndian.Uint32(dsi[44:48]) != uint32(firstPTS) || binary.BigEndian.Uint32(dsi[48:52]) != uint32(vobEnd) {
			t.Fatalf("NAV %d VOB presentation span = %d..%d, want %d..%d", index, binary.BigEndian.Uint32(dsi[44:48]), binary.BigEndian.Uint32(dsi[48:52]), firstPTS, vobEnd)
		}
		if record := records[index]; record.StartSector != uint32(nav.sector) || record.StartPTM != startPTM || record.EndPTM != endPTM {
			t.Fatalf("VOBU record %d = %+v, want sector %d and time %d..%d", index, record, nav.sector, startPTM, endPTM)
		}
		wantSync := DVDNAVAudioSyncNone
		for _, pack := range audioPacks {
			if pack.pts <= uint64(startPTM) && uint64(startPTM) < pack.pts+uint64((len(pack.payload)-7)/320)*150 {
				if pack.sector >= nav.sector {
					wantSync = uint16(pack.sector - nav.sector)
				} else {
					wantSync = uint16(nav.sector-pack.sector) | 0x8000
				}
			}
		}
		if got := binary.BigEndian.Uint16(dsi[402:404]); got != wantSync {
			t.Fatalf("NAV %d A_SYNCA = %#x, want %#x", index, got, wantSync)
		}
		// The search table addresses VOBUs by presentation time: the next
		// VOBU is the one this VOBU's end time starts.
		if index+1 < len(navs) {
			if got, want := binary.BigEndian.Uint32(dsi[234+80:]), 0x80000000|uint32(navs[index+1].sector-nav.sector); got != want {
				t.Fatalf("NAV %d next VOBU = %#x, want %#x", index, got, want)
			}
		}
	}
	return vobEnd
}

// The timestamps of a soft-telecined stream follow its display fields: a
// picture with repeat_first_field lasts 4504.5 ticks, one without 3003, and
// each picture is decoded when the one before it is presented.
func TestMuxDVDTitleVOBTimesSoftTelecineByFields(t *testing.T) {
	// GOPs of 12, 10, 11, 12 and 9 pictures: the last three start on a
	// bottom field, half a frame period off the frame grid.
	gopStarts := map[int]bool{0: true, 12: true, 22: true, 33: true, 45: true}
	const pictures = 54
	sizes := make([]int, pictures)
	for index := range sizes {
		sizes[index] = 30_000
		if gopStarts[index] {
			sizes[index] = 150_000
		}
	}
	// 54 pictures are 135 fields, 2.25225 s: 108108 samples.
	input := newTestFilmMuxInput(t, sizes, gopStarts, 108_108)
	var vob bytes.Buffer
	var records []TestDVDVOBURecord
	if err := ExportMuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob, func(record TestDVDVOBURecord) error {
		records = append(records, record)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	requireFilmVOBTiming(t, input, vob.Bytes(), records, gopStarts)

	// The first two pulldown cycles, spelled out. Fields start every 1501.5
	// ticks, rounded down.
	wantPTS := []uint64{30003, 34507, 37510, 42015, 45018, 49522, 52525, 57030, 60033}
	wantDTS := []uint64{27000, 30003, 34507, 37510, 42015, 45018, 49522, 52525, 57030}
	picture := 0
	for _, pack := range parseTestVOB(t, vob.Bytes()) {
		if pack.streamID != 0xe0 || !pack.hasPTS || picture >= len(wantPTS) {
			continue
		}
		if pack.pts != wantPTS[picture] || pack.dts != wantDTS[picture] {
			t.Fatalf("picture %d PTS/DTS = %d/%d, want %d/%d", picture, pack.pts, pack.dts, wantPTS[picture], wantDTS[picture])
		}
		picture++
	}
	// VOBUs of 30, 25, 28, 30 and 22 fields. The boundaries behind an odd
	// number of fields are on a half tick, which is rounded down.
	wantSpans := [][2]uint32{{30003, 75048}, {75048, 112585}, {112585, 154627}, {154627, 199672}, {199672, 232705}}
	for index, record := range records {
		if got := [2]uint32{record.StartPTM, record.EndPTM}; got != wantSpans[index] {
			t.Fatalf("VOBU %d presentation span = %v, want %v", index, got, wantSpans[index])
		}
	}
}

// Ten minutes of film: 14400 pictures are exactly 36000 fields, 600.6 s, and
// the last timestamps are as exact as the first.
func TestMuxDVDTitleVOBSoftTelecineDoesNotDrift(t *testing.T) {
	const pictures = 14_400
	gopStarts := make(map[int]bool)
	sizes := make([]int, pictures)
	for index := range sizes {
		sizes[index] = 1_500
		if index%12 == 0 {
			gopStarts[index] = true
			sizes[index] = 6_000
		}
	}
	input := newTestFilmMuxInput(t, sizes, gopStarts, 48_000)
	var vob bytes.Buffer
	var records []TestDVDVOBURecord
	if err := ExportMuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob, func(record TestDVDVOBURecord) error {
		records = append(records, record)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	end := requireFilmVOBTiming(t, input, vob.Bytes(), records, gopStarts)
	const firstPTS, tenMinutes = 30_003, 600_600 * 90
	if end != firstPTS+tenMinutes || records[len(records)-1].EndPTM != firstPTS+tenMinutes {
		t.Fatalf("video ends at %d (VOBU record %d), want %d", end, records[len(records)-1].EndPTM, firstPTS+tenMinutes)
	}
	for index, record := range records {
		// Every GOP of 12 pictures is 30 fields: 45045 ticks.
		if want := uint32(firstPTS + index*45_045); record.StartPTM != want || record.EndPTM != want+45_045 {
			t.Fatalf("VOBU %d presentation span = %d..%d, want %d..%d", index, record.StartPTM, record.EndPTM, want, want+45_045)
		}
	}
}

// A stream that flags a repeated field on a picture that is not a
// progressive frame is not valid MPEG-2, and one coded at the film rate is
// not DVD-Video; both are refused instead of being timed wrongly.
func TestMuxDVDTitleVOBRejectsInvalidFilmStreams(t *testing.T) {
	gopStarts := map[int]bool{0: true}
	input := newTestFilmMuxInput(t, []int{20_000, 5_000, 5_000, 5_000}, gopStarts, 8_000)
	// Clear progressive_frame of the third picture, which repeats a field.
	extension := input.starts[2] + bytes.Index(input.video[input.starts[2]:], []byte{0, 0, 1, 0xb5})
	input.video[extension+8] = 0
	if err := os.WriteFile(filepath.Join(input.dir, "video.m2v"), input.video, 0o600); err != nil {
		t.Fatal(err)
	}
	err := MuxDVDTitleVOB(context.Background(), input.title, input.dir, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "repeat_first_field") {
		t.Fatalf("interlaced picture with repeat_first_field: %v", err)
	}

	input = newTestFilmMuxInput(t, []int{20_000, 5_000}, gopStarts, 8_000)
	input.title.FrameRateNumerator = 24000
	err = MuxDVDTitleVOB(context.Background(), input.title, input.dir, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "soft telecine") {
		t.Fatalf("title declared as 24000/1001: %v", err)
	}
	if err := ExportCheckDVDFrameRate(30000, 1001); err != nil {
		t.Fatal(err)
	}
	if err := ExportCheckDVDFrameRate(25, 1); err != nil {
		t.Fatal(err)
	}

	// The Converter refuses such a title of an external encoder for the
	// same reason, and any other rate the way it always did.
	profile, err := ExportProfile(Options{Aspect: input.title.Aspect})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExportValidateTitle(input.title, input.dir, profile); err == nil || !strings.Contains(err.Error(), "soft telecine") {
		t.Fatalf("title declared as 24000/1001: %v", err)
	}
	for _, rate := range [][2]int{{50, 1}, {25, 1}, {24, 1}, {30, 1}} {
		input.title.FrameRateNumerator, input.title.FrameRateDenominator = rate[0], rate[1]
		want := fmt.Sprintf("frame rate %d/%d is not valid for the selected DVD standard", rate[0], rate[1])
		if err := ExportValidateTitle(input.title, input.dir, profile); err == nil || err.Error() != want {
			t.Fatalf("title declared as %d/%d: %v, want %q", rate[0], rate[1], err, want)
		}
	}
	input.title.FrameRateNumerator, input.title.FrameRateDenominator = 30000, 1001
	if err := ExportValidateTitle(input.title, input.dir, profile); err != nil {
		t.Fatal(err)
	}
}

func TestMPEG2PictureDisplayFields(t *testing.T) {
	stream, starts := makeTestFilmStream([]byte{0, 0, 1, 0xb3, 1, 2, 3, 4, 0, 0, 1, 0xb8, 0, 0, 0, 0}, []int{64, 64, 64, 64, 64}, map[int]bool{0: true, 4: true})
	for picture := range 5 {
		end := len(stream)
		if picture+1 < len(starts) {
			end = starts[picture+1]
		}
		fields, topFieldFirst, repeatFirstField, progressive, err := ExportMPEG2PictureDisplay(stream[starts[picture]:end])
		if err != nil {
			t.Fatal(err)
		}
		wantTop, wantRepeat := filmPictureFlags(picture)
		if fields != 2+int(wantRepeat) || topFieldFirst != (wantTop == 1) || repeatFirstField != (wantRepeat == 1) || !progressive {
			t.Fatalf("picture %d: %d fields, top_field_first=%v repeat_first_field=%v progressive_frame=%v", picture, fields, topFieldFirst, repeatFirstField, progressive)
		}
	}
	// A picture without a coding extension, and one whose slice data happens
	// to hold what looks like one, last two fields.
	plain := []byte{0, 0, 1, 0x00, 0, 0x0f, 0xff, 0xf8, 0, 0, 1, 0x01, 0xa5, 0, 0, 1, 0xb5, 0x8f, 0xff, 0xf3, 0xc3, 0x80}
	if fields, _, repeatFirstField, _, err := ExportMPEG2PictureDisplay(plain); err != nil || fields != 2 || repeatFirstField {
		t.Fatalf("picture without coding extension: %d fields, repeat_first_field=%v, %v", fields, repeatFirstField, err)
	}
	if _, _, _, _, err := ExportMPEG2PictureDisplay([]byte{0, 0, 1, 0xb3}); err == nil {
		t.Fatal("data without a picture was accepted")
	}
}

func TestTitleLengthFromFields(t *testing.T) {
	for _, pictures := range []int{1, 2, 3, 4, 471, 4315, 14_400, 259_200} {
		fields := int64(filmFieldsBefore(pictures))
		duration, samples, err := ExportTitleLengthFromFields(fields, 30000, 1001)
		if err != nil {
			t.Fatal(err)
		}
		// A field is 1001/60000 s, which is 800.8 samples.
		if want := time.Duration(fields * 1001 * int64(time.Second) / 60000); duration != want {
			t.Fatalf("%d film pictures last %v, want %v", pictures, duration, want)
		}
		if difference := 10*samples - fields*8008; difference < -5 || difference > 5 {
			t.Fatalf("%d film pictures get %d samples for %d fields", pictures, samples, fields)
		}
	}
	// Whole frames give what the frame count always gave.
	for _, rate := range [][2]int{{30000, 1001}, {25, 1}} {
		for frames := int64(1); frames < 5000; frames += 7 {
			wantDuration, wantSamples, err := ExportTitleLengthFromFrames(frames, rate[0], rate[1])
			if err != nil {
				t.Fatal(err)
			}
			exact := time.Duration(frames) * time.Second * time.Duration(rate[1]) / time.Duration(rate[0])
			duration, samples, err := ExportTitleLengthFromFields(2*frames, rate[0], rate[1])
			if err != nil || duration != wantDuration || samples != wantSamples || duration != exact {
				t.Fatalf("%d frames at %d/%d: %v and %d samples, want %v and %d (%v)", frames, rate[0], rate[1], duration, samples, wantDuration, wantSamples, err)
			}
		}
	}
	if _, _, err := ExportTitleLengthFromFields(0, 30000, 1001); err == nil {
		t.Fatal("an empty title has a length")
	}
}
