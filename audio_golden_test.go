package video2dvd_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// singleAudioGoldenHash is the SHA-256 of every file the conversion in
// TestSingleAudioOutputIsUnchanged authors, as v1.1.0 wrote them on
// darwin/arm64. The encoders compute in floating point, which the compiler
// fuses into multiply-adds on arm64 and not on amd64, so the hash is only
// compared on arm64.
const singleAudioGoldenHash = "d3a01d616e9cc0f269aa7eec9a4564bee06f23802ef4f648abe2c9dbb4913ca3"

// A title with one audio track is written exactly as before titles could
// have several: LPCM substream 0xa0, one unlabelled audio stream in the
// IFOs, and on arm64 the same files byte for byte as v1.1.0.
func TestSingleAudioOutputIsUnchanged(t *testing.T) {
	media, blocks := testWebMMedia(t, true)
	webm := writeTestWebM(t, "title.webm", media, blocks, time.Second)
	aac, err := filepath.Abs(filepath.Join("testdata", "aac_lc_44100_stereo.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	files := convertTestDVD(t, []MediaInput{{VideoPath: webm}, {VideoPath: webm, AudioPath: aac}}, Options{})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	hash := sha256.New()
	for _, name := range names {
		hash.Write([]byte(name))
		hash.Write(files[name])
	}
	got := hex.EncodeToString(hash.Sum(nil))
	t.Logf("files %s, hash %s", strings.Join(names, " "), got)
	for _, vts := range []string{"VTS_01", "VTS_02"} {
		ifo := files[vts+"_0.IFO"]
		if attributes := ifo[0x202:0x20c]; !bytes.Equal(attributes, []byte{0, 1, 0x80, 0x01, 0, 0, 0, 0, 0, 0}) {
			t.Fatalf("%s audio attributes % x, want one unlabelled LPCM stream", vts, attributes)
		}
		if control := testTitlePGC(ifo)[0x0c:0x10]; !bytes.Equal(control, []byte{0x80, 0, 0, 0}) {
			t.Fatalf("%s audio control % x", vts, control)
		}
		for _, pack := range parseTestVOB(t, files[vts+"_1.VOB"]) {
			if pack.streamID == 0xbd && pack.payload[0]&0xe0 != 0x20 && pack.payload[0] != 0xa0 {
				t.Fatalf("%s has audio substream %#x", vts, pack.payload[0])
			}
			if pack.nav {
				if sync := pack.raw[0x407+0x192+2 : 0x407+0x192+16]; !bytes.Equal(sync, make([]byte, 14)) {
					t.Fatalf("%s NAV pack states A_SYNCA for absent streams: % x", vts, sync)
				}
			}
		}
	}
	if runtime.GOARCH != "arm64" {
		t.Skipf("the golden hash was recorded on arm64; this is %s", runtime.GOARCH)
	}
	if got != singleAudioGoldenHash {
		t.Fatalf("authored files hash to %s, want %s", got, singleAudioGoldenHash)
	}
}
