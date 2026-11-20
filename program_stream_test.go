package video2dvd_test

import (
	"bytes"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
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
	firstPack := sectors[:DVDVOBSectorSize]
	if got := uint32(firstPack[10])<<14 | uint32(firstPack[11])<<6 | uint32(firstPack[12]>>2); got != 25_200 {
		t.Fatalf("program mux rate = %d units of 50 bytes/s, want DVD rate 25200 (10.08 Mbit/s)", got)
	}
	if !bytes.Equal(sectors[14:18], []byte{0, 0, 1, 0xe0}) {
		t.Fatal("first pack does not contain a video PES packet")
	}
	pesLength := int(binary.BigEndian.Uint16(sectors[18:20]))
	// A full pack carries no padding packet: 2048 - 14 (pack) - 9 (PES) - 5 (PTS).
	if pesLength != 3+5+2020 {
		t.Fatalf("first PES length = %d, want %d", pesLength, 3+5+2020)
	}
	if got := decodeTestMPEGTimestamp(sectors[23:28]); got != 90_000 {
		t.Fatalf("first PES PTS = %d, want 90000", got)
	}
}

func TestMPEGProgramStreamWriterKeepsClockAlignedWithDVDMaximumRate(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the highest configured video rate together with DVD LPCM:
	// an 8 Mbit/s video frame every 30000/1001 seconds and 10 ms audio PES.
	const videoFrames = 90
	const audioPackets = 300
	videoPayload := make([]byte, 33_366)
	audioPayload := make([]byte, 1_927)
	frame, audioPacket := 0, 0
	for frame < videoFrames || audioPacket < audioPackets {
		videoPTS := time.Duration(frame) * time.Second * 1001 / 30000
		audioPTS := time.Duration(audioPacket) * 10 * time.Millisecond
		if audioPacket < audioPackets && (frame == videoFrames || audioPTS < videoPTS) {
			if err := writer.WritePES(0xbd, audioPTS, audioPayload); err != nil {
				t.Fatal(err)
			}
			audioPacket++
			continue
		}
		if err := writer.WritePES(0xe0, videoPTS, videoPayload); err != nil {
			t.Fatal(err)
		}
		frame++
	}

	var maxSCRLead uint64
	sectors := output.Bytes()
	for offset := 0; offset < len(sectors); offset += DVDVOBSectorSize {
		sector := sectors[offset : offset+DVDVOBSectorSize]
		if !bytes.Equal(sector[14:17], []byte{0, 0, 1}) {
			continue
		}
		streamID := sector[17]
		if streamID != 0xbd && streamID != 0xe0 || sector[21]&0xc0 != 0x80 {
			continue
		}
		clock := decodeTestMPEG2SCR(sector[4:14])
		pts := decodeTestMPEGTimestamp(sector[23:28])
		if clock > pts {
			maxSCRLead = max(maxSCRLead, clock-pts)
		}
	}
	const maxAllowedSCRLead = 3_000 // one frame at the DVD NTSC frame rate.
	if maxSCRLead > maxAllowedSCRLead {
		t.Fatalf("SCR leads PES timestamps by %d ticks; want at most %d ticks", maxSCRLead, maxAllowedSCRLead)
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
	if ExportProgramStreamClosedError(writer) != nil {
		t.Fatalf("timestamp validation poisoned the writer: %v", ExportProgramStreamClosedError(writer))
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

func decodeTestMPEG2SCR(value []byte) uint64 {
	readBits := func(offset, count int) uint64 {
		var result uint64
		for bit := offset; bit < offset+count; bit++ {
			result = result<<1 | uint64(value[bit/8]>>(7-bit%8)&1)
		}
		return result
	}
	return readBits(2, 3)<<30 | readBits(6, 15)<<15 | readBits(22, 15)
}

func TestMPEGProgramStreamWriterClockFollowsBytesNotTimestamps(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	// Three packs of video stamped 100 ms, then audio stamped 150 ms. The
	// clock must start at zero and count sectors at 1.26 MB/s; it must not
	// jump to either timestamp.
	if err := writer.WritePES(0xe0, 100*time.Millisecond, make([]byte, 5000)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WritePES(0xbd, 150*time.Millisecond, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	sectors := output.Bytes()
	if len(sectors) != 4*DVDVOBSectorSize {
		t.Fatalf("output size = %d, want four sectors", len(sectors))
	}
	for index := range 4 {
		header := sectors[index*DVDVOBSectorSize+4 : index*DVDVOBSectorSize+14]
		got := decodeTestMPEG2SCR(header)*300 + decodeTestMPEG2SCRExtension(header)
		want := uint64(index) * DVDVOBSectorSize * 27_000_000 / 1_260_000
		if got != want {
			t.Fatalf("pack %d SCR = %d (27 MHz), want %d", index, got, want)
		}
		if header[9] != 0xf8 {
			t.Fatalf("pack %d reserved/stuffing byte = %#x, want 0xf8", index, header[9])
		}
	}
	// 2048 bytes take 43885.71 ticks: the 27 MHz extension must be in use.
	if decodeTestMPEG2SCRExtension(sectors[DVDVOBSectorSize+4:DVDVOBSectorSize+14]) != 43_885%300 {
		t.Fatal("second pack does not carry the SCR extension")
	}
}

func TestMPEGProgramStreamWriterWaitsForSparseStreams(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	// With nothing to send the clock may idle, but only up to the point
	// where the packet is still 0.3 s ahead of its presentation time.
	if err := writer.WritePES(0xbd, 2*time.Second, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if got := decodeTestMPEG2SCR(output.Bytes()[4:14]); got != 180_000-27_000 {
		t.Fatalf("SCR = %d, want %d (0.3 s before the PTS)", got, 180_000-27_000)
	}
}

func TestMPEGProgramStreamWriterRoundsTimestampsToTheNearestTick(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewMPEGProgramStreamWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	for frame := range 30 {
		// 1001/30000 s is not a whole number of nanoseconds.
		if err := writer.WritePES(0xe0, time.Duration(frame)*time.Second*1001/30000, []byte{1}); err != nil {
			t.Fatal(err)
		}
		sector := output.Bytes()[frame*DVDVOBSectorSize:]
		if got := decodeTestMPEGTimestamp(sector[23:28]); got != uint64(frame)*3003 {
			t.Fatalf("frame %d PTS = %d, want %d", frame, got, frame*3003)
		}
	}
}

func TestMPEGProgramStreamWriterCompletesShortPacks(t *testing.T) {
	const capacity = DVDVOBSectorSize - 14 - 9 - 5
	for _, test := range []struct {
		spare    int
		stuffing int
	}{{0, 0}, {1, 1}, {7, 7}, {8, 0}, {500, 0}} {
		var output bytes.Buffer
		writer, err := NewMPEGProgramStreamWriter(&output)
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{0x55}, capacity-test.spare)
		if err := writer.WritePES(0xe0, 0, payload); err != nil {
			t.Fatal(err)
		}
		sector := output.Bytes()
		if len(sector) != DVDVOBSectorSize {
			t.Fatalf("spare %d: wrote %d bytes, want one sector", test.spare, len(sector))
		}
		// A gap too small for a padding packet is absorbed by PES header
		// stuffing; anything larger becomes a padding packet.
		if got := int(sector[22]); got != 5+test.stuffing {
			t.Fatalf("spare %d: PES header data length = %d, want %d", test.spare, got, 5+test.stuffing)
		}
		for _, value := range sector[28 : 28+test.stuffing] {
			if value != 0xff {
				t.Fatalf("spare %d: stuffing byte = %#x", test.spare, value)
			}
		}
		end := 20 + int(binary.BigEndian.Uint16(sector[18:20]))
		if !bytes.Equal(sector[23+int(sector[22]):end], payload) {
			t.Fatalf("spare %d: payload is not intact", test.spare)
		}
		padding := DVDVOBSectorSize - end
		if wantPadding := test.spare - test.stuffing; padding != wantPadding {
			t.Fatalf("spare %d: %d bytes follow the PES packet, want %d", test.spare, padding, wantPadding)
		}
		if padding != 0 && (!bytes.Equal(sector[end:end+4], []byte{0, 0, 1, 0xbe}) || int(binary.BigEndian.Uint16(sector[end+4:end+6])) != padding-6) {
			t.Fatalf("spare %d: sector is not completed by a padding packet", test.spare)
		}
	}
}
