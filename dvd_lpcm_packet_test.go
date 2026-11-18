package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
	"testing"
	"time"
)

func TestDVDLPCMPacketWriterFramesAndTimestamps(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewDVDLPCMPacketWriter(&output, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]int16, 79*2)
	for i := range first {
		first[i] = int16(i - 60)
	}
	if err := writer.WriteChunk(PCMChunk{SampleRate: 48000, Channels: 2, Samples: first}); err != nil {
		t.Fatal(err)
	}
	second := make([]int16, 401*2)
	for i := range second {
		second[i] = int16(i + 100)
	}
	if err := writer.WriteChunk(PCMChunk{
		Start:      time.Duration(79) * time.Second / 48000,
		SampleRate: 48000,
		Channels:   2,
		Samples:    second,
	}); err != nil {
		t.Fatal(err)
	}
	last := make([]int16, 81*2)
	for i := range last {
		last[i] = -1234
	}
	if err := writer.WriteChunk(PCMChunk{
		Start:      10 * time.Millisecond,
		SampleRate: 48000,
		Channels:   2,
		Samples:    last,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close returned an error: %v", err)
	}
	if got, want := output.Len(), 2*DVDVOBSectorSize; got != want {
		t.Fatalf("output size = %d, want %d", got, want)
	}

	firstSector := output.Bytes()[:DVDVOBSectorSize]
	firstPayload := firstSector[28:]
	if firstSector[17] != 0xbd {
		t.Fatalf("first PES stream id = %#x, want private_stream_1", firstSector[17])
	}
	if firstPayload[0] != 0xa0 || firstPayload[1] != 6 || binary.BigEndian.Uint16(firstPayload[2:4]) != 0 {
		t.Fatalf("unexpected first LPCM packet prefix: % x", firstPayload[:4])
	}
	if firstPayload[4] != 0 || firstPayload[5] != 0x01 || firstPayload[6] != 0x80 {
		t.Fatalf("unexpected first LPCM format header: % x", firstPayload[4:7])
	}
	if got := binary.BigEndian.Uint16(firstPayload[7:9]); got != uint16(first[0]) {
		t.Fatalf("first LPCM sample = %d, want %d", int16(got), first[0])
	}
	if got := decodeTestMPEGTimestamp(firstSector[23:28]); got != 0 {
		t.Fatalf("first packet PTS = %d, want 0", got)
	}

	secondSector := output.Bytes()[DVDVOBSectorSize:]
	secondPayload := secondSector[28:]
	if secondPayload[0] != 0xa0 || secondPayload[1] != 2 || secondPayload[4] != 6 {
		t.Fatalf("unexpected final LPCM packet header: % x", secondPayload[:7])
	}
	if got := decodeTestMPEGTimestamp(secondSector[23:28]); got != 900 {
		t.Fatalf("second packet PTS = %d, want 900", got)
	}
	if got := binary.BigEndian.Uint16(secondPayload[7:9]); int16(got) != -1234 {
		t.Fatalf("first final-packet sample = %d, want -1234", int16(got))
	}
	if got := binary.BigEndian.Uint16(secondPayload[7+81*2*2:]); got != 0 {
		t.Fatalf("padded sample = %d, want silence", int16(got))
	}
}

func TestDVDLPCMPacketWriterValidatesInput(t *testing.T) {
	if _, err := NewDVDLPCMPacketWriter(&bytes.Buffer{}, 8); err == nil {
		t.Fatal("expected invalid stream number to fail")
	}
	writer, err := NewDVDLPCMPacketWriter(&bytes.Buffer{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteChunk(PCMChunk{SampleRate: 44100, Channels: 2, Samples: []int16{1, 2}}); err == nil {
		t.Fatal("expected unsupported sample rate to fail")
	}
	if err := writer.WriteChunk(PCMChunk{SampleRate: 48000, Channels: 2, Samples: []int16{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteChunk(PCMChunk{Start: time.Millisecond, SampleRate: 48000, Channels: 2, Samples: []int16{3, 4}}); err == nil {
		t.Fatal("expected a discontinuous timestamp to fail")
	}
}
