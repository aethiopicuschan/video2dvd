package video2dvd

// Test-only exports for subtitles.

// ExportFlattenSubtitleCues returns the events flattenSubtitleCues makes of
// cues, as cues.
func ExportFlattenSubtitleCues(cues []SubtitleCue) []SubtitleCue {
	events := flattenSubtitleCues(cues)
	flat := make([]SubtitleCue, len(events))
	for i, event := range events {
		flat[i] = SubtitleCue{Start: event.start, End: event.end, Text: event.text}
	}
	return flat
}

// ExportWrapSubtitleLine breaks a line for the subtitles of profile at the
// given scale and returns the lines with their widths in pixels.
func ExportWrapSubtitleLine(profile Profile, scale int, line string, maxWidth int) ([]string, []int) {
	text := newDVDScaledText(profile, scale)
	lines := wrapSubtitleLine(text, text.normalize(line), maxWidth)
	widths := make([]int, len(lines))
	for i, line := range lines {
		widths[i] = text.width(line)
	}
	return lines, widths
}

// ExportSubtitleLayout returns the lines a subtitle text is drawn in and the
// scale it is drawn at.
func ExportSubtitleLayout(profile Profile, text string) ([]string, int) {
	renderer := newDVDSubtitleRenderer(profile)
	maxWidth := profile.FrameWidth * 8 / 10
	lines := renderer.layout(renderer.text, text, maxWidth)
	if len(lines) > dvdSubtitleMaxLines {
		lines = renderer.layout(renderer.small, text, maxWidth)
		return lines[:min(len(lines), dvdSubtitleMaxLines*dvdSubtitleScale)], 1
	}
	return lines, dvdSubtitleScale
}

var (
	ExportTX3GSampleText     = tx3gSampleText
	ExportDVDSubtitlePalette = dvdSubtitlePalette
)

const (
	ExportDVDSubtitleMaxUnitBytes = dvdSubtitleMaxUnitBytes
	ExportDVDSubtitleMaxDelay     = dvdSubtitleMaxDelay
)
