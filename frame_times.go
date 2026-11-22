package video2dvd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	vp9frame "github.com/aethiopicuschan/go-vp9/frame"
	"github.com/thesyncim/goav/container/webm"
)

// mp4VideoFrameTimes returns the presentation times of the frames of an MP4
// video track in presentation order, read from its sample tables without
// touching the samples. Like the conversion, it leaves out frames that end
// before the track's first media edit starts.
func mp4VideoFrameTimes(path string, trackIndex *int) ([]time.Duration, error) {
	info, err := ProbeMP4(path)
	if err != nil {
		return nil, err
	}
	track, err := selectVideoTrack(info, trackIndex)
	if err != nil {
		return nil, err
	}
	var times []time.Duration
	_, err = DemuxMP4(path, func(sampleTrack TrackInfo, sample SampleInfo, _ io.Reader) error {
		if sampleTrack.Index != track.Index {
			return nil
		}
		start, err := ticksToDuration(sample.PTS, track.TimeScale)
		if err != nil {
			return err
		}
		duration, err := ticksToDuration(int64(sample.Duration), track.TimeScale)
		if err != nil {
			return err
		}
		if track.edit.present && start <= track.edit.mediaTime-duration {
			return nil
		}
		times = append(times, start)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(times)
	return times, nil
}

// webMVideoFrameTimes returns the container timestamps of the frames a WebM
// video track shows, in file order, read without decoding the video.
func webMVideoFrameTimes(path string, trackIndex *int) ([]time.Duration, error) {
	info, err := ProbeWebM(path)
	if err != nil {
		return nil, err
	}
	track, err := selectWebMVideoTrack(info, trackIndex)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: open WebM: %w", err)
	}
	defer f.Close()
	demuxer, err := openWebMDemuxer(f)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: parse WebM: %w", err)
	}
	tracks := demuxer.Tracks()
	if track.Index < 0 || track.Index >= len(tracks) || tracks[track.Index].Type != webm.TrackVideo {
		return nil, fmt.Errorf("video2dvd: WebM track %d is not video", track.Index)
	}
	trackID := tracks[track.Index].ID
	var times []time.Duration
	packet := webm.Packet{Data: make([]byte, 0, maxWebMPacketBytes)}
	previousWidth, previousHeight := 0, 0
	for {
		if err := demuxer.ReadPacket(&packet); err != nil {
			if errors.Is(err, io.EOF) {
				return times, nil
			}
			return nil, fmt.Errorf("video2dvd: read WebM packet: %w", err)
		}
		if packet.TrackID != trackID || len(packet.Data) == 0 {
			continue
		}
		// Frames that only update references are not shown and so have no
		// place in the cadence: the decoders skip them as well.
		shown := !packet.Invisible
		switch track.Codec {
		case "vp9":
			header, err := vp9frame.Parse(packet.Data, previousWidth, previousHeight)
			if err != nil {
				return nil, fmt.Errorf("video2dvd: parse VP9 frame header: %w", err)
			}
			if header.FrameWidth > 0 && header.FrameHeight > 0 {
				previousWidth, previousHeight = header.FrameWidth, header.FrameHeight
			}
			shown = shown && (header.ShowFrame || header.ShowExistingFrame || hasVP9Superframe(packet.Data))
		case "vp8":
			// show_frame of the frame tag.
			shown = shown && packet.Data[0]&0x10 != 0
		}
		if shown {
			times = append(times, time.Duration(packet.TimeNS))
		}
	}
}
