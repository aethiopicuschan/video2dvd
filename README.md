# mp4todvd

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen?style=flat-square)](/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/aethiopicuschan/mp4todvd.svg)](https://pkg.go.dev/github.com/aethiopicuschan/mp4todvd)
[![Go Report Card](https://goreportcard.com/badge/github.com/aethiopicuschan/mp4todvd)](https://goreportcard.com/report/github.com/aethiopicuschan/mp4todvd)
[![CI](https://github.com/aethiopicuschan/mp4todvd/actions/workflows/ci.yaml/badge.svg)](https://github.com/aethiopicuschan/mp4todvd/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/aethiopicuschan/mp4todvd/graph/badge.svg?token=hUdXoeMoFl)](https://codecov.io/gh/aethiopicuschan/mp4todvd)

複数の MP4 から DVD-Video ディレクトリを作成するための Go ライブラリ API です。外部コマンドは起動しません。

## 現在の実装

`Converter` は入力ファイルの検証、クリップごとの変換処理、DVD オーサリング処理、出力の公開を管理します。中間ファイルは出力先と同じ親ディレクトリに一時作成し、処理がすべて成功した場合だけ `VIDEO_TS` ディレクトリを出力先へ移動します。

MP4 のデコード、DVD 用 MPEG-2/音声へのエンコード、および IFO/VOB オーサリングは、`Encoder` と `Authorer` の実装として渡してください。このリポジトリにはこれらのメディアバックエンドは含まれていないため、`Converter` 単体では MP4 を変換できません。バックエンドは外部プロセスを使わず Go 内で処理する実装を想定しています。

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

出力先は既存でないパスを指定します。成功時は `disc/VIDEO_TS` 以下に DVD-Video ファイルが作成されます。エンコーダーの出力は MPEG-2 映像と AC-3、LPCM、または MPEG-1 Layer II 音声で、解像度とフレームレートは選択した DVD 規格に適合する必要があります。
