package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// testAC3Stream returns frames AC-3 syncframes of a 48 kHz stereo stream at
// the given bitrate: valid headers, and payloads that carry the stream and
// frame number so that they can be told apart after multiplexing.
func testAC3Stream(stream, frames, kbps int) []byte {
	sizes := map[int]int{192: 10, 224: 11, 256: 12, 320: 13, 384: 14, 448: 15}
	size := kbps * 4
	var data []byte
	for k := range frames {
		frame := make([]byte, size)
		frame[0], frame[1] = 0x0b, 0x77
		frame[4] = byte(2 * sizes[kbps]) // fscod 0: 48 kHz.
		frame[5] = 8 << 3                // bsid 8, bsmod 0.
		frame[6] = 2 << 5                // acmod 2, dsurmod 0, lfeon 0.
		for i := 8; i < size; i++ {
			frame[i] = byte(stream*67 + k*13 + i)
		}
		data = append(data, frame...)
	}
	return data
}

// testAC3Title encodes the video of a short WebM file and gives it AC-3
// audio streams with the given bitrates and languages instead of its LPCM.
func testAC3Title(t *testing.T, profile Profile, kbps []int, languages []string) (EncodedTitle, [][]byte) {
	t.Helper()
	media, blocks := testWebMMedia(t, false)
	source := writeTestWebM(t, "title.webm", media, blocks, time.Second)
	workDir := t.TempDir()
	title, err := NewInProcessEncoder().EncodeStreams(context.Background(), MediaInput{VideoPath: source, AudioPath: source}, workDir, profile)
	if err != nil {
		t.Fatal(err)
	}
	title.VideoPath = filepath.Join(workDir, title.VideoPath)
	var streams [][]byte
	title.Audio = nil
	for i, rate := range kbps {
		// A little more audio than video, as the encoder pads to whole
		// syncframes.
		data := testAC3Stream(i, int(title.Duration/(32*time.Millisecond))+1, rate)
		path := filepath.Join(workDir, "audio-"+languages[i]+".ac3")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		streams = append(streams, data)
		title.Audio = append(title.Audio, EncodedAudio{Path: path, Codec: AudioCodecAC3, Language: languages[i]})
	}
	title.AudioPath, title.AudioCodec = title.Audio[0].Path, title.Audio[0].Codec
	return title, streams
}

// Several AC-3 streams are multiplexed on substreams 0x80 and up, each
// syncframe timed by its position in the stream, within the 4 KiB audio
// buffer, with A_SYNCA for every stream, and declared with their languages
// in the IFOs and the PGC's audio control.
func TestAuthorMultiplexesAC3Streams(t *testing.T) {
	profile := testSubtitleProfile(t, Options{})
	title, streams := testAC3Title(t, profile, []int{192, 448}, []string{"ja", "en"})
	output := t.TempDir()
	if err := NewInProcessAuthorer().Author(context.Background(), []EncodedTitle{title}, output, profile); err != nil {
		t.Fatal(err)
	}
	ifo := readAuthorerTestFile(t, output, "VTS_01_0.IFO")
	wantAttributes := []byte{0, 2, 0x04, 0x01, 'j', 'a', 0, 0, 0, 0, 0x04, 0x01, 'e', 'n', 0, 0, 0, 0}
	if got := ifo[0x202 : 0x202+len(wantAttributes)]; !bytes.Equal(got, wantAttributes) {
		t.Fatalf("VTS audio attributes % x, want % x", got, wantAttributes)
	}
	vmg := readAuthorerTestFile(t, output, "VIDEO_TS.IFO")
	table := vmg[int(binary.BigEndian.Uint32(vmg[0xd0:]))*DVDVOBSectorSize:]
	entry := table[binary.BigEndian.Uint32(table[8:12]):]
	if got := entry[8+0x102 : 8+0x102+len(wantAttributes)]; !bytes.Equal(got, wantAttributes) {
		t.Fatalf("VMG_VTS_ATRT audio attributes % x, want % x", got, wantAttributes)
	}
	if control := testTitlePGC(ifo)[0x0c:0x12]; !bytes.Equal(control, []byte{0x80, 0, 0x81, 0, 0, 0}) {
		t.Fatalf("PGC audio control % x", control)
	}

	// AC-3 is presented 256 samples early, its encoder delay.
	const startTime = 27_000 + 3003 - 480
	packs := parseTestVOB(t, readAuthorerTestFile(t, output, "VTS_01_1.VOB"))
	type started struct {
		pts    uint64
		sector int
	}
	received := make([][]byte, len(streams))
	starts := make([][]started, len(streams))
	type buffered struct {
		pts   uint64
		bytes int
	}
	buffers := make([][]buffered, len(streams))
	for _, pack := range packs {
		if pack.streamID != 0xbd || pack.payload[0]&0xf8 != 0x80 {
			continue
		}
		stream := int(pack.payload[0] - 0x80)
		if stream >= len(streams) {
			t.Fatalf("unexpected AC-3 substream %#x", pack.payload[0])
		}
		count, pointer := int(pack.payload[1]), int(binary.BigEndian.Uint16(pack.payload[2:4]))
		data := pack.payload[4:]
		offset := len(received[stream])
		frameSize := []int{768, 1792}[stream]
		// Frame starts in this pack, from the stream itself.
		var wantStarts []int
		for position := (frameSize - offset%frameSize) % frameSize; position < len(data); position += frameSize {
			wantStarts = append(wantStarts, position)
		}
		if count != len(wantStarts) || count > 0 && pointer != wantStarts[0]+1 || count == 0 && pointer != 0 {
			t.Fatalf("stream %d pack at sector %d: %d frames from %d, want %v", stream, pack.sector, count, pointer, wantStarts)
		}
		if count > 0 {
			first := (offset + wantStarts[0]) / frameSize
			if !pack.hasPTS || pack.pts != startTime+uint64(first)*2880 {
				t.Fatalf("stream %d pack at sector %d PTS %d (%v), want syncframe %d at %d", stream, pack.sector, pack.pts, pack.hasPTS, first, startTime+first*2880)
			}
			for i := range wantStarts {
				starts[stream] = append(starts[stream], started{startTime + uint64(first+i)*2880, pack.sector})
			}
		} else if pack.hasPTS {
			t.Fatalf("stream %d pack at sector %d has a PTS but no syncframe start", stream, pack.sector)
		}
		// The decoder's buffer: bytes leave when their syncframe is
		// presented; what arrives must fit 4 KiB and be in time.
		now := pack.scr / 300
		buffers[stream] = slices.DeleteFunc(buffers[stream], func(b buffered) bool { return b.pts <= now })
		total := 0
		for i := range data {
			pts := startTime + uint64((offset+i)/frameSize)*2880
			if pts*300 < pack.scr+43_886 {
				t.Fatalf("stream %d byte %d arrives after its presentation", stream, offset+i)
			}
			buffers[stream] = append(buffers[stream], buffered{pts, 1})
		}
		for _, b := range buffers[stream] {
			total += b.bytes
		}
		if total > 4096 {
			t.Fatalf("stream %d holds %d bytes in its buffer at sector %d", stream, total, pack.sector)
		}
		received[stream] = append(received[stream], data...)
	}
	for stream := range streams {
		if !bytes.Equal(received[stream], streams[stream]) {
			t.Fatalf("stream %d: %d bytes multiplexed, want the %d of the file", stream, len(received[stream]), len(streams[stream]))
		}
	}
	// A_SYNCA of each stream points at the pack in which the syncframe
	// presented at the VOBU start begins.
	navs := 0
	for _, pack := range packs {
		if !pack.nav {
			continue
		}
		navs++
		start := uint64(binary.BigEndian.Uint32(pack.raw[0x39:]))
		for stream := range 8 {
			got := binary.BigEndian.Uint16(pack.raw[0x407+0x192+stream*2:])
			want := uint16(0)
			if stream < len(streams) {
				want = 0x3fff
				for _, s := range starts[stream] {
					if s.pts <= start && start < s.pts+2880 {
						if s.sector >= pack.sector {
							want = uint16(s.sector - pack.sector)
						} else {
							want = uint16(pack.sector-s.sector) | 0x8000
						}
					}
				}
			}
			if got != want {
				t.Fatalf("NAV at sector %d A_SYNCA of stream %d = %#x, want %#x", pack.sector, stream, got, want)
			}
		}
	}
	if navs < 2 {
		t.Fatalf("%d VOBUs", navs)
	}
}
