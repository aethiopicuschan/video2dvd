package video2dvd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const dvdMenuMaxButtons = 36

func createDVDMenuTitle(ctx context.Context, titles []EncodedTitle, profile Profile, workDir string) (EncodedTitle, [][]DVDNAVButton, error) {
	maxButtons := dvdMenuMaxButtons
	if profile.Aspect == AspectRatio16x9 {
		maxButtons = 18
	}
	maxButtons-- // reserve PLAY ALL on the first page
	// Two buttons are reserved for previous/next page navigation when the
	// menu spans more than one page.
	if len(titles) > maxButtons {
		maxButtons -= 2
	}
	if len(titles) == 0 {
		return EncodedTitle{}, nil, errors.New("video2dvd: automatically generated menu requires at least one title")
	}
	pages := (len(titles) + maxButtons - 1) / maxButtons
	// PGCs are ordered as menu pages, PLAY ALL, then one title launcher per
	// title and one PLAY ALL continuation per title.
	titleLaunchBase := pages + 1
	videoPath := filepath.Join(workDir, "menu.m2v")
	videoFile, err := os.Create(videoPath)
	if err != nil {
		return EncodedTitle{}, nil, err
	}
	buttonPages := make([][]DVDNAVButton, 0, pages)
	for page := range pages {
		start := page * maxButtons
		end := min(len(titles), start+maxButtons)
		var encoded bytes.Buffer
		encoder, encodeErr := NewMPEG2VideoEncoder(&encoded, profile)
		if encodeErr == nil {
			encodeErr = encoder.WriteFrame(newDVDMenuFrame(profile, titles[start:end], start, pages > 1))
		}
		if encodeErr == nil {
			encodeErr = encoder.Close()
		}
		if encodeErr != nil {
			_ = videoFile.Close()
			return EncodedTitle{}, nil, encodeErr
		}
		if _, err := videoFile.Write(encoded.Bytes()); err != nil {
			_ = videoFile.Close()
			return EncodedTitle{}, nil, err
		}
		buttonPages = append(buttonPages, makeDVDMenuButtons(start, end, profile, page, pages, titleLaunchBase))
	}
	if closeErr := videoFile.Close(); closeErr != nil {
		return EncodedTitle{}, nil, closeErr
	}
	audioPath := filepath.Join(workDir, "menu.lpcm")
	if err := os.WriteFile(audioPath, []byte{0, 0, 0, 0}, 0o600); err != nil {
		return EncodedTitle{}, nil, err
	}
	return EncodedTitle{
		VideoPath: videoPath, AudioPath: audioPath,
		VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
		Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
		FrameRateNumerator:   menuFrameRateNumerator(profile.Standard),
		FrameRateDenominator: menuFrameRateDenominator(profile.Standard),
		Duration:             time.Duration(pages) * time.Second,
	}, buttonPages, ctx.Err()
}

func makeDVDMenuButtons(start, end int, profile Profile, page, pageCount, titleLaunchBase int) []DVDNAVButton {
	controls := 0
	if pageCount > 1 {
		controls = 2
	}
	playAll := 0
	if page == 0 {
		playAll = 1
	}
	buttons := make([]DVDNAVButton, end-start+controls+playAll)
	if playAll != 0 {
		buttons[0] = DVDNAVButton{XStart: 48, YStart: 52, XEnd: 300, YEnd: 76, Command: linkPGCNCommand(pageCount)}
	}
	if controls != 0 {
		base := playAll
		buttons[base] = DVDNAVButton{XStart: 48, YStart: 52, XEnd: 300, YEnd: 76, Command: linkPGCNCommand(page)}
		buttons[base+1] = DVDNAVButton{XStart: 368, YStart: 52, XEnd: 704, YEnd: 76, Command: linkPGCNCommand((page + 1) % pageCount)}
	}
	offset := controls + playAll
	titleCount := end - start
	for i := range titleCount {
		index := i + offset
		column, row := i/18, i%18
		left, right := 48, 672
		if column == 1 {
			left, right = 368, 704
		}
		if profile.Aspect == AspectRatio16x9 {
			left, right = 48, 672
		}
		top := 94 + row*20
		buttons[index] = DVDNAVButton{
			XStart: left, YStart: top, XEnd: right, YEnd: top + 16,
			Command: linkPGCNCommand(titleLaunchBase + start + i),
		}
	}
	if playAll != 0 {
		target := offset + 1
		buttons[0].Up, buttons[0].Down = 1, uint8(target)
		buttons[0].Left, buttons[0].Right = 1, 1
	}
	if controls != 0 {
		prev, next := playAll, playAll+1
		target := offset + 1
		buttons[prev].Up, buttons[prev].Down = uint8(prev+1), uint8(target)
		buttons[prev].Left, buttons[prev].Right = uint8(prev+1), uint8(next+1)
		buttons[next].Up, buttons[next].Down = uint8(next+1), uint8(target)
		buttons[next].Left, buttons[next].Right = uint8(prev+1), uint8(next+1)
	}
	for i := range titleCount {
		column, row := i/18, i%18
		columnStart := column * 18
		columnCount := min(18, titleCount-columnStart)
		index := offset + i
		upRow, downRow := (row+columnCount-1)%columnCount, (row+1)%columnCount
		buttons[index].Up = uint8(offset + columnStart + upRow + 1)
		buttons[index].Down = uint8(offset + columnStart + downRow + 1)
		buttons[index].Left = uint8(index + 1)
		buttons[index].Right = uint8(index + 1)
		if column > 0 {
			leftCount := min(18, titleCount-(column-1)*18)
			buttons[index].Left = uint8(offset + (column-1)*18 + min(row, leftCount-1) + 1)
		}
		if rightStart := (column + 1) * 18; rightStart < titleCount {
			rightCount := min(18, titleCount-rightStart)
			buttons[index].Right = uint8(offset + rightStart + min(row, rightCount-1) + 1)
		}
	}
	return buttons
}

func dvdJumpTitleCommand(titleNumber int) [8]byte {
	return [8]byte{0x30, 0x02, 0, 0, 0, byte(titleNumber)}
}

func dvdJumpVTSTitleCommand(titleNumber int) [8]byte {
	return [8]byte{0x30, 0x03, 0, 0, 0, byte(titleNumber)}
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

func newDVDMenuFrame(profile Profile, titles []EncodedTitle, start int, pageControls bool) DecodedVideoFrame {
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
	drawDVDMenuText(&frame, 48, 24, strings.ToUpper(title), textLuma)
	if pageControls {
		drawDVDMenuText(&frame, 56, 54, "PREV", textLuma)
		drawDVDMenuText(&frame, 376, 54, "NEXT", textLuma)
	}
	if start == 0 {
		drawDVDMenuText(&frame, 56, 54, "PLAY ALL", textLuma)
	}
	for i, title := range titles {
		name := strings.ToUpper(filepath.Base(title.VideoPath))
		name = strings.TrimSuffix(name, strings.ToUpper(filepath.Ext(name)))
		column, row := i/18, i%18
		x := 56
		if column == 1 {
			x = 376
		}
		drawDVDMenuText(&frame, x, 94+row*20, padMenuNumber(start+i+1)+" "+name, textLuma)
	}
	return frame
}

func padMenuNumber(number int) string {
	if number < 10 {
		return "0" + string(rune('0'+number))
	}
	return string(rune('0'+number/10)) + string(rune('0'+number%10))
}

var dvdMenuFont = map[byte][7]byte{
	'?': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x00, 0x04}, ' ': {0, 0, 0, 0, 0, 0, 0},
	'0': {0x0e, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0e}, '1': {0x04, 0x0c, 0x04, 0x04, 0x04, 0x04, 0x0e},
	'2': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1f}, '3': {0x1e, 0x01, 0x01, 0x0e, 0x01, 0x01, 0x1e},
	'4': {0x02, 0x06, 0x0a, 0x12, 0x1f, 0x02, 0x02}, '5': {0x1f, 0x10, 0x10, 0x1e, 0x01, 0x01, 0x1e},
	'6': {0x06, 0x08, 0x10, 0x1e, 0x11, 0x11, 0x0e}, '7': {0x1f, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0e, 0x11, 0x11, 0x0e, 0x11, 0x11, 0x0e}, '9': {0x0e, 0x11, 0x11, 0x0f, 0x01, 0x02, 0x0c},
	'A': {0x0e, 0x11, 0x11, 0x1f, 0x11, 0x11, 0x11}, 'D': {0x1e, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1e},
	'I': {0x0e, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0e},
	'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0a, 0x04}, 'E': {0x1f, 0x10, 0x10, 0x1e, 0x10, 0x10, 0x1f},
	'O': {0x0e, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0e}, 'T': {0x1f, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'B': {0x1e, 0x11, 0x11, 0x1e, 0x11, 0x11, 0x1e}, 'F': {0x1f, 0x10, 0x10, 0x1e, 0x10, 0x10, 0x10},
	'M': {0x11, 0x1b, 0x15, 0x15, 0x11, 0x11, 0x11}, 'P': {0x1e, 0x11, 0x11, 0x1e, 0x10, 0x10, 0x10},
}

func drawDVDMenuText(frame *DecodedVideoFrame, x, y int, text string, value byte) {
	for _, char := range []byte(text) {
		glyph, ok := dvdMenuFont[char]
		if !ok && char != ' ' {
			glyph = dvdMenuFont['?']
		}
		for row, bits := range glyph {
			for column := range 5 {
				if bits&(1<<(4-column)) != 0 && x+column*2+1 < frame.Width && y+row*2+1 < frame.Height {
					for dy := range 2 {
						for dx := range 2 {
							frame.Y[(y+row*2+dy)*frame.Width+x+column*2+dx] = value
						}
					}
				}
			}
		}
		x += 12
	}
}
