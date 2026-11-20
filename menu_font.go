package video2dvd

import (
	"errors"
	"fmt"
	"image"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// MenuFont is an outline font for the text of the generated title menu. Set
// Options.MenuFont to draw the menu heading and the title names with it
// instead of the built-in bitmap fonts.
//
// A MenuFont is immutable and may be shared by concurrent conversions.
type MenuFont struct {
	font *sfnt.Font
}

// ParseMenuFont parses a TrueType or OpenType font (TTF or OTF data) for
// Options.MenuFont. Given a font collection (TTC or OTC) it selects the first
// font; use ParseMenuFontIndex for another one.
//
// The font keeps referring to data, which must not be modified afterwards.
// Fonts are read with golang.org/x/image/font/sfnt, which does not accept
// every font file; an error is returned for those.
func ParseMenuFont(data []byte) (*MenuFont, error) {
	return ParseMenuFontIndex(data, 0)
}

// ParseMenuFontIndex is like ParseMenuFont but selects the font with the
// given zero-based index of a font collection. A single font is a collection
// of one.
func ParseMenuFontIndex(data []byte, index int) (menuFont *MenuFont, err error) {
	// The font parser can index past the end of a damaged table.
	defer func() {
		if recover() != nil {
			menuFont, err = nil, errors.New("video2dvd: parse menu font: malformed font data")
		}
	}()
	if len(data) == 0 {
		return nil, errors.New("video2dvd: parse menu font: no data")
	}
	collection, err := sfnt.ParseCollection(data)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: parse menu font: %w", err)
	}
	if index < 0 || index >= collection.NumFonts() {
		return nil, fmt.Errorf("video2dvd: menu font index %d is outside the %d fonts of the data", index, collection.NumFonts())
	}
	font, err := collection.Font(index)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: parse menu font: %w", err)
	}
	if font.UnitsPerEm() == 0 {
		return nil, errors.New("video2dvd: parse menu font: font has no em size")
	}
	return &MenuFont{font: font}, nil
}

const (
	// Outline text is set with a 16-line em whose top is the top of the
	// 18-line button rectangle, so the baseline is 14 lines below it: the
	// ideographic em box of a Japanese font spans 0.88 em above the
	// baseline and 0.12 em below. That leaves the lines below the baseline
	// for Latin descenders.
	dvdMenuOutlineEm       = 16
	dvdMenuOutlineBaseline = 12 // relative to the text's y position.

	// The rasteriser samples menuOutlineSubsamples lines per frame line;
	// the horizontal coverage of each is exact to 1/64 pixel.
	menuOutlineSubsamples = 16
	menuOutlineFullCover  = 64 * menuOutlineSubsamples
)

// dvdMenuPixelAspect returns the pixel aspect ratio of a profile's frame as
// width over height. It follows the convention used for MPEG-2 on DVD, in
// which 704 of the 720 pixels of a line span the picture width: 10:11 and
// 40:33 for NTSC 4:3 and 16:9, 12:11 and 16:11 for PAL.
func dvdMenuPixelAspect(profile Profile) (numerator, denominator int) {
	pal, wide := profile.Standard == VideoStandardPAL, profile.Aspect == AspectRatio16x9
	switch {
	case pal && wide:
		return 16, 11
	case pal:
		return 12, 11
	case wide:
		return 40, 33
	default:
		return 10, 11
	}
}

// menuOutlineEdge is a straight piece of a glyph outline in 26.6 fixed-point
// frame coordinates, directed so that y0 < y1. winding is +1 if the outline
// runs downwards along it and -1 otherwise.
type menuOutlineEdge struct {
	x0, y0, x1, y1 int64
	winding        int
}

// menuOutlineRasterizer turns glyph outlines into coverage. It uses integer
// arithmetic only, so that a menu picture is the same on every platform;
// floating-point rasterisers may differ in the last bit where the compiler
// fuses multiplications and additions.
type menuOutlineRasterizer struct {
	edges     []menuOutlineEdge
	active    []menuOutlineEdge
	crossings []menuOutlineCrossing
	coverage  []int32
}

type menuOutlineCrossing struct {
	x       int64
	winding int
}

func (r *menuOutlineRasterizer) line(ax, ay, bx, by int64) {
	switch {
	case ay < by:
		r.edges = append(r.edges, menuOutlineEdge{ax, ay, bx, by, 1})
	case ay > by:
		r.edges = append(r.edges, menuOutlineEdge{bx, by, ax, ay, -1})
	}
}

// menuOutlineCurveSteps returns the number of lines that approximate a curve
// whose control points span the given distance: one per pixel and a half.
func menuOutlineCurveSteps(points ...fixed.Point26_6) int64 {
	bounds := fixed.Rectangle26_6{Min: points[0], Max: points[0]}
	for _, p := range points[1:] {
		bounds = bounds.Union(fixed.Rectangle26_6{Min: p, Max: p})
	}
	extent := max(bounds.Max.X-bounds.Min.X, bounds.Max.Y-bounds.Min.Y)
	return min(int64(extent)/96+2, 16)
}

// divideRounded divides rounding half away from zero.
func divideRounded(value, divisor int64) int64 {
	if value < 0 {
		return -((-value + divisor/2) / divisor)
	}
	return (value + divisor/2) / divisor
}

// setOutline replaces the rasteriser's edges by those of a glyph. Segment
// coordinates are scaled horizontally by scaleNumerator/scaleDenominator and
// moved to the origin, which is in 26.6 fixed point.
func (r *menuOutlineRasterizer) setOutline(segments sfnt.Segments, origin fixed.Point26_6, scaleNumerator, scaleDenominator int) {
	r.edges = r.edges[:0]
	place := func(p fixed.Point26_6) fixed.Point26_6 {
		return fixed.Point26_6{
			X: origin.X + fixed.Int26_6(divideRounded(int64(p.X)*int64(scaleNumerator), int64(scaleDenominator))),
			Y: origin.Y + p.Y,
		}
	}
	var start, pen fixed.Point26_6
	lineTo := func(p fixed.Point26_6) {
		r.line(int64(pen.X), int64(pen.Y), int64(p.X), int64(p.Y))
		pen = p
	}
	for _, segment := range segments {
		a, b, c := place(segment.Args[0]), place(segment.Args[1]), place(segment.Args[2])
		switch segment.Op {
		case sfnt.SegmentOpMoveTo:
			lineTo(start) // close the previous contour.
			start, pen = a, a
		case sfnt.SegmentOpLineTo:
			lineTo(a)
		case sfnt.SegmentOpQuadTo:
			from, n := pen, menuOutlineCurveSteps(pen, a, b)
			for i := int64(1); i <= n; i++ {
				// Bernstein weights of the point at i/n, times n*n.
				w0, w1, w2 := (n-i)*(n-i), 2*i*(n-i), i*i
				lineTo(fixed.Point26_6{
					X: fixed.Int26_6(divideRounded(w0*int64(from.X)+w1*int64(a.X)+w2*int64(b.X), n*n)),
					Y: fixed.Int26_6(divideRounded(w0*int64(from.Y)+w1*int64(a.Y)+w2*int64(b.Y), n*n)),
				})
			}
		case sfnt.SegmentOpCubeTo:
			from, n := pen, menuOutlineCurveSteps(pen, a, b, c)
			for i := int64(1); i <= n; i++ {
				j := n - i
				w0, w1, w2, w3 := j*j*j, 3*i*j*j, 3*i*i*j, i*i*i
				lineTo(fixed.Point26_6{
					X: fixed.Int26_6(divideRounded(w0*int64(from.X)+w1*int64(a.X)+w2*int64(b.X)+w3*int64(c.X), n*n*n)),
					Y: fixed.Int26_6(divideRounded(w0*int64(from.Y)+w1*int64(a.Y)+w2*int64(b.Y)+w3*int64(c.Y), n*n*n)),
				})
			}
		}
	}
	lineTo(start)
}

// draw fills the current outline with the non-zero winding rule and calls
// set for every pixel inside clip that it touches, with the covered share of
// the pixel from 1 to 255.
func (r *menuOutlineRasterizer) draw(clip image.Rectangle, set func(x, y int, coverage uint8)) {
	if len(r.edges) == 0 {
		return
	}
	bounds := image.Rectangle{Min: image.Pt(1<<30, 1<<30), Max: image.Pt(-1<<30, -1<<30)}
	for _, edge := range r.edges {
		bounds.Min.X = min(bounds.Min.X, int(min(edge.x0, edge.x1)>>6))
		bounds.Max.X = max(bounds.Max.X, int(max(edge.x0, edge.x1)>>6)+1)
		bounds.Min.Y = min(bounds.Min.Y, int(edge.y0>>6))
		bounds.Max.Y = max(bounds.Max.Y, int(edge.y1>>6)+1)
	}
	bounds = bounds.Intersect(clip)
	if bounds.Empty() {
		return
	}
	if cap(r.coverage) < bounds.Dx() {
		r.coverage = make([]int32, bounds.Dx())
	}
	coverage := r.coverage[:bounds.Dx()]
	left, right := int64(bounds.Min.X)<<6, int64(bounds.Max.X)<<6
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		clear(coverage)
		r.active = r.active[:0]
		for _, edge := range r.edges {
			if edge.y0 < int64(y+1)<<6 && int64(y)<<6 < edge.y1 {
				r.active = append(r.active, edge)
			}
		}
		for sample := range int64(menuOutlineSubsamples) {
			// Sample lines sit in the middle of their share of the pixel.
			sampleY := int64(y)<<6 + (2*sample+1)*64/(2*menuOutlineSubsamples)
			r.crossings = r.crossings[:0]
			for _, edge := range r.active {
				if edge.y0 <= sampleY && sampleY < edge.y1 {
					x := edge.x0 + (edge.x1-edge.x0)*(sampleY-edge.y0)/(edge.y1-edge.y0)
					r.crossings = append(r.crossings, menuOutlineCrossing{x, edge.winding})
				}
			}
			// Insertion sort: there are few crossings per line.
			for i := 1; i < len(r.crossings); i++ {
				for j := i; j > 0 && r.crossings[j].x < r.crossings[j-1].x; j-- {
					r.crossings[j], r.crossings[j-1] = r.crossings[j-1], r.crossings[j]
				}
			}
			winding, spanStart := 0, int64(0)
			for _, crossing := range r.crossings {
				if winding == 0 {
					spanStart = crossing.x
				}
				winding += crossing.winding
				if winding != 0 {
					continue
				}
				from, to := max(spanStart, left), min(crossing.x, right)
				for x := from >> 6; x<<6 < to; x++ {
					coverage[x-int64(bounds.Min.X)] += int32(min(to, (x+1)<<6) - max(from, x<<6))
				}
			}
		}
		for i, covered := range coverage {
			if value := (covered*255 + menuOutlineFullCover/2) / menuOutlineFullCover; value > 0 {
				set(bounds.Min.X+i, y, uint8(min(value, 255)))
			}
		}
	}
}
