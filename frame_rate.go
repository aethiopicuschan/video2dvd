package video2dvd

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
	// side remembers on which side of an output timestamp the last input
	// frame near one fell: -1 at or before it, +1 after it, 0 not yet known.
	side        int8
	haveInput   bool
	fixedOrigin bool
	finished    bool
}

// NewDVDFrameRateConverter creates a streaming DVD frame-rate converter.
func NewDVDFrameRateConverter(profile Profile, handler func(DecodedVideoFrame) error) (*DVDFrameRateConverter, error) {
	if handler == nil {
		return nil, errors.New("video2dvd: frame handler is required")
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

// setOrigin makes source time origin the first output timestamp instead of
// the first frame's PTS. Frames that end before it are dropped, and a first
// frame that starts after it is shown from the origin.
func (c *DVDFrameRateConverter) setOrigin(origin time.Duration) {
	c.origin = origin
	c.fixedOrigin = true
}

// Push adds one decoded frame. Output is emitted as soon as the next input
// frame establishes that no earlier source frame can be selected for that
// output timestamp.
func (c *DVDFrameRateConverter) Push(frame DecodedVideoFrame) error {
	return c.push(frame, true)
}

// pushOwned accepts a frame whose planes are newly allocated for this call.
// The streaming preparation paths use it to avoid copying the fitted frame
// before the next output timestamp is ready.
func (c *DVDFrameRateConverter) pushOwned(frame DecodedVideoFrame) error {
	return c.push(frame, false)
}

func (c *DVDFrameRateConverter) push(frame DecodedVideoFrame, cloneFrame bool) error {
	if c == nil || c.handler == nil {
		return errors.New("video2dvd: frame-rate converter is not configured")
	}
	if c.finished {
		return errors.New("video2dvd: frame-rate converter is already finished")
	}
	if err := validateDecodedFrame(frame); err != nil {
		return err
	}
	if frame.Duration <= 0 {
		return errors.New("video2dvd: decoded frame duration must be positive")
	}
	if frame.PTS > time.Duration(math.MaxInt64)-frame.Duration {
		return errors.New("video2dvd: decoded frame end timestamp overflows")
	}
	if c.haveInput {
		if frame.Width != c.width || frame.Height != c.height {
			return errors.New("video2dvd: frame dimensions changed during frame-rate conversion")
		}
		if frame.PTS < c.lastInputPTS {
			return errors.New("video2dvd: decoded frames must be in presentation order")
		}
		limit, err := subtractDurations(frame.PTS, c.origin)
		if err != nil {
			return err
		}
		threshold, err := c.steadyThreshold(limit - c.tolerance())
		if err != nil {
			return err
		}
		if err := c.emitBefore(threshold); err != nil {
			return err
		}
	} else {
		if !c.fixedOrigin {
			c.origin = frame.PTS
		}
		c.width, c.height = frame.Width, frame.Height
		c.haveInput = true
	}
	if cloneFrame {
		owned := cloneDecodedVideoFrame(frame)
		c.pending = &owned
	} else {
		c.pending = &frame
	}
	c.lastInputPTS = frame.PTS
	return nil
}

// Finish emits output timestamps covered by the final input frame and closes
// the converter. It may be called only once.
func (c *DVDFrameRateConverter) Finish() error {
	if c == nil || c.handler == nil {
		return errors.New("video2dvd: frame-rate converter is not configured")
	}
	if c.finished {
		return errors.New("video2dvd: frame-rate converter is already finished")
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
	return c.emitBefore(end - c.tolerance())
}

func subtractDurations(left, right time.Duration) (time.Duration, error) {
	if right < 0 && left > time.Duration(math.MaxInt64)+right {
		return 0, errors.New("video2dvd: timestamp difference overflows duration")
	}
	if right > 0 && left < time.Duration(math.MinInt64)+right {
		return 0, errors.New("video2dvd: timestamp difference overflows duration")
	}
	return left - right, nil
}

// emitBefore shows the pending frame at every output timestamp before
// threshold. Callers pass the next frame's start (or the last frame's end)
// less tolerance: source timestamps are rounded to container ticks, so a
// frame that starts within a quarter of an output period after an output
// timestamp is the one meant for it; otherwise equal cadences would repeat
// and drop frames.
func (c *DVDFrameRateConverter) emitBefore(threshold time.Duration) error {
	for c.pending != nil {
		pts, err := c.outputTimestamp(c.nextIndex)
		if err != nil {
			return err
		}
		if pts >= threshold {
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

func (c *DVDFrameRateConverter) period() time.Duration {
	return time.Duration(c.rateDenominator) * time.Second / time.Duration(c.rateNumerator)
}

func (c *DVDFrameRateConverter) tolerance() time.Duration {
	return c.period() / 4
}

// steadyThreshold adds hysteresis to the decision whether an input frame
// starts before or after an output timestamp. A source whose cadence is close
// to a multiple of the output rate drifts across that decision slowly, and
// timestamp rounding would otherwise flip it back and forth for dozens of
// frames, each flip a dropped or repeated frame. A threshold within a
// sixteenth of an output period of an output timestamp is therefore moved to
// the side the preceding nearby frames were on; the side changes only once a
// frame is clearly, by more than that margin, on the other one.
func (c *DVDFrameRateConverter) steadyThreshold(threshold time.Duration) (time.Duration, error) {
	margin := c.period() / 16
	index := int64(math.Round(float64(threshold) * float64(c.rateNumerator) / (float64(c.rateDenominator) * float64(time.Second))))
	if index < 0 || margin <= 0 {
		return threshold, nil
	}
	nearest, err := c.outputTimestamp(index)
	if err != nil {
		return 0, err
	}
	distance := threshold - nearest
	side := int8(-1)
	if distance > 0 {
		side = 1
	}
	if distance <= -margin || distance >= margin {
		if distance > -2*margin && distance < 2*margin {
			c.side = side
		}
		return threshold, nil
	}
	if c.side == 0 {
		c.side = side
	}
	if c.side > 0 {
		// Just past the timestamp: the pending frame keeps that slot.
		return nearest + 1, nil
	}
	return nearest, nil
}

func (c *DVDFrameRateConverter) outputTimestamp(index int64) (time.Duration, error) {
	if index < 0 {
		return 0, errors.New("video2dvd: negative output frame index")
	}
	unit := c.rateDenominator * int64(time.Second)
	whole := index / c.rateNumerator
	remainder := index % c.rateNumerator
	if whole > math.MaxInt64/unit {
		return 0, errors.New("video2dvd: output timestamp overflows duration")
	}
	wholeNanoseconds := whole * unit
	fractionNanoseconds := remainder * unit / c.rateNumerator
	if fractionNanoseconds > math.MaxInt64-wholeNanoseconds {
		return 0, errors.New("video2dvd: output timestamp overflows duration")
	}
	nanoseconds := wholeNanoseconds + fractionNanoseconds
	return time.Duration(nanoseconds), nil
}

func validateDecodedFrame(frame DecodedVideoFrame) error {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Width%2 != 0 || frame.Height%2 != 0 || frame.Width > int(^uint(0)>>1)/frame.Height {
		return errors.New("video2dvd: decoded frame dimensions must be positive even values")
	}
	pixels := frame.Width * frame.Height
	if len(frame.Y) < pixels || len(frame.U) < pixels/4 || len(frame.V) < pixels/4 {
		return errors.New("video2dvd: decoded frame has incomplete YUV 4:2:0 planes")
	}
	return nil
}

func cloneDecodedVideoFrame(frame DecodedVideoFrame) DecodedVideoFrame {
	return DecodedVideoFrame{
		PTS:      frame.PTS,
		Duration: frame.Duration,
		Width:    frame.Width,
		Height:   frame.Height,
		Color:    frame.Color,
		Y:        append([]byte(nil), frame.Y...),
		U:        append([]byte(nil), frame.U...),
		V:        append([]byte(nil), frame.V...),
	}
}
