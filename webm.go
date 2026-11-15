package video2dvd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	vp9decoder "github.com/mgvs/go-vp9/decoder"
	vp9frame "github.com/mgvs/go-vp9/frame"
	"github.com/mgvs/go-vp9/refbuf"
	"github.com/thesyncim/goav"
	"github.com/thesyncim/goav/av"
	"github.com/thesyncim/goav/bundle"
	"github.com/thesyncim/goav/component"
	"github.com/thesyncim/goav/container/webm"
)

const (
	maxWebMPacketBytes = 64 << 20
	maxWebMProbeBytes  = 4096
)

// ProbeWebM reads metadata from a WebM file without decoding its streams.
func ProbeWebM(path string) (MP4Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: open WebM: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: stat WebM: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return MP4Info{}, errors.New("video2dvd: WebM input is not a regular file")
	}
	return probeWebMReader(f)
}

// ProbeMedia selects the built-in MP4 or WebM metadata reader from the file
// signature and extension. WebM VP9, AV1, and Opus inputs can then use the
// corresponding DVD preparation functions.
func ProbeMedia(path string) (MP4Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: open input: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: stat input: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return MP4Info{}, errors.New("video2dvd: input is not a regular file")
	}
	header := make([]byte, maxWebMProbeBytes)
	n, readErr := io.ReadFull(f, header)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return MP4Info{}, fmt.Errorf("video2dvd: read input header: %w", readErr)
	}
	header = header[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: rewind input: %w", err)
	}
	if isWebMFile(header, path) {
		return probeWebMReader(f)
	}
	return ProbeMP4Reader(f, stat.Size())
}

func isWebMFile(header []byte, path string) bool {
	if strings.EqualFold(filepath.Ext(path), ".webm") {
		return true
	}
	return len(header) >= 4 && bytes.Equal(header[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) && bytes.Contains(header, []byte("webm"))
}

func probeWebMReader(r io.Reader) (MP4Info, error) {
	demuxer, err := webm.NewDemuxer(r, webm.DemuxerOptions{MaxLacePayload: maxWebMPacketBytes})
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: parse WebM: %w", err)
	}
	segmentInfo := demuxer.Info()
	info := MP4Info{}
	if segmentInfo.DurationSet && segmentInfo.DurationNS > 0 {
		info.Duration = time.Duration(segmentInfo.DurationNS)
	}
	for index, source := range demuxer.Tracks() {
		track := TrackInfo{
			Index:       index,
			CodecConfig: append([]byte(nil), source.CodecPrivate...),
			Duration:    info.Duration,
			TimeScale:   1_000_000_000,
		}
		switch source.Type {
		case webm.TrackVideo:
			track.Kind = TrackKindVideo
			track.Width = source.Video.Width
			track.Height = source.Video.Height
			switch source.Codec {
			case webm.CodecVP9:
				track.Codec = "vp9"
			case webm.CodecVP8:
				track.Codec = "vp8"
			case webm.CodecAV1:
				track.Codec = "av1"
			default:
				track.Codec = source.CodecName
			}
			if source.Video.DisplayWidth > 0 && source.Video.DisplayHeight > 0 && track.Width > 0 && track.Height > 0 {
				parN := uint64(source.Video.DisplayWidth) * uint64(track.Height)
				parD := uint64(source.Video.DisplayHeight) * uint64(track.Width)
				divisor := gcdUint64(parN, parD)
				if divisor > 0 && parN/divisor <= uint64(^uint32(0)) && parD/divisor <= uint64(^uint32(0)) {
					track.PixelAspectHorizontal = uint32(parN / divisor)
					track.PixelAspectVertical = uint32(parD / divisor)
				}
			}
		case webm.TrackAudio:
			track.Kind = TrackKindAudio
			track.SampleRate = source.Audio.SampleRate
			track.Channels = source.Audio.Channels
			switch source.Codec {
			case webm.CodecOpus:
				track.Codec = "opus"
			case webm.CodecVorbis:
				track.Codec = "vorbis"
			default:
				track.Codec = source.CodecName
			}
		default:
			track.Kind = TrackKindOther
			track.Codec = source.CodecName
		}
		info.Tracks = append(info.Tracks, track)
	}
	return info, nil
}

// PrepareWebMVideoForDVD decodes WebM VP9 (including 10/12-bit VP9 Profile 2),
// VP8, or AV1 video, converts frames to 8-bit YUV 4:2:0, and prepares the DVD
// raster and frame rate. High-bit-depth VP9 is reduced to 8-bit for DVD.
func PrepareWebMVideoForDVD(
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
	err = DecodeWebMVideo(ctx, path, trackIndex, func(frame DecodedVideoFrame) error {
		fitted, err := FitVideoFrame(frame, profile, sourceAspectNumerator, sourceAspectDenominator, mode)
		if err != nil {
			return err
		}
		return rateConverter.Push(fitted)
	})
	if err != nil {
		return err
	}
	return rateConverter.Finish()
}

// PrepareWebMAudioForDVD decodes an Opus track to 48 kHz stereo and writes DVD
// LPCM audio essence. AV1/Opus decoding and WebM demuxing use pure-Go packages.
func PrepareWebMAudioForDVD(ctx context.Context, path string, trackIndex *int, dst io.Writer) error {
	if dst == nil {
		return errors.New("video2dvd: DVD LPCM writer is required")
	}
	return DecodeWebMAudio(ctx, path, trackIndex, func(chunk PCMChunk) error {
		return WriteDVDLPCM(dst, chunk)
	})
}

// DecodeWebMVideo sends owned 8-bit YUV 4:2:0 frames from a WebM video track
// to handler. VP9 Profile 0 and Profile 2 use a pure-Go VP9 decoder; VP8/AV1
// use the bundled pure-Go media pipeline.
func DecodeWebMVideo(ctx context.Context, path string, trackIndex *int, handler func(DecodedVideoFrame) error) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if handler == nil {
		return errors.New("video2dvd: video frame handler is required")
	}
	info, err := ProbeWebM(path)
	if err != nil {
		return err
	}
	track, err := selectWebMVideoTrack(info, trackIndex)
	if err != nil {
		return err
	}
	if track.Codec == "vp9" {
		return decodeWebMVP9(ctx, path, track, handler)
	}
	if track.Codec == "vp8" || track.Codec == "av1" {
		return decodeWebMWithGoAV(ctx, path, track.Index, true, func(frame *av.Frame) error {
			decoded, err := copyGoAVVideoFrame(frame)
			if err != nil {
				return err
			}
			return handler(decoded)
		})
	}
	return fmt.Errorf("video2dvd: WebM video codec %q is not supported by the built-in decoder", track.Codec)
}

// DecodeWebMAudio sends 48 kHz stereo Opus PCM chunks from a WebM file. The
// selected stream index is zero-based in the container track list.
func DecodeWebMAudio(ctx context.Context, path string, trackIndex *int, handler func(PCMChunk) error) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if handler == nil {
		return errors.New("video2dvd: PCM handler is required")
	}
	info, err := ProbeWebM(path)
	if err != nil {
		return err
	}
	track, err := selectWebMAudioTrack(info, trackIndex)
	if err != nil {
		return err
	}
	if track.Codec != "opus" {
		return fmt.Errorf("video2dvd: WebM audio codec %q is not supported by the built-in decoder", track.Codec)
	}
	return decodeWebMWithGoAV(ctx, path, track.Index, false, func(frame *av.Frame) error {
		chunk, err := copyGoAVPCMFrame(frame)
		if err != nil {
			return err
		}
		return handler(chunk)
	})
}

func decodeWebMVP9(ctx context.Context, path string, track TrackInfo, handler func(DecodedVideoFrame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("video2dvd: open WebM: %w", err)
	}
	defer f.Close()
	demuxer, err := webm.NewDemuxer(f, webm.DemuxerOptions{MaxLacePayload: maxWebMPacketBytes})
	if err != nil {
		return fmt.Errorf("video2dvd: parse WebM: %w", err)
	}
	tracks := demuxer.Tracks()
	if track.Index < 0 || track.Index >= len(tracks) || tracks[track.Index].Type != webm.TrackVideo || tracks[track.Index].Codec != webm.CodecVP9 {
		return fmt.Errorf("video2dvd: WebM track %d is not VP9 video", track.Index)
	}
	trackID := tracks[track.Index].ID
	decoder := vp9decoder.New()
	packet := webm.Packet{Data: make([]byte, 0, maxWebMPacketBytes)}
	previousWidth, previousHeight := 0, 0
	decodedFrames := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := demuxer.ReadPacket(&packet); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("video2dvd: read WebM packet: %w", err)
		}
		if packet.TrackID != trackID {
			continue
		}
		if len(packet.Data) == 0 || len(packet.Data) > maxWebMPacketBytes {
			return errors.New("video2dvd: WebM VP9 packet is empty or exceeds the packet limit")
		}
		header, err := vp9frame.Parse(packet.Data, previousWidth, previousHeight)
		if err != nil {
			return fmt.Errorf("video2dvd: parse VP9 frame header: %w", err)
		}
		var decoded DecodedVideoFrame
		if header.Profile == 2 {
			frame, err := decoder.DecodeFrame16(packet.Data)
			if err != nil {
				return fmt.Errorf("video2dvd: decode VP9 Profile 2: %w", err)
			}
			decoded, err = copyVP9HighBitDepthFrame(frame, frame.BitDepth, time.Duration(packet.TimeNS), time.Duration(packet.DurationNS))
			if err != nil {
				return err
			}
		} else if header.Profile == 0 {
			frame, err := decoder.DecodeFrame(packet.Data)
			if err != nil {
				return fmt.Errorf("video2dvd: decode VP9 Profile 0: %w", err)
			}
			decoded, err = copyVP9Frame(frame, time.Duration(packet.TimeNS), time.Duration(packet.DurationNS))
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("video2dvd: VP9 profile %d is unsupported; supported profiles are 0 and 2 4:2:0", header.Profile)
		}
		previousWidth, previousHeight = decoded.Width, decoded.Height
		if packet.Invisible || !header.ShowFrame && !header.ShowExistingFrame && !hasVP9Superframe(packet.Data) {
			continue
		}
		if err := handler(decoded); err != nil {
			return err
		}
		decodedFrames++
	}
	if decodedFrames == 0 {
		return errors.New("video2dvd: WebM VP9 track contains no visible frames")
	}
	return ctx.Err()
}

func copyVP9Frame(src *refbuf.Frame, pts, duration time.Duration) (DecodedVideoFrame, error) {
	if src == nil || src.Width <= 0 || src.Height <= 0 || src.Width%2 != 0 || src.Height%2 != 0 || src.YStride < src.Width || src.UVStride < src.Width/2 {
		return DecodedVideoFrame{}, errors.New("video2dvd: VP9 decoder returned invalid I420 dimensions")
	}
	decoded := DecodedVideoFrame{PTS: pts, Duration: duration, Width: src.Width, Height: src.Height, Y: make([]byte, src.Width*src.Height), U: make([]byte, src.Width*src.Height/4), V: make([]byte, src.Width*src.Height/4)}
	if err := copyPlaneFromOffset(decoded.Y, src.Y, src.YBase(), src.Width, src.Height, src.YStride); err != nil {
		return DecodedVideoFrame{}, err
	}
	if err := copyPlaneFromOffset(decoded.U, src.U, src.UBase(), src.Width/2, src.Height/2, src.UVStride); err != nil {
		return DecodedVideoFrame{}, err
	}
	if err := copyPlaneFromOffset(decoded.V, src.V, src.VBase(), src.Width/2, src.Height/2, src.UVStride); err != nil {
		return DecodedVideoFrame{}, err
	}
	return decoded, nil
}

func copyVP9HighBitDepthFrame(src *refbuf.Frame16, bitDepth int, pts, duration time.Duration) (DecodedVideoFrame, error) {
	if src == nil || bitDepth < 9 || bitDepth > 12 || src.Width <= 0 || src.Height <= 0 || src.Width%2 != 0 || src.Height%2 != 0 || src.YStride < src.Width || src.UVStride < src.Width/2 {
		return DecodedVideoFrame{}, errors.New("video2dvd: VP9 decoder returned invalid high-bit-depth I420 dimensions")
	}
	decoded := DecodedVideoFrame{PTS: pts, Duration: duration, Width: src.Width, Height: src.Height, Y: make([]byte, src.Width*src.Height), U: make([]byte, src.Width*src.Height/4), V: make([]byte, src.Width*src.Height/4)}
	maxSample := (1 << bitDepth) - 1
	convert := func(dst []byte, plane []uint16, offset, width, height, stride int) error {
		if offset < 0 || stride < width || len(plane) < offset+(height-1)*stride+width {
			return errors.New("video2dvd: VP9 decoder returned a truncated high-bit-depth plane")
		}
		for row := range height {
			for col := range width {
				sample := int(plane[offset+row*stride+col])
				dst[row*width+col] = byte((sample*255 + maxSample/2) / maxSample)
			}
		}
		return nil
	}
	if err := convert(decoded.Y, src.Y, src.YBase(), src.Width, src.Height, src.YStride); err != nil {
		return DecodedVideoFrame{}, err
	}
	if err := convert(decoded.U, src.U, src.UBase(), src.Width/2, src.Height/2, src.UVStride); err != nil {
		return DecodedVideoFrame{}, err
	}
	if err := convert(decoded.V, src.V, src.VBase(), src.Width/2, src.Height/2, src.UVStride); err != nil {
		return DecodedVideoFrame{}, err
	}
	return decoded, nil
}

func copyPlaneFromOffset(dst, src []byte, offset, width, height, stride int) error {
	if offset < 0 || width <= 0 || height <= 0 || stride < width || len(dst) != width*height || len(src) < offset+(height-1)*stride+width {
		return errors.New("video2dvd: VP9 decoder returned a truncated plane")
	}
	for row := range height {
		copy(dst[row*width:(row+1)*width], src[offset+row*stride:offset+row*stride+width])
	}
	return nil
}

func hasVP9Superframe(packet []byte) bool {
	if len(packet) < 2 {
		return false
	}
	marker := packet[len(packet)-1]
	if marker&0xe0 != 0xc0 {
		return false
	}
	frames := int(marker&0x7) + 1
	mag := int((marker>>3)&0x3) + 1
	indexSize := 2 + frames*mag
	return indexSize <= len(packet) && packet[len(packet)-indexSize] == marker
}

func decodeWebMWithGoAV(ctx context.Context, path string, streamIndex int, video bool, handler func(*av.Frame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("video2dvd: open WebM: %w", err)
	}
	defer f.Close()
	in := goav.FileInput(path, f)
	var stream goav.Stream
	selection := goav.StreamIndex(streamIndex)
	if video {
		stream = in.Video(selection).Decode()
	} else {
		stream = in.Audio(selection).Decode().Resample(48000, 2)
	}
	var frames int
	out := stream.To(goav.Sink(component.SinkFunc("video2dvd-webm", func(ctx context.Context, message component.Message) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if message.Kind != "frame" || message.Frame == nil {
			return nil
		}
		if err := handler(message.Frame); err != nil {
			return err
		}
		frames++
		return nil
	})))
	if err := bundle.Run(ctx, out); err != nil {
		return fmt.Errorf("video2dvd: decode WebM stream: %w", err)
	}
	if frames == 0 {
		return errors.New("video2dvd: WebM stream contains no decoded frames")
	}
	return ctx.Err()
}

func copyGoAVVideoFrame(frame *av.Frame) (DecodedVideoFrame, error) {
	if frame == nil || frame.Video == nil || frame.Video.Width <= 0 || frame.Video.Height <= 0 || frame.Video.PixelFormat != av.PixelFormatI420 {
		return DecodedVideoFrame{}, errors.New("video2dvd: WebM decoder did not return an 8-bit I420 frame")
	}
	if frame.Video.Width%2 != 0 || frame.Video.Height%2 != 0 || len(frame.Planes) < 3 {
		return DecodedVideoFrame{}, errors.New("video2dvd: WebM decoder returned invalid 4:2:0 plane dimensions")
	}
	pts, _ := frame.PTS.ToDuration()
	duration, _ := frame.Duration.ToDuration()
	decoded := DecodedVideoFrame{PTS: pts, Duration: duration, Width: frame.Video.Width, Height: frame.Video.Height, Y: make([]byte, frame.Video.Width*frame.Video.Height), U: make([]byte, frame.Video.Width*frame.Video.Height/4), V: make([]byte, frame.Video.Width*frame.Video.Height/4)}
	for i, dst := range [][]byte{decoded.Y, decoded.U, decoded.V} {
		width, height := decoded.Width, decoded.Height
		if i > 0 {
			width, height = width/2, height/2
		}
		plane := frame.Planes[i]
		if plane.Stride < width || plane.Offset < 0 || plane.Offset+(height-1)*plane.Stride+width > len(plane.Buffer.Bytes) {
			return DecodedVideoFrame{}, errors.New("video2dvd: WebM decoder returned a truncated video plane")
		}
		for row := 0; row < height; row++ {
			start := plane.Offset + row*plane.Stride
			copy(dst[row*width:(row+1)*width], plane.Buffer.Bytes[start:start+width])
		}
	}
	return decoded, nil
}

func copyGoAVPCMFrame(frame *av.Frame) (PCMChunk, error) {
	if frame == nil || frame.Audio == nil || frame.Audio.SampleRate != 48000 || frame.Audio.Channels != 2 || frame.Audio.SampleFormat != av.SampleFormatS16 || len(frame.Planes) == 0 {
		return PCMChunk{}, errors.New("video2dvd: WebM decoder did not return 48 kHz stereo S16 PCM")
	}
	data := frame.Planes[0].Buffer.Bytes
	wantBytes := frame.Audio.Samples * frame.Audio.Channels * 2
	if frame.Audio.Samples < 0 || wantBytes < 0 || len(data) < wantBytes || wantBytes%2 != 0 {
		return PCMChunk{}, errors.New("video2dvd: WebM decoder returned a truncated PCM sample")
	}
	data = data[:wantBytes]
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.NativeEndian.Uint16(data[i*2 : i*2+2]))
	}
	start, _ := frame.PTS.ToDuration()
	return PCMChunk{Start: start, SampleRate: 48000, Channels: 2, Samples: samples}, nil
}

func selectWebMVideoTrack(info MP4Info, requested *int) (TrackInfo, error) {
	return selectWebMTrack(info, requested, TrackKindVideo)
}

func selectWebMAudioTrack(info MP4Info, requested *int) (TrackInfo, error) {
	return selectWebMTrack(info, requested, TrackKindAudio)
}

func selectWebMTrack(info MP4Info, requested *int, kind TrackKind) (TrackInfo, error) {
	if requested != nil {
		for _, track := range info.Tracks {
			if track.Index == *requested {
				if track.Kind != kind {
					return TrackInfo{}, fmt.Errorf("video2dvd: WebM track %d is not %s", *requested, kind)
				}
				return track, nil
			}
		}
		return TrackInfo{}, fmt.Errorf("video2dvd: WebM %s track index %d is out of range", kind, *requested)
	}
	for _, track := range info.Tracks {
		if track.Kind == kind && (kind == TrackKindVideo && (track.Codec == "vp9" || track.Codec == "vp8" || track.Codec == "av1") || kind == TrackKindAudio && track.Codec == "opus") {
			return track, nil
		}
	}
	return TrackInfo{}, fmt.Errorf("video2dvd: WebM has no supported %s track", kind)
}
