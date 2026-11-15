package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// SampleInfo describes one compressed MP4 sample. DTS, PTS, and Duration are
// expressed in the track's TimeScale. SampleNumber is one-based.
type SampleInfo struct {
	SampleNumber     int
	Offset           int64
	Size             uint32
	DTS              int64
	PTS              int64
	Duration         uint32
	KeyFrame         bool
	DescriptionIndex uint32
}

// SampleHandler receives compressed samples in track order. The Reader is
// limited to the current sample and becomes invalid after the callback returns.
type SampleHandler func(track TrackInfo, sample SampleInfo, data io.Reader) error

type sampleTables struct {
	sizes       []uint32
	chunkOffset []uint64
	chunkMap    []chunkRun
	timing      []timeRun
	composition []compositionRun
	syncSamples map[uint32]struct{}
	allSync     bool
}

const maxMP4SampleCount = 2_000_000
const maxMP4TableSize = 128 << 20

type chunkRun struct {
	firstChunk       uint32
	samplesPerChunk  uint32
	descriptionIndex uint32
}

type timeRun struct {
	count    uint32
	duration uint32
}

type compositionRun struct {
	count  uint32
	offset int64
}

// DemuxMP4 reports every compressed sample from a non-fragmented MP4 in track
// order. It does not decode samples or convert H.264 length prefixes to Annex B.
func DemuxMP4(path string, handler SampleHandler) (MP4Info, error) {
	if handler == nil {
		return MP4Info{}, errors.New("video2dvd: sample handler is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: open MP4: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return MP4Info{}, fmt.Errorf("video2dvd: stat MP4: %w", err)
	}
	return demuxMP4Reader(f, stat.Size(), handler)
}

func demuxMP4Reader(r io.ReaderAt, size int64, handler SampleHandler) (MP4Info, error) {
	info, err := ProbeMP4Reader(r, size)
	if err != nil {
		return MP4Info{}, err
	}
	top, err := readMP4Boxes(r, 0, size)
	if err != nil {
		return MP4Info{}, fmt.Errorf("%w: %v", ErrInvalidMP4, err)
	}
	var moov *mp4Box
	for i := range top {
		if top[i].kind == "moov" {
			moov = &top[i]
			break
		}
	}
	if moov == nil {
		return MP4Info{}, ErrInvalidMP4
	}
	children, err := readMP4Boxes(r, moov.data, moov.end)
	if err != nil {
		return MP4Info{}, fmt.Errorf("%w: %v", ErrInvalidMP4, err)
	}
	trackIndex := 0
	for _, child := range children {
		if child.kind != "trak" {
			continue
		}
		track := info.Tracks[trackIndex]
		trackIndex++
		if track.Kind == TrackKindOther {
			continue
		}
		tables, err := parseSampleTables(r, child)
		if err != nil {
			return MP4Info{}, fmt.Errorf("video2dvd: track %d sample tables: %w", trackIndex, err)
		}
		if err := emitTrackSamples(r, size, track, tables, handler); err != nil {
			return MP4Info{}, fmt.Errorf("video2dvd: track %d: %w", trackIndex, err)
		}
	}
	return info, nil
}

func parseSampleTables(r io.ReaderAt, trak mp4Box) (sampleTables, error) {
	trakChildren, err := readMP4Boxes(r, trak.data, trak.end)
	if err != nil {
		return sampleTables{}, err
	}
	var mdia *mp4Box
	for i := range trakChildren {
		if trakChildren[i].kind == "mdia" {
			mdia = &trakChildren[i]
			break
		}
	}
	if mdia == nil {
		return sampleTables{}, errors.New("trak has no mdia box")
	}
	mdiaChildren, err := readMP4Boxes(r, mdia.data, mdia.end)
	if err != nil {
		return sampleTables{}, err
	}
	var minf *mp4Box
	for i := range mdiaChildren {
		if mdiaChildren[i].kind == "minf" {
			minf = &mdiaChildren[i]
			break
		}
	}
	if minf == nil {
		return sampleTables{}, errors.New("mdia has no minf box")
	}
	minfChildren, err := readMP4Boxes(r, minf.data, minf.end)
	if err != nil {
		return sampleTables{}, err
	}
	var stbl *mp4Box
	for i := range minfChildren {
		if minfChildren[i].kind == "stbl" {
			stbl = &minfChildren[i]
			break
		}
	}
	if stbl == nil {
		return sampleTables{}, errors.New("minf has no stbl box")
	}
	boxes, err := readMP4Boxes(r, stbl.data, stbl.end)
	if err != nil {
		return sampleTables{}, err
	}
	tables := sampleTables{allSync: true}
	var hasSizes, hasChunks, hasChunkMap, hasTiming bool
	for i := range boxes {
		box := boxes[i]
		switch box.kind {
		case "stsz":
			tables.sizes, err = parseSampleSizes(r, box)
			hasSizes = err == nil
		case "stco":
			tables.chunkOffset, err = parseChunkOffsets(r, box, false)
			hasChunks = err == nil
		case "co64":
			tables.chunkOffset, err = parseChunkOffsets(r, box, true)
			hasChunks = err == nil
		case "stsc":
			tables.chunkMap, err = parseChunkMap(r, box)
			hasChunkMap = err == nil
		case "stts":
			tables.timing, err = parseTimeRuns(r, box)
			hasTiming = err == nil
		case "ctts":
			tables.composition, err = parseCompositionRuns(r, box)
		case "stss":
			tables.syncSamples, err = parseSyncSamples(r, box)
			tables.allSync = false
		}
		if err != nil {
			return sampleTables{}, fmt.Errorf("invalid %s box: %w", box.kind, err)
		}
	}
	if !hasSizes || !hasChunks || !hasChunkMap || !hasTiming {
		return sampleTables{}, errors.New("stbl is missing a required sample table")
	}
	if len(tables.sizes) > 0 && !tables.allSync && len(tables.syncSamples) == 0 {
		return sampleTables{}, errors.New("stss marks no samples as sync samples")
	}
	return tables, nil
}

func parseSampleSizes(r io.ReaderAt, box mp4Box) ([]uint32, error) {
	data, err := tableData(r, box)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 {
		return nil, io.ErrUnexpectedEOF
	}
	constantSize := binary.BigEndian.Uint32(data[4:8])
	sampleCount := binary.BigEndian.Uint32(data[8:12])
	if sampleCount > maxMP4SampleCount {
		return nil, errors.New("stsz sample count is too large")
	}
	sizes := make([]uint32, sampleCount)
	if constantSize != 0 {
		for i := range sizes {
			sizes[i] = constantSize
		}
		return sizes, nil
	}
	if uint64(sampleCount)*4 > uint64(len(data)-12) {
		return nil, io.ErrUnexpectedEOF
	}
	for i := range sizes {
		sizes[i] = binary.BigEndian.Uint32(data[12+i*4 : 16+i*4])
	}
	return sizes, nil
}

func parseChunkOffsets(r io.ReaderAt, box mp4Box, wide bool) ([]uint64, error) {
	data, err := tableData(r, box)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	entryCount := binary.BigEndian.Uint32(data[4:8])
	if entryCount > maxMP4SampleCount {
		return nil, errors.New("chunk offset count is too large")
	}
	entrySize := uint64(4)
	if wide {
		entrySize = 8
	}
	if uint64(entryCount)*entrySize > uint64(len(data)-8) {
		return nil, errors.New("invalid chunk offset count")
	}
	offsets := make([]uint64, entryCount)
	for i := range offsets {
		position := 8 + i*int(entrySize)
		if wide {
			offsets[i] = binary.BigEndian.Uint64(data[position : position+8])
		} else {
			offsets[i] = uint64(binary.BigEndian.Uint32(data[position : position+4]))
		}
	}
	return offsets, nil
}

func parseChunkMap(r io.ReaderAt, box mp4Box) ([]chunkRun, error) {
	data, err := tableData(r, box)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	entryCount := binary.BigEndian.Uint32(data[4:8])
	if entryCount > maxMP4SampleCount {
		return nil, errors.New("stsc entry count is too large")
	}
	if uint64(entryCount)*12 > uint64(len(data)-8) {
		return nil, errors.New("invalid stsc entry count")
	}
	runs := make([]chunkRun, entryCount)
	for i := range runs {
		position := 8 + i*12
		runs[i] = chunkRun{
			firstChunk:       binary.BigEndian.Uint32(data[position : position+4]),
			samplesPerChunk:  binary.BigEndian.Uint32(data[position+4 : position+8]),
			descriptionIndex: binary.BigEndian.Uint32(data[position+8 : position+12]),
		}
		if runs[i].firstChunk == 0 || runs[i].samplesPerChunk == 0 || runs[i].descriptionIndex == 0 {
			return nil, errors.New("stsc fields must be non-zero")
		}
		if i > 0 && runs[i].firstChunk <= runs[i-1].firstChunk {
			return nil, errors.New("stsc first_chunk values are not increasing")
		}
	}
	if len(runs) == 0 {
		return nil, errors.New("stsc has no entries")
	}
	if runs[0].firstChunk != 1 {
		return nil, errors.New("stsc does not start at chunk one")
	}
	return runs, nil
}

func parseTimeRuns(r io.ReaderAt, box mp4Box) ([]timeRun, error) {
	data, err := tableData(r, box)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	entryCount := binary.BigEndian.Uint32(data[4:8])
	if entryCount > maxMP4SampleCount {
		return nil, errors.New("stts entry count is too large")
	}
	if uint64(entryCount)*8 > uint64(len(data)-8) {
		return nil, errors.New("invalid stts entry count")
	}
	runs := make([]timeRun, entryCount)
	for i := range runs {
		position := 8 + i*8
		runs[i] = timeRun{
			count:    binary.BigEndian.Uint32(data[position : position+4]),
			duration: binary.BigEndian.Uint32(data[position+4 : position+8]),
		}
		if runs[i].count == 0 || runs[i].duration == 0 {
			return nil, errors.New("stts run has a zero count or duration")
		}
	}
	return runs, nil
}

func parseCompositionRuns(r io.ReaderAt, box mp4Box) ([]compositionRun, error) {
	data, err := tableData(r, box)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	entryCount := binary.BigEndian.Uint32(data[4:8])
	if entryCount > maxMP4SampleCount {
		return nil, errors.New("ctts entry count is too large")
	}
	if uint64(entryCount)*8 > uint64(len(data)-8) {
		return nil, errors.New("invalid ctts entry count")
	}
	version := data[0]
	runs := make([]compositionRun, entryCount)
	for i := range runs {
		position := 8 + i*8
		runs[i].count = binary.BigEndian.Uint32(data[position : position+4])
		if version == 1 {
			runs[i].offset = int64(int32(binary.BigEndian.Uint32(data[position+4 : position+8])))
		} else if version == 0 {
			runs[i].offset = int64(binary.BigEndian.Uint32(data[position+4 : position+8]))
		} else {
			return nil, fmt.Errorf("unsupported ctts version %d", version)
		}
		if runs[i].count == 0 {
			return nil, errors.New("ctts run has a zero count")
		}
	}
	return runs, nil
}

func parseSyncSamples(r io.ReaderAt, box mp4Box) (map[uint32]struct{}, error) {
	data, err := tableData(r, box)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	entryCount := binary.BigEndian.Uint32(data[4:8])
	if entryCount > maxMP4SampleCount {
		return nil, errors.New("stss entry count is too large")
	}
	if uint64(entryCount)*4 > uint64(len(data)-8) {
		return nil, errors.New("invalid stss entry count")
	}
	samples := make(map[uint32]struct{}, entryCount)
	var previous uint32
	for i := range entryCount {
		number := binary.BigEndian.Uint32(data[8+i*4 : 12+i*4])
		if number == 0 || number <= previous {
			return nil, errors.New("stss sample numbers must be positive and increasing")
		}
		samples[number] = struct{}{}
		previous = number
	}
	return samples, nil
}

func tableData(r io.ReaderAt, box mp4Box) ([]byte, error) {
	size := box.end - box.data
	if size < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	if size > maxMP4TableSize || size > int64(^uint(0)>>1) {
		return nil, errors.New("sample table is too large")
	}
	data := make([]byte, int(size))
	if _, err := r.ReadAt(data, box.data); err != nil {
		return nil, err
	}
	return data, nil
}

func emitTrackSamples(r io.ReaderAt, fileSize int64, track TrackInfo, tables sampleTables, handler SampleHandler) error {
	sampleCount := len(tables.sizes)
	if sampleCount == 0 {
		return nil
	}
	durations := make([]uint32, sampleCount)
	var sampleIndex int
	for _, run := range tables.timing {
		for i := uint32(0); i < run.count; i++ {
			if sampleIndex >= sampleCount {
				return errors.New("stts has more samples than stsz")
			}
			durations[sampleIndex] = run.duration
			sampleIndex++
		}
	}
	if sampleIndex != sampleCount {
		return errors.New("stts has fewer samples than stsz")
	}
	offsets := make([]uint64, sampleCount)
	descriptionIndices := make([]uint32, sampleCount)
	chunkRunIndex := 0
	sampleIndex = 0
	for chunkIndex, chunkOffset := range tables.chunkOffset {
		chunkNumber := uint32(chunkIndex + 1)
		for chunkRunIndex+1 < len(tables.chunkMap) && tables.chunkMap[chunkRunIndex+1].firstChunk <= chunkNumber {
			chunkRunIndex++
		}
		run := tables.chunkMap[chunkRunIndex]
		position := chunkOffset
		for i := uint32(0); i < run.samplesPerChunk; i++ {
			if sampleIndex >= sampleCount {
				return errors.New("stsc maps more samples than stsz")
			}
			size := uint64(tables.sizes[sampleIndex])
			if position > uint64(fileSize) || size > uint64(fileSize)-position || position+size > uint64(^uint64(0)>>1) {
				return fmt.Errorf("sample %d points outside the file", sampleIndex+1)
			}
			offsets[sampleIndex] = position
			descriptionIndices[sampleIndex] = run.descriptionIndex
			position += size
			sampleIndex++
		}
	}
	if sampleIndex != sampleCount {
		return errors.New("stsc maps fewer samples than stsz")
	}
	if tables.chunkMap[len(tables.chunkMap)-1].firstChunk > uint32(len(tables.chunkOffset)) {
		return errors.New("stsc references a chunk beyond the chunk offset table")
	}
	for number := range tables.syncSamples {
		if number > uint32(sampleCount) {
			return errors.New("stss references a sample beyond stsz")
		}
	}
	compositionOffsets := make([]int64, sampleCount)
	if len(tables.composition) > 0 {
		sampleIndex = 0
		for _, run := range tables.composition {
			for i := uint32(0); i < run.count; i++ {
				if sampleIndex >= sampleCount {
					return errors.New("ctts has more samples than stsz")
				}
				compositionOffsets[sampleIndex] = run.offset
				sampleIndex++
			}
		}
		if sampleIndex != sampleCount {
			return errors.New("ctts has fewer samples than stsz")
		}
	}
	var dts int64
	for i := range sampleCount {
		if descriptionIndices[i] != 1 {
			return fmt.Errorf("sample %d uses unsupported sample description %d", i+1, descriptionIndices[i])
		}
		pts := dts + compositionOffsets[i]
		keyFrame := tables.allSync
		if !tables.allSync {
			_, keyFrame = tables.syncSamples[uint32(i+1)]
		}
		sample := SampleInfo{
			SampleNumber:     i + 1,
			Offset:           int64(offsets[i]),
			Size:             tables.sizes[i],
			DTS:              dts,
			PTS:              pts,
			Duration:         durations[i],
			KeyFrame:         keyFrame,
			DescriptionIndex: descriptionIndices[i],
		}
		section := io.NewSectionReader(r, sample.Offset, int64(sample.Size))
		if err := handler(track, sample, section); err != nil {
			return err
		}
		dts += int64(durations[i])
	}
	return nil
}
