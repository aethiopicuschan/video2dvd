package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// testSubpictureUnit is a subpicture unit decoded independently of the
// encoder.
type testSubpictureUnit struct {
	pts uint64 // 90 kHz ticks from the title start.
	// show and hide are the delays of the control sequences holding
	// STA_DSP and STP_DSP, in 1024-tick units; hide is -1 without STP_DSP.
	show, hide int
	color      [2]byte
	contrast   [2]byte
	area       image.Rectangle
	// codes holds the 2-bit pixel code of every pixel of area, row by row.
	codes []byte
}

func (u testSubpictureUnit) code(x, y int) byte {
	return u.codes[(y-u.area.Min.Y)*u.area.Dx()+x-u.area.Min.X]
}

// decodeTestSubtitleStream reads a subtitle stream file.
func decodeTestSubtitleStream(t *testing.T, data []byte) []testSubpictureUnit {
	t.Helper()
	var units []testSubpictureUnit
	for len(data) > 0 {
		if len(data) < 12 {
			t.Fatalf("stream ends inside a unit header")
		}
		pts := binary.BigEndian.Uint64(data)
		size := int(binary.BigEndian.Uint16(data[8:]))
		if size > len(data)-8 {
			t.Fatalf("unit of %d bytes exceeds the stream", size)
		}
		unit := decodeTestSubpictureUnit(t, data[8:8+size])
		unit.pts = pts
		units = append(units, unit)
		data = data[8+size:]
	}
	return units
}

// decodeTestSubpictureUnit decodes one unit: its control sequences, which
// must hold the commands the subtitles use, and its two fields.
func decodeTestSubpictureUnit(t *testing.T, unit []byte) testSubpictureUnit {
	t.Helper()
	if len(unit) > ExportDVDSubtitleMaxUnitBytes || int(binary.BigEndian.Uint16(unit)) != len(unit) {
		t.Fatalf("unit size field %d, unit is %d bytes", binary.BigEndian.Uint16(unit), len(unit))
	}
	decoded := testSubpictureUnit{show: -1, hide: -1}
	var fields [2]int
	offset := int(binary.BigEndian.Uint16(unit[2:]))
	for sequences := 0; ; sequences++ {
		if sequences > 4 || offset+4 > len(unit) || offset%2 != 0 {
			t.Fatalf("control sequence at %d is invalid", offset)
		}
		delay := int(binary.BigEndian.Uint16(unit[offset:]))
		next := int(binary.BigEndian.Uint16(unit[offset+2:]))
		position := offset + 4
	commands:
		for {
			command := unit[position]
			position++
			switch command {
			case 0x01:
				decoded.show = delay
			case 0x02:
				decoded.hide = delay
			case 0x03:
				copy(decoded.color[:], unit[position:position+2])
				position += 2
			case 0x04:
				copy(decoded.contrast[:], unit[position:position+2])
				position += 2
			case 0x05:
				a := unit[position : position+6]
				decoded.area = image.Rect(int(a[0])<<4|int(a[1])>>4, int(a[3])<<4|int(a[4])>>4,
					(int(a[1])&0xf)<<8|int(a[2])+1, (int(a[4])&0xf)<<8|int(a[5])+1)
				position += 6
			case 0x06:
				fields[0], fields[1] = int(binary.BigEndian.Uint16(unit[position:])), int(binary.BigEndian.Uint16(unit[position+2:]))
				position += 4
			case 0xff:
				break commands
			default:
				t.Fatalf("unexpected control command %#x", command)
			}
		}
		if next == offset {
			break
		}
		if next < offset {
			t.Fatalf("control sequences go backwards")
		}
		offset = next
	}
	width, height := decoded.area.Dx(), decoded.area.Dy()
	if width <= 0 || height <= 0 || decoded.area.Min.Y%2 != 0 || height%2 != 0 {
		t.Fatalf("display area %v must start on an even line and have an even height", decoded.area)
	}
	decoded.codes = make([]byte, width*height)
	for field := range 2 {
		nibble := fields[field] * 2
		read := func() int {
			value := int(unit[nibble/2])
			if nibble%2 == 0 {
				value >>= 4
			}
			nibble++
			return value & 0xf
		}
		for y := field; y < height; y += 2 {
			for x := 0; x < width; {
				value := read()
				for limit := 0x4; value < limit && limit <= 0x40; limit <<= 2 {
					value = value<<4 | read()
				}
				count, code := value>>2, byte(value&3)
				if count == 0 {
					count = width - x
				}
				if x+count > width {
					t.Fatalf("run of %d pixels passes the end of a %d-pixel line", count, width)
				}
				for range count {
					decoded.codes[y*width+x] = code
					x++
				}
			}
			nibble += nibble % 2 // lines end on a byte boundary.
		}
	}
	return decoded
}

func testSubtitleProfile(t *testing.T, options Options) Profile {
	t.Helper()
	profile, err := ExportProfile(options)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// A subtitle unit shows white text with a black outline on a transparent
// background, centred at the bottom of the title-safe area, and hides it
// again before the next unit.
func TestWriteDVDSubtitleStreamDrawsOutlinedText(t *testing.T) {
	for _, options := range []Options{{}, {Aspect: AspectRatio4x3}, {Standard: VideoStandardPAL}} {
		profile := testSubtitleProfile(t, options)
		cues := []SubtitleCue{
			{Start: time.Second, End: 3 * time.Second, Text: "字幕のテスト"},
			{Start: 2500 * time.Millisecond, End: 4 * time.Second, Text: "Second cue"},
		}
		var stream bytes.Buffer
		if err := WriteDVDSubtitleStream(&stream, cues, 10*time.Second, profile); err != nil {
			t.Fatal(err)
		}
		units := decodeTestSubtitleStream(t, stream.Bytes())
		// While the cues overlap both are shown, the earlier one on top.
		wantStarts := []uint64{90_000, 225_000, 270_000}
		wantEnds := []uint64{225_000, 270_000, 360_000}
		if len(units) != len(wantStarts) {
			t.Fatalf("%d units, want %d", len(units), len(wantStarts))
		}
		safe := image.Rect(profile.FrameWidth/10, profile.FrameHeight/10, profile.FrameWidth*9/10, profile.FrameHeight*9/10)
		for i, unit := range units {
			hide := unit.pts + uint64(unit.hide)*1024
			if unit.pts != wantStarts[i] || unit.show != 0 || hide > wantEnds[i] || wantEnds[i]-hide >= 1024 {
				t.Fatalf("unit %d shown at %d (+%d) until %d, want %d until %d", i, unit.pts, unit.show, hide, wantStarts[i], wantEnds[i])
			}
			if unit.color != [2]byte{0x32, 0x10} || unit.contrast != [2]byte{0xff, 0xf0} {
				t.Fatalf("unit %d colours % x, contrast % x", i, unit.color, unit.contrast)
			}
			// The outline may reach two pixels beyond the safe area.
			if !unit.area.In(safe.Inset(-2)) || unit.area.Max.Y < profile.FrameHeight*8/10 {
				t.Fatalf("unit %d area %v is not at the bottom of the safe area %v", i, unit.area, safe)
			}
			// Lines are centred by their advance width; bold bitmap glyphs
			// start a pixel left of their cell and end with blank columns.
			if centre := unit.area.Min.X + unit.area.Max.X; centre < profile.FrameWidth-12 || centre > profile.FrameWidth+4 {
				t.Fatalf("unit %d area %v is not centred", i, unit.area)
			}
			var counts [4]int
			for y := unit.area.Min.Y; y < unit.area.Max.Y; y++ {
				for x := unit.area.Min.X; x < unit.area.Max.X; x++ {
					code := unit.code(x, y)
					counts[code]++
					if code != 1 {
						continue
					}
					// Text never touches the transparent background.
					for _, d := range []image.Point{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
						p := image.Pt(x, y).Add(d)
						if p.In(unit.area) && unit.code(p.X, p.Y) == 0 {
							t.Fatalf("unit %d text pixel %v borders the background", i, image.Pt(x, y))
						}
					}
				}
			}
			if counts[1] == 0 || counts[2] == 0 || counts[0] == 0 || counts[3] != 0 {
				t.Fatalf("unit %d pixel counts %v: want text, outline and background and, with bitmap fonts, no edge pixels", i, counts)
			}
		}
		// The ASCII glyphs of the second line end two lines higher than
		// Japanese ones.
		if units[1].area.Dy() <= units[0].area.Dy()+30 || units[0].area.Max.Y-units[1].area.Max.Y > 4 {
			t.Fatalf("two lines %v should rise above one line %v from the same bottom", units[1].area, units[0].area)
		}
	}
	palette := ExportDVDSubtitlePalette
	if palette[1] != [4]byte{0, 235, 128, 128} || palette[2] != [4]byte{0, 16, 128, 128} {
		t.Fatalf("palette %v: text must be white and the outline black", palette[:4])
	}
}

// Cues are cut to the title, too short ones are left out, and a subtitle
// longer than one control sequence can wait is shown by several units.
func TestWriteDVDSubtitleStreamCutsAndSplitsCues(t *testing.T) {
	profile := testSubtitleProfile(t, Options{})
	const title = 30 * time.Minute
	cues := []SubtitleCue{
		{Start: -2 * time.Second, End: -time.Second, Text: "before"},
		{Start: -time.Second, End: 20 * time.Minute, Text: "long"},
		{Start: 21 * time.Minute, End: 21*time.Minute + 5*time.Millisecond, Text: "too short"},
		{Start: 29 * time.Minute, End: 31 * time.Minute, Text: "end"},
		{Start: 31 * time.Minute, End: 32 * time.Minute, Text: "after"},
	}
	var stream bytes.Buffer
	if err := WriteDVDSubtitleStream(&stream, cues, title, profile); err != nil {
		t.Fatal(err)
	}
	units := decodeTestSubtitleStream(t, stream.Bytes())
	maxTicks := uint64(ExportDVDSubtitleMaxDelay) * 1024
	var covered []uint64
	previousEnd := uint64(0)
	for i, unit := range units {
		if unit.pts < previousEnd {
			t.Fatalf("unit %d at %d starts before unit %d ends at %d", i, unit.pts, i-1, previousEnd)
		}
		previousEnd = unit.pts + uint64(unit.hide)*1024
		covered = append(covered, unit.pts, previousEnd)
	}
	// 20 minutes from zero take two units, the last minute one.
	want := []uint64{0, maxTicks, maxTicks, 1200 * 90_000 / 1024 * 1024, 29 * 60 * 90_000, 30 * 60 * 90_000}
	if len(units) != 3 || covered[0] != want[0] || covered[1] != want[1] || covered[2] != want[2] || covered[3] > 1200*90_000 || 1200*90_000-covered[3] >= 1024 || covered[4] != want[4] || covered[5] > want[5] || want[5]-covered[5] >= 1024 {
		t.Fatalf("units cover %v, want about %v", covered, want)
	}
	if err := WriteDVDSubtitleStream(&stream, cues, 0, profile); err == nil {
		t.Fatal("a title without duration was accepted")
	}
}

// Overlapping cues are shown together while they overlap, the same text
// once, and only cues with text that end after they start are shown.
func TestFlattenSubtitleCues(t *testing.T) {
	s := time.Second
	got := ExportFlattenSubtitleCues([]SubtitleCue{
		{Start: 4 * s, End: 6 * s, Text: "C"},
		{Start: 0, End: 2 * s, Text: "A"},
		{Start: 1 * s, End: 3 * s, Text: "B"},
		{Start: 1 * s, End: 2 * s, Text: "A"},
		{Start: 5 * s, End: 5 * s, Text: "empty time"},
		{Start: 7 * s, End: 8 * s, Text: ""},
		{Start: 6 * s, End: 7 * s, Text: "C"},
	})
	want := []SubtitleCue{
		{Start: 0, End: 1 * s, Text: "A"},
		{Start: 1 * s, End: 2 * s, Text: "A\nB"},
		{Start: 2 * s, End: 3 * s, Text: "B"},
		{Start: 4 * s, End: 7 * s, Text: "C"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// Lines break at spaces and between Japanese characters, never before
// closing punctuation, into lines of about equal width.
func TestWrapSubtitleLine(t *testing.T) {
	profile := testSubtitleProfile(t, Options{})
	const maxWidth = 576
	long := "これは三十文字を超える長い日本語の字幕で、画面の幅に収まるように折り返されることを確かめるための文です。"
	lines, widths := ExportWrapSubtitleLine(profile, 2, long, maxWidth)
	if len(lines) != 3 || strings.Join(lines, "") != long {
		t.Fatalf("lines = %q", lines)
	}
	for i, line := range lines {
		if widths[i] > maxWidth {
			t.Fatalf("line %q is %d pixels wide", line, widths[i])
		}
		if first := []rune(line)[0]; strings.ContainsRune("、。」）ょっ", first) {
			t.Fatalf("line %q starts with %q", line, first)
		}
	}
	if slices.Max(widths)-slices.Min(widths) > 3*32 {
		t.Fatalf("widths %v are uneven", widths)
	}
	// A break after punctuation is preferred to an even one.
	lines, _ = ExportWrapSubtitleLine(profile, 2, "こんにちは、世界。これは字幕のテストです。", maxWidth)
	if !slices.Equal(lines, []string{"こんにちは、世界。", "これは字幕のテストです。"}) {
		t.Fatalf("lines = %q", lines)
	}
	english := "The quick brown fox jumps over the lazy dog, again and again and again."
	lines, widths = ExportWrapSubtitleLine(profile, 2, english, maxWidth)
	if len(lines) != 4 || strings.Join(lines, " ") != strings.ToUpper(english) {
		t.Fatalf("lines = %q, want the words of %q", lines, english)
	}
	for i := range lines {
		if widths[i] > maxWidth {
			t.Fatalf("line %q is %d pixels wide", lines[i], widths[i])
		}
	}
	word := strings.Repeat("W", 60)
	lines, _ = ExportWrapSubtitleLine(profile, 2, word, maxWidth)
	if len(lines) != 3 || strings.Join(lines, "") != word {
		t.Fatalf("a long word was broken into %q", lines)
	}
	// Text that needs more than four lines is drawn at half the size.
	if lines, scale := ExportSubtitleLayout(profile, strings.Repeat(long, 2)); scale != 1 || len(lines) > 8 {
		t.Fatalf("%d lines at scale %d", len(lines), scale)
	}
	if _, scale := ExportSubtitleLayout(profile, long); scale != 2 {
		t.Fatalf("three lines drawn at scale %d", scale)
	}
}

func TestDVDLanguageCode(t *testing.T) {
	for language, want := range map[string]string{
		"ja": "ja", "jpn": "ja", "ja-JP": "ja", "JPN": "ja", "en": "en", "eng": "en", "en-US": "en",
		"ger": "de", "deu": "de", "fre": "fr", "fra": "fr", "chi": "zh", "zho": "zh", "zh-Hant": "zh",
		"cmn": "zh", "yue": "zh", "pt-BR": "pt", "fil": "tl", "iw": "he", "es_419": "es",
		"und": "", "mul": "", "zxx": "", "": "", "x-klingon": "", "qaa": "", "e": "",
	} {
		if got := DVDLanguageCode(language); got != want {
			t.Errorf("DVDLanguageCode(%q) = %q, want %q", language, got, want)
		}
	}
}

func TestTX3GSampleText(t *testing.T) {
	style := mp4TestBox("styl", []byte{0, 1, 0, 0, 0, 5, 0, 1, 0, 0, 0x12, 0xff, 0xff, 0xff, 0xff})
	for _, tc := range []struct {
		sample []byte
		want   string
	}{
		{append([]byte{0, 5}, append([]byte("Hello"), style...)...), "Hello"},
		{append([]byte{0, 6}, "字幕"...), "字幕"},
		{[]byte{0, 6, 0xfe, 0xff, 0x5b, 0x57, 0x5e, 0x55}, "字幕"},
		{[]byte{0, 6, 0xff, 0xfe, 0x57, 0x5b, 0x55, 0x5e}, "字幕"},
		{[]byte{0, 9, 'a', 'b'}, "ab"},
		{[]byte{0, 0}, ""},
		{[]byte{0}, ""},
	} {
		if got := ExportTX3GSampleText(tc.sample); got != tc.want {
			t.Errorf("text of % x = %q, want %q", tc.sample, got, tc.want)
		}
	}
}

// writeTestTX3GMP4 writes an MP4 file with a tx3g subtitle track, which has
// an edit list that starts it 500 ms into the movie, and a QuickTime chapter
// text track that the subtitle track refers to.
func writeTestTX3GMP4(t *testing.T) string {
	t.Helper()
	samples := [][]byte{
		append([]byte{0, 5}, "Hello"...),
		{0, 0}, // a gap.
		append(append([]byte{0, 13}, "Two\nlines <b>"...), mp4TestBox("styl", []byte{0, 0})...),
		{0, 0}, // ends the last cue, with zero duration.
	}
	durations := []uint32{1000, 500, 1500, 0}
	var mdat []byte
	var stts, sizes []byte
	for i, sample := range samples {
		mdat = append(mdat, sample...)
		stts = append(stts, mp4TestUint32s(1, durations[i])...)
		sizes = append(sizes, mp4TestUint32s(uint32(len(sample)))...)
	}
	ftyp := mp4TestBox("ftyp", []byte("isom\x00\x00\x02\x00"))
	mdatBox := mp4TestBox("mdat", mdat)
	tx3gEntry := mp4TestBox("tx3g", make([]byte, 8+30))
	stbl := mp4TestBox("stbl",
		mp4TestBox("stsd", mp4TestUint32s(0, 1), tx3gEntry),
		mp4TestBox("stts", mp4TestUint32s(0, uint32(len(samples))), stts),
		mp4TestBox("stsc", mp4TestUint32s(0, 1, 1, uint32(len(samples)), 1)),
		mp4TestBox("stsz", mp4TestUint32s(0, 0, uint32(len(samples))), sizes),
		mp4TestBox("stco", mp4TestUint32s(0, 1, uint32(len(ftyp)+8))),
	)
	handler := func(kind, name string) []byte {
		data := make([]byte, 24)
		copy(data[8:12], kind)
		return append(append(data, name...), 0)
	}
	// "jpn" packed into three five-bit letters.
	language := uint32('j'-0x60)<<10 | uint32('p'-0x60)<<5 | uint32('n'-0x60)
	mdhd := mp4TestBox("mdhd", mp4TestUint32s(0, 0, 0, 1000, 3000, language<<16))
	elng := mp4TestBox("elng", append([]byte{0, 0, 0, 0}, "ja-JP\x00"...))
	tkhd := func(id uint32) []byte { return mp4TestBox("tkhd", mp4TestUint32s(0, 0, 0, id, 0, 3000)) }
	subtitle := mp4TestBox("trak", tkhd(1),
		mp4TestBox("tref", mp4TestBox("chap", mp4TestUint32s(2))),
		mp4TestBox("edts", mp4TestBox("elst", mp4TestUint32s(0, 2, 500, 0xffffffff, 0x10000, 3000, 0, 0x10000))),
		mp4TestBox("mdia", mdhd, elng, mp4TestBox("hdlr", handler("sbtl", "日本語")), mp4TestBox("minf", stbl)))
	chapters := mp4TestBox("trak", tkhd(2),
		mp4TestBox("mdia", mp4TestBox("mdhd", mp4TestUint32s(0, 0, 0, 1000, 3000, 0)), mp4TestBox("hdlr", handler("text", "Chapters")), mp4TestBox("minf", stbl)))
	moov := mp4TestBox("moov", mp4TestBox("mvhd", mp4TestUint32s(0, 0, 0, 1000, 3500)), subtitle, chapters)
	path := filepath.Join(t.TempDir(), "tx3g.mp4")
	if err := os.WriteFile(path, slices.Concat(ftyp, mdatBox, moov), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An MP4 timed-text track reports its language and name, and its cues are
// placed on the movie timeline by its edit list. A text track that holds
// chapter names is not a subtitle track.
func TestReadSubtitleCuesFromMP4(t *testing.T) {
	path := writeTestTX3GMP4(t)
	info, err := ProbeMedia(path)
	if err != nil {
		t.Fatal(err)
	}
	if track := info.Tracks[0]; track.Kind != TrackKindSubtitle || track.Codec != SubtitleCodecTX3G || track.Language != "ja-JP" || track.Name != "日本語" {
		t.Fatalf("subtitle track = %+v", track)
	}
	if track := info.Tracks[1]; track.Kind != TrackKindOther || track.Name != "Chapters" {
		t.Fatalf("chapter track = %+v", track)
	}
	cues, err := ReadSubtitleCues(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	ms := time.Millisecond
	want := []SubtitleCue{{Start: 500 * ms, End: 1500 * ms, Text: "Hello"}, {Start: 2000 * ms, End: 3500 * ms, Text: "Two\nlines <b>"}}
	if !slices.Equal(cues, want) {
		t.Fatalf("cues = %q, want %q", cues, want)
	}
	if _, err := ReadSubtitleCues(path, 1); err == nil {
		t.Fatal("the chapter track was read as subtitles")
	}
}
