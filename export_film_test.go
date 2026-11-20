package video2dvd

import (
	"context"
	"time"
)

// Test-only exports for soft telecine: the film mode of the frame-rate
// converter, the encoder's film GOP structure, and the picture display flags
// the multiplexer times a stream by.

const (
	ExportMPEG2FilmGOPPictures    = mpeg2FilmGOPPictures
	ExportMPEG2FilmMinGOPPictures = mpeg2FilmMinGOPPictures
	ExportDVDFilmMinFrames        = dvdFilmMinFrames
)

var (
	ExportTitleLengthFromFields  = titleLengthFromFields
	ExportCheckDVDFrameRate      = checkDVDFrameRate
	ExportValidateTitle          = validateTitle
	ExportMPEG2FilmDecodeFields  = mpeg2FilmDecodeFields
	ExportParseMPEG2VideoHeaders = parseMPEG2VideoHeaders
)

// ExportNewDVDTitleFrameRateConverter returns the converter the built-in
// encoder uses for a title whose video track has frames at the given times,
// and whether it is the film-rate one.
func ExportNewDVDTitleFrameRateConverter(profile Profile, times []time.Duration, handler func(DecodedVideoFrame) error) (*DVDFrameRateConverter, bool, error) {
	return newDVDTitleFrameRateConverter(profile, times, handler)
}

var (
	ExportIsFilmCadence       = isFilmCadence
	ExportMP4VideoFrameTimes  = mp4VideoFrameTimes
	ExportWebMVideoFrameTimes = webMVideoFrameTimes
)

// ExportMPEG2PictureDisplay returns what the multiplexer reads from a
// picture's coding extension.
func ExportMPEG2PictureDisplay(picture []byte) (fields int, topFieldFirst, repeatFirstField, progressiveFrame bool, err error) {
	display, err := mpeg2PictureDisplayOf(picture)
	return display.fields, display.topFieldFirst, display.repeatFirstField, display.progressiveFrame, err
}

// ExportPrepareWebMTitleVideo runs the built-in encoder's WebM video
// preparation and returns the number of frames it produced, the container
// time of the first one, and whether the source was taken for film.
func ExportPrepareWebMTitleVideo(ctx context.Context, path string, profile Profile) (frames int, start time.Duration, film bool, err error) {
	times, err := webMVideoFrameTimes(path, nil)
	if err != nil {
		return 0, 0, false, err
	}
	converter, film, err := newDVDTitleFrameRateConverter(profile, times, func(DecodedVideoFrame) error {
		frames++
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	start, err = prepareWebMVideoForDVD(ctx, path, nil, profile, 1, 1, FitLetterbox, converter)
	return frames, start, film, err
}

// ExportMPEG2RateFullness returns the decoder buffer occupancy, in bits,
// that the encoder's rate control expects just before the next picture is
// decoded.
func ExportMPEG2RateFullness(e *MPEG2VideoEncoder) float64 { return e.rate.fullness }

// ExportMPEG2PredictiveSpareBytes returns what the encoder's rate control
// lets an ordinary P picture spend beyond its share when the decoder buffer
// holds fullness bits before it and the next picture is decoded nextFields
// field periods later.
func ExportMPEG2PredictiveSpareBytes(e *MPEG2VideoEncoder, fullness float64, nextFields int) int {
	rate := e.rate
	rate.fullness = fullness
	rate.complexity = max(rate.complexity, 1)
	return rate.pictureBudget(false, false, 2, 2*nextFields, nextFields).spareBytes
}
