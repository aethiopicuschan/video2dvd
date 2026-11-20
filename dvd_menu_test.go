package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

func TestDVDMenuButtonsAreDisjointAndReachable(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		start, end               int
		page, pageCount, pgcBase int
		aspect                   AspectRatio
		wantButtons              int
	}{
		{name: "single title", start: 0, end: 1, pageCount: 1, pgcBase: 2, aspect: AspectRatio4x3, wantButtons: 2},
		{name: "one full column", start: 0, end: 18, pageCount: 1, pgcBase: 2, aspect: AspectRatio4x3, wantButtons: 19},
		{name: "second column with one title", start: 0, end: 19, pageCount: 1, pgcBase: 2, aspect: AspectRatio4x3, wantButtons: 20},
		{name: "full single page", start: 0, end: 35, pageCount: 1, pgcBase: 2, aspect: AspectRatio4x3, wantButtons: 36},
		{name: "first page with 36 buttons", start: 0, end: 33, page: 0, pageCount: 2, pgcBase: 3, aspect: AspectRatio4x3, wantButtons: 36},
		{name: "short second page", start: 33, end: 35, page: 1, pageCount: 2, pgcBase: 3, aspect: AspectRatio4x3, wantButtons: 4},
		{name: "widescreen single page", start: 0, end: 17, pageCount: 1, pgcBase: 2, aspect: AspectRatio16x9, wantButtons: 18},
		{name: "widescreen first page", start: 0, end: 15, page: 0, pageCount: 2, pgcBase: 3, aspect: AspectRatio16x9, wantButtons: 18},
		{name: "widescreen middle page", start: 15, end: 30, page: 1, pageCount: 3, pgcBase: 4, aspect: AspectRatio16x9, wantButtons: 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buttons := ExportMakeDVDMenuButtons(tc.start, tc.end, Profile{Aspect: tc.aspect}, tc.page, tc.pageCount, tc.pgcBase)
			if len(buttons) != tc.wantButtons {
				t.Fatalf("button count = %d, want %d", len(buttons), tc.wantButtons)
			}
			for index, button := range buttons {
				if button.XStart < 0 || button.XEnd > 719 || button.XStart >= button.XEnd ||
					button.YStart < 0 || button.YEnd > 479 || button.YStart >= button.YEnd {
					t.Errorf("button %d rectangle (%d,%d)-(%d,%d) is outside the 720x480 raster", index+1, button.XStart, button.YStart, button.XEnd, button.YEnd)
				}
				// A field holds every other line, so an odd height would
				// highlight one more line in one field than in the other.
				if (button.YEnd-button.YStart+1)%2 != 0 {
					t.Errorf("button %d covers an odd number of lines", index+1)
				}
				for other := range index {
					o := buttons[other]
					if button.XStart <= o.XEnd && o.XStart <= button.XEnd && button.YStart <= o.YEnd && o.YStart <= button.YEnd {
						t.Errorf("buttons %d and %d overlap", other+1, index+1)
					}
				}
				for direction, target := range map[string]uint8{
					"up": button.Up, "down": button.Down,
					"left": button.Left, "right": button.Right,
				} {
					if target == 0 || int(target) > len(buttons) {
						t.Errorf("button %d %s target = %d, outside 1..%d", index+1, direction, target, len(buttons))
					}
				}
			}
			if t.Failed() {
				return
			}
			// The player selects button 1 first; the arrow keys must be
			// able to reach every other button from there.
			reached := map[uint8]bool{1: true}
			for queue := []uint8{1}; len(queue) > 0; queue = queue[1:] {
				button := buttons[queue[0]-1]
				for _, target := range []uint8{button.Up, button.Down, button.Left, button.Right} {
					if !reached[target] {
						reached[target] = true
						queue = append(queue, target)
					}
				}
			}
			if len(reached) != len(buttons) {
				t.Errorf("only %d of %d buttons are reachable from button 1", len(reached), len(buttons))
			}

			// Controls come first: PLAY ALL on the first page, then PREV
			// and NEXT when the menu has several pages. LinkPGCN operands
			// are one-based PGC numbers; the pages are PGCs 1..pageCount
			// and PLAY ALL is the PGC after them.
			linkTarget := func(button DVDNAVButton) int {
				if button.Command[0] != 0x20 || button.Command[1] != 0x04 {
					t.Fatalf("button command % x is not LinkPGCN", button.Command)
				}
				return int(button.Command[7])
			}
			next := 0
			if tc.page == 0 {
				if got := linkTarget(buttons[next]); got != tc.pageCount+1 {
					t.Errorf("PLAY ALL links to PGC %d, want %d", got, tc.pageCount+1)
				}
				next++
			}
			if tc.pageCount > 1 {
				wantPrevious := (tc.page+tc.pageCount-1)%tc.pageCount + 1
				wantNext := (tc.page+1)%tc.pageCount + 1
				if got := linkTarget(buttons[next]); got != wantPrevious {
					t.Errorf("PREV links to PGC %d, want page PGC %d", got, wantPrevious)
				}
				if got := linkTarget(buttons[next+1]); got != wantNext {
					t.Errorf("NEXT links to PGC %d, want page PGC %d", got, wantNext)
				}
				next += 2
			}
			for i, button := range buttons[next:] {
				if got, want := linkTarget(button), tc.pgcBase+tc.start+i+1; got != want {
					t.Errorf("title %d button links to PGC %d, want launcher PGC %d", tc.start+i+1, got, want)
				}
			}
		})
	}
}

func TestDVDMenuLabels(t *testing.T) {
	wide := Profile{Aspect: AspectRatio16x9}
	labels := ExportDVDMenuLabels(wide, []string{"holiday-trip", "Birthday_Party.2024", "", strings.Repeat("x", 80)}, 0, 0, 2)
	want := []string{"PLAY ALL", "PREV", "NEXT", "01 HOLIDAY-TRIP", "02 BIRTHDAY_PARTY.2024", "03", "04 " + strings.Repeat("X", 47)}
	if len(labels) != len(want) {
		t.Fatalf("labels = %q, want %q", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("label %d = %q, want %q", i, labels[i], want[i])
		}
	}
	if labels := ExportDVDMenuLabels(wide, []string{"a"}, 15, 1, 2); len(labels) != 3 || labels[0] != "PREV" || labels[2] != "16 A" {
		t.Errorf("second page labels = %q, want PREV, NEXT and title 16", labels)
	}

	// A label must end before its button does, also in the half-width
	// columns of a two-column 4:3 page, and count characters, not bytes.
	names := make([]string, 20)
	for i := range names {
		names[i] = strings.Repeat("é", 60)
	}
	standard := Profile{Aspect: AspectRatio4x3, FrameWidth: 720, FrameHeight: 480}
	labels = ExportDVDMenuLabels(standard, names, 0, 0, 1)
	_, buttons := ExportDVDMenuPage(standard, names, 0, 0, 1, 2)
	for i, button := range buttons {
		const charWidth, textInset = 12, 8
		if end := button.XStart + textInset + len([]rune(labels[i]))*charWidth; end > button.XEnd+1 {
			t.Errorf("label %d (%d characters) ends at x=%d, beyond its button at x=%d", i, len([]rune(labels[i])), end, button.XEnd)
		}
	}
	if got := len([]rune(labels[1])); got != 24 {
		t.Errorf("two-column title label has %d characters, want 24", got)
	}

	if got := ExportDVDMenuTitleName(EncodedTitle{VideoPath: filepath.Join("work", "My Holiday.m2v")}); got != "My Holiday" {
		t.Errorf("title name = %q, want the stream file name without extension", got)
	}
	// A title named by the converter shows that name, not its stream file.
	if got := ExportDVDMenuTitleName(EncodedTitle{Name: "Birthday 2024", VideoPath: filepath.Join("work", "video.m2v")}); got != "Birthday 2024" {
		t.Errorf("title name = %q, want the title's own name", got)
	}
	for path, want := range map[string]string{
		filepath.Join("clips", "My Holiday.mp4"): "My Holiday",
		"part.1.webm":                            "part.1",
		"noext":                                  "noext",
	} {
		if got := ExportMediaTitleName(path); got != want {
			t.Errorf("menu name of input %q = %q, want %q", path, got, want)
		}
	}
}

func TestDVDMenuFontCoversLabelsAndMarksUnsupportedCharacters(t *testing.T) {
	const cell = 12
	render := func(text string) [][]byte {
		frame := DecodedVideoFrame{Width: cell * len([]rune(text)), Height: 14}
		frame.Y = make([]byte, frame.Width*frame.Height)
		ExportDrawDVDMenuText(&frame, 0, 0, text, 255)
		cells := make([][]byte, len([]rune(text)))
		for i := range cells {
			for y := range frame.Height {
				cells[i] = append(cells[i], frame.Y[y*frame.Width+i*cell:y*frame.Width+(i+1)*cell]...)
			}
		}
		return cells
	}
	const supported = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
	questionMark := render("?")[0]
	seen := map[string]rune{}
	for i, glyph := range render(supported) {
		char := []rune(supported)[i]
		if bytes.Equal(glyph, questionMark) || bytes.Equal(glyph, make([]byte, len(glyph))) {
			t.Errorf("%q has no glyph of its own", char)
		}
		if other, ok := seen[string(glyph)]; ok {
			t.Errorf("%q and %q share a glyph", other, char)
		}
		seen[string(glyph)] = char
	}
	// Unsupported characters, including multi-byte ones, take one cell each.
	cells := render("A日é a/")
	if len(cells) != 6 {
		t.Fatalf("rendered %d cells, want 6", len(cells))
	}
	for _, index := range []int{1, 2, 4, 5} {
		if !bytes.Equal(cells[index], questionMark) {
			t.Errorf("cell %d of %q is not drawn as '?'", index, "A日é a/")
		}
	}
	if !bytes.Equal(cells[3], make([]byte, len(cells[3]))) {
		t.Error("space is not blank")
	}
}

func TestDVDMenuFrameDrawsLabelsInsideButtons(t *testing.T) {
	profile := Profile{Aspect: AspectRatio4x3, FrameWidth: 720, FrameHeight: 576}
	names := make([]string, 33)
	for i := range names {
		names[i] = "TITLE-NAME-" + strings.Repeat("W", i)
	}
	frame, buttons := ExportDVDMenuPage(profile, names, 0, 0, 2, 3)
	const background, heading = 24, 52
	inked := make([]int, len(buttons))
	for y := heading; y < frame.Height; y++ {
		for x := range frame.Width {
			if frame.Y[y*frame.Width+x] == background {
				continue
			}
			owner := -1
			for i, button := range buttons {
				if button.XStart <= x && x <= button.XEnd && button.YStart <= y && y <= button.YEnd {
					owner = i
				}
			}
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
}

// decodeTestSubpicture decodes a DVD subpicture unit into pixel codes and
// returns them with the commands of its single control sequence, keyed by
// command code.
func decodeTestSubpicture(t *testing.T, unit []byte, width, height int) ([][]byte, map[byte][]byte) {
	t.Helper()
	if int(binary.BigEndian.Uint16(unit[0:2])) != len(unit) {
		t.Fatalf("subpicture size field = %d, unit has %d bytes", binary.BigEndian.Uint16(unit[0:2]), len(unit))
	}
	control := int(binary.BigEndian.Uint16(unit[2:4]))
	if binary.BigEndian.Uint16(unit[control:control+2]) != 0 || int(binary.BigEndian.Uint16(unit[control+2:control+4])) != control {
		t.Fatalf("control sequence header % x is not one immediate, final sequence", unit[control:control+4])
	}
	commands := unit[control+4:]
	parsed := map[byte][]byte{}
	var offsets [2]int
	for i := 0; i < len(commands); {
		code := commands[i]
		switch code {
		case 0x00, 0x01, 0x02:
			parsed[code] = []byte{}
			i++
		case 0x03, 0x04:
			parsed[code] = commands[i+1 : i+3]
			i += 3
		case 0x05:
			area := commands[i+1 : i+7]
			parsed[code] = area
			x1, x2 := int(area[0])<<4|int(area[1])>>4, int(area[1]&0x0f)<<8|int(area[2])
			y1, y2 := int(area[3])<<4|int(area[4])>>4, int(area[4]&0x0f)<<8|int(area[5])
			if x1 != 0 || y1 != 0 || x2 != width-1 || y2 != height-1 {
				t.Fatalf("display area = (%d,%d)-(%d,%d), want the whole %dx%d frame", x1, y1, x2, y2, width, height)
			}
			i += 7
		case 0x06:
			parsed[code] = commands[i+1 : i+5]
			offsets[0] = int(binary.BigEndian.Uint16(commands[i+1 : i+3]))
			offsets[1] = int(binary.BigEndian.Uint16(commands[i+3 : i+5]))
			i += 5
		case 0xff:
			parsed[code] = []byte{}
			i = len(commands)
		default:
			t.Fatalf("unknown subpicture command %#x", commands[i])
		}
	}
	pixels := make([][]byte, height)
	for field := range 2 {
		position := offsets[field] * 2 // in nibbles
		nibble := func() int {
			value := unit[position/2]
			if position%2 == 0 {
				value >>= 4
			}
			position++
			return int(value & 0x0f)
		}
		for y := field; y < height; y += 2 {
			line := make([]byte, 0, width)
			for len(line) < width {
				code := nibble()
				if code < 0x4 {
					code = code<<4 | nibble()
					if code < 0x10 {
						code = code<<4 | nibble()
						if code < 0x40 {
							code = code<<4 | nibble()
						}
					}
				}
				count := code >> 2
				if count == 0 || count > width-len(line) {
					count = width - len(line)
				}
				line = append(line, bytes.Repeat([]byte{byte(code & 3)}, count)...)
			}
			position += position % 2 // lines are byte-aligned.
			pixels[y] = line
		}
		if field == 0 && position/2 > offsets[1] || field == 1 && position/2 > control {
			t.Fatalf("field %d pixel data overruns its area", field)
		}
	}
	return pixels, parsed
}

func TestDVDMenuSubpictureCoversButtonsInvisibly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		aspect  AspectRatio
		height  int
		titles  int
		pages   int
		pgcBase int
	}{
		{name: "NTSC widescreen", aspect: AspectRatio16x9, height: 480, titles: 2, pages: 1, pgcBase: 2},
		{name: "PAL widescreen full page", aspect: AspectRatio16x9, height: 576, titles: 17, pages: 1, pgcBase: 2},
		{name: "PAL two full columns", aspect: AspectRatio4x3, height: 576, titles: 33, pages: 2, pgcBase: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const width = 720
			buttons := ExportMakeDVDMenuButtons(0, tc.titles, Profile{Aspect: tc.aspect}, 0, tc.pages, tc.pgcBase)
			unit, err := ExportEncodeDVDMenuSubpicture(buttons, width, tc.height)
			if err != nil {
				t.Fatal(err)
			}
			// Pack header, PES header with PTS and P-STD buffer size, and
			// the substream byte leave 2016 bytes for the unit in one
			// 2048-byte pack.
			if len(unit) > 2016 || len(unit)%2 != 0 {
				t.Fatalf("subpicture unit has %d bytes, want an even size of at most 2016", len(unit))
			}
			pixels, commands := decodeTestSubpicture(t, unit, width, tc.height)
			for index, button := range buttons {
				for y := button.YStart; y <= button.YEnd; y++ {
					for x := button.XStart; x <= button.XEnd; x++ {
						if pixels[y][x] != 1 {
							t.Fatalf("pixel (%d,%d) inside button %d has code %d, want 1 (pattern)", x, y, index+1, pixels[y][x])
						}
					}
				}
			}
			for y, line := range pixels {
				crossed := false
				for _, button := range buttons {
					crossed = crossed || button.YStart <= y && y <= button.YEnd
				}
				// Some players treat a line of a single colour as empty
				// and crop it, so a button line must not be uniform.
				if crossed == bytes.Equal(line, make([]byte, width)) || crossed && line[0] != 0 {
					t.Fatalf("line %d: crossed by a button = %v, pixels start with % x", y, crossed, line[:8])
				}
			}
			// A menu subpicture is shown even when the user disabled
			// subpictures (forced start, 0x00) and is never stopped (0x02).
			_, forced := commands[0x00]
			_, started := commands[0x01]
			_, stopped := commands[0x02]
			_, ended := commands[0xff]
			if !forced || started || stopped || !ended {
				t.Errorf("display commands: forced start %v, normal start %v, stop %v, end %v", forced, started, stopped, ended)
			}
			// Pattern pixels use palette entry 1; every pixel code has
			// zero contrast, so only a highlight makes anything visible.
			if !bytes.Equal(commands[0x03], []byte{0x00, 0x10}) || !bytes.Equal(commands[0x04], []byte{0x00, 0x00}) {
				t.Errorf("SET_COLOR % x / SET_CONTR % x, want 00 10 / 00 00", commands[0x03], commands[0x04])
			}
		})
	}
	if _, err := ExportEncodeDVDMenuSubpicture(nil, 720, 481); err == nil {
		t.Fatal("expected an odd frame height to fail")
	}
}
