package video2dvd

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"slices"
	"strings"
	"time"

	"golang.org/x/image/math/fixed"
)

// DVD subtitles are subpicture streams: run-length coded pictures of two bits
// per pixel that a player lays over the video. Each subpicture unit (SPU)
// holds one picture and the commands that show it at the unit's
// presentation time and hide it again after a delay.
//
// The text of a subtitle is drawn in white with a black outline, centred at
// the bottom of the title-safe area, on a transparent background.
const (
	// dvdSubtitleMaxStreams is the number of subpicture streams of a title.
	dvdSubtitleMaxStreams = 32
	// dvdSubtitleMaxUnitBytes is the size of the subpicture decoder buffer,
	// which a unit must fit.
	dvdSubtitleMaxUnitBytes = 53220
	// dvdSubtitleDelayTicks is the unit of the delay of a display control
	// sequence (SP_DCSQ_STM): 1024 ticks of the 90 kHz clock.
	dvdSubtitleDelayTicks = 1024
	// dvdSubtitleMaxDelay is the largest delay a control sequence holds. A
	// subtitle shown for longer is split into several units.
	dvdSubtitleMaxDelay = 0xffff
	// dvdSubtitleRecordHeader is the size of the presentation time that
	// precedes each unit in a subtitle stream file.
	dvdSubtitleRecordHeader = 8

	// Text is drawn at twice the size of the menu text, 32 lines for a line
	// of Japanese, and lines follow each other at dvdSubtitleLinePitch times
	// the scale. A subtitle that needs more than dvdSubtitleMaxLines lines is
	// drawn at the menu's size instead, with twice as many lines; lines
	// beyond that are left out.
	dvdSubtitleScale     = 2
	dvdSubtitleLinePitch = 18
	dvdSubtitleMaxLines  = 4
	// dvdSubtitleOutline is the width of the outline in pixels.
	dvdSubtitleOutline = 2

	// Pixel codes of a subpicture, which SET_COLOR maps to palette entries
	// and SET_CONTR to opacities.
	dvdSubtitleBackground = 0 // transparent.
	dvdSubtitleText       = 1 // the text: palette entry 1, white.
	dvdSubtitleOutlined   = 2 // the outline: palette entry 2, black.
	dvdSubtitleAntialias  = 3 // edges of outline glyphs: palette entry 3, grey.
	// dvdSubtitleTextCoverage is the coverage from which a pixel of an
	// outline glyph counts as text rather than as its edge.
	dvdSubtitleTextCoverage = 160
)

// dvdSubtitlePalette is the colour table of a title PGC with subtitles. Each
// entry is a reserved byte followed by Y, Cr and Cb.
var dvdSubtitlePalette = [16][4]byte{
	0: {0, 16, 128, 128},
	1: {0, 235, 128, 128},
	2: {0, 16, 128, 128},
	3: {0, 128, 128, 128},
}

// dvdSubtitleEvent is a time during which one text is shown; the events of a
// stream do not overlap.
type dvdSubtitleEvent struct {
	start, end time.Duration
	text       string
}

// flattenSubtitleCues turns cues, which may overlap, into events that do not:
// while several cues are shown together their texts are shown as one, in
// the order the cues started, the same text only once. Cues that end before
// they start or have no text are left out.
func flattenSubtitleCues(cues []SubtitleCue) []dvdSubtitleEvent {
	type entry struct {
		cue   SubtitleCue
		order int
	}
	entries := make([]entry, 0, len(cues))
	bounds := make([]time.Duration, 0, 2*len(cues))
	for i, cue := range cues {
		if cue.End <= cue.Start || cue.Text == "" {
			continue
		}
		entries = append(entries, entry{cue, i})
		bounds = append(bounds, cue.Start, cue.End)
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		if a.cue.Start != b.cue.Start {
			return cmp.Compare(a.cue.Start, b.cue.Start)
		}
		return a.order - b.order
	})
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)
	var events []dvdSubtitleEvent
	var active []entry
	next := 0
	for i := 0; i+1 < len(bounds); i++ {
		from, to := bounds[i], bounds[i+1]
		active = slices.DeleteFunc(active, func(e entry) bool { return e.cue.End <= from })
		for next < len(entries) && entries[next].cue.Start <= from {
			active = append(active, entries[next])
			next++
		}
		var texts []string
		for _, e := range active {
			if !slices.Contains(texts, e.cue.Text) {
				texts = append(texts, e.cue.Text)
			}
		}
		if len(texts) == 0 {
			continue
		}
		text := strings.Join(texts, "\n")
		if last := len(events) - 1; last >= 0 && events[last].end == from && events[last].text == text {
			events[last].end = to
			continue
		}
		events = append(events, dvdSubtitleEvent{start: from, end: to, text: text})
	}
	return events
}

// dvdSubtitleImage is the picture of one subtitle: its display area on the
// frame, whose top line is even and whose height is even, and the run-length
// coded lines of its two fields.
type dvdSubtitleImage struct {
	area   image.Rectangle
	fields [2][]byte
}

// dvdSubtitleRenderer draws the subtitles of one profile and keeps the
// picture of every text it has drawn.
type dvdSubtitleRenderer struct {
	width, height int
	text, small   *dvdMenuText
	images        map[string]*dvdSubtitleImage
}

func newDVDSubtitleRenderer(profile Profile) *dvdSubtitleRenderer {
	return &dvdSubtitleRenderer{
		width:  profile.FrameWidth,
		height: profile.FrameHeight,
		text:   newDVDScaledText(profile, dvdSubtitleScale),
		small:  newDVDScaledText(profile, 1),
		images: map[string]*dvdSubtitleImage{},
	}
}

// image returns the picture of a subtitle text.
func (r *dvdSubtitleRenderer) image(text string) (*dvdSubtitleImage, error) {
	if picture, ok := r.images[text]; ok {
		return picture, nil
	}
	maxWidth := r.width * 8 / 10
	drawer, lines := r.text, r.layout(r.text, text, maxWidth)
	if len(lines) > dvdSubtitleMaxLines {
		drawer, lines = r.small, r.layout(r.small, text, maxWidth)
		lines = lines[:min(len(lines), dvdSubtitleMaxLines*dvdSubtitleScale)]
	}
	picture, err := r.draw(drawer, lines)
	if err != nil {
		return nil, err
	}
	r.images[text] = picture
	return picture, nil
}

// layout breaks the lines of a text so that each fits maxWidth.
func (r *dvdSubtitleRenderer) layout(drawer *dvdMenuText, text string, maxWidth int) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		lines = append(lines, wrapSubtitleLine(drawer, drawer.normalize(line), maxWidth)...)
	}
	return lines
}

// draw draws lines centred above the bottom of the title-safe area and
// codes the picture.
func (r *dvdSubtitleRenderer) draw(drawer *dvdMenuText, lines []string) (*dvdSubtitleImage, error) {
	scale := drawer.scale
	pitch := dvdSubtitleLinePitch * scale
	// A line's glyphs reach from two lines (times the scale) above its text
	// position to sixteen below it, like the rows of the menu.
	bottom := (r.height - r.height/10) &^ 1
	top := bottom - len(lines)*pitch
	// Only the lines of the frame around the text are drawn; the canvas
	// starts on an even frame line, so that its fields are the frame's.
	canvasTop := max(0, top-2*dvdSubtitleOutline) &^ 1
	canvasBottom := min(r.height, bottom+2*dvdSubtitleOutline)
	canvas := DecodedVideoFrame{Width: r.width, Height: canvasBottom - canvasTop, Y: make([]byte, r.width*(canvasBottom-canvasTop))}
	for i, line := range lines {
		x := (r.width - drawer.width(line)) / 2
		y := top + i*pitch + 2*scale - canvasTop
		drawer.draw(&canvas, x, y, line, 255, image.Rect(0, 0, canvas.Width, canvas.Height))
	}
	picture := encodeDVDSubtitleImage(canvas)
	if picture != nil {
		picture.area = picture.area.Add(image.Pt(0, canvasTop))
	}
	return picture, nil
}

// encodeDVDSubtitleImage turns the coverage of the text in the luma plane of
// a canvas into pixel codes, adds the outline and run-length codes the
// smallest area that holds them. It returns nil for a canvas without text.
// The canvas height is even.
func encodeDVDSubtitleImage(canvas DecodedVideoFrame) *dvdSubtitleImage {
	width, height := canvas.Width, canvas.Height
	codes := make([]byte, width*height)
	bounds := image.Rectangle{}
	for y := range height {
		for x := range width {
			coverage := canvas.Y[y*width+x]
			if coverage == 0 {
				continue
			}
			code := byte(dvdSubtitleText)
			if coverage < dvdSubtitleTextCoverage {
				code = dvdSubtitleAntialias
			}
			codes[y*width+x] = code
			bounds = bounds.Union(image.Rect(x, y, x+1, y+1))
		}
	}
	if bounds.Empty() {
		return nil
	}
	// The outline is the text dilated by a square, in two passes.
	const reach = dvdSubtitleOutline
	bounds = image.Rect(bounds.Min.X-reach, bounds.Min.Y-reach, bounds.Max.X+reach, bounds.Max.Y+reach).Intersect(image.Rect(0, 0, width, height))
	wide := make([]bool, width*height)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			for dx := -reach; dx <= reach; dx++ {
				if nx := x + dx; nx >= 0 && nx < width && codes[y*width+nx] != dvdSubtitleBackground {
					wide[y*width+x] = true
					break
				}
			}
		}
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if codes[y*width+x] != dvdSubtitleBackground {
				continue
			}
			for dy := -reach; dy <= reach; dy++ {
				if ny := y + dy; ny >= 0 && ny < height && wide[ny*width+x] {
					codes[y*width+x] = dvdSubtitleOutlined
					break
				}
			}
		}
	}
	// The area starts on an even line and has as many lines in each field;
	// the canvas height is even, so there is room for that.
	bounds.Min.Y &^= 1
	if bounds.Dy()%2 != 0 {
		bounds.Max.Y++
	}
	picture := &dvdSubtitleImage{area: bounds}
	for field := range 2 {
		for y := bounds.Min.Y + field; y < bounds.Max.Y; y += 2 {
			var line dvdSubpictureLine
			row := codes[y*width+bounds.Min.X : y*width+bounds.Max.X]
			for x := 0; x < len(row); {
				run := 1
				for x+run < len(row) && row[x+run] == row[x] {
					run++
				}
				if x+run == len(row) {
					line.fill(uint32(row[x]))
				} else {
					line.run(run, uint32(row[x]))
				}
				x += run
			}
			picture.fields[field] = append(picture.fields[field], line.data...)
		}
	}
	return picture
}

// encodeDVDSubtitleUnit returns the subpicture unit that shows picture from
// its presentation time on and hides it after delay units of 1024 ticks.
func encodeDVDSubtitleUnit(picture *dvdSubtitleImage, delay int) ([]byte, error) {
	if delay < 1 || delay > dvdSubtitleMaxDelay {
		return nil, errors.New("video2dvd: subtitle display time is outside the range of a subpicture unit")
	}
	const headerSize = 4
	unit := make([]byte, headerSize, headerSize+len(picture.fields[0])+len(picture.fields[1])+40)
	topField := len(unit)
	unit = append(unit, picture.fields[0]...)
	bottomField := len(unit)
	unit = append(unit, picture.fields[1]...)
	// Control sequences start on an even offset.
	if len(unit)%2 != 0 {
		unit = append(unit, 0)
	}
	first := len(unit)
	const firstSize = 4 + 1 + 3 + 3 + 7 + 5 + 1
	second := first + firstSize + firstSize%2
	area := picture.area
	left, right, top, bottom := area.Min.X, area.Max.X-1, area.Min.Y, area.Max.Y-1
	unit = append(unit,
		0, 0, // shown at the unit's presentation time,
		byte(second>>8), byte(second), // followed by the sequence that hides it.
		0x01,             // STA_DSP: show, unless the viewer turned subtitles off.
		0x03, 0x32, 0x10, // SET_COLOR: antialias 3, outline 2, text 1, background 0.
		0x04, 0xff, 0xf0, // SET_CONTR: all opaque but the background.
		0x05, byte(left>>4), byte(left<<4|right>>8), byte(right), byte(top>>4), byte(top<<4|bottom>>8), byte(bottom), // SET_DAREA.
		0x06, byte(topField>>8), byte(topField), byte(bottomField>>8), byte(bottomField), // SET_DSPXA.
		0xff, // end of the sequence.
	)
	if len(unit) != second {
		unit = append(unit, 0xff)
	}
	unit = append(unit,
		byte(delay>>8), byte(delay), // delay,
		byte(second>>8), byte(second), // the last sequence, which points at itself.
		0x02, // STP_DSP: hide.
		0xff,
	)
	if len(unit) > dvdSubtitleMaxUnitBytes {
		return nil, fmt.Errorf("video2dvd: subtitle picture of %d bytes exceeds the %d bytes of the subpicture buffer", len(unit), dvdSubtitleMaxUnitBytes)
	}
	binary.BigEndian.PutUint16(unit[0:2], uint16(len(unit)))
	binary.BigEndian.PutUint16(unit[2:4], uint16(first))
	return unit, nil
}

// dvdSubtitleTicks converts a time on the title timeline to 90 kHz ticks,
// rounding down.
func dvdSubtitleTicks(t time.Duration) int64 {
	return int64(t/time.Second)*90_000 + int64(t%time.Second)*90_000/int64(time.Second)
}

// WriteDVDSubtitleStream draws subtitle cues for a title as a DVD subtitle
// stream file, the form EncodedSubtitle.Path refers to. Cue times are on the
// title timeline, which starts when the first picture of the title is shown,
// and the cues are cut to the title's duration. Cues may overlap: while
// several are shown together, their texts are shown one below the other.
//
// The text is drawn with profile.MenuFont, or with the built-in fonts when
// it is nil, as on the title menu: without a MenuFont, ASCII letters are
// shown in upper case. It is drawn in white with a black outline, centred at
// the bottom of the title-safe area, in lines broken to fit 80% of the
// frame width; Japanese text may break between any two characters except
// before closing punctuation and small kana.
//
// The file is a sequence of subpicture units in presentation order, each
// preceded by its presentation time as a big-endian 64-bit count of 90 kHz
// ticks from the start of the title. Every unit shows its picture at that
// time and hides it before the next unit is presented.
func WriteDVDSubtitleStream(w io.Writer, cues []SubtitleCue, duration time.Duration, profile Profile) error {
	if w == nil {
		return errors.New("video2dvd: subtitle stream writer is required")
	}
	if err := validateAuthoringProfile(profile); err != nil {
		return err
	}
	return writeDVDSubtitleStream(w, flattenSubtitleCues(cues), duration, newDVDSubtitleRenderer(profile))
}

// writeDVDSubtitleStream writes the units of events, which must not overlap,
// cut to [0, duration).
func writeDVDSubtitleStream(w io.Writer, events []dvdSubtitleEvent, duration time.Duration, renderer *dvdSubtitleRenderer) error {
	if duration <= 0 {
		return errors.New("video2dvd: subtitle stream needs a positive title duration")
	}
	buffered := bufio.NewWriter(w)
	limit := dvdSubtitleTicks(duration)
	previous := int64(-1)
	for _, event := range events {
		start, end := max(dvdSubtitleTicks(max(event.start, 0)), previous+1), min(dvdSubtitleTicks(min(event.end, duration)), limit)
		picture, err := renderer.image(event.text)
		if err != nil {
			return err
		}
		if picture == nil {
			continue
		}
		// A subtitle that lasts longer than a control sequence can wait is
		// shown again by a further unit; one that lasts less than one
		// delay unit is too short to be shown.
		for end-start >= dvdSubtitleDelayTicks {
			delay := min((end-start)/dvdSubtitleDelayTicks, dvdSubtitleMaxDelay)
			unit, err := encodeDVDSubtitleUnit(picture, int(delay))
			if err != nil {
				return err
			}
			var header [dvdSubtitleRecordHeader]byte
			binary.BigEndian.PutUint64(header[:], uint64(start))
			if _, err := buffered.Write(header[:]); err != nil {
				return err
			}
			if _, err := buffered.Write(unit); err != nil {
				return err
			}
			previous = start
			start += delay * dvdSubtitleDelayTicks
		}
	}
	return buffered.Flush()
}

// dvdSubtitleUnit is one unit of a subtitle stream file.
type dvdSubtitleUnit struct {
	// pts is the presentation time in 90 kHz ticks from the title start.
	pts  uint64
	data []byte
}

// dvdSubtitleStreamReader reads the units of a subtitle stream file and
// checks them.
type dvdSubtitleStreamReader struct {
	r        *bufio.Reader
	previous uint64
	started  bool
}

func newDVDSubtitleStreamReader(r io.Reader) *dvdSubtitleStreamReader {
	return &dvdSubtitleStreamReader{r: bufio.NewReader(r)}
}

// next returns the next unit, or io.EOF after the last one.
func (s *dvdSubtitleStreamReader) next() (dvdSubtitleUnit, error) {
	var header [dvdSubtitleRecordHeader + 4]byte
	if _, err := io.ReadFull(s.r, header[:]); err != nil {
		if err == io.EOF {
			return dvdSubtitleUnit{}, io.EOF
		}
		return dvdSubtitleUnit{}, errors.New("video2dvd: subtitle stream ends inside a unit")
	}
	pts := binary.BigEndian.Uint64(header[:dvdSubtitleRecordHeader])
	size := int(binary.BigEndian.Uint16(header[dvdSubtitleRecordHeader:]))
	control := int(binary.BigEndian.Uint16(header[dvdSubtitleRecordHeader+2:]))
	if size < 4 || size > dvdSubtitleMaxUnitBytes || control < 4 || control+4 > size {
		return dvdSubtitleUnit{}, errors.New("video2dvd: subtitle stream holds an invalid subpicture unit")
	}
	if s.started && pts <= s.previous {
		return dvdSubtitleUnit{}, errors.New("video2dvd: subtitle stream units are not in presentation order")
	}
	if pts > maxMPEGTimestamp/2 {
		return dvdSubtitleUnit{}, errors.New("video2dvd: subtitle presentation time is out of range")
	}
	data := make([]byte, size)
	copy(data, header[dvdSubtitleRecordHeader:])
	if _, err := io.ReadFull(s.r, data[4:]); err != nil {
		return dvdSubtitleUnit{}, errors.New("video2dvd: subtitle stream ends inside a unit")
	}
	s.previous, s.started = pts, true
	return dvdSubtitleUnit{pts: pts, data: data}, nil
}

// dvdSubtitleStreamSectors returns the number of sectors that multiplexing
// the units of a subtitle stream file takes at most: every unit starts a
// pack, and a pack carries a little less than a sector of it.
func dvdSubtitleStreamSectors(r io.Reader) (int64, error) {
	reader := newDVDSubtitleStreamReader(r)
	var sectors int64
	for {
		unit, err := reader.next()
		if err == io.EOF {
			return sectors, nil
		}
		if err != nil {
			return 0, err
		}
		sectors += dvdSubtitleUnitSectors(len(unit.data))
	}
}

// dvdSubtitleUnitSectors returns the packs a unit of size bytes needs at
// most: each carries the substream number and, in the first, a time stamp
// and a buffer size.
func dvdSubtitleUnitSectors(size int) int64 {
	capacity := mpegPESHeader{hasPTS: true, pstdBuffer: dvdSubpicturePSTDBuffer}.capacity() - 1
	return int64((size + capacity - 1) / capacity)
}

// wrapSubtitleLine breaks one line of text into lines no wider than
// maxWidth, at spaces and between the characters of Japanese, Chinese and
// Korean text. Of the fewest lines it can, it makes them as even in width
// as greedy breaking at a narrower width allows, so that two lines are
// about equally long instead of a full one and a short remainder.
func wrapSubtitleLine(drawer *dvdMenuText, line string, maxWidth int) []string {
	runes := []rune(line)
	if len(runes) == 0 {
		return nil
	}
	advances := make([]fixed.Int26_6, len(runes))
	for i, char := range runes {
		advances[i] = drawer.glyph(char).advance
	}
	lines := breakSubtitleLine(runes, advances, fixed.I(maxWidth))
	if len(lines) > 1 {
		low, high := fixed.Int26_6(0), fixed.I(maxWidth)
		for high-low > 64 {
			width := (low + high) / 2
			if len(breakSubtitleLine(runes, advances, width)) <= len(lines) {
				high = width
			} else {
				low = width
			}
		}
		if even := breakSubtitleLine(runes, advances, high); len(even) <= len(lines) {
			lines = even
		}
	}
	return lines
}

// breakSubtitleLine breaks runes greedily into lines no wider than
// maxWidth. A line ends at the last place it may break, or after a space or
// punctuation if that comes no earlier than 60% of the way along. A word
// wider than a line is broken between any two characters.
func breakSubtitleLine(runes []rune, advances []fixed.Int26_6, maxWidth fixed.Int26_6) []string {
	var lines []string
	start := 0
	for start < len(runes) {
		for start < len(runes) && runes[start] == ' ' {
			start++
		}
		if start == len(runes) {
			break
		}
		end, lastBreak, lastStrong := start, -1, -1
		var width, strongWidth fixed.Int26_6
		for end < len(runes) {
			if end > start && subtitleBreakAllowed(runes, end) {
				lastBreak = end
				if runes[end-1] == ' ' || strings.ContainsRune(subtitleStrongBreakAfter, runes[end-1]) {
					lastStrong, strongWidth = end, width
				}
			}
			if width+advances[end] > maxWidth && end > start {
				break
			}
			width += advances[end]
			end++
		}
		if end < len(runes) {
			switch {
			case runes[end] == ' ':
				// The line is full up to a space, which is a break.
			case lastStrong > start && strongWidth*10 >= maxWidth*6:
				end = lastStrong
			case lastBreak > start:
				end = lastBreak
			}
		}
		lines = append(lines, strings.TrimRight(string(runes[start:end]), " "))
		start = end
	}
	return lines
}

// subtitleBreakAllowed reports whether a line may break before runes[i]:
// after a space, or next to a character of Japanese, Chinese or Korean
// text, but never before closing punctuation or a small kana, after
// opening punctuation, or inside a word.
func subtitleBreakAllowed(runes []rune, i int) bool {
	before, after := runes[i-1], runes[i]
	if after == ' ' {
		return false
	}
	if before == ' ' {
		return true
	}
	if strings.ContainsRune(subtitleNoBreakBefore, after) || strings.ContainsRune(subtitleNoBreakAfter, before) {
		return false
	}
	return isSubtitleWideRune(before) || isSubtitleWideRune(after)
}

const (
	// subtitleStrongBreakAfter holds the punctuation after which a line
	// breaks best.
	subtitleStrongBreakAfter = "、。，．！？」』）!?,.;:"
	subtitleNoBreakBefore    = "、。，．・：；？！ゝゞ々ー～」』）］｝〉》】〕〙〗〟’”ぁぃぅぇぉっゃゅょゎゕゖァィゥェォッャュョヮヵヶ…‥!?,.:;)]}%"
	subtitleNoBreakAfter     = "「『（［｛〈《【〔〘〖〝‘“([{"
)

// isSubtitleWideRune reports whether a character belongs to the scripts that
// are broken between characters: CJK ideographs, kana, Hangul, and CJK
// symbols and full-width forms.
func isSubtitleWideRune(char rune) bool {
	switch {
	case char >= 0x1100 && char <= 0x11ff, // Hangul jamo.
		char >= 0x2e80 && char <= 0x9fff,   // CJK radicals to unified ideographs, kana, CJK symbols.
		char >= 0xac00 && char <= 0xd7af,   // Hangul syllables.
		char >= 0xf900 && char <= 0xfaff,   // CJK compatibility ideographs.
		char >= 0xfe30 && char <= 0xfe4f,   // CJK compatibility forms.
		char >= 0xff00 && char <= 0xffef,   // half-width and full-width forms.
		char >= 0x20000 && char <= 0x3ffff: // supplementary ideographs.
		return true
	}
	return false
}
