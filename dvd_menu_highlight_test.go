package video2dvd_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

// decodeTestNAVButton returns the rectangle of entry index of a NAV pack's
// button table.
func decodeTestNAVButton(hli []byte, index int) (xStart, yStart, xEnd, yEnd int) {
	entry := hli[0x2e+index*18:]
	xStart = int(entry[0]&0x3f)<<4 | int(entry[1]>>4)
	xEnd = int(entry[1]&0x03)<<8 | int(entry[2])
	yStart = int(entry[3]&0x3f)<<4 | int(entry[4]>>4)
	yEnd = int(entry[4]&0x03)<<8 | int(entry[5])
	return
}

// The selected menu button is only visible through the highlight subpicture,
// so every menu VOBU must carry it: one unit per display mode, presented with
// the VOBU's picture, and described consistently by the NAV pack.
func TestMuxDVDMenuVOBUsCarryHighlightSubpictures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		standard  VideoStandard
		aspect    AspectRatio
		titles    int
		wantPages int
		// One group and stream for 4:3; widescreen and letterbox for 16:9.
		wantGroupInfo uint16
		wantStreams   int
	}{
		{name: "NTSC 4:3", standard: VideoStandardNTSC, aspect: AspectRatio4x3, titles: 2, wantPages: 1, wantGroupInfo: 0x1000, wantStreams: 1},
		{name: "PAL 4:3 full two-column page", standard: VideoStandardPAL, aspect: AspectRatio4x3, titles: 35, wantPages: 1, wantGroupInfo: 0x1000, wantStreams: 1},
		{name: "NTSC 16:9", standard: VideoStandardNTSC, aspect: AspectRatio16x9, titles: 2, wantPages: 1, wantGroupInfo: 0x2120, wantStreams: 2},
		{name: "PAL 16:9 three pages", standard: VideoStandardPAL, aspect: AspectRatio16x9, titles: 40, wantPages: 3, wantGroupInfo: 0x2120, wantStreams: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ExportProfile(Options{Standard: tc.standard, Aspect: tc.aspect})
			if err != nil {
				t.Fatal(err)
			}
			titles := make([]EncodedTitle, tc.titles)
			for i := range titles {
				titles[i].Name = fmt.Sprintf("clip-%d", i+1)
			}
			dir := t.TempDir()
			menu, pages, err := ExportCreateDVDMenuPages(context.Background(), titles, profile, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != tc.wantPages || len(menu) != tc.wantPages {
				t.Fatalf("menu has %d pages and %d button pages, want %d", len(menu), len(pages), tc.wantPages)
			}
			var vob bytes.Buffer
			var records []TestDVDVOBURecord
			if err := ExportMuxDVDMenuVOB(context.Background(), menu, pages, dir, &vob, func(record TestDVDVOBURecord) error {
				records = append(records, record)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			sectors := vob.Len() / DVDVOBSectorSize
			if len(records) != len(pages) || records[0].StartSector != 0 || int(records[len(records)-1].EndSector) != sectors-1 {
				t.Fatalf("VOBU records %+v do not cover the %d pages in %d sectors", records, len(pages), sectors)
			}
			// Every page is a VOB of its own with its own clock; packs
			// are numbered by their sector in the whole menu VOB.
			packs := make([]testVOBPack, sectors)
			subpicturePacks := 0
			for _, record := range records {
				pagePacks := parseTestVOB(t, vob.Bytes()[int(record.StartSector)*DVDVOBSectorSize:int(record.EndSector+1)*DVDVOBSectorSize])
				for i, pack := range pagePacks {
					if pack.streamID == 0xbd && pack.payload[0]&0xe0 == 0x20 {
						subpicturePacks++
					}
					// The system clock is the byte clock: it never runs
					// backwards and two packs never share a value.
					if i > 0 && pack.scr <= pagePacks[i-1].scr {
						t.Fatalf("sector %d SCR %d does not follow %d", pack.sector, pack.scr, pagePacks[i-1].scr)
					}
					pack.sector += int(record.StartSector)
					packs[pack.sector] = pack
				}
			}
			if subpicturePacks != len(pages)*tc.wantStreams {
				t.Fatalf("menu VOB has %d subpicture packs, want %d per page", subpicturePacks, tc.wantStreams)
			}

			for page, record := range records {
				if page > 0 && record.StartSector != records[page-1].EndSector+1 {
					t.Fatalf("VOBU %d starts at sector %d, want the sector after %d", page+1, record.StartSector, records[page-1].EndSector)
				}
				nav := packs[record.StartSector]
				if !nav.nav {
					t.Fatalf("VOBU %d does not start with a NAV pack", page+1)
				}
				pci, dsi := nav.raw[45:], nav.raw[0x407:]
				hli := pci[0x60:]
				startPTM := binary.BigEndian.Uint32(pci[12:16])
				if startPTM != record.StartPTM || binary.BigEndian.Uint32(dsi[4:8]) != record.StartSector {
					t.Fatalf("VOBU %d NAV pack: start time %d at sector %d, record %+v", page+1, startPTM, binary.BigEndian.Uint32(dsi[4:8]), record)
				}
				// The subpicture packs are part of the VOBU that the NAV
				// pack measures.
				if got := binary.BigEndian.Uint32(dsi[8:12]); got != record.EndSector-record.StartSector {
					t.Fatalf("VOBU %d end address = %d, want %d", page+1, got, record.EndSector-record.StartSector)
				}
				if end := binary.BigEndian.Uint32(dsi[12:16]); end <= uint32(tc.wantStreams) || end > record.EndSector-record.StartSector {
					t.Fatalf("VOBU %d first reference picture ends at relative sector %d, outside the video packs", page+1, end)
				}
				if binary.BigEndian.Uint16(hli[0:2]) != 1 || binary.BigEndian.Uint32(hli[2:6]) != startPTM ||
					binary.BigEndian.Uint32(hli[6:10]) != 0xffffffff || binary.BigEndian.Uint32(hli[10:14]) != 0xffffffff {
					t.Fatalf("VOBU %d highlight status and times = % x, want new information from %d without end", page+1, hli[:14], startPTM)
				}
				if got := binary.BigEndian.Uint16(hli[0x0e:0x10]); got != tc.wantGroupInfo {
					t.Fatalf("VOBU %d button group information = %#04x, want %#04x", page+1, got, tc.wantGroupInfo)
				}
				buttons := pages[page]
				if hli[0x10] != 0 || int(hli[0x11]) != len(buttons) || int(hli[0x12]) != len(buttons) {
					t.Fatalf("VOBU %d button offset/counts = % x, want 0 and %d twice", page+1, hli[0x10:0x13], len(buttons))
				}
				for stream := range 32 {
					want := uint32(0)
					if stream < tc.wantStreams {
						want = DVDNAVSubpictureSyncCurrent
					}
					if got := binary.BigEndian.Uint32(dsi[dvdNavTestSubpictureSyncOffset+stream*4:]); got != want {
						t.Fatalf("VOBU %d SP_SYNCA[%d] = %#x, want %#x", page+1, stream, got, want)
					}
				}

				letterboxed := ExportLetterboxDVDMenuButtons(buttons, profile.FrameHeight)
				for stream := range tc.wantStreams {
					pack := packs[int(record.StartSector)+1+stream]
					if pack.streamID != 0xbd || pack.payload[0] != byte(0x20+stream) {
						t.Fatalf("VOBU %d sector %d is not the pack of subpicture substream %#x", page+1, pack.sector, 0x20+stream)
					}
					// Presented with the VOBU's picture, declared with the
					// 52 KiB subpicture buffer, and completely delivered
					// before its presentation time.
					if !pack.hasPTS || pack.hasDTS || pack.pts != uint64(startPTM) || pack.pstd != 0x6000|52 {
						t.Fatalf("VOBU %d subpicture pack: PTS %d (present %v), DTS present %v, P-STD %#x; want PTS %d and 0x6034",
							page+1, pack.pts, pack.hasPTS, pack.hasDTS, pack.pstd, startPTM)
					}
					delivered := pack.scr + DVDVOBSectorSize*27_000_000/1_260_000
					if delivered > pack.pts*300 || pack.pts*300-pack.scr > 27_000_000 {
						t.Fatalf("VOBU %d subpicture pack SCR %d is not within a second before its PTS %d", page+1, pack.scr, pack.pts*300)
					}
					unit := pack.payload[1:]
					if int(binary.BigEndian.Uint16(unit[0:2])) != len(unit) {
						t.Fatalf("VOBU %d pack carries %d bytes of a %d-byte subpicture unit", page+1, len(unit), binary.BigEndian.Uint16(unit[0:2]))
					}
					// The group's button table and its subpicture agree:
					// every pixel of every button is a pattern pixel.
					want := buttons
					if stream == 1 {
						want = letterboxed
					}
					pixels, _ := decodeTestSubpicture(t, unit, profile.FrameWidth, profile.FrameHeight)
					for index, button := range want {
						xStart, yStart, xEnd, yEnd := decodeTestNAVButton(hli, stream*(36/tc.wantStreams)+index)
						if xStart != button.XStart || yStart != button.YStart || xEnd != button.XEnd || yEnd != button.YEnd {
							t.Fatalf("VOBU %d group %d button %d = (%d,%d)-(%d,%d), want (%d,%d)-(%d,%d)", page+1, stream+1, index+1,
								xStart, yStart, xEnd, yEnd, button.XStart, button.YStart, button.XEnd, button.YEnd)
						}
						for y := yStart; y <= yEnd; y++ {
							for x := xStart; x <= xEnd; x++ {
								if pixels[y][x] != 1 {
									t.Fatalf("VOBU %d stream %d: pixel (%d,%d) of button %d has code %d, want 1 (pattern)", page+1, stream, x, y, index+1, pixels[y][x])
								}
							}
						}
					}
				}
				// The picture follows the subpictures.
				if video := packs[int(record.StartSector)+1+tc.wantStreams]; video.streamID != 0xe0 || !video.hasPTS || video.pts != uint64(startPTM) {
					t.Fatalf("VOBU %d: sector %d is not the start of the page's picture", page+1, video.sector)
				}
			}
		})
	}
}

func TestMuxDVDTitleVOBHasNoSubpicture(t *testing.T) {
	input := newTestMuxInput(t, VideoStandardNTSC, testGOPSizes(30, 15, 40_000, 9_000), 15, 48_000)
	var vob bytes.Buffer
	if err := MuxDVDTitleVOB(context.Background(), input.title, input.dir, &vob); err != nil {
		t.Fatal(err)
	}
	for _, pack := range parseTestVOB(t, vob.Bytes()) {
		if pack.streamID == 0xbd && pack.payload[0] != 0xa0 {
			t.Fatalf("title VOB sector %d carries private substream %#x", pack.sector, pack.payload[0])
		}
		if pack.nav {
			if !bytes.Equal(pack.raw[0x407+dvdNavTestSubpictureSyncOffset:][:32*4], make([]byte, 32*4)) || pack.raw[45+0x60+1] != 0 {
				t.Fatalf("title VOB NAV pack at sector %d declares a subpicture or a highlight", pack.sector)
			}
		}
	}
}

// A 4:3 display shows a 16:9 menu letterboxed in the middle three quarters
// of the frame, and a player does not scale the highlight with the picture.
func TestLetterboxDVDMenuButtonsFollowTheSqueezedPicture(t *testing.T) {
	for _, height := range []int{480, 576} {
		buttons := ExportMakeDVDMenuButtons(0, 17, Profile{Aspect: AspectRatio16x9}, 0, 1, 2)
		letterboxed := ExportLetterboxDVDMenuButtons(buttons, height)
		if len(letterboxed) != len(buttons) {
			t.Fatalf("letterboxed %d of %d buttons", len(letterboxed), len(buttons))
		}
		for i, button := range letterboxed {
			source := buttons[i]
			if button.XStart != source.XStart || button.XEnd != source.XEnd || button.Command != source.Command ||
				button.Up != source.Up || button.Down != source.Down || button.Left != source.Left || button.Right != source.Right {
				t.Errorf("height %d button %d: letterboxing changed more than the lines", height, i+1)
			}
			// A line y of the picture is shown at height/8 + 3y/4.
			top, bottom := float64(height)/8+float64(source.YStart)*0.75, float64(height)/8+float64(source.YEnd)*0.75
			if d := float64(button.YStart) - top; d < -2 || d > 2 {
				t.Errorf("height %d button %d starts on line %d, want about %.1f", height, i+1, button.YStart, top)
			}
			if d := float64(button.YEnd) - bottom; d < -2 || d > 2 {
				t.Errorf("height %d button %d ends on line %d, want about %.1f", height, i+1, button.YEnd, bottom)
			}
			if button.YStart%2 != 0 || button.YEnd%2 != 1 || button.YStart < height/8 || button.YEnd >= height-height/8 {
				t.Errorf("height %d button %d spans lines %d-%d, want whole line pairs inside the letterboxed picture", height, i+1, button.YStart, button.YEnd)
			}
			for other := range i {
				o := letterboxed[other]
				if button.XStart <= o.XEnd && o.XStart <= button.XEnd && button.YStart <= o.YEnd && o.YStart <= button.YEnd {
					t.Errorf("height %d: letterboxed buttons %d and %d overlap", height, other+1, i+1)
				}
			}
		}
		if buttons[0].YStart != 52 {
			t.Fatalf("letterboxing modified its input: first button starts on line %d", buttons[0].YStart)
		}
	}
}
