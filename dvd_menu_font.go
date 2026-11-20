package video2dvd

import (
	"bytes"
	"compress/flate"
	_ "embed"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"sync"
)

// The embedded Japanese menu font is the 16-pixel gothic face of the
// Shinonome font family 0.9.11 by The Electronic Font Open Laboratory
// (/efont/), which is in the public domain; see third_party_licenses. It
// holds JIS X 0208 (kana, level 1 and 2 kanji, full-width forms and symbols)
// in 16x16 cells and the JIS X 0201 half-width katakana in 8x16 cells.
//
// gen/menufont converts the font's source archive into the embedded table and
// documents its layout. It is a module of its own, so its dependencies stay
// out of this one.
//
//go:generate go run -C gen/menufont . -out ../../dvd_menu_font_shinonome16.bin

//go:embed dvd_menu_font_shinonome16.bin
var dvdMenuJapaneseFontData []byte

const (
	dvdMenuJapaneseFontVersion = 1
	// dvdMenuJapaneseFontHeight is the cell height of the embedded font.
	dvdMenuJapaneseFontHeight = 16
)

// dvdMenuBitmapSection is the glyphs of one cell width.
type dvdMenuBitmapSection struct {
	width int
	// codes is ascending; glyph i belongs to codes[i].
	codes []uint16
	// planes[k] holds byte k of every glyph row, dvdMenuJapaneseFontHeight
	// rows per glyph.
	planes [][]byte
}

// dvdMenuBitmapGlyph is one glyph of the embedded font.
type dvdMenuBitmapGlyph struct {
	section *dvdMenuBitmapSection
	index   int
}

// width returns the cell width, which is also the advance.
func (g dvdMenuBitmapGlyph) width() int { return g.section.width }

// row returns one row of the glyph. Bit width-1 is the leftmost pixel and
// bit 0 the rightmost.
func (g dvdMenuBitmapGlyph) row(row int) uint32 {
	var bits uint32
	for _, plane := range g.section.planes {
		bits = bits<<8 | uint32(plane[g.index*dvdMenuJapaneseFontHeight+row])
	}
	return bits >> (len(g.section.planes)*8 - g.section.width)
}

// boldRow returns one row of the emboldened glyph, which is a pixel wider on
// the left: bit width is the pixel left of the cell. The rule is that of the
// bold faces of the Shinonome family: every pixel is repeated on its left
// unless that would close the one-pixel gap to the next stroke. previous is
// the same row of the glyph drawn directly to the left, or zero; its last two
// columns count as strokes too, so neighbouring glyphs do not fuse.
func (g dvdMenuBitmapGlyph) boldRow(row int, previous uint32) uint32 {
	width := g.width()
	bits := previous&3<<width | g.row(row)
	return (bits | bits<<1&^(bits>>1)) & (1<<(width+1) - 1)
}

// dvdMenuJapaneseFont decompresses the embedded font the first time a menu
// needs a glyph from it.
var dvdMenuJapaneseFont = sync.OnceValues(func() ([]dvdMenuBitmapSection, error) {
	return decodeDVDMenuBitmapFont(dvdMenuJapaneseFontData)
})

func decodeDVDMenuBitmapFont(compressed []byte) ([]dvdMenuBitmapSection, error) {
	errCorrupt := errors.New("video2dvd: embedded menu font is corrupt")
	data, err := io.ReadAll(flate.NewReader(bytes.NewReader(compressed)))
	if err != nil || len(data) < 7 || string(data[:4]) != "V2MF" || data[4] != dvdMenuJapaneseFontVersion || data[5] != dvdMenuJapaneseFontHeight {
		return nil, errCorrupt
	}
	sections := make([]dvdMenuBitmapSection, data[6])
	data = data[7:]
	for i := range sections {
		if len(data) < 3 {
			return nil, errCorrupt
		}
		width, count := int(data[0]), int(binary.BigEndian.Uint16(data[1:3]))
		data = data[3:]
		planeCount, planeSize := (width+7)/8, count*dvdMenuJapaneseFontHeight
		if width == 0 || len(data) < count*2+planeCount*planeSize {
			return nil, errCorrupt
		}
		section := dvdMenuBitmapSection{width: width, codes: make([]uint16, count)}
		var code uint16
		for j := range section.codes {
			code += binary.BigEndian.Uint16(data[j*2:])
			section.codes[j] = code
		}
		data = data[count*2:]
		for range planeCount {
			section.planes = append(section.planes, data[:planeSize])
			data = data[planeSize:]
		}
		sections[i] = section
	}
	return sections, nil
}

// lookupDVDMenuJapaneseGlyph returns the embedded font's glyph of a
// character.
func lookupDVDMenuJapaneseGlyph(char rune) (dvdMenuBitmapGlyph, bool) {
	// The font starts at U+00A2; ASCII never needs it.
	if char < 0x80 || char > 0xffff {
		return dvdMenuBitmapGlyph{}, false
	}
	sections, err := dvdMenuJapaneseFont()
	if err != nil {
		// The data is part of the program and covered by the tests; without
		// it every character is simply one the font does not have.
		return dvdMenuBitmapGlyph{}, false
	}
	for i := range sections {
		if index, found := slices.BinarySearch(sections[i].codes, uint16(char)); found {
			return dvdMenuBitmapGlyph{section: &sections[i], index: index}, true
		}
	}
	return dvdMenuBitmapGlyph{}, false
}
