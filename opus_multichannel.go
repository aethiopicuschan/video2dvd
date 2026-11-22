package video2dvd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/thesyncim/goav/container/matroska"
	"github.com/thesyncim/goav/container/webm"
	"github.com/thesyncim/gopus"
)

// opusMaxFrameSamples is the longest Opus packet, 120 ms, in 48 kHz samples
// per channel; one decode never returns more.
const opusMaxFrameSamples = 48000 * 120 / 1000

// opusHead is the part of an RFC 7845 identification header (the WebM
// CodecPrivate of an Opus track) that multistream decoding needs.
type opusHead struct {
	channels       int
	preSkip        int
	mappingFamily  byte
	streams        int
	coupledStreams int
	mapping        []byte
}

// parseOpusHead reads an OpusHead packet. Mapping family 0 has no channel
// mapping table; RFC 7845 defines it as one stream, coupled when there are two
// channels.
func parseOpusHead(data []byte) (opusHead, error) {
	if len(data) < 19 || string(data[:8]) != "OpusHead" {
		return opusHead{}, errors.New("video2dvd: Opus track has no valid OpusHead codec private data")
	}
	if data[8]>>4 != 0 {
		return opusHead{}, fmt.Errorf("video2dvd: OpusHead version %d is not supported", data[8])
	}
	head := opusHead{
		channels:      int(data[9]),
		preSkip:       int(binary.LittleEndian.Uint16(data[10:12])),
		mappingFamily: data[18],
	}
	if head.channels == 0 {
		return opusHead{}, errors.New("video2dvd: OpusHead declares zero channels")
	}
	if head.mappingFamily == 0 {
		if head.channels > 2 {
			return opusHead{}, fmt.Errorf("video2dvd: OpusHead mapping family 0 cannot carry %d channels", head.channels)
		}
		head.streams = 1
		head.coupledStreams = head.channels - 1
		head.mapping = []byte{0, 1}[:head.channels]
		return head, nil
	}
	if len(data) < 21+head.channels {
		return opusHead{}, errors.New("video2dvd: OpusHead channel mapping table is truncated")
	}
	head.streams = int(data[19])
	head.coupledStreams = int(data[20])
	head.mapping = append([]byte(nil), data[21:21+head.channels]...)
	if head.streams == 0 || head.coupledStreams > head.streams {
		return opusHead{}, fmt.Errorf("video2dvd: OpusHead declares %d streams with %d coupled", head.streams, head.coupledStreams)
	}
	for _, index := range head.mapping {
		if index != 255 && int(index) >= head.streams+head.coupledStreams {
			return opusHead{}, fmt.Errorf("video2dvd: OpusHead maps a channel to missing decoded channel %d", index)
		}
	}
	return head, nil
}

// opusTrackChannels is the channel count the decoder will produce for an
// Opus track. The OpusHead is authoritative because it configures the
// decoder; the Matroska Channels element is only used when the header cannot
// be read, and decoding then reports what is wrong with it.
func opusTrackChannels(track TrackInfo) int {
	if head, err := parseOpusHead(track.CodecConfig); err == nil {
		return head.channels
	}
	return track.Channels
}

// opusDownmixLayout returns the speaker order of an Opus header that the
// stereo downmix accepts: mapping family 1 (Vorbis order) with three to eight
// channels. Family 2 and 3 carry ambisonics and family 255 has no defined
// speakers, so none of them has a downmix.
func opusDownmixLayout(head opusHead) ([]speakerPosition, error) {
	if head.mappingFamily != 1 {
		return nil, fmt.Errorf("video2dvd: %d-channel Opus audio with channel mapping family %d cannot be downmixed (only family 1, Vorbis channel order, is supported)", head.channels, head.mappingFamily)
	}
	layout, ok := vorbisChannelOrder[head.channels]
	if !ok {
		return nil, fmt.Errorf("video2dvd: %d-channel Opus audio cannot be downmixed (channel mapping family 1 defines up to 8 channels)", head.channels)
	}
	return layout, nil
}

// decodeWebMOpusMultichannel decodes an Opus track with more than two
// channels to 48 kHz stereo. The goav Opus stage only opens mono and stereo
// decoders, so this path reads the track's packets itself and decodes them
// with the pure-Go gopus multistream decoder, then folds the channels with
// newStereoDownmix. Timing follows the goav stage used for stereo Opus: a
// packet's time is its block time minus the track's CodecDelay, the header's
// pre-skip is dropped from the start of the stream, and each chunk starts at
// its first kept sample. A block's DiscardPadding is trimmed from its end.
// The header's output gain is not applied, as in the stereo path.
func decodeWebMOpusMultichannel(ctx context.Context, path string, track TrackInfo, handler func(PCMChunk) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
				return
			}
			err = fmt.Errorf("video2dvd: decode WebM Opus audio %q panicked: %v", path, recovered)
		}
	}()
	head, err := parseOpusHead(track.CodecConfig)
	if err != nil {
		return err
	}
	layout, err := opusDownmixLayout(head)
	if err != nil {
		return err
	}
	mix, err := newStereoDownmix(layout)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("video2dvd: open WebM: %w", err)
	}
	defer f.Close()
	demuxer, err := openWebMDemuxer(f)
	if err != nil {
		return fmt.Errorf("video2dvd: parse WebM: %w", err)
	}
	tracks := demuxer.Tracks()
	if track.Index < 0 || track.Index >= len(tracks) || tracks[track.Index].Type != webm.TrackAudio || tracks[track.Index].Codec != webm.CodecOpus {
		return fmt.Errorf("video2dvd: WebM track %d is not Opus audio", track.Index)
	}
	trackID := tracks[track.Index].ID
	codecDelayNS := tracks[track.Index].CodecDelayNS
	decoder, err := gopus.NewMultistreamDecoder(48000, head.channels, head.streams, head.coupledStreams, head.mapping)
	if err != nil {
		return fmt.Errorf("video2dvd: open Opus multistream decoder: %w", err)
	}
	decoded := make([]float32, opusMaxFrameSamples*head.channels)
	packet := matroska.Packet{Data: make([]byte, 0, maxWebMPacketBytes)}
	skip := head.preSkip
	chunks := 0
	// The newest decoded chunk is held back until the next packet arrives:
	// DiscardPadding trims the end of the track only, and goav gives every
	// frame of a laced block the block's padding, so only the chunk that
	// turns out to be the last is trimmed.
	var held *PCMChunk
	heldPadding := 0
	emit := func(trim int) error {
		if held == nil {
			return nil
		}
		chunk := *held
		held = nil
		chunk.Samples = chunk.Samples[:len(chunk.Samples)-2*min(trim, len(chunk.Samples)/2)]
		if len(chunk.Samples) == 0 {
			return nil
		}
		chunks++
		return handler(chunk)
	}
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
		if len(packet.Data) == 0 {
			return errors.New("video2dvd: WebM Opus packet is empty")
		}
		frames, err := decoder.Decode(packet.Data, decoded)
		if err != nil {
			return fmt.Errorf("video2dvd: decode Opus multistream packet: %w", err)
		}
		drop := min(skip, frames)
		skip -= drop
		kept := frames - drop
		if kept == 0 {
			continue
		}
		samples := make([]int16, kept*2)
		if err := mix.apply(samples, decoded[drop*head.channels:(drop+kept)*head.channels]); err != nil {
			return err
		}
		if err := emit(0); err != nil {
			return err
		}
		start := time.Duration(packet.TimeNS-codecDelayNS) + time.Duration(drop)*time.Second/48000
		held = &PCMChunk{Start: start, SampleRate: 48000, Channels: 2, Samples: samples}
		heldPadding = 0
		if packet.DiscardPaddingNS > 0 {
			heldPadding = int((packet.DiscardPaddingNS*48000 + 500_000_000) / 1_000_000_000)
		}
	}
	if err := emit(heldPadding); err != nil {
		return err
	}
	if chunks == 0 {
		return errors.New("video2dvd: WebM stream contains no decoded frames")
	}
	return ctx.Err()
}
