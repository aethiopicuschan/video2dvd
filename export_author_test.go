package video2dvd

// Test-only exports for the DVD authorer, menu and image writer.

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"image"
	"io"
	"time"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

const (
	TestDVDTitleVOBFileMaxSectors = dvdTitleVOBFileMaxSectors
	TestDVDTitleVOBMaxFiles       = dvdTitleVOBMaxFiles
	TestDVDMenuSubpictureStream   = dvdMenuSubpictureSubstream
)

var (
	ExportEncodeDVDMenuSubpicture  = encodeDVDMenuSubpicture
	ExportDVDMenuSubpicturePalette = dvdMenuSubpicturePalette
	ExportDVDMenuSelectionColor    = dvdNAVSelectionColor
	ExportDVDMenuActionColor       = dvdNAVActionColor
	ExportMediaTitleName           = mediaTitleName
	ExportBuildDVDVTSTimeMap       = buildDVDVTSTimeMap
	ExportLetterboxDVDMenuButtons  = letterboxDVDMenuButtons
	ExportDVDMenuSubpictureControl = dvdMenuSubpictureStreamControl
	ExportCreateDVDMenuPages       = createDVDMenuPages
	ExportDVDMenuPageDuration      = dvdMenuPageDuration
	ExportDVDMenuPageAudioSamples  = dvdMenuPageAudioSamples
	ExportDVDMenuTitleName         = dvdMenuTitleName
)

// testDVDAuthoredMenu describes a menu VOB whose PGCs have the given
// subpicture stream control entry; a zero entry is a menu without the
// highlight subpicture.
func testDVDAuthoredMenu(menuSectors uint32, pageTime [4]byte, records []dvdVOBURecord, subpicture [4]byte) dvdAuthoredMenu {
	return dvdAuthoredMenu{sectors: menuSectors, pageTime: pageTime, records: records, subpictureControl: subpicture}
}

func ExportBuildDVDVMGMenuPGCI(menuSectors uint32, pageTime [4]byte, records []dvdVOBURecord, titleCount int, subpicture [4]byte) []byte {
	return buildDVDVMGMenuPGCI(testDVDAuthoredMenu(menuSectors, pageTime, records, subpicture), titleCount)
}

func ExportBuildDVDVMGI(titles []EncodedTitle, profile Profile, menuSectors uint32, records []dvdVOBURecord, subpicture [4]byte) ([]byte, error) {
	pageTime, err := dvdIFOTime(time.Second, profile.Standard)
	if err != nil {
		return nil, err
	}
	vmg, _, err := buildDVDVMGI(titles, profile, nil, testDVDAuthoredMenu(menuSectors, pageTime, records, subpicture))
	return vmg, err
}

// ExportNewSplitAuthorer returns an authorer that starts a new title VOB file
// every fileSectors sectors.
func ExportNewSplitAuthorer(fileSectors uint32) *InProcessAuthorer {
	return &InProcessAuthorer{titleVOBFileSectors: fileSectors}
}

// ExportWriteSplitTitleVOB writes data through the title VOB splitter in
// chunks of chunkSize bytes and returns the number of files created.
func ExportWriteSplitTitleVOB(dir, stem string, fileBytes int64, data []byte, chunkSize int) (int, error) {
	w := &dvdTitleVOBWriter{dir: dir, stem: stem, fileBytes: fileBytes}
	for len(data) > 0 {
		count := min(chunkSize, len(data))
		if _, err := w.Write(data[:count]); err != nil {
			_ = w.Close()
			return w.files, err
		}
		data = data[count:]
	}
	return w.files, w.Close()
}

// ExportDVDMenuPage lays out one menu page for the given title names and
// returns its picture and its buttons.
func ExportDVDMenuPage(profile Profile, names []string, start, page, pageCount, titleLaunchBase int) (DecodedVideoFrame, []DVDNAVButton) {
	text := newDVDMenuText(profile)
	layout := layoutDVDMenuPage(text, names, start, profile.Aspect, page, pageCount, titleLaunchBase)
	return newDVDMenuFrame(profile, layout, text), layout.buttons()
}

// ExportDVDMenuLabels returns the control labels followed by the title labels
// of one menu page.
func ExportDVDMenuLabels(profile Profile, names []string, start, page, pageCount int) []string {
	layout := layoutDVDMenuPage(newDVDMenuText(profile), names, start, profile.Aspect, page, pageCount, 0)
	labels := make([]string, 0, len(layout.controls)+len(layout.titles))
	for _, entry := range layout.controls {
		labels = append(labels, entry.label)
	}
	for _, entry := range layout.titles {
		labels = append(labels, entry.label)
	}
	return labels
}

func ExportDrawDVDMenuText(frame *DecodedVideoFrame, x, y int, text string, value byte) {
	drawDVDMenuText(frame, x, y, text, value)
}

// ExportDVDMenuTextWidth returns the width in pixels of text as the menu of
// the profile draws it.
func ExportDVDMenuTextWidth(profile Profile, text string) int {
	return newDVDMenuText(profile).width(text)
}

// ExportNormalizeDVDMenuText returns a heading or title name as the menu of
// the profile shows it, before truncation.
func ExportNormalizeDVDMenuText(profile Profile, text string) string {
	return newDVDMenuText(profile).normalize(text)
}

// ExportDrawDVDMenuTextWithProfile draws text like the menu of the profile,
// anywhere in the frame.
func ExportDrawDVDMenuTextWithProfile(profile Profile, frame *DecodedVideoFrame, x, y int, text string, value byte) {
	newDVDMenuText(profile).draw(frame, x, y, text, value, image.Rect(0, 0, frame.Width, frame.Height))
}

// ExportDVDMenuJapaneseGlyph returns the cell width and the rows of a glyph
// of the embedded Japanese font, as the font stores it (not emboldened). The
// leftmost pixel of a row is bit width-1.
func ExportDVDMenuJapaneseGlyph(char rune) (width int, rows []uint32, ok bool) {
	glyph, ok := lookupDVDMenuJapaneseGlyph(char)
	if !ok {
		return 0, nil, false
	}
	for row := range dvdMenuJapaneseFontHeight {
		rows = append(rows, glyph.row(row))
	}
	return glyph.width(), rows, true
}

// ExportDVDMenuJapaneseFontSummary returns the number of glyphs of the
// embedded Japanese font and the SHA-256 of its decompressed table, which
// gen/menufont prints when it writes the font.
func ExportDVDMenuJapaneseFontSummary() (glyphs int, checksum [sha256.Size]byte, err error) {
	sections, err := dvdMenuJapaneseFont()
	if err != nil {
		return 0, checksum, err
	}
	for _, section := range sections {
		glyphs += len(section.codes)
	}
	data, err := io.ReadAll(flate.NewReader(bytes.NewReader(dvdMenuJapaneseFontData)))
	return glyphs, sha256.Sum256(data), err
}

func ExportDVDMenuPixelAspect(profile Profile) (numerator, denominator int) {
	return dvdMenuPixelAspect(profile)
}

// ExportFillMenuOutline fills a closed polygon, given in 26.6 fixed-point
// pixels, and returns the coverage of every pixel of a width x height area.
func ExportFillMenuOutline(polygon []fixed.Point26_6, width, height int) []uint8 {
	segments := sfnt.Segments{{Op: sfnt.SegmentOpMoveTo, Args: [3]fixed.Point26_6{polygon[0]}}}
	for _, point := range polygon[1:] {
		segments = append(segments, sfnt.Segment{Op: sfnt.SegmentOpLineTo, Args: [3]fixed.Point26_6{point}})
	}
	var rasterizer menuOutlineRasterizer
	rasterizer.setOutline(segments, fixed.Point26_6{}, 1, 1)
	coverage := make([]uint8, width*height)
	rasterizer.draw(image.Rect(0, 0, width, height), func(x, y int, value uint8) { coverage[y*width+x] = value })
	return coverage
}
