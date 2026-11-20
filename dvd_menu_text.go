package video2dvd

import (
	"image"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// The combining voiced and semi-voiced sound marks of kana.
const (
	combiningDakuten    rune = 0x3099
	combiningHandakuten rune = 0x309a
)

// dvdMenuKanaCompositions maps a kana followed by a combining voiced or
// semi-voiced sound mark to the single character for the pair.
var dvdMenuKanaCompositions = sync.OnceValue(func() map[[2]rune]rune {
	// Each pair of characters is a kana and its marked form.
	const (
		voiced = "かがきぎくぐけげこごさざしじすずせぜそぞただちぢつづてでとど" +
			"はばひびふぶへべほぼうゔゝゞ" +
			"カガキギクグケゲコゴサザシジスズセゼソゾタダチヂツヅテデトド" +
			"ハバヒビフブヘベホボウヴワヷヰヸヱヹヲヺヽヾ"
		semiVoiced = "はぱひぴふぷへぺほぽハパヒピフプヘペホポ"
	)
	compositions := map[[2]rune]rune{}
	for mark, pairs := range map[rune]string{combiningDakuten: voiced, combiningHandakuten: semiVoiced} {
		runes := []rune(pairs)
		for i := 0; i < len(runes); i += 2 {
			compositions[[2]rune{runes[i], mark}] = runes[i+1]
		}
	}
	return compositions
})

// composeDVDMenuKana replaces kana followed by a combining voiced or
// semi-voiced sound mark (U+3099, U+309A) by the precomposed kana. File names
// on macOS are stored decomposed, so that が arrives as か and a mark, and the
// menu draws one glyph per character. A mark that does not combine with the
// character before it becomes the spacing mark (U+309B, U+309C).
func composeDVDMenuKana(text string) string {
	if !strings.ContainsAny(text, string([]rune{combiningDakuten, combiningHandakuten})) {
		return text
	}
	compositions := dvdMenuKanaCompositions()
	composed := make([]rune, 0, len(text))
	for _, char := range text {
		if char == combiningDakuten || char == combiningHandakuten {
			if last := len(composed) - 1; last >= 0 {
				if single, ok := compositions[[2]rune{composed[last], char}]; ok {
					composed[last] = single
					continue
				}
			}
			char += 2 // the spacing form of the mark.
		}
		composed = append(composed, char)
	}
	return string(composed)
}

// upperASCII returns text with the letters a to z in upper case. Other
// characters, including non-ASCII letters, are unchanged.
func upperASCII(text string) string {
	return strings.Map(upperASCIILetter, text)
}

func upperASCIILetter(char rune) rune {
	if 'a' <= char && char <= 'z' {
		return char - 'a' + 'A'
	}
	return char
}

// Kinds of dvdMenuGlyph.
const (
	dvdMenuGlyphSmall    = iota // the 5x7 font, drawn at twice its size.
	dvdMenuGlyphJapanese        // the embedded Japanese bitmap font.
	dvdMenuGlyphOutline         // Options.MenuFont.
)

// dvdMenuGlyph is how one character of menu text is drawn.
type dvdMenuGlyph struct {
	kind int
	// advance is the distance to the next character in pixels.
	advance  fixed.Int26_6
	small    [7]byte
	japanese dvdMenuBitmapGlyph
	outline  sfnt.GlyphIndex
}

// dvdMenuText measures and draws the text of a menu. Every character is one
// glyph: of the profile's MenuFont if it is set and has the character, else
// of the built-in 5x7 font (ASCII) or of the embedded Japanese font, and a
// question mark if none of them has it.
//
// Text is positioned by its top left corner (x, y). The 5x7 font occupies
// the 14 lines from y; the 16-line cells of the Japanese font start one line
// higher, which centres both on the same line. The Japanese font is drawn
// bold, to match the two-pixel strokes of the 5x7 font and because
// one-pixel strokes are faint on a television.
type dvdMenuText struct {
	font *MenuFont
	// Outlines are scaled horizontally by scaleNumerator/scaleDenominator,
	// the inverse of the pixel aspect ratio, so that they have their
	// natural proportions on the display.
	scaleNumerator, scaleDenominator int
	buffer                           sfnt.Buffer
	rasterizer                       menuOutlineRasterizer
	glyphs                           map[rune]dvdMenuGlyph
}

func newDVDMenuText(profile Profile) *dvdMenuText {
	text := &dvdMenuText{font: profile.MenuFont, glyphs: map[rune]dvdMenuGlyph{}}
	text.scaleDenominator, text.scaleNumerator = dvdMenuPixelAspect(profile)
	return text
}

// normalize prepares a heading or a title name for the menu. The built-in
// 5x7 font has capital letters only, so without a MenuFont ASCII letters are
// shown in upper case.
func (t *dvdMenuText) normalize(text string) string {
	text = composeDVDMenuKana(text)
	// These voiced kana are not in JIS X 0208, so without a font that has
	// them they are spelled as the base kana and a spacing voiced mark.
	if strings.ContainsAny(text, "ゔヷヸヹヺ") {
		var spelled []rune
		for _, char := range text {
			base, voiced := map[rune]rune{'ゔ': 'う', 'ヷ': 'ワ', 'ヸ': 'ヰ', 'ヹ': 'ヱ', 'ヺ': 'ヲ'}[char]
			if _, inFont := t.outlineGlyph(char); voiced && !inFont {
				spelled = append(spelled, base, combiningDakuten+2)
				continue
			}
			spelled = append(spelled, char)
		}
		text = string(spelled)
	}
	if t.font == nil {
		text = upperASCII(text)
	}
	return text
}

func (t *dvdMenuText) outlineGlyph(char rune) (glyph dvdMenuGlyph, ok bool) {
	if t.font == nil {
		return dvdMenuGlyph{}, false
	}
	// A font that parsed can still have damaged glyph data; such a glyph is
	// treated like a missing one.
	defer func() {
		if recover() != nil {
			glyph, ok = dvdMenuGlyph{}, false
		}
	}()
	index, err := t.font.font.GlyphIndex(&t.buffer, char)
	if err != nil || index == 0 {
		return dvdMenuGlyph{}, false
	}
	// A glyph without an outline, such as a bitmap or a colour glyph, is
	// treated like a missing one.
	if _, err := t.font.font.LoadGlyph(&t.buffer, index, fixed.I(dvdMenuOutlineEm), nil); err != nil {
		return dvdMenuGlyph{}, false
	}
	advance, err := t.font.font.GlyphAdvance(&t.buffer, index, fixed.I(dvdMenuOutlineEm), font.HintingNone)
	if err != nil {
		return dvdMenuGlyph{}, false
	}
	scaled := divideRounded(int64(advance)*int64(t.scaleNumerator), int64(t.scaleDenominator))
	return dvdMenuGlyph{kind: dvdMenuGlyphOutline, advance: fixed.Int26_6(scaled), outline: index}, true
}

func (t *dvdMenuText) glyph(char rune) dvdMenuGlyph {
	if glyph, ok := t.glyphs[char]; ok {
		return glyph
	}
	glyph, ok := t.outlineGlyph(char)
	if !ok {
		var small [7]byte
		if small, ok = dvdMenuFont[upperASCIILetter(char)]; ok {
			glyph = dvdMenuGlyph{kind: dvdMenuGlyphSmall, advance: fixed.I(dvdMenuCharWidth), small: small}
		}
	}
	if !ok {
		var japanese dvdMenuBitmapGlyph
		if japanese, ok = lookupDVDMenuJapaneseGlyph(char); ok {
			glyph = dvdMenuGlyph{kind: dvdMenuGlyphJapanese, advance: fixed.I(japanese.width()), japanese: japanese}
		}
	}
	if !ok {
		if glyph, ok = t.outlineGlyph('?'); !ok {
			glyph = dvdMenuGlyph{kind: dvdMenuGlyphSmall, advance: fixed.I(dvdMenuCharWidth), small: dvdMenuFont['?']}
		}
	}
	t.glyphs[char] = glyph
	return glyph
}

// width returns the width of text in pixels, rounded up.
func (t *dvdMenuText) width(text string) int {
	var width fixed.Int26_6
	for _, char := range text {
		width += t.glyph(char).advance
	}
	return width.Ceil()
}

// truncate returns the longest prefix of text made of whole characters that
// is at most maxWidth pixels wide.
func (t *dvdMenuText) truncate(text string, maxWidth int) string {
	var width fixed.Int26_6
	for offset, char := range text {
		width += t.glyph(char).advance
		if width > fixed.I(maxWidth) {
			return text[:offset]
		}
	}
	return text
}

// draw draws text with its top left corner at (x, y). Only pixels inside
// clip and the frame are changed. Bitmap glyphs set pixels to luma; outline
// glyphs blend towards it by their coverage of each pixel.
func (t *dvdMenuText) draw(frame *DecodedVideoFrame, x, y int, text string, luma byte, clip image.Rectangle) {
	clip = clip.Intersect(image.Rect(0, 0, frame.Width, frame.Height))
	set := func(x, y int) {
		if image.Pt(x, y).In(clip) {
			frame.Y[y*frame.Width+x] = luma
		}
	}
	pen := fixed.I(x)
	// previous is the Japanese bitmap glyph that ends where the next glyph
	// starts, if any.
	var previous *dvdMenuBitmapGlyph
	previousEnd := 0
	for _, char := range text {
		glyph := t.glyph(char)
		left := pen.Round()
		if glyph.kind != dvdMenuGlyphJapanese || previousEnd != left {
			previous = nil
		}
		switch glyph.kind {
		case dvdMenuGlyphSmall:
			for row, bits := range glyph.small {
				for column := range 5 {
					if bits&(1<<(4-column)) != 0 {
						for offset := range 4 {
							set(left+column*2+offset%2, y+row*2+offset/2)
						}
					}
				}
			}
		case dvdMenuGlyphJapanese:
			top, width := y-(dvdMenuJapaneseFontHeight-14)/2, glyph.japanese.width()
			for row := range dvdMenuJapaneseFontHeight {
				// Bit width-1-column is the pixel of a column; the bold
				// row also has column -1.
				var neighbour uint32
				if previous != nil {
					neighbour = previous.row(row)
				}
				bits := glyph.japanese.boldRow(row, neighbour)
				for column := -1; column < width; column++ {
					if bits&(1<<(width-1-column)) != 0 {
						set(left+column, top+row)
					}
				}
			}
			japanese := glyph.japanese
			previous, previousEnd = &japanese, left+width
		case dvdMenuGlyphOutline:
			segments, err := t.font.font.LoadGlyph(&t.buffer, glyph.outline, fixed.I(dvdMenuOutlineEm), nil)
			if err != nil {
				break // glyph loaded this outline before.
			}
			origin := fixed.Point26_6{X: pen, Y: fixed.I(y + dvdMenuOutlineBaseline)}
			t.rasterizer.setOutline(segments, origin, t.scaleNumerator, t.scaleDenominator)
			t.rasterizer.draw(clip, func(x, y int, coverage uint8) {
				pixel := &frame.Y[y*frame.Width+x]
				*pixel = byte(int64(*pixel) + divideRounded((int64(luma)-int64(*pixel))*int64(coverage), 255))
			})
		}
		pen += glyph.advance
	}
}

// drawDVDMenuText draws text with the built-in fonts anywhere in the frame.
func drawDVDMenuText(frame *DecodedVideoFrame, x, y int, text string, value byte) {
	newDVDMenuText(Profile{}).draw(frame, x, y, text, value, image.Rect(0, 0, frame.Width, frame.Height))
}
