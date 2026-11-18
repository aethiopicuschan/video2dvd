package video2dvd

import (
	"context"
	"errors"
)

// PrepareMP4VideoForDVD decodes an MP4 H.264 track, fits frames to the DVD
// raster, and converts the presentation timestamps to the selected DVD frame
// rate. Source display aspect ratio must be supplied by the caller because
// some MP4 files use non-square pixels or rotation metadata not represented by
// the decoded frame dimensions. The handler receives owned DVD-raster frames.
func PrepareMP4VideoForDVD(
	ctx context.Context,
	path string,
	trackIndex *int,
	profile Profile,
	sourceAspectNumerator, sourceAspectDenominator int,
	mode FitMode,
	handler func(DecodedVideoFrame) error,
) error {
	if handler == nil {
		return errors.New("video2dvd: DVD video frame handler is required")
	}
	rateConverter, err := NewDVDFrameRateConverter(profile, handler)
	if err != nil {
		return err
	}
	err = DecodeMP4Video(ctx, path, trackIndex, func(frame DecodedVideoFrame) error {
		fitted, err := FitVideoFrame(frame, profile, sourceAspectNumerator, sourceAspectDenominator, mode)
		if err != nil {
			return err
		}
		return rateConverter.pushOwned(fitted)
	})
	if err != nil {
		return err
	}
	return rateConverter.Finish()
}
