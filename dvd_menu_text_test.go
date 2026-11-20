package video2dvd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
)

// Options and Profile are compared and used as map keys by callers; the menu
// font must not change that.
var (
	_ = Options{} == Options{}
	_ = Profile{} == Profile{}
)

// decomposed returns text with every kana that carries a voiced or
// semi-voiced sound mark split into the plain kana and the combining mark
// (U+3099 or U+309A), as macOS stores file names.
func decomposed(text string) string {
	const (
		voiced     = "がぎぐげござじずぜぞだぢづでどばびぶべぼガギグゲゴザジズゼゾダヂヅデドバビブベボ"
		semiVoiced = "ぱぴぷぺぽパピプペポ"
	)
	var result []rune
	for _, char := range text {
		switch {
		case strings.ContainsRune(voiced, char):
			result = append(result, char-1, 0x3099)
		case strings.ContainsRune(semiVoiced, char):
			result = append(result, char-2, 0x309a)
		case char == 'ヴ':
			result = append(result, 'ウ', 0x3099)
		case char == 'ゔ':
			result = append(result, 'う', 0x3099)
		default:
			result = append(result, char)
		}
	}
	return string(result)
}

func goRegularMenuFont(t *testing.T) *MenuFont {
	t.Helper()
	menuFont, err := ParseMenuFont(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	return menuFont
}

// drawMenuText draws text into a new black frame with the fonts of profile.
func drawMenuText(profile Profile, width, height, x, y int, text string) DecodedVideoFrame {
	frame := DecodedVideoFrame{Width: width, Height: height, Y: make([]byte, width*height)}
	ExportDrawDVDMenuTextWithProfile(profile, &frame, x, y, text, 255)
	return frame
}

func inkedPixels(frame DecodedVideoFrame) int {
	return len(frame.Y) - bytes.Count(frame.Y, []byte{0})
}

func TestDVDMenuJapaneseFontHasJISGlyphs(t *testing.T) {
	count, checksum, err := ExportDVDMenuJapaneseFontSummary()
	if err != nil {
		t.Fatal(err)
	}
	// JIS X 0208 has 6879 characters, eight of which are also reachable by
	// a second code point; JIS X 0201 has 63 half-width katakana.
	if count != 6879+8+63 {
		t.Errorf("embedded font has %d glyphs, want %d", count, 6879+8+63)
	}
	// Printed by gen/menufont for the Shinonome 0.9.11 source archive.
	const wantChecksum = "69bc0c935cd5a70990be469afe20252ca5b4939e8b10a4383ded5ca2d3780302"
	if got := fmt.Sprintf("%x", checksum); got != wantChecksum {
		t.Errorf("embedded font table sha256 = %s, want %s; was it regenerated?", got, wantChecksum)
	}

	seen := map[string]rune{}
	for _, class := range []struct {
		name  string
		chars string
		width int
	}{
		{"hiragana", "あいうんがぱぁゃっゑ", 16},
		{"katakana", "アイウンガパァャッヴヶ", 16},
		{"kanji level 1", "亜日本語夏休運動会腕", 16},
		{"kanji level 2", "弌鬱薔薇躑躅龠堯熙", 16},
		{"full-width forms", "ＡＺａｚ０９！？（）", 16},
		{"punctuation", "、。「」・ー〜々〒※", 16},
		{"symbols also in Latin-1", "×÷°±", 16},
		{"half-width katakana", "ｱｲｳｶﾝｯｬﾞﾟ｡｢｣､･ｰ", 8},
	} {
		for _, char := range class.chars {
			width, rows, ok := ExportDVDMenuJapaneseGlyph(char)
			if !ok {
				t.Errorf("%s: %q (U+%04X) has no glyph", class.name, char, char)
				continue
			}
			if width != class.width || len(rows) != 16 {
				t.Errorf("%s: %q is %dx%d, want %dx16", class.name, char, width, len(rows), class.width)
			}
			if !slices.ContainsFunc(rows, func(row uint32) bool { return row != 0 }) {
				t.Errorf("%s: %q is blank", class.name, char)
			}
			key := fmt.Sprint(width, rows)
			if other, duplicate := seen[key]; duplicate {
				t.Errorf("%s: %q and %q share a glyph", class.name, other, char)
			}
			seen[key] = char
		}
	}
	// The ideographic space is the one blank glyph.
	if width, rows, ok := ExportDVDMenuJapaneseGlyph(0x3000); !ok || width != 16 || slices.ContainsFunc(rows, func(row uint32) bool { return row != 0 }) {
		t.Errorf("ideographic space: width %d, rows %v, found %v; want a blank 16-pixel cell", width, rows, ok)
	}
	// Text converted from Shift_JIS on Windows uses other code points for
	// some JIS X 0208 characters than text converted elsewhere.
	for _, pair := range [][2]rune{{0x301c, 0xff5e}, {0x2212, 0xff0d}, {0x2016, 0x2225}, {0x2014, 0x2015}, {0xa5, 0xffe5}} {
		_, first, firstOK := ExportDVDMenuJapaneseGlyph(pair[0])
		_, second, secondOK := ExportDVDMenuJapaneseGlyph(pair[1])
		if !firstOK || !secondOK || !slices.Equal(first, second) {
			t.Errorf("U+%04X and U+%04X do not share a glyph", pair[0], pair[1])
		}
	}
	for _, char := range []rune{'A', '?', 'é', '한', '😀', 0x3099, 0x0301, 0xfffd} {
		if _, _, ok := ExportDVDMenuJapaneseGlyph(char); ok {
			t.Errorf("%q (U+%04X) has a glyph in the Japanese font", char, char)
		}
	}
}

func TestDVDMenuTextComposesDecomposedKana(t *testing.T) {
	bitmap := Profile{Aspect: AspectRatio16x9}
	const composed = "がっこうのプールでパピプペポ ぎゃくてん ヴィデオ ばびぶべぼ"
	nfd := decomposed(composed)
	if nfd == composed || len([]rune(nfd)) <= len([]rune(composed)) {
		t.Fatalf("test input %q is not decomposed", nfd)
	}
	if got := ExportNormalizeDVDMenuText(bitmap, nfd); got != composed {
		t.Errorf("normalised %q = %q, want %q", nfd, got, composed)
	}
	if got, want := ExportDVDMenuTextWidth(bitmap, ExportNormalizeDVDMenuText(bitmap, nfd)), ExportDVDMenuTextWidth(bitmap, composed); got != want {
		t.Errorf("decomposed name is %d pixels wide, composed one %d", got, want)
	}
	// A mark that cannot combine is shown as the spacing mark; nothing is
	// dropped or reordered.
	for input, want := range map[string]string{
		string([]rune{'あ', 0x3099}):         "あ゛",
		string([]rune{0x309a, 'か'}):         "゜か",
		string([]rune{'ハ', 0x309a, 0x3099}): "パ゛",
		string([]rune{'A', 0x3099}):         "A゛",
		"かな":                                "かな",
		"ASCII only":                        "ASCII ONLY",
	} {
		if got := ExportNormalizeDVDMenuText(bitmap, input); got != want {
			t.Errorf("normalised %q = %q, want %q", input, got, want)
		}
	}

	// The name of a file as macOS reports it gives the same menu as the
	// composed name, and its label is cut by composed characters: 15
	// full-width kana fit beside the number in a half-width column.
	names := func(name string) []string { return slices.Repeat([]string{name}, 20) }
	standard := Profile{Aspect: AspectRatio4x3, FrameWidth: 720, FrameHeight: 480}
	long := strings.Repeat("ガギグゲゴ", 8)
	fromNFD, _ := ExportDVDMenuPage(standard, names(decomposed(long)), 0, 0, 1, 2)
	fromNFC, _ := ExportDVDMenuPage(standard, names(long), 0, 0, 1, 2)
	if !bytes.Equal(fromNFD.Y, fromNFC.Y) {
		t.Error("menu pages of a decomposed and a composed name differ")
	}
	labels := ExportDVDMenuLabels(standard, names(decomposed(long)), 0, 0, 1)
	if want := "01 " + strings.Repeat("ガギグゲゴ", 3); labels[1] != want {
		t.Errorf("label = %q, want %q", labels[1], want)
	}
}

func TestDVDMenuTextKeepsNonASCIICase(t *testing.T) {
	bitmap := Profile{Aspect: AspectRatio16x9}
	// Only the letters the 5x7 font lacks in lower case are folded.
	if got, want := ExportNormalizeDVDMenuText(bitmap, "abcｘｙｚ straße ÿ"), "ABCｘｙｚ STRAßE ÿ"; got != want {
		t.Errorf("normalised text = %q, want %q", got, want)
	}
	// Full-width small and capital letters are different glyphs.
	_, small, _ := ExportDVDMenuJapaneseGlyph('ａ')
	_, capital, _ := ExportDVDMenuJapaneseGlyph('Ａ')
	if slices.Equal(small, capital) {
		t.Error("full-width a and A share a glyph")
	}
	// With a font, text is shown as written.
	outline := Profile{Aspect: AspectRatio16x9, MenuFont: goRegularMenuFont(t)}
	if got := ExportNormalizeDVDMenuText(outline, "Holiday in München"); got != "Holiday in München" {
		t.Errorf("normalised text with a menu font = %q, want it unchanged", got)
	}
	labels := ExportDVDMenuLabels(outline, []string{"Holiday"}, 0, 0, 1)
	if labels[0] != "PLAY ALL" || labels[1] != "01 Holiday" {
		t.Errorf("labels with a menu font = %q", labels)
	}
}

func TestDVDMenuDrawsJapaneseText(t *testing.T) {
	bitmap := Profile{}
	const text = "夏休みのビデオ"
	if got := ExportDVDMenuTextWidth(bitmap, text); got != 7*16 {
		t.Errorf("width of %q = %d, want %d", text, got, 7*16)
	}
	// Mixed text advances 12 pixels per ASCII character, 16 per full-width
	// and 8 per half-width one.
	if got, want := ExportDVDMenuTextWidth(bitmap, "01 夏休み 2024.MP4 ｶﾞｯｺｳ"), 12*13+16*3+8*5; got != want {
		t.Errorf("width of mixed text = %d, want %d", got, want)
	}
	japanese := drawMenuText(bitmap, 7*16, 18, 0, 2, text)
	unknown := drawMenuText(bitmap, 7*16, 18, 0, 2, strings.Repeat("?", 7))
	if inkedPixels(japanese) < 7*20 {
		t.Errorf("%q has only %d text pixels", text, inkedPixels(japanese))
	}
	if bytes.Equal(japanese.Y, unknown.Y) {
		t.Errorf("%q is drawn as question marks", text)
	}
	// Japanese glyphs use the 16 lines around the 14 of the ASCII glyphs,
	// which a button rectangle covers with a line to spare on either side.
	for y, wantInk := range map[int]bool{0: false, 1: true, 16: true, 17: false} {
		row := japanese.Y[y*japanese.Width : (y+1)*japanese.Width]
		if hasInk := bytes.ContainsFunc(row, func(r rune) bool { return r != 0 }); hasInk != wantInk {
			t.Errorf("line %d of text drawn at line 2: ink = %v, want %v", y, hasInk, wantInk)
		}
	}
	// Every character without a glyph is one question mark.
	const missing = "한😀é"
	if got := ExportDVDMenuTextWidth(bitmap, missing); got != 3*12 {
		t.Errorf("width of %q = %d, want three 12-pixel cells", missing, got)
	}
	if !bytes.Equal(drawMenuText(bitmap, 40, 14, 0, 0, missing).Y, drawMenuText(bitmap, 40, 14, 0, 0, "???").Y) {
		t.Errorf("%q is not drawn as three question marks", missing)
	}
	// Drawing next to or beyond the frame edges must not panic or wrap.
	edge := drawMenuText(bitmap, 20, 10, -9, -5, "夏A休")
	if inkedPixels(edge) == 0 {
		t.Error("text overlapping the frame edge left no pixels")
	}
}

// longMenuNames returns names that are all wider than a button.
func longMenuNames(count int) []string {
	names := make([]string, count)
	for i := range names {
		switch i % 5 {
		case 0:
			names[i] = strings.Repeat("夏休みの思い出", 12)
		case 1:
			names[i] = strings.Repeat("ｶﾞｯｺｳ運動会（午前の部）Part2 ", 6)
		case 2:
			names[i] = decomposed(strings.Repeat("プールで泳ぐビデオ", 9))
		case 3:
			names[i] = strings.Repeat("Wide Latin name gjpqy ", 9)
		default:
			names[i] = strings.Repeat("iＭ.한", 40)
		}
	}
	return names
}

func TestDVDMenuLabelsFitButtonsByPixelWidth(t *testing.T) {
	const textInset = 8 // a button starts this far left of its label and ends this far right of the widest label.
	fonts := map[string]*MenuFont{"built-in fonts": nil, "outline font": goRegularMenuFont(t)}
	for fontName, menuFont := range fonts {
		for _, tc := range []struct {
			name     string
			standard VideoStandard
			aspect   AspectRatio
			height   int
			titles   int
		}{
			{"NTSC 4:3 two columns", VideoStandardNTSC, AspectRatio4x3, 480, 33},
			{"NTSC 16:9", VideoStandardNTSC, AspectRatio16x9, 480, 15},
			{"PAL 4:3 two columns", VideoStandardPAL, AspectRatio4x3, 576, 33},
			{"PAL 16:9", VideoStandardPAL, AspectRatio16x9, 576, 15},
		} {
			t.Run(fontName+" "+tc.name, func(t *testing.T) {
				profile := Profile{Standard: tc.standard, Aspect: tc.aspect, FrameWidth: 720, FrameHeight: tc.height,
					MenuTitle: strings.Repeat("家族ビデオ Family 2024年 ", 8), MenuFont: menuFont}
				names := longMenuNames(tc.titles)
				labels := ExportDVDMenuLabels(profile, names, 0, 0, 2)
				frame, buttons := ExportDVDMenuPage(profile, names, 0, 0, 2, 3)
				if len(labels) != len(buttons) || len(buttons) != tc.titles+3 {
					t.Fatalf("%d labels and %d buttons for %d titles", len(labels), len(buttons), tc.titles)
				}
				for i, button := range buttons {
					width := ExportDVDMenuTextWidth(profile, labels[i])
					if end := button.XStart + textInset + width; end > button.XEnd+1-textInset {
						t.Errorf("label %d %q is %d pixels wide and ends at x=%d; its button ends at x=%d", i, labels[i], width, end, button.XEnd)
					}
					if i < 3 {
						continue
					}
					// The label is cut, and no earlier than necessary: the
					// next character of the name would not have fitted.
					name := []rune(ExportNormalizeDVDMenuText(profile, names[i-3]))
					shown := len([]rune(labels[i])) - 3
					if shown <= 0 || shown >= len(name) {
						t.Fatalf("label %d %q shows %d characters of the name", i, labels[i], shown)
					}
					if !strings.HasSuffix(labels[i], string(name[:shown])) {
						t.Errorf("label %d %q is not a prefix of its name", i, labels[i])
					}
					longer := labels[i] + string(name[shown])
					if button.XStart+textInset+ExportDVDMenuTextWidth(profile, longer) <= button.XEnd+1-textInset {
						t.Errorf("label %d %q could show another character", i, labels[i])
					}
				}
				// Every text pixel below the heading belongs to a button,
				// and the heading stays within the title-safe width.
				const background, headingEnd = 24, 44
				inked := make([]int, len(buttons))
				for y := range frame.Height {
					for x := range frame.Width {
						if frame.Y[y*frame.Width+x] == background {
							continue
						}
						if y < headingEnd {
							if x < 40 || x >= 680 || y < 22 {
								t.Fatalf("heading pixel at (%d,%d)", x, y)
							}
							continue
						}
						owner := slices.IndexFunc(buttons, func(button DVDNAVButton) bool {
							return button.XStart <= x && x <= button.XEnd && button.YStart <= y && y <= button.YEnd
						})
						if owner < 0 {
							t.Fatalf("text pixel at (%d,%d) is outside every button", x, y)
						}
						inked[owner]++
					}
				}
				for i, count := range inked {
					if count == 0 {
						t.Errorf("button %d has no label", i+1)
					}
				}
			})
		}
	}
}

func asciiGoldenNames(count int) []string {
	names := make([]string, count)
	for i := range names {
		switch i % 5 {
		case 0:
			names[i] = fmt.Sprintf("Holiday-Trip_%02d.part", i)
		case 1:
			names[i] = strings.Repeat("long name 0123456789 ", 5)
		case 2:
			names[i] = ""
		case 3:
			names[i] = "THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG?"
		default:
			names[i] = "zyxwvutsrqponmlkjihgfedcba"
		}
	}
	return names
}

func menuPageChecksum(frame DecodedVideoFrame, buttons []DVDNAVButton) string {
	hash := sha256.New()
	hash.Write(frame.Y)
	hash.Write(frame.U)
	hash.Write(frame.V)
	fmt.Fprintf(hash, "%v", buttons)
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// TestDVDMenuASCIIPagesAreUnchanged compares menu pages whose text is made
// of letters, digits, space, '-', '_', '.' and '?' with checksums of the
// pictures and buttons that the menu produced before it could show Japanese.
func TestDVDMenuASCIIPagesAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		profile                    Profile
		titles, start, page, pages int
		want                       string
	}{
		{"ntsc 4:3 36 buttons", Profile{Standard: VideoStandardNTSC, Aspect: AspectRatio4x3, FrameWidth: 720, FrameHeight: 480}, 33, 0, 0, 2,
			"a839ada53e8598932ee111da69876f3e24a743b7cfbee6b6980b97e0c340de70"},
		{"ntsc 16:9 heading", Profile{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, FrameWidth: 720, FrameHeight: 480, MenuTitle: "Family Videos 2024", MenuBackgroundLuma: 40, MenuTextLuma: 200}, 4, 0, 0, 1,
			"2d69adaf0e3c0d14d7efd1f963feb8c2a12d4eacdf7293b8021a8432b922b834"},
		{"pal 16:9 middle page", Profile{Standard: VideoStandardPAL, Aspect: AspectRatio16x9, FrameWidth: 720, FrameHeight: 576, MenuTitle: strings.Repeat("Heading ", 12)}, 15, 15, 1, 3,
			"e07592bd9bb98e591810035496e5c4077efcbbfa2be474b3971489267ad42046"},
		{"pal 4:3 two columns", Profile{Standard: VideoStandardPAL, Aspect: AspectRatio4x3, FrameWidth: 720, FrameHeight: 576}, 20, 0, 0, 1,
			"9f1f10920f9646555d8b3b4466f857d5959463cb2d1abf3ad2833ed57335556d"},
	} {
		frame, buttons := ExportDVDMenuPage(tc.profile, asciiGoldenNames(tc.titles), tc.start, tc.page, tc.pages, tc.pages+1)
		if got := menuPageChecksum(frame, buttons); got != tc.want {
			t.Errorf("%s: page checksum = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseMenuFont(t *testing.T) {
	menuFont := goRegularMenuFont(t)
	profile, err := ExportProfile(Options{MenuFont: menuFont})
	if err != nil || profile.MenuFont != menuFont {
		t.Errorf("profile menu font = %p, %v; want the font of the options", profile.MenuFont, err)
	}
	if indexed, err := ParseMenuFontIndex(goregular.TTF, 0); err != nil || indexed == nil {
		t.Errorf("font 0 of a single font: %v", err)
	}
	for name, parse := range map[string]func() (*MenuFont, error){
		"empty data":   func() (*MenuFont, error) { return ParseMenuFont(nil) },
		"not a font":   func() (*MenuFont, error) { return ParseMenuFont([]byte(strings.Repeat("not a font ", 100))) },
		"truncated":    func() (*MenuFont, error) { return ParseMenuFont(goregular.TTF[:1000]) },
		"index 1 of 1": func() (*MenuFont, error) { return ParseMenuFontIndex(goregular.TTF, 1) },
		"index -1":     func() (*MenuFont, error) { return ParseMenuFontIndex(goregular.TTF, -1) },
	} {
		if menuFont, err := parse(); err == nil || menuFont != nil {
			t.Errorf("%s: font %p, error %v; want an error", name, menuFont, err)
		}
	}
}

func TestDVDMenuOutlineFontDrawsAntiAliasedText(t *testing.T) {
	menuFont := goRegularMenuFont(t)
	outline := Profile{Standard: VideoStandardNTSC, Aspect: AspectRatio4x3, MenuFont: menuFont}
	bitmap := Profile{Standard: VideoStandardNTSC, Aspect: AspectRatio4x3}
	const text = "Holiday gjpqy 2024"
	width := ExportDVDMenuTextWidth(outline, text)
	if width < 60 || width > 200 {
		t.Fatalf("width of %q = %d", text, width)
	}
	frame := drawMenuText(outline, width+8, 18, 4, 2, text)
	levels := map[byte]int{}
	for _, value := range frame.Y {
		levels[value]++
	}
	if levels[255] == 0 || len(levels) < 8 {
		t.Errorf("outline text has %d luma levels and %d full pixels; want anti-aliased strokes", len(levels), levels[255])
	}
	if bytes.Equal(frame.Y, drawMenuText(bitmap, width+8, 18, 4, 2, text).Y) {
		t.Error("the menu font does not change the text")
	}
	// The text lies within its measured width, and within the 18 lines of
	// a button drawn two lines above the text position: capitals and
	// descenders are complete.
	for y := range frame.Height {
		for x := range frame.Width {
			if frame.Y[y*frame.Width+x] != 0 && (x < 3 || x > 4+width) {
				t.Fatalf("pixel at (%d,%d) is outside the %d measured pixels", x, y, width)
			}
		}
	}
	tall := drawMenuText(outline, width+8, 40, 4, 12, text)
	for y := range tall.Height {
		row := tall.Y[y*tall.Width : (y+1)*tall.Width]
		if bytes.ContainsFunc(row, func(r rune) bool { return r != 0 }) && (y < 10 || y > 27) {
			t.Errorf("text drawn at line 12 has pixels on line %d, outside its button's lines 10 to 27", y)
		}
	}

	// Blending is between the background and the text luma.
	page, _ := ExportDVDMenuPage(Profile{Aspect: AspectRatio16x9, FrameWidth: 720, FrameHeight: 480, MenuFont: menuFont, MenuBackgroundLuma: 60, MenuTextLuma: 180},
		[]string{"Holiday"}, 0, 0, 1, 2)
	low, high := slices.Min(page.Y), slices.Max(page.Y)
	if low != 60 || high != 180 {
		t.Errorf("page luma spans %d..%d, want 60..180", low, high)
	}

	// The picture is the same every time, and on every platform: outlines
	// are scaled and filled with integer arithmetic.
	again := drawMenuText(outline, width+8, 18, 4, 2, text)
	if !bytes.Equal(frame.Y, again.Y) {
		t.Error("two renderings of the same text differ")
	}
	const want = "389be035d9c98b709c14248aeb5e57fae4c24f06dcd1e10acf132492a0689498"
	if got := fmt.Sprintf("%x", sha256.Sum256(frame.Y)); got != want {
		t.Errorf("rendering checksum = %s, want %s", got, want)
	}
}

func TestDVDMenuOutlineFontFallsBackPerCharacter(t *testing.T) {
	outline := Profile{Aspect: AspectRatio16x9, MenuFont: goRegularMenuFont(t)}
	bitmap := Profile{Aspect: AspectRatio16x9}
	// Go Regular has no Japanese: those characters come from the embedded
	// font, exactly as without a menu font.
	const japanese = "夏休みｶﾞｯｺｳ"
	if got, want := ExportDVDMenuTextWidth(outline, japanese), 3*16+5*8; got != want {
		t.Errorf("width of %q with a Latin font = %d, want %d", japanese, got, want)
	}
	if !bytes.Equal(drawMenuText(outline, 100, 18, 2, 2, japanese).Y, drawMenuText(bitmap, 100, 18, 2, 2, japanese).Y) {
		t.Errorf("%q is not drawn with the embedded font", japanese)
	}
	// In mixed text both fonts are used.
	const mixed = "Trip 夏休み"
	mixedFrame := drawMenuText(outline, 140, 18, 2, 2, mixed)
	latinWidth := ExportDVDMenuTextWidth(outline, "Trip ")
	if got, want := ExportDVDMenuTextWidth(outline, mixed), latinWidth+3*16; got != want {
		t.Errorf("width of %q = %d, want %d", mixed, got, want)
	}
	latinOnly := drawMenuText(outline, 140, 18, 2, 2, "Trip ")
	if bytes.Equal(mixedFrame.Y, latinOnly.Y) || inkedPixels(mixedFrame) < inkedPixels(latinOnly)+60 {
		t.Errorf("%q shows no Japanese", mixed)
	}
	// A character in neither font is the question mark of the menu font.
	if got, want := ExportDVDMenuTextWidth(outline, "한😀"), ExportDVDMenuTextWidth(outline, "??"); got != want {
		t.Errorf("width of two unknown characters = %d, want that of two question marks, %d", got, want)
	}
	if !bytes.Equal(drawMenuText(outline, 40, 18, 2, 2, "한").Y, drawMenuText(outline, 40, 18, 2, 2, "?").Y) {
		t.Error("an unknown character is not drawn as a question mark")
	}
}

func TestDVDMenuOutlineFontCompensatesPixelAspect(t *testing.T) {
	menuFont := goRegularMenuFont(t)
	const text = "MMMMMMMMMMMMMMMMMMMM"
	type format struct {
		standard               VideoStandard
		aspect                 AspectRatio
		numerator, denominator int
	}
	formats := []format{
		{VideoStandardNTSC, AspectRatio4x3, 10, 11},
		{VideoStandardNTSC, AspectRatio16x9, 40, 33},
		{VideoStandardPAL, AspectRatio4x3, 12, 11},
		{VideoStandardPAL, AspectRatio16x9, 16, 11},
	}
	for _, f := range formats {
		profile := Profile{Standard: f.standard, Aspect: f.aspect, MenuFont: menuFont}
		if numerator, denominator := ExportDVDMenuPixelAspect(profile); numerator != f.numerator || denominator != f.denominator {
			t.Errorf("pixel aspect of standard %d aspect %d = %d:%d, want %d:%d", f.standard, f.aspect, numerator, denominator, f.numerator, f.denominator)
		}
		// On the display a pixel is numerator/denominator as wide as it
		// is high, so the displayed width of a text is the same in every
		// format, to within the rounding of 20 advances.
		width := ExportDVDMenuTextWidth(profile, text)
		if displayed := width * f.numerator * 100 / f.denominator; displayed < 26400 || displayed > 27100 {
			t.Errorf("standard %d aspect %d: %d pixels are %d.%02d square pixels wide on the display, want about 267", f.standard, f.aspect, width, displayed/100, displayed%100)
		}
		// The ink is scaled like the advances; the height is not scaled.
		frame := drawMenuText(profile, 400, 18, 2, 2, text)
		left, right, top, bottom := frame.Width, 0, frame.Height, 0
		for y := range frame.Height {
			for x := range frame.Width {
				if frame.Y[y*frame.Width+x] >= 128 {
					left, right, top, bottom = min(left, x), max(right, x), min(top, y), max(bottom, y)
				}
			}
		}
		if right-left+1 < width-4 || right-left+1 > width {
			t.Errorf("standard %d aspect %d: ink spans %d pixels of %d measured", f.standard, f.aspect, right-left+1, width)
		}
		if top != 2 || bottom != 13 {
			t.Errorf("standard %d aspect %d: capitals span lines %d..%d, want 2..13", f.standard, f.aspect, top, bottom)
		}
	}
	// The built-in fonts are bitmaps and are not scaled.
	for _, f := range formats {
		if got := ExportDVDMenuTextWidth(Profile{Standard: f.standard, Aspect: f.aspect}, "AB夏"); got != 40 {
			t.Errorf("standard %d aspect %d: built-in fonts measure %d pixels, want 40", f.standard, f.aspect, got)
		}
	}
}

func TestMenuOutlineFillCoverage(t *testing.T) {
	point := func(x, y float64) fixed.Point26_6 {
		return fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(y * 64)}
	}
	// A rectangle from (1.5, 1) to (3.25, 2.5) in a 5x4 area.
	rectangle := []fixed.Point26_6{point(1.5, 1), point(3.25, 1), point(3.25, 2.5), point(1.5, 2.5)}
	want := []uint8{
		0, 0, 0, 0, 0,
		0, 128, 255, 64, 0,
		0, 64, 128, 32, 0,
		0, 0, 0, 0, 0,
	}
	if got := ExportFillMenuOutline(rectangle, 5, 4); !bytes.Equal(got, want) {
		t.Errorf("rectangle coverage = %v, want %v", got, want)
	}
	// The direction of the outline does not matter.
	slices.Reverse(rectangle)
	if got := ExportFillMenuOutline(rectangle, 5, 4); !bytes.Equal(got, want) {
		t.Errorf("reversed rectangle coverage = %v, want %v", got, want)
	}
	// A triangle covering half of a 4x4 square, clipped to 3 columns.
	triangle := []fixed.Point26_6{point(0, 0), point(4, 4), point(0, 4)}
	got := ExportFillMenuOutline(triangle, 3, 4)
	for y := range 4 {
		for x := range 3 {
			want := 0
			switch {
			case x < y:
				want = 255
			case x == y:
				want = 128
			}
			if value := int(got[y*3+x]); value < want-2 || value > want+2 {
				t.Errorf("triangle coverage at (%d,%d) = %d, want %d", x, y, value, want)
			}
		}
	}
}

func TestDVDMenuTitleWithJapaneseNamesAndMenuFont(t *testing.T) {
	for name, menuFont := range map[string]*MenuFont{"built-in fonts": nil, "outline font": goRegularMenuFont(t)} {
		t.Run(name, func(t *testing.T) {
			profile := Profile{Standard: VideoStandardNTSC, Aspect: AspectRatio4x3, FrameWidth: 720, FrameHeight: 480,
				MenuTitle: "家族ビデオ 2024年", MenuFont: menuFont}
			titles := make([]EncodedTitle, 36)
			for i := range titles {
				titles[i] = EncodedTitle{Name: fmt.Sprintf("夏休みの思い出 Part%d", i+1)}
			}
			dir := t.TempDir()
			menu, pages, err := ExportCreateDVDMenuTitle(context.Background(), titles, profile, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 2 || len(pages[0]) != 36 {
				t.Fatalf("menu has %d pages and %d buttons on the first, want 2 and 36", len(pages), len(pages[0]))
			}
			if info, err := os.Stat(menu.VideoPath); err != nil || info.Size() == 0 {
				t.Errorf("menu video: %v", err)
			}
		})
	}
}

// TestDVDMenuSystemJapaneseFont draws Japanese with a font of the system,
// where there is one.
func TestDVDMenuSystemJapaneseFont(t *testing.T) {
	var data []byte
	for _, path := range []string{
		"/System/Library/Fonts/ヒラギノ角ゴシック W3.ttc",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/truetype/fonts-japanese-gothic.ttf",
		`C:\Windows\Fonts\msgothic.ttc`,
	} {
		if contents, err := os.ReadFile(path); err == nil {
			data = contents
			break
		}
	}
	if data == nil {
		t.Skip("no Japanese system font found")
	}
	menuFont, err := ParseMenuFont(data)
	if err != nil {
		t.Fatal(err)
	}
	outline := Profile{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9, FrameWidth: 720, FrameHeight: 480, MenuFont: menuFont, MenuTitle: "家族ビデオ"}
	bitmap := outline
	bitmap.MenuFont = nil
	const text = "夏休みの思い出"
	width := ExportDVDMenuTextWidth(outline, text)
	// Seven full-width characters of a 16-line em, narrowed by 33/40.
	if width < 7*12 || width > 7*14 {
		t.Errorf("width of %q = %d, want about %d", text, width, 7*16*33/40)
	}
	frame := drawMenuText(outline, 120, 18, 2, 2, text)
	if inkedPixels(frame) < 200 || bytes.Equal(frame.Y, drawMenuText(bitmap, 120, 18, 2, 2, text).Y) {
		t.Errorf("%q is not drawn with the system font", text)
	}
	// Decomposed names are composed before the font is asked for glyphs.
	if !bytes.Equal(drawMenuText(outline, 120, 18, 2, 2, ExportNormalizeDVDMenuText(outline, decomposed("ガギグゲゴ"))).Y, drawMenuText(outline, 120, 18, 2, 2, "ガギグゲゴ").Y) {
		t.Error("decomposed and composed kana are drawn differently")
	}
	names := longMenuNames(15)
	labels := ExportDVDMenuLabels(outline, names, 0, 0, 1)
	_, buttons := ExportDVDMenuPage(outline, names, 0, 0, 1, 2)
	for i, button := range buttons {
		if end := button.XStart + 8 + ExportDVDMenuTextWidth(outline, labels[i]); end > button.XEnd+1-8 {
			t.Errorf("label %d ends at x=%d, beyond its button's text area", i, end)
		}
	}
	// A collection has more than one font; a later one can be selected.
	if second, err := ParseMenuFontIndex(data, 1); err == nil && second == nil {
		t.Error("font 1 of the collection is nil without an error")
	}
}
