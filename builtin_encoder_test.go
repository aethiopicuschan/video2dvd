package video2dvd_test

import (
	"bytes"
	"errors"
	. "github.com/aethiopicuschan/video2dvd"
	"testing"
	"time"
)

func TestTrackDisplayAspectIncludesPixelAspect(t *testing.T) {
	gotNumerator, gotDenominator, err := ExportTrackDisplayAspect(TrackInfo{
		Width:                 1920,
		Height:                1080,
		PixelAspectHorizontal: 4,
		PixelAspectVertical:   3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotNumerator != 64 || gotDenominator != 27 {
		t.Fatalf("ExportTrackDisplayAspect() = %d/%d, want 64/27", gotNumerator, gotDenominator)
	}
}

func TestCheckEstimatedTitleSize(t *testing.T) {
	// 1,000,000 video bytes and 192,000 LPCM bytes plus 1/32 multiplex overhead.
	if err := ExportCheckEstimatedTitleSize(time.Second, 1_229_250, ExportMPEG2VideoBitrate); err != nil {
		t.Fatalf("one-second estimate should fit exactly: %v", err)
	}
	if err := ExportCheckEstimatedTitleSize(time.Second, 1_229_249, ExportMPEG2VideoBitrate); !errors.Is(err, ErrDiscCapacity) {
		t.Fatalf("one-byte undersized capacity should fail with ErrDiscCapacity, got %v", err)
	}
}

func TestSizeLimitedWriterStopsAtLimit(t *testing.T) {
	var output bytes.Buffer
	writer := ExportNewSizeLimitedWriter(&output, 4)
	if n, err := writer.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatalf("first write = %d, %v; want 2, nil", n, err)
	}
	if n, err := writer.Write([]byte("cde")); n != 2 || !errors.Is(err, ErrDiscCapacity) {
		t.Fatalf("second write = %d, %v; want 2, ErrDiscCapacity", n, err)
	}
	if got := output.String(); got != "abcd" {
		t.Fatalf("limited output = %q, want %q", got, "abcd")
	}
}
