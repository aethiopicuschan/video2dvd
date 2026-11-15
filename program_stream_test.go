package mp4todvd

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestMPEGProgramStreamWriterUsesFixedSectors(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x55}, 5000)
	if err := writer.WritePES(0xe0, time.Second, payload); err != nil {
		t.Fatal(err)
	}
	if got := output.Len(); got != 3*DVDVOBSectorSize {
		t.Fatalf("output size = %d, want %d", got, 3*DVDVOBSectorSize)
	}
	sectors := output.Bytes()
	for offset := 0; offset < len(sectors); offset += DVDVOBSectorSize {
		if !bytes.Equal(sectors[offset:offset+4], []byte{0, 0, 1, 0xba}) {
			t.Fatalf("sector at offset %d does not start with a pack header", offset)
		}
	}
	if !bytes.Equal(sectors[14:18], []byte{0, 0, 1, 0xe0}) {
		t.Fatal("first pack does not contain a video PES packet")
	}
	pesLength := int(binary.BigEndian.Uint16(sectors[18:20]))
	if pesLength != 3+5+2014 {
		t.Fatalf("first PES length = %d, want %d", pesLength, 3+5+2014)
	}
	if got := decodeTestMPEGTimestamp(sectors[23:28]); got != 90_000 {
		t.Fatalf("first PES PTS = %d, want 90000", got)
	}
}

func TestMPEGProgramStreamWriterRejectsInvalidIDsAndRegressingPTS(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WritePES(0xbe, time.Second, []byte{1}); err == nil {
		t.Fatal("expected padding stream id to be rejected")
	}
	if err := writer.WritePES(0xe0, time.Second, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WritePES(0xc0, 0, []byte{2}); err == nil {
		t.Fatal("expected regressing PTS to be rejected")
	}
	if writer.closedErr != nil {
		t.Fatalf("timestamp validation poisoned the writer: %v", writer.closedErr)
	}
}

func TestMPEGProgramStreamWriterClosesWithProgramEndCode(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WritePES(0xe0, 0, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.Len(), 2*DVDVOBSectorSize; got != want {
		t.Fatalf("output size = %d, want %d", got, want)
	}
	last := output.Bytes()[DVDVOBSectorSize:]
	if !bytes.Equal(last[:4], []byte{0, 0, 1, 0xba}) || !bytes.Equal(last[len(last)-4:], []byte{0, 0, 1, 0xb9}) {
		t.Fatal("final sector does not contain a pack header and program_end_code")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close returned an error: %v", err)
	}
	if err := writer.WritePES(0xe0, 0, []byte{4}); err == nil {
		t.Fatal("expected a write after Close to fail")
	}
}

func decodeTestMPEGTimestamp(value []byte) uint64 {
	return (uint64(value[0]&0x0e) << 29) |
		(uint64(value[1]) << 22) |
		(uint64(value[2]&0xfe) << 14) |
		(uint64(value[3]) << 7) |
		uint64(value[4]>>1)
}
