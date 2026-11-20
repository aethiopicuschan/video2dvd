package video2dvd

import (
	"errors"
	"fmt"
	"math"
	"sync"
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
// as 16 and 9. Enlargement and reduction both use a Lanczos-3 filter whose
// support grows with the reduction ratio. Chroma is treated as horizontally
// co-sited with even luma columns (the MPEG-2, H.264 and VP9 default) and
// vertically centred, and picture edges are chroma-aligned.
//
// For FitLetterbox, unused pixels are set to studio-range black. For FitCrop,
// the source is center-cropped. The input frame is not modified. Samples are
// only resampled: no colour-matrix or range conversion is applied and Color
// is copied from the source. FitVideoFrameForDVD adds that conversion.
func FitVideoFrame(frame DecodedVideoFrame, profile Profile, sourceAspectNumerator, sourceAspectDenominator int, mode FitMode) (DecodedVideoFrame, error) {
	result, _, err := fitVideoFrame(frame, profile, sourceAspectNumerator, sourceAspectDenominator, mode)
	return result, err
}

// FitVideoFrameForDVD is FitVideoFrame followed by conversion of the picture
// area to the colour encoding DVD players assume, BT.601 Y'CbCr in studio
// range. frame.Color selects the conversion; see VideoColor. The preparation
// functions and the built-in encoder use this function.
func FitVideoFrameForDVD(frame DecodedVideoFrame, profile Profile, sourceAspectNumerator, sourceAspectDenominator int, mode FitMode) (DecodedVideoFrame, error) {
	result, picture, err := fitVideoFrame(frame, profile, sourceAspectNumerator, sourceAspectDenominator, mode)
	if err != nil {
		return DecodedVideoFrame{}, err
	}
	convertPictureColorForDVD(&result, picture, frame.Color.resolve(frame.Width, frame.Height))
	return result, nil
}

// pictureRect is the part of a fitted frame that carries source picture. All
// four values are even.
type pictureRect struct {
	x, y, width, height int
}

func fitVideoFrame(frame DecodedVideoFrame, profile Profile, sourceAspectNumerator, sourceAspectDenominator int, mode FitMode) (DecodedVideoFrame, pictureRect, error) {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Width%2 != 0 || frame.Height%2 != 0 || frame.Width > int(^uint(0)>>1)/frame.Height {
		return DecodedVideoFrame{}, pictureRect{}, errors.New("video2dvd: source frame dimensions must be positive even values")
	}
	if len(frame.Y) < frame.Width*frame.Height || len(frame.U) < frame.Width*frame.Height/4 || len(frame.V) < frame.Width*frame.Height/4 {
		return DecodedVideoFrame{}, pictureRect{}, errors.New("video2dvd: source frame has incomplete YUV 4:2:0 planes")
	}
	if sourceAspectNumerator <= 0 || sourceAspectDenominator <= 0 {
		return DecodedVideoFrame{}, pictureRect{}, errors.New("video2dvd: source display aspect ratio must be positive")
	}
	wantWidth, wantHeight := 0, 0
	switch profile.Standard {
	case VideoStandardNTSC:
		wantWidth, wantHeight = 720, 480
	case VideoStandardPAL:
		wantWidth, wantHeight = 720, 576
	default:
		return DecodedVideoFrame{}, pictureRect{}, ErrInvalidProfile
	}
	if profile.FrameWidth != wantWidth || profile.FrameHeight != wantHeight {
		return DecodedVideoFrame{}, pictureRect{}, ErrInvalidProfile
	}
	if profile.Aspect != AspectRatio4x3 && profile.Aspect != AspectRatio16x9 {
		return DecodedVideoFrame{}, pictureRect{}, ErrInvalidProfile
	}
	if mode != FitLetterbox && mode != FitCrop {
		return DecodedVideoFrame{}, pictureRect{}, fmt.Errorf("video2dvd: unknown fit mode %d", mode)
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
		return DecodedVideoFrame{}, pictureRect{}, errors.New("video2dvd: aspect ratio leaves less than one chroma sample")
	}

	result := DecodedVideoFrame{
		PTS:      frame.PTS,
		Duration: frame.Duration,
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Color:    frame.Color,
		Y:        make([]byte, profile.FrameWidth*profile.FrameHeight),
		U:        make([]byte, profile.FrameWidth*profile.FrameHeight/4),
		V:        make([]byte, profile.FrameWidth*profile.FrameHeight/4),
	}
	if dstW != result.Width || dstH != result.Height {
		for i := range result.Y {
			result.Y[i] = 16
		}
		for i := range result.U {
			result.U[i], result.V[i] = 128, 128
		}
	}
	scalePlane(frame.Y, frame.Width, result.Y, result.Width,
		srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH, resamplePhaseCentre)
	scalePlane(frame.U, frame.Width/2, result.U, result.Width/2,
		srcX/2, srcY/2, srcW/2, srcH/2, dstX/2, dstY/2, dstW/2, dstH/2, resamplePhaseLeft)
	scalePlane(frame.V, frame.Width/2, result.V, result.Width/2,
		srcX/2, srcY/2, srcW/2, srcH/2, dstX/2, dstY/2, dstW/2, dstH/2, resamplePhaseLeft)
	return result, pictureRect{x: dstX, y: dstY, width: dstW, height: dstH}, nil
}

const (
	resampleWeightBits = 14
	// resampleIntermediateBits is the fraction kept between the two passes
	// so the vertical pass does not compound the horizontal rounding.
	resampleIntermediateBits = 6
	// resampleLobes is the Lanczos window size.
	resampleLobes = 3

	// Sample positions as a fraction of the sample pitch: 0.5 for samples
	// centred in their cell (luma, and 4:2:0 chroma vertically), 0.25 for
	// 4:2:0 chroma co-sited with even luma columns.
	resamplePhaseCentre = 0.5
	resamplePhaseLeft   = 0.25
)

// resampleKernel holds, for every output sample of one axis, the first source
// sample it reads and taps signed fixed-point weights that sum to
// 1<<resampleWeightBits.
type resampleKernel struct {
	taps    int
	first   []int
	weights []int32
}

type resampleKernelKey struct {
	srcSize, dstSize int
	phase            float64
}

// Kernels depend only on the two sizes and the sample siting, and a title
// asks for the same few on every frame.
var resampleKernels struct {
	sync.Mutex
	cache map[resampleKernelKey]*resampleKernel
}

const maxCachedResampleKernels = 64

func cachedResampleKernel(srcSize, dstSize int, phase float64) *resampleKernel {
	key := resampleKernelKey{srcSize: srcSize, dstSize: dstSize, phase: phase}
	resampleKernels.Lock()
	defer resampleKernels.Unlock()
	if kernel := resampleKernels.cache[key]; kernel != nil {
		return kernel
	}
	if resampleKernels.cache == nil || len(resampleKernels.cache) >= maxCachedResampleKernels {
		resampleKernels.cache = make(map[resampleKernelKey]*resampleKernel)
	}
	kernel := newResampleKernel(srcSize, dstSize, phase)
	resampleKernels.cache[key] = kernel
	return kernel
}

// newResampleKernel builds a Lanczos kernel stretched by the reduction ratio,
// so it low-passes at the output sampling rate; at a ratio of one or less it
// interpolates. phase is the position of a sample within its pitch on both
// the source and the output grid. Samples beyond the source edge repeat the
// edge sample. Equal sizes give the identity for either phase.
func newResampleKernel(srcSize, dstSize int, phase float64) *resampleKernel {
	ratio := float64(srcSize) / float64(dstSize)
	stretch := max(ratio, 1)
	support := resampleLobes * stretch
	taps := min(int(2*support)+2, srcSize)
	kernel := &resampleKernel{taps: taps, first: make([]int, dstSize), weights: make([]int32, dstSize*taps)}
	scratch := make([]float64, taps)
	for index := range dstSize {
		center := (float64(index)+phase)*ratio - phase
		lowest := int(math.Ceil(center - support))
		highest := int(math.Floor(center + support))
		first := min(max(lowest, 0), srcSize-taps)
		clear(scratch)
		var total float64
		for sample := lowest; sample <= highest; sample++ {
			weight := lanczosWeight((float64(sample)-center)/stretch, resampleLobes)
			scratch[min(max(sample, 0), srcSize-1)-first] += weight
			total += weight
		}
		weights := kernel.weights[index*taps : (index+1)*taps]
		remainder, largest := int32(1<<resampleWeightBits), 0
		for tap, weight := range scratch {
			weights[tap] = int32(math.Round(weight / total * (1 << resampleWeightBits)))
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

func lanczosWeight(distance, lobes float64) float64 {
	distance = math.Abs(distance)
	if distance < 1e-9 {
		return 1
	}
	if distance >= lobes {
		return 0
	}
	x := math.Pi * distance
	return lobes * math.Sin(x) * math.Sin(x/lobes) / (x * x)
}

var resampleIntermediates sync.Pool

// scalePlane resamples the source rectangle of one plane into the destination
// rectangle. Every output sample is an integer function of the source alone,
// so the result does not depend on how rows are shared between goroutines.
func scalePlane(src []byte, srcStride int, dst []byte, dstStride int,
	srcX, srcY, srcW, srcH, dstX, dstY, dstW, dstH int, horizontalPhase float64) {
	if srcW == dstW && srcH == dstH {
		for row := range dstH {
			copy(dst[(dstY+row)*dstStride+dstX:][:dstW], src[(srcY+row)*srcStride+srcX:])
		}
		return
	}
	horizontal := cachedResampleKernel(srcW, dstW, horizontalPhase)
	vertical := cachedResampleKernel(srcH, dstH, resamplePhaseCentre)
	var intermediate []int16
	if pooled, ok := resampleIntermediates.Get().(*[]int16); ok && cap(*pooled) >= srcH*dstW {
		intermediate = (*pooled)[:srcH*dstW]
	} else {
		intermediate = make([]int16, srcH*dstW)
	}
	defer resampleIntermediates.Put(&intermediate)

	const horizontalShift = resampleWeightBits - resampleIntermediateBits
	hTaps := horizontal.taps
	parallelMPEG2Rows(srcH, func(row int) {
		source := src[(srcY+row)*srcStride+srcX:][:srcW]
		target := intermediate[row*dstW:][:dstW]
		for x := range target {
			samples := source[horizontal.first[x]:][:hTaps]
			weights := horizontal.weights[x*hTaps:][:hTaps]
			var sum int32
			for tap, sample := range samples {
				sum += weights[tap] * int32(sample)
			}
			// Signed weights overshoot next to edges; the sample range is
			// restored once, after the vertical pass.
			target[x] = int16(min(max((sum+1<<(horizontalShift-1))>>horizontalShift, math.MinInt16), math.MaxInt16))
		}
	})

	const verticalShift = resampleWeightBits + resampleIntermediateBits
	vTaps := vertical.taps
	parallelMPEG2Rows(dstH, func(row int) {
		var buffer [720]int32
		sums := buffer[:]
		if dstW > len(sums) {
			sums = make([]int32, dstW)
		}
		sums = sums[:dstW]
		first := vertical.first[row]
		for tap, weight := range vertical.weights[row*vTaps:][:vTaps] {
			if weight == 0 {
				continue
			}
			for x, sample := range intermediate[(first+tap)*dstW:][:dstW] {
				sums[x] += weight * int32(sample)
			}
		}
		target := dst[(dstY+row)*dstStride+dstX:][:dstW]
		for x, sum := range sums {
			target[x] = byte(min(max((sum+1<<(verticalShift-1))>>verticalShift, 0), 255))
		}
	})
}

func evenDimension(value float64) int {
	dimension := int(value + 0.5)
	return dimension &^ 1
}
