package video2dvd

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/arabian9ts/aac-go/adts"
	"github.com/rcarmo/go-264/audio/pcm"
)

func TestMakeAACADTSHeaderFromMP4AudioSpecificConfig(t *testing.T) {
	header, err := makeAACADTSHeader([]byte{0x12, 0x10}, 123)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := adts.Parse(append(header, make([]byte, 123)...))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Profile != 1 || parsed.SamplingFrequencyIndex != 4 || parsed.ChannelConfiguration != 2 || parsed.FrameLength != 130 {
		t.Fatalf("ADTS header = %+v", parsed)
	}
}

func TestAACPCMFileSourceAppliesEditTimeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pcm.raw")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for frame := int16(0); frame < 6; frame++ {
		var bytes [4]byte
		binary.LittleEndian.PutUint16(bytes[0:2], uint16(frame+1))
		binary.LittleEndian.PutUint16(bytes[2:4], uint16(-(frame + 1)))
		raw = append(raw, bytes[:]...)
	}
	if err := writeAll(file, raw); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	source := &aacPCMFileSource{
		file: file, info: pcm.Info{SampleRate: 44100, Channels: 2, Frames: 5},
		priming: 1, leading: 2,
	}
	got := make([]float64, 10)
	n, err := source.ReadFrames(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 0, 0, 0, 2.0 / 32768, -2.0 / 32768, 3.0 / 32768, -3.0 / 32768, 4.0 / 32768, -4.0 / 32768}
	if n != 5 || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadFrames = %d, %v; want 5, %v", n, got, want)
	}
	if err := source.SeekFrame(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	var next [2]float64
	n, err = source.ReadFrames(context.Background(), next[:])
	if err != nil || n != 1 || next != [2]float64{3.0 / 32768, -3.0 / 32768} {
		t.Fatalf("read after seek = %d, %v, %v", n, next, err)
	}
}

func TestMakeAACADTSHeaderRejectsUnsupportedConfig(t *testing.T) {
	for _, config := range [][]byte{{}, {0x0a, 0x10}, {0x12, 0x18}, {0x16, 0x90}} {
		if _, err := makeAACADTSHeader(config, 32); err == nil {
			t.Errorf("accepted unsupported AudioSpecificConfig %x", config)
		}
	}
	if _, err := makeAACADTSHeader([]byte{0x12, 0x10}, 8193); err == nil {
		t.Fatal("accepted an access unit larger than the ADTS frame limit")
	}
}

func TestAudioVideoDecodersHonorCancellationBeforeOpeningInputs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pcmHandler := func(PCMChunk) error { return nil }
	if err := DecodeMP4Audio(ctx, "missing.mp4", nil, pcmHandler); !errors.Is(err, context.Canceled) {
		t.Fatalf("DecodeMP4Audio error = %v, want context.Canceled", err)
	}
	videoHandler := func(DecodedVideoFrame) error { return nil }
	if err := DecodeWebMVideo(ctx, "missing.webm", nil, videoHandler); !errors.Is(err, context.Canceled) {
		t.Fatalf("DecodeWebMVideo error = %v, want context.Canceled", err)
	}
}
