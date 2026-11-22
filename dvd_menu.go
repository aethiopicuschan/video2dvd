package video2dvd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	dvdMenuMaxButtons    = 36
	dvdMenuRowsPerColumn = 18

	// Menu geometry on the 720-pixel-wide DVD raster. The built-in 5x7 font
	// is drawn with 12-pixel character cells and 14-pixel-high glyphs; a
	// button rectangle extends dvdMenuButtonMarginX left and right of its
	// text column and covers the 18 lines from two above those glyphs to
	// two below. Japanese and outline glyphs use up to 16 of these lines.
	dvdMenuCharWidth     = 12
	dvdMenuHeadingX      = 48
	dvdMenuHeadingY      = 24
	dvdMenuHeadingWidth  = 720 - 2*dvdMenuHeadingX
	dvdMenuControlY      = 54
	dvdMenuFirstRowY     = 94
	dvdMenuRowStep       = 20
	dvdMenuButtonMarginX = 8
	dvdMenuButtonTop     = 2
	dvdMenuButtonBottom  = 15
	dvdMenuLeftColumnX   = 56
	dvdMenuRightColumnX  = 376
	dvdMenuWideColumnEnd = 671
	dvdMenuLeftColumnEnd = 351
	dvdMenuPlayAllX      = 56
	dvdMenuPreviousX     = 440
	dvdMenuNextX         = 560
	// dvdMenuControlGap is the least distance between two control buttons.
	dvdMenuControlGap = 2

	// dvdMenuPagePictures is the number of pictures of a menu page: the page
	// is shown for that long, as one closed GOP and therefore one VOBU, and
	// silent audio of the same length plays with it. The cell is a still
	// cell, so the last picture stays on screen until a button is chosen.
	// macOS DVD Player needs both: its screen stayed black for a menu of
	// one picture, with 2 ms of audio or with audio as long as the
	// picture, and for 15 pictures with 2 ms of audio; it showed 15
	// pictures with audio as long as them.
	dvdMenuPagePictures = 15

	// dvdMenuSubpictureSubstream is the private_stream_1 substream of the
	// first DVD subpicture stream.
	dvdMenuSubpictureSubstream = 0x20
	// A subpicture unit plus its substream byte must fit the payload of one
	// 2048-byte pack whose PES header carries a PTS and the P-STD buffer
	// size.
	dvdMenuSubpictureMaxUnit = DVDVOBSectorSize - mpegPackHeaderSize - mpegPESFixedHeaderSize - 5 - 3 - 1

	// Display types of a button group and its subpicture stream.
	dvdButtonGroupNormal    = 0 // 4:3 video.
	dvdButtonGroupWide      = 1 // 16:9 video on a 16:9 display.
	dvdButtonGroupLetterbox = 2 // 16:9 video letterboxed on a 4:3 display.
)

// dvdMenuEntry is one selectable item of a menu page. The label is drawn at
// (textX, textY) and the button rectangle spans left..right horizontally.
type dvdMenuEntry struct {
	label        string
	textX, textY int
	left, right  int
	command      [8]byte
}

// dvdMenuPage is the layout of one menu page. The picture, the button
// rectangles and the button navigation are all derived from it so that they
// cannot disagree.
type dvdMenuPage struct {
	// controls holds PLAY ALL on the first page and PREV/NEXT on every page
	// of a multi-page menu, left to right.
	controls []dvdMenuEntry
	// titles is in column-major order, dvdMenuRowsPerColumn per column.
	titles []dvdMenuEntry
}

// createDVDMenuPages draws and encodes the pages of the title menu. Every
// page is a title of its own, with its own MPEG-2 sequence and its own
// silent LPCM audio, so that each can be multiplexed as a separate VOB. It
// returns the pages and the highlight buttons of each.
func createDVDMenuPages(ctx context.Context, titles []EncodedTitle, profile Profile, workDir string) ([]EncodedTitle, [][]DVDNAVButton, error) {
	maxButtons := dvdMenuMaxButtons
	if profile.Aspect == AspectRatio16x9 {
		maxButtons = dvdMenuRowsPerColumn
	}
	maxButtons-- // reserve PLAY ALL on the first page
	// Two buttons are reserved for previous/next page navigation when the
	// menu spans more than one page.
	if len(titles) > maxButtons {
		maxButtons -= 2
	}
	if len(titles) == 0 {
		return nil, nil, errors.New("video2dvd: automatically generated menu requires at least one title")
	}
	pages := (len(titles) + maxButtons - 1) / maxButtons
	// PGCs are ordered as menu pages, PLAY ALL, then one title launcher per
	// title and one PLAY ALL continuation per title.
	titleLaunchBase := pages + 1
	names := make([]string, len(titles))
	for i, title := range titles {
		names[i] = dvdMenuTitleName(title)
	}
	text := newDVDMenuText(profile)
	// Every page has the same silent audio.
	silence := make([]byte, dvdMenuPageAudioSamples(profile.Standard)*dvdLPCMStereoSampleBytes)
	menuPages := make([]EncodedTitle, 0, pages)
	buttonPages := make([][]DVDNAVButton, 0, pages)
	for page := range pages {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		start := page * maxButtons
		end := min(len(titles), start+maxButtons)
		layout := layoutDVDMenuPage(text, names[start:end], start, profile.Aspect, page, pages, titleLaunchBase)
		var encoded bytes.Buffer
		encoder, err := NewMPEG2VideoEncoder(&encoded, profile)
		if err != nil {
			return nil, nil, err
		}
		frame := newDVDMenuFrame(profile, layout, text)
		for range dvdMenuPagePictures {
			if err := encoder.WriteFrame(frame); err != nil {
				return nil, nil, err
			}
		}
		if err := encoder.Close(); err != nil {
			return nil, nil, err
		}
		videoPath := filepath.Join(workDir, fmt.Sprintf("menu-%02d.m2v", page+1))
		if err := os.WriteFile(videoPath, encoded.Bytes(), 0o600); err != nil {
			return nil, nil, err
		}
		audioPath := filepath.Join(workDir, fmt.Sprintf("menu-%02d.lpcm", page+1))
		if err := os.WriteFile(audioPath, silence, 0o600); err != nil {
			return nil, nil, err
		}
		menuPages = append(menuPages, EncodedTitle{
			VideoPath: videoPath, AudioPath: audioPath,
			VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
			Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
			FrameRateNumerator:   menuFrameRateNumerator(profile.Standard),
			FrameRateDenominator: menuFrameRateDenominator(profile.Standard),
			Duration:             dvdMenuPageDuration(profile.Standard),
		})
		buttonPages = append(buttonPages, layout.buttons())
	}
	return menuPages, buttonPages, nil
}

// muxDVDMenuVOB writes the menu pages to dst one after the other, each as a
// VOB of its own: page n is VOB n of the menu VOB set and its single cell is
// cell 1 of that VOB. A page is played by itself, as a still cell reached
// through its own PGC, so each VOB carries all of the page's audio and its
// own clock, just as the menu of a one-page disc does. In one continuous VOB
// the audio of a page's last fraction of a second would be multiplexed into
// the next page, behind its NAV pack. recordVOBU receives the VOBU of every
// page in order.
func muxDVDMenuVOB(ctx context.Context, pages []EncodedTitle, buttonPages [][]DVDNAVButton, workDir string, dst io.Writer, recordVOBU func(dvdVOBURecord) error) error {
	if len(pages) != len(buttonPages) {
		return errors.New("video2dvd: menu pages and button pages differ in number")
	}
	if len(pages) > math.MaxUint16 {
		return errors.New("video2dvd: menu has more pages than a VOB set has VOB IDs")
	}
	var nextSector uint32
	for page, title := range pages {
		records := 0
		err := muxDVDVOB(ctx, title, workDir, dst, func(record dvdVOBURecord) error {
			records++
			nextSector = record.EndSector + 1
			return recordVOBU(record)
		}, buttonPages[page:page+1], dvdVOBPlacement{firstSector: nextSector, vobID: uint16(page + 1)})
		if err != nil {
			return fmt.Errorf("menu page %d: %w", page+1, err)
		}
		if records != 1 {
			return fmt.Errorf("menu page %d is %d VOBUs, want one", page+1, records)
		}
	}
	return nil
}

// dvdLPCMStereoSampleBytes is the size of one 16-bit stereo LPCM sample.
const dvdLPCMStereoSampleBytes = 4

// dvdMenuPageDuration returns how long the pictures of one menu page last.
func dvdMenuPageDuration(standard VideoStandard) time.Duration {
	numerator, denominator := menuFrameRateNumerator(standard), menuFrameRateDenominator(standard)
	return time.Duration(dvdMenuPagePictures*denominator) * time.Second / time.Duration(numerator)
}

// dvdMenuPageAudioSamples returns the number of 48 kHz samples that last as
// long as the pictures of one menu page: 24024 for NTSC and 28800 for PAL.
func dvdMenuPageAudioSamples(standard VideoStandard) int {
	numerator, denominator := menuFrameRateNumerator(standard), menuFrameRateDenominator(standard)
	return dvdMenuPagePictures * denominator * 48_000 / numerator
}

// dvdMenuTitleName returns the name shown for a title: its Name, or else the
// base name of its video stream file without the extension.
func dvdMenuTitleName(title EncodedTitle) string {
	if title.Name != "" {
		return title.Name
	}
	name := filepath.Base(title.VideoPath)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// layoutDVDMenuPage places the controls and the titles names[i], which are
// titles start+i of the disc, on one page. 4:3 pages with more than one
// column of titles use two half-width columns. Labels are measured with text,
// which must also draw the page, and cut to the width of their buttons.
func layoutDVDMenuPage(text *dvdMenuText, names []string, start int, aspect AspectRatio, page, pageCount, titleLaunchBase int) dvdMenuPage {
	var layout dvdMenuPage
	// A control is as wide as its label, which may extend to nextX.
	control := func(label string, textX, nextX int, command [8]byte) {
		label = text.truncate(label, nextX-dvdMenuControlGap-2*dvdMenuButtonMarginX-textX)
		layout.controls = append(layout.controls, dvdMenuEntry{
			label: label, textX: textX, textY: dvdMenuControlY,
			left:    textX - dvdMenuButtonMarginX,
			right:   textX + text.width(label) + dvdMenuButtonMarginX - 1,
			command: command,
		})
	}
	if page == 0 {
		control("PLAY ALL", dvdMenuPlayAllX, dvdMenuPreviousX, linkPGCNCommand(pageCount))
	}
	if pageCount > 1 {
		control("PREV", dvdMenuPreviousX, dvdMenuNextX, linkPGCNCommand((page+pageCount-1)%pageCount))
		control("NEXT", dvdMenuNextX, dvdMenuWideColumnEnd+1+dvdMenuButtonMarginX+dvdMenuControlGap, linkPGCNCommand((page+1)%pageCount))
	}
	twoColumns := aspect != AspectRatio16x9 && len(names) > dvdMenuRowsPerColumn
	for i, name := range names {
		column, row := i/dvdMenuRowsPerColumn, i%dvdMenuRowsPerColumn
		textX, right := dvdMenuLeftColumnX, dvdMenuWideColumnEnd
		if twoColumns {
			right = dvdMenuLeftColumnEnd
			if column == 1 {
				textX, right = dvdMenuRightColumnX, dvdMenuWideColumnEnd
			}
		}
		label := padMenuNumber(start + i + 1)
		if name != "" {
			label += " " + text.normalize(name)
		}
		layout.titles = append(layout.titles, dvdMenuEntry{
			label: text.truncate(label, right+1-dvdMenuButtonMarginX-textX),
			textX: textX, textY: dvdMenuFirstRowY + row*dvdMenuRowStep,
			left: textX - dvdMenuButtonMarginX, right: right,
			command: linkPGCNCommand(titleLaunchBase + start + i),
		})
	}
	return layout
}

// buttons returns the page's highlight buttons: the controls followed by the
// titles. Every button is reachable from button 1 with the arrow keys: the
// control row links left and right, and each title column wraps through the
// control row.
func (p dvdMenuPage) buttons() []DVDNAVButton {
	controlCount, titleCount := len(p.controls), len(p.titles)
	columns := (titleCount + dvdMenuRowsPerColumn - 1) / dvdMenuRowsPerColumn
	buttons := make([]DVDNAVButton, 0, controlCount+titleCount)
	for _, entry := range slices.Concat(p.controls, p.titles) {
		buttons = append(buttons, DVDNAVButton{
			XStart: entry.left, XEnd: entry.right,
			YStart: entry.textY - dvdMenuButtonTop, YEnd: entry.textY + dvdMenuButtonBottom,
			Command: entry.command,
		})
	}
	// Button numbers are one-based.
	titleButton := func(column, row int) uint8 {
		return uint8(controlCount + column*dvdMenuRowsPerColumn + row + 1)
	}
	columnRows := func(column int) int {
		return min(dvdMenuRowsPerColumn, titleCount-column*dvdMenuRowsPerColumn)
	}
	for i := range controlCount {
		button := &buttons[i]
		self := uint8(i + 1)
		button.Up, button.Down, button.Left, button.Right = self, self, self, self
		if i > 0 {
			button.Left = self - 1
		}
		if i < controlCount-1 {
			button.Right = self + 1
		}
		if columns > 0 {
			column := i * columns / controlCount
			button.Up = titleButton(column, columnRows(column)-1)
			button.Down = titleButton(column, 0)
		}
	}
	for i := range titleCount {
		column, row := i/dvdMenuRowsPerColumn, i%dvdMenuRowsPerColumn
		button := &buttons[controlCount+i]
		self := titleButton(column, row)
		// The leftmost column wraps through the first control and the
		// rightmost through the last one.
		controlButton := self
		if controlCount > 0 {
			controlButton = 1
			if column > 0 && column == columns-1 {
				controlButton = uint8(controlCount)
			}
		}
		button.Up, button.Down, button.Left, button.Right = controlButton, controlButton, self, self
		if row > 0 {
			button.Up = self - 1
		}
		if row < columnRows(column)-1 {
			button.Down = self + 1
		}
		if column > 0 {
			button.Left = titleButton(column-1, min(row, columnRows(column-1)-1))
		}
		if column < columns-1 {
			button.Right = titleButton(column+1, min(row, columnRows(column+1)-1))
		}
	}
	return buttons
}

func makeDVDMenuButtons(start, end int, profile Profile, page, pageCount, titleLaunchBase int) []DVDNAVButton {
	return layoutDVDMenuPage(newDVDMenuText(profile), make([]string, end-start), start, profile.Aspect, page, pageCount, titleLaunchBase).buttons()
}

func dvdJumpTitleCommand(titleNumber int) [8]byte {
	return [8]byte{0x30, 0x02, 0, 0, 0, byte(titleNumber)}
}

func dvdCallMenuCommand() [8]byte {
	return [8]byte{0x30, 0x08, 0, 0, 0, 0x42}
}

func dvdConditionalCallVMGPGCCommand(pgcNumber int) [8]byte {
	return [8]byte{0x30, 0x38, byte(pgcNumber >> 8), byte(pgcNumber), 0, 0xc0, 0, 1}
}

func dvdSetGPRMCommand(register, value int) [8]byte {
	return [8]byte{0x71, 0, 0, byte(register), byte(value >> 8), byte(value)}
}

// dvdSetPlayAllCommand marks playback initiated by the PLAY ALL menu item.
func dvdSetPlayAllCommand() [8]byte { return dvdSetGPRMCommand(0, 1) }

func dvdJumpMenuCommand() [8]byte {
	return [8]byte{0x30, 0x06, 0, 0, 0, 0x42}
}

func linkPGCNCommand(pgcn int) [8]byte {
	return [8]byte{0x20, 0x04, 0, 0, 0, 0, 0, byte(pgcn + 1)}
}

func menuFrameRateNumerator(standard VideoStandard) int {
	if standard == VideoStandardPAL {
		return 25
	}
	return 30000
}

func menuFrameRateDenominator(standard VideoStandard) int {
	if standard == VideoStandardPAL {
		return 1
	}
	return 1001
}

// newDVDMenuFrame draws a menu page that was laid out with text.
func newDVDMenuFrame(profile Profile, page dvdMenuPage, text *dvdMenuText) DecodedVideoFrame {
	frame := DecodedVideoFrame{Width: profile.FrameWidth, Height: profile.FrameHeight, Duration: time.Second,
		Y: make([]byte, profile.FrameWidth*profile.FrameHeight), U: make([]byte, profile.FrameWidth*profile.FrameHeight/4), V: make([]byte, profile.FrameWidth*profile.FrameHeight/4)}
	background := profile.MenuBackgroundLuma
	if background == 0 {
		background = 24
	}
	for i := range frame.Y {
		frame.Y[i] = background
	}
	for i := range frame.U {
		frame.U[i], frame.V[i] = 128, 128
	}
	title := profile.MenuTitle
	if title == "" {
		title = "VIDEO2DVD"
	}
	textLuma := profile.MenuTextLuma
	if textLuma == 0 {
		textLuma = 220
	}
	// Text stays within the lines of a button, and within its rectangle
	// where it has one.
	lines := func(left, right, textY int) image.Rectangle {
		return image.Rect(left, textY-dvdMenuButtonTop, right+1, textY+dvdMenuButtonBottom+1)
	}
	heading := text.truncate(text.normalize(title), dvdMenuHeadingWidth)
	text.draw(&frame, dvdMenuHeadingX, dvdMenuHeadingY, heading, textLuma,
		lines(dvdMenuHeadingX-dvdMenuButtonMarginX, dvdMenuHeadingX+dvdMenuHeadingWidth+dvdMenuButtonMarginX-1, dvdMenuHeadingY))
	for _, entry := range slices.Concat(page.controls, page.titles) {
		text.draw(&frame, entry.textX, entry.textY, entry.label, textLuma, lines(entry.left, entry.right, entry.textY))
	}
	return frame
}

func padMenuNumber(number int) string {
	if number < 10 {
		return "0" + string(rune('0'+number))
	}
	return string(rune('0'+number/10)) + string(rune('0'+number%10))
}

// dvdMenuFont holds the 5x7 glyphs of ASCII, one byte per row with bit 4
// leftmost. It has capital letters only; dvdMenuText draws small letters
// with them.
var dvdMenuFont = map[rune][7]byte{
	'?': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x00, 0x04}, ' ': {0, 0, 0, 0, 0, 0, 0},
	'-': {0x00, 0x00, 0x00, 0x1f, 0x00, 0x00, 0x00}, '_': {0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1f},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x0c, 0x0c},
	'0': {0x0e, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0e}, '1': {0x04, 0x0c, 0x04, 0x04, 0x04, 0x04, 0x0e},
	'2': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1f}, '3': {0x1e, 0x01, 0x01, 0x0e, 0x01, 0x01, 0x1e},
	'4': {0x02, 0x06, 0x0a, 0x12, 0x1f, 0x02, 0x02}, '5': {0x1f, 0x10, 0x10, 0x1e, 0x01, 0x01, 0x1e},
	'6': {0x06, 0x08, 0x10, 0x1e, 0x11, 0x11, 0x0e}, '7': {0x1f, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0e, 0x11, 0x11, 0x0e, 0x11, 0x11, 0x0e}, '9': {0x0e, 0x11, 0x11, 0x0f, 0x01, 0x02, 0x0c},
	'A': {0x0e, 0x11, 0x11, 0x1f, 0x11, 0x11, 0x11}, 'B': {0x1e, 0x11, 0x11, 0x1e, 0x11, 0x11, 0x1e},
	'C': {0x0e, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0e}, 'D': {0x1e, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1e},
	'E': {0x1f, 0x10, 0x10, 0x1e, 0x10, 0x10, 0x1f}, 'F': {0x1f, 0x10, 0x10, 0x1e, 0x10, 0x10, 0x10},
	'G': {0x0e, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0f}, 'H': {0x11, 0x11, 0x11, 0x1f, 0x11, 0x11, 0x11},
	'I': {0x0e, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0e}, 'J': {0x07, 0x02, 0x02, 0x02, 0x02, 0x12, 0x0c},
	'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11}, 'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1f},
	'M': {0x11, 0x1b, 0x15, 0x15, 0x11, 0x11, 0x11}, 'N': {0x11, 0x19, 0x15, 0x13, 0x11, 0x11, 0x11},
	'O': {0x0e, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0e}, 'P': {0x1e, 0x11, 0x11, 0x1e, 0x10, 0x10, 0x10},
	'Q': {0x0e, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0d}, 'R': {0x1e, 0x11, 0x11, 0x1e, 0x14, 0x12, 0x11},
	'S': {0x0f, 0x10, 0x10, 0x0e, 0x01, 0x01, 0x1e}, 'T': {0x1f, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0e}, 'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0a, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0a}, 'X': {0x11, 0x11, 0x0a, 0x04, 0x0a, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x11, 0x0a, 0x04, 0x04, 0x04}, 'Z': {0x1f, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1f},
	'!': {0x04, 0x04, 0x04, 0x04, 0x04, 0x00, 0x04}, '"': {0x0a, 0x0a, 0x0a, 0x00, 0x00, 0x00, 0x00},
	'#': {0x0a, 0x0a, 0x1f, 0x0a, 0x1f, 0x0a, 0x0a}, '$': {0x04, 0x0f, 0x14, 0x0e, 0x05, 0x1e, 0x04},
	'%': {0x18, 0x19, 0x02, 0x04, 0x08, 0x13, 0x03}, '&': {0x0c, 0x12, 0x14, 0x08, 0x15, 0x12, 0x0d},
	'\'': {0x04, 0x04, 0x08, 0x00, 0x00, 0x00, 0x00}, '(': {0x02, 0x04, 0x08, 0x08, 0x08, 0x04, 0x02},
	')': {0x08, 0x04, 0x02, 0x02, 0x02, 0x04, 0x08}, '*': {0x00, 0x04, 0x15, 0x0e, 0x15, 0x04, 0x00},
	'+': {0x00, 0x04, 0x04, 0x1f, 0x04, 0x04, 0x00}, ',': {0x00, 0x00, 0x00, 0x00, 0x0c, 0x04, 0x08},
	'/': {0x00, 0x01, 0x02, 0x04, 0x08, 0x10, 0x00}, ':': {0x00, 0x0c, 0x0c, 0x00, 0x0c, 0x0c, 0x00},
	';': {0x00, 0x0c, 0x0c, 0x00, 0x0c, 0x04, 0x08}, '<': {0x02, 0x04, 0x08, 0x10, 0x08, 0x04, 0x02},
	'=': {0x00, 0x00, 0x1f, 0x00, 0x1f, 0x00, 0x00}, '>': {0x08, 0x04, 0x02, 0x01, 0x02, 0x04, 0x08},
	'@': {0x0e, 0x11, 0x01, 0x0d, 0x15, 0x15, 0x0e}, '[': {0x0e, 0x08, 0x08, 0x08, 0x08, 0x08, 0x0e},
	'\\': {0x00, 0x10, 0x08, 0x04, 0x02, 0x01, 0x00}, ']': {0x0e, 0x02, 0x02, 0x02, 0x02, 0x02, 0x0e},
	'^': {0x04, 0x0a, 0x11, 0x00, 0x00, 0x00, 0x00}, '`': {0x08, 0x04, 0x02, 0x00, 0x00, 0x00, 0x00},
	'{': {0x02, 0x04, 0x04, 0x08, 0x04, 0x04, 0x02}, '|': {0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'}': {0x08, 0x04, 0x04, 0x02, 0x04, 0x04, 0x08}, '~': {0x00, 0x00, 0x08, 0x15, 0x02, 0x00, 0x00},
}

// The menu highlight is a DVD subpicture with "pattern" pixels over every
// button and zero contrast, so it is invisible by itself. A player applies
// the highlight colours of the NAV pack (dvdNAVSelectionColor and
// dvdNAVActionColor) to the subpicture pixels inside the selected button's
// rectangle, which tints that button.
var (
	// dvdMenuSubpicturePalette is the menu PGC colour table. Each entry is
	// a reserved byte followed by Y, Cr and Cb.
	dvdMenuSubpicturePalette = [16][4]byte{
		0: {0, 16, 128, 128},
		1: {0, 110, 110, 200}, // selected button: blue.
		2: {0, 170, 160, 50},  // activated button: amber.
	}
)

const (
	dvdSubpictureBackground = 0
	dvdSubpicturePattern    = 1
)

// dvdSubpictureLine accumulates the 4-bit units of one run-length coded
// subpicture line.
type dvdSubpictureLine struct {
	data    []byte
	nibbles int
}

func (l *dvdSubpictureLine) put(value uint32, nibbles int) {
	for shift := (nibbles - 1) * 4; shift >= 0; shift -= 4 {
		nibble := byte(value>>shift) & 0x0f
		if l.nibbles%2 == 0 {
			l.data = append(l.data, nibble<<4)
		} else {
			l.data[len(l.data)-1] |= nibble
		}
		l.nibbles++
	}
}

// run appends count pixels of one colour. A code is the count followed by
// the two-bit colour, in the fewest of one to four nibbles that hold it.
func (l *dvdSubpictureLine) run(count int, color uint32) {
	for count > 0 {
		n := min(count, 255)
		code := uint32(n)<<2 | color
		switch {
		case n < 4:
			l.put(code, 1)
		case n < 16:
			l.put(code, 2)
		case n < 64:
			l.put(code, 3)
		default:
			l.put(code, 4)
		}
		count -= n
	}
}

// fill appends one colour up to the end of the line: a four-nibble code with
// a zero count.
func (l *dvdSubpictureLine) fill(color uint32) { l.put(color, 4) }

// encodeDVDMenuSubpicture returns the subpicture unit for a menu page of the
// given frame size. On every line that crosses a button, the pixels from the
// leftmost such button to the end of the line are pattern pixels; pixels
// outside the selected button's rectangle never become visible, so runs do
// not have to stop at button edges.
func encodeDVDMenuSubpicture(buttons []DVDNAVButton, width, height int) ([]byte, error) {
	if width <= 0 || width > 720 || height <= 0 || height > 576 || height%2 != 0 {
		return nil, errors.New("video2dvd: menu subpicture size is outside the DVD raster")
	}
	const headerSize = 4
	unit := make([]byte, headerSize, dvdMenuSubpictureMaxUnit)
	var fieldOffsets [2]int
	// Lines are stored per field: all even lines, then all odd lines. Each
	// line ends on a byte boundary.
	for field := range 2 {
		fieldOffsets[field] = len(unit)
		for y := field; y < height; y += 2 {
			left := width
			for _, button := range buttons {
				if button.YStart <= y && y <= button.YEnd && button.XStart < left {
					left = max(button.XStart, 0)
				}
			}
			var line dvdSubpictureLine
			if left < width {
				line.run(left, dvdSubpictureBackground)
				line.fill(dvdSubpicturePattern)
			} else {
				line.fill(dvdSubpictureBackground)
			}
			unit = append(unit, line.data...)
		}
	}
	controlOffset := len(unit)
	right, bottom := width-1, height-1
	unit = append(unit,
		0, 0, // this sequence runs at the unit's presentation time,
		byte(controlOffset>>8), byte(controlOffset), // and is the last one: it points at itself.
		0x00,             // FSTA_DSP: show even when the user turned subpictures off.
		0x03, 0x00, 0x10, // SET_COLOR: pattern pixels use palette entry 1.
		0x04, 0x00, 0x00, // SET_CONTR: everything transparent outside a highlight.
		0x05, 0, byte(right>>8), byte(right), 0, byte(bottom>>8), byte(bottom), // SET_DAREA from (0,0).
		0x06, byte(fieldOffsets[0]>>8), byte(fieldOffsets[0]), byte(fieldOffsets[1]>>8), byte(fieldOffsets[1]), // SET_DSPXA.
		0xff, // end of the command sequence.
	)
	if len(unit)%2 != 0 {
		unit = append(unit, 0xff)
	}
	if len(unit) > dvdMenuSubpictureMaxUnit {
		return nil, errors.New("video2dvd: menu subpicture does not fit one pack")
	}
	binary.BigEndian.PutUint16(unit[0:2], uint16(len(unit)))
	binary.BigEndian.PutUint16(unit[2:4], uint16(controlOffset))
	return unit, nil
}

// dvdMenuHighlight is what the VOBU of one menu page carries besides its
// picture: the button groups for its NAV pack and one subpicture unit per
// group. Group i belongs to subpicture stream i.
type dvdMenuHighlight struct {
	groups []DVDNAVButtonGroup
	// subpictures holds one private_stream_1 payload per stream: the
	// substream ID followed by a complete subpicture unit.
	subpictures [][]byte
}

// dvdMenuSubpictureStreamControl returns the PGC subpicture stream control
// entry for the menu highlight: the stream is available, and its remaining
// bytes name the substreams decoded for 16:9 video on a widescreen display,
// letterboxed and in pan-and-scan. A 16:9 menu has no highlight for
// pan-and-scan, which its video attributes therefore rule out.
func dvdMenuSubpictureStreamControl(aspect AspectRatio) [4]byte {
	if aspect == AspectRatio16x9 {
		return [4]byte{0x80, 0, 1, 0}
	}
	return [4]byte{0x80, 0, 0, 0}
}

// letterboxDVDMenuButtons returns the buttons as they appear when a 4:3
// display shows the 16:9 menu letterboxed: the picture is squeezed into the
// middle three quarters of the frame height. Rectangles keep starting on an
// even line and ending on an odd one, so each covers whole frame lines of
// both fields.
func letterboxDVDMenuButtons(buttons []DVDNAVButton, height int) []DVDNAVButton {
	letterboxed := slices.Clone(buttons)
	for i := range letterboxed {
		button := &letterboxed[i]
		button.YStart = (height/8 + button.YStart*3/4) &^ 1
		button.YEnd = min((height/8+button.YEnd*3/4)|1, height-1)
	}
	return letterboxed
}

// buildDVDMenuHighlights prepares the highlight of every menu page. A 4:3
// menu has one button group. A 16:9 menu has two, because a player does not
// scale subpictures with the video: the first group and subpicture stream
// are for a widescreen display, the second ones for the letterboxed picture
// on a 4:3 display.
func buildDVDMenuHighlights(buttonPages [][]DVDNAVButton, aspect AspectRatio, width, height int) ([]dvdMenuHighlight, error) {
	highlights := make([]dvdMenuHighlight, len(buttonPages))
	for page, buttons := range buttonPages {
		groups := []DVDNAVButtonGroup{{DisplayType: dvdButtonGroupNormal, Buttons: buttons}}
		if aspect == AspectRatio16x9 {
			groups = []DVDNAVButtonGroup{
				{DisplayType: dvdButtonGroupWide, Buttons: buttons},
				{DisplayType: dvdButtonGroupLetterbox, Buttons: letterboxDVDMenuButtons(buttons, height)},
			}
		}
		if len(buttons) > dvdNAVMaxButtons/len(groups) {
			return nil, fmt.Errorf("video2dvd: menu page %d has more than %d buttons", page+1, dvdNAVMaxButtons/len(groups))
		}
		highlights[page].groups = groups
		for stream, group := range groups {
			unit, err := encodeDVDMenuSubpicture(group.Buttons, width, height)
			if err != nil {
				return nil, err
			}
			payload := append([]byte{dvdMenuSubpictureSubstream + byte(stream)}, unit...)
			highlights[page].subpictures = append(highlights[page].subpictures, payload)
		}
	}
	return highlights, nil
}
