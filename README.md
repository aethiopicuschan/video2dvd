# video2dvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/video2dvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/video2dvd)
[![CI](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/video2dvd/graph/badge.svg?token=gV5qlDuFTw)](https://codecov.io/gh/aethiopicuschan/video2dvd)

A Go library for creating DVD-Video directories and ISO images from multiple MP4 and WebM files. It does not launch external commands such as FFmpeg.

## Current implementation

`Converter` validates input files and coordinates per-clip conversion, DVD authoring, and output publishing. Intermediate files are staged in a temporary directory beside the destination. The `VIDEO_TS` directory is moved to the destination only after all processing succeeds.

`ProbeMedia` detects MP4 and WebM files and reads their tracks, codecs, resolutions, audio information, and durations, and the language and name of each track. Subtitle tracks are reported as `TrackKindSubtitle`; a WebM file with subtitle tracks (or any other track outside the WebM audio and video codecs) is read like the same file without them. You can also use `ProbeMP4` and `ProbeWebM` directly. `DemuxMP4` reads audio and video samples from non-fragmented MP4 files in order, including their DTS and PTS values. The library can read H.264 AVC configuration and convert NAL units and SPS/PPS to Annex B using `AVCToAnnexB` and `AVCConfigToAnnexB`. It can also read AAC AudioSpecificConfig data.

`DecodeMP4Video` decodes progressive 8-bit 4:2:0 H.264 into YUV frames. `DecodeMP4Audio` decodes supported AAC-LC (mono, stereo, or channel configurations 3 to 7, downmixed) and AC-3 audio into 48 kHz stereo PCM on the movie timeline: a leading empty edit becomes silence, and the first media edit's start and length are applied rounded to whole samples, so edit lists written on a millisecond movie timescale are accepted. If the primary MP4 AAC decoder rejects an access unit because of non-zero byte-alignment padding (some encoders leave such bits set), a pure-Go AAC-LC fallback decodes the MP4 access units via ADTS framing instead. The switch also happens when the primary decoder has already delivered PCM: the fallback decodes the track from its start, applies the same edit-list timing and sample-rate conversion, and passes on only the audio after the last frame already delivered, so the PCM handler receives one contiguous stream with nothing repeated or missing. The fallback buffers its decoded PCM in a temporary file, which it removes when it returns. Any other decoding failure is returned as an error, and if the fallback fails too the error reports both decoders' failures. `DecodeWebMVideo` decodes VP9 Profile 0/2, VP8, and AV1 video; `DecodeWebMAudio` decodes Opus audio into DVD-ready frames and PCM. VP9 Profile 2 10/12-bit video is converted to 8-bit. VP9 Profiles 0 and 2 use [`github.com/aethiopicuschan/go-vp9`](https://github.com/aethiopicuschan/go-vp9), which maintains VP9 reference state and processes superframes. H.264, AAC and AC-3 are decoded by [`github.com/aethiopicuschan/go-264`](https://github.com/aethiopicuschan/go-264), a fork of [`github.com/rcarmo/go-264`](https://github.com/rcarmo/go-264) that adds multichannel AAC-LC decoding, which is proposed upstream in [rcarmo/go-264#26](https://github.com/rcarmo/go-264/pull/26). The decoders are implemented in pure Go and do not launch external commands such as FFmpeg.

The built-in converter supports MP4 with H.264 and AAC-LC/AC-3, and WebM with VP8, VP9 Profile 0/2, or AV1 video and Opus audio. VP9 Profiles 1/3, WebM Vorbis, high-bit-depth AV1, HDR tone mapping, and MP4 video codecs other than AVC are not supported. VP9 Profile 2 bit depth is converted, but BT.2020/PQ/HLG video is not converted to DVD-compatible SDR.

`PrepareMP4VideoForDVD` and `PrepareWebMVideoForDVD` fit input video to the DVD raster and convert it to NTSC 29.97 fps or PAL 25 fps. The input display aspect ratio must be provided explicitly. When an MP4 video track has an edit list, output starts at movie time zero: frames before the first media edit are skipped and a leading empty edit holds the first frame, which keeps video aligned with the audio track's edits. Frames are resampled with a Lanczos-3 filter. Sources whose colour matrix is BT.709 (tagged in the container or bitstream, or untagged and larger than 1024x576) are converted to the BT.601 matrix that DVD players assume, and sources explicitly tagged full range are compressed to studio range; without this step greens and reds of HD sources are visibly off. WebM timestamps rounded to milliseconds are restored to an even cadence before frame-rate conversion, so 30 and 60 fps sources do not stutter. A 1080p or 4K source still looks softer on DVD than the original: 720x480 or 720x576 is the format's resolution, and the fair comparison is the source scaled to that size. These two functions always produce the video frame rate, so a 24 fps source comes out with every fourth frame repeated; the built-in encoder treats such sources as film instead (see [Film sources](#film-sources-soft-telecine)). For lower-level processing, use `FitVideoFrame` (resampling only), `FitVideoFrameForDVD` (resampling and colour conversion, using `DecodedVideoFrame.Color`), `DVDFrameRateConverter`, and `NewDVDFilmFrameRateConverter`. `PrepareMP4AudioForDVD` and `PrepareWebMAudioForDVD` convert audio to 48 kHz stereo LPCM data. `DVDLPCMPacketWriter` wraps PCM chunks in DVD LPCM PES packets and writes them into 2048-byte program-stream sectors.

`NewInProcessEncoder` creates MPEG-2 video and LPCM or AC-3 audio from MP4 or WebM without external commands (see [Audio tracks](#audio-tracks)). Requested video bitrates above 7.5 Mbit/s, including the 8 Mbit/s default, are coded and declared at 7.5 Mbit/s so that video, LPCM audio, and pack overhead fit the 10.08 Mbit/s DVD multiplex rate. `MediaInput` lets you select separate video and audio files and choose tracks within each container. The title is exactly as long as the encoded video. Audio is shifted by inserting silence or trimming its start according to the requested offset, then padded with silence or trimmed to the video length. Decoded audio chunks are placed by their running sample count; container timestamps only reposition audio when they deviate by more than 20 ms, so nanosecond- or millisecond-rounded timestamps never drop or insert samples. When video and audio come from the same WebM file, the title starts at the first video frame and audio moves with it. Inputs whose audio tracks cannot be decoded (for example MP3 in MP4, or AAC whose channels a program config element describes) are rejected before video encoding starts; multichannel AAC-LC, AC-3 and Opus are downmixed (see [Audio tracks](#audio-tracks)). The MPEG-2 encoder emits I/P frames in closed GOPs of up to 15 pictures (12 or 13 for film) and starts a new GOP at a scene cut. P frames use half-pixel motion prediction over a range of 32 pixels with quantized DCT residuals, code macroblocks without a usable prediction as intra, and use reconstructed frames as references. Rate control follows the VBV model and balances quality within a GOP. The stream is labelled SMPTE 170M (NTSC) or BT.470BG (PAL). The encoder does not use B frames or adaptive quantization, so its compression efficiency is below that of dedicated encoders.

`MuxDVDTitleVOB` multiplexes one title's MPEG-2 video and LPCM audio against a byte-driven system clock: SCR starts at zero and advances with the 10.08 Mbit/s multiplex rate, the first picture is decoded 0.3 s into the stream, and every access unit is delivered ahead of its DTS/PTS within the DVD decoder buffer sizes. Picture timestamps count display fields, two per picture or three where `repeat_first_field` is set, so soft-telecined film is timed correctly. A NAV pack with PCI/DSI, including the VOBU search tables and audio sync pointers, starts each VOBU at an I frame. The muxer reads the elementary streams twice (a planning pass, then the writing pass) and returns an error before writing anything if the video cannot be delivered in time. `MPEGProgramStreamWriter` and `DVDLPCMPacketWriter` are lower-level components, and `BuildDVDNAVPack` creates a standalone NAV pack. Fragmented MP4, encrypted tracks, MP4 edits after the first media edit, and MP4 rotation metadata are outside the current scope. `DemuxMP4` and `DecodeMP4Video` report media timestamps without applying edit lists.

`NewInProcessAuthorer` authors up to 99 inputs as separate VTS/title sets in input order, registering each input as one title and one chapter. The generated title menu in `VIDEO_TS.VOB` lists each title by its input file name (`EncodedTitle.Name`); users can navigate to titles with the DVD remote's arrow and enter buttons, and the selected button is highlighted through a subpicture stream. 16:9 menus carry a second, letterboxed highlight for 4:3 displays. Playback returns to the title menu when a title ends, and the menu loops back to its first page after playback. The 4:3 menu uses two columns and the 16:9 menu uses one column. Menus are automatically split into pages of up to 36 buttons. Each page is half a second of video (15 pictures, 0.6 s on PAL) with silent LPCM audio of the same length, stored as a VOB of its own inside `VIDEO_TS.VOB`; the page is a still cell, so its picture stays on screen until a button is chosen. macOS DVD Player needs both the pictures and the audio: it shows only a black screen for the single-picture menu with 2 ms of audio that earlier versions wrote. `Options.MenuTitle`, `MenuBackgroundLuma`, and `MenuTextLuma` configure the menu heading and brightness; the heading and the title names may contain Japanese (see [Menu text](#menu-text)), and labels are truncated to the button width. Text subtitle tracks of the inputs can be recorded as DVD subtitles (see [Subtitles](#subtitles)). The authorer creates VMG/VTS IFO and BUP files, PTT/PGC/cell/VOBU tables, a VTS time map, and each title's VOB, split into `VTS_nn_1.VOB` through `VTS_nn_9.VOB` of at most 1 GiB each, so a title may be up to about 9 GiB. Title sets have no menu VOB of their own (`VTS_nn_0.VOB` is not written). `WriteDVDISO` packages `VIDEO_TS` into a DVD-Video image readable through both ISO 9660 and UDF 1.02. It lays each file set out contiguously (IFO, VOBs, BUP), sets the IFO sector references from that layout relative to the owning IFO, and checks capacity including ISO metadata. Converting to an ISO needs temporary space beside the destination for the elementary streams, the `VIDEO_TS` files, and the image, roughly three times the image size. UDF structures are written using the pure-Go [go-sdk-winmediafoundry UDF package](https://pkg.go.dev/github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf); no external processes are launched. Writing to physical discs is not implemented.

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

## Audio tracks

A title can have up to eight audio streams, which the viewer switches between with the player's audio button; players label each with its language. Set `Options.IncludeAllAudio` to record every audio track of an input that the built-in decoders read (tracks they cannot decode, such as AAC whose channels a program config element describes, are skipped; an input with `AudioTrackIndex` keeps its one track), or list the tracks of one input in `MediaInput.AudioTracks` (tracks of `AudioPath`, or of `VideoPath` when it is empty; it cannot be combined with `AudioTrackIndex`). The first stream is the one players start with: with `IncludeAllAudio` that is the track the file marks as default (see [Default tracks](#default-tracks)), followed by the others in track order. Each stream is labelled with its track's language, mapped as `DVDLanguageCode` describes. Every stream is placed on the title timeline exactly as the single audio track always was (edit lists, `AudioOffset`, the first video frame of a WebM file) and padded with silence or cut to the length of the video, each on its own. Track lists with more than eight tracks, unknown or unsupported tracks, or repeated tracks are refused before anything is encoded.

```go
err = converter.ConvertSources(ctx,
	[]video2dvd.MediaInput{{VideoPath: "dubbed.webm", AudioTracks: []int{2, 1}}}, // track 2 plays first
	"./disc",
	video2dvd.Options{},
)
```

`Options.AudioFormat` decides how the audio is coded. With the default, `AudioFormatAuto`, a title with one audio stream keeps LPCM, as before, and its output is unchanged byte for byte; a title with several gets AC-3 for all of them, because a second 1.536 Mbit/s LPCM stream does not fit the 10.08 Mbit/s multiplex beside the video, and LPCM and AC-3 are the only audio formats every NTSC player must play. `AudioFormatAC3` also codes a single stream as AC-3. AC-3 is written by a pure-Go encoder at 48 kHz stereo; `Options.AC3Bitrate` selects 192 (the default), 224, 256, 320, 384 or 448 kbit/s. `AudioFormatLPCM` keeps several streams LPCM and lowers the video bitrate instead: video and audio together get 9.036 Mbit/s, the share of 7.5 Mbit/s video and one LPCM stream, so two LPCM streams leave the video 5.964 Mbit/s, three 4.428 and four 2.892; from five on the video would get less than 2 Mbit/s, and the conversion is refused before anything is encoded. AC-3 streams are accounted the same way, so many streams at a high AC-3 bitrate lower the video bitrate as well (eight at 448 kbit/s leave 5.452 Mbit/s).

The multiplexer puts LPCM streams on substreams 0xA0 and up and AC-3 streams on 0x80 and up, writes the audio sync pointers of every stream into the NAV packs, and keeps each AC-3 stream within a 4 KiB decoder buffer. The IFOs state each stream's coding, channel count and language, and the title PGC enables all of them. An `Encoder` of your own can return several streams in `EncodedTitle.Audio` (LPCM or AC-3 files with their languages); `AudioPath` and `AudioCodec` then describe the first one.

`EstimateDVDBytes` estimates the size of the ISO image for titles of given lengths and audio stream counts with given options, before anything is encoded, so that an application can tell whether its inputs fit a disc. It counts every stream at its full rate, which the video encoder never exceeds but undershoots on simple pictures, so it is an upper estimate: detailed video comes close to it, while a still or mostly black picture takes a fraction.

The AC-3 encoder is written in Go for this library (ATSC A/52: 512-sample MDCT with the KBD window, long blocks, rematrixing, adaptive exponent strategies, the standard parametric bit allocation with a per-frame SNR offset search, no coupling, 19.8 kHz bandwidth up to 256 kbit/s and 20.9 kHz above). Every stream starts with 256 samples of silence, which keeps the start of the audio free of the aliasing a decoder cannot cancel in the first block; the multiplexer presents AC-3 that much earlier, which also suits AC-3 from encoders such as ffmpeg's, which have the same delay. Its quality was measured by decoding its output with both ffmpeg and the pure-Go AC-3 decoder of go-264 and comparing it with the input sample by sample, from the first sample (the decoders agree within 0.3 dB; both CRCs of every frame verify, and ffmpeg reports no errors even with `-err_detect crccheck+bitstream+buffer`):

| Signal | 192 kbit/s | 448 kbit/s |
|---|---|---|
| 1 kHz sine at -6 dBFS | 82.8 dB | 86.2 dB |
| ten tones from 100 Hz to 15 kHz | 39.6 dB | 62.8 dB |
| music-like (band-limited noise and notes) | 21.6 dB | 27.3 dB |

On the music-like signal ffmpeg's own AC-3 encoder reaches 22.1 and 27.1 dB; a perceptual coder places its noise under the masking threshold instead of minimizing it, so broadband signals score lower than tones. Discs with two AC-3 streams made from MP4 (AAC) and WebM (Opus) sources were checked with ffprobe, lsdvd, libdvdnav, VLC and macOS DVD Player; on a test signal of a flash and a beep every second, each stream kept the source's offset between picture and sound within 0.2 ms over 20 seconds. The encoder runs at about 240 times realtime. It writes long blocks only, so sharp transients can pre-echo slightly.

Audio with more than two channels is downmixed to stereo. Opus in WebM with 3 to 8 channels (channel mapping family 1, Vorbis order) is decoded with gopus's multistream decoder and mixed with the ITU-R BS.775 coefficients: Lo = L + 0.7071·C + 0.7071·Ls + 0.7071·Lb + 0.5·Cb and Ro = R + 0.7071·C + 0.7071·Rs + 0.7071·Rb + 0.5·Cb, with the LFE channel discarded; both outputs are then divided by their coefficient sum (1/(1 + 0.7071 + 0.7071) for 5.1, 1/3.1213 for 7.1), so the mix cannot clip. AC-3 in MP4 of any channel mode is downmixed by the go-264 decoder with the centre and surround mix levels coded in the stream, LFE discarded and normalized the same way; at the usual -3 dB levels this is the BS.775 downmix for the layouts with two surround channels, while a single surround channel (2/1, 3/1) is added to both outputs at the full surround level. AAC-LC in MP4 with channel configurations 3 to 7 (3.0, 4.0, 5.0, 5.1 and 7.1, as ffmpeg writes 5.1 by default) is decoded and downmixed by the go-264 decoder with the same BS.775 coefficients and normalization; measured on test tones, every channel lands within 0.01 dB of its coefficient and LFE is gone. Mono and stereo sources are unchanged. Refused before encoding are: AAC whose channel layout is given by a program config element (channel configuration 0, which ffmpeg writes for layouts such as 5.1(side)); multichannel HE-AAC, which go-264 does not decode at all; and Opus ambisonics (mapping families 2 and 3). The AAC-LC fallback decoder that takes over when the primary decoder rejects non-zero alignment padding handles mono and stereo only, so a multichannel track with such padding still fails.

## Default tracks

`TrackInfo.Default` reports the container's mark of a default track: the enabled flag of an MP4 track header (ffprobe shows it as the default disposition) or a WebM track's `FlagDefault`. Matroska makes a track without a `FlagDefault` element a default track, so `TrackInfo.Default` is true for such tracks. The converter only follows such an implied mark when another track of the same kind has `FlagDefault` 0, which is how ffmpeg writes it; in files from tools that write no `FlagDefault` at all, which would have every track marked, the implied marks are ignored.

Unless `MediaInput.AudioTrackIndex` or `AudioTracks` says otherwise, the converter records the first supported audio track that carries a default mark, and the first supported audio track when none does. With `IncludeSubtitles`, the subtitle track that carries a default mark becomes subtitle stream 1 and the others follow in track order. These are changes from v1.1.0, which always took the first supported audio track and kept subtitle tracks in track order; files without marks, or whose first tracks are the marked ones, convert as before.

## Subtitles

Set `Options.IncludeSubtitles` to record the text subtitle tracks of the inputs as DVD subtitles (subpicture streams), which the viewer turns on and off and switches between with the player's subtitle button. The converter reads MP4 timed text (`tx3g`, which ffmpeg calls `mov_text`) and WebM WebVTT (`D_WEBVTT/SUBTITLES`, as well as `S_TEXT/WEBVTT`) and SubRip (`S_TEXT/UTF8`) tracks. Every such track of an input's video file becomes a stream of its title, the one marked as default first (see [Default tracks](#default-tracks)) and the others in track order, up to the 32 streams a title can have; other subtitle tracks, such as bitmap subtitles, are skipped. `MediaInput.SubtitleTracks` chooses the tracks of one input and their order instead (an empty list records none). `ReadSubtitleCues` returns the cues of a track, `DVDLanguageCode` the language code that is recorded for it.

```go
err = converter.ConvertSources(ctx,
	[]video2dvd.MediaInput{{VideoPath: "talk.webm", SubtitleTracks: []int{3, 2}}}, // track 3 becomes stream 1
	"./disc",
	video2dvd.Options{IncludeSubtitles: true, ShowSubtitles: true},
)
```

A title with subtitles starts with its first stream selected and hidden; with `Options.ShowSubtitles` it is shown. `Options.ShowDefaultSubtitles` shows it instead when the track it comes from carries a default mark (see [Default tracks](#default-tracks)) and hides it otherwise, and `MediaInput.ShowSubtitles` decides for one input. The first that is set wins, in this order: `MediaInput.ShowSubtitles`, `Options.ShowDefaultSubtitles`, `Options.ShowSubtitles`. Each stream is labelled with the language of its track (the BCP 47 tag, or else the ISO 639-2 code, mapped to the two-letter code DVD-Video stores), so players list it as Japanese or English; an undetermined language is recorded as unspecified. Cues keep their place relative to the first video frame of their file, as the audio of the same file does. Markup is removed: WebVTT tags and ruby text, SubRip `<i>`/`<b>`/`<font>` tags and `{\an8}`-style overrides, and the style boxes of timed text; WebVTT character references are resolved. While cues overlap, their texts are shown together, one below the other, the same text once. A subtitle longer than about 12 minutes is shown by several subpicture units, and cues are cut to the title.

Subtitles are drawn with the menu fonts at twice their size (32 lines for a line of Japanese), or with `Options.MenuFont` at twice its menu size, in white with a two-pixel black outline on a transparent background, centred at the bottom of the title-safe area. Lines are broken to fit 80% of the frame width: at spaces, and between the characters of Japanese, Chinese or Korean text except before closing punctuation and small kana, preferring a break after punctuation; the lines of a cue are made about equally long. Text that needs more than four lines is drawn at the menu size, and lines beyond eight are left out. The limits of the menu text apply: without a `MenuFont`, ASCII letters are shown in upper case, and characters outside the fonts are shown as `?`. The same picture serves 4:3 and 16:9 displays, so on a 4:3 display a letterboxed 16:9 title shows its subtitles at the bottom of the frame and narrower than on a widescreen one.

Subtitles go through the rest of the pipeline as stream files that `WriteDVDSubtitleStream` writes from cues (`EncodedTitle.Subtitles`), so a custom `Encoder` can supply them as well. They count towards the disc capacity checks. Subtitled discs made from MP4 and WebM inputs have been checked with ffmpeg, lsdvd, libdvdnav, VLC, and macOS DVD Player (showing, switching, and turning off subtitles), not on a hardware player.

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
