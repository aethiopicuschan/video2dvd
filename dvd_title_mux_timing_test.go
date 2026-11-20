package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/aethiopicuschan/video2dvd"
)

const (
	testMuxStartDelay   = 27_000 // first decode time in 90 kHz ticks.
	testMuxPackTicks    = 43_886 // one sector at 10.08 Mbit/s in 27 MHz ticks, rounded up.
	testMuxVideoBuffer  = 232 * 1024
	testMuxAudioBuffer  = 58 * 1024
	testMuxAudioMaxLead = 18_000 // 0.2 s in 90 kHz ticks.
)

type testVOBPack struct {
	sector   int
	scr      uint64 // 27 MHz.
	nav      bool
	streamID byte
	hasPTS   bool
	hasDTS   bool
	pts      uint64
	dts      uint64
	pstd     uint16
	payload  []byte
	raw      []byte
}

// parseTestVOB checks the framing of every sector and returns its packs. The
// final program end sector is validated and left out.
func parseTestVOB(t *testing.T, vob []byte) []testVOBPack {
	t.Helper()
	if len(vob) == 0 || len(vob)%DVDVOBSectorSize != 0 {
		t.Fatalf("VOB size = %d, want nonzero DVD sectors", len(vob))
	}
	var packs []testVOBPack
	count := len(vob) / DVDVOBSectorSize
	for index := range count {
		raw := vob[index*DVDVOBSectorSize : (index+1)*DVDVOBSectorSize]
		if !bytes.Equal(raw[:4], []byte{0, 0, 1, 0xba}) {
			t.Fatalf("sector %d has no MPEG pack header", index)
		}
		if raw[4]&0xc4 != 0x44 || raw[6]&0x04 == 0 || raw[8]&0x04 == 0 || raw[9]&0x01 == 0 || raw[12]&0x03 != 0x03 || raw[13] != 0xf8 {
			t.Fatalf("sector %d pack header has wrong marker or reserved bits: % x", index, raw[:14])
		}
		if rate := uint32(raw[10])<<14 | uint32(raw[11])<<6 | uint32(raw[12]>>2); rate != 25_200 {
			t.Fatalf("sector %d program_mux_rate = %d, want 25200", index, rate)
		}
		pack := testVOBPack{sector: index, scr: decodeTestMPEG2SCR(raw[4:14])*300 + decodeTestMPEG2SCRExtension(raw[4:14]), raw: raw}
		if !bytes.Equal(raw[14:17], []byte{0, 0, 1}) {
			t.Fatalf("sector %d has no packet after the pack header", index)
		}
		pack.streamID = raw[17]
		switch pack.streamID {
		case 0xbb:
			pack.nav = true
		case 0xbe:
			if index != count-1 || !bytes.Equal(raw[len(raw)-4:], []byte{0, 0, 1, 0xb9}) {
				t.Fatalf("sector %d is a padding-only pack that is not the program end", index)
			}
			if got := int(binary.BigEndian.Uint16(raw[18:20])); got != DVDVOBSectorSize-14-6-4 {
				t.Fatalf("program end padding length = %d", got)
			}
			return packs
		case 0xe0, 0xbd:
			length := int(binary.BigEndian.Uint16(raw[18:20]))
			flags, headerData := raw[21], int(raw[22])
			if raw[20] != 0x80 {
				t.Fatalf("sector %d PES marker byte = %#x", index, raw[20])
			}
			offset := 23
			if flags&0x80 != 0 {
				prefix := byte(0x2)
				if flags&0x40 != 0 {
					prefix = 0x3
				}
				if raw[offset]>>4 != prefix {
					t.Fatalf("sector %d PTS prefix = %#x, want %#x", index, raw[offset]>>4, prefix)
				}
				pack.hasPTS, pack.pts = true, decodeTestMPEGTimestamp(raw[offset:offset+5])
				offset += 5
			}
			if flags&0x40 != 0 {
				if flags&0x80 == 0 || raw[offset]>>4 != 0x1 {
					t.Fatalf("sector %d has a malformed DTS", index)
				}
				pack.hasDTS, pack.dts = true, decodeTestMPEGTimestamp(raw[offset:offset+5])
				offset += 5
			}
			if flags&0x01 != 0 {
				if raw[offset] != 0x1e {
					t.Fatalf("sector %d PES extension flags = %#x, want only P-STD_buffer", index, raw[offset])
				}
				pack.pstd = binary.BigEndian.Uint16(raw[offset+1 : offset+3])
				offset += 3
			}
			if flags&0x3e != 0 {
				t.Fatalf("sector %d has unexpected PES flags %#x", index, flags)
			}
			stuffing := raw[offset : 23+headerData]
			if len(stuffing) >= 8 || len(bytes.Trim(stuffing, "\xff")) != 0 {
				t.Fatalf("sector %d has invalid PES header stuffing % x", index, stuffing)
			}
			end := 20 + length
			pack.payload = raw[23+headerData : end]
			if rest := raw[end:]; len(rest) != 0 {
				if len(rest) < 8 || !bytes.Equal(rest[:4], []byte{0, 0, 1, 0xbe}) || int(binary.BigEndian.Uint16(rest[4:6])) != len(rest)-6 {
					t.Fatalf("sector %d is completed by %d bytes that are not a padding packet", index, len(rest))
				}
			}
		default:
			t.Fatalf("sector %d carries unexpected stream id %#x", index, pack.streamID)
		}
		packs = append(packs, pack)
	}
	t.Fatal("VOB does not end with a program end pack")
	return nil
}

func decodeTestMPEG2SCRExtension(value []byte) uint64 {
	return uint64(value[4]&0x03)<<7 | uint64(value[5]>>1)
}

// testMPEG2StreamPrefix returns the sequence header, sequence extension and
// GOP header that the built-in encoder writes in front of an I picture.
func testMPEG2StreamPrefix(t *testing.T, profile Profile) []byte {
	t.Helper()
	var stream bytes.Buffer
	encoder, err := NewMPEG2VideoEncoder(&stream, profile)
	if err != nil {
		t.Fatal(err)
	}
	frameDuration := time.Second * 1001 / 30000
	if profile.Standard == VideoStandardPAL {
		frameDuration = time.Second / 25
	}
	if err := encoder.WriteFrame(DecodedVideoFrame{
		Width: profile.FrameWidth, Height: profile.FrameHeight, Duration: frameDuration,
		Y: bytes.Repeat([]byte{80}, profile.FrameWidth*profile.FrameHeight),
		U: bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
		V: bytes.Repeat([]byte{128}, profile.FrameWidth*profile.FrameHeight/4),
	}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	picture := bytes.Index(stream.Bytes(), []byte{0, 0, 1, 0x00})
	if picture <= 0 {
		t.Fatal("encoded stream has no sequence prefix")
	}
	return bytes.Clone(stream.Bytes()[:picture])
}

// makeTestMPEG2Stream builds an elementary stream of pictures with the given
// sizes. The muxer only reads the headers, so the picture bodies are filler.
// It returns the stream and the offset at which each picture starts.
func makeTestMPEG2Stream(prefix []byte, sizes []int, gop int) ([]byte, []int) {
	var stream []byte
	starts := make([]int, len(sizes))
	for index, size := range sizes {
		starts[index] = len(stream)
		begin := len(stream)
		pictureType := byte(2)
		if index%gop == 0 {
			stream = append(stream, prefix...)
			pictureType = 1
		}
		reference := index % gop
		stream = append(stream, 0, 0, 1, 0x00, byte(reference>>2), byte(reference<<6)|pictureType<<3|0x07, 0xff, 0xf8, 0, 0, 1, 0x01)
		for len(stream)-begin < size {
			stream = append(stream, 0xa5)
		}
	}
	return append(stream, 0, 0, 1, 0xb7), starts
}

type testMuxInput struct {
	title  EncodedTitle
	dir    string
	video  []byte
	starts []int
	audio  []byte
}

func newTestMuxInput(t *testing.T, standard VideoStandard, sizes []int, gop, audioSamples int) testMuxInput {
	t.Helper()
	profile, err := ExportProfile(Options{Standard: standard, Aspect: AspectRatio16x9})
	if err != nil {
		t.Fatal(err)
	}
	input := testMuxInput{dir: t.TempDir()}
	input.video, input.starts = makeTestMPEG2Stream(testMPEG2StreamPrefix(t, profile), sizes, gop)
	input.audio = make([]byte, audioSamples*4)
	for i := range input.audio {
		input.audio[i] = byte(i*7 + i/251)
	}
	if err := os.WriteFile(filepath.Join(input.dir, "video.m2v"), input.video, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.dir, "audio.lpcm"), input.audio, 0o600); err != nil {
		t.Fatal(err)
	}
	input.title = EncodedTitle{
		VideoPath: "video.m2v", AudioPath: "audio.lpcm",
		VideoCodec: VideoCodecMPEG2, AudioCodec: AudioCodecLPCM,
		Aspect: profile.Aspect, Width: profile.FrameWidth, Height: profile.FrameHeight,
		FrameRateNumerator: 30000, FrameRateDenominator: 1001,
		Duration: time.Duration(len(sizes)) * time.Second * 1001 / 30000,
	}
	if standard == VideoStandardPAL {
		input.title.FrameRateNumerator, input.title.FrameRateDenominator = 25, 1
		input.title.Duration = time.Duration(len(sizes)) * time.Second / 25
	}
	return input
}

func testGOPSizes(frames, gop, intra, predicted int) []int {
	sizes := make([]int, frames)
	for i := range sizes {
		sizes[i] = predicted
		if i%gop == 0 {
			sizes[i] = intra
		}
	}
	return sizes
}

// TestMuxDVDTitleVOBTimingModel checks the whole timing model on a stream
// with large I pictures: a byte-driven clock from zero, a start-up delay on
// one shared timeline, exact timestamps, decoder buffers that neither
// underflow nor overflow, and NAV packs that describe what was written.
func TestMuxDVDTitleVOBTimingModel(t *testing.T) {
	const frames, gop, frameTicks = 60, 15, 3003
	// About 7.8 Mbit/s of video with I pictures six times the P size, and
	// audio that ends inside an LPCM frame.
	input := newTestMuxInput(t, VideoStandardNTSC, testGOPSizes(frames, gop, 150_000, 24_000), gop, 80_000+37)
	var vob bytes.Buffer
	var records []TestDVDVOBURecord
	if err := ExportMuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob, func(record TestDVDVOBURecord) error {
		records = append(records, record)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	packs := parseTestVOB(t, vob.Bytes())

	firstPTS := uint64(testMuxStartDelay + frameTicks)
	pictureOf := func(offset int) int {
		picture := 0
		for picture+1 < len(input.starts) && input.starts[picture+1] <= offset {
			picture++
		}
		return picture
	}
	type buffered struct {
		release uint64
		bytes   int
	}
	var video, audio []byte
	var videoBuffer, audioBuffer []buffered
	var navs []testVOBPack
	var audioPacks []testVOBPack
	stamped := 0
	vobuHasVideo, vobuHasAudio := false, false
	for index, pack := range packs {
		if index == 0 && (!pack.nav || pack.scr != 0) {
			t.Fatalf("first pack: nav=%v SCR=%d, want a NAV pack at SCR 0", pack.nav, pack.scr)
		}
		if index > 0 && pack.scr < packs[index-1].scr+testMuxPackTicks-1 {
			t.Fatalf("sector %d SCR %d follows %d by less than one pack time", index, pack.scr, packs[index-1].scr)
		}
		packEnd := pack.scr + testMuxPackTicks
		for len(videoBuffer) > 0 && videoBuffer[0].release <= pack.scr {
			videoBuffer = videoBuffer[1:]
		}
		for len(audioBuffer) > 0 && audioBuffer[0].release <= pack.scr {
			audioBuffer = audioBuffer[1:]
		}
		switch {
		case pack.nav:
			navs = append(navs, pack)
			vobuHasVideo, vobuHasAudio = false, false
		case pack.streamID == 0xe0:
			wantPSTD := uint16(0)
			if !vobuHasVideo {
				wantPSTD = 0x60e8
			}
			if pack.pstd != wantPSTD {
				t.Fatalf("sector %d video P-STD buffer = %#x, want %#x", index, pack.pstd, wantPSTD)
			}
			vobuHasVideo = true
			first := pictureOf(len(video))
			last := pictureOf(len(video) + len(pack.payload) - 1)
			starting := -1
			if input.starts[first] == len(video) {
				starting = first
			} else if last != first {
				starting = last
			}
			if last-first > 1 || starting >= 0 && last != starting {
				t.Fatalf("sector %d carries the start of more than one picture", index)
			}
			if pack.hasPTS != (starting >= 0) || pack.hasDTS != pack.hasPTS {
				t.Fatalf("sector %d timestamps present=%v/%v, picture starting=%d", index, pack.hasPTS, pack.hasDTS, starting)
			}
			if starting >= 0 {
				wantDTS := uint64(testMuxStartDelay + frameTicks*starting)
				if pack.dts != wantDTS || pack.pts != wantDTS+frameTicks {
					t.Fatalf("picture %d PTS/DTS = %d/%d, want %d/%d", starting, pack.pts, pack.dts, wantDTS+frameTicks, wantDTS)
				}
				if starting != stamped {
					t.Fatalf("picture %d is stamped out of order, expected %d", starting, stamped)
				}
				stamped++
			}
			// Every byte must arrive before the picture it belongs to is decoded.
			if deadline := uint64(testMuxStartDelay+frameTicks*first) * 300; packEnd > deadline {
				t.Fatalf("sector %d delivers picture %d at %d, after its decode time %d", index, first, packEnd, deadline)
			}
			for offset := len(video); offset < len(video)+len(pack.payload); {
				picture := pictureOf(offset)
				end := len(input.video)
				if picture+1 < len(input.starts) {
					end = input.starts[picture+1]
				}
				end = min(end, len(video)+len(pack.payload))
				videoBuffer = append(videoBuffer, buffered{release: uint64(testMuxStartDelay+frameTicks*picture) * 300, bytes: end - offset})
				offset = end
			}
			// The PES header waits in the buffer with the data it precedes.
			videoBuffer[len(videoBuffer)-1].bytes += 9 + int(pack.raw[22])
			occupancy := 0
			for _, entry := range videoBuffer {
				occupancy += entry.bytes
			}
			if occupancy > testMuxVideoBuffer {
				t.Fatalf("sector %d fills the video decoder buffer to %d bytes, above %d", index, occupancy, testMuxVideoBuffer)
			}
			video = append(video, pack.payload...)
		default:
			wantPSTD := uint16(0)
			if !vobuHasAudio {
				wantPSTD = 0x603a
			}
			if pack.pstd != wantPSTD {
				t.Fatalf("sector %d audio P-STD buffer = %#x, want %#x", index, pack.pstd, wantPSTD)
			}
			vobuHasAudio = true
			header, samples := pack.payload[:7], pack.payload[7:]
			frameNumber := len(audio) / 320
			if len(samples) == 0 || len(samples)%320 != 0 || header[0] != 0xa0 || int(header[1]) != len(samples)/320 ||
				binary.BigEndian.Uint16(header[2:4]) != 4 || int(header[4]) != frameNumber%20 || header[5] != 0x01 || header[6] != 0x80 {
				t.Fatalf("sector %d LPCM header % x does not describe %d bytes from frame %d", index, header, len(samples), frameNumber)
			}
			wantPTS := firstPTS + uint64(frameNumber)*150
			if !pack.hasPTS || pack.hasDTS || pack.pts != wantPTS {
				t.Fatalf("sector %d audio PTS = %d, want %d", index, pack.pts, wantPTS)
			}
			if packEnd > wantPTS*300 {
				t.Fatalf("sector %d delivers audio at %d, after its presentation time %d", index, packEnd, wantPTS*300)
			}
			if lead := wantPTS*300 - pack.scr; lead > (testMuxAudioMaxLead+150)*300 {
				t.Fatalf("sector %d delivers audio %d ms early", index, lead/27_000)
			}
			audioBuffer = append(audioBuffer, buffered{release: (wantPTS + uint64(len(samples)/320)*150) * 300, bytes: len(pack.payload)})
			occupancy := 0
			for _, entry := range audioBuffer {
				occupancy += entry.bytes
			}
			if occupancy > testMuxAudioBuffer {
				t.Fatalf("sector %d fills the audio decoder buffer to %d bytes, above %d", index, occupancy, testMuxAudioBuffer)
			}
			audio = append(audio, samples...)
			audioPacks = append(audioPacks, pack)
		}
	}
	if !bytes.Equal(video, input.video) {
		t.Fatalf("demultiplexed video differs from the elementary stream (%d of %d bytes)", len(video), len(input.video))
	}
	if stamped != frames {
		t.Fatalf("%d of %d pictures carry timestamps", stamped, frames)
	}
	paddedAudio := append(bytes.Clone(input.audio), make([]byte, 320-len(input.audio)%320)...)
	if !bytes.Equal(audio, paddedAudio) {
		t.Fatalf("demultiplexed audio differs from the LPCM input (%d of %d bytes)", len(audio), len(paddedAudio))
	}

	wantVOBUs := (frames + gop - 1) / gop
	if len(navs) != wantVOBUs || len(records) != wantVOBUs {
		t.Fatalf("found %d NAV packs and %d VOBU records, want %d", len(navs), len(records), wantVOBUs)
	}
	sectorCount := uint32(vob.Len() / DVDVOBSectorSize)
	vobEnd := uint32(firstPTS + frames*frameTicks)
	for index, nav := range navs {
		pci, dsi := nav.raw[45:], nav.raw[0x407:]
		firstFrame := index * gop
		lastFrame := min(firstFrame+gop, frames)
		startPTM := uint32(firstPTS) + uint32(firstFrame*frameTicks)
		endPTM := uint32(firstPTS) + uint32(lastFrame*frameTicks)
		nextSector := sectorCount
		if index+1 < len(navs) {
			nextSector = uint32(navs[index+1].sector)
		}
		if got := binary.BigEndian.Uint32(pci[0:4]); got != uint32(nav.sector) || binary.BigEndian.Uint32(dsi[4:8]) != got {
			t.Fatalf("NAV %d logical block = %d, want %d", index, got, nav.sector)
		}
		if got := binary.BigEndian.Uint32(dsi[0:4]); got != uint32(nav.scr/300) {
			t.Fatalf("NAV %d DSI SCR = %d, want %d", index, got, nav.scr/300)
		}
		if binary.BigEndian.Uint32(pci[12:16]) != startPTM || binary.BigEndian.Uint32(pci[16:20]) != endPTM {
			t.Fatalf("NAV %d presentation span = %d..%d, want %d..%d", index, binary.BigEndian.Uint32(pci[12:16]), binary.BigEndian.Uint32(pci[16:20]), startPTM, endPTM)
		}
		if uint64(startPTM)*300 <= nav.scr {
			t.Fatalf("NAV %d arrives at %d, not before its VOBU is presented at %d", index, nav.scr, uint64(startPTM)*300)
		}
		wantSequenceEnd := uint32(0)
		if index == len(navs)-1 {
			wantSequenceEnd = endPTM
		}
		if got := binary.BigEndian.Uint32(pci[20:24]); got != wantSequenceEnd {
			t.Fatalf("NAV %d vobu_se_e_ptm = %d, want %d", index, got, wantSequenceEnd)
		}
		wantElapsed, err := ExportDVDCellElapsedTime(time.Duration(firstFrame)*time.Second*1001/30000, 30000, 1001)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pci[24:28], wantElapsed[:]) || !bytes.Equal(dsi[28:32], wantElapsed[:]) {
			t.Fatalf("NAV %d cell elapsed time = % x / % x, want % x", index, pci[24:28], dsi[28:32], wantElapsed)
		}
		if got := binary.BigEndian.Uint32(dsi[8:12]); got != nextSector-1-uint32(nav.sector) {
			t.Fatalf("NAV %d VOBU end address = %d, want %d", index, got, nextSector-1-uint32(nav.sector))
		}
		if binary.BigEndian.Uint32(dsi[44:48]) != uint32(firstPTS) || binary.BigEndian.Uint32(dsi[48:52]) != vobEnd {
			t.Fatalf("NAV %d VOB presentation span = %d..%d, want %d..%d", index, binary.BigEndian.Uint32(dsi[44:48]), binary.BigEndian.Uint32(dsi[48:52]), firstPTS, vobEnd)
		}
		record := records[index]
		if record.StartSector != uint32(nav.sector) || record.EndSector != nextSector-1 || record.StartPTM != startPTM || record.EndPTM != endPTM {
			t.Fatalf("VOBU record %d = %+v, want sectors %d..%d and time %d..%d", index, record, nav.sector, nextSector-1, startPTM, endPTM)
		}

		// A_SYNCA addresses the audio pack that is presented at the VOBU start.
		wantSync := DVDNAVAudioSyncNone
		for _, pack := range audioPacks {
			if pack.pts <= uint64(startPTM) && uint64(startPTM) < pack.pts+uint64((len(pack.payload)-7)/320)*150 {
				if pack.sector >= nav.sector {
					wantSync = uint16(pack.sector - nav.sector)
				} else {
					wantSync = uint16(nav.sector-pack.sector) | 0x8000
				}
			}
		}
		if got := binary.BigEndian.Uint16(dsi[402:404]); got != wantSync {
			t.Fatalf("NAV %d A_SYNCA = %#x, want %#x", index, got, wantSync)
		}
		if !bytes.Equal(dsi[404:418], make([]byte, 14)) {
			t.Fatalf("NAV %d has sync pointers for absent audio streams", index)
		}

		// Search pointers: 15-frame VOBUs last 0.5005 s, so the VOBU that
		// is presented n half seconds later is n-1 VOBUs ahead.
		pointer := func(target int) uint32 {
			if target < 0 || target >= len(navs) {
				return DVDNAVSRIEndOfCell
			}
			if target > index {
				return 0x80000000 | uint32(navs[target].sector-nav.sector)
			}
			return 0x80000000 | uint32(nav.sector-navs[target].sector)
		}
		sri := dsi[234:]
		for step := 1; step <= 15; step++ {
			if got, want := binary.BigEndian.Uint32(sri[4+(19-step)*4:]), pointer(index+step-1); got != want {
				t.Fatalf("NAV %d forward pointer for %d half seconds = %#x, want %#x", index, step, got, want)
			}
			if got, want := binary.BigEndian.Uint32(sri[88+(step-1)*4:]), pointer(index-step); got != want {
				t.Fatalf("NAV %d backward pointer for %d half seconds = %#x, want %#x", index, step, got, want)
			}
		}
		if got, want := binary.BigEndian.Uint32(sri[80:]), pointer(index+1); got != want {
			t.Fatalf("NAV %d next VOBU = %#x, want %#x", index, got, want)
		}
		if got, want := binary.BigEndian.Uint32(sri[84:]), pointer(index-1); got != want {
			t.Fatalf("NAV %d previous VOBU = %#x, want %#x", index, got, want)
		}
	}
}

func TestMuxDVDTitleVOBUsesExactPALFramePeriod(t *testing.T) {
	const frames, gop = 26, 12
	input := newTestMuxInput(t, VideoStandardPAL, testGOPSizes(frames, gop, 60_000, 20_000), gop, 48_000)
	var vob bytes.Buffer
	if err := MuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob); err != nil {
		t.Fatal(err)
	}
	picture := 0
	for _, pack := range parseTestVOB(t, vob.Bytes()) {
		if pack.streamID != 0xe0 || !pack.hasPTS {
			continue
		}
		wantDTS := uint64(testMuxStartDelay + 3600*picture)
		if pack.dts != wantDTS || pack.pts != wantDTS+3600 {
			t.Fatalf("PAL picture %d PTS/DTS = %d/%d, want %d/%d", picture, pack.pts, pack.dts, wantDTS+3600, wantDTS)
		}
		picture++
	}
	if picture != frames {
		t.Fatalf("found timestamps for %d of %d pictures", picture, frames)
	}
}

func TestDVDVideoFrameTicksAreExact(t *testing.T) {
	for frame := range int64(200_000) {
		if got := ExportDVDVideoFieldTicks(2*frame, 30000, 1001); got != uint64(frame)*3003 {
			t.Fatalf("NTSC frame %d starts at %d ticks, want %d", frame, got, frame*3003)
		}
		if got := ExportDVDVideoFieldTicks(2*frame, 25, 1); got != uint64(frame)*3600 {
			t.Fatalf("PAL frame %d starts at %d ticks, want %d", frame, got, frame*3600)
		}
		// An NTSC field is 1501.5 ticks: the second field of a frame starts
		// on a half tick, which is rounded down.
		if got := ExportDVDVideoFieldTicks(2*frame+1, 30000, 1001); got != uint64(frame)*3003+1501 {
			t.Fatalf("NTSC field %d starts at %d ticks, want %d", 2*frame+1, got, frame*3003+1501)
		}
	}
}

// A stream that cannot be delivered at the DVD mux rate must be refused: the
// system clock may never run past a decode time.
func TestMuxDVDTitleVOBRejectsVideoAboveTheMuxRate(t *testing.T) {
	// 40 kB per frame is 9.6 Mbit/s; about 8.3 Mbit/s fits beside LPCM.
	input := newTestMuxInput(t, VideoStandardNTSC, testGOPSizes(120, 15, 40_000, 40_000), 15, 240_000)
	var vob bytes.Buffer
	err := MuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob)
	if err == nil || !strings.Contains(err.Error(), "multiplex rate") {
		t.Fatalf("muxing 9.6 Mbit/s of video returned %v, want a multiplex rate error", err)
	}
	if vob.Len() != 0 {
		t.Fatalf("a rejected title wrote %d bytes", vob.Len())
	}

	// A single picture that can never fit the decoder buffer is refused too.
	sizes := testGOPSizes(15, 15, 300_000, 2_000)
	input = newTestMuxInput(t, VideoStandardNTSC, sizes, 15, 4_800)
	if err := MuxDVDTitleVOB(context.Background(), input.title, input.dir, &bytes.Buffer{}); err == nil {
		t.Fatal("expected a picture larger than the DVD video buffer to fail")
	}
}

// 8 Mbit/s of evenly sized pictures is what the built-in encoder produces at
// its default rate; it must fit beside LPCM without eating into the start-up
// delay.
func TestMuxDVDTitleVOBFitsEightMegabitVideo(t *testing.T) {
	input := newTestMuxInput(t, VideoStandardNTSC, testGOPSizes(150, 15, 33_366, 33_366), 15, 240_000)
	var vob bytes.Buffer
	if err := MuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob); err != nil {
		t.Fatal(err)
	}
	minLead := uint64(1 << 62)
	for _, pack := range parseTestVOB(t, vob.Bytes()) {
		if pack.streamID == 0xe0 && pack.hasDTS {
			minLead = min(minLead, pack.dts*300-pack.scr)
		}
	}
	if minLead < 100*27_000 {
		t.Fatalf("pictures start arriving only %d ms before decoding; the mux has no headroom at 8 Mbit/s", minLead/27_000)
	}
}

func TestMuxDVDTitleVOBMenuPagesAreSeparateCells(t *testing.T) {
	// Two I pictures: one VOBU, and so one cell, per menu page.
	input := newTestMuxInput(t, VideoStandardNTSC, []int{9_000, 9_000}, 1, 1)
	pages := [][]DVDNAVButton{
		{{XStart: 10, YStart: 10, XEnd: 100, YEnd: 30, Up: 1, Down: 1, Left: 1, Right: 1}},
		{{XStart: 20, YStart: 40, XEnd: 200, YEnd: 60, Up: 1, Down: 1, Left: 1, Right: 1}},
	}
	var vob bytes.Buffer
	var records []TestDVDVOBURecord
	if err := ExportMuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob, func(record TestDVDVOBURecord) error {
		records = append(records, record)
		return nil
	}, pages); err != nil {
		t.Fatal(err)
	}
	cell := byte(0)
	for _, pack := range parseTestVOB(t, vob.Bytes()) {
		if !pack.nav {
			continue
		}
		cell++
		pci, dsi := pack.raw[45:], pack.raw[0x407:]
		if dsi[27] != cell {
			t.Fatalf("menu VOBU %d is in cell %d", cell, dsi[27])
		}
		if !bytes.Equal(pci[24:28], []byte{0, 0, 0, 0xc0}) {
			t.Fatalf("menu cell %d elapsed time = % x, want zero at the cell start", cell, pci[24:28])
		}
		// The only picture of the VOBU ends all three reference addresses.
		if end := binary.BigEndian.Uint32(dsi[12:16]); end == 0 || end > binary.BigEndian.Uint32(dsi[8:12]) ||
			binary.BigEndian.Uint32(dsi[16:20]) != end || binary.BigEndian.Uint32(dsi[20:24]) != end {
			t.Fatalf("menu cell %d reference end addresses = % x, want the picture's last sector three times", cell, dsi[12:24])
		}
		if pci[0x60+0x11] != 1 || pci[0x60+0x2e+2] != byte(pages[cell-1][0].XEnd) {
			t.Fatalf("menu cell %d does not carry its own button", cell)
		}
		for offset := 234; offset < 402; offset += 4 {
			if got := binary.BigEndian.Uint32(dsi[offset:]) & 0x3fffffff; got != DVDNAVSRIEndOfCell {
				t.Fatalf("menu cell %d search pointer at %d = %#x leaves the cell", cell, offset, got)
			}
		}
	}
	if cell != 2 || len(records) != 2 || records[0].StartSector != 0 || records[1].StartSector != records[0].EndSector+1 {
		t.Fatalf("menu produced %d cells and records %+v, want two adjacent VOBUs", cell, records)
	}
}
