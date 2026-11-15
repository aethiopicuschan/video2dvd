# mp4todvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/mp4todvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/mp4todvd)
[![Go Report Card](https://goreportcard.com/badge/github.com/aethiopicuschan/mp4todvd)](https://goreportcard.com/report/github.com/aethiopicuschan/mp4todvd)
[![CI](https://github.com/aethiopicuschan/mp4todvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/mp4todvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/mp4todvd/graph/badge.svg?token=hUdXoeMoFl)](https://codecov.io/gh/aethiopicuschan/mp4todvd)

複数の MP4 から DVD-Video ディレクトリを作成するための Go ライブラリ API です。外部コマンドは起動しません。

## 現在の実装

`Converter` は入力ファイルの検証、クリップごとの変換処理、DVD オーサリング処理、出力の公開を管理します。中間ファイルは出力先と同じ親ディレクトリに一時作成し、処理がすべて成功した場合だけ `VIDEO_TS` ディレクトリを出力先へ移動します。

`ProbeMP4` は通常の MP4 のトラック・コーデック・解像度・音声情報・再生時間を読み取ります。`DemuxMP4` は非 fragmented MP4 の音声・映像サンプルを DTS/PTS とともに順に取り出します。H.264 の AVC 設定を取得でき、`AVCToAnnexB` と `AVCConfigToAnnexB` でNAL列とSPS/PPSを Annex B 形式に変換できます。AAC の AudioSpecificConfig も取得します。これらは圧縮データを返す段階までで、映像・音声のデコードは行いません。

`DecodeMP4Video` は progressive 8-bit 4:2:0 H.264 をYUVフレームに、`DecodeMP4Audio` は対応する AAC-LC/AC-3 を48 kHz stereo PCMにデコードします。デコーダーは純Goの [go-264](https://github.com/rcarmo/go-264) を使い、FFmpegなどの外部コマンドは起動しません。映像デコードは未対応のinterlaced、10-bit、4:2:2/4:4:4形式をエラーにします。

`PrepareMP4VideoForDVD` はH.264 MP4をデコードし、DVDラスタへのフィットとNTSC 29.97 fps／PAL 25 fpsへの間引き・反復を続けて行います。入力の表示アスペクト比は明示します。個別処理には `FitVideoFrame` と `DVDFrameRateConverter` を使えます。`PrepareMP4AudioForDVD` はAAC-LC/AC-3を48 kHz stereo LPCM音声データへ変換します。音声出力はDVD LPCM用のビッグエンディアンで、DVD用PESヘッダーやVOBへの多重化は別途必要です。

`NewInProcessEncoder` は外部コマンドを使わずMP4からMPEG-2映像とLPCM音声を生成します。現段階のMPEG-2エンコーダーはIフレームのみで各8x8ブロックのDC係数しか符号化しないため、細部が大きく失われます。標準ヘッダー検査を通る初期実装であり、一般的な品質の変換にはまだ使えません。

`MPEGProgramStreamWriter` はPESを2048バイトのMPEG-2プログラムストリームセクターへ書き込む低レベル部品です。IFO/BUP、DVD NAVパック、LPCM用DVDサブストリームヘッダーを含むオーサリングは未実装で、`Converter` 単体では再生可能なDVDを作成できません。fragmented MP4、暗号化トラック、映像demux側でのedit list適用も現在の対象外です。

## バックエンドの接続

```go
encoder := mycodec.NewEncoder()
authorer := mydvd.NewAuthorer()

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

出力先は既存でないパスを指定します。成功時は `disc/VIDEO_TS` 以下に DVD-Video ファイルが作成されます。`DiscCapacityBytes` を省略すると DVD-5 の4.7 GBを上限にします。エンコーダーの出力は MPEG-2 映像と AC-3、LPCM、または MPEG-1 Layer II 音声で、解像度とフレームレートは選択した DVD 規格に適合する必要があります。
