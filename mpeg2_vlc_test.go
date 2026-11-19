package video2dvd_test

import (
	"testing"

	. "github.com/aethiopicuschan/video2dvd"
)

func TestMPEG2DCTRunLevelVLCUsesEveryStandardRun(t *testing.T) {
	for run, tc := range []struct {
		code  uint32
		width int
	}{
		{0x3, 2}, {0x3, 3}, {0x5, 4}, {0x7, 5}, {0x6, 5}, {0x7, 6}, {0x5, 6}, {0x4, 6},
		{0x7, 7}, {0x5, 7}, {0x27, 8}, {0x23, 8}, {0x22, 8}, {0x20, 8}, {0xe, 10}, {0xd, 10},
		{0x8, 10}, {0x1f, 12}, {0x1a, 12}, {0x19, 12}, {0x17, 12}, {0x16, 12}, {0x1f, 13},
		{0x1e, 13}, {0x1d, 13}, {0x1c, 13}, {0x1b, 13}, {0x1f, 16}, {0x1e, 16}, {0x1d, 16},
		{0x1c, 16}, {0x1b, 16},
	} {
		data := ExportMPEG2DCTRunLevel(run, 1, false)
		reader := ExportNewBitReader(data)
		got, err := ExportReadBits(reader, tc.width+1)
		if err != nil {
			t.Fatalf("run %d: read VLC: %v", run, err)
		}
		if want := tc.code << 1; got != want {
			t.Errorf("run %d, level +1 VLC = %#x/%d bits, want %#x/%d bits", run, got, tc.width+1, want, tc.width+1)
		}
	}
}

func TestMPEG2DCTRunLevelSpecialAndEscapeCodes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		run, level    int
		firstNonIntra bool
		width         int
		want          uint32
	}{
		{name: "first positive coefficient", run: 0, level: 1, firstNonIntra: true, width: 2, want: 0b10},
		{name: "first negative coefficient", run: 0, level: -1, firstNonIntra: true, width: 2, want: 0b11},
		{name: "run escape", run: 32, level: 1, width: 24, want: (1 << 18) | (32 << 12) | 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := ExportMPEG2DCTRunLevel(tc.run, tc.level, tc.firstNonIntra)
			reader := ExportNewBitReader(data)
			got, err := ExportReadBits(reader, tc.width)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("code = %#x, want %#x", got, tc.want)
			}
		})
	}
}
