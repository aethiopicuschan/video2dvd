package video2dvd_test

import (
	"os"
	"strings"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
	"golang.org/x/image/font/gofont/goregular"
)

// drawJapaneseRows draws text with the built-in fonts and returns the luma
// rows of the 16-line Japanese cell as strings of '#' and '.'.
func drawJapaneseRows(text string, width int) []string {
	const x, y = 2, 4
	frame := DecodedVideoFrame{Width: width, Height: 24, Y: make([]byte, width*24), U: make([]byte, width*6), V: make([]byte, width*6)}
	ExportDrawDVDMenuText(&frame, x, y, text, 255)
	rows := make([]string, 16)
	for row := range rows {
		var line strings.Builder
		for column := range width {
			if frame.Y[(y-1+row)*width+column] != 0 {
				line.WriteByte('#')
			} else {
				line.WriteByte('.')
			}
		}
		rows[row] = line.String()
	}
	return rows
}

// The katakana ト is asymmetric in both directions, so this pins the bit
// order, the row order, the cell position, and the emboldening.
func TestDVDMenuJapaneseGlyphPixels(t *testing.T) {
	width, stored, ok := ExportDVDMenuJapaneseGlyph('ト')
	if !ok || width != 16 {
		t.Fatalf("ト: width %d, found %v", width, ok)
	}
	rows := drawJapaneseRows("ト", 20)
	for row, bits := range stored {
		// The stored row starts at frame column 2; the bold pixel of a
		// stroke is the one on its left.
		for column := range 16 {
			if bits&(1<<(15-column)) != 0 && rows[row][2+column] != '#' {
				t.Fatalf("row %d: stored pixel %d is not drawn in %q", row, column, rows[row])
			}
		}
	}
	// The vertical stroke is two pixels wide and sits left of the centre;
	// the diagonal branches to the right below the top.
	top, branch := -1, -1
	for row, line := range rows {
		if strings.Contains(line, "#") && top < 0 {
			top = row
		}
		if first, last := strings.Index(line, "#"), strings.LastIndex(line, "#"); first >= 0 && last-first > 4 && branch < 0 {
			branch = row
		}
	}
	if top < 0 || branch <= top {
		t.Fatalf("ト has no stem above its branch:\n%s", strings.Join(rows, "\n"))
	}
	stem := rows[top]
	if first, last := strings.Index(stem, "#"), strings.LastIndex(stem, "#"); last-first != 1 || first >= 10 {
		t.Fatalf("ト stem row %q is not a two-pixel stroke left of the centre", stem)
	}
	for _, line := range rows[branch:] {
		if last := strings.LastIndex(line, "#"); last >= 0 && last < strings.LastIndex(stem, "#") {
			t.Fatalf("ト is mirrored: branch row %q ends left of the stem", line)
		}
	}
}

// Emboldening must not close the one-pixel gap between two strokes, whether
// they belong to one glyph or to neighbouring glyphs.
func TestDVDMenuBoldKeepsGapsBetweenGlyphs(t *testing.T) {
	for _, text := range []string{"ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄ", "第一部開会式", "令和六年度春季大運動会", "鬱憂驚曜鷹麗警護響襲"} {
		plain := make([]string, 16)
		for row := range plain {
			var line strings.Builder
			line.WriteString("..")
			for _, char := range text {
				width, stored, ok := ExportDVDMenuJapaneseGlyph(char)
				if !ok {
					t.Fatalf("%q has no glyph", char)
				}
				for column := range width {
					if stored[row]&(1<<(width-1-column)) != 0 {
						line.WriteByte('#')
					} else {
						line.WriteByte('.')
					}
				}
			}
			plain[row] = line.String()
		}
		bold := drawJapaneseRows(text, len(plain[0])+4)
		joined := 0
		for row := range plain {
			for column := 1; column+1 < len(plain[row]); column++ {
				if plain[row][column] == '.' && plain[row][column-1] == '#' && plain[row][column+1] == '#' && bold[row][column] == '#' {
					joined++
				}
			}
			if !strings.Contains(bold[row], "#") && strings.Contains(plain[row], "#") {
				t.Fatalf("%q row %d lost its ink", text, row)
			}
		}
		if joined != 0 {
			t.Fatalf("%q: emboldening closed %d one-pixel gaps", text, joined)
		}
		if strings.Join(bold, "") == strings.Join(plain, "") {
			t.Fatalf("%q is not emboldened", text)
		}
	}
}

// ゔ and the voiced ワ row are outside JIS X 0208; the built-in font shows
// the base kana with a spacing voiced mark instead of a question mark.
func TestDVDMenuTextSpellsKanaMissingFromTheBuiltInFont(t *testing.T) {
	profile, err := ExportProfile(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{
		"ゔぁいおりん":  "う゛ぁいおりん",
		"ゔぁいおりん": "う゛ぁいおりん",
		"ヷヸヹヺ":    "ワ゛ヰ゛ヱ゛ヲ゛",
		"ヴァイオリン":  "ヴァイオリン",
	} {
		if got := ExportNormalizeDVDMenuText(profile, input); got != want {
			t.Errorf("normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

// Damaged font tables must produce an error or a usable font, never a panic.
func TestParseMenuFontSurvivesDamagedFonts(t *testing.T) {
	sources := map[string][]byte{"goregular": goregular.TTF}
	for _, path := range []string{"/System/Library/Fonts/Supplemental/Arial.ttf", "/System/Library/Fonts/Supplemental/Verdana.ttf"} {
		if data, err := os.ReadFile(path); err == nil {
			sources[path] = data
		}
	}
	profile, err := ExportProfile(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range sources {
		// Table directory and the start of each table, then a spread of
		// bytes over the whole file.
		offsets := make([]int, 0, 1200)
		for offset := 0; offset < min(len(source), 600); offset++ {
			offsets = append(offsets, offset)
		}
		for i := range 600 {
			offsets = append(offsets, int(uint64(i)*2654435761%uint64(len(source))))
		}
		// The two single-byte changes that crashed the parser before.
		if strings.HasSuffix(name, "Arial.ttf") && len(source) > 710285 {
			offsets = append(offsets, 710285)
		}
		if strings.HasSuffix(name, "Verdana.ttf") && len(source) > 176387 {
			offsets = append(offsets, 176387)
		}
		for _, offset := range offsets {
			damaged := append([]byte(nil), source...)
			damaged[offset] = 0xff
			menuFont, err := ParseMenuFont(damaged)
			if err != nil {
				continue
			}
			profile.MenuFont = menuFont
			frame := DecodedVideoFrame{Width: 64, Height: 24, Y: make([]byte, 64*24), U: make([]byte, 16*24), V: make([]byte, 16*24)}
			ExportDrawDVDMenuTextWithProfile(profile, &frame, 2, 4, "Ag日本", 255)
		}
	}
}
