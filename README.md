# video2dvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/video2dvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/video2dvd)
[![CI](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/video2dvd/graph/badge.svg?token=gV5qlDuFTw)](https://codecov.io/gh/aethiopicuschan/video2dvd)

A Go library for creating DVD-Video directories and ISO images from multiple MP4 and WebM files. It does not launch external commands such as FFmpeg.

## Current implementation

`Converter` validates input files and coordinates per-clip conversion, DVD authoring, and output publishing. Intermediate files are staged in a temporary directory beside the destination. The `VIDEO_TS` directory is moved to the destination only after all processing succeeds.

`ProbeMedia` detects MP4 and WebM files and reads their tracks, codecs, resolutions, audio information, and durations. You can also use `ProbeMP4` and `ProbeWebM` directly. `DemuxMP4` reads audio and video samples from non-fragmented MP4 files in order, including their DTS and PTS values. The library can read H.264 AVC configuration and convert NAL units and SPS/PPS to Annex B using `AVCToAnnexB` and `AVCConfigToAnnexB`. It can also read AAC AudioSpecificConfig data.

`DecodeMP4Video` decodes progressive 8-bit 4:2:0 H.264 into YUV frames. `DecodeMP4Audio` decodes supported AAC-LC (mono or stereo) and AC-3 audio into 48 kHz stereo PCM on the movie timeline: a leading empty edit becomes silence, and the first media edit's start and length are applied rounded to whole samples, so edit lists written on a millisecond movie timescale are accepted. If the primary MP4 AAC decoder rejects an access unit because of non-zero byte-alignment padding, a pure-Go AAC-LC fallback decodes MP4 access units via ADTS framing. The fallback applies the same edit-list timing and sample-rate conversion, and buffers decoded PCM to a temporary file before calling the PCM handler. `DecodeWebMVideo` decodes VP9 Profile 0/2, VP8, and AV1 video; `DecodeWebMAudio` decodes Opus audio into DVD-ready frames and PCM. VP9 Profile 2 10/12-bit video is converted to 8-bit. VP9 Profiles 0 and 2 use [`github.com/aethiopicuschan/go-vp9`](https://github.com/aethiopicuschan/go-vp9), which maintains VP9 reference state and processes superframes. The decoders are implemented in pure Go and do not launch external commands such as FFmpeg.

The built-in converter supports MP4 with H.264 and AAC-LC/AC-3, and WebM with VP8, VP9 Profile 0/2, or AV1 video and Opus audio. VP9 Profiles 1/3, WebM Vorbis, high-bit-depth AV1, HDR tone mapping, and MP4 video codecs other than AVC are not supported. VP9 Profile 2 bit depth is converted, but BT.2020/PQ/HLG video is not converted to DVD-compatible SDR.

`PrepareMP4VideoForDVD` and `PrepareWebMVideoForDVD` fit input video to the DVD raster and convert it to NTSC 29.97 fps or PAL 25 fps. The input display aspect ratio must be provided explicitly. When an MP4 video track has an edit list, output starts at movie time zero: frames before the first media edit are skipped and a leading empty edit holds the first frame, which keeps video aligned with the audio track's edits. Frames are resampled with a Lanczos-3 filter. Sources whose colour matrix is BT.709 (tagged in the container or bitstream, or untagged and larger than 1024x576) are converted to the BT.601 matrix that DVD players assume, and sources explicitly tagged full range are compressed to studio range; without this step greens and reds of HD sources are visibly off. WebM timestamps rounded to milliseconds are restored to an even cadence before frame-rate conversion, so 30 and 60 fps sources do not stutter. A 1080p or 4K source still looks softer on DVD than the original: 720x480 or 720x576 is the format's resolution, and the fair comparison is the source scaled to that size. These two functions always produce the video frame rate, so a 24 fps source comes out with every fourth frame repeated; the built-in encoder treats such sources as film instead (see [Film sources](#film-sources-soft-telecine)). For lower-level processing, use `FitVideoFrame` (resampling only), `FitVideoFrameForDVD` (resampling and colour conversion, using `DecodedVideoFrame.Color`), `DVDFrameRateConverter`, and `NewDVDFilmFrameRateConverter`. `PrepareMP4AudioForDVD` and `PrepareWebMAudioForDVD` convert audio to 48 kHz stereo LPCM data. `DVDLPCMPacketWriter` wraps PCM chunks in DVD LPCM PES packets and writes them into 2048-byte program-stream sectors.

`NewInProcessEncoder` creates MPEG-2 video and LPCM audio from MP4 or WebM without external commands. Requested video bitrates above 7.5 Mbit/s, including the 8 Mbit/s default, are coded and declared at 7.5 Mbit/s so that video, LPCM audio, and pack overhead fit the 10.08 Mbit/s DVD multiplex rate. `MediaInput` lets you select separate video and audio files and choose tracks within each container. The title is exactly as long as the encoded video. Audio is shifted by inserting silence or trimming its start according to the requested offset, then padded with silence or trimmed to the video length. Decoded audio chunks are placed by their running sample count; container timestamps only reposition audio when they deviate by more than 20 ms, so nanosecond- or millisecond-rounded timestamps never drop or insert samples. When video and audio come from the same WebM file, the title starts at the first video frame and audio moves with it. Inputs whose audio tracks cannot be decoded (for example MP3 in MP4, or 5.1 AAC/Opus) are rejected before video encoding starts. The MPEG-2 encoder emits I/P frames in closed GOPs of up to 15 pictures (12 or 13 for film) and starts a new GOP at a scene cut. P frames use half-pixel motion prediction over a range of 32 pixels with quantized DCT residuals, code macroblocks without a usable prediction as intra, and use reconstructed frames as references. Rate control follows the VBV model and balances quality within a GOP. The stream is labelled SMPTE 170M (NTSC) or BT.470BG (PAL). The encoder does not use B frames or adaptive quantization, so its compression efficiency is below that of dedicated encoders.

`MuxDVDTitleVOB` multiplexes one title's MPEG-2 video and LPCM audio against a byte-driven system clock: SCR starts at zero and advances with the 10.08 Mbit/s multiplex rate, the first picture is decoded 0.3 s into the stream, and every access unit is delivered ahead of its DTS/PTS within the DVD decoder buffer sizes. Picture timestamps count display fields, two per picture or three where `repeat_first_field` is set, so soft-telecined film is timed correctly. A NAV pack with PCI/DSI, including the VOBU search tables and audio sync pointers, starts each VOBU at an I frame. The muxer reads the elementary streams twice (a planning pass, then the writing pass) and returns an error before writing anything if the video cannot be delivered in time. `MPEGProgramStreamWriter` and `DVDLPCMPacketWriter` are lower-level components, and `BuildDVDNAVPack` creates a standalone NAV pack. Fragmented MP4, encrypted tracks, MP4 edits after the first media edit, and MP4 rotation metadata are outside the current scope. `DemuxMP4` and `DecodeMP4Video` report media timestamps without applying edit lists.

`NewInProcessAuthorer` authors up to 99 inputs as separate VTS/title sets in input order, registering each input as one title and one chapter. The generated title menu in `VIDEO_TS.VOB` lists each title by its input file name (`EncodedTitle.Name`); users can navigate to titles with the DVD remote's arrow and enter buttons, and the selected button is highlighted through a subpicture stream. 16:9 menus carry a second, letterboxed highlight for 4:3 displays. Playback returns to the title menu when a title ends, and the menu loops back to its first page after playback. The 4:3 menu uses two columns and the 16:9 menu uses one column. Menus are automatically split into pages of up to 36 buttons. `Options.MenuTitle`, `MenuBackgroundLuma`, and `MenuTextLuma` configure the menu heading and brightness; the heading and the title names may contain Japanese (see [Menu text](#menu-text)), and labels are truncated to the button width. Subtitles are not supported. The authorer creates VMG/VTS IFO and BUP files, PTT/PGC/cell/VOBU tables, a VTS time map, and each title's VOB, split into `VTS_nn_1.VOB` through `VTS_nn_9.VOB` of at most 1 GiB each, so a title may be up to about 9 GiB. Title sets have no menu VOB of their own (`VTS_nn_0.VOB` is not written). `WriteDVDISO` packages `VIDEO_TS` into a DVD-Video image readable through both ISO 9660 and UDF 1.02. It lays each file set out contiguously (IFO, VOBs, BUP), sets the IFO sector references from that layout relative to the owning IFO, and checks capacity including ISO metadata. Converting to an ISO needs temporary space beside the destination for the elementary streams, the `VIDEO_TS` files, and the image, roughly three times the image size. UDF structures are written using the pure-Go [go-sdk-winmediafoundry UDF package](https://pkg.go.dev/github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf); no external processes are launched. Writing to physical discs is not implemented.

## Film sources (soft telecine)

On NTSC discs the built-in encoder codes a source that runs at 23.976 (24000/1001) or 24 fps throughout as film, the way commercial film DVDs are made. Only the source frames are coded, 23.976 pictures per second, and each picture carries the `top_field_first` and `repeat_first_field` flags that tell the player how to fill the 59.94 fields per second of NTSC: the frames are shown for three and two fields in turn (3:2 pulldown). The stream itself stays a 29.97 fps DVD stream of progressive frame pictures.

- An interlaced player or TV output shows the standard 3:2 cadence, 50 ms and 33 ms per frame alternately, instead of three frames of 33 ms and one of 67 ms.
- Progressive-scan DVD players and software players such as VLC decode the 23.976 film frames themselves and pace them for their display.
- A fifth fewer pictures are coded at the same bitrate, so each one gets about 25% more bits.

Whether a source is film is decided before encoding starts, from the timestamps of the whole video track; they are read from the MP4 sample tables or the WebM blocks without decoding, and millisecond-rounded WebM timestamps are recognised. A track is film when at least nine in ten of its frames follow the one before after one film period and frames never come faster than 24 a second. A frame that stays longer (a held first frame, a gap, a passage at a lower rate) does not change that; it is repeated on the film grid as it would be on the video grid. A track with a passage at a higher rate, such as 24 fps followed by 30 fps, is not film: coded as film it would lose every fifth frame of that passage, so it is converted at the video rate. Sources at 25, 29.97, 30, 50, 59.94, or 60 fps, such variable-frame-rate sources, and all PAL titles are converted exactly as before.

A 23.976 fps source maps frame for frame. A source at exactly 24 fps keeps its real duration, so audio stays in sync without being resampled: one frame in 1001 is left out, the first after 21 seconds and then one every 42 seconds. Each picture is the source frame nearest in time, so video is never more than half a film period (21 ms) early or late against the audio; on an interlaced display 3:2 pulldown itself shows every other frame another 8 ms late.

A film GOP holds 12 pictures, 30 fields, and starts with a top field on a frame of the 29.97 fps display raster. A scene change may start a GOP after 11 pictures (28 fields); the GOPs after it hold 13 (32 fields) until one ends on a scene change after 12, which keeps every GOP on the display raster. The title always ends on a whole display frame, with a bottom field: where the 3:2 cycle would stop after a top field, the last picture is shown for one field more or less. Soft-telecined titles have been checked with ffmpeg, VLC (libdvdnav and libdvdread), and lsdvd, not on a hardware player.

Set `Options.DisableSoftTelecine` (or `Profile.DisableSoftTelecine` when calling an encoder directly) to convert film sources like any other, with every fourth frame shown twice.

`EncodedTitle.FrameRateNumerator/Denominator` remain the display rate of the standard (30000/1001) for film titles. The multiplexer reads the flags of every picture and derives timestamps, VOBU times, and navigation data from the display fields, so MPEG-2 streams soft-telecined by another encoder work as well. A title declared as 24000/1001, or a stream whose sequence header says so, is rejected: that is not a DVD-Video frame rate. (Earlier versions accepted the declaration from an external encoder, but only together with such a stream, and timed it as 23.976 progressive pictures per second, which no DVD player is required to play.) The multiplexer does not alter a stream from another encoder, which may end on a single field. `NewMPEG2SoftTelecineEncoder` and `NewDVDFilmFrameRateConverter` expose the film path for custom pipelines; `MPEG2VideoEncoder.DisplayFields` gives the stream length once the encoder is closed.

## Menu text

Without any configuration the title menu draws ASCII with a built-in 5x7 font and Japanese with an embedded bitmap font, the 16-dot gothic face of the public-domain [Shinonome font family](http://openlab.ring.gr.jp/efont/shinonome/). The embedded font covers JIS X 0208 (hiragana, katakana, level 1 and 2 kanji, full-width letters, digits and symbols, Japanese punctuation) and half-width katakana; its table is 114 KB, and together with the font reader for `MenuFont` it adds about 0.4 MB to a program. It is drawn bold so that its strokes match the ASCII font and stay visible on a television. ASCII letters are shown in upper case; other characters are shown as written. File names that macOS reports in decomposed form (`か` followed by a combining voiced sound mark instead of `が`) are composed first. A character that no font has is shown as `?`.

Labels and the heading are laid out by pixel width: an ASCII character takes 12 pixels, a full-width character 16, and a half-width katakana 8. A label that is wider than its button is cut after the last character that fits, without an ellipsis. A 16:9 menu shows about 35 full-width characters of a title name, and a two-column 4:3 menu about 15.

To draw the menu with another typeface, or in another language, pass a TrueType or OpenType font. Text is then anti-aliased, keeps its upper and lower case, and is scaled horizontally by the inverse of the pixel aspect ratio (10:11 and 40:33 for NTSC 4:3 and 16:9, 12:11 and 16:11 for PAL), so glyphs have their natural proportions on the television instead of being stretched. Characters missing from the font fall back to the built-in fonts one by one, so a Latin-only font still shows Japanese names.

```go
data, err := os.ReadFile("/System/Library/Fonts/ヒラギノ角ゴシック W3.ttc")
if err != nil {
	return err
}
menuFont, err := video2dvd.ParseMenuFont(data) // TTF, OTF, or the first font of a TTC/OTC
if err != nil {
	return err
}
err = converter.Convert(ctx, []string{"夏休みの思い出.mp4"}, "./disc", video2dvd.Options{
	MenuTitle: "家族ビデオ 2024年",
	MenuFont:  menuFont,
})
```

`ParseMenuFontIndex` selects another font of a collection. Rendering uses integer arithmetic only, so a menu picture is identical on every platform. Limits: the font size is fixed (a 16-line em) and the bitmap fonts are not scaled to the pixel aspect ratio, so they look about 21% wider on a 16:9 NTSC display and 9% narrower on a 4:3 one; text is drawn left to right, one glyph per character, without kerning, ligatures, shaping, or vertical forms, which rules out scripts such as Arabic or Devanagari; combining marks other than the kana sound marks are not composed; characters outside JIS X 0208 (emoji, rare kanji, Hangul, accented Latin letters) need a `MenuFont` that has them, except that `ゔ`, `ヷ`, `ヸ`, `ヹ`, and `ヺ` are spelled as the base kana followed by `゛`; marks above capital letters, as in `É`, can lose their top lines to the button rectangle; hinting, bitmap or colour glyphs, and variable-font axes of a `MenuFont` are not used; and `ParseMenuFont` returns an error for fonts its reader (`golang.org/x/image/font/sfnt`) does not accept, which includes some system fonts.

## Connecting the backends

```go
encoder := video2dvd.NewInProcessEncoder()
authorer := video2dvd.NewInProcessAuthorer()

converter, err := video2dvd.New(encoder, authorer)
if err != nil {
	return err
}

err = converter.Convert(ctx,
	[]string{"part-1.mp4", "part-2.mp4"},
	"./disc",
	video2dvd.Options{
		Standard: video2dvd.VideoStandardNTSC,
		Aspect:   video2dvd.AspectRatio16x9,
	},
)
```

Use `ConvertISO` to create an ISO image as well:

```go
err = converter.ConvertISO(ctx,
	[]string{"part-1.mp4", "part-2.mp4"},
	"./disc.iso",
	video2dvd.Options{Standard: video2dvd.VideoStandardNTSC},
)
```

Use `ConvertSources` when a downloader saves video and audio to separate files. Track indexes are zero-based, matching `TrackInfo.Index` returned by `ProbeMedia`. A positive `AudioOffset` inserts silence before the audio; a negative value trims the beginning of the audio.

```go
err = converter.ConvertSources(ctx,
	[]video2dvd.MediaInput{{
		VideoPath:   "video-only.webm",
		AudioPath:   "audio-only.webm",
		AudioOffset: 250 * time.Millisecond,
	}},
	"./disc",
	video2dvd.Options{Standard: video2dvd.VideoStandardNTSC},
)
```

To select video and audio tracks from the same file, set `VideoTrackIndex` and `AudioTrackIndex` on `MediaInput`. If omitted, the decoder selects the first supported track.

The output path must not already exist. On success, `Convert` creates DVD-Video files under `disc/VIDEO_TS`; `ConvertISO` creates an ISO image. You can create an image from an existing directory with `WriteDVDISO(ctx, "./disc/VIDEO_TS", "./disc.iso")`. If `DiscCapacityBytes` is omitted, the DVD-5 capacity of 4.7 GB is used, including ISO/UDF metadata. `Options.VolumeID` sets the ISO volume name (up to 32 ASCII letters, digits, underscores, and spaces, stored in upper case; default `VIDEO2DVD`). `Options.VideoBitrate` accepts 2 to 8.5 Mbps and defaults to 8 Mbps; the built-in encoder codes anything above 7.5 Mbps at 7.5 Mbps. The encoder outputs MPEG-2 video and AC-3, LPCM, or MPEG-1 Layer II audio; the resolution and frame rate must match the selected DVD standard.

Third-party license notices for dependencies used by the built-in decoder and for the menu fonts are in [`third_party_licenses`](third_party_licenses/README.md).
