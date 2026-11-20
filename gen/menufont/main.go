// Command menufont builds the embedded Japanese menu font of video2dvd from
// the Shinonome bitmap font sources.
//
// It reads the gothic JIS X 0208 font and the JIS X 0201 half-width katakana
// of one pixel size straight out of the source archive, maps every glyph to
// its Unicode code point, and writes them as a DEFLATE-compressed table. The
// archive is only parsed as data: nothing in it is executed.
//
// This program lives in its own module so that the EUC-JP tables of
// golang.org/x/text, which are needed for the JIS X 0208 to Unicode mapping,
// do not become a dependency of video2dvd itself.
//
// Uncompressed layout, all integers big-endian:
//
//	"V2MF" version(1) cellHeight sectionCount
//	per section: cellWidth, glyphCount(2), then the glyphs' ascending code
//	points, each as its distance(2) from the one before (the first from
//	zero), then (cellWidth+7)/8 byte planes. Plane k holds byte
//	k of every row, cellHeight rows per glyph in code point order; a row's
//	most significant bit is its leftmost pixel. Keeping the left and the
//	right halves of the glyphs apart compresses better than whole rows.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
)

const (
	// Shinonome font 0.9.11, The Electronic Font Open Laboratory (/efont/).
	archiveURL    = "http://openlab.ring.gr.jp/efont/dist/shinonome/shinonome-0.9.11-src.tar.bz2"
	archiveSHA256 = "bc0b023096dcab99cccd61fe774e247b4e076a3c264c95f53e128dc23c79c729"
	archiveRoot   = "shinonome-0.9.11-src"

	formatVersion = 1
)

// aliases lists code points that share the glyph of another one. JIS X 0208
// has one cell for each of these pairs, and which of the two a text uses
// depends on the conversion table of the system that produced it (for
// example the wave dash U+301C against the full-width tilde U+FF5E).
var aliases = [][2]rune{
	{0x301c, 0xff5e}, // WAVE DASH, FULLWIDTH TILDE
	{0x2016, 0x2225}, // DOUBLE VERTICAL LINE, PARALLEL TO
	{0x2212, 0xff0d}, // MINUS SIGN, FULLWIDTH HYPHEN-MINUS
	{0x2014, 0x2015}, // EM DASH, HORIZONTAL BAR
	{0x00a2, 0xffe0}, // CENT SIGN, FULLWIDTH CENT SIGN
	{0x00a3, 0xffe1}, // POUND SIGN, FULLWIDTH POUND SIGN
	{0x00ac, 0xffe2}, // NOT SIGN, FULLWIDTH NOT SIGN
	{0x00a5, 0xffe5}, // YEN SIGN, FULLWIDTH YEN SIGN
}

type glyph struct {
	code rune
	rows []byte
}

func main() {
	log.SetFlags(0)
	archive := flag.String("archive", "", "path of shinonome-0.9.11-src.tar.bz2; downloaded from "+archiveURL+" when empty")
	size := flag.Int("size", 16, "glyph height in pixels: 12, 14 or 16")
	out := flag.String("out", "", "output file")
	flag.Parse()
	if *out == "" {
		log.Fatal("menufont: -out is required")
	}
	if err := run(*archive, *size, *out); err != nil {
		log.Fatalf("menufont: %v", err)
	}
}

func run(archivePath string, size int, out string) error {
	data, err := readArchive(archivePath)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != archiveSHA256 {
		return fmt.Errorf("archive sha256 is %x, want %s", sum, archiveSHA256)
	}
	prefix := archiveRoot + "/" + strconv.Itoa(size) + "/"
	sources, err := extract(data, prefix+"kanjic/font_src.bit", prefix+"hankaku/font_src_diff.bit")
	if err != nil {
		return err
	}
	full, err := parseBit(sources[0], size, size)
	if err != nil {
		return fmt.Errorf("kanjic: %w", err)
	}
	half, err := parseBit(sources[1], (size+1)/2, size)
	if err != nil {
		return fmt.Errorf("hankaku: %w", err)
	}
	fullGlyphs, err := mapJISX0208(full)
	if err != nil {
		return err
	}
	halfGlyphs := mapJISX0201Katakana(half)

	payload := []byte{'V', '2', 'M', 'F', formatVersion, byte(size), 2}
	payload = appendSection(payload, size, fullGlyphs)
	payload = appendSection(payload, (size+1)/2, halfGlyphs)

	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(out, compressed.Bytes(), 0o644); err != nil {
		return err
	}
	log.Printf("menufont: %d full-width and %d half-width glyphs, %d bytes (%d uncompressed, sha256 %x)",
		len(fullGlyphs), len(halfGlyphs), compressed.Len(), len(payload), sha256.Sum256(payload))
	return nil
}

func readArchive(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	client := http.Client{Timeout: 2 * time.Minute}
	response, err := client.Get(archiveURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", archiveURL, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, 64<<20))
}

// extract returns the contents of the named regular files of a .tar.bz2
// archive, in the order of names.
func extract(archive []byte, names ...string) ([][]byte, error) {
	files := make([][]byte, len(names))
	reader := tar.NewReader(bzip2.NewReader(bytes.NewReader(archive)))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		index := slices.Index(names, header.Name)
		if index < 0 || header.Typeflag != tar.TypeReg {
			continue
		}
		if files[index], err = io.ReadAll(io.LimitReader(reader, 64<<20)); err != nil {
			return nil, err
		}
	}
	for i, file := range files {
		if file == nil {
			return nil, fmt.Errorf("%s is not in the archive", names[i])
		}
	}
	return files, nil
}

// parseBit reads a Shinonome "bit" source: a BDF file whose BITMAP rows are
// drawn with '.' and '@' instead of hexadecimal digits. It returns the glyphs
// keyed by their ENCODING value, each as packed rows.
func parseBit(source []byte, width, height int) (map[int][]byte, error) {
	glyphs := map[int][]byte{}
	rowBytes := (width + 7) / 8
	encoding, inBitmap := -1, false
	var rows []byte
	scanner := bufio.NewScanner(bytes.NewReader(source))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimRight(scanner.Text(), " \t\r")
		fields := strings.Fields(text)
		switch {
		case inBitmap && strings.HasPrefix(text, "ENDCHAR"):
			if encoding < 0 || len(rows) != height*rowBytes {
				return nil, fmt.Errorf("line %d: glyph %d has %d rows, want %d", line, encoding, len(rows)/rowBytes, height)
			}
			if _, duplicate := glyphs[encoding]; duplicate {
				return nil, fmt.Errorf("line %d: glyph %d is defined twice", line, encoding)
			}
			glyphs[encoding] = rows
			encoding, inBitmap, rows = -1, false, nil
		case inBitmap:
			if strings.Trim(text, ".@") != "" {
				return nil, fmt.Errorf("line %d: bitmap row %q is not made of '.' and '@'", line, text)
			}
			// A few source rows are a pixel longer than the glyph box;
			// like bit2bdf, keep what is inside the box.
			text = text[:min(len(text), width)]
			row := make([]byte, rowBytes)
			for x, pixel := range []byte(text) {
				if pixel == '@' {
					row[x/8] |= 0x80 >> (x % 8)
				}
			}
			rows = append(rows, row...)
		case len(fields) == 2 && fields[0] == "ENCODING":
			value, err := strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			encoding = value
		case len(fields) == 5 && fields[0] == "BBX":
			if fields[1] != strconv.Itoa(width) || fields[2] != strconv.Itoa(height) {
				return nil, fmt.Errorf("line %d: glyph box %s x %s, want %d x %d", line, fields[1], fields[2], width, height)
			}
		case text == "BITMAP":
			inBitmap = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(glyphs) == 0 {
		return nil, errors.New("no glyphs")
	}
	return glyphs, nil
}

func blank(rows []byte) bool {
	return !slices.ContainsFunc(rows, func(b byte) bool { return b != 0 })
}

// mapJISX0208 keys the glyphs of a JIS X 0208 font by Unicode code point.
// The mapping is the EUC-JP table of golang.org/x/text, extended by aliases.
func mapJISX0208(source map[int][]byte) ([]glyph, error) {
	byCode := map[rune][]byte{}
	decoder := japanese.EUCJP.NewDecoder()
	for encoding, rows := range source {
		high, low := byte(encoding>>8), byte(encoding)
		if high < 0x21 || high > 0x7e || low < 0x21 || low > 0x7e {
			return nil, fmt.Errorf("glyph %#x is outside JIS X 0208", encoding)
		}
		decoded, err := decoder.Bytes([]byte{high | 0x80, low | 0x80})
		if err != nil {
			return nil, err
		}
		runes := []rune(string(decoded))
		if len(runes) != 1 || runes[0] == utf8.RuneError || runes[0] > 0xffff {
			log.Printf("menufont: skipping glyph %#x: not a JIS X 0208 character", encoding)
			continue
		}
		// The only blank cell that is a character is the ideographic space.
		if blank(rows) && runes[0] != 0x3000 {
			log.Printf("menufont: skipping glyph %#x (U+%04X): blank", encoding, runes[0])
			continue
		}
		byCode[runes[0]] = rows
	}
	for _, pair := range aliases {
		for i, code := range pair {
			if rows, ok := byCode[code]; ok {
				if _, taken := byCode[pair[1-i]]; !taken {
					byCode[pair[1-i]] = rows
				}
				break
			}
		}
	}
	return sorted(byCode), nil
}

// mapJISX0201Katakana returns the half-width katakana block 0xA1..0xDF as
// U+FF61..U+FF9F.
func mapJISX0201Katakana(source map[int][]byte) []glyph {
	byCode := map[rune][]byte{}
	for encoding, rows := range source {
		if encoding >= 0xa1 && encoding <= 0xdf && !blank(rows) {
			byCode[rune(0xff61+encoding-0xa1)] = rows
		}
	}
	return sorted(byCode)
}

func sorted(byCode map[rune][]byte) []glyph {
	glyphs := make([]glyph, 0, len(byCode))
	for code, rows := range byCode {
		glyphs = append(glyphs, glyph{code: code, rows: rows})
	}
	slices.SortFunc(glyphs, func(a, b glyph) int { return int(a.code - b.code) })
	return glyphs
}

func appendSection(payload []byte, width int, glyphs []glyph) []byte {
	payload = append(payload, byte(width))
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(glyphs)))
	previous := rune(0)
	for _, g := range glyphs {
		payload = binary.BigEndian.AppendUint16(payload, uint16(g.code-previous))
		previous = g.code
	}
	rowBytes := (width + 7) / 8
	for plane := range rowBytes {
		for _, g := range glyphs {
			for i := plane; i < len(g.rows); i += rowBytes {
				payload = append(payload, g.rows[i])
			}
		}
	}
	return payload
}
