package video2dvd

import (
	"image"
	"testing"
	"time"

	vp9decoder "github.com/aethiopicuschan/go-vp9/decoder"
	"github.com/thesyncim/govpx"
)

func TestCopyGovpxFrameFromProfile0Decoder(t *testing.T) {
	const width, height = 16, 16
	encoder, err := govpx.NewVP9Encoder(govpx.VP9EncoderOptions{
		Width:  width,
		Height: height,
		FPS:    30,
	})
	if err != nil {
		t.Fatalf("create VP9 encoder: %v", err)
	}
	defer encoder.Close()

	src := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	for i := range src.Y {
		src.Y[i] = byte(i)
	}
	for i := range src.Cb {
		src.Cb[i] = 96
		src.Cr[i] = 160
	}
	packet := make([]byte, 64<<10)
	n, err := encoder.EncodeInto(src, packet)
	if err != nil {
		t.Fatalf("encode Profile 0 frame: %v", err)
	}
	if n <= 0 {
		t.Fatalf("encode Profile 0 frame returned %d bytes", n)
	}

	decoder := vp9decoder.New()
	frame, err := decoder.DecodeFrame(packet[:n])
	if err != nil {
		t.Fatalf("decode Profile 0 frame: %v", err)
	}
	if frame == nil {
		t.Fatal("Profile 0 decoder produced no frame")
	}

	got, err := copyVP9Frame(frame, 7*time.Millisecond, 33*time.Millisecond)
	if err != nil {
		t.Fatalf("copy govpx frame: %v", err)
	}
	if got.Width != width || got.Height != height || got.PTS != 7*time.Millisecond || got.Duration != 33*time.Millisecond {
		t.Fatalf("copied frame metadata = %dx%d PTS %s duration %s", got.Width, got.Height, got.PTS, got.Duration)
	}
	if len(got.Y) != width*height || len(got.U) != width*height/4 || len(got.V) != width*height/4 {
		t.Fatalf("copied plane lengths = %d/%d/%d", len(got.Y), len(got.U), len(got.V))
	}
}
