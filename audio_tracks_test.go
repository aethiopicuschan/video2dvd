package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// testMultiAudioWebM writes a WebM file with VP9 video, the given Opus
// tracks (all silent) and the given subtitle tracks, in that order.
func testMultiAudioWebM(t *testing.T, audio []testWebMTrack, subtitles []testWebMTrack) string {
	t.Helper()
	media, blocks := testWebMMedia(t, false)
	tracks := slices.Clone(media)
	for i, track := range audio {
		track.trackType, track.codecID, track.private = 2, "A_OPUS", testOpusHead
		tracks = append(tracks, track)
		for k := range 50 {
			blocks = append(blocks, testWebMBlock{track: 1 + i, time: time.Duration(k) * 20 * time.Millisecond, keyframe: true, data: []byte{0xf8, 0xff, 0xfe}})
		}
	}
	for i, track := range subtitles {
		track.trackType, track.codecID = 0x11, "D_WEBVTT/SUBTITLES"
		tracks = append(tracks, track)
		blocks = append(blocks, testWebMBlock{track: 1 + len(audio) + i, time: 100 * time.Millisecond, duration: 500 * time.Millisecond, data: []byte("\n\n" + track.name)})
	}
	return writeTestWebM(t, "title.webm", tracks, blocks, time.Second)
}

// TrackInfo.Default reports the MP4 enabled flag and the WebM FlagDefault,
// which Matroska makes 1 when it is not written.
func TestTrackInfoDefault(t *testing.T) {
	path := testMultiAudioWebM(t, []testWebMTrack{{flagDefault: "0"}, {flagDefault: "1"}, {}}, nil)
	info, err := ProbeMedia(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []bool
	for _, track := range info.Tracks[1:] {
		got = append(got, track.Default)
	}
	if !slices.Equal(got, []bool{false, true, true}) {
		t.Fatalf("WebM defaults %v", got)
	}
	mp4, err := ProbeMedia(writeTestTX3GMP4(t))
	if err != nil {
		t.Fatal(err)
	}
	// The test file's track headers have no flags set.
	if mp4.Tracks[0].Default {
		t.Fatal("a disabled MP4 track is reported as default")
	}
}

// Without a choice, the audio track that carries a default mark is played,
// and IncludeAllAudio puts it first; a WebM track without FlagDefault is not
// marked. Subtitles follow the same order.
func TestDefaultMarksChooseTracks(t *testing.T) {
	path := testMultiAudioWebM(t,
		[]testWebMTrack{{language: "eng", flagDefault: "0"}, {language: "jpn", flagDefault: "1"}, {language: "fre"}},
		[]testWebMTrack{{language: "eng", name: "en", flagDefault: "0"}, {language: "jpn", name: "ja", flagDefault: "1"}})
	info, err := ProbeMedia(path)
	if err != nil {
		t.Fatal(err)
	}
	if track, ok := ExportFirstSupportedAudioTrack(info, nil); !ok || track.Index != 2 {
		t.Fatalf("default audio track %d, want 2", track.Index)
	}
	tracks, err := ExportSelectInputAudioTracks(info, nil, true)
	if err != nil || !slices.Equal(tracks, []int{2, 1, 3}) {
		t.Fatalf("all audio tracks %v (%v), want [2 1 3]", tracks, err)
	}
	subtitles, err := ExportSelectInputSubtitleTracks(info, nil, true)
	if err != nil || !slices.Equal(subtitles, []int{5, 4}) {
		t.Fatalf("subtitle tracks %v (%v), want [5 4]", subtitles, err)
	}
	// ffmpeg leaves FlagDefault out for the default track and writes 0 for
	// the others.
	ffmpeg := testMultiAudioWebM(t, []testWebMTrack{{flagDefault: "0"}, {}}, []testWebMTrack{{name: "a", flagDefault: "0"}, {name: "b"}})
	if info, err = ProbeMedia(ffmpeg); err != nil {
		t.Fatal(err)
	}
	if tracks, _ := ExportSelectInputAudioTracks(info, nil, true); !slices.Equal(tracks, []int{2, 1}) {
		t.Fatalf("ffmpeg-style audio tracks %v", tracks)
	}
	if subtitles, _ := ExportSelectInputSubtitleTracks(info, nil, true); !slices.Equal(subtitles, []int{4, 3}) {
		t.Fatalf("ffmpeg-style subtitle tracks %v", subtitles)
	}
	// Without explicit marks the order is the track order.
	plain := testMultiAudioWebM(t, []testWebMTrack{{}, {}}, []testWebMTrack{{name: "a"}, {name: "b"}})
	if info, err = ProbeMedia(plain); err != nil {
		t.Fatal(err)
	}
	if tracks, _ := ExportSelectInputAudioTracks(info, nil, true); !slices.Equal(tracks, []int{1, 2}) {
		t.Fatalf("unmarked audio tracks %v", tracks)
	}
	if subtitles, _ := ExportSelectInputSubtitleTracks(info, nil, true); !slices.Equal(subtitles, []int{3, 4}) {
		t.Fatalf("unmarked subtitle tracks %v", subtitles)
	}
}

// MediaInput.ShowSubtitles wins over ShowDefaultSubtitles, which wins over
// ShowSubtitles.
func TestSubtitleDisplayPrecedence(t *testing.T) {
	marked := testMultiAudioWebM(t, []testWebMTrack{{}}, []testWebMTrack{{name: "ja", flagDefault: "1"}})
	unmarked := testMultiAudioWebM(t, []testWebMTrack{{}}, []testWebMTrack{{name: "en", flagDefault: "0"}})
	yes, no := true, false
	for _, tc := range []struct {
		name    string
		input   MediaInput
		options Options
		shown   bool
	}{
		{"options only", MediaInput{VideoPath: unmarked}, Options{ShowSubtitles: true}, true},
		{"default mark shown", MediaInput{VideoPath: marked}, Options{ShowDefaultSubtitles: true}, true},
		{"no default mark", MediaInput{VideoPath: unmarked}, Options{ShowDefaultSubtitles: true, ShowSubtitles: true}, false},
		{"input hides", MediaInput{VideoPath: marked, ShowSubtitles: &no}, Options{ShowDefaultSubtitles: true, ShowSubtitles: true}, false},
		{"input shows", MediaInput{VideoPath: unmarked, ShowSubtitles: &yes}, Options{ShowDefaultSubtitles: true}, true},
	} {
		tc.options.IncludeSubtitles = true
		files := convertTestDVD(t, []MediaInput{tc.input}, tc.options)
		pgc := testTitlePGC(files["VTS_01_0.IFO"])
		command := pgc[binary.BigEndian.Uint16(pgc[0xe4:])+8:][:8]
		if shown := command[4]&0x40 != 0; command[0] != 0x51 || shown != tc.shown {
			t.Fatalf("%s: SetSTN % x, want shown %v", tc.name, command, tc.shown)
		}
	}
}

// Invalid audio track lists are refused before anything is encoded.
func TestConvertRejectsInvalidAudioTracks(t *testing.T) {
	path := testMultiAudioWebM(t, []testWebMTrack{{}, {}}, []testWebMTrack{{name: "x"}})
	five := testMultiAudioWebM(t, make([]testWebMTrack, 5), nil)
	converter, err := New(NewInProcessEncoder(), NewInProcessAuthorer())
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	for _, tc := range []struct {
		name    string
		input   MediaInput
		options Options
	}{
		{"video track", MediaInput{AudioTracks: []int{0}}, Options{}},
		{"subtitle track", MediaInput{AudioTracks: []int{3}}, Options{}},
		{"out of range", MediaInput{AudioTracks: []int{7}}, Options{}},
		{"twice", MediaInput{AudioTracks: []int{1, 1}}, Options{}},
		{"empty", MediaInput{AudioTracks: []int{}}, Options{}},
		{"nine", MediaInput{AudioTracks: slices.Repeat([]int{1, 2}, 5)[:9]}, Options{}},
		{"with AudioTrackIndex", MediaInput{AudioTracks: []int{1}, AudioTrackIndex: &one}, Options{}},
		{"five LPCM streams", MediaInput{VideoPath: five}, Options{IncludeAllAudio: true, AudioFormat: AudioFormatLPCM}},
		{"AC-3 bitrate", MediaInput{}, Options{AC3Bitrate: 200_000}},
	} {
		if tc.input.VideoPath == "" {
			tc.input.VideoPath = path
		}
		if err := converter.ConvertSources(context.Background(), []MediaInput{tc.input}, filepath.Join(t.TempDir(), "disc"), tc.options); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// With AudioFormatLPCM several streams stay LPCM, on substreams 0xa0 and
// up with their languages, and the video bitrate is lowered to make room.
func TestConvertSeveralLPCMStreams(t *testing.T) {
	path := testMultiAudioWebM(t, []testWebMTrack{{language: "jpn", languageBCP47: "ja"}, {language: "eng"}}, nil)
	files := convertTestDVD(t, []MediaInput{{VideoPath: path}}, Options{IncludeAllAudio: true, AudioFormat: AudioFormatLPCM})
	ifo := files["VTS_01_0.IFO"]
	want := []byte{0, 2, 0x84, 0x01, 'j', 'a', 0, 0, 0, 0, 0x84, 0x01, 'e', 'n', 0, 0, 0, 0}
	if got := ifo[0x202 : 0x202+len(want)]; !bytes.Equal(got, want) {
		t.Fatalf("audio attributes % x, want % x", got, want)
	}
	if control := testTitlePGC(ifo)[0x0c:0x10]; !bytes.Equal(control, []byte{0x80, 0, 0x81, 0}) {
		t.Fatalf("audio control % x", control)
	}
	substreams := map[byte]int{}
	var bitRate uint32
	for _, pack := range parseTestVOB(t, files["VTS_01_1.VOB"]) {
		if pack.streamID == 0xbd {
			substreams[pack.payload[0]]++
		}
		if i := bytes.Index(pack.payload, []byte{0, 0, 1, 0xb3}); pack.streamID == 0xe0 && i >= 0 && bitRate == 0 {
			header := pack.payload[i+4:]
			bitRate = (uint32(header[4])<<10 | uint32(header[5])<<2 | uint32(header[6])>>6) * 400
		}
	}
	if substreams[0xa0] == 0 || substreams[0xa0] != substreams[0xa1] || len(substreams) != 2 {
		t.Fatalf("audio packs per substream %v", substreams)
	}
	// 9.036 Mbit/s less two LPCM streams.
	if bitRate != 5_964_000 {
		t.Fatalf("video bitrate %d, want 5964000", bitRate)
	}
}

func TestEstimateDVDBytes(t *testing.T) {
	hour := []TitleEstimate{{Duration: time.Hour}}
	one, err := EstimateDVDBytes(hour, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 7.5 Mbit/s video and 1.536 Mbit/s LPCM for an hour, plus overhead.
	if want := int64(9_036_000 / 8 * 3600); one < want || one > want*107/100 {
		t.Fatalf("one hour estimated at %d bytes, want about %d", one, want)
	}
	two, err := EstimateDVDBytes([]TitleEstimate{{Duration: time.Hour, AudioStreams: 2}}, Options{})
	if err != nil || two >= one {
		t.Fatalf("two AC-3 streams estimated at %d (%v), want less than one LPCM stream's %d", two, err, one)
	}
	lpcm, err := EstimateDVDBytes([]TitleEstimate{{Duration: time.Hour, AudioStreams: 2}}, Options{AudioFormat: AudioFormatLPCM})
	if err != nil || lpcm != one {
		t.Fatalf("two LPCM streams estimated at %d (%v), want the %d of one, as the video gives way", lpcm, err, one)
	}
	for _, tc := range []struct {
		titles  []TitleEstimate
		options Options
	}{
		{[]TitleEstimate{{}}, Options{}},
		{[]TitleEstimate{{Duration: time.Hour, AudioStreams: 9}}, Options{}},
		{[]TitleEstimate{{Duration: time.Hour, AudioStreams: 5}}, Options{AudioFormat: AudioFormatLPCM}},
		{hour, Options{AC3Bitrate: 1}},
	} {
		if _, err := EstimateDVDBytes(tc.titles, tc.options); err == nil {
			t.Errorf("estimate of %+v with %+v accepted", tc.titles, tc.options)
		}
	}
}

// IncludeAllAudio with the default AudioFormat codes several streams as
// AC-3 on substreams 0x80 and up, labelled with their languages, and keeps
// the full video bitrate.
func TestConvertSeveralAC3Streams(t *testing.T) {
	path := testMultiAudioWebM(t, []testWebMTrack{{language: "eng", flagDefault: "0"}, {language: "jpn", languageBCP47: "ja-JP", flagDefault: "1"}}, nil)
	files := convertTestDVD(t, []MediaInput{{VideoPath: path}}, Options{IncludeAllAudio: true, AC3Bitrate: 256_000})
	ifo := files["VTS_01_0.IFO"]
	// The default-marked Japanese track comes first.
	want := []byte{0, 2, 0x04, 0x01, 'j', 'a', 0, 0, 0, 0, 0x04, 0x01, 'e', 'n', 0, 0, 0, 0}
	if got := ifo[0x202 : 0x202+len(want)]; !bytes.Equal(got, want) {
		t.Fatalf("audio attributes % x, want % x", got, want)
	}
	if control := testTitlePGC(ifo)[0x0c:0x10]; !bytes.Equal(control, []byte{0x80, 0, 0x81, 0}) {
		t.Fatalf("audio control % x", control)
	}
	frames := map[byte]int{}
	var bitRate uint32
	for _, pack := range parseTestVOB(t, files["VTS_01_1.VOB"]) {
		if pack.streamID == 0xbd {
			if pack.payload[0]&0xf8 != 0x80 {
				t.Fatalf("audio substream %#x", pack.payload[0])
			}
			frames[pack.payload[0]] += int(pack.payload[1])
		}
		if i := bytes.Index(pack.payload, []byte{0, 0, 1, 0xb3}); pack.streamID == 0xe0 && i >= 0 && bitRate == 0 {
			header := pack.payload[i+4:]
			bitRate = (uint32(header[4])<<10 | uint32(header[5])<<2 | uint32(header[6])>>6) * 400
		}
	}
	// One second of audio behind the 256-sample silent block: 32 frames
	// of 1536 samples.
	if frames[0x80] != 32 || frames[0x81] != 32 {
		t.Fatalf("syncframes per substream %v, want 32 each", frames)
	}
	if bitRate != 7_500_000 {
		t.Fatalf("video bitrate %d, want 7500000", bitRate)
	}
}

// Tracks the decoders cannot downmix, such as 5.1 AAC, are passed over by
// the automatic choices instead of failing the input; an explicit request
// for one still fails.
func TestAutomaticAudioChoiceSkipsUndecodableTracks(t *testing.T) {
	stereo := TrackInfo{Index: 1, Kind: TrackKindAudio, Codec: "mp4a", Channels: 2, CodecConfig: []byte{0x11, 0x90}}
	surround := TrackInfo{Index: 2, Kind: TrackKindAudio, Codec: "mp4a", Channels: 6, CodecConfig: []byte{0x11, 0xb0}, Default: true}
	info := MP4Info{Tracks: []TrackInfo{{Index: 0, Kind: TrackKindVideo, Codec: "avc1"}, stereo, surround}}
	if track, ok := ExportFirstSupportedAudioTrack(info, nil); !ok || track.Index != 1 {
		t.Fatalf("chose track %d, want the stereo track 1 over the marked 5.1 track", track.Index)
	}
	if tracks, err := ExportSelectInputAudioTracks(info, nil, true); err != nil || !slices.Equal(tracks, []int{1}) {
		t.Fatalf("all tracks %v (%v), want [1]", tracks, err)
	}
	if _, err := ExportSelectInputAudioTracks(info, []int{2}, false); err == nil {
		t.Fatal("an explicit request for 5.1 AAC was accepted")
	}
}

// IncludeAllAudio leaves an input with AudioTrackIndex its one track.
func TestIncludeAllAudioKeepsAudioTrackIndex(t *testing.T) {
	path := testMultiAudioWebM(t, []testWebMTrack{{language: "jpn"}, {language: "eng"}}, nil)
	two := 2
	files := convertTestDVD(t, []MediaInput{{VideoPath: path, AudioTrackIndex: &two}}, Options{IncludeAllAudio: true})
	if got := files["VTS_01_0.IFO"][0x202:0x206]; !bytes.Equal(got, []byte{0, 1, 0x80, 0x01}) {
		t.Fatalf("audio attributes % x, want one LPCM stream", got)
	}
}

// AC-3 that is not DVD-Video audio is refused before it is multiplexed:
// more than 448 kbit/s, or a reduced sample rate (bsid 9 and 10).
func TestAuthorRejectsAC3OutsideDVDVideo(t *testing.T) {
	profile := testSubtitleProfile(t, Options{})
	title, streams := testAC3Title(t, profile, []int{448}, []string{"ja"})
	for name, patch := range map[string]func([]byte){
		"512 kbit/s": func(frame []byte) { frame[4] = 32 },
		"bsid 9":     func(frame []byte) { frame[5] = 9 << 3 },
	} {
		data := slices.Clone(streams[0])
		patch(data)
		if err := os.WriteFile(title.Audio[0].Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewInProcessAuthorer().Author(context.Background(), []EncodedTitle{title}, t.TempDir(), profile); err == nil {
			t.Errorf("%s: authored", name)
		}
	}
}
