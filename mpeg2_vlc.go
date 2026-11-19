package video2dvd

// The coefficient VLCs below are the MPEG-1 DCT coefficient table used by
// MPEG-2 streams when intra_vlc_format is zero. The run/level order follows
// ISO/IEC 11172-2 Table B.14 (also H.262 Annex B).
var mpeg2DCTRunLevelCodes = [32][]mpeg2VLC{
	{
		{0x3, 2}, {0x4, 4}, {0x5, 5}, {0x6, 7}, {0x26, 8}, {0x21, 8}, {0xa, 10}, {0x1d, 12},
		{0x18, 12}, {0x13, 12}, {0x10, 12}, {0x1a, 13}, {0x19, 13}, {0x18, 13}, {0x17, 13},
		{0x1f, 14}, {0x1e, 14}, {0x1d, 14}, {0x1c, 14}, {0x1b, 14}, {0x1a, 14}, {0x19, 14},
		{0x18, 14}, {0x17, 14}, {0x16, 14}, {0x15, 14}, {0x14, 14}, {0x13, 14}, {0x12, 14},
		{0x11, 14}, {0x10, 14}, {0x18, 15}, {0x17, 15}, {0x16, 15}, {0x15, 15}, {0x14, 15},
		{0x13, 15}, {0x12, 15}, {0x11, 15}, {0x10, 15},
	},
	{{0x3, 3}, {0x6, 6}, {0x25, 8}, {0xc, 10}, {0x1b, 12}, {0x16, 13}, {0x15, 13}, {0x1f, 15}, {0x1e, 15}, {0x1d, 15}, {0x1c, 15}, {0x1b, 15}, {0x1a, 15}, {0x19, 15}, {0x13, 16}, {0x12, 16}, {0x11, 16}, {0x10, 16}},
	{{0x5, 4}, {0x4, 7}, {0xb, 10}, {0x14, 12}, {0x14, 13}},
	{{0x7, 5}, {0x24, 8}, {0x1c, 12}, {0x13, 13}},
	{{0x6, 5}, {0xf, 10}, {0x12, 12}},
	{{0x7, 6}, {0x9, 10}, {0x12, 13}},
	{{0x5, 6}, {0x1e, 12}, {0x14, 16}},
	{{0x4, 6}, {0x15, 12}},
	{{0x7, 7}, {0x11, 12}},
	{{0x5, 7}, {0x11, 13}},
	{{0x27, 8}, {0x10, 13}},
	{{0x23, 8}, {0x1a, 16}},
	{{0x22, 8}, {0x19, 16}},
	{{0x20, 8}, {0x18, 16}},
	{{0xe, 10}, {0x17, 16}},
	{{0xd, 10}, {0x16, 16}},
	{{0x8, 10}, {0x15, 16}},
	{{0x1f, 12}},
	{{0x1a, 12}},
	{{0x19, 12}},
	{{0x17, 12}},
	{{0x16, 12}},
	{{0x1f, 13}},
	{{0x1e, 13}},
	{{0x1d, 13}},
	{{0x1c, 13}},
	{{0x1b, 13}},
	{{0x1f, 16}},
	{{0x1e, 16}},
	{{0x1d, 16}},
	{{0x1c, 16}},
	{{0x1b, 16}},
}

// writeMPEG2DCTRunLevel writes a run/level pair using the short VLC whenever
// the pair is in the standard table. MPEG-2 escape coding remains necessary
// for larger levels and longer runs. The first non-intra coefficient has a
// shorter special code for run=0, |level|=1.
func writeMPEG2DCTRunLevel(bits *mpeg2BitWriter, run, level int, firstNonIntra bool) {
	absLevel := absInt(level)
	if firstNonIntra && run == 0 && absLevel == 1 {
		sign := 0
		if level < 0 {
			sign = 1
		}
		bits.write(uint64(0b10|sign), 2)
		return
	}
	if run >= 0 && run < len(mpeg2DCTRunLevelCodes) && absLevel > 0 && absLevel <= len(mpeg2DCTRunLevelCodes[run]) {
		code := mpeg2DCTRunLevelCodes[run][absLevel-1]
		sign := 0
		if level < 0 {
			sign = 1
		}
		bits.write(uint64(code.bits)<<1|uint64(sign), int(code.length)+1)
		return
	}
	bits.write(1, 6) // DCT escape code 000001.
	bits.write(uint64(run), 6)
	bits.write(uint64(level)&0x0fff, 12) // MPEG-2 signed_level.
}
