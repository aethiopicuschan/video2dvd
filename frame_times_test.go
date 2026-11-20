package video2dvd_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

func mp4TestUint32s(values ...uint32) []byte {
	data := make([]byte, 4*len(values))
	for i, value := range values {
		binary.BigEndian.PutUint32(data[4*i:], value)
	}
	return data
}

// writeTestTimedMP4 writes an MP4 file whose video track, of 24000 ticks per
// second, has one-byte samples with the given durations in decoding order.
// Like a stream with two B pictures between reference pictures, every third
// sample after the first is presented two places later than it is decoded.
// The track's edit list starts at media time editStart.
func writeTestTimedMP4(t *testing.T, durations []uint32, editStart uint32) string {
	t.Helper()
	count := uint32(len(durations))
	stts := mp4TestUint32s(0, count)
	ctts := mp4TestUint32s(0, count)
	// Decoding times and, from them, presentation times: sample 3k+1 takes
	// the place of sample 3k+3 and the two after it move up.
	decode := make([]uint32, count+1)
	for i, duration := range durations {
		stts = append(stts, mp4TestUint32s(1, duration)...)
		decode[i+1] = decode[i] + duration
	}
	present := make([]uint32, count)
	for i := range count {
		present[i] = i
	}
	for i := uint32(1); i+2 < count; i += 3 {
		present[i], present[i+1], present[i+2] = i+2, i, i+1
	}
	for i := range count {
		// Presentation is a sample period behind decoding, so that no
		// offset is negative.
		ctts = append(ctts, mp4TestUint32s(1, decode[present[i]]+durations[0]-decode[i])...)
	}
	ftyp := mp4TestBox("ftyp", []byte("isom\x00\x00\x02\x00"))
	mdat := mp4TestBox("mdat", make([]byte, count))
	stbl := mp4TestBox("stbl",
		mp4TestBox("stsd", mp4TestUint32s(0, 1), mp4TestBox("avc1", make([]byte, 78), mp4TestBox("avcC", []byte{1, 100, 0, 30, 0xff, 0xe0, 0}))),
		mp4TestBox("stts", stts),
		mp4TestBox("ctts", ctts),
		mp4TestBox("stsc", mp4TestUint32s(0, 1, 1, count, 1)),
		mp4TestBox("stsz", mp4TestUint32s(0, 1, count)),
		mp4TestBox("stco", mp4TestUint32s(0, 1, uint32(len(ftyp)+8))),
	)
	hdlr := make([]byte, 12)
	copy(hdlr[8:12], "vide")
	mdia := mp4TestBox("mdia", mp4TestBox("mdhd", mp4TestUint32s(0, 0, 0, 24000, decode[count], 0)), mp4TestBox("hdlr", hdlr), mp4TestBox("minf", stbl))
	edts := mp4TestBox("edts", mp4TestBox("elst", mp4TestUint32s(0, 1, decode[count]/24, editStart, 0x10000)))
	moov := mp4TestBox("moov", mp4TestBox("mvhd", mp4TestUint32s(0, 0, 0, 1000, decode[count]/24)), mp4TestBox("trak", edts, mdia))
	path := filepath.Join(t.TempDir(), "timed.mp4")
	if err := os.WriteFile(path, append(append(ftyp, mdat...), moov...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The frame times of an MP4 track come from its sample tables, in
// presentation order whatever the decoding order, and decide about film for
// the whole track.
func TestMP4VideoFrameTimes(t *testing.T) {
	film := make([]uint32, 100)
	for i := range film {
		film[i] = 1000
	}
	mixed := append([]uint32(nil), film...)
	for range 120 {
		mixed = append(mixed, 800)
	}
	for _, tc := range []struct {
		name      string
		durations []uint32
		editStart uint32
		// first is the first frame time in ticks and count the number of
		// frames.
		first, count int
		film         bool
	}{
		{"film", film, 1000, 1000, 100, true},
		// Three frames end before the edit starts and are not shown.
		{"film, edit starts later", film, 4500, 4000, 97, true},
		{"film, then 30 fps", mixed, 1000, 1000, 220, false},
	} {
		path := writeTestTimedMP4(t, tc.durations, tc.editStart)
		times, err := ExportMP4VideoFrameTimes(path, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(times) != tc.count || times[0] != time.Duration(tc.first)*time.Second/24000 {
			t.Fatalf("%s: %d frames from %v, want %d from tick %d", tc.name, len(times), times[0], tc.count, tc.first)
		}
		for index := 1; index < len(times); index++ {
			// Decoding order would step back by two frames.
			want := time.Duration(tc.durations[index-1+len(tc.durations)-tc.count]) * time.Second / 24000
			if step := times[index] - times[index-1]; step < want-time.Microsecond || step > want+time.Microsecond {
				t.Fatalf("%s: frame %d comes %v after the one before it, want %v", tc.name, index, step, want)
			}
		}
		if got := ExportIsFilmCadence(times); got != tc.film {
			t.Fatalf("%s: film=%v, want %v", tc.name, got, tc.film)
		}
	}
	if _, err := ExportMP4VideoFrameTimes(filepath.Join(t.TempDir(), "missing.mp4"), nil); err == nil {
		t.Fatal("frame times were read from a file that does not exist")
	}
}
