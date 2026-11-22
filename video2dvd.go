// Package video2dvd provides APIs for preparing DVD-compatible media and
// orchestrating DVD-Video directory creation from MP4 and WebM media.
//
// The package itself does not start external processes. It includes in-process
// encoder and authorer implementations for creating menu-less DVD-Video output.
package video2dvd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var (
	ErrNoInputs       = errors.New("video2dvd: at least one input file is required")
	ErrOutputExists   = errors.New("video2dvd: output path already exists")
	ErrInvalidProfile = errors.New("video2dvd: invalid DVD profile")
	ErrDiscCapacity   = errors.New("video2dvd: authored DVD exceeds configured capacity")
)

const DVD5CapacityBytes int64 = 4_700_000_000

// VideoStandard selects the DVD video raster and frame-rate family.
type VideoStandard uint8

const (
	VideoStandardNTSC VideoStandard = iota + 1
	VideoStandardPAL
)

// AspectRatio is the display aspect ratio signaled in the MPEG-2 stream.
type AspectRatio uint8

const (
	AspectRatio4x3 AspectRatio = iota + 1
	AspectRatio16x9
)

// Profile describes the DVD-compatible video target requested from Encoder.
type Profile struct {
	Standard          VideoStandard
	Aspect            AspectRatio
	FrameWidth        int
	FrameHeight       int
	DiscCapacityBytes int64
	// MenuTitle and menu luma values customize the generated title menu.
	MenuTitle          string
	MenuBackgroundLuma uint8
	MenuTextLuma       uint8
	// MenuFont, if not nil, draws the menu text and the subtitles; see
	// Options.MenuFont.
	MenuFont     *MenuFont
	VideoBitrate int
	// DisableSoftTelecine is Options.DisableSoftTelecine.
	DisableSoftTelecine bool
	// ShowSubtitles is Options.ShowSubtitles.
	ShowSubtitles bool
	// AudioFormat and AC3Bitrate are Options.AudioFormat and
	// Options.AC3Bitrate; zero values select the defaults.
	AudioFormat AudioFormat
	AC3Bitrate  int
}

// AudioFormat selects how the built-in encoder codes the audio streams of
// a title.
type AudioFormat uint8

const (
	// AudioFormatAuto codes a title with one audio stream as LPCM and a
	// title with several as AC-3: two LPCM streams beside full-rate video
	// exceed the DVD multiplex rate.
	AudioFormatAuto AudioFormat = iota
	// AudioFormatLPCM codes every stream as 16-bit 48 kHz stereo LPCM. With
	// several streams the video bitrate is lowered so that video and audio
	// fit the multiplex rate; see Options.AudioFormat.
	AudioFormatLPCM
	// AudioFormatAC3 codes every stream as 48 kHz stereo AC-3 at
	// Options.AC3Bitrate.
	AudioFormatAC3
)

// DefaultAC3Bitrate is the AC-3 bitrate used when Options.AC3Bitrate is
// zero, in bit/s.
const DefaultAC3Bitrate = 192_000

// Options controls conversion and DVD authoring. The zero value selects NTSC,
// 16:9, and the full-resolution DVD raster for that standard.
type Options struct {
	Standard          VideoStandard
	Aspect            AspectRatio
	DiscCapacityBytes int64
	// MenuTitle is the heading of the generated title menu; empty selects
	// VIDEO2DVD. It and the title names may contain Japanese text.
	MenuTitle          string
	MenuBackgroundLuma uint8
	MenuTextLuma       uint8
	// MenuFont is an optional TrueType or OpenType font, made by
	// ParseMenuFont, for the text of the title menu. Text is drawn
	// anti-aliased, in the case it was written in, and with the proportions
	// of the font on the display. Characters the font lacks are drawn with
	// the built-in fonts. With a nil MenuFont all text uses the built-in
	// fonts: ASCII in upper case, and Japanese in an embedded 16-pixel
	// bitmap font. Subtitles (IncludeSubtitles) are drawn with the same
	// fonts, at twice the size.
	MenuFont     *MenuFont
	VideoBitrate int
	// VolumeID is the volume name of an ISO image, as shown when the disc is
	// mounted. It may contain up to 32 ASCII letters, digits, underscores,
	// and spaces; letters are stored in upper case. Empty selects VIDEO2DVD.
	// It has no effect on directory output.
	VolumeID string
	// DisableSoftTelecine turns off the film mode of NTSC titles. By default
	// the built-in encoder codes a source whose whole video track runs at
	// 24000/1001 or 24 frames per second as film: only the source frames are coded, and
	// flags in the stream have the player show them for three and two fields
	// in turn (3:2 pulldown) or, on a progressive-scan player, as they are.
	// With this set such a source is converted like any other, by showing
	// every fourth frame twice. It has no effect on PAL titles and on
	// sources at other frame rates.
	DisableSoftTelecine bool
	// IncludeSubtitles records the text subtitle tracks of the inputs as
	// DVD subtitles, which the viewer turns on and off and chooses between
	// with the player's subtitle button. For each input whose
	// MediaInput.SubtitleTracks is nil, every track of its video file that
	// ReadSubtitleCues can read becomes a subtitle stream, up to the 32
	// streams of a title: the one that carries a default mark (see
	// TrackInfo.Default) first and the others in track order; other subtitle
	// tracks are skipped. An
	// input without such tracks gets no subtitles. Subtitles are drawn as
	// WriteDVDSubtitleStream describes, with MenuFont when it is set.
	IncludeSubtitles bool
	// ShowSubtitles turns the first subtitle stream of a title on when the
	// title starts playing. By default a title with subtitles starts with
	// them hidden. It has no effect on titles without subtitles, and
	// ShowDefaultSubtitles and MediaInput.ShowSubtitles take precedence.
	ShowSubtitles bool
	// ShowDefaultSubtitles starts a title with its first subtitle stream
	// shown when the track it comes from carries the container's default
	// mark (TrackInfo.Default; for WebM a track without a FlagDefault element
	// counts only when another subtitle track has FlagDefault 0, as ffmpeg
	// writes it), and hidden otherwise, instead of following
	// ShowSubtitles. MediaInput.ShowSubtitles takes precedence over it.
	ShowDefaultSubtitles bool
	// IncludeAllAudio records every audio track of an input that the
	// built-in decoders read, up to the 8 audio streams of a title, for
	// each input whose MediaInput.AudioTracks and AudioTrackIndex are nil;
	// tracks they cannot decode, such as AAC with a program config element, are skipped. The track that
	// carries a default mark (or else the first) becomes audio stream 1,
	// which players start with; the others follow in track order. Each
	// stream is labelled with its track's language (see DVDLanguageCode).
	IncludeAllAudio bool
	// AudioFormat selects LPCM or AC-3 for the audio; the zero value,
	// AudioFormatAuto, keeps a single stream LPCM and codes several as AC-3.
	// With AudioFormatLPCM and several streams the video bitrate is lowered
	// to make room: LPCM takes 1.536 Mbit/s per stream, and video and audio
	// together may take 9.036 Mbit/s, the share of a 7.5 Mbit/s video and
	// one LPCM stream. A title whose video would get less than 2 Mbit/s that
	// way, which is the case from five LPCM streams on, is refused before
	// anything is encoded. AC-3 streams take their bitrate from that share
	// as well, so eight streams at 448 kbit/s leave 5.45 Mbit/s to video.
	AudioFormat AudioFormat
	// AC3Bitrate is the bitrate of every AC-3 stream in bit/s: 192000,
	// 224000, 256000, 320000, 384000 or 448000. Zero selects
	// DefaultAC3Bitrate.
	AC3Bitrate int
}

func (o Options) profile() (Profile, error) {
	if o.Standard == 0 {
		o.Standard = VideoStandardNTSC
	}
	if o.Aspect == 0 {
		o.Aspect = AspectRatio16x9
	}
	if o.Aspect != AspectRatio4x3 && o.Aspect != AspectRatio16x9 {
		return Profile{}, ErrInvalidProfile
	}
	if o.DiscCapacityBytes < 0 {
		return Profile{}, ErrInvalidProfile
	}
	if o.DiscCapacityBytes == 0 {
		o.DiscCapacityBytes = DVD5CapacityBytes
	}
	if o.AudioFormat > AudioFormatAC3 {
		return Profile{}, ErrInvalidProfile
	}
	if o.AC3Bitrate == 0 {
		o.AC3Bitrate = DefaultAC3Bitrate
	}
	if !slices.Contains(ac3Bitrates, o.AC3Bitrate) {
		return Profile{}, fmt.Errorf("%w: AC-3 bitrate %d is not one of %v", ErrInvalidProfile, o.AC3Bitrate, ac3Bitrates)
	}

	switch o.Standard {
	case VideoStandardNTSC:
		return Profile{Standard: o.Standard, Aspect: o.Aspect, FrameWidth: 720, FrameHeight: 480, DiscCapacityBytes: o.DiscCapacityBytes, MenuTitle: o.MenuTitle, MenuBackgroundLuma: o.MenuBackgroundLuma, MenuTextLuma: o.MenuTextLuma, MenuFont: o.MenuFont, VideoBitrate: o.VideoBitrate, DisableSoftTelecine: o.DisableSoftTelecine, ShowSubtitles: o.ShowSubtitles, AudioFormat: o.AudioFormat, AC3Bitrate: o.AC3Bitrate}, nil
	case VideoStandardPAL:
		return Profile{Standard: o.Standard, Aspect: o.Aspect, FrameWidth: 720, FrameHeight: 576, DiscCapacityBytes: o.DiscCapacityBytes, MenuTitle: o.MenuTitle, MenuBackgroundLuma: o.MenuBackgroundLuma, MenuTextLuma: o.MenuTextLuma, MenuFont: o.MenuFont, VideoBitrate: o.VideoBitrate, DisableSoftTelecine: o.DisableSoftTelecine, ShowSubtitles: o.ShowSubtitles, AudioFormat: o.AudioFormat, AC3Bitrate: o.AC3Bitrate}, nil
	default:
		return Profile{}, ErrInvalidProfile
	}
}

// EncodedTitle describes DVD-compatible elementary streams produced for one
// input. Relative paths are resolved from the work directory passed to
// Encoder.Encode. Both streams must be non-empty files inside that directory.
// Video is MPEG-2; audio must be AC-3, LPCM, or MPEG-1 Layer II. The authorer
// may use the duration and frame rate to build navigation data.
//
// The frame rate is that of the MPEG-2 sequence header, which on a DVD is the
// display rate of the standard: 30000/1001 or 25. Film on an NTSC disc is
// such a 30000/1001 stream as well, with repeat_first_field set on the
// pictures that are shown for three fields (soft telecine); the multiplexer
// times every picture by its own flags. 24000/1001 is not accepted.
type EncodedTitle struct {
	VideoPath            string
	AudioPath            string
	VideoCodec           string
	AudioCodec           string
	Aspect               AspectRatio
	Width                int
	Height               int
	FrameRateNumerator   int
	FrameRateDenominator int
	Duration             time.Duration
	// Name is an optional label for the title in the disc menu. The
	// Converter sets it to the base name of the input video file without
	// its extension unless the encoder provided one; without a name the
	// menu falls back to the base name of VideoPath.
	Name string
	// Subtitles are the subtitle streams of the title, at most 32, in DVD
	// stream order. Their files may be empty; a stream without units has no
	// subtitles to show.
	Subtitles []EncodedSubtitle
	// ShowSubtitles, if not nil, decides for this title whether its first
	// subtitle stream is shown when it starts, instead of
	// Profile.ShowSubtitles. The Converter sets it from
	// MediaInput.ShowSubtitles and Options.ShowDefaultSubtitles unless the
	// encoder did.
	ShowSubtitles *bool
	// Audio lists the audio streams of the title, at most 8, in DVD stream
	// order; the first is the one players start with. When it is empty the
	// title has the one stream of AudioPath and AudioCodec. When it is set,
	// AudioPath and AudioCodec are those of its first stream (the Converter
	// fills them in when they are empty). The built-in authorer multiplexes
	// LPCM and AC-3 streams.
	Audio []EncodedAudio
}

// EncodedAudio is one audio stream of an EncodedTitle.
type EncodedAudio struct {
	// Path is the stream: big-endian 16-bit 48 kHz stereo samples for
	// LPCM, as WriteDVDLPCM writes them, or AC-3 syncframes at 48 kHz.
	// Relative paths are resolved from the work directory.
	Path string
	// Codec is AudioCodecLPCM or AudioCodecAC3.
	Codec string
	// Language is the ISO 639-1 code of the stream, such as "ja", which
	// players show when switching audio (see DVDLanguageCode); empty marks
	// it as unspecified.
	Language string
}

// EncodedSubtitle is one DVD subtitle stream of an EncodedTitle.
type EncodedSubtitle struct {
	// Path is a subtitle stream file as WriteDVDSubtitleStream writes it.
	// A relative path is resolved from the work directory, and the file
	// must be inside it.
	Path string
	// Language is the ISO 639-1 code of the subtitles, such as "ja", which
	// players show and choose streams by (see DVDLanguageCode); empty marks
	// the language as unspecified.
	Language string
}

// MediaInput identifies the video and optional audio source for one DVD title.
// When AudioPath is empty, audio is read from VideoPath. Track indexes are
// zero-based container track indexes; nil selects the first supported track.
// AudioOffset shifts decoded audio on the video timeline. Positive values add
// silence before the audio; negative values trim audio that starts before the
// video.
//
// SubtitleTracks lists the indexes of the text subtitle tracks of VideoPath
// to record as the title's subtitle streams, in stream order: the first is
// subtitle stream 1 of the title. Each must be a track ReadSubtitleCues can
// read, and a title has at most 32 streams. Nil leaves the choice to
// Options.IncludeSubtitles; an empty, non-nil list records no subtitles.
// Subtitles follow the video: they keep their place on the timeline of
// VideoPath, whatever AudioOffset is. ShowSubtitles, if not nil, decides
// whether the title's first subtitle stream is shown when it starts, over
// Options.ShowDefaultSubtitles and Options.ShowSubtitles.
//
// AudioTracks lists the audio tracks to record as the title's audio
// streams, in stream order: the first is audio stream 1, which players
// start with. They are tracks of AudioPath, or of VideoPath when AudioPath
// is empty, at most 8, each of a codec the built-in decoders read; all are
// placed on the timeline like a single audio track, including AudioOffset.
// It cannot be combined with AudioTrackIndex. Nil leaves the choice to
// Options.IncludeAllAudio and otherwise records one track: AudioTrackIndex,
// or the first supported track that carries a default mark
// (TrackInfo.Default), or the first supported track.
type MediaInput struct {
	VideoPath       string
	AudioPath       string
	VideoTrackIndex *int
	AudioTrackIndex *int
	AudioOffset     time.Duration
	SubtitleTracks  []int
	ShowSubtitles   *bool
	AudioTracks     []int
}

const (
	VideoCodecMPEG2       = "mpeg2video"
	AudioCodecAC3         = "ac3"
	AudioCodecLPCM        = "lpcm"
	AudioCodecMPEG1Layer2 = "mp2"
)

// Encoder decodes and encodes one input file into DVD-compatible elementary streams.
// Implementations should honor context cancellation and keep generated files
// inside workDir. It is called once for each input, in input order.
type Encoder interface {
	Encode(ctx context.Context, inputPath, workDir string, profile Profile) (EncodedTitle, error)
}

// StreamEncoder optionally accepts independent video and audio sources.
// Converter uses it when available; legacy Encoder implementations continue to
// work for single-file inputs without explicit track selection or offsets.
type StreamEncoder interface {
	EncodeStreams(ctx context.Context, input MediaInput, workDir string, profile Profile) (EncodedTitle, error)
}

// Authorer creates a DVD-Video directory (including VIDEO_TS.IFO/BUP and VOB
// files) from the encoded titles. outputDir is an empty directory. Implementors
// should honor context cancellation. Converter passes absolute elementary-stream
// paths in titles.
type Authorer interface {
	Author(ctx context.Context, titles []EncodedTitle, outputDir string, profile Profile) error
}

// Converter coordinates encoding and DVD authoring. It stages all files beside
// the destination and publishes the result with a directory rename only after
// authoring succeeds.
type Converter struct {
	encoder  Encoder
	authorer Authorer
}

// New constructs a Converter with the supplied encoding and authoring
// backends. It does not select defaults or invoke external commands.
func New(encoder Encoder, authorer Authorer) (*Converter, error) {
	if encoder == nil {
		return nil, errors.New("video2dvd: encoder is required")
	}
	if authorer == nil {
		return nil, errors.New("video2dvd: authorer is required")
	}
	return &Converter{encoder: encoder, authorer: authorer}, nil
}

// Convert turns inputs into a DVD-Video directory at outputDir. outputDir must
// not already exist; it will contain a VIDEO_TS subdirectory on success.
func (c *Converter) Convert(ctx context.Context, inputs []string, outputDir string, options Options) error {
	mediaInputs := make([]MediaInput, len(inputs))
	for i, input := range inputs {
		mediaInputs[i] = MediaInput{VideoPath: input}
	}
	return c.ConvertSources(ctx, mediaInputs, outputDir, options)
}

// ConvertSources creates a DVD-Video directory from video sources and optional
// independent audio sources. Each MediaInput becomes one DVD title.
func (c *Converter) ConvertSources(ctx context.Context, inputs []MediaInput, outputDir string, options Options) error {
	if c == nil || c.encoder == nil || c.authorer == nil {
		return errors.New("video2dvd: converter is not configured")
	}
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if len(inputs) == 0 {
		return ErrNoInputs
	}
	profile, err := options.profile()
	if err != nil {
		return err
	}

	mediaInputs, err := validateMediaInputs(inputs, options, profile)
	if err != nil {
		return err
	}
	outputDir, err = filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("video2dvd: resolve output path: %w", err)
	}
	parent := filepath.Dir(outputDir)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("video2dvd: inspect output parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("video2dvd: output parent %q is not a directory", parent)
	}
	if _, err := os.Lstat(outputDir); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, outputDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("video2dvd: inspect output path: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	stageRoot, err := os.MkdirTemp(parent, ".video2dvd-")
	if err != nil {
		return fmt.Errorf("video2dvd: create staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	encodedRoot := filepath.Join(stageRoot, "encoded")
	if err := os.Mkdir(encodedRoot, 0o700); err != nil {
		return fmt.Errorf("video2dvd: create encoded directory: %w", err)
	}
	titles := make([]EncodedTitle, 0, len(mediaInputs))
	var encodedBytes int64
	for i, input := range mediaInputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		workDir := filepath.Join(encodedRoot, fmt.Sprintf("title-%03d", i+1))
		if err := os.Mkdir(workDir, 0o700); err != nil {
			return fmt.Errorf("video2dvd: create work directory for input %d: %w", i+1, err)
		}
		var title EncodedTitle
		if streamEncoder, ok := c.encoder.(StreamEncoder); ok {
			title, err = streamEncoder.EncodeStreams(ctx, input, workDir, profile)
		} else if input.AudioPath == input.VideoPath && input.VideoTrackIndex == nil && input.AudioTrackIndex == nil && input.AudioOffset == 0 && len(input.SubtitleTracks) == 0 && input.AudioTracks == nil {
			title, err = c.encoder.Encode(ctx, input.VideoPath, workDir, profile)
		} else {
			err = errors.New("video2dvd: encoder does not support separate media streams, track selection, several audio tracks or subtitles")
		}
		if err != nil {
			return fmt.Errorf("video2dvd: encode input %d (%q): %w", i+1, input.VideoPath, err)
		}
		if len(title.Audio) > 0 && title.AudioPath == "" && title.AudioCodec == "" {
			title.AudioPath, title.AudioCodec = title.Audio[0].Path, title.Audio[0].Codec
		}
		if err := validateTitle(title, workDir, profile); err != nil {
			return fmt.Errorf("video2dvd: invalid encoded title for input %d (%q): %w", i+1, input.VideoPath, err)
		}
		title.VideoPath, err = validateGeneratedFile(title.VideoPath, workDir, "video")
		if err != nil {
			return fmt.Errorf("video2dvd: resolve encoded video for input %d: %w", i+1, err)
		}
		title.Audio = slices.Clone(title.Audio)
		for j := range title.Audio {
			title.Audio[j].Path, err = validateGeneratedFile(title.Audio[j].Path, workDir, "audio")
			if err != nil {
				return fmt.Errorf("video2dvd: resolve encoded audio stream %d for input %d: %w", j+1, i+1, err)
			}
		}
		if len(title.Audio) > 0 {
			title.AudioPath, title.AudioCodec = title.Audio[0].Path, title.Audio[0].Codec
		}
		title.AudioPath, err = validateGeneratedFile(title.AudioPath, workDir, "audio")
		if err != nil {
			return fmt.Errorf("video2dvd: resolve encoded audio for input %d: %w", i+1, err)
		}
		if title.ShowSubtitles == nil && input.ShowSubtitles != nil {
			show := *input.ShowSubtitles
			title.ShowSubtitles = &show
		}
		title.Subtitles = slices.Clone(title.Subtitles)
		for j := range title.Subtitles {
			title.Subtitles[j].Path, err = validateGeneratedSubtitle(title.Subtitles[j].Path, workDir)
			if err != nil {
				return fmt.Errorf("video2dvd: resolve encoded subtitle stream %d for input %d: %w", j+1, i+1, err)
			}
		}
		if title.Name == "" {
			title.Name = mediaTitleName(inputs[i].VideoPath)
		}
		titles = append(titles, title)
		// Fail before encoding the remaining inputs once the streams so far
		// can no longer fit; multiplexing only adds to their size.
		paths := []string{title.VideoPath}
		for _, audio := range titleAudioStreams(title) {
			paths = append(paths, audio.Path)
		}
		for _, path := range paths {
			stat, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("video2dvd: measure encoded input %d: %w", i+1, err)
			}
			encodedBytes += stat.Size()
		}
		for _, subtitle := range title.Subtitles {
			sectors, err := measureDVDSubtitleStream(subtitle.Path)
			if err != nil {
				return fmt.Errorf("video2dvd: measure encoded subtitles of input %d: %w", i+1, err)
			}
			encodedBytes += sectors * DVDVOBSectorSize
		}
		if encodedBytes > profile.DiscCapacityBytes {
			return fmt.Errorf("%w: the first %d of %d inputs already need %d bytes, limit is %d", ErrDiscCapacity, i+1, len(inputs), encodedBytes, profile.DiscCapacityBytes)
		}
	}

	dvdRoot := filepath.Join(stageRoot, "dvd")
	videoTS := filepath.Join(dvdRoot, "VIDEO_TS")
	if err := os.Mkdir(dvdRoot, 0o700); err != nil {
		return fmt.Errorf("video2dvd: create DVD root directory: %w", err)
	}
	if err := os.Mkdir(videoTS, 0o700); err != nil {
		return fmt.Errorf("video2dvd: create VIDEO_TS directory: %w", err)
	}
	if err := c.authorer.Author(ctx, titles, videoTS, profile); err != nil {
		return fmt.Errorf("video2dvd: author DVD-Video: %w", err)
	}
	if err := validateDVDDirectory(videoTS, len(titles)); err != nil {
		return err
	}
	usedBytes, err := directorySize(videoTS)
	if err != nil {
		return fmt.Errorf("video2dvd: measure authored DVD: %w", err)
	}
	if usedBytes > profile.DiscCapacityBytes {
		return fmt.Errorf("%w: %d bytes used, limit is %d", ErrDiscCapacity, usedBytes, profile.DiscCapacityBytes)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(outputDir); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, outputDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("video2dvd: inspect output path before publish: %w", err)
	}
	if err := os.Rename(dvdRoot, outputDir); err != nil {
		return fmt.Errorf("video2dvd: publish DVD-Video directory: %w", err)
	}
	return nil
}

// ConvertISO converts inputs into a DVD-Video UDF bridge ISO image. The image
// contains both ISO 9660 and UDF 1.02 directory views over the same VIDEO_TS
// file extents and is published only after authoring and image creation succeed.
func (c *Converter) ConvertISO(ctx context.Context, inputs []string, outputISO string, options Options) error {
	mediaInputs := make([]MediaInput, len(inputs))
	for i, input := range inputs {
		mediaInputs[i] = MediaInput{VideoPath: input}
	}
	return c.ConvertSourcesISO(ctx, mediaInputs, outputISO, options)
}

// ConvertSourcesISO creates a DVD-Video UDF bridge image from media sources.
func (c *Converter) ConvertSourcesISO(ctx context.Context, inputs []MediaInput, outputISO string, options Options) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	profile, err := options.profile()
	if err != nil {
		return err
	}
	// Reject a bad volume name now rather than after every input is encoded.
	if _, _, err := normalizeDVDImageOptions(DVDImageOptions{VolumeID: options.VolumeID, DiscCapacityBytes: profile.DiscCapacityBytes}); err != nil {
		return err
	}
	outputISO, err = filepath.Abs(outputISO)
	if err != nil {
		return fmt.Errorf("video2dvd: resolve image path: %w", err)
	}
	if _, err := os.Lstat(outputISO); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, outputISO)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("video2dvd: inspect image path: %w", err)
	}
	parent := filepath.Dir(outputISO)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("video2dvd: inspect image parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("video2dvd: image parent %q is not a directory", parent)
	}
	stageRoot, err := os.MkdirTemp(parent, ".video2dvd-iso-")
	if err != nil {
		return fmt.Errorf("video2dvd: create conversion staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	discDir := filepath.Join(stageRoot, "disc")
	if err := c.ConvertSources(ctx, inputs, discDir, options); err != nil {
		return err
	}
	stagedISO := filepath.Join(stageRoot, "disc.iso")
	if err := WriteDVDISOWithOptions(ctx, filepath.Join(discDir, "VIDEO_TS"), stagedISO, DVDImageOptions{VolumeID: options.VolumeID, DiscCapacityBytes: profile.DiscCapacityBytes}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stagedISO, outputISO); err != nil {
		return fmt.Errorf("video2dvd: publish DVD image: %w", err)
	}
	return nil
}

// mediaTitleName returns the menu label of an input: the base name of its
// video file without the extension.
func mediaTitleName(videoPath string) string {
	name := filepath.Base(videoPath)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// validateMediaInputs checks the inputs and resolves what the options leave
// to them: the audio and subtitle tracks to record and whether subtitles
// start shown. Each input's audio must leave the video enough of the
// multiplex rate.
func validateMediaInputs(inputs []MediaInput, options Options, profile Profile) ([]MediaInput, error) {
	validated := make([]MediaInput, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for i, input := range inputs {
		videoPath, err := resolveMediaPath(input.VideoPath, i+1, "video")
		if err != nil {
			return nil, err
		}
		audioPath := input.AudioPath
		if strings.TrimSpace(audioPath) == "" {
			audioPath = videoPath
		} else {
			audioPath, err = resolveMediaPath(audioPath, i+1, "audio")
			if err != nil {
				return nil, err
			}
		}
		videoInfo, err := ProbeMedia(videoPath)
		if err != nil {
			return nil, fmt.Errorf("video2dvd: inspect video input %d (%q): %w", i+1, input.VideoPath, err)
		}
		if _, err := selectInputVideoTrack(videoInfo, input.VideoTrackIndex); err != nil {
			return nil, fmt.Errorf("video2dvd: video input %d (%q): %w", i+1, input.VideoPath, err)
		}
		audioInfo := videoInfo
		if audioPath != videoPath {
			audioInfo, err = ProbeMedia(audioPath)
			if err != nil {
				return nil, fmt.Errorf("video2dvd: inspect audio input %d (%q): %w", i+1, input.AudioPath, err)
			}
		}
		if input.AudioTrackIndex != nil && input.AudioTracks != nil {
			return nil, fmt.Errorf("video2dvd: audio input %d sets both AudioTrackIndex and AudioTracks", i+1)
		}
		if input.AudioTrackIndex != nil || audioPath != videoPath {
			if _, err := selectInputAudioTrack(audioInfo, input.AudioTrackIndex); err != nil && input.AudioTracks == nil {
				return nil, fmt.Errorf("video2dvd: audio input %d (%q): %w", i+1, audioPath, err)
			}
		}
		// An input that names its one track keeps it whatever IncludeAllAudio
		// says.
		audioTracks, err := selectInputAudioTracks(audioInfo, input.AudioTracks, options.IncludeAllAudio && input.AudioTrackIndex == nil)
		if err != nil {
			return nil, fmt.Errorf("video2dvd: audio input %d (%q): %w", i+1, audioPath, err)
		}
		input.AudioTracks = audioTracks
		if streams := len(audioTracks); streams > 1 {
			codecs := slices.Repeat([]string{titleAudioCodec(profile.AudioFormat, streams)}, streams)
			if _, err := titleVideoBitrate(profile, codecs); err != nil {
				return nil, fmt.Errorf("video2dvd: input %d: %w", i+1, err)
			}
		}
		subtitles, err := selectInputSubtitleTracks(videoInfo, input.SubtitleTracks, options.IncludeSubtitles)
		if err != nil {
			return nil, fmt.Errorf("video2dvd: video input %d (%q): %w", i+1, input.VideoPath, err)
		}
		input.SubtitleTracks = subtitles
		if input.ShowSubtitles == nil && options.ShowDefaultSubtitles {
			show := len(subtitles) > 0 && hasDefaultMark(videoInfo, videoInfo.Tracks[subtitles[0]])
			input.ShowSubtitles = &show
		}
		key := videoPath + "\x00" + audioPath
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("video2dvd: input %d duplicates an earlier video/audio source pair", i+1)
		}
		seen[key] = struct{}{}
		input.VideoPath = videoPath
		input.AudioPath = audioPath
		validated[i] = input
	}
	return validated, nil
}

func resolveMediaPath(path string, inputNumber int, kind string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("video2dvd: input %d has an empty %s path", inputNumber, kind)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("video2dvd: resolve %s input %d: %w", kind, inputNumber, err)
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("video2dvd: resolve %s input %d (%q): %w", kind, inputNumber, path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("video2dvd: inspect %s input %d (%q): %w", kind, inputNumber, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("video2dvd: %s input %d (%q) is not a regular file", kind, inputNumber, path)
	}
	return resolved, nil
}

func validateTitle(title EncodedTitle, workDir string, profile Profile) error {
	if title.VideoCodec != VideoCodecMPEG2 {
		return fmt.Errorf("video codec must be %q, got %q", VideoCodecMPEG2, title.VideoCodec)
	}
	if title.Aspect != profile.Aspect {
		return fmt.Errorf("aspect ratio must be %v, got %v", profile.Aspect, title.Aspect)
	}
	switch title.AudioCodec {
	case AudioCodecAC3, AudioCodecLPCM, AudioCodecMPEG1Layer2:
	default:
		return fmt.Errorf("unsupported DVD audio codec %q", title.AudioCodec)
	}
	if !validRaster(profile.Standard, title.Width, title.Height) {
		return fmt.Errorf("raster %dx%d is not valid for the selected DVD standard", title.Width, title.Height)
	}
	if title.FrameRateNumerator <= 0 || title.FrameRateDenominator <= 0 || !validFrameRate(profile.Standard, title.FrameRateNumerator, title.FrameRateDenominator) {
		if profile.Standard == VideoStandardNTSC && title.FrameRateNumerator == 24000 && title.FrameRateDenominator == 1001 {
			// Accepted once, although it never made a DVD-Video stream.
			return checkDVDFrameRate(title.FrameRateNumerator, title.FrameRateDenominator)
		}
		return fmt.Errorf("frame rate %d/%d is not valid for the selected DVD standard", title.FrameRateNumerator, title.FrameRateDenominator)
	}
	if title.Duration <= 0 {
		return errors.New("duration must be greater than zero")
	}
	videoPath, err := validateGeneratedFile(title.VideoPath, workDir, "video")
	if err != nil {
		return err
	}
	audioPath, err := validateGeneratedFile(title.AudioPath, workDir, "audio")
	if err != nil {
		return err
	}
	if videoPath == audioPath {
		return errors.New("video and audio streams must be separate files")
	}
	if len(title.Audio) > dvdMaxAudioStreams {
		return fmt.Errorf("a title has at most %d audio streams, got %d", dvdMaxAudioStreams, len(title.Audio))
	}
	for i, audio := range title.Audio {
		if audio.Codec != AudioCodecLPCM && audio.Codec != AudioCodecAC3 {
			return fmt.Errorf("audio stream %d codec must be %q or %q, got %q", i+1, AudioCodecLPCM, AudioCodecAC3, audio.Codec)
		}
		if !validDVDLanguage(audio.Language) {
			return fmt.Errorf("audio stream %d language %q is not an ISO 639-1 code", i+1, audio.Language)
		}
		path, err := validateGeneratedFile(audio.Path, workDir, "audio")
		if err != nil {
			return fmt.Errorf("audio stream %d: %w", i+1, err)
		}
		if path == videoPath {
			return errors.New("video and audio streams must be separate files")
		}
		if i == 0 && (path != audioPath || audio.Codec != title.AudioCodec) {
			return errors.New("AudioPath and AudioCodec must be those of the first stream of Audio")
		}
		if audio.Codec == AudioCodecAC3 {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, _, err = probeAC3Stream(f)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("audio stream %d: %w", i+1, err)
			}
		}
	}
	if len(title.Subtitles) > dvdSubtitleMaxStreams {
		return fmt.Errorf("a title has at most %d subtitle streams, got %d", dvdSubtitleMaxStreams, len(title.Subtitles))
	}
	for i, subtitle := range title.Subtitles {
		if !validDVDLanguage(subtitle.Language) {
			return fmt.Errorf("subtitle stream %d language %q is not an ISO 639-1 code", i+1, subtitle.Language)
		}
		path, err := validateGeneratedSubtitle(subtitle.Path, workDir)
		if err != nil {
			return fmt.Errorf("subtitle stream %d: %w", i+1, err)
		}
		if path == videoPath || path == audioPath {
			return fmt.Errorf("subtitle stream %d must be a file of its own", i+1)
		}
		if _, err := measureDVDSubtitleStream(path); err != nil {
			return fmt.Errorf("subtitle stream %d: %w", i+1, err)
		}
	}
	videoInfo, err := ProbeMPEG2Video(videoPath)
	if err != nil {
		return fmt.Errorf("MPEG-2 video stream: %w", err)
	}
	if videoInfo.Width != title.Width || videoInfo.Height != title.Height {
		return fmt.Errorf("MPEG-2 sequence dimensions %dx%d do not match declared %dx%d", videoInfo.Width, videoInfo.Height, title.Width, title.Height)
	}
	if videoInfo.Aspect != title.Aspect {
		return errors.New("MPEG-2 sequence aspect ratio does not match the declared aspect ratio")
	}
	if videoInfo.FrameRateNumerator*title.FrameRateDenominator != title.FrameRateNumerator*videoInfo.FrameRateDenominator {
		return fmt.Errorf("MPEG-2 sequence frame rate %d/%d does not match declared %d/%d", videoInfo.FrameRateNumerator, videoInfo.FrameRateDenominator, title.FrameRateNumerator, title.FrameRateDenominator)
	}
	return nil
}

func validRaster(standard VideoStandard, width, height int) bool {
	if standard == VideoStandardNTSC {
		return height == 480 && (width == 720 || width == 704 || width == 352)
	}
	if standard == VideoStandardPAL {
		return height == 576 && (width == 720 || width == 704 || width == 352)
	}
	return false
}

func validFrameRate(standard VideoStandard, numerator, denominator int) bool {
	if standard == VideoStandardPAL {
		return numerator == 25 && denominator == 1
	}
	if standard == VideoStandardNTSC {
		return numerator == 30000 && denominator == 1001
	}
	return false
}

// validateGeneratedSubtitle resolves a subtitle stream file, which unlike
// the other streams may be empty.
func validateGeneratedSubtitle(path, workDir string) (string, error) {
	return resolveGeneratedFile(path, workDir, "subtitle", true)
}

func validateGeneratedFile(path, workDir, kind string) (string, error) {
	return resolveGeneratedFile(path, workDir, kind, false)
}

func resolveGeneratedFile(path, workDir, kind string, mayBeEmpty bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s stream path is empty", kind)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s stream path: %w", kind, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve %s stream file: %w", kind, err)
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve encoding work directory: %w", err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("%s stream file must be inside the encoding work directory", kind)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect %s stream file: %w", kind, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 && !mayBeEmpty {
		return "", fmt.Errorf("%s stream must be a non-empty regular file", kind)
	}
	return resolved, nil
}

// measureDVDSubtitleStream checks a subtitle stream file and returns the
// sectors that multiplexing it takes at most.
func measureDVDSubtitleStream(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return dvdSubtitleStreamSectors(f)
}

// validDVDLanguage reports whether a language is empty or a two-letter
// ISO 639-1 code in lower case.
func validDVDLanguage(language string) bool {
	return language == "" || len(language) == 2 && DVDLanguageCode(language) == language
}

func validateDVDDirectory(videoTS string, expectedVTSCount int) error {
	if err := validateIFOBackup(videoTS, "VIDEO_TS.IFO", "VIDEO_TS.BUP", "DVDVIDEO-VMG"); err != nil {
		return err
	}
	vtsCount := 0
	for number := 1; number <= dvdMaxTitles; number++ {
		stem := fmt.Sprintf("VTS_%02d_0", number)
		_, err := os.Lstat(filepath.Join(videoTS, stem+".IFO"))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("video2dvd: inspect %s.IFO: %w", stem, err)
		}
		if err := validateIFOBackup(videoTS, stem+".IFO", stem+".BUP", "DVDVIDEO-VTS"); err != nil {
			return err
		}
		vobName := fmt.Sprintf("VTS_%02d_1.VOB", number)
		vobInfo, err := os.Lstat(filepath.Join(videoTS, vobName))
		if err != nil {
			return fmt.Errorf("video2dvd: authorer did not create %s: %w", vobName, err)
		}
		if !vobInfo.Mode().IsRegular() || vobInfo.Size() == 0 {
			return fmt.Errorf("video2dvd: authored %s must be a non-empty regular file", vobName)
		}
		vtsCount++
	}
	if vtsCount != expectedVTSCount {
		return fmt.Errorf("video2dvd: authorer created %d title sets; want %d", vtsCount, expectedVTSCount)
	}
	if err := validateVOBs(videoTS); err != nil {
		return err
	}
	return nil
}

func validateIFOBackup(videoTS, ifoName, bupName, signatureText string) error {
	ifoPath := filepath.Join(videoTS, ifoName)
	bupPath := filepath.Join(videoTS, bupName)
	ifoInfo, err := os.Lstat(ifoPath)
	if err != nil {
		return fmt.Errorf("video2dvd: authorer did not create %s: %w", ifoName, err)
	}
	bupInfo, err := os.Lstat(bupPath)
	if err != nil {
		return fmt.Errorf("video2dvd: authorer did not create %s: %w", bupName, err)
	}
	if !ifoInfo.Mode().IsRegular() || ifoInfo.Size() == 0 || ifoInfo.Size()%DVDVOBSectorSize != 0 {
		return fmt.Errorf("video2dvd: authored %s must be a non-empty sector-aligned regular file", ifoName)
	}
	if !bupInfo.Mode().IsRegular() || bupInfo.Size() == 0 || bupInfo.Size()%DVDVOBSectorSize != 0 {
		return fmt.Errorf("video2dvd: authored %s must be a non-empty sector-aligned regular file", bupName)
	}
	ifo, err := os.Open(ifoPath)
	if err != nil {
		return err
	}
	var signature [12]byte
	_, readErr := io.ReadFull(ifo, signature[:])
	closeErr := ifo.Close()
	if readErr != nil {
		return fmt.Errorf("video2dvd: read %s header: %w", ifoName, readErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if string(signature[:]) != signatureText {
		return fmt.Errorf("video2dvd: %s has an invalid DVD-Video signature", ifoName)
	}
	if ifoInfo.Size() != bupInfo.Size() {
		return fmt.Errorf("video2dvd: %s must be the same size as %s", bupName, ifoName)
	}
	equal, err := filesEqual(ifoPath, bupPath, ifoInfo.Size())
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("video2dvd: %s must be an exact backup of %s", bupName, ifoName)
	}
	return nil
}

func filesEqual(first, second string, size int64) (bool, error) {
	a, err := os.Open(first)
	if err != nil {
		return false, err
	}
	defer a.Close()
	b, err := os.Open(second)
	if err != nil {
		return false, err
	}
	defer b.Close()
	const chunkSize = 32 * 1024
	left := make([]byte, chunkSize)
	right := make([]byte, chunkSize)
	for offset := int64(0); offset < size; {
		count := min(int64(chunkSize), size-offset)
		if _, err := io.ReadFull(io.NewSectionReader(a, offset, count), left[:count]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(io.NewSectionReader(b, offset, count), right[:count]); err != nil {
			return false, err
		}
		if !bytes.Equal(left[:count], right[:count]) {
			return false, nil
		}
		offset += count
	}
	return true, nil
}

// dvdVOBFileMaxBytes is the largest VOB file DVD-Video allows: 1 GiB less
// one sector.
const dvdVOBFileMaxBytes = 1<<30 - DVDVOBSectorSize

func validateVOBs(videoTS string) error {
	return filepath.WalkDir(videoTS, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToUpper(entry.Name()), ".VOB") {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size()%2048 != 0 || info.Size() > dvdVOBFileMaxBytes {
			return fmt.Errorf("video2dvd: %s has an invalid DVD VOB size", entry.Name())
		}
		return validateVOBPacks(path, entry.Name(), info.Size())
	})
}

func validateVOBPacks(path, name string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	const sectorsPerChunk = 64
	const sectorSize = 2048
	buffer := make([]byte, sectorsPerChunk*sectorSize)
	for offset := int64(0); offset < size; {
		chunkSize := min(int64(len(buffer)), size-offset)
		chunk := buffer[:int(chunkSize)]
		if _, err := io.ReadFull(io.NewSectionReader(file, offset, chunkSize), chunk); err != nil {
			return fmt.Errorf("video2dvd: read %s packs at sector offset %d: %w", name, offset, err)
		}
		for position := 0; position < len(chunk); position += sectorSize {
			if chunk[position] != 0 || chunk[position+1] != 0 || chunk[position+2] != 1 || chunk[position+3] != 0xba {
				return fmt.Errorf("video2dvd: %s has no MPEG pack start at offset %d", name, offset+int64(position))
			}
		}
		offset += chunkSize
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("authored DVD contains a symbolic link: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return fmt.Errorf("authored DVD contains a non-regular file: %s", path)
		}
		if info.Size() > int64(^uint64(0)>>1)-total {
			return errors.New("authored DVD size overflows int64")
		}
		total += info.Size()
		return nil
	})
	return total, err
}
