package video2dvd

import (
	"errors"
	"fmt"
	"math"
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
// as 16 and 9. The output uses bilinear scaling, low-pass filtered along any
// axis that is reduced, and even chroma-aligned edges.
//
// For FitLetterbox, unused pixels are set to studio-range black. For FitCrop,
// the source is center-cropped. The input frame is not modified.
func FitVideoFrame(frame DecodedVideoFrame, profile Profile, sourceAspectNumerator, sourceAspectDenominator int, mode FitMode) (DecodedVideoFrame, error) {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Width%2 != 0 || frame.Height%2 != 0 || frame.Width > int(^uint(0)>>1)/frame.Height {
		return DecodedVideoFrame{}, errors.New("video2dvd: source frame dimensions must be positive even values")
	}
	if len(frame.Y) < frame.Width*frame.Height || len(frame.U) < frame.Width*frame.Height/4 || len(frame.V) < frame.Width*frame.Height/4 {
		return DecodedVideoFrame{}, errors.New("video2dvd: source frame has incomplete YUV 4:2:0 planes")
	}
	if sourceAspectNumerator <= 0 || sourceAspectDenominator <= 0 {
		return DecodedVideoFrame{}, errors.New("video2dvd: source display aspect ratio must be positive")
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
		return DecodedVideoFrame{}, fmt.Errorf("video2dvd: unknown fit mode %d", mode)
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
		return DecodedVideoFrame{}, errors.New("video2dvd: aspect ratio leaves less than one chroma sample")
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
	scalePlane(frame.Y, frame.Width, frame.Height, result.Y, result.Width, result.Height,
		srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH)
	scalePlane(frame.U, frame.Width/2, frame.Height/2, result.U, result.Width/2, result.Height/2,
		srcX/2, srcY/2, srcW/2, srcH/2, dstX/2, dstY/2, dstW/2, dstH/2)
	scalePlane(frame.V, frame.Width/2, frame.Height/2, result.V, result.Width/2, result.Height/2,
		srcX/2, srcY/2, srcW/2, srcH/2, dstX/2, dstY/2, dstW/2, dstH/2)
	return result, nil
}

// scalePlane resamples one plane. Two-tap bilinear sampling skips source
// samples once an axis shrinks, which folds fine detail back as moire, so
// reductions go through a filter whose support grows with the ratio.
func scalePlane(src []byte, srcStride, srcHeight int, dst []byte, dstStride, dstHeight int,
	srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH int) {
	if srcW <= dstW && srcH <= dstH {
		scalePlaneBilinear(src, srcStride, srcHeight, dst, dstStride, dstHeight,
			srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH)
		return
	}
	scalePlaneFiltered(src, srcStride, dst, dstStride, srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH)
}

const resampleWeightBits = 14

// resampleKernel holds, for every output sample of one axis, the first source
// sample it reads and taps fixed-point weights that sum to 1<<resampleWeightBits.
type resampleKernel struct {
	taps    int
	first   []int
	weights []int32
}

// newResampleKernel builds a triangle (bilinear) kernel widened by the
// reduction ratio. At a ratio of one or less it is plain bilinear
// interpolation. Samples beyond the source edge repeat the edge sample.
func newResampleKernel(srcSize, dstSize int) resampleKernel {
	ratio := float64(srcSize) / float64(dstSize)
	support := max(ratio, 1)
	taps := min(int(2*support)+2, srcSize)
	kernel := resampleKernel{taps: taps, first: make([]int, dstSize), weights: make([]int32, dstSize*taps)}
	scratch := make([]float64, taps)
	for index := range dstSize {
		center := (float64(index)+0.5)*ratio - 0.5
		lowest := int(math.Ceil(center - support))
		highest := int(math.Floor(center + support))
		first := min(max(lowest, 0), srcSize-taps)
		clear(scratch)
		var total float64
		for sample := lowest; sample <= highest; sample++ {
			weight := 1 - math.Abs(float64(sample)-center)/support
			if weight <= 0 {
				continue
			}
			scratch[min(max(sample, 0), srcSize-1)-first] += weight
			total += weight
		}
		weights := kernel.weights[index*taps : (index+1)*taps]
		remainder, largest := int32(1<<resampleWeightBits), 0
		for tap, weight := range scratch {
			weights[tap] = int32(weight/total*(1<<resampleWeightBits) + 0.5)
			remainder -= weights[tap]
			if weights[tap] > weights[largest] {
				largest = tap
			}
		}
		weights[largest] += remainder
		kernel.first[index] = first
	}
	return kernel
}

func scalePlaneFiltered(src []byte, srcStride int, dst []byte, dstStride int,
	srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH int) {
	horizontal := newResampleKernel(srcW, dstW)
	vertical := newResampleKernel(srcH, dstH)
	// The intermediate keeps six fractional bits so the vertical pass does
	// not compound the horizontal rounding.
	intermediate := make([]uint16, srcH*dstW)
	parallelMPEG2Rows(srcH, func(row int) {
		source := src[(srcY+row)*srcStride+srcX:]
		target := intermediate[row*dstW : (row+1)*dstW]
		for x := range target {
			samples := source[horizontal.first[x]:]
			var sum int32
			for tap, weight := range horizontal.weights[x*horizontal.taps : (x+1)*horizontal.taps] {
				sum += weight * int32(samples[tap])
			}
			target[x] = uint16((sum + 1<<7) >> 8)
		}
	})
	parallelMPEG2Rows(dstH, func(row int) {
		weights := vertical.weights[row*vertical.taps : (row+1)*vertical.taps]
		first := vertical.first[row]
		target := dst[(dstY+row)*dstStride+dstX:]
		for x := range dstW {
			var sum int32
			for tap, weight := range weights {
				sum += weight * int32(intermediate[(first+tap)*dstW+x])
			}
			target[x] = byte((sum + 1<<(2*resampleWeightBits-9)) >> (2*resampleWeightBits - 8))
		}
	})
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
