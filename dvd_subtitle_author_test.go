package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// testSubtitleWebM writes a WebM file with VP9 video, Opus audio and three
// subtitle tracks: Japanese WebVTT with two overlapping cues, English
// SubRip, and WebVTT of no stated language. It also returns the same file
// without the subtitle tracks.
func testSubtitleWebM(t *testing.T) (withSubtitles, plain string) {
	t.Helper()
	media, blocks := testWebMMedia(t, true)
	// Both files have the same name, which the title menu shows.
	plain = writeTestWebM(t, "title.webm", media, blocks, time.Second)
	ms := time.Millisecond
	tracks, all := withTestSubtitles(media, blocks, 2, []testWebMTrack{
		{trackType: 0x11, codecID: "D_WEBVTT/SUBTITLES", language: "jpn", languageBCP47: "ja", name: "日本語"},
		{trackType: 0x11, codecID: "S_TEXT/UTF8", language: "eng"},
		{trackType: 0x11, codecID: "S_TEXT/WEBVTT", language: "und"},
	}, [][]testWebMBlock{
		{
			{time: 100 * ms, duration: 800 * ms, data: []byte("\n\nこんにちは")},
			{time: 400 * ms, duration: 200 * ms, data: []byte("\n\n重なり")},
		},
		{{time: 200 * ms, duration: 700 * ms, data: []byte("<i>Hello</i>")}},
		{{time: 0, duration: 2 * time.Second, data: []byte("whole title")}},
	})
	withSubtitles = writeTestWebM(t, "title.webm", tracks, all, time.Second)
	return withSubtitles, plain
}

// convertTestDVD converts inputs to a DVD-Video directory and returns its
// files by name.
func convertTestDVD(t *testing.T, inputs []MediaInput, options Options) map[string][]byte {
	t.Helper()
	converter, err := New(NewInProcessEncoder(), NewInProcessAuthorer())
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "disc")
	if err := converter.ConvertSources(context.Background(), inputs, output, options); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(output, "VIDEO_TS"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		files[entry.Name()] = readAuthorerTestFile(t, filepath.Join(output, "VIDEO_TS"), entry.Name())
	}
	return files
}

// testTitlePGC returns the PGC of the only title of a VTS IFO.
func testTitlePGC(ifo []byte) []byte {
	pgci := ifo[int(binary.BigEndian.Uint32(ifo[0xcc:]))*DVDVOBSectorSize:]
	return pgci[binary.BigEndian.Uint32(pgci[12:16]):]
}

// testVOBSubtitleUnit is a subtitle unit reassembled from the packs of a VOB.
type testVOBSubtitleUnit struct {
	stream      int
	pts         uint64
	firstSector int
	lastSCR     uint64
	data        []byte
}

// testVOBSubtitleUnits reassembles the subtitle units of a VOB from its
// private_stream_1 packs.
func testVOBSubtitleUnits(t *testing.T, packs []testVOBPack) []testVOBSubtitleUnit {
	t.Helper()
	var units []testVOBSubtitleUnit
	open := map[int]int{}
	for _, pack := range packs {
		if pack.streamID != 0xbd || len(pack.payload) == 0 || pack.payload[0]&0xe0 != 0x20 {
			continue
		}
		stream := int(pack.payload[0] - 0x20)
		index, ok := open[stream]
		if pack.hasPTS {
			if ok {
				t.Fatalf("stream %d unit at sector %d starts before the previous one is complete", stream, pack.sector)
			}
			units = append(units, testVOBSubtitleUnit{stream: stream, pts: pack.pts, firstSector: pack.sector})
			index = len(units) - 1
			open[stream] = index
		} else if !ok {
			t.Fatalf("stream %d continues at sector %d without a unit start", stream, pack.sector)
		}
		unit := &units[index]
		unit.data = append(unit.data, pack.payload[1:]...)
		unit.lastSCR = pack.scr
		if len(unit.data) >= 2 && len(unit.data) >= int(binary.BigEndian.Uint16(unit.data)) {
			if len(unit.data) != int(binary.BigEndian.Uint16(unit.data)) {
				t.Fatalf("stream %d unit carries more bytes than its size", stream)
			}
			delete(open, stream)
		}
	}
	if len(open) != 0 {
		t.Fatalf("units %v are incomplete", open)
	}
	return units
}

// The subtitle tracks of a WebM file become the DVD subtitle streams of its
// title: declared with their languages in the VTS IFO and its copy in the
// VMG, available in every display mode of the title PGC with the subtitle
// colours, selected by a pre-command, and multiplexed with SP_SYNCA in the
// NAV packs.
func TestConvertRecordsSubtitleStreams(t *testing.T) {
	withSubtitles, _ := testSubtitleWebM(t)
	const startTime = 27_000 + 3003 // presentation time of the first picture.
	for _, tc := range []struct {
		name    string
		options Options
	}{
		{"16:9, hidden", Options{IncludeSubtitles: true}},
		{"4:3, shown", Options{IncludeSubtitles: true, ShowSubtitles: true, Aspect: AspectRatio4x3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := convertTestDVD(t, []MediaInput{{VideoPath: withSubtitles}}, tc.options)
			ifo := files["VTS_01_0.IFO"]
			attributes := ifo[0x254 : 0x256+3*6]
			wantAttributes := []byte{0, 3, 1, 0, 'j', 'a', 0, 0, 1, 0, 'e', 'n', 0, 0, 0, 0, 0, 0, 0, 0}
			if !bytes.Equal(attributes, wantAttributes) {
				t.Fatalf("VTS subpicture attributes % x, want % x", attributes, wantAttributes)
			}
			vmg := files["VIDEO_TS.IFO"]
			table := vmg[int(binary.BigEndian.Uint32(vmg[0xd0:]))*DVDVOBSectorSize:]
			entry := table[binary.BigEndian.Uint32(table[8:12]):]
			if copied := entry[8+0x154 : 8+0x156+3*6]; !bytes.Equal(copied, wantAttributes) {
				t.Fatalf("VMG_VTS_ATRT subpicture attributes % x, want % x", copied, wantAttributes)
			}
			pgc := testTitlePGC(ifo)
			for stream := range 32 {
				control := pgc[0x1c+stream*4 : 0x20+stream*4]
				want := []byte{0, 0, 0, 0}
				if stream < 3 {
					n := byte(stream)
					want = []byte{0x80 | n, 0, 0, 0}
					if tc.options.Aspect != AspectRatio4x3 {
						want = []byte{0x80 | n, n, n, n}
					}
				}
				if !bytes.Equal(control, want) {
					t.Fatalf("subpicture stream %d control % x, want % x", stream, control, want)
				}
			}
			for i, color := range ExportDVDSubtitlePalette {
				if !bytes.Equal(pgc[0xa4+i*4:0xa8+i*4], color[:]) {
					t.Fatalf("palette entry %d is % x", i, pgc[0xa4+i*4:0xa8+i*4])
				}
			}
			commands := pgc[binary.BigEndian.Uint16(pgc[0xe4:]):]
			setSTN := []byte{0x51, 0, 0, 0, 0x80, 0, 0, 0}
			if tc.options.ShowSubtitles {
				setSTN[4] |= 0x40
			}
			if binary.BigEndian.Uint16(commands[0:2]) != 1 || !bytes.Equal(commands[8:16], setSTN) {
				t.Fatalf("pre-commands: %d, first % x, want SetSTN % x", binary.BigEndian.Uint16(commands[0:2]), commands[8:16], setSTN)
			}

			packs := parseTestVOB(t, files["VTS_01_1.VOB"])
			units := testVOBSubtitleUnits(t, packs)
			wantPTS := [][]uint64{
				{startTime + 9_000, startTime + 36_000, startTime + 54_000},
				{startTime + 18_000},
				{startTime},
			}
			var navSectors []int
			for _, pack := range packs {
				if pack.nav {
					navSectors = append(navSectors, pack.sector)
				}
			}
			vobuOf := func(sector int) int {
				index, found := slices.BinarySearch(navSectors, sector)
				if !found {
					index--
				}
				return index
			}
			type shown struct {
				pts, end uint64
				vobu     int
			}
			perStream := make([][]shown, 3)
			for _, unit := range units {
				decoded := decodeTestSubpictureUnit(t, unit.data)
				if unit.lastSCR/300+600 > unit.pts {
					t.Fatalf("stream %d unit at %d arrives at %d", unit.stream, unit.pts, unit.lastSCR/300)
				}
				perStream[unit.stream] = append(perStream[unit.stream], shown{unit.pts, unit.pts + uint64(decoded.hide)*1024, vobuOf(unit.firstSector)})
			}
			for stream, want := range wantPTS {
				var got []uint64
				for _, unit := range perStream[stream] {
					got = append(got, unit.pts)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("stream %d units at %v, want %v", stream, got, want)
				}
			}
			if len(navSectors) < 2 {
				t.Fatalf("title has %d VOBUs", len(navSectors))
			}
			for index, sector := range navSectors {
				nav := packs[sector].raw
				start, end := uint64(binary.BigEndian.Uint32(nav[0x39:])), uint64(binary.BigEndian.Uint32(nav[0x3d:]))
				for stream := range 32 {
					got := binary.BigEndian.Uint32(nav[0x5a9+stream*4:])
					var want uint32
					if stream < 3 {
						// The rule of dvdauthor: the unit shown at the
						// VOBU's start, else the next one shown during it.
						target := -1
						for _, unit := range perStream[stream] {
							if unit.pts <= start {
								target = -1
								if unit.end > start {
									target = unit.vobu
								}
							} else {
								if target < 0 && unit.pts < end {
									target = unit.vobu
								}
								break
							}
						}
						switch {
						case target < 0 || target == index:
							want = 0x7fffffff
						case target < index:
							want = uint32(sector-navSectors[target]) | 0x80000000
						default:
							want = uint32(navSectors[target] - sector)
						}
					}
					if got != want {
						t.Fatalf("VOBU %d SP_SYNCA of stream %d = %#x, want %#x", index, stream, got, want)
					}
				}
			}
		})
	}
}

// Without subtitles the output does not change: a file's subtitle tracks
// that are not selected leave every authored file as that of the same file
// without them.
func TestConvertWithoutSubtitlesIsUnchanged(t *testing.T) {
	withSubtitles, plain := testSubtitleWebM(t)
	want := convertTestDVD(t, []MediaInput{{VideoPath: plain}}, Options{})
	for _, tc := range []struct {
		name    string
		input   MediaInput
		options Options
	}{
		{"subtitles not requested", MediaInput{VideoPath: withSubtitles}, Options{}},
		{"no subtitle tracks selected", MediaInput{VideoPath: withSubtitles, SubtitleTracks: []int{}}, Options{IncludeSubtitles: true, ShowSubtitles: true}},
		{"input without subtitle tracks", MediaInput{VideoPath: plain}, Options{IncludeSubtitles: true}},
	} {
		got := convertTestDVD(t, []MediaInput{tc.input}, tc.options)
		if len(got) != len(want) {
			t.Fatalf("%s: %d files, want %d", tc.name, len(got), len(want))
		}
		for name, data := range want {
			if !bytes.Equal(got[name], data) {
				t.Fatalf("%s: %s differs from the output without subtitles", tc.name, name)
			}
		}
	}
}

// SubtitleTracks chooses the streams and their order; tracks that are not
// readable text subtitles are refused before anything is encoded.
func TestConvertSelectsSubtitleTracks(t *testing.T) {
	withSubtitles, _ := testSubtitleWebM(t)
	files := convertTestDVD(t, []MediaInput{{VideoPath: withSubtitles, SubtitleTracks: []int{3, 2}}}, Options{})
	if got, want := files["VTS_01_0.IFO"][0x254:0x256+2*6], []byte{0, 2, 1, 0, 'e', 'n', 0, 0, 1, 0, 'j', 'a', 0, 0}; !bytes.Equal(got, want) {
		t.Fatalf("subpicture attributes % x, want % x", got, want)
	}
	converter, err := New(NewInProcessEncoder(), NewInProcessAuthorer())
	if err != nil {
		t.Fatal(err)
	}
	for _, tracks := range [][]int{{0}, {1}, {2, 2}, {9}, {-1}, slices.Repeat([]int{2}, 33)} {
		err := converter.ConvertSources(context.Background(), []MediaInput{{VideoPath: withSubtitles, SubtitleTracks: tracks}}, filepath.Join(t.TempDir(), "disc"), Options{})
		if err == nil {
			t.Fatalf("subtitle tracks %v were accepted", tracks)
		}
	}
}

// An encoder that cannot take MediaInput is refused subtitles instead of
// silently dropping them.
func TestConvertRefusesSubtitlesForLegacyEncoders(t *testing.T) {
	withSubtitles, _ := testSubtitleWebM(t)
	converter, err := New(legacyTestEncoder{}, NewInProcessAuthorer())
	if err != nil {
		t.Fatal(err)
	}
	err = converter.ConvertSources(context.Background(), []MediaInput{{VideoPath: withSubtitles}}, filepath.Join(t.TempDir(), "disc"), Options{IncludeSubtitles: true})
	if err == nil {
		t.Fatal("subtitles were accepted for an encoder that cannot write them")
	}
}

type legacyTestEncoder struct{}

func (legacyTestEncoder) Encode(ctx context.Context, inputPath, workDir string, profile Profile) (EncodedTitle, error) {
	return NewInProcessEncoder().Encode(ctx, inputPath, workDir, profile)
}

// An MP4 file with a mov_text track, as ffmpeg writes it, gets a subtitle
// stream with the track's language. The test needs ffmpeg to make the H.264
// input.
func TestConvertMP4TimedText(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "subtitles.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,200 --> 00:00:00,800\nこんにちは\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.mp4")
	command := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x48:rate=30:duration=1,format=yuv420p",
		"-i", srt, "-map", "0", "-map", "1", "-c:v", "libx264", "-c:s", "mov_text", "-metadata:s:s:0", "language=jpn", input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot write the input: %v: %s", err, output)
	}
	files := convertTestDVD(t, []MediaInput{{VideoPath: input}}, Options{IncludeSubtitles: true})
	if got, want := files["VTS_01_0.IFO"][0x254:0x256+6], []byte{0, 1, 1, 0, 'j', 'a', 0, 0}; !bytes.Equal(got, want) {
		t.Fatalf("subpicture attributes % x, want % x", got, want)
	}
	units := testVOBSubtitleUnits(t, parseTestVOB(t, files["VTS_01_1.VOB"]))
	if len(units) != 1 || units[0].stream != 0 || units[0].pts != 27_000+3003+18_000 {
		t.Fatalf("units %+v, want one at 0.2 s", units)
	}
}
