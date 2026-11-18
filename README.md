# video2dvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/video2dvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/video2dvd)
[![CI](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/video2dvd/graph/badge.svg?token=gV5qlDuFTw)](https://codecov.io/gh/aethiopicuschan/video2dvd)

A Go library for creating DVD-Video directories and ISO images from multiple MP4 and WebM files. It does not launch external commands such as FFmpeg.

## Current implementation

`Converter` validates input files and coordinates per-clip conversion, DVD authoring, and output publishing. Intermediate files are staged in a temporary directory beside the destination. The `VIDEO_TS` directory is moved to the destination only after all processing succeeds.

`ProbeMedia` detects MP4 and WebM files and reads their tracks, codecs, resolutions, audio information, and durations. You can also use `ProbeMP4` and `ProbeWebM` directly. `DemuxMP4` reads audio and video samples from non-fragmented MP4 files in order, including their DTS and PTS values. The library can read H.264 AVC configuration and convert NAL units and SPS/PPS to Annex B using `AVCToAnnexB` and `AVCConfigToAnnexB`. It can also read AAC AudioSpecificConfig data.

`DecodeMP4Video` decodes progressive 8-bit 4:2:0 H.264 into YUV frames. `DecodeMP4Audio` decodes supported AAC-LC and AC-3 audio into 48 kHz stereo PCM. `DecodeWebMVideo` decodes VP9 Profile 0/2, VP8, and AV1 video; `DecodeWebMAudio` decodes Opus audio into DVD-ready frames and PCM. VP9 Profile 2 10/12-bit video is converted to 8-bit. VP9 Profile 0 uses [`github.com/thesyncim/govpx`](https://github.com/thesyncim/govpx), while Profile 2 uses [`github.com/aethiopicuschan/go-vp9`](https://github.com/aethiopicuschan/go-vp9). The decoders are implemented in pure Go and do not launch external commands such as FFmpeg.

The built-in converter supports MP4 with H.264 and AAC-LC/AC-3, and WebM with VP8, VP9 Profile 0/2, or AV1 video and Opus audio. VP9 Profiles 1/3, WebM Vorbis, high-bit-depth AV1, HDR tone mapping, and MP4 video codecs other than AVC are not supported. VP9 Profile 2 bit depth is converted, but BT.2020/PQ/HLG video is not converted to DVD-compatible SDR.

`PrepareMP4VideoForDVD` and `PrepareWebMVideoForDVD` fit input video to the DVD raster and convert it to NTSC 29.97 fps or PAL 25 fps. The input display aspect ratio must be provided explicitly. For lower-level processing, use `FitVideoFrame` and `DVDFrameRateConverter`. `PrepareMP4AudioForDVD` and `PrepareWebMAudioForDVD` convert audio to 48 kHz stereo LPCM data. `DVDLPCMPacketWriter` wraps PCM chunks in DVD LPCM PES packets and writes them into 2048-byte program-stream sectors.

`NewInProcessEncoder` creates MPEG-2 video and LPCM audio from MP4 or WebM without external commands. It raises the maximum video bitrate to 7 Mbit/s and updates the program stream's declared rate. `MediaInput` lets you select separate video and audio files and choose tracks within each container. Audio is shifted by inserting silence or trimming its start according to the requested offset, then trimmed to the video duration. The MPEG-2 encoder emits I/P frames in closed GOPs. P frames use half-pixel motion prediction and quantized DCT residuals, and reconstructed frames are used as references for subsequent predictions. Motion search and rate control are currently simple, and the encoder does not use B frames, so its quality and compression efficiency are below those of dedicated encoders.

`MuxDVDTitleVOB` multiplexes one title's MPEG-2 video and LPCM audio in PTS order, placing a NAV pack with PCI/DSI at each VOBU that starts with an I frame. `MPEGProgramStreamWriter` and `DVDLPCMPacketWriter` are lower-level components, and `BuildDVDNAVPack` creates a standalone NAV pack. Fragmented MP4, encrypted tracks, and edit-list handling in the video demuxer are outside the current scope.

`NewInProcessAuthorer` authors up to 99 inputs as separate VTS/title sets in input order, registering each input as one title and one chapter. The generated title menu displays video filenames in `VIDEO_TS.VOB`; users can navigate to titles with the DVD remote's arrow and enter buttons. Playback returns to the title menu when a title ends, and the menu loops back to its first page after playback. The 4:3 menu uses two columns and the 16:9 menu uses one column. Menus are automatically split into pages of up to 36 buttons. `Options.MenuTitle`, `MenuBackgroundLuma`, and `MenuTextLuma` configure the menu heading and brightness. Characters outside the English alphanumeric set are displayed as `?`. Subtitles are not supported. The authorer creates VMG/VTS IFO and BUP files, PTT/PGC/cell/VOBU tables, and each title's VOB. `WriteDVDISO` packages `VIDEO_TS` into a DVD-Video image readable through both ISO 9660 and UDF 1.02. It uses the actual UDF file layout to set sector references in IFO files and checks capacity including ISO metadata. UDF structures are written using the pure-Go [go-sdk-winmediafoundry UDF package](https://pkg.go.dev/github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf); no external processes are launched. Writing to physical discs is not implemented.

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

The output path must not already exist. On success, `Convert` creates DVD-Video files under `disc/VIDEO_TS`; `ConvertISO` creates an ISO image. You can create an image from an existing directory with `WriteDVDISO(ctx, "./disc/VIDEO_TS", "./disc.iso")`. If `DiscCapacityBytes` is omitted, the DVD-5 capacity of 4.7 GB is used, including ISO/UDF metadata. `Options.VideoBitrate` sets the video bitrate from 2 to 8.5 Mbps and defaults to 8 Mbps. The encoder outputs MPEG-2 video and AC-3, LPCM, or MPEG-1 Layer II audio; the resolution and frame rate must match the selected DVD standard.
