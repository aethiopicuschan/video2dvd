# video2dvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/video2dvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/video2dvd)
[![Go Report Card](https://goreportcard.com/badge/github.com/aethiopicuschan/video2dvd)](https://goreportcard.com/report/github.com/aethiopicuschan/video2dvd)
[![CI](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/video2dvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/video2dvd/graph/badge.svg?token=gV5qlDuFTw)](https://codecov.io/gh/aethiopicuschan/video2dvd)

複数の MP4/WebM から DVD-Video ディレクトリを作成するための Go ライブラリ API です。FFmpeg などの外部コマンドは起動しません。

## 現在の実装

`Converter` は入力ファイルの検証、クリップごとの変換処理、DVD オーサリング処理、出力の公開を管理します。中間ファイルは出力先と同じ親ディレクトリに一時作成し、処理がすべて成功した場合だけ `VIDEO_TS` ディレクトリを出力先へ移動します。

`ProbeMedia` はファイルから MP4/WebM を判別してトラック・コーデック・解像度・音声情報・再生時間を読み取ります。個別の `ProbeMP4` と `ProbeWebM` も使えます。`DemuxMP4` は非 fragmented MP4 の音声・映像サンプルを DTS/PTS とともに順に取り出します。H.264 の AVC 設定を取得でき、`AVCToAnnexB` と `AVCConfigToAnnexB` でNAL列とSPS/PPSを Annex B 形式に変換できます。AAC の AudioSpecificConfig も取得します。

`DecodeMP4Video` は progressive 8-bit 4:2:0 H.264 をYUVフレームに、`DecodeMP4Audio` は対応する AAC-LC/AC-3 を48 kHz stereo PCMにデコードします。`DecodeWebMVideo` は VP9 Profile 0/2、VP8、AV1 の映像を、`DecodeWebMAudio` は Opus 音声をDVD向けフレーム/PCMへデコードします。VP9 Profile 2 の10/12-bit映像は8-bitへ変換します。各デコーダーは純Go実装を使い、FFmpegなどの外部コマンドは起動しません。

組み込み変換器で扱える主な入力は MP4 H.264 + AAC-LC/AC-3 と WebM VP8/VP9 Profile 0/2/AV1 + Opus です。VP9 Profile 1/3、WebM Vorbis、AV1の高ビット深度、HDRのトーンマッピング、AVC以外のMP4映像コーデックは未対応です。VP9 Profile 2のビット深度変換は行いますが、BT.2020/PQ/HLGからDVD向けSDRへの色変換は行いません。

`PrepareMP4VideoForDVD` と `PrepareWebMVideoForDVD` は入力映像をDVDラスタへフィットし、NTSC 29.97 fps／PAL 25 fpsへ変換します。入力の表示アスペクト比は明示します。個別処理には `FitVideoFrame` と `DVDFrameRateConverter` を使えます。`PrepareMP4AudioForDVD` と `PrepareWebMAudioForDVD` は入力音声を48 kHz stereo LPCM音声データへ変換します。`DVDLPCMPacketWriter` はPCMチャンクをDVD LPCMヘッダー付きPESにまとめ、2048バイトのプログラムストリームセクターへ書きます。

`NewInProcessEncoder` は外部コマンドを使わず MP4/WebM からMPEG-2映像とLPCM音声を生成します。映像の上限ビットレートを7 Mbit/sに引き上げ、プログラムストリームの宣言レートも更新しています。`MediaInput` を使うと映像と音声を別ファイルから選び、コンテナ内のトラック番号を指定できます。音声の開始オフセットに応じて無音を挿入し、映像の再生時間に合わせて音声を切り詰めます。MPEG-2エンコーダーは閉じたGOPにI/Pフレームを出力し、Pフレームでは半画素精度の動き予測と量子化DCT残差を使います。各フレームの再構成画像を次の予測参照に使います。現段階では動き探索とレート制御が簡易的で、Bフレームも使わないため、画質と圧縮効率は専用エンコーダーに及びません。

`MuxDVDTitleVOB` は1タイトル分のMPEG-2映像とLPCM音声をPTS順に多重化し、Iフレームから始まるVOBUごとにPCI/DSI入りのNAVパックを置きます。`MPEGProgramStreamWriter` と `DVDLPCMPacketWriter` はその下位部品で、`BuildDVDNAVPack` はNAVパック単体を構築します。fragmented MP4、暗号化トラック、映像demux側でのedit list適用も現在の対象外です。

`NewInProcessAuthorer` は最大99個の入力を入力順に別々のVTS/タイトルとしてオーサリングし、VMG/VTSのIFO・BUP、PTT/PGC/セル/VOBUテーブルと各タイトルのVOBを作成します。現在はMPEG-2映像とLPCM音声に対応し、タイトルメニューや字幕は作成しません。`WriteDVDISO` は現在のメニューなし `VIDEO_TS` をISO 9660とUDF 1.02の両方から読めるDVD-Videoイメージにまとめます。UDFの実ファイル配置に基づいてIFO内のセクタ参照を確定し、ISOメタデータを含めた容量も確認します。UDFの記述には純Goの [go-sdk-winmediafoundry UDF package](https://pkg.go.dev/github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf) を使い、外部プロセスは起動しません。ディスクへの書き込み、ディスク起動時の自動再生、複数タイトルの自動連続再生、メニュー操作は未実装です。

## バックエンドの接続

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

ISOイメージまで作成する場合は `ConvertISO` を使います。

```go
err = converter.ConvertISO(ctx,
	[]string{"part-1.mp4", "part-2.mp4"},
	"./disc.iso",
	video2dvd.Options{Standard: video2dvd.VideoStandardNTSC},
)
```

YouTube ダウンローダーが映像と音声を別ファイルに保存した場合は `ConvertSources` を使います。トラック番号は `ProbeMedia` が返す `TrackInfo.Index` と同じゼロ始まりです。`AudioOffset` が正なら音声の前に無音を挿入し、負なら音声の先頭を切り詰めます。

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

同じファイル内の映像・音声トラックを選ぶ場合も `MediaInput` の `VideoTrackIndex` と `AudioTrackIndex` を設定できます。未指定時は、デコーダーが対応する最初のトラックを選びます。

出力先は既存でないパスを指定します。`Convert` の成功時は `disc/VIDEO_TS` 以下に DVD-Video ファイルが作成されます。`ConvertISO` はISOイメージファイルを作ります。すでに作成したディレクトリは `WriteDVDISO(ctx, "./disc/VIDEO_TS", "./disc.iso")` でイメージ化できます。`DiscCapacityBytes` を省略するとDVD-5の4.7 GBを上限にし、ISO/UDFの管理領域も含めて判定します。エンコーダーの出力は MPEG-2 映像と AC-3、LPCM、または MPEG-1 Layer II 音声で、解像度とフレームレートは選択した DVD 規格に適合する必要があります。
