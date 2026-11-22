package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
	"github.com/thesyncim/govpx"
)

// testEBMLElement encodes one EBML element with an eight-byte size.
func testEBMLElement(id uint32, payload ...[]byte) []byte {
	var idBytes []byte
	for shift := 24; shift >= 0; shift -= 8 {
		if b := byte(id >> shift); b != 0 || len(idBytes) > 0 {
			idBytes = append(idBytes, b)
		}
	}
	body := bytes.Join(payload, nil)
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(body)))
	size[0] = 0x01
	return slices.Concat(idBytes, size, body)
}

func testEBMLUint(id uint32, value uint64) []byte {
	var data []byte
	for shift := 56; shift >= 0; shift -= 8 {
		if b := byte(value >> shift); b != 0 || len(data) > 0 || shift == 0 {
			data = append(data, b)
		}
	}
	return testEBMLElement(id, data)
}

func testEBMLFloat(id uint32, value float64) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, math.Float64bits(value))
	return testEBMLElement(id, data)
}

func testEBMLString(id uint32, value string) []byte {
	return testEBMLElement(id, []byte(value))
}

// testWebMTrack describes one track of a test WebM file.
type testWebMTrack struct {
	trackType               uint64 // 1 video, 2 audio, 0x11 subtitle.
	codecID                 string
	language, languageBCP47 string
	name                    string
	private                 []byte
	width, height           int
}

// testWebMBlock is one block of a test WebM file. A block with a duration
// is written as a BlockGroup with a BlockDuration, as subtitles are.
type testWebMBlock struct {
	track    int // index into the tracks, from zero.
	time     time.Duration
	duration time.Duration
	keyframe bool
	data     []byte
}

// writeTestWebM writes a WebM file with the given tracks and blocks, in
// clusters of one second.
func writeTestWebM(t *testing.T, name string, tracks []testWebMTrack, blocks []testWebMBlock, duration time.Duration) string {
	t.Helper()
	header := testEBMLElement(0x1a45dfa3,
		testEBMLUint(0x4286, 1), testEBMLUint(0x42f7, 1), testEBMLUint(0x42f2, 4), testEBMLUint(0x42f3, 8),
		testEBMLString(0x4282, "webm"), testEBMLUint(0x4287, 4), testEBMLUint(0x4285, 2))
	info := testEBMLElement(0x1549a966, testEBMLUint(0x2ad7b1, 1_000_000), testEBMLFloat(0x4489, float64(duration/time.Millisecond)),
		testEBMLString(0x4d80, "video2dvd test"), testEBMLString(0x5741, "video2dvd test"))
	var entries [][]byte
	for i, track := range tracks {
		fields := [][]byte{
			testEBMLUint(0xd7, uint64(i+1)), testEBMLUint(0x73c5, uint64(i+100)), testEBMLUint(0x83, track.trackType),
			testEBMLString(0x86, track.codecID), testEBMLUint(0x9c, 0),
		}
		if track.language != "" {
			fields = append(fields, testEBMLString(0x22b59c, track.language))
		}
		if track.languageBCP47 != "" {
			fields = append(fields, testEBMLString(0x22b59d, track.languageBCP47))
		}
		if track.name != "" {
			fields = append(fields, testEBMLString(0x536e, track.name))
		}
		if track.private != nil {
			fields = append(fields, testEBMLElement(0x63a2, track.private))
		}
		switch track.trackType {
		case 1:
			fields = append(fields, testEBMLElement(0xe0, testEBMLUint(0xb0, uint64(track.width)), testEBMLUint(0xba, uint64(track.height))))
		case 2:
			fields = append(fields, testEBMLElement(0xe1, testEBMLFloat(0xb5, 48000), testEBMLUint(0x9f, 2)))
		}
		entries = append(entries, testEBMLElement(0xae, fields...))
	}
	sorted := slices.Clone(blocks)
	slices.SortStableFunc(sorted, func(a, b testWebMBlock) int { return int(a.time - b.time) })
	var clusters [][]byte
	for start := 0; start < len(sorted); {
		clusterTime := sorted[start].time / time.Second * time.Second
		fields := [][]byte{testEBMLUint(0xe7, uint64(clusterTime/time.Millisecond))}
		end := start
		for end < len(sorted) && sorted[end].time < clusterTime+time.Second {
			block := sorted[end]
			var flags byte
			if block.keyframe {
				flags = 0x80
			}
			relative := int16((block.time - clusterTime) / time.Millisecond)
			data := slices.Concat([]byte{0x80 | byte(block.track+1), byte(uint16(relative) >> 8), byte(relative)}, []byte{flags}, block.data)
			if block.duration > 0 {
				data[3] = 0
				fields = append(fields, testEBMLElement(0xa0, testEBMLElement(0xa1, data), testEBMLUint(0x9b, uint64(block.duration/time.Millisecond))))
			} else {
				fields = append(fields, testEBMLElement(0xa3, data))
			}
			end++
		}
		clusters = append(clusters, testEBMLElement(0x1f43b675, fields...))
		start = end
	}
	segment := testEBMLElement(0x18538067, slices.Concat([][]byte{info, testEBMLElement(0x1654ae6b, entries...)}, clusters)...)
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, append(header, segment...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testOpusHead is the CodecPrivate of a stereo Opus track.
var testOpusHead = []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0}

// testWebMMedia returns the VP9 video track and blocks of 30 frames at 30
// fps, plus, with audio set, an Opus track of one second of silence.
func testWebMMedia(t *testing.T, audio bool) ([]testWebMTrack, []testWebMBlock) {
	t.Helper()
	const width, height, frames = 64, 48, 30
	encoder, err := govpx.NewVP9Encoder(govpx.VP9EncoderOptions{Width: width, Height: height, FPS: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	tracks := []testWebMTrack{{trackType: 1, codecID: "V_VP9", width: width, height: height}}
	src := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	for i := range src.Cb {
		src.Cb[i], src.Cr[i] = 128, 128
	}
	var blocks []testWebMBlock
	packet := make([]byte, 64<<10)
	for index := range frames {
		for i := range src.Y {
			src.Y[i] = byte(40 + (i%width+3*index)%160)
		}
		n, err := encoder.EncodeInto(src, packet)
		if err != nil || n <= 0 {
			t.Fatalf("encode frame %d: %v", index, err)
		}
		blocks = append(blocks, testWebMBlock{track: 0, time: time.Duration(index) * time.Second / 30 / time.Millisecond * time.Millisecond, keyframe: index == 0, data: slices.Clone(packet[:n])})
	}
	if audio {
		tracks = append(tracks, testWebMTrack{trackType: 2, codecID: "A_OPUS", private: testOpusHead})
		// A 20 ms CELT frame that decodes to silence.
		for index := range 50 {
			blocks = append(blocks, testWebMBlock{track: 1, time: time.Duration(index) * 20 * time.Millisecond, keyframe: true, data: []byte{0xf8, 0xff, 0xfe}})
		}
	}
	return tracks, blocks
}

// withTestSubtitles inserts subtitle tracks at position and returns the
// tracks and the blocks, whose track numbers follow the insertion.
func withTestSubtitles(tracks []testWebMTrack, blocks []testWebMBlock, position int, subtitles []testWebMTrack, cues [][]testWebMBlock) ([]testWebMTrack, []testWebMBlock) {
	tracks = slices.Insert(slices.Clone(tracks), position, subtitles...)
	moved := make([]testWebMBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.track >= position {
			block.track += len(subtitles)
		}
		moved = append(moved, block)
	}
	for i, list := range cues {
		for _, cue := range list {
			cue.track = position + i
			moved = append(moved, cue)
		}
	}
	return tracks, moved
}

// WebM files with subtitle tracks of every codec the converter reads, and
// with the subtitles before or among the media tracks, probe and decode like
// the same file without them.
func TestWebMWithSubtitleTracksProbesAndDecodes(t *testing.T) {
	media, mediaBlocks := testWebMMedia(t, true)
	plain := writeTestWebM(t, "plain.webm", media, mediaBlocks, time.Second)
	cue := func(at time.Duration, text string) testWebMBlock {
		return testWebMBlock{time: at, duration: 500 * time.Millisecond, data: []byte(text)}
	}
	vtt := testWebMTrack{trackType: 0x11, codecID: "D_WEBVTT/SUBTITLES", language: "jpn", languageBCP47: "ja", name: "日本語"}
	matroskaVTT := testWebMTrack{trackType: 0x11, codecID: "S_TEXT/WEBVTT", language: "eng", name: "English"}
	srt := testWebMTrack{trackType: 0x11, codecID: "S_TEXT/UTF8", language: "ger"}
	for _, tc := range []struct {
		name      string
		position  int
		subtitles []testWebMTrack
		cues      [][]testWebMBlock
	}{
		{"WebVTT after the media", 2, []testWebMTrack{vtt}, [][]testWebMBlock{{cue(100*time.Millisecond, "\n\nこんにちは")}}},
		{"Matroska WebVTT", 2, []testWebMTrack{matroskaVTT}, [][]testWebMBlock{{cue(100*time.Millisecond, "Hello")}}},
		{"SubRip", 2, []testWebMTrack{srt}, [][]testWebMBlock{{cue(100*time.Millisecond, "Hallo")}}},
		{"subtitles first", 0, []testWebMTrack{vtt}, [][]testWebMBlock{{cue(100*time.Millisecond, "\n\n字幕")}}},
		{"between video and audio", 1, []testWebMTrack{srt}, [][]testWebMBlock{{cue(0, "x")}}},
		{"several subtitle tracks", 2, []testWebMTrack{vtt, matroskaVTT, srt}, [][]testWebMBlock{{cue(0, "\n\nあ")}, {cue(200*time.Millisecond, "b")}, {cue(400*time.Millisecond, "c")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracks, blocks := withTestSubtitles(media, mediaBlocks, tc.position, tc.subtitles, tc.cues)
			path := writeTestWebM(t, "subtitles.webm", tracks, blocks, time.Second)
			info, err := ProbeMedia(path)
			if err != nil {
				t.Fatalf("ProbeMedia: %v", err)
			}
			if len(info.Tracks) != len(tracks) {
				t.Fatalf("tracks = %d, want %d", len(info.Tracks), len(tracks))
			}
			for i, track := range info.Tracks {
				if track.Index != i {
					t.Fatalf("track %d has index %d", i, track.Index)
				}
				if i < tc.position || i >= tc.position+len(tc.subtitles) {
					continue
				}
				want := tc.subtitles[i-tc.position]
				wantLanguage := want.languageBCP47
				if wantLanguage == "" {
					wantLanguage = want.language
				}
				if track.Kind != TrackKindSubtitle || track.Codec != want.codecID || track.Language != wantLanguage || track.Name != want.name || !SupportedSubtitleCodec(track.Codec) {
					t.Fatalf("subtitle track %d = %+v, want codec %q language %q name %q", i, track, want.codecID, wantLanguage, want.name)
				}
			}
			ctx := context.Background()
			var frames, wantFrames, samples, wantSamples int
			if err := DecodeWebMVideo(ctx, path, nil, func(DecodedVideoFrame) error { frames++; return nil }); err != nil {
				t.Fatalf("DecodeWebMVideo: %v", err)
			}
			if err := DecodeWebMVideo(ctx, plain, nil, func(DecodedVideoFrame) error { wantFrames++; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := DecodeWebMAudio(ctx, path, nil, func(chunk PCMChunk) error { samples += len(chunk.Samples); return nil }); err != nil {
				t.Fatalf("DecodeWebMAudio: %v", err)
			}
			if err := DecodeWebMAudio(ctx, plain, nil, func(chunk PCMChunk) error { wantSamples += len(chunk.Samples); return nil }); err != nil {
				t.Fatal(err)
			}
			if frames != wantFrames || samples != wantSamples || frames == 0 || samples == 0 {
				t.Fatalf("decoded %d frames and %d samples, want %d and %d", frames, samples, wantFrames, wantSamples)
			}
			times, err := ExportWebMVideoFrameTimes(path, nil)
			if err != nil || len(times) != wantFrames {
				t.Fatalf("frame times: %d, %v", len(times), err)
			}
		})
	}
}

// ReadSubtitleCues returns the plain text of the cues of each WebM subtitle
// codec on the block timeline.
func TestReadSubtitleCuesFromWebM(t *testing.T) {
	media, mediaBlocks := testWebMMedia(t, false)
	for _, tc := range []struct {
		name  string
		codec string
		cues  []testWebMBlock
		want  []SubtitleCue
	}{
		{
			name:  "WebM WebVTT",
			codec: "D_WEBVTT/SUBTITLES",
			cues: []testWebMBlock{
				{time: 100 * time.Millisecond, duration: 900 * time.Millisecond, data: []byte("\n\nFish &amp; chips &lt;3\n<v Bob>&#x3042;<b>bold</b></v>")},
				{time: 500 * time.Millisecond, duration: 300 * time.Millisecond, data: []byte("cue-1\nline:0 align:start\n<ruby>漢字<rt>かんじ</rt></ruby>です")},
				{time: 600 * time.Millisecond, duration: 100 * time.Millisecond, data: []byte("\n\n  ")},
			},
			want: []SubtitleCue{
				{100 * time.Millisecond, time.Second, "Fish & chips <3\nあbold"},
				{500 * time.Millisecond, 800 * time.Millisecond, "漢字です"},
			},
		},
		{
			name:  "Matroska WebVTT",
			codec: "S_TEXT/WEBVTT",
			cues:  []testWebMBlock{{time: 200 * time.Millisecond, duration: 300 * time.Millisecond, data: []byte("<i>two</i>\r\nlines&nbsp;here")}},
			want:  []SubtitleCue{{200 * time.Millisecond, 500 * time.Millisecond, "two\nlines here"}},
		},
		{
			name:  "SubRip",
			codec: "S_TEXT/UTF8",
			cues: []testWebMBlock{
				{time: 0, duration: 400 * time.Millisecond, data: []byte("{\\an8}<font color=\"#ff0000\">Red</font> & <u>under</u> <3")},
				// Without a duration a cue lasts until the next one starts.
				{time: 600 * time.Millisecond, data: []byte("open")},
				{time: 900 * time.Millisecond, duration: 100 * time.Millisecond, data: []byte("last")},
			},
			want: []SubtitleCue{
				{0, 400 * time.Millisecond, "Red & under <3"},
				{600 * time.Millisecond, 900 * time.Millisecond, "open"},
				{900 * time.Millisecond, time.Second, "last"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracks, blocks := withTestSubtitles(media, mediaBlocks, 1, []testWebMTrack{{trackType: 0x11, codecID: tc.codec}}, [][]testWebMBlock{tc.cues})
			path := writeTestWebM(t, "cues.webm", tracks, blocks, time.Second)
			cues, err := ReadSubtitleCues(path, 1)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cues, tc.want) {
				t.Fatalf("cues = %q, want %q", cues, tc.want)
			}
		})
	}
	path := writeTestWebM(t, "none.webm", media, mediaBlocks, time.Second)
	if _, err := ReadSubtitleCues(path, 0); err == nil {
		t.Fatal("ReadSubtitleCues accepted a video track")
	}
}
