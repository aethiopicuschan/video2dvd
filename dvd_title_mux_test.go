package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	. "github.com/aethiopicuschan/video2dvd"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMPEG2PictureReaderPreservesPrefixAcrossShortReads(t *testing.T) {
	stream := []byte{
		0, 0, 1, 0xb3, 0x11, 0x22,
		0, 0, 1, 0x00, 0x33, 0x44,
		0, 0, 1, 0x01, 0x55,
		0, 0, 1, 0x00, 0x66, 0x77,
		0, 0, 1, 0xb7,
	}
	reader := *ExportNewMPEG2PictureReader(&limitedReadSizeReader{data: stream, limit: 2})
	first, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := stream[:17]
	if !bytes.Equal(first, wantFirst) {
		t.Fatalf("first picture = % x, want % x", first, wantFirst)
	}
	second, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, stream[17:]) {
		t.Fatalf("second picture = % x, want % x", second, stream[17:])
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("third picture error = %v, want EOF", err)
	}
}

func TestMuxDVDTitleVOBInterleavesVideoAndLPCM(t *testing.T) {
	profile, err := ExportProfile(Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	videoPath := filepath.Join(workDir, "video.m2v")
	videoFile, err := os.Create(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := NewMPEG2VideoEncoder(videoFile, profile)
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width:    profile.FrameWidth,
		Height:   profile.FrameHeight,
		Duration: time.Second * 1001 / 30000,
		Y:        bytes.Repeat([]byte{80}, profile.FrameWidth*profile.FrameHeight),
		U:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V:        bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	for i := range 16 {
		if i == 1 {
			frame = ExportCloneDecodedVideoFrame(frame)
			frame.Y[100*frame.Width+100] = 140
		}
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := videoFile.Close(); err != nil {
		t.Fatal(err)
	}

	audioPath := filepath.Join(workDir, "audio.lpcm")
	audio := make([]int16, 960*2)
	for i := range audio {
		audio[i] = int16(i * 3)
	}
	var audioBytes bytes.Buffer
	if err := WriteDVDLPCM(&audioBytes, PCMChunk{SampleRate: 48000, Channels: 2, Samples: audio}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audioPath, audioBytes.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	title := EncodedTitle{
		VideoPath:            "video.m2v",
		AudioPath:            "audio.lpcm",
		VideoCodec:           VideoCodecMPEG2,
		AudioCodec:           AudioCodecLPCM,
		Aspect:               profile.Aspect,
		Width:                profile.FrameWidth,
		Height:               profile.FrameHeight,
		FrameRateNumerator:   30000,
		FrameRateDenominator: 1001,
		Duration:             16 * time.Second * 1001 / 30000,
	}
	var vob bytes.Buffer
	if err := MuxDVDTitleVOB(context.Background(), title, workDir, &vob); err != nil {
		t.Fatal(err)
	}
	if vob.Len() == 0 || vob.Len()%DVDVOBSectorSize != 0 {
		t.Fatalf("VOB size = %d, want nonzero DVD sectors", vob.Len())
	}
	var streamIDs []byte
	var timestamps []uint64
	var navAddresses []uint32
	var navEnds []uint32
	var navNextVOBUs []uint32
	var navPreviousVOBUs []uint32
	var navCellTimes [][4]byte
	for offset := 0; offset < vob.Len(); offset += DVDVOBSectorSize {
		sector := vob.Bytes()[offset : offset+DVDVOBSectorSize]
		if !bytes.Equal(sector[:4], []byte{0, 0, 1, 0xba}) {
			t.Fatalf("sector %d has no MPEG pack header", offset/DVDVOBSectorSize)
		}
		if bytes.Equal(sector[0x400:0x404], []byte{0, 0, 1, 0xbf}) && sector[0x406] == 1 {
			dsi := sector[0x407:]
			navAddresses = append(navAddresses, binary.BigEndian.Uint32(dsi[4:8]))
			navEnds = append(navEnds, binary.BigEndian.Uint32(dsi[8:12]))
			navNextVOBUs = append(navNextVOBUs, binary.BigEndian.Uint32(dsi[0x13a:0x13e]))
			navPreviousVOBUs = append(navPreviousVOBUs, binary.BigEndian.Uint32(dsi[0x13e:0x142]))
			var elapsed [4]byte
			copy(elapsed[:], sector[65:69])
			navCellTimes = append(navCellTimes, elapsed)
		}
		if !bytes.Equal(sector[14:17], []byte{0, 0, 1}) {
			continue
		}
		streamID := sector[17]
		if streamID != 0xbd && streamID != 0xe0 {
			continue
		}
		streamIDs = append(streamIDs, streamID)
		if sector[21]&0x80 != 0 {
			timestamps = append(timestamps, decodeTestMPEGTimestamp(sector[23:28]))
		}
	}
	if !bytes.Contains(streamIDs, []byte{0xe0}) || !bytes.Contains(streamIDs, []byte{0xbd}) {
		t.Fatalf("VOB stream IDs = % x, want MPEG-2 video and private LPCM PES", streamIDs)
	}
	if len(navAddresses) != 2 || navAddresses[0] != 0 || navAddresses[1] != navEnds[0]+1 {
		t.Fatalf("NAV sectors addresses=%v ends=%v, want two linked VOBUs", navAddresses, navEnds)
	}
	nextVOBUOffset := navAddresses[1] - navAddresses[0]
	if navNextVOBUs[0] != 0x80000000|nextVOBUOffset || navNextVOBUs[1] != DVDNAVSRIEndOfCell {
		t.Fatalf("next VOBU pointers = %#v, want first offset %#x and final end-of-cell %#x", navNextVOBUs, 0x80000000|nextVOBUOffset, DVDNAVSRIEndOfCell)
	}
	if navPreviousVOBUs[0] != DVDNAVSRIEndOfCell || navPreviousVOBUs[1] != 0x80000000|nextVOBUOffset {
		t.Fatalf("previous VOBU pointers = %#v, want first end-of-cell %#x and previous offset %#x", navPreviousVOBUs, DVDNAVSRIEndOfCell, 0x80000000|nextVOBUOffset)
	}
	if navCellTimes[1] != [4]byte{0x00, 0x00, 0x00, 0xcf} {
		t.Fatalf("second NAV cell elapsed time = % x, want NTSC frame 15", navCellTimes[1])
	}
	if len(timestamps) < 4 {
		t.Fatalf("found only %d PES timestamps: %v", len(timestamps), timestamps)
	}
	for i := 1; i < len(timestamps); i++ {
		if timestamps[i] < timestamps[i-1] {
			t.Fatalf("PES timestamps regress: %v", timestamps)
		}
	}
	if !bytes.Equal(vob.Bytes()[vob.Len()-4:], []byte{0, 0, 1, 0xb9}) {
		t.Fatal("VOB does not end with program_end_code")
	}
}

type limitedReadSizeReader struct {
	data  []byte
	limit int
}

func (r *limitedReadSizeReader) Read(dst []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(dst) > r.limit {
		dst = dst[:r.limit]
	}
	n := copy(dst, r.data)
	r.data = r.data[n:]
	return n, nil
}
