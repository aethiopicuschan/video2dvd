package mp4todvd

import (
	"bytes"
	"context"
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
	reader := mpeg2PictureReader{src: &limitedReadSizeReader{data: stream, limit: 2}}
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
	profile, err := (Options{Standard: VideoStandardNTSC, Aspect: AspectRatio16x9}).profile()
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
	if err := encoder.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	frame = cloneDecodedVideoFrame(frame)
	frame.Y[100*frame.Width+100] = 140
	if err := encoder.WriteFrame(frame); err != nil {
		t.Fatal(err)
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
		Duration:             2 * time.Second * 1001 / 30000,
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
	for offset := 0; offset < vob.Len(); offset += DVDVOBSectorSize {
		sector := vob.Bytes()[offset : offset+DVDVOBSectorSize]
		if !bytes.Equal(sector[:4], []byte{0, 0, 1, 0xba}) {
			t.Fatalf("sector %d has no MPEG pack header", offset/DVDVOBSectorSize)
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
