package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

var ErrInvalidMP4 = errors.New("video2dvd: invalid or unsupported MP4 container")

// TrackKind identifies the media carried by an MP4 track.
type TrackKind string

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
	TrackKindOther TrackKind = "other"
)

// TrackInfo contains container-level metadata. Codec is the MP4 sample-entry
// code or the WebM codec name. CodecConfig contains codec-specific setup data.
// PixelAspectHorizontal and PixelAspectVertical describe the displayed pixel
// shape; zero means square pixels or no pixel aspect metadata was present.
type TrackInfo struct {
	Index                 int
	Kind                  TrackKind
	Codec                 string
	CodecConfig           []byte
	NALLengthSize         int
	Width                 int
	Height                int
	PixelAspectHorizontal uint32
	PixelAspectVertical   uint32
	SampleRate            int
	Channels              int
	Duration              time.Duration
	DefaultDuration       time.Duration
	TimeScale             uint32

	edit mp4EditList
}

// mp4EditList is the part of a track's edit list the converter honours: an
// optional leading empty edit followed by the first media edit.
type mp4EditList struct {
	present bool
	// typeOffset is the file offset of the "edts" box type.
	typeOffset int64
	// empty and segment are in movie time; mediaTime is in media time.
	empty     time.Duration
	segment   time.Duration
	mediaTime time.Duration

	emptyTicks, segmentTicks uint64
	mediaTicks               int64
}

// MP4Info contains basic movie and track metadata read from the MP4 box tables.
type MP4Info struct {
	Duration time.Duration
	Tracks   []TrackInfo
}

type mp4Box struct {
	kind  string
	start int64
	data  int64
	end   int64
}

// ProbeMP4 reads container metadata without decoding the media. It supports
// regular, non-fragmented MP4 files with a moov box and returns the first sample
// description for each track.
func ProbeMP4(path string) (MP4Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: open MP4: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: stat MP4: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return MP4Info{}, fmt.Errorf("%w: input is not a regular file", ErrInvalidMP4)
	}
	return ProbeMP4Reader(f, stat.Size())
}

// ProbeMP4Reader is the ReaderAt form of ProbeMP4. size is the total length of
// the file. The reader is not closed or retained.
func ProbeMP4Reader(r io.ReaderAt, size int64) (MP4Info, error) {
	if r == nil || size < 8 {
		return MP4Info{}, ErrInvalidMP4
	}
	top, err := readMP4Boxes(r, 0, size)
	if err != nil {
		return MP4Info{}, fmt.Errorf("%w: %v", ErrInvalidMP4, err)
	}
	var hasFileType bool
	var movieBox *mp4Box
	for i := range top {
		switch top[i].kind {
		case "ftyp", "styp":
			hasFileType = true
		case "moov":
			box := top[i]
			movieBox = &box
		}
	}
	if !hasFileType || movieBox == nil {
		return MP4Info{}, fmt.Errorf("%w: missing ftyp or moov box", ErrInvalidMP4)
	}

	children, err := readMP4Boxes(r, movieBox.data, movieBox.end)
	if err != nil {
		return MP4Info{}, fmt.Errorf("%w: invalid moov box: %v", ErrInvalidMP4, err)
	}
	info := MP4Info{}
	var movieTimeScale uint32
	for _, child := range children {
		switch child.kind {
		case "mvhd":
			info.Duration, movieTimeScale, err = parseMovieDuration(r, child)
			if err != nil {
				return MP4Info{}, fmt.Errorf("%w: invalid mvhd box: %v", ErrInvalidMP4, err)
			}
		case "trak":
			track, parseErr := parseTrack(r, child)
			if parseErr != nil {
				return MP4Info{}, fmt.Errorf("%w: invalid trak box: %v", ErrInvalidMP4, parseErr)
			}
			track.Index = len(info.Tracks)
			info.Tracks = append(info.Tracks, track)
		}
	}
	if len(info.Tracks) == 0 {
		return MP4Info{}, fmt.Errorf("%w: movie has no tracks", ErrInvalidMP4)
	}
	for i := range info.Tracks {
		info.Tracks[i].edit.resolve(movieTimeScale, info.Tracks[i].TimeScale)
	}
	return info, nil
}

func (e *mp4EditList) resolve(movieTimeScale, mediaTimeScale uint32) {
	if !e.present {
		return
	}
	empty, emptyErr := toDuration(e.emptyTicks, movieTimeScale)
	segment, segmentErr := toDuration(e.segmentTicks, movieTimeScale)
	mediaTime, mediaErr := ticksToDuration(e.mediaTicks, mediaTimeScale)
	if emptyErr != nil || segmentErr != nil || mediaErr != nil {
		*e = mp4EditList{typeOffset: e.typeOffset}
		return
	}
	e.empty, e.segment, e.mediaTime = empty, segment, mediaTime
}

// parseEditList reads the leading empty edits and the first media edit of an
// edts box. Later edits and non-unit rates are not applied.
func parseEditList(r io.ReaderAt, edts mp4Box) (mp4EditList, error) {
	children, err := readMP4Boxes(r, edts.data, edts.end)
	if err != nil {
		return mp4EditList{}, err
	}
	for _, child := range children {
		if child.kind != "elst" {
			continue
		}
		data, err := readBoxPayload(r, child, 1<<20)
		if err != nil {
			return mp4EditList{}, err
		}
		if len(data) < 8 {
			return mp4EditList{}, io.ErrUnexpectedEOF
		}
		entrySize := 12
		if data[0] == 1 {
			entrySize = 20
		} else if data[0] != 0 {
			return mp4EditList{}, fmt.Errorf("unsupported elst version %d", data[0])
		}
		count := binary.BigEndian.Uint32(data[4:8])
		entries := data[8:]
		if uint64(count)*uint64(entrySize) > uint64(len(entries)) {
			return mp4EditList{}, errors.New("truncated elst box")
		}
		var edit mp4EditList
		for i := 0; i < int(count); i++ {
			entry := entries[i*entrySize:]
			var segment uint64
			var mediaTime int64
			if entrySize == 20 {
				segment = binary.BigEndian.Uint64(entry[:8])
				mediaTime = int64(binary.BigEndian.Uint64(entry[8:16]))
			} else {
				segment = uint64(binary.BigEndian.Uint32(entry[:4]))
				mediaTime = int64(int32(binary.BigEndian.Uint32(entry[4:8])))
			}
			if mediaTime < 0 {
				if segment > ^uint64(0)-edit.emptyTicks {
					return mp4EditList{}, errors.New("elst empty edits overflow")
				}
				edit.emptyTicks += segment
				continue
			}
			edit.present = true
			edit.mediaTicks = mediaTime
			edit.segmentTicks = segment
			return edit, nil
		}
		return mp4EditList{}, nil
	}
	return mp4EditList{}, nil
}

func readMP4Boxes(r io.ReaderAt, start, end int64) ([]mp4Box, error) {
	if start < 0 || end < start {
		return nil, errors.New("invalid box range")
	}
	boxes := make([]mp4Box, 0, 8)
	for pos := start; pos < end; {
		if end-pos < 8 {
			return nil, errors.New("truncated box header")
		}
		var header [16]byte
		if _, err := r.ReadAt(header[:8], pos); err != nil {
			return nil, err
		}
		boxSize := uint64(binary.BigEndian.Uint32(header[:4]))
		boxHeaderSize := int64(8)
		if boxSize == 1 {
			if end-pos < 16 {
				return nil, errors.New("truncated extended box header")
			}
			if _, err := r.ReadAt(header[8:16], pos+8); err != nil {
				return nil, err
			}
			boxSize = binary.BigEndian.Uint64(header[8:16])
			boxHeaderSize = 16
		} else if boxSize == 0 {
			boxSize = uint64(end - pos)
		}
		if boxSize < uint64(boxHeaderSize) || boxSize > uint64(end-pos) {
			return nil, fmt.Errorf("invalid %q box size", string(header[4:8]))
		}
		boxEnd := pos + int64(boxSize)
		boxes = append(boxes, mp4Box{
			kind:  string(header[4:8]),
			start: pos,
			data:  pos + boxHeaderSize,
			end:   boxEnd,
		})
		pos = boxEnd
	}
	return boxes, nil
}

func parseMovieDuration(r io.ReaderAt, box mp4Box) (time.Duration, uint32, error) {
	data, err := readBoxPrefix(r, box, 20)
	if err != nil {
		return 0, 0, err
	}
	if data[0] == 1 {
		if data, err = readBoxPrefix(r, box, 32); err != nil {
			return 0, 0, err
		}
	}
	version := data[0]
	var timeScale uint32
	var duration uint64
	switch version {
	case 0:
		timeScale = binary.BigEndian.Uint32(data[12:16])
		duration = uint64(binary.BigEndian.Uint32(data[16:20]))
	case 1:
		timeScale = binary.BigEndian.Uint32(data[20:24])
		duration = binary.BigEndian.Uint64(data[24:32])
	default:
		return 0, 0, fmt.Errorf("unsupported mvhd version %d", version)
	}
	value, err := toDuration(duration, timeScale)
	return value, timeScale, err
}

func parseTrack(r io.ReaderAt, box mp4Box) (TrackInfo, error) {
	children, err := readMP4Boxes(r, box.data, box.end)
	if err != nil {
		return TrackInfo{}, err
	}
	var mdia *mp4Box
	var edit mp4EditList
	for i := range children {
		switch children[i].kind {
		case "mdia":
			if mdia == nil {
				mdia = &children[i]
			}
		case "edts":
			// A malformed edit list is ignored rather than failing the probe.
			edit, _ = parseEditList(r, children[i])
			edit.typeOffset = children[i].start + 4
		}
	}
	if mdia == nil {
		return TrackInfo{}, errors.New("track has no mdia box")
	}
	mdiaChildren, err := readMP4Boxes(r, mdia.data, mdia.end)
	if err != nil {
		return TrackInfo{}, err
	}
	var handler string
	var duration time.Duration
	var timeScale uint32
	var minf *mp4Box
	for i := range mdiaChildren {
		child := mdiaChildren[i]
		switch child.kind {
		case "hdlr":
			handler, err = parseHandler(r, child)
			if err != nil {
				return TrackInfo{}, err
			}
		case "mdhd":
			duration, timeScale, err = parseMediaDuration(r, child)
			if err != nil {
				return TrackInfo{}, err
			}
		case "minf":
			minf = &mdiaChildren[i]
		}
	}
	if handler == "" {
		return TrackInfo{}, errors.New("track has no handler type")
	}
	track := TrackInfo{Duration: duration, TimeScale: timeScale, edit: edit}
	switch handler {
	case "vide":
		track.Kind = TrackKindVideo
	case "soun":
		track.Kind = TrackKindAudio
	default:
		track.Kind = TrackKindOther
	}
	if minf == nil {
		return track, nil
	}
	minfChildren, err := readMP4Boxes(r, minf.data, minf.end)
	if err != nil {
		return TrackInfo{}, err
	}
	for i := range minfChildren {
		if minfChildren[i].kind != "stbl" {
			continue
		}
		stblChildren, parseErr := readMP4Boxes(r, minfChildren[i].data, minfChildren[i].end)
		if parseErr != nil {
			return TrackInfo{}, parseErr
		}
		for j := range stblChildren {
			if stblChildren[j].kind == "stsd" {
				if err := parseSampleDescription(r, stblChildren[j], &track); err != nil {
					return TrackInfo{}, err
				}
				return track, nil
			}
		}
	}
	return track, nil
}

func parseHandler(r io.ReaderAt, box mp4Box) (string, error) {
	data, err := readBoxPrefix(r, box, 12)
	if err != nil {
		return "", err
	}
	return string(data[8:12]), nil
}

func parseMediaDuration(r io.ReaderAt, box mp4Box) (time.Duration, uint32, error) {
	versionData, err := readBoxPrefix(r, box, 4)
	if err != nil {
		return 0, 0, err
	}
	minimumSize := 0
	switch versionData[0] {
	case 0:
		minimumSize = 24
	case 1:
		minimumSize = 32
	default:
		return 0, 0, fmt.Errorf("unsupported mdhd version %d", versionData[0])
	}
	data, err := readBoxPrefix(r, box, minimumSize)
	if err != nil {
		return 0, 0, err
	}
	var timeScale uint32
	var duration uint64
	switch data[0] {
	case 0:
		timeScale = binary.BigEndian.Uint32(data[12:16])
		duration = uint64(binary.BigEndian.Uint32(data[16:20]))
	case 1:
		timeScale = binary.BigEndian.Uint32(data[20:24])
		duration = binary.BigEndian.Uint64(data[24:32])
	}
	value, err := toDuration(duration, timeScale)
	return value, timeScale, err
}

func parseSampleDescription(r io.ReaderAt, box mp4Box, track *TrackInfo) error {
	if box.end-box.data < 8 {
		return io.ErrUnexpectedEOF
	}
	var header [8]byte
	if _, err := r.ReadAt(header[:], box.data); err != nil {
		return err
	}
	entryCount := binary.BigEndian.Uint32(header[4:8])
	entries, err := readMP4Boxes(r, box.data+8, box.end)
	if err != nil {
		return err
	}
	if entryCount == 0 || len(entries) == 0 {
		return errors.New("stsd has no sample entries")
	}
	if uint64(entryCount) != uint64(len(entries)) {
		return errors.New("stsd entry count does not match its contents")
	}
	entry := entries[0]
	track.Codec = entry.kind
	dataLength := entry.end - entry.data
	if track.Kind == TrackKindVideo && dataLength >= 28 {
		var data [28]byte
		if _, err := r.ReadAt(data[:], entry.data); err != nil {
			return err
		}
		track.Width = int(binary.BigEndian.Uint16(data[24:26]))
		track.Height = int(binary.BigEndian.Uint16(data[26:28]))
	}
	if track.Kind == TrackKindAudio && dataLength >= 28 {
		var data [28]byte
		if _, err := r.ReadAt(data[:], entry.data); err != nil {
			return err
		}
		track.Channels = int(binary.BigEndian.Uint16(data[16:18]))
		rate := binary.BigEndian.Uint32(data[24:28])
		track.SampleRate = int(rate >> 16)
	}
	if track.Kind == TrackKindVideo && (entry.kind == "avc1" || entry.kind == "avc3") {
		if dataLength < 78 {
			return errors.New("truncated AVC sample entry")
		}
		children, err := readMP4Boxes(r, entry.data+78, entry.end)
		if err != nil {
			return fmt.Errorf("invalid video sample entry: %w", err)
		}
		for _, child := range children {
			switch child.kind {
			case "avcC":
				if len(track.CodecConfig) != 0 {
					return errors.New("AVC sample entry has multiple avcC boxes")
				}
				config, err := readBoxPayload(r, child, 1<<20)
				if err != nil {
					return err
				}
				lengthSize, err := AVCNALLengthSize(config)
				if err != nil {
					return err
				}
				track.CodecConfig = config
				track.NALLengthSize = lengthSize
			case "pasp":
				if child.end-child.data != 8 {
					return errors.New("invalid pixel aspect ratio box")
				}
				var data [8]byte
				if _, err := r.ReadAt(data[:], child.data); err != nil {
					return err
				}
				horizontal := binary.BigEndian.Uint32(data[:4])
				vertical := binary.BigEndian.Uint32(data[4:])
				if horizontal == 0 || vertical == 0 {
					return errors.New("invalid zero pixel aspect ratio")
				}
				track.PixelAspectHorizontal = horizontal
				track.PixelAspectVertical = vertical
			}
		}
		if len(track.CodecConfig) == 0 {
			return errors.New("AVC sample entry has no avcC configuration")
		}
	}
	if track.Kind == TrackKindAudio && entry.kind == "mp4a" {
		if dataLength < 28 {
			return errors.New("truncated AAC sample entry")
		}
		children, err := readMP4Boxes(r, entry.data+28, entry.end)
		if err != nil {
			return fmt.Errorf("invalid audio sample entry: %w", err)
		}
		for _, child := range children {
			if child.kind != "esds" {
				continue
			}
			esds, err := readBoxPayload(r, child, 1<<20)
			if err != nil {
				return err
			}
			if objectType, ok := esdsObjectTypeIndication(esds); ok && !isAACObjectTypeIndication(objectType) {
				// mp4a also carries MP3 and other MPEG audio; report the track
				// as unsupported audio instead of rejecting the whole file.
				track.Codec = fmt.Sprintf("mp4a.%02x", objectType)
				return nil
			}
			config, err := extractAACAudioSpecificConfig(esds)
			if err != nil {
				return err
			}
			track.CodecConfig = config
			break
		}
		if len(track.CodecConfig) == 0 {
			return errors.New("AAC sample entry has no esds configuration")
		}
	}
	return nil
}

func readBoxPayload(r io.ReaderAt, box mp4Box, maxSize int64) ([]byte, error) {
	size := box.end - box.data
	if size < 0 || size > maxSize || size > int64(^uint(0)>>1) {
		return nil, errors.New("MP4 codec configuration is too large")
	}
	data := make([]byte, int(size))
	if _, err := r.ReadAt(data, box.data); err != nil {
		return nil, err
	}
	return data, nil
}

func readBoxPrefix(r io.ReaderAt, box mp4Box, minSize int) ([]byte, error) {
	size := box.end - box.data
	if size < int64(minSize) {
		return nil, io.ErrUnexpectedEOF
	}
	readSize := int64(minSize)
	data := make([]byte, readSize)
	if _, err := r.ReadAt(data, box.data); err != nil {
		return nil, err
	}
	return data, nil
}

func toDuration(duration uint64, timeScale uint32) (time.Duration, error) {
	if timeScale == 0 {
		return 0, errors.New("zero media time scale")
	}
	seconds := duration / uint64(timeScale)
	remainder := duration % uint64(timeScale)
	if seconds > uint64((1<<63-1)/int64(time.Second)) {
		return 0, errors.New("duration overflows time.Duration")
	}
	nanoseconds := seconds*uint64(time.Second) + remainder*uint64(time.Second)/uint64(timeScale)
	if nanoseconds > uint64(1<<63-1) {
		return 0, errors.New("duration overflows time.Duration")
	}
	return time.Duration(nanoseconds), nil
}
