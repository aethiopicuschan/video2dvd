package video2dvd

import (
	"math"
	"sync"
)

// ColorMatrix identifies the Y'CbCr matrix a video was encoded with.
type ColorMatrix uint8

const (
	// ColorMatrixUnspecified means the source carries no usable tag. Like
	// players do, the conversion then assumes BT.709 for pictures taller than
	// 576 lines or wider than 1024 samples and BT.601 for smaller ones.
	ColorMatrixUnspecified ColorMatrix = iota
	// ColorMatrixBT601 covers BT.601, BT.470BG, SMPTE 170M and FCC.
	ColorMatrixBT601
	// ColorMatrixBT709 is the HD matrix.
	ColorMatrixBT709
	// ColorMatrixOther is any tagged encoding the library does not convert,
	// such as BT.2020, SMPTE 240M, YCgCo or RGB. Samples pass through as they
	// are.
	ColorMatrixOther
)

// VideoColor describes how the samples of a DecodedVideoFrame are encoded.
// The zero value means untagged studio-range video.
//
// DVD-Video has no colour signalling that players honour: they decode every
// disc as BT.601 in studio range. FitVideoFrameForDVD therefore rewrites the
// samples of BT.709 pictures with the BT.709 to BT.601 matrix, and compresses
// full-range pictures (BT.601 or BT.709) to studio range. Primaries and
// transfer characteristics are left alone. ColorMatrixOther sources are not
// converted in any way.
type VideoColor struct {
	Matrix ColorMatrix
	// FullRange is set only when the source explicitly signals full-range
	// (0..255) samples instead of studio range (16..235 luma, 16..240
	// chroma).
	FullRange bool
}

// resolve replaces an unspecified matrix by the one players assume for a
// picture of the given source size.
func (c VideoColor) resolve(width, height int) VideoColor {
	if c.Matrix == ColorMatrixUnspecified {
		c.Matrix = ColorMatrixBT601
		if height > 576 || width > 1024 {
			c.Matrix = ColorMatrixBT709
		}
	}
	return c
}

// colorMatrixFromCode maps the matrix_coefficients code points shared by
// H.264 VUI, MP4 colr and Matroska Colour (ISO/IEC 23091-2).
func colorMatrixFromCode(code int) ColorMatrix {
	switch code {
	case 1:
		return ColorMatrixBT709
	case 4, 5, 6:
		return ColorMatrixBT601
	case 2:
		return ColorMatrixUnspecified
	}
	return ColorMatrixOther
}

const (
	colorFixedBits = 14

	bt601Kr, bt601Kb = 0.299, 0.114
	bt709Kr, bt709Kb = 0.2126, 0.0722
	bt601Kg          = 1 - bt601Kr - bt601Kb
	bt709Kg          = 1 - bt709Kr - bt709Kb

	// BT.709 Y'PbPr to R'G'B' gains, then the BT.601 luma of that R'G'B'
	// expressed on the BT.709 colour-difference signals.
	bt709RedFromPr   = 2 * (1 - bt709Kr)
	bt709BlueFromPb  = 2 * (1 - bt709Kb)
	bt709GreenFromPb = 2 * bt709Kb * (1 - bt709Kb) / bt709Kg
	bt709GreenFromPr = 2 * bt709Kr * (1 - bt709Kr) / bt709Kg
	bt601LumaFromPb  = bt601Kb*bt709BlueFromPb - bt601Kg*bt709GreenFromPb
	bt601LumaFromPr  = bt601Kr*bt709RedFromPr - bt601Kg*bt709GreenFromPr
	// The BT.601 colour-difference signals of the same R'G'B'.
	bt601PbFromPb = (bt709BlueFromPb - bt601LumaFromPb) / (2 * (1 - bt601Kb))
	bt601PbFromPr = -bt601LumaFromPr / (2 * (1 - bt601Kb))
	bt601PrFromPb = -bt601LumaFromPb / (2 * (1 - bt601Kr))
	bt601PrFromPr = (bt709RedFromPr - bt601LumaFromPr) / (2 * (1 - bt601Kr))
)

// colorTransform maps source code values to BT.601 studio-range code values
// in colorFixedBits fixed point, with cb and cr taken relative to 128:
//
//	Y'  = lumaOffset + lumaScale*Y + lumaFromCb*cb + lumaFromCr*cr
//	Cb' = 128 + cbFromCb*cb + cbFromCr*cr
//	Cr' = 128 + crFromCb*cb + crFromCr*cr
//
// Neutral samples (cb = cr = 0) stay neutral, and with studio-range input
// lumaScale is exactly one so their luma is unchanged.
type colorTransform struct {
	lumaOffset             int32
	lumaScale              int32
	lumaFromCb, lumaFromCr int32
	cbFromCb, cbFromCr     int32
	crFromCb, crFromCr     int32
}

func colorFixed(value float64) int32 {
	return int32(math.Round(value * (1 << colorFixedBits)))
}

// colorTransformForDVD reports the transform for a resolved source colour, or
// false when its samples are already BT.601 studio range or must be left
// alone.
func colorTransformForDVD(color VideoColor) (colorTransform, bool) {
	if color.Matrix != ColorMatrixBT601 && color.Matrix != ColorMatrixBT709 {
		return colorTransform{}, false
	}
	if color.Matrix == ColorMatrixBT601 && !color.FullRange {
		return colorTransform{}, false
	}
	// Excursions of the source code values for unit luma and for unit
	// colour difference (Pb, Pr = +-0.5).
	lumaRange, chromaRange, offset := 219.0, 224.0, int32(0)
	if color.FullRange {
		lumaRange, chromaRange, offset = 255, 255, 16
	}
	lumaFromPb, lumaFromPr := 0.0, 0.0
	pbFromPb, pbFromPr, prFromPb, prFromPr := 1.0, 0.0, 0.0, 1.0
	if color.Matrix == ColorMatrixBT709 {
		lumaFromPb, lumaFromPr = bt601LumaFromPb, bt601LumaFromPr
		pbFromPb, pbFromPr, prFromPb, prFromPr = bt601PbFromPb, bt601PbFromPr, bt601PrFromPb, bt601PrFromPr
	}
	return colorTransform{
		lumaOffset: offset,
		lumaScale:  colorFixed(219 / lumaRange),
		lumaFromCb: colorFixed(219 / chromaRange * lumaFromPb),
		lumaFromCr: colorFixed(219 / chromaRange * lumaFromPr),
		cbFromCb:   colorFixed(224 / chromaRange * pbFromPb),
		cbFromCr:   colorFixed(224 / chromaRange * pbFromPr),
		crFromCb:   colorFixed(224 / chromaRange * prFromPb),
		crFromCr:   colorFixed(224 / chromaRange * prFromPr),
	}, true
}

var colorLumaCorrections sync.Pool

// convertPictureColorForDVD rewrites the picture area of a fitted frame as
// BT.601 studio-range samples. Letterbox and pillarbox bars are outside the
// picture area and keep their exact black. Results saturate at 1..254 rather
// than at the studio limits: resampling overshoot outside 16..235 can still
// be a displayable colour, and clipping it there would shift that colour.
func convertPictureColorForDVD(frame *DecodedVideoFrame, picture pictureRect, color VideoColor) {
	transform, needed := colorTransformForDVD(color)
	if !needed {
		if color.Matrix == ColorMatrixBT601 {
			frame.Color = VideoColor{Matrix: ColorMatrixBT601}
		}
		return
	}
	frame.Color = VideoColor{Matrix: ColorMatrixBT601}
	chromaStride := frame.Width / 2
	chromaW, chromaH := picture.width/2, picture.height/2
	chromaOrigin := picture.y/2*chromaStride + picture.x/2

	// The chroma contribution to luma is linear, so it is formed once per
	// chroma sample and then interpolated to each luma position the way a
	// player interpolates chroma: co-sited with even columns, centred
	// between row pairs.
	var correction []int32
	if transform.lumaFromCb != 0 || transform.lumaFromCr != 0 {
		if pooled, ok := colorLumaCorrections.Get().(*[]int32); ok && cap(*pooled) >= chromaW*chromaH {
			correction = (*pooled)[:chromaW*chromaH]
		} else {
			correction = make([]int32, chromaW*chromaH)
		}
		defer colorLumaCorrections.Put(&correction)
		for row := range chromaH {
			u := frame.U[chromaOrigin+row*chromaStride:][:chromaW]
			v := frame.V[chromaOrigin+row*chromaStride:][:chromaW]
			target := correction[row*chromaW:][:chromaW]
			for x := range target {
				target[x] = transform.lumaFromCb*(int32(u[x])-128) + transform.lumaFromCr*(int32(v[x])-128)
			}
		}
	}

	const half = 1 << (colorFixedBits - 1)
	parallelMPEG2Rows(picture.height, func(row int) {
		luma := frame.Y[(picture.y+row)*frame.Width+picture.x:][:picture.width]
		if correction == nil {
			for x, sample := range luma {
				luma[x] = byte(min(max(transform.lumaOffset+(transform.lumaScale*int32(sample)+half)>>colorFixedBits, 1), 254))
			}
			return
		}
		chromaRow := row / 2
		otherRow := max(chromaRow-1, 0)
		if row%2 == 1 {
			otherRow = min(chromaRow+1, chromaH-1)
		}
		near := correction[chromaRow*chromaW:][:chromaW]
		far := correction[otherRow*chromaW:][:chromaW]
		for x, sample := range luma {
			column := x / 2
			// Vertical weights 3:1 and horizontal weights 2:0 or 1:1 give
			// eight times the interpolated correction.
			var eighths int32
			if x%2 == 0 {
				eighths = 2 * (3*near[column] + far[column])
			} else {
				next := min(column+1, chromaW-1)
				eighths = 3*(near[column]+near[next]) + far[column] + far[next]
			}
			value := transform.lumaScale*int32(sample) + (eighths+4)>>3
			luma[x] = byte(min(max(transform.lumaOffset+(value+half)>>colorFixedBits, 1), 254))
		}
	})

	for row := range chromaH {
		u := frame.U[chromaOrigin+row*chromaStride:][:chromaW]
		v := frame.V[chromaOrigin+row*chromaStride:][:chromaW]
		for x := range u {
			cb, cr := int32(u[x])-128, int32(v[x])-128
			u[x] = byte(min(max(128+(transform.cbFromCb*cb+transform.cbFromCr*cr+half)>>colorFixedBits, 1), 254))
			v[x] = byte(min(max(128+(transform.crFromCb*cb+transform.crFromCr*cr+half)>>colorFixedBits, 1), 254))
		}
	}
}
