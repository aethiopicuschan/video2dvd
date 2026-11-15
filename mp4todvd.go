// Package mp4todvd provides APIs for preparing DVD-compatible media and
// orchestrating DVD-Video directory creation from multiple MP4 files.
//
// The package itself does not start external processes. It includes a basic
// in-process Encoder; DVD directory generation still requires an Authorer.
package mp4todvd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNoInputs       = errors.New("mp4todvd: at least one input file is required")
	ErrOutputExists   = errors.New("mp4todvd: output path already exists")
	ErrInvalidProfile = errors.New("mp4todvd: invalid DVD profile")
	ErrDiscCapacity   = errors.New("mp4todvd: authored DVD exceeds configured capacity")
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
}

// Options controls conversion and DVD authoring. The zero value selects NTSC,
// 16:9, and the full-resolution DVD raster for that standard.
type Options struct {
	Standard          VideoStandard
	Aspect            AspectRatio
	DiscCapacityBytes int64
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

	switch o.Standard {
	case VideoStandardNTSC:
		return Profile{Standard: o.Standard, Aspect: o.Aspect, FrameWidth: 720, FrameHeight: 480, DiscCapacityBytes: o.DiscCapacityBytes}, nil
	case VideoStandardPAL:
		return Profile{Standard: o.Standard, Aspect: o.Aspect, FrameWidth: 720, FrameHeight: 576, DiscCapacityBytes: o.DiscCapacityBytes}, nil
	default:
		return Profile{}, ErrInvalidProfile
	}
}

// EncodedTitle describes DVD-compatible elementary streams produced for one
// input. Relative paths are resolved from the work directory passed to
// Encoder.Encode. Both streams must be non-empty files inside that directory.
// Video is MPEG-2; audio must be AC-3, LPCM, or MPEG-1 Layer II. The authorer
// may use the duration and frame rate to build navigation data.
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
}

const (
	VideoCodecMPEG2       = "mpeg2video"
	AudioCodecAC3         = "ac3"
	AudioCodecLPCM        = "lpcm"
	AudioCodecMPEG1Layer2 = "mp2"
)

// Encoder decodes and encodes one MP4 into DVD-compatible elementary streams.
// Implementations should honor context cancellation and keep generated files
// inside workDir. It is called once for each input, in input order.
type Encoder interface {
	Encode(ctx context.Context, inputPath, workDir string, profile Profile) (EncodedTitle, error)
}

// Authorer creates a DVD-Video directory (including VIDEO_TS.IFO/BUP and VOB
// files) from the encoded titles. outputDir is an empty directory. Implementors
// should honor context cancellation.
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
		return nil, errors.New("mp4todvd: encoder is required")
	}
	if authorer == nil {
		return nil, errors.New("mp4todvd: authorer is required")
	}
	return &Converter{encoder: encoder, authorer: authorer}, nil
}

// Convert turns inputs into a DVD-Video directory at outputDir. outputDir must
// not already exist; it will contain a VIDEO_TS subdirectory on success.
func (c *Converter) Convert(ctx context.Context, inputs []string, outputDir string, options Options) error {
	if c == nil || c.encoder == nil || c.authorer == nil {
		return errors.New("mp4todvd: converter is not configured")
	}
	if ctx == nil {
		return errors.New("mp4todvd: context must not be nil")
	}
	if len(inputs) == 0 {
		return ErrNoInputs
	}
	profile, err := options.profile()
	if err != nil {
		return err
	}

	inputPaths, err := validateInputs(inputs)
	if err != nil {
		return err
	}
	outputDir, err = filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("mp4todvd: resolve output path: %w", err)
	}
	parent := filepath.Dir(outputDir)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("mp4todvd: inspect output parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("mp4todvd: output parent %q is not a directory", parent)
	}
	if _, err := os.Lstat(outputDir); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, outputDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mp4todvd: inspect output path: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	stageRoot, err := os.MkdirTemp(parent, ".mp4todvd-")
	if err != nil {
		return fmt.Errorf("mp4todvd: create staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	encodedRoot := filepath.Join(stageRoot, "encoded")
	if err := os.Mkdir(encodedRoot, 0o700); err != nil {
		return fmt.Errorf("mp4todvd: create encoded directory: %w", err)
	}
	titles := make([]EncodedTitle, 0, len(inputPaths))
	for i, inputPath := range inputPaths {
		if err := ctx.Err(); err != nil {
			return err
		}
		workDir := filepath.Join(encodedRoot, fmt.Sprintf("title-%03d", i+1))
		if err := os.Mkdir(workDir, 0o700); err != nil {
			return fmt.Errorf("mp4todvd: create work directory for input %d: %w", i+1, err)
		}
		title, err := c.encoder.Encode(ctx, inputPath, workDir, profile)
		if err != nil {
			return fmt.Errorf("mp4todvd: encode input %d (%q): %w", i+1, inputPath, err)
		}
		if err := validateTitle(title, workDir, profile); err != nil {
			return fmt.Errorf("mp4todvd: invalid encoded title for input %d (%q): %w", i+1, inputPath, err)
		}
		titles = append(titles, title)
	}

	dvdRoot := filepath.Join(stageRoot, "dvd")
	videoTS := filepath.Join(dvdRoot, "VIDEO_TS")
	if err := os.Mkdir(dvdRoot, 0o700); err != nil {
		return fmt.Errorf("mp4todvd: create DVD root directory: %w", err)
	}
	if err := os.Mkdir(videoTS, 0o700); err != nil {
		return fmt.Errorf("mp4todvd: create VIDEO_TS directory: %w", err)
	}
	if err := c.authorer.Author(ctx, titles, videoTS, profile); err != nil {
		return fmt.Errorf("mp4todvd: author DVD-Video: %w", err)
	}
	if err := validateDVDDirectory(videoTS); err != nil {
		return err
	}
	usedBytes, err := directorySize(videoTS)
	if err != nil {
		return fmt.Errorf("mp4todvd: measure authored DVD: %w", err)
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
		return fmt.Errorf("mp4todvd: inspect output path before publish: %w", err)
	}
	if err := os.Rename(dvdRoot, outputDir); err != nil {
		return fmt.Errorf("mp4todvd: publish DVD-Video directory: %w", err)
	}
	return nil
}

func validateInputs(inputs []string) ([]string, error) {
	paths := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for i, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return nil, fmt.Errorf("mp4todvd: input %d has an empty path", i+1)
		}
		path, err := filepath.Abs(input)
		if err != nil {
			return nil, fmt.Errorf("mp4todvd: resolve input %d: %w", i+1, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("mp4todvd: resolve input %d (%q): %w", i+1, input, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("mp4todvd: inspect input %d (%q): %w", i+1, input, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("mp4todvd: input %d (%q) is not a regular file", i+1, input)
		}
		movie, err := ProbeMP4(resolved)
		if err != nil {
			return nil, fmt.Errorf("mp4todvd: inspect input %d (%q): %w", i+1, input, err)
		}
		hasVideo := false
		for _, track := range movie.Tracks {
			if track.Kind == TrackKindVideo {
				hasVideo = true
				break
			}
		}
		if !hasVideo {
			return nil, fmt.Errorf("mp4todvd: input %d (%q) has no video track", i+1, input)
		}
		if _, ok := seen[resolved]; ok {
			return nil, fmt.Errorf("mp4todvd: input %d (%q) duplicates an earlier input", i+1, input)
		}
		seen[resolved] = struct{}{}
		paths = append(paths, resolved)
	}
	return paths, nil
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
		return (numerator == 30000 && denominator == 1001) || (numerator == 24000 && denominator == 1001)
	}
	return false
}

func validateGeneratedFile(path, workDir, kind string) (string, error) {
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
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("%s stream must be a non-empty regular file", kind)
	}
	return resolved, nil
}

func validateDVDDirectory(videoTS string) error {
	required := []string{"VIDEO_TS.IFO", "VIDEO_TS.BUP", "VTS_01_0.IFO", "VTS_01_0.BUP", "VTS_01_1.VOB"}
	files := make(map[string]fs.FileInfo, len(required))
	for _, name := range required {
		path := filepath.Join(videoTS, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("mp4todvd: authorer did not create %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("mp4todvd: authored %s must be a non-empty regular file", name)
		}
		files[name] = info
	}
	for _, pair := range [][3]string{
		{"VIDEO_TS.IFO", "VIDEO_TS.BUP", "DVDVIDEO-VMG"},
		{"VTS_01_0.IFO", "VTS_01_0.BUP", "DVDVIDEO-VTS"},
	} {
		ifoPath := filepath.Join(videoTS, pair[0])
		bupPath := filepath.Join(videoTS, pair[1])
		ifoInfo, bupInfo := files[pair[0]], files[pair[1]]
		if ifoInfo.Size()%2048 != 0 || bupInfo.Size()%2048 != 0 {
			return fmt.Errorf("mp4todvd: %s and %s must be DVD-sector aligned", pair[0], pair[1])
		}
		ifo, err := os.Open(ifoPath)
		if err != nil {
			return err
		}
		var signature [12]byte
		_, readErr := io.ReadFull(ifo, signature[:])
		closeErr := ifo.Close()
		if readErr != nil {
			return fmt.Errorf("mp4todvd: read %s header: %w", pair[0], readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if string(signature[:]) != pair[2] {
			return fmt.Errorf("mp4todvd: %s has an invalid DVD-Video signature", pair[0])
		}
		equal, err := filesEqual(ifoPath, bupPath, ifoInfo.Size())
		if err != nil {
			return err
		}
		if !equal || ifoInfo.Size() != bupInfo.Size() {
			return fmt.Errorf("mp4todvd: %s must be an exact backup of %s", pair[1], pair[0])
		}
	}
	if err := validateVOBs(videoTS); err != nil {
		return err
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
		count := int64(chunkSize)
		if size-offset < count {
			count = size - offset
		}
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
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size()%2048 != 0 || info.Size() > 1<<30 {
			return fmt.Errorf("mp4todvd: %s has an invalid DVD VOB size", entry.Name())
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
		chunkSize := int64(len(buffer))
		if size-offset < chunkSize {
			chunkSize = size - offset
		}
		chunk := buffer[:int(chunkSize)]
		if _, err := io.ReadFull(io.NewSectionReader(file, offset, chunkSize), chunk); err != nil {
			return fmt.Errorf("mp4todvd: read %s packs at sector offset %d: %w", name, offset, err)
		}
		for position := 0; position < len(chunk); position += sectorSize {
			if chunk[position] != 0 || chunk[position+1] != 0 || chunk[position+2] != 1 || chunk[position+3] != 0xba {
				return fmt.Errorf("mp4todvd: %s has no MPEG pack start at offset %d", name, offset+int64(position))
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
