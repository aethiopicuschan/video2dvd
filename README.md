# mp4todvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/mp4todvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/mp4todvd)
[![Go Report Card](https://goreportcard.com/badge/github.com/aethiopicuschan/mp4todvd)](https://goreportcard.com/report/github.com/aethiopicuschan/mp4todvd)
[![CI](https://github.com/aethiopicuschan/mp4todvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/mp4todvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/mp4todvd/graph/badge.svg?token=hUdXoeMoFl)](https://codecov.io/gh/aethiopicuschan/mp4todvd)

複数の MP4/WebM から DVD-Video ディレクトリを作成するための Go ライブラリ API です。FFmpeg などの外部コマンドは起動しません。

## 現在の実装

`Converter` は入力ファイルの検証、クリップごとの変換処理、DVD オーサリング処理、出力の公開を管理します。中間ファイルは出力先と同じ親ディレクトリに一時作成し、処理がすべて成功した場合だけ `VIDEO_TS` ディレクトリを出力先へ移動します。

`ProbeMedia` はファイルから MP4/WebM を判別してトラック・コーデック・解像度・音声情報・再生時間を読み取ります。個別の `ProbeMP4` と `ProbeWebM` も使えます。`DemuxMP4` は非 fragmented MP4 の音声・映像サンプルを DTS/PTS とともに順に取り出します。H.264 の AVC 設定を取得でき、`AVCToAnnexB` と `AVCConfigToAnnexB` でNAL列とSPS/PPSを Annex B 形式に変換できます。AAC の AudioSpecificConfig も取得します。

`DecodeMP4Video` は progressive 8-bit 4:2:0 H.264 をYUVフレームに、`DecodeMP4Audio` は対応する AAC-LC/AC-3 を48 kHz stereo PCMにデコードします。`DecodeWebMVideo` は VP9 Profile 0/2、VP8、AV1 の映像を、`DecodeWebMAudio` は Opus 音声をDVD向けフレーム/PCMへデコードします。VP9 Profile 2 の10/12-bit映像は8-bitへ変換します。各デコーダーは純Go実装を使い、FFmpegなどの外部コマンドは起動しません。

組み込み変換器で扱える主な入力は MP4 H.264 + AAC-LC/AC-3 と WebM VP8/VP9 Profile 0/2/AV1 + Opus です。VP9 Profile 1/3、WebM Vorbis、AV1の高ビット深度、HDRのトーンマッピング、AVC以外のMP4映像コーデックは未対応です。VP9 Profile 2のビット深度変換は行いますが、BT.2020/PQ/HLGからDVD向けSDRへの色変換は行いません。

`PrepareMP4VideoForDVD` と `PrepareWebMVideoForDVD` は入力映像をDVDラスタへフィットし、NTSC 29.97 fps／PAL 25 fpsへ変換します。入力の表示アスペクト比は明示します。個別処理には `FitVideoFrame` と `DVDFrameRateConverter` を使えます。`PrepareMP4AudioForDVD` と `PrepareWebMAudioForDVD` は入力音声を48 kHz stereo LPCM音声データへ変換します。`DVDLPCMPacketWriter` はPCMチャンクをDVD LPCMヘッダー付きPESにまとめ、2048バイトのプログラムストリームセクターへ書きます。

`NewInProcessEncoder` は外部コマンドを使わず MP4/WebM からMPEG-2映像とLPCM音声を生成します。現段階のMPEG-2エンコーダーは閉じたGOPにI/Pフレームを出力し、前フレームと完全一致するマクロブロックをPフレームで省略します。変更されたマクロブロックはイントラ符号化するため動き予測は行わず、単純な量子化とあわせて圧縮効率と画質は限定的です。一般的な品質の変換にはまだ使えません。

`MuxDVDTitleVOB` は1タイトル分のMPEG-2映像とLPCM音声をPTS順に多重化し、Iフレームから始まるVOBUごとにPCI/DSI入りのNAVパックを置きます。`MPEGProgramStreamWriter` と `DVDLPCMPacketWriter` はその下位部品で、`BuildDVDNAVPack` はNAVパック単体を構築します。fragmented MP4、暗号化トラック、映像demux側でのedit list適用も現在の対象外です。

`NewInProcessAuthorer` は最大99個の入力を入力順に別々のVTS/タイトルとしてオーサリングし、VMG/VTSのIFO・BUP、PTT/PGC/セル/VOBUテーブルと各タイトルのVOBを作成します。現在はMPEG-2映像とLPCM音声に対応し、タイトルメニューや字幕は作成しません。`WriteDVDISO` は現在のメニューなし `VIDEO_TS` をISO 9660とUDF 1.02の両方から読めるDVD-Videoイメージにまとめます。UDFの実ファイル配置に基づいてIFO内のセクタ参照を確定し、ISOメタデータを含めた容量も確認します。UDFの記述には純Goの [go-sdk-winmediafoundry UDF package](https://pkg.go.dev/github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf) を使い、外部プロセスは起動しません。ディスクへの書き込み、ディスク起動時の自動再生、複数タイトルの自動連続再生、メニュー操作は未実装です。

## バックエンドの接続

```go
encoder := mp4todvd.NewInProcessEncoder()
authorer := mp4todvd.NewInProcessAuthorer()

converter, err := mp4todvd.New(encoder, authorer)
if err != nil {
	return err
}

err = converter.Convert(ctx,
	[]string{"part-1.mp4", "part-2.mp4"},
	"./disc",
	mp4todvd.Options{
		Standard: mp4todvd.VideoStandardNTSC,
		Aspect:   mp4todvd.AspectRatio16x9,
	},
)
```

ISOイメージまで作成する場合は `ConvertISO` を使います。

```go
err = converter.ConvertISO(ctx,
	[]string{"part-1.mp4", "part-2.mp4"},
	"./disc.iso",
	mp4todvd.Options{Standard: mp4todvd.VideoStandardNTSC},
)
```

出力先は既存でないパスを指定します。`Convert` の成功時は `disc/VIDEO_TS` 以下に DVD-Video ファイルが作成されます。`ConvertISO` はISOイメージファイルを作ります。すでに作成したディレクトリは `WriteDVDISO(ctx, "./disc/VIDEO_TS", "./disc.iso")` でイメージ化できます。`DiscCapacityBytes` を省略するとDVD-5の4.7 GBを上限にし、ISO/UDFの管理領域も含めて判定します。エンコーダーの出力は MPEG-2 映像と AC-3、LPCM、または MPEG-1 Layer II 音声で、解像度とフレームレートは選択した DVD 規格に適合する必要があります。
