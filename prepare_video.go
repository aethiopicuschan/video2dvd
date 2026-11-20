package video2dvd

import (
	"context"
	"errors"
)

// PrepareMP4VideoForDVD decodes an MP4 H.264 track, fits frames to the DVD
// raster and its BT.601 studio-range colour encoding (see FitVideoFrameForDVD),
// and converts the presentation timestamps to the selected DVD frame
// rate. Source display aspect ratio must be supplied by the caller because
// some MP4 files use non-square pixels or rotation metadata not represented by
// the decoded frame dimensions. The handler receives owned DVD-raster frames.
// When the track has an edit list, output starts at movie time zero: frames
// before the first media edit are skipped and a leading empty edit holds the
// first frame.
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
	return prepareMP4VideoForDVD(ctx, path, trackIndex, profile, sourceAspectNumerator, sourceAspectDenominator, mode, rateConverter)
}

// prepareMP4VideoForDVD feeds the fitted frames of an MP4 track to
// rateConverter and finishes it.
func prepareMP4VideoForDVD(
	ctx context.Context,
	path string,
	trackIndex *int,
	profile Profile,
	sourceAspectNumerator, sourceAspectDenominator int,
	mode FitMode,
	rateConverter *DVDFrameRateConverter,
) error {
	if info, probeErr := ProbeMP4(path); probeErr == nil {
		if track, selectErr := selectVideoTrack(info, trackIndex); selectErr == nil && track.edit.present {
			// Movie time zero is the edit's media time, delayed by any
			// leading empty edit, which keeps video aligned with the audio
			// track's own edit list.
			rateConverter.setOrigin(track.edit.mediaTime - track.edit.empty)
		}
	}
	err := DecodeMP4Video(ctx, path, trackIndex, func(frame DecodedVideoFrame) error {
		fitted, err := FitVideoFrameForDVD(frame, profile, sourceAspectNumerator, sourceAspectDenominator, mode)
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
