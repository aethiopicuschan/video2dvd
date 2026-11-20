package video2dvd

// Test-only exports for the DVD authorer, menu and image writer.

import "time"

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
	ExportCreateDVDMenuTitle       = createDVDMenuTitle
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
	layout := layoutDVDMenuPage(names, start, profile.Aspect, page, pageCount, titleLaunchBase)
	return newDVDMenuFrame(profile, layout), layout.buttons()
}

// ExportDVDMenuLabels returns the control labels followed by the title labels
// of one menu page.
func ExportDVDMenuLabels(profile Profile, names []string, start, page, pageCount int) []string {
	layout := layoutDVDMenuPage(names, start, profile.Aspect, page, pageCount, 0)
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
