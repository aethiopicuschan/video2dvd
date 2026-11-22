package video2dvd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/thesyncim/goav/av"
	"github.com/thesyncim/goav/container/ebml"
	"github.com/thesyncim/goav/container/matroska"
	"github.com/thesyncim/goav/container/webm"
	"github.com/thesyncim/goav/shape"
)

// openWebMDemuxer reads the header of a WebM file. It uses the Matroska
// demuxer, which WebM is a profile of, because webm.NewDemuxer refuses a file
// as soon as one of its tracks has a codec outside the WebM profile's audio
// and video codecs, and so every file with a subtitle track. The demuxer
// returns the packets of all tracks; the callers pick theirs by track number.
func openWebMDemuxer(r io.Reader) (*matroska.Demuxer, error) {
	demuxer, err := matroska.NewDemuxer(r, matroska.DemuxerOptions{MaxLacePayload: maxWebMPacketBytes})
	if err != nil {
		return nil, err
	}
	if demuxer.DocType() != "webm" {
		return nil, webm.ErrUnsupportedWebMDocType
	}
	return demuxer, nil
}

// Matroska element IDs read by webMTrackCodecIDs.
const (
	matroskaIDEBML        ebml.ID = 0x1a45dfa3
	matroskaIDSegment     ebml.ID = 0x18538067
	matroskaIDTracks      ebml.ID = 0x1654ae6b
	matroskaIDCluster     ebml.ID = 0x1f43b675
	matroskaIDTrackEntry  ebml.ID = 0xae
	matroskaIDTrackNumber ebml.ID = 0xd7
	matroskaIDCodecID     ebml.ID = 0x86
	// matroskaMaxTracksSize bounds the Tracks element that is read whole.
	matroskaMaxTracksSize = 16 << 20
)

// webMTrackCodecIDs returns the CodecID of every track of a Matroska file by
// track number. The goav demuxer maps codec IDs it does not know, which
// includes every subtitle codec but S_TEXT/UTF8, to one unknown codec, and
// the codec ID is what tells the subtitle formats apart.
func webMTrackCodecIDs(r io.Reader) (map[uint64]string, error) {
	reader := ebml.NewReader(r, ebml.ReaderOptions{})
	header, err := reader.ReadHeader()
	if err != nil {
		return nil, err
	}
	if header.ID != matroskaIDEBML || header.Size.Unknown {
		return nil, errors.New("video2dvd: WebM file does not start with an EBML header")
	}
	if err := reader.Skip(header.Size.Value); err != nil {
		return nil, err
	}
	if header, err = reader.ReadHeader(); err != nil {
		return nil, err
	}
	if header.ID != matroskaIDSegment {
		return nil, errors.New("video2dvd: WebM file has no segment")
	}
	// The segment may be of unknown size, so its children are read until
	// the Tracks element, which comes before the first cluster.
	for {
		child, err := reader.ReadHeader()
		if err != nil {
			return nil, err
		}
		if child.ID == matroskaIDCluster || child.Size.Unknown {
			return map[uint64]string{}, nil
		}
		if child.ID != matroskaIDTracks {
			if err := reader.Skip(child.Size.Value); err != nil {
				return nil, err
			}
			continue
		}
		if child.Size.Value > matroskaMaxTracksSize {
			return nil, errors.New("video2dvd: WebM track list is too large")
		}
		data := make([]byte, child.Size.Value)
		if err := reader.ReadFull(data); err != nil {
			return nil, err
		}
		return parseMatroskaTrackCodecIDs(data)
	}
}

// parseMatroskaTrackCodecIDs reads the TrackNumber and CodecID of the
// TrackEntry elements of a Tracks element body.
func parseMatroskaTrackCodecIDs(data []byte) (map[uint64]string, error) {
	codecs := map[uint64]string{}
	err := forEachEBMLChild(data, func(id ebml.ID, entry []byte) error {
		if id != matroskaIDTrackEntry {
			return nil
		}
		var number uint64
		var codec string
		err := forEachEBMLChild(entry, func(id ebml.ID, value []byte) error {
			switch id {
			case matroskaIDTrackNumber:
				if len(value) == 0 || len(value) > 8 {
					return errors.New("video2dvd: invalid WebM track number")
				}
				for _, b := range value {
					number = number<<8 | uint64(b)
				}
			case matroskaIDCodecID:
				// A string element may be padded with zero bytes.
				for len(value) > 0 && value[len(value)-1] == 0 {
					value = value[:len(value)-1]
				}
				codec = string(value)
			}
			return nil
		})
		if err != nil {
			return err
		}
		codecs[number] = codec
		return nil
	})
	return codecs, err
}

// forEachEBMLChild calls visit with the ID and body of every element of an
// EBML master element body.
func forEachEBMLChild(data []byte, visit func(ebml.ID, []byte) error) error {
	for len(data) > 0 {
		id, idWidth, err := ebml.DecodeIDVINT(data)
		if err != nil {
			return err
		}
		size, err := ebml.DecodeSizeVINT(data[idWidth:])
		if err != nil {
			return err
		}
		start := idWidth + size.Width
		if size.Unknown || size.Value > uint64(len(data)-start) {
			return errors.New("video2dvd: invalid WebM element size")
		}
		end := start + int(size.Value)
		if err := visit(id, data[start:end]); err != nil {
			return err
		}
		data = data[end:]
	}
	return nil
}

// webMMatroskaInput is a WebM file for the goav pipeline that is read with
// the Matroska demuxer, for the reason given at openWebMDemuxer: goav picks
// its WebM demuxer for a WebM file, which refuses subtitle tracks.
type webMMatroskaInput struct {
	path string
}

func (i webMMatroskaInput) SourceShape() shape.Spec {
	return shape.Spec{Format: av.FormatMatroska}
}

func (i webMMatroskaInput) OpenBytes(context.Context) (io.ReadCloser, error) {
	f, err := os.Open(i.path)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: open WebM: %w", err)
	}
	return f, nil
}

func (i webMMatroskaInput) Name() string { return i.path }

// webMHasOnlyMediaTracks reports whether every track of a probed WebM file
// is a video or an audio track, which the goav WebM demuxer reads.
func webMHasOnlyMediaTracks(info MP4Info) bool {
	for _, track := range info.Tracks {
		if track.Kind != TrackKindVideo && track.Kind != TrackKindAudio {
			return false
		}
	}
	return true
}
