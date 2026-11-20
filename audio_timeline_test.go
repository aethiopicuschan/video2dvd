package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

func rampPCM(first, frames int) []int16 {
	samples := make([]int16, frames*2)
	for i := range frames {
		value := int16((first+i)%30000 + 1)
		samples[i*2], samples[i*2+1] = value, -value
	}
	return samples
}

func decodeBigEndianPCM(t *testing.T, data []byte) []int16 {
	t.Helper()
	if len(data)%4 != 0 {
		t.Fatalf("LPCM length %d is not a whole number of stereo frames", len(data))
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.BigEndian.Uint16(data[i*2:]))
	}
	return samples
}

// Decoders stamp contiguous chunks with a nanosecond-truncated start time.
// The window writer must reproduce the samples exactly instead of dropping or
// inserting one at chunk boundaries.
func TestPCMWindowKeepsContiguousChunksSampleExact(t *testing.T) {
	for _, chunkFrames := range []int{648, 960, 1024, 4096, 8192} {
		const total = 48000
		var out bytes.Buffer
		writer, err := ExportNewPCMWindowWriter(&out, total, 0)
		if err != nil {
			t.Fatal(err)
		}
		for frame := 0; frame < total; frame += chunkFrames {
			count := min(chunkFrames, total-frame)
			err := writer.WriteChunk(PCMChunk{
				Start:      time.Duration(frame) * time.Second / 48000,
				SampleRate: 48000,
				Channels:   2,
				Samples:    rampPCM(frame, count),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Finish(); err != nil {
			t.Fatal(err)
		}
		got := decodeBigEndianPCM(t, out.Bytes())
		want := rampPCM(0, total)
		if len(got) != len(want) {
			t.Fatalf("chunk %d: wrote %d samples, want %d", chunkFrames, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("chunk %d: sample %d = %d, want %d", chunkFrames, i, got[i], want[i])
			}
		}
	}
}

// WebM block timestamps sit on a millisecond grid and may jitter; that must
// not open gaps or drop samples.
func TestPCMWindowAbsorbsTimestampJitter(t *testing.T) {
	const chunkFrames, chunks = 960, 50
	var out bytes.Buffer
	writer, err := ExportNewPCMWindowWriter(&out, chunkFrames*chunks, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range chunks {
		jitter := time.Duration(i%3-1) * time.Millisecond
		start := time.Duration(i)*20*time.Millisecond + jitter
		if i == 0 {
			start = 0
		}
		if err := writer.WriteChunk(PCMChunk{Start: start, SampleRate: 48000, Channels: 2, Samples: rampPCM(i*chunkFrames, chunkFrames)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	got := decodeBigEndianPCM(t, out.Bytes())
	want := rampPCM(0, chunkFrames*chunks)
	if !equalPCM(got, want) {
		t.Fatal("jittered chunk timestamps changed the PCM")
	}
}

func TestPCMWindowPlacesRealGapsAndOffsets(t *testing.T) {
	var out bytes.Buffer
	writer, err := ExportNewPCMWindowWriter(&out, 48000, -500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Two chunks are trimmed by the negative offset; the third follows a
	// genuine 250 ms hole.
	for _, chunk := range []PCMChunk{
		{Start: 0, Samples: rampPCM(0, 16000)},
		{Start: time.Duration(16000) * time.Second / 48000, Samples: rampPCM(16000, 16000)},
		{Start: time.Duration(44000) * time.Second / 48000, Samples: rampPCM(44000, 4000)},
	} {
		chunk.SampleRate, chunk.Channels = 48000, 2
		if err := writer.WriteChunk(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	got := decodeBigEndianPCM(t, out.Bytes())
	want := append([]int16(nil), rampPCM(24000, 8000)...)
	want = append(want, make([]int16, 12000*2)...)
	want = append(want, rampPCM(44000, 4000)...)
	want = append(want, make([]int16, 24000*2)...)
	if !equalPCM(got, want) {
		t.Fatalf("got %d samples, want %d with matching content", len(got), len(want))
	}
}

func TestEditedPCMAppliesEmptyEditMediaTimeAndLength(t *testing.T) {
	var got []int16
	var next time.Duration
	write := ExportEditPCM(
		time.Duration(480)*time.Second/48000,
		time.Duration(1024)*time.Second/48000,
		time.Duration(3000)*time.Second/48000,
		func(chunk PCMChunk) error {
			if chunk.Start != next {
				t.Fatalf("chunk starts at %s, want %s", chunk.Start, next)
			}
			got = append(got, chunk.Samples...)
			next = time.Duration(len(got)/2) * time.Second / 48000
			return nil
		})
	for frame := 0; frame < 8192; frame += 700 {
		if err := write(PCMChunk{SampleRate: 48000, Channels: 2, Samples: rampPCM(frame, 700)}); err != nil {
			t.Fatal(err)
		}
	}
	want := append(make([]int16, 480*2), rampPCM(1024, 3000)...)
	if !equalPCM(got, want) {
		t.Fatalf("edited PCM has %d samples, want %d with matching content", len(got), len(want))
	}
}

func TestHideMP4EditListRenamesOnlyTheBoxType(t *testing.T) {
	data := []byte("....edts....elst")
	hidden := ExportHideMP4EditList(bytes.NewReader(data), 4)
	for _, tc := range []struct {
		offset int64
		length int
		want   string
	}{{0, 16, "....free....elst"}, {5, 2, "re"}, {6, 6, "ee...."}, {8, 8, "....elst"}} {
		buffer := make([]byte, tc.length)
		if _, err := hidden.ReadAt(buffer, tc.offset); err != nil {
			t.Fatal(err)
		}
		if string(buffer) != tc.want {
			t.Fatalf("ReadAt(%d,%d) = %q, want %q", tc.offset, tc.length, buffer, tc.want)
		}
	}
}

func TestProbeMP4ReadsEditList(t *testing.T) {
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], 3500)
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 44100)
	binary.BigEndian.PutUint32(mdhd[16:20], 132300)
	hdlr := make([]byte, 12)
	copy(hdlr[8:12], "soun")
	elst := make([]byte, 8+2*12)
	binary.BigEndian.PutUint32(elst[4:8], 2)
	binary.BigEndian.PutUint32(elst[8:12], 478)
	binary.BigEndian.PutUint32(elst[12:16], 0xffffffff)
	binary.BigEndian.PutUint32(elst[20:24], 2999)
	binary.BigEndian.PutUint32(elst[24:28], 1024)
	edts := mp4TestBox("edts", mp4TestBox("elst", elst))
	trak := mp4TestBox("trak", edts, mp4TestBox("mdia", mp4TestBox("mdhd", mdhd), mp4TestBox("hdlr", hdlr)))
	// The movie header follows the track to check that order does not matter.
	data := append(mp4TestBox("ftyp", []byte("isom\x00\x00\x02\x00")), mp4TestBox("moov", trak, mp4TestBox("mvhd", mvhd))...)
	info, err := ProbeMP4Reader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	present, typeOffset, empty, mediaTime, segment := ExportTrackEdit(info.Tracks[0])
	if !present || empty != 478*time.Millisecond || segment != 2999*time.Millisecond || mediaTime != time.Duration(1024)*time.Second/44100 {
		t.Fatalf("edit = present %v empty %s mediaTime %s segment %s", present, empty, mediaTime, segment)
	}
	if string(data[typeOffset:typeOffset+4]) != "edts" {
		t.Fatalf("type offset %d does not point at the edts box type", typeOffset)
	}
}

func TestTitleLengthFromFrames(t *testing.T) {
	duration, samples, err := ExportTitleLengthFromFrames(90, 30000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 3003*time.Millisecond || samples != 144144 {
		t.Fatalf("NTSC 90 frames = %s, %d samples", duration, samples)
	}
	duration, samples, err = ExportTitleLengthFromFrames(75, 25, 1)
	if err != nil || duration != 3*time.Second || samples != 144000 {
		t.Fatalf("PAL 75 frames = %s, %d samples, %v", duration, samples, err)
	}
	if _, _, err := ExportTitleLengthFromFrames(0, 25, 1); err == nil {
		t.Fatal("zero frames must be rejected")
	}
}

func TestValidateInputAudioChannels(t *testing.T) {
	// AAC-LC, 48 kHz, channel configuration 6 (5.1) and 2 (stereo).
	if err := ExportValidateInputAudioChannels(TrackInfo{Codec: "mp4a", Channels: 2, CodecConfig: []byte{0x11, 0xb0}}); err == nil {
		t.Fatal("5.1 AAC must be rejected")
	}
	if err := ExportValidateInputAudioChannels(TrackInfo{Codec: "mp4a", Channels: 2, CodecConfig: []byte{0x11, 0x90}}); err != nil {
		t.Fatal(err)
	}
	if err := ExportValidateInputAudioChannels(TrackInfo{Codec: "ac-3", Channels: 6}); err != nil {
		t.Fatal(err)
	}
	if err := ExportValidateInputAudioChannels(TrackInfo{Codec: "opus", Channels: 6}); err == nil {
		t.Fatal("5.1 Opus must be rejected")
	}
}

func TestFrameRateConverterFixedOrigin(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardPAL})
	if err != nil {
		t.Fatal(err)
	}
	frame := func(pts time.Duration, luma byte) DecodedVideoFrame {
		return DecodedVideoFrame{PTS: pts, Duration: 40 * time.Millisecond, Width: 2, Height: 2, Y: []byte{luma, luma, luma, luma}, U: []byte{128}, V: []byte{128}}
	}
	run := func(origin time.Duration, inputs ...DecodedVideoFrame) []byte {
		var lumas []byte
		converter, err := NewDVDFrameRateConverter(profile, func(out DecodedVideoFrame) error {
			lumas = append(lumas, out.Y[0])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ExportSetFrameRateOrigin(converter, origin)
		for _, input := range inputs {
			if err := converter.Push(input); err != nil {
				t.Fatal(err)
			}
		}
		if err := converter.Finish(); err != nil {
			t.Fatal(err)
		}
		return lumas
	}
	// Frames before the origin are dropped.
	if got := run(80*time.Millisecond, frame(0, 1), frame(40*time.Millisecond, 2), frame(80*time.Millisecond, 3), frame(120*time.Millisecond, 4)); !bytes.Equal(got, []byte{3, 4}) {
		t.Fatalf("skip before origin: %v", got)
	}
	// A first frame after the origin is held from the origin.
	if got := run(-80*time.Millisecond, frame(0, 1), frame(40*time.Millisecond, 2)); !bytes.Equal(got, []byte{1, 1, 1, 2}) {
		t.Fatalf("hold first frame: %v", got)
	}
}

func equalPCM(left, right []int16) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// A source at the output cadence must map one to one even though its
// timestamps are rounded to container ticks and offset by a B-frame delay.
func TestFrameRateConverterKeepsEqualCadenceOneToOne(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC})
	if err != nil {
		t.Fatal(err)
	}
	for name, pts := range map[string]func(i int) time.Duration{
		"mp4 timescale 30000 with a two-frame offset": func(i int) time.Duration {
			return time.Duration(2002+i*1001) * time.Second / 30000
		},
		"webm millisecond timestamps": func(i int) time.Duration {
			return (time.Duration(i)*time.Second*1001/30000 + time.Millisecond/2) / time.Millisecond * time.Millisecond
		},
	} {
		var got []byte
		converter, err := NewDVDFrameRateConverter(profile, func(out DecodedVideoFrame) error {
			got = append(got, out.Y[0])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		const frames = 200
		for i := range frames {
			luma := byte(i)
			err := converter.Push(DecodedVideoFrame{PTS: pts(i), Duration: pts(i+1) - pts(i), Width: 2, Height: 2, Y: []byte{luma, luma, luma, luma}, U: []byte{128}, V: []byte{128}})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := converter.Finish(); err != nil {
			t.Fatal(err)
		}
		if len(got) != frames {
			t.Fatalf("%s: %d output frames, want %d", name, len(got), frames)
		}
		for i, luma := range got {
			if luma != byte(i) {
				t.Fatalf("%s: output frame %d shows source frame %d", name, i, luma)
			}
		}
	}
}
