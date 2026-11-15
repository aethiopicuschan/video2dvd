package mp4todvd

import (
	"errors"
	"fmt"
)

// FitMode controls how FitVideoFrame places source pixels in the DVD raster.
type FitMode uint8

const (
	// FitLetterbox preserves the entire picture and fills any unused area with
	// black. This is the zero value and avoids cropping source content.
	FitLetterbox FitMode = iota
	// FitCrop fills the DVD raster and crops any excess source image.
	FitCrop
)

// FitVideoFrame converts an owned planar YUV 4:2:0 frame to the raster and
// display aspect ratio described by profile. sourceAspectNumerator and
// sourceAspectDenominator describe the source's displayed aspect ratio, such
// as 16 and 9. The output uses bilinear scaling and even chroma-aligned edges.
//
// For FitLetterbox, unused pixels are set to studio-range black. For FitCrop,
// the source is center-cropped. The input frame is not modified.
func FitVideoFrame(frame DecodedVideoFrame, profile Profile, sourceAspectNumerator, sourceAspectDenominator int, mode FitMode) (DecodedVideoFrame, error) {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Width%2 != 0 || frame.Height%2 != 0 || frame.Width > int(^uint(0)>>1)/frame.Height {
		return DecodedVideoFrame{}, errors.New("mp4todvd: source frame dimensions must be positive even values")
	}
	if len(frame.Y) < frame.Width*frame.Height || len(frame.U) < frame.Width*frame.Height/4 || len(frame.V) < frame.Width*frame.Height/4 {
		return DecodedVideoFrame{}, errors.New("mp4todvd: source frame has incomplete YUV 4:2:0 planes")
	}
	if sourceAspectNumerator <= 0 || sourceAspectDenominator <= 0 {
		return DecodedVideoFrame{}, errors.New("mp4todvd: source display aspect ratio must be positive")
	}
	wantWidth, wantHeight := 0, 0
	switch profile.Standard {
	case VideoStandardNTSC:
		wantWidth, wantHeight = 720, 480
	case VideoStandardPAL:
		wantWidth, wantHeight = 720, 576
	default:
		return DecodedVideoFrame{}, ErrInvalidProfile
	}
	if profile.FrameWidth != wantWidth || profile.FrameHeight != wantHeight {
		return DecodedVideoFrame{}, ErrInvalidProfile
	}
	if profile.Aspect != AspectRatio4x3 && profile.Aspect != AspectRatio16x9 {
		return DecodedVideoFrame{}, ErrInvalidProfile
	}
	if mode != FitLetterbox && mode != FitCrop {
		return DecodedVideoFrame{}, fmt.Errorf("mp4todvd: unknown fit mode %d", mode)
	}

	// DVD pixels are not square. Derive the horizontal-to-vertical pixel ratio
	// from the requested display aspect and the selected raster dimensions.
	displayW, displayH := 4.0, 3.0
	if profile.Aspect == AspectRatio16x9 {
		displayW, displayH = 16, 9
	}
	pixelAspect := (displayW / displayH) / (float64(profile.FrameWidth) / float64(profile.FrameHeight))
	sourcePixelAspect := (float64(sourceAspectNumerator) / float64(sourceAspectDenominator)) / pixelAspect
	canvasAspect := float64(profile.FrameWidth) / float64(profile.FrameHeight)

	dstX, dstY, dstW, dstH := 0, 0, profile.FrameWidth, profile.FrameHeight
	srcX, srcY, srcW, srcH := 0, 0, frame.Width, frame.Height
	sourceCanvasAspect := sourcePixelAspect
	if mode == FitLetterbox {
		if sourceCanvasAspect > canvasAspect {
			dstH = evenDimension(float64(profile.FrameWidth) / sourceCanvasAspect)
			dstY = (profile.FrameHeight - dstH) / 2 &^ 1
		} else {
			dstW = evenDimension(float64(profile.FrameHeight) * sourceCanvasAspect)
			dstX = (profile.FrameWidth - dstW) / 2 &^ 1
		}
	} else {
		targetDisplayAspect := displayW / displayH
		actualDisplayAspect := float64(sourceAspectNumerator) / float64(sourceAspectDenominator)
		if actualDisplayAspect > targetDisplayAspect {
			srcW = evenDimension(float64(frame.Width) * targetDisplayAspect / actualDisplayAspect)
			srcX = (frame.Width - srcW) / 2 &^ 1
		} else if actualDisplayAspect < targetDisplayAspect {
			srcH = evenDimension(float64(frame.Height) * actualDisplayAspect / targetDisplayAspect)
			srcY = (frame.Height - srcH) / 2 &^ 1
		}
	}
	if dstW < 2 || dstH < 2 || srcW < 2 || srcH < 2 {
		return DecodedVideoFrame{}, errors.New("mp4todvd: aspect ratio leaves less than one chroma sample")
	}

	result := DecodedVideoFrame{
		PTS:      frame.PTS,
		Duration: frame.Duration,
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Y:        make([]byte, profile.FrameWidth*profile.FrameHeight),
		U:        make([]byte, profile.FrameWidth*profile.FrameHeight/4),
		V:        make([]byte, profile.FrameWidth*profile.FrameHeight/4),
	}
	for i := range result.Y {
		result.Y[i] = 16
	}
	for i := range result.U {
		result.U[i], result.V[i] = 128, 128
	}
	scalePlaneBilinear(frame.Y, frame.Width, frame.Height, result.Y, result.Width, result.Height,
		srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH)
	scalePlaneBilinear(frame.U, frame.Width/2, frame.Height/2, result.U, result.Width/2, result.Height/2,
		srcX/2, srcY/2, srcW/2, srcH/2, dstX/2, dstY/2, dstW/2, dstH/2)
	scalePlaneBilinear(frame.V, frame.Width/2, frame.Height/2, result.V, result.Width/2, result.Height/2,
		srcX/2, srcY/2, srcW/2, srcH/2, dstX/2, dstY/2, dstW/2, dstH/2)
	return result, nil
}

func evenDimension(value float64) int {
	dimension := int(value + 0.5)
	return dimension &^ 1
}

func scalePlaneBilinear(src []byte, srcStride, srcHeight int, dst []byte, dstStride, dstHeight int,
	srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH int) {
	for y := range dstH {
		fy := float64(srcY) + (float64(y)+0.5)*float64(srcH)/float64(dstH) - 0.5
		y0 := max(int(fy), srcY)
		y1 := y0 + 1
		maxY := srcY + srcH - 1
		if y1 > maxY {
			y1 = maxY
		}
		wy := fy - float64(y0)
		if wy < 0 {
			wy = 0
		}
		for x := range dstW {
			fx := float64(srcX) + (float64(x)+0.5)*float64(srcW)/float64(dstW) - 0.5
			x0 := max(int(fx), srcX)
			x1 := x0 + 1
			maxX := srcX + srcW - 1
			if x1 > maxX {
				x1 = maxX
			}
			wx := fx - float64(x0)
			if wx < 0 {
				wx = 0
			}
			a := float64(src[y0*srcStride+x0])
			b := float64(src[y0*srcStride+x1])
			c := float64(src[y1*srcStride+x0])
			d := float64(src[y1*srcStride+x1])
			top := a + (b-a)*wx
			bottom := c + (d-c)*wx
			value := top + (bottom-top)*wy
			if value < 0 {
				value = 0
			} else if value > 255 {
				value = 255
			}
			dst[(dstY+y)*dstStride+dstX+x] = byte(value + 0.5)
		}
	}
}
