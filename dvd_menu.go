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
	if len(titles) == 0 {
		return EncodedTitle{}, nil, errors.New("video2dvd: automatically generated menu requires at least one title")
	}
	pages := (len(titles) + maxButtons - 1) / maxButtons
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
			encodeErr = encoder.WriteFrame(newDVDMenuFrame(profile, titles[start:end], start))
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
		buttonPages = append(buttonPages, makeDVDMenuButtons(start, end, profile))
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

func makeDVDMenuButtons(start, end int, profile Profile) []DVDNAVButton {
	buttons := make([]DVDNAVButton, end-start)
	for i := range buttons {
		column, row := i/18, i%18
		up, down := i-1, i+1
		if row == 0 {
			up = i + 17
		}
		if row == 17 || down >= len(buttons) || down/18 != column {
			down = i - row
		}
		leftIndex, rightIndex := i, i
		if column == 0 && i+18 < len(buttons) {
			rightIndex = i + 18
		}
		if column == 1 {
			leftIndex = i - 18
		}
		left, right := 48, 672
		if column == 1 {
			left, right = 368, 704
		}
		if profile.Aspect == AspectRatio16x9 {
			left, right = 48, 672
		}
		top := 92 + row*20
		buttons[i] = DVDNAVButton{
			XStart: left, YStart: top, XEnd: right, YEnd: top + 16,
			Up: uint8(up + 1), Down: uint8(down + 1),
			Left: uint8(leftIndex + 1), Right: uint8(rightIndex + 1),
			Command: dvdJumpTitleCommand(start + i + 1),
		}
	}
	return buttons
}

func dvdJumpTitleCommand(titleNumber int) [8]byte {
	return [8]byte{0x30, 0x02, 0, 0, 0, byte(titleNumber)}
}

func dvdJumpMenuCommand() [8]byte {
	return [8]byte{0x30, 0x06, 0, 0, 0, 0x42}
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

func newDVDMenuFrame(profile Profile, titles []EncodedTitle, start int) DecodedVideoFrame {
	frame := DecodedVideoFrame{Width: profile.FrameWidth, Height: profile.FrameHeight, Duration: time.Second,
		Y: make([]byte, profile.FrameWidth*profile.FrameHeight), U: make([]byte, profile.FrameWidth*profile.FrameHeight/4), V: make([]byte, profile.FrameWidth*profile.FrameHeight/4)}
	for i := range frame.Y {
		frame.Y[i] = 24
	}
	for i := range frame.U {
		frame.U[i], frame.V[i] = 128, 128
	}
	drawDVDMenuText(&frame, 48, 48, "VIDEO2DVD", 220)
	for i, title := range titles {
		name := strings.ToUpper(filepath.Base(title.VideoPath))
		name = strings.TrimSuffix(name, strings.ToUpper(filepath.Ext(name)))
		column, row := i/18, i%18
		x := 56
		if column == 1 {
			x = 376
		}
		drawDVDMenuText(&frame, x, 94+row*20, padMenuNumber(start+i+1)+" "+name, 210)
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
		if ok {
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
		}
		x += 12
	}
}
