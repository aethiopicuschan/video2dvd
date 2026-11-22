package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thesyncim/goav/container/webm"
)

// Codecs of the text subtitle tracks the converter reads, as TrackInfo
// reports them.
const (
	// SubtitleCodecTX3G is MP4 timed text (3GPP TS 26.245, MPEG-4 Part 17),
	// which ffmpeg calls mov_text.
	SubtitleCodecTX3G = "tx3g"
	// SubtitleCodecWebMVTT is WebVTT stored as the WebM project defines it:
	// each block holds the cue identifier, the cue settings and the cue
	// text, separated by line feeds.
	SubtitleCodecWebMVTT = "D_WEBVTT/SUBTITLES"
	// SubtitleCodecMatroskaVTT is WebVTT as the Matroska specification
	// stores it: each block holds the cue text only.
	SubtitleCodecMatroskaVTT = "S_TEXT/WEBVTT"
	// SubtitleCodecMatroskaSRT is plain UTF-8 text, the SubRip format.
	SubtitleCodecMatroskaSRT = "S_TEXT/UTF8"
)

// SupportedSubtitleCodec reports whether the converter reads subtitle tracks
// of a codec, as reported by TrackInfo.Codec.
func SupportedSubtitleCodec(codec string) bool {
	switch codec {
	case SubtitleCodecTX3G, SubtitleCodecWebMVTT, SubtitleCodecMatroskaVTT, SubtitleCodecMatroskaSRT:
		return true
	}
	return false
}

// SubtitleCue is a text shown from Start until End. Text is plain text: the
// markup of the source format is removed, character references are
// resolved, and lines are separated by "\n".
type SubtitleCue struct {
	Start time.Duration
	End   time.Duration
	Text  string
}

const (
	// subtitleMaxSampleBytes bounds one subtitle sample or block.
	subtitleMaxSampleBytes = 1 << 20
	// subtitleMaxCues bounds the cues of one track.
	subtitleMaxCues = 1_000_000
	// subtitleOpenEndedCue is how long a cue without a stated end is shown
	// when no later cue ends it earlier.
	subtitleOpenEndedCue = 5 * time.Second
)

// ReadSubtitleCues reads the cues of a text subtitle track (see
// SupportedSubtitleCodec) of an MP4 or WebM file. trackIndex is the
// zero-based track index of TrackInfo.Index. Cues are returned in file order
// on the timeline of the file: the WebM block timestamps, or for MP4 the
// movie timeline the track's edit list maps its samples to. A cue whose
// duration is not stated lasts until the next cue starts, at most five
// seconds. Cues may overlap; cues without text are left out.
func ReadSubtitleCues(path string, trackIndex int) ([]SubtitleCue, error) {
	info, err := ProbeMedia(path)
	if err != nil {
		return nil, err
	}
	if trackIndex < 0 || trackIndex >= len(info.Tracks) {
		return nil, fmt.Errorf("video2dvd: subtitle track index %d is out of range", trackIndex)
	}
	track := info.Tracks[trackIndex]
	if track.Kind != TrackKindSubtitle || !SupportedSubtitleCodec(track.Codec) {
		return nil, fmt.Errorf("video2dvd: track %d is not a supported text subtitle track (%s %q)", trackIndex, track.Kind, track.Codec)
	}
	if track.Codec == SubtitleCodecTX3G {
		return readMP4SubtitleCues(path, track)
	}
	return readWebMSubtitleCues(path, track)
}

// readMP4SubtitleCues reads the samples of a tx3g track.
func readMP4SubtitleCues(path string, track TrackInfo) ([]SubtitleCue, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: open MP4: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("video2dvd: stat MP4: %w", err)
	}
	trak, err := findMP4Trak(f, stat.Size(), track.Index)
	if err != nil {
		return nil, err
	}
	tables, err := parseSampleTables(f, trak, true)
	if err != nil {
		return nil, fmt.Errorf("video2dvd: subtitle track %d sample tables: %w", track.Index, err)
	}
	if len(tables.sizes) > subtitleMaxCues {
		return nil, fmt.Errorf("video2dvd: subtitle track %d has too many samples", track.Index)
	}
	// Media time moves to the movie timeline by the track's own edit list,
	// as the video's does (prepareMP4VideoForDVD).
	shift := time.Duration(0)
	if track.edit.present {
		shift = track.edit.empty - track.edit.mediaTime
	}
	var cues []SubtitleCue
	err = emitTrackSamples(f, stat.Size(), track, tables, func(_ TrackInfo, sample SampleInfo, data io.Reader) error {
		if sample.Size > subtitleMaxSampleBytes {
			return fmt.Errorf("video2dvd: subtitle sample %d is too large", sample.SampleNumber)
		}
		payload := make([]byte, sample.Size)
		if _, err := io.ReadFull(data, payload); err != nil {
			return fmt.Errorf("video2dvd: read subtitle sample %d: %w", sample.SampleNumber, err)
		}
		start, err := ticksToDuration(sample.PTS, track.TimeScale)
		if err != nil {
			return err
		}
		length, err := ticksToDuration(int64(sample.Duration), track.TimeScale)
		if err != nil {
			return err
		}
		text := cleanSubtitleText(tx3gSampleText(payload))
		if text == "" || length <= 0 {
			return nil
		}
		cues = append(cues, SubtitleCue{Start: start + shift, End: start + shift + length, Text: text})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("video2dvd: subtitle track %d: %w", track.Index, err)
	}
	return cues, nil
}

// findMP4Trak returns the trak box of the track with the given index.
func findMP4Trak(r io.ReaderAt, size int64, index int) (mp4Box, error) {
	top, err := readMP4Boxes(r, 0, size)
	if err != nil {
		return mp4Box{}, fmt.Errorf("%w: %v", ErrInvalidMP4, err)
	}
	for _, box := range top {
		if box.kind != "moov" {
			continue
		}
		children, err := readMP4Boxes(r, box.data, box.end)
		if err != nil {
			return mp4Box{}, fmt.Errorf("%w: %v", ErrInvalidMP4, err)
		}
		for _, child := range children {
			if child.kind != "trak" {
				continue
			}
			if index == 0 {
				return child, nil
			}
			index--
		}
	}
	return mp4Box{}, fmt.Errorf("%w: track not found", ErrInvalidMP4)
}

// tx3gSampleText returns the text of a timed-text sample: a 16-bit length
// and that many bytes of UTF-8, or of UTF-16 after a byte order mark. Style
// and other boxes may follow and are ignored.
func tx3gSampleText(sample []byte) string {
	if len(sample) < 2 {
		return ""
	}
	length := int(binary.BigEndian.Uint16(sample))
	text := sample[2:]
	if length < len(text) {
		text = text[:length]
	}
	if len(text) >= 2 && (text[0] == 0xfe && text[1] == 0xff || text[0] == 0xff && text[1] == 0xfe) {
		order := binary.ByteOrder(binary.BigEndian)
		if text[0] == 0xff {
			order = binary.LittleEndian
		}
		units := make([]uint16, (len(text)-2)/2)
		for i := range units {
			units[i] = order.Uint16(text[2+2*i:])
		}
		return string(utf16.Decode(units))
	}
	return strings.ToValidUTF8(string(text), "")
}

// readWebMSubtitleCues reads the blocks of a WebM subtitle track.
func readWebMSubtitleCues(path string, track TrackInfo) ([]SubtitleCue, error) {
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
	if track.Index >= len(tracks) {
		return nil, fmt.Errorf("video2dvd: WebM track %d not found", track.Index)
	}
	trackID := tracks[track.Index].ID
	type block struct {
		start, duration time.Duration
		text            string
	}
	var blocks []block
	packet := webm.Packet{Data: make([]byte, 0, maxWebMPacketBytes)}
	for {
		if err := demuxer.ReadPacket(&packet); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("video2dvd: read WebM packet: %w", err)
		}
		if packet.TrackID != trackID {
			continue
		}
		if len(blocks) >= subtitleMaxCues {
			return nil, fmt.Errorf("video2dvd: subtitle track %d has too many cues", track.Index)
		}
		duration := time.Duration(packet.DurationNS)
		if duration <= 0 {
			duration = track.DefaultDuration
		}
		blocks = append(blocks, block{start: time.Duration(packet.TimeNS), duration: duration, text: webMSubtitleText(track.Codec, packet.Data)})
	}
	cues := make([]SubtitleCue, 0, len(blocks))
	for i, b := range blocks {
		end := b.start + b.duration
		if b.duration <= 0 {
			end = b.start + subtitleOpenEndedCue
			// The next cue that starts later ends an open-ended one.
			for _, next := range blocks[i+1:] {
				if next.start > b.start {
					end = min(end, next.start)
					break
				}
			}
		}
		if b.text == "" || end <= b.start {
			continue
		}
		cues = append(cues, SubtitleCue{Start: b.start, End: end, Text: b.text})
	}
	return cues, nil
}

// webMSubtitleText returns the plain text of a subtitle block.
func webMSubtitleText(codec string, data []byte) string {
	text := strings.ReplaceAll(strings.ToValidUTF8(string(data), ""), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	switch codec {
	case SubtitleCodecWebMVTT:
		// The cue identifier and the cue settings come first, each on a
		// line of its own.
		for range 2 {
			if newline := strings.IndexByte(text, '\n'); newline >= 0 {
				text = text[newline+1:]
			} else {
				text = ""
			}
		}
		return cleanSubtitleText(stripWebVTTMarkup(text))
	case SubtitleCodecMatroskaVTT:
		return cleanSubtitleText(stripWebVTTMarkup(text))
	default:
		return cleanSubtitleText(stripSRTMarkup(text))
	}
}

// stripWebVTTMarkup removes the tags of WebVTT cue text and resolves its
// character references. Ruby text (<rt>) is left out with its tags: the
// subtitles have no room above a line for it.
func stripWebVTTMarkup(text string) string {
	var out strings.Builder
	rubyText := 0
	for len(text) > 0 {
		switch text[0] {
		case '<':
			end := strings.IndexByte(text, '>')
			if end < 0 {
				// A lone "<" is not a tag.
				out.WriteByte('<')
				text = text[1:]
				continue
			}
			tag := strings.ToLower(strings.TrimSpace(text[1:end]))
			name, closing := strings.TrimPrefix(tag, "/"), strings.HasPrefix(tag, "/")
			if fields := strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == ' ' || r == '\t' }); len(fields) > 0 {
				name = fields[0]
			}
			if name == "rt" {
				if closing {
					rubyText = max(rubyText-1, 0)
				} else {
					rubyText++
				}
			}
			text = text[end+1:]
		case '&':
			char, size := decodeCharacterReference(text)
			if rubyText == 0 {
				out.WriteString(char)
			}
			text = text[size:]
		default:
			_, size := utf8.DecodeRuneInString(text)
			if rubyText == 0 {
				out.WriteString(text[:size])
			}
			text = text[size:]
		}
	}
	return out.String()
}

// decodeCharacterReference resolves the HTML character reference at the
// start of text, which begins with "&". It returns what the reference stands
// for and how many bytes it takes; anything that is not a known reference is
// an ampersand by itself.
func decodeCharacterReference(text string) (string, int) {
	end := strings.IndexByte(text, ';')
	if end < 2 || end > 10 {
		return "&", 1
	}
	name := text[1:end]
	named := map[string]string{
		"amp": "&", "lt": "<", "gt": ">", "quot": "\"", "apos": "'",
		"nbsp": "\u00a0", "lrm": "\u200e", "rlm": "\u200f",
	}
	if value, ok := named[name]; ok {
		return value, end + 1
	}
	if strings.HasPrefix(name, "#") {
		digits, base := name[1:], 10
		if strings.HasPrefix(digits, "x") || strings.HasPrefix(digits, "X") {
			digits, base = digits[1:], 16
		}
		if code, err := strconv.ParseUint(digits, base, 32); err == nil && digits != "" && utf8.ValidRune(rune(code)) {
			return string(rune(code)), end + 1
		}
	}
	return "&", 1
}

// stripSRTMarkup removes the HTML-like tags (<i>, <b>, <u>, <font ...>) and
// the Advanced SubStation override blocks ({\an8}) that SubRip files carry.
func stripSRTMarkup(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		if text[0] == '<' {
			if end := strings.IndexByte(text, '>'); end > 0 && isSRTTag(text[1:end]) {
				text = text[end+1:]
				continue
			}
		}
		if strings.HasPrefix(text, "{\\") {
			if end := strings.IndexByte(text, '}'); end > 0 {
				text = text[end+1:]
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(text)
		out.WriteString(text[:size])
		text = text[size:]
	}
	return out.String()
}

// isSRTTag reports whether the inside of angle brackets is a formatting tag
// of SubRip text.
func isSRTTag(tag string) bool {
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tag), "/"))
	if fields := strings.Fields(name); len(fields) > 0 {
		name = fields[0]
	}
	return slices.Contains([]string{"i", "b", "u", "s", "font"}, name)
}

// cleanSubtitleText normalizes the plain text of a cue: line breaks become
// "\n", tabs and no-break spaces become spaces, control and invisible
// format characters are removed, and lines are trimmed; empty lines are
// dropped.
func cleanSubtitleText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var lines []string
	for line := range strings.FieldsFuncSeq(text, func(r rune) bool { return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 }) {
		line = strings.Map(func(r rune) rune {
			switch {
			case r == '\t' || r == 0xa0:
				return ' '
			case r >= 0x200b && r <= 0x200f || r == 0xfeff || r == 0x2060:
				return -1
			case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069:
				return -1
			case r < 0x20 || r == 0x7f:
				return -1
			}
			return r
		}, line)
		// Runs of spaces become one; the ideographic space of Japanese
		// text is kept inside a line, as it is wider.
		line = strings.Join(strings.FieldsFunc(line, func(r rune) bool { return r == ' ' }), " ")
		line = strings.Trim(line, " \u3000")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// selectInputSubtitleTracks returns the subtitle tracks to record for an
// input: the requested ones, which must be readable text subtitle tracks,
// or with requested nil and includeAll set every readable one, up to the
// streams of a title: the first that carries a default mark comes first
// (see hasDefaultMark), the others follow in track order.
func selectInputSubtitleTracks(info MP4Info, requested []int, includeAll bool) ([]int, error) {
	readable := func(track TrackInfo) bool {
		return track.Kind == TrackKindSubtitle && SupportedSubtitleCodec(track.Codec)
	}
	if requested == nil {
		if !includeAll {
			return nil, nil
		}
		var all []int
		first, found := selectDefaultTrack(info, readable)
		if found {
			all = append(all, first.Index)
		}
		for _, track := range info.Tracks {
			if readable(track) && !(found && track.Index == first.Index) && len(all) < dvdSubtitleMaxStreams {
				all = append(all, track.Index)
			}
		}
		return all, nil
	}
	if len(requested) > dvdSubtitleMaxStreams {
		return nil, fmt.Errorf("a title has at most %d subtitle streams, %d were requested", dvdSubtitleMaxStreams, len(requested))
	}
	for i, index := range requested {
		if slices.Contains(requested[:i], index) {
			return nil, fmt.Errorf("subtitle track %d is requested twice", index)
		}
		if index < 0 || index >= len(info.Tracks) {
			return nil, fmt.Errorf("subtitle track index %d is out of range", index)
		}
		if track := info.Tracks[index]; !readable(track) {
			return nil, fmt.Errorf("track %d is not a supported text subtitle track (%s %q)", index, track.Kind, track.Codec)
		}
	}
	return slices.Clone(requested), nil
}
