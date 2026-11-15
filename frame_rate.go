package mp4todvd

import (
	"errors"
	"math"
	"time"
)

// DVDFrameRateConverter maps timestamped decoded frames to the standard DVD
// output rate: 30000/1001 fps for NTSC and 25 fps for PAL. It holds the most
// recent source frame and repeats or drops frames as needed. Input PTS values
// must be in presentation order and each frame must carry a positive Duration.
type DVDFrameRateConverter struct {
	rateNumerator   int64
	rateDenominator int64
	handler         func(DecodedVideoFrame) error
	pending         *DecodedVideoFrame
	origin          time.Duration
	lastInputPTS    time.Duration
	nextIndex       int64
	width           int
	height          int
	haveInput       bool
	finished        bool
}

// NewDVDFrameRateConverter creates a streaming DVD frame-rate converter.
func NewDVDFrameRateConverter(profile Profile, handler func(DecodedVideoFrame) error) (*DVDFrameRateConverter, error) {
	if handler == nil {
		return nil, errors.New("mp4todvd: frame handler is required")
	}
	converter := &DVDFrameRateConverter{handler: handler}
	switch profile.Standard {
	case VideoStandardNTSC:
		if profile.FrameWidth != 720 || profile.FrameHeight != 480 {
			return nil, ErrInvalidProfile
		}
		converter.rateNumerator, converter.rateDenominator = 30000, 1001
	case VideoStandardPAL:
		if profile.FrameWidth != 720 || profile.FrameHeight != 576 {
			return nil, ErrInvalidProfile
		}
		converter.rateNumerator, converter.rateDenominator = 25, 1
	default:
		return nil, ErrInvalidProfile
	}
	return converter, nil
}

// Push adds one decoded frame. Output is emitted as soon as the next input
// frame establishes that no earlier source frame can be selected for that
// output timestamp.
func (c *DVDFrameRateConverter) Push(frame DecodedVideoFrame) error {
	if c == nil || c.handler == nil {
		return errors.New("mp4todvd: frame-rate converter is not configured")
	}
	if c.finished {
		return errors.New("mp4todvd: frame-rate converter is already finished")
	}
	if err := validateDecodedFrame(frame); err != nil {
		return err
	}
	if frame.Duration <= 0 {
		return errors.New("mp4todvd: decoded frame duration must be positive")
	}
	if frame.PTS > time.Duration(math.MaxInt64)-frame.Duration {
		return errors.New("mp4todvd: decoded frame end timestamp overflows")
	}
	if c.haveInput {
		if frame.Width != c.width || frame.Height != c.height {
			return errors.New("mp4todvd: frame dimensions changed during frame-rate conversion")
		}
		if frame.PTS < c.lastInputPTS {
			return errors.New("mp4todvd: decoded frames must be in presentation order")
		}
		limit, err := subtractDurations(frame.PTS, c.origin)
		if err != nil {
			return err
		}
		if err := c.emitBefore(limit); err != nil {
			return err
		}
	} else {
		c.origin = frame.PTS
		c.width, c.height = frame.Width, frame.Height
		c.haveInput = true
	}
	owned := cloneDecodedVideoFrame(frame)
	c.pending = &owned
	c.lastInputPTS = frame.PTS
	return nil
}

// Finish emits output timestamps covered by the final input frame and closes
// the converter. It may be called only once.
func (c *DVDFrameRateConverter) Finish() error {
	if c == nil || c.handler == nil {
		return errors.New("mp4todvd: frame-rate converter is not configured")
	}
	if c.finished {
		return errors.New("mp4todvd: frame-rate converter is already finished")
	}
	c.finished = true
	if !c.haveInput || c.pending == nil {
		return nil
	}
	frameEnd := c.pending.PTS + c.pending.Duration
	end, err := subtractDurations(frameEnd, c.origin)
	if err != nil {
		return err
	}
	return c.emitBefore(end)
}

func subtractDurations(left, right time.Duration) (time.Duration, error) {
	if right < 0 && left > time.Duration(math.MaxInt64)+right {
		return 0, errors.New("mp4todvd: timestamp difference overflows duration")
	}
	if right > 0 && left < time.Duration(math.MinInt64)+right {
		return 0, errors.New("mp4todvd: timestamp difference overflows duration")
	}
	return left - right, nil
}

func (c *DVDFrameRateConverter) emitBefore(limit time.Duration) error {
	for c.pending != nil {
		pts, err := c.outputTimestamp(c.nextIndex)
		if err != nil {
			return err
		}
		if pts >= limit {
			return nil
		}
		nextPTS, err := c.outputTimestamp(c.nextIndex + 1)
		if err != nil {
			return err
		}
		frame := cloneDecodedVideoFrame(*c.pending)
		frame.PTS = pts
		frame.Duration = nextPTS - pts
		if err := c.handler(frame); err != nil {
			return err
		}
		c.nextIndex++
	}
	return nil
}

func (c *DVDFrameRateConverter) outputTimestamp(index int64) (time.Duration, error) {
	if index < 0 {
		return 0, errors.New("mp4todvd: negative output frame index")
	}
	unit := c.rateDenominator * int64(time.Second)
	whole := index / c.rateNumerator
	remainder := index % c.rateNumerator
	if whole > math.MaxInt64/unit {
		return 0, errors.New("mp4todvd: output timestamp overflows duration")
	}
	nanoseconds := whole*unit + remainder*unit/c.rateNumerator
	if nanoseconds > math.MaxInt64 {
		return 0, errors.New("mp4todvd: output timestamp overflows duration")
	}
	return time.Duration(nanoseconds), nil
}

func validateDecodedFrame(frame DecodedVideoFrame) error {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Width%2 != 0 || frame.Height%2 != 0 || frame.Width > int(^uint(0)>>1)/frame.Height {
		return errors.New("mp4todvd: decoded frame dimensions must be positive even values")
	}
	pixels := frame.Width * frame.Height
	if len(frame.Y) < pixels || len(frame.U) < pixels/4 || len(frame.V) < pixels/4 {
		return errors.New("mp4todvd: decoded frame has incomplete YUV 4:2:0 planes")
	}
	return nil
}

func cloneDecodedVideoFrame(frame DecodedVideoFrame) DecodedVideoFrame {
	return DecodedVideoFrame{
		PTS:      frame.PTS,
		Duration: frame.Duration,
		Width:    frame.Width,
		Height:   frame.Height,
		Y:        append([]byte(nil), frame.Y...),
		U:        append([]byte(nil), frame.U...),
		V:        append([]byte(nil), frame.V...),
	}
}
