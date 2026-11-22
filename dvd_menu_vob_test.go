package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

// TestAuthoredMenuPagesAreSelfContainedVOBs checks the structure of the title
// menu that macOS DVD Player needs before it shows anything. It showed a
// black screen for a menu of one picture (with 2 ms of audio or with audio
// as long as the picture) and for 15 pictures with 2 ms of audio; it showed
// the menu for 15 pictures with audio as long as them. Every page is
// therefore a VOB of its own whose single VOBU holds 15 pictures ending with
// sequence_end_code and silent LPCM audio covering those pictures, and
// whose cell lasts exactly as long as the VOBU.
func TestAuthoredMenuPagesAreSelfContainedVOBs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		standard  VideoStandard
		aspect    AspectRatio
		titles    int
		wantPages int
		// The video attributes of the VMGM, and the playback time of a
		// page: 15 frames with the frame-rate code of the standard.
		wantVideoAttr uint16
		wantPageTime  []byte
		frameTicks    uint32
		audioSamples  int
	}{
		{name: "NTSC 4:3 one page", standard: VideoStandardNTSC, aspect: AspectRatio4x3, titles: 2, wantPages: 1,
			wantVideoAttr: 0x4000, wantPageTime: []byte{0, 0, 0, 0xc0 | 0x15}, frameTicks: 3003, audioSamples: 24024},
		{name: "NTSC 16:9 two pages", standard: VideoStandardNTSC, aspect: AspectRatio16x9, titles: 20, wantPages: 2,
			wantVideoAttr: 0x4e00, wantPageTime: []byte{0, 0, 0, 0xc0 | 0x15}, frameTicks: 3003, audioSamples: 24024},
		{name: "PAL 4:3 two pages", standard: VideoStandardPAL, aspect: AspectRatio4x3, titles: 37, wantPages: 2,
			wantVideoAttr: 0x5000, wantPageTime: []byte{0, 0, 0, 0x40 | 0x15}, frameTicks: 3600, audioSamples: 28800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: tc.aspect})
			if err != nil {
				t.Fatal(err)
			}
			title := makeMenuTestTitle(t, profile)
			titles := make([]EncodedTitle, tc.titles)
			for i := range titles {
				titles[i] = title
				titles[i].Name = fmt.Sprintf("clip-%d", i+1)
			}
			videoTS := t.TempDir()
			if err := NewInProcessAuthorer().Author(context.Background(), titles, videoTS, profile); err != nil {
				t.Fatal(err)
			}
			vmg := readAuthorerTestFile(t, videoTS, "VIDEO_TS.IFO")
			vob := readAuthorerTestFile(t, videoTS, "VIDEO_TS.VOB")

			// The VMGM attributes describe what the menu VOB carries: its
			// video, one 16-bit 48 kHz stereo LPCM stream and one
			// subpicture stream.
			if got := binary.BigEndian.Uint16(vmg[0x100:0x102]); got != tc.wantVideoAttr {
				t.Errorf("VMGM video attributes = %#04x, want %#04x", got, tc.wantVideoAttr)
			}
			if binary.BigEndian.Uint16(vmg[0x102:0x104]) != 1 || vmg[0x104] != 0x80 || vmg[0x105] != 0x01 {
				t.Errorf("VMGM audio = %d stream(s) % x, want one LPCM stream 80 01", binary.BigEndian.Uint16(vmg[0x102:0x104]), vmg[0x104:0x10c])
			}
			if got := binary.BigEndian.Uint16(vmg[0x154:0x156]); got != 1 {
				t.Errorf("VMGM subpicture streams = %d, want 1", got)
			}

			cadt := vmg[int(binary.BigEndian.Uint32(vmg[0xd8:0xdc]))*DVDVOBSectorSize:]
			admap := vmg[int(binary.BigEndian.Uint32(vmg[0xdc:0xe0]))*DVDVOBSectorSize:]
			if got := int(binary.BigEndian.Uint16(cadt[0:2])); got != tc.wantPages {
				t.Fatalf("menu C_ADT counts %d VOBs, want %d pages", got, tc.wantPages)
			}
			if got := int(binary.BigEndian.Uint32(cadt[4:8])); got != 8+12*tc.wantPages-1 {
				t.Fatalf("menu C_ADT last byte = %d, want %d cells", got, tc.wantPages)
			}
			if got := int(binary.BigEndian.Uint32(admap[0:4])); got != 4*tc.wantPages+3 {
				t.Fatalf("menu VOBU_ADMAP last byte = %d, want one VOBU per page", got)
			}
			menuTable := vmg[int(binary.BigEndian.Uint32(vmg[0xc8:0xcc]))*DVDVOBSectorSize:]
			unit := menuTable[binary.BigEndian.Uint32(menuTable[12:16]):]

			var firstSCR uint64
			var firstStart, firstEnd uint32
			nextSector := uint32(0)
			for page := range tc.wantPages {
				entry := cadt[8+12*page:]
				start, end := binary.BigEndian.Uint32(entry[4:8]), binary.BigEndian.Uint32(entry[8:12])
				if vobID, cellID := binary.BigEndian.Uint16(entry[0:2]), entry[2]; int(vobID) != page+1 || cellID != 1 {
					t.Fatalf("page %d C_ADT entry = VOB %d cell %d, want VOB %d cell 1", page+1, vobID, cellID, page+1)
				}
				if start != nextSector || end < start || int(end) >= len(vob)/DVDVOBSectorSize {
					t.Fatalf("page %d cell sectors %d-%d do not follow sector %d inside the %d-sector VOB", page+1, start, end, nextSector, len(vob)/DVDVOBSectorSize)
				}
				nextSector = end + 1
				if got := binary.BigEndian.Uint32(admap[4+4*page:]); got != start {
					t.Fatalf("page %d VOBU_ADMAP entry = %d, want %d", page+1, got, start)
				}

				// The page's PGC plays its cell for exactly as long as the
				// VOBU presents pictures, then holds the last one.
				pgc := unit[binary.BigEndian.Uint32(unit[8+8*page+4:]):]
				cell := pgc[binary.BigEndian.Uint16(pgc[0xe8:0xea]):]
				position := pgc[binary.BigEndian.Uint16(pgc[0xea:0xec]):]
				if !bytes.Equal(pgc[4:8], tc.wantPageTime) || !bytes.Equal(cell[4:8], tc.wantPageTime) {
					t.Fatalf("page %d PGC/cell playback time = % x / % x, want % x (15 frames)", page+1, pgc[4:8], cell[4:8], tc.wantPageTime)
				}
				if cell[2] != 0xff {
					t.Fatalf("page %d still time = %d, want 255", page+1, cell[2])
				}
				if binary.BigEndian.Uint32(cell[8:12]) != start || binary.BigEndian.Uint32(cell[16:20]) != start || binary.BigEndian.Uint32(cell[20:24]) != end {
					t.Fatalf("page %d cell sectors = % x, want first VOBU and last VOBU %d, last sector %d", page+1, cell[8:24], start, end)
				}
				if vobID := binary.BigEndian.Uint16(position[0:2]); int(vobID) != page+1 || position[3] != 1 {
					t.Fatalf("page %d cell position = VOB %d cell %d, want VOB %d cell 1", page+1, vobID, position[3], page+1)
				}

				pageVOB := vob[int(start)*DVDVOBSectorSize : int(end+1)*DVDVOBSectorSize]
				packs := parseTestVOB(t, pageVOB)
				nav := packs[0]
				if !nav.nav {
					t.Fatalf("page %d does not start with a NAV pack", page+1)
				}
				pci, dsi := nav.raw[45:], nav.raw[0x407:]
				if binary.BigEndian.Uint32(dsi[4:8]) != start || binary.BigEndian.Uint32(dsi[8:12]) != end-start {
					t.Fatalf("page %d NAV pack: LBN %d, VOBU end %d; want %d and %d", page+1, binary.BigEndian.Uint32(dsi[4:8]), binary.BigEndian.Uint32(dsi[8:12]), start, end-start)
				}
				if vobID := binary.BigEndian.Uint16(dsi[24:26]); int(vobID) != page+1 || dsi[27] != 1 {
					t.Fatalf("page %d NAV pack is in VOB %d cell %d, want VOB %d cell 1", page+1, vobID, dsi[27], page+1)
				}
				startPTM, endPTM := binary.BigEndian.Uint32(pci[12:16]), binary.BigEndian.Uint32(pci[16:20])
				if endPTM-startPTM != 15*tc.frameTicks || binary.BigEndian.Uint32(pci[20:24]) != endPTM {
					t.Fatalf("page %d VOBU presents %d-%d (sequence end %d), want 15 frames of %d ticks ending with the sequence", page+1, startPTM, endPTM, binary.BigEndian.Uint32(pci[20:24]), tc.frameTicks)
				}
				// Each page has its own clock, like the menu of a one-page
				// disc, so every page starts at the same SCR and PTM.
				if page == 0 {
					firstSCR, firstStart, firstEnd = nav.scr, startPTM, endPTM
				} else if nav.scr != firstSCR || startPTM != firstStart || endPTM != firstEnd {
					t.Fatalf("page %d starts at SCR %d with VOBU %d-%d, want SCR %d and %d-%d as page 1", page+1, nav.scr, startPTM, endPTM, firstSCR, firstStart, firstEnd)
				}

				var video []byte
				audioBytes := 0
				var firstAudioPTS, lastAudioEnd uint64
				for _, pack := range packs[1:] {
					switch {
					case pack.streamID == 0xe0:
						video = append(video, pack.payload...)
					case pack.streamID == 0xbd && pack.payload[0] == 0xa0:
						if !pack.hasPTS {
							t.Fatalf("page %d LPCM pack at sector %d has no PTS", page+1, pack.sector)
						}
						data := len(pack.payload) - 7
						if audioBytes == 0 {
							firstAudioPTS = pack.pts
						}
						audioBytes += data
						lastAudioEnd = pack.pts + uint64(data/320)*150
					}
				}
				if pictures := bytes.Count(video, []byte{0, 0, 1, 0}); pictures != 15 {
					t.Fatalf("page %d VOBU has %d pictures, want 15", page+1, pictures)
				}
				if !bytes.HasSuffix(video, []byte{0, 0, 1, 0xb7}) {
					t.Fatalf("page %d video does not end with sequence_end_code", page+1)
				}
				// The audio of the page lies inside its VOB, starts with
				// the first picture and lasts at least until the VOBU ends;
				// only the last 80-sample LPCM frame may be padded.
				if audioBytes < tc.audioSamples*4 || audioBytes >= (tc.audioSamples+80)*4 {
					t.Fatalf("page %d carries %d bytes of LPCM, want %d samples (%d bytes) padded to whole frames", page+1, audioBytes, tc.audioSamples, tc.audioSamples*4)
				}
				if firstAudioPTS != uint64(startPTM) || lastAudioEnd < uint64(endPTM) {
					t.Fatalf("page %d audio spans %d-%d, want it to cover the VOBU %d-%d", page+1, firstAudioPTS, lastAudioEnd, startPTM, endPTM)
				}
				sync := binary.BigEndian.Uint16(dsi[0x192:0x194])
				if sync == DVDNAVAudioSyncNone || sync&0x8000 != 0 {
					t.Fatalf("page %d A_SYNCA = %#04x, want the offset of an audio pack after the NAV pack", page+1, sync)
				}
				if int(sync) >= len(packs) || packs[sync].streamID != 0xbd || packs[sync].payload[0] != 0xa0 || packs[sync].pts != uint64(startPTM) {
					t.Fatalf("page %d A_SYNCA %d does not point at the page's first LPCM pack", page+1, sync)
				}
			}
			if int(nextSector) != len(vob)/DVDVOBSectorSize {
				t.Fatalf("menu cells end at sector %d of a %d-sector VOB", nextSector, len(vob)/DVDVOBSectorSize)
			}
		})
	}
}

func TestDVDMenuPageLength(t *testing.T) {
	for _, tc := range []struct {
		standard VideoStandard
		duration time.Duration
		samples  int
	}{
		{VideoStandardNTSC, 500_500 * time.Microsecond, 24024},
		{VideoStandardPAL, 600 * time.Millisecond, 28800},
	} {
		if got := ExportDVDMenuPageDuration(tc.standard); got != tc.duration {
			t.Errorf("standard %d: page duration = %v, want %v", tc.standard, got, tc.duration)
		}
		if got := ExportDVDMenuPageAudioSamples(tc.standard); got != tc.samples {
			t.Errorf("standard %d: page audio = %d samples, want %d", tc.standard, got, tc.samples)
		}
	}
}

// makeMenuTestTitle returns a short title of the profile's standard.
func makeMenuTestTitle(t *testing.T, profile Profile) EncodedTitle {
	t.Helper()
	dir := t.TempDir()
	numerator, denominator := 30000, 1001
	if profile.Standard == VideoStandardPAL {
		numerator, denominator = 25, 1
	}
	frameDuration := time.Second * time.Duration(denominator) / time.Duration(numerator)
	var video bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&video, profile)
	if err != nil {
		t.Fatal(err)
	}
	frame := DecodedVideoFrame{
		Width: profile.FrameWidth, Height: profile.FrameHeight, Duration: frameDuration,
		Y: bytes.Repeat([]byte{80}, profile.FrameWidth*profile.FrameHeight),
		U: bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V: bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}
	const frames = 8
	for range frames {
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	videoPath := filepath.Join(dir, "video.m2v")
	if err := os.WriteFile(videoPath, video.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(dir, "audio.lpcm")
	samples := frames * 48_000 * denominator / numerator
	if err := os.WriteFile(audioPath, make([]byte, samples*4), 0o600); err != nil {
		t.Fatal(err)
	}
	return EncodedTitle{
		VideoPath: videoPath, AudioPath: audioPath,
		VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
		Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
		FrameRateNumerator: numerator, FrameRateDenominator: denominator,
		Duration: frames * frameDuration,
	}
}
