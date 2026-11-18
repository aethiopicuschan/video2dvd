package video2dvd

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func TestProbeMP4VersionZeroMDHD(t *testing.T) {
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], 2500)
	hdlr := make([]byte, 12)
	copy(hdlr[8:12], []byte("vide"))
	mdia := mp4TestBox("mdia", mp4TestBox("mdhd", mdhd), mp4TestBox("hdlr", hdlr))
	trak := mp4TestBox("trak", mdia)
	data := append(mp4TestBox("ftyp", []byte("isom\x00\x00\x02\x00")), mp4TestBox("moov", trak)...)
	info, err := ProbeMP4Reader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Tracks) != 1 || info.Tracks[0].Duration != 2500*time.Millisecond || info.Tracks[0].TimeScale != 1000 {
		t.Fatalf("tracks = %+v", info.Tracks)
	}
}

func TestParseMediaDurationRejectsShortMDHDVersions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version byte
		payload int
	}{
		{name: "version0", version: 0, payload: 23},
		{name: "version1", version: 1, payload: 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.payload)
			data[0] = tc.version
			_, _, err := parseMediaDuration(bytes.NewReader(data), mp4Box{data: 0, end: int64(len(data))})
			if err == nil || err != io.ErrUnexpectedEOF {
				t.Fatalf("error = %v, want unexpected EOF", err)
			}
		})
	}
}

func mp4TestBox(kind string, payload ...[]byte) []byte {
	var body []byte
	for _, part := range payload {
		body = append(body, part...)
	}
	box := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(box[:4], uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], body)
	return box
}
