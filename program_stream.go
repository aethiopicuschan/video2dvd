package mp4todvd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	DVDVOBSectorSize        = 2048
	defaultProgramMuxRate   = 1_000_000 // bytes per second
	maxProgramStreamMuxRate = (1<<22 - 1) * 50
	maxMPEGTimestamp        = (1 << 33) - 1
)

// MPEGProgramStreamWriter packetizes PES data into 2048-byte MPEG-2 program
// stream sectors. It is a muxing primitive, not a complete DVD-Video authorer:
// DVD NAV packs, system/navigation tables, LPCM substream headers, and IFO/BUP
// files must be supplied by a higher layer.
type MPEGProgramStreamWriter struct {
	dst       io.Writer
	muxRate   uint32
	scr       uint64
	lastPTS   uint64
	havePTS   bool
	started   bool
	closedErr error
}

// NewMPEGProgramStreamWriter creates a sector writer with a 1 MB/s declared
// program mux rate. PES packets must be submitted in nondecreasing PTS order.
func NewMPEGProgramStreamWriter(dst io.Writer) (*MPEGProgramStreamWriter, error) {
	if dst == nil {
		return nil, errors.New("mp4todvd: program stream destination is required")
	}
	rateUnits := defaultProgramMuxRate / 50
	if rateUnits == 0 || rateUnits > maxProgramStreamMuxRate/50 {
		return nil, errors.New("mp4todvd: invalid program mux rate")
	}
	return &MPEGProgramStreamWriter{dst: dst, muxRate: uint32(rateUnits)}, nil
}

// WritePES writes a PES payload using a video, audio, or private_stream_1
// stream id. The first PES fragment carries PTS; continuation fragments do
// not. Each fragment is placed in its own fixed-size pack sector with padding.
func (w *MPEGProgramStreamWriter) WritePES(streamID byte, pts time.Duration, payload []byte) error {
	if w == nil || w.dst == nil {
		return errors.New("mp4todvd: program stream writer is not configured")
	}
	if w.closedErr != nil {
		return w.closedErr
	}
	if !validProgramStreamID(streamID) {
		return fmt.Errorf("mp4todvd: invalid PES stream id 0x%02x", streamID)
	}
	ptsTicks, err := durationToMPEGTimestamp(pts)
	if err != nil {
		return err
	}
	if w.havePTS && ptsTicks < w.lastPTS {
		return errors.New("mp4todvd: PES timestamps must be nondecreasing")
	}
	if !w.started || ptsTicks > w.scr {
		w.scr = ptsTicks
	}
	w.started = true
	w.havePTS = true
	w.lastPTS = ptsTicks

	first := true
	for first || len(payload) > 0 {
		ptsLength := 0
		optionalHeader := 3
		if first {
			ptsLength = 5
			optionalHeader += ptsLength
		}
		maxPayload := DVDVOBSectorSize - 14 - 6 - optionalHeader - 6
		count := len(payload)
		if count > maxPayload {
			count = maxPayload
		}
		packet := buildPESPacket(streamID, ptsTicks, first, payload[:count])
		if err := w.writePack(packet); err != nil {
			w.closedErr = fmt.Errorf("mp4todvd: write program stream pack: %w", err)
			return w.closedErr
		}
		payload = payload[count:]
		first = false
	}
	return nil
}

func (w *MPEGProgramStreamWriter) writePack(packet []byte) error {
	var pack bytes.Buffer
	pack.Write(buildMPEG2PackHeader(w.scr, w.muxRate))
	pack.Write(packet)
	padding := DVDVOBSectorSize - pack.Len()
	if padding != 0 {
		if padding < 6 || padding > 0xffff+6 {
			return fmt.Errorf("mp4todvd: cannot pad pack to a sector: %d bytes remain", padding)
		}
		pack.Write([]byte{0, 0, 1, 0xbe, byte((padding - 6) >> 8), byte(padding - 6)})
		pack.Write(bytes.Repeat([]byte{0xff}, padding-6))
	}
	if pack.Len() != DVDVOBSectorSize {
		return errors.New("mp4todvd: generated program stream pack has an invalid sector size")
	}
	if err := writeAll(w.dst, pack.Bytes()); err != nil {
		return err
	}
	step := (uint64(DVDVOBSectorSize)*90_000 + uint64(w.muxRate)*50 - 1) / (uint64(w.muxRate) * 50)
	w.scr += step
	return nil
}

func buildPESPacket(streamID byte, pts uint64, includePTS bool, payload []byte) []byte {
	optionalHeaderLength := 0
	flags := byte(0)
	if includePTS {
		optionalHeaderLength = 5
		flags = 0x80
	}
	packetLength := 3 + optionalHeaderLength + len(payload)
	packet := make([]byte, 6+3+optionalHeaderLength+len(payload))
	packet[2], packet[3] = 1, streamID
	binary.BigEndian.PutUint16(packet[4:6], uint16(packetLength))
	packet[6], packet[7], packet[8] = 0x80, flags, byte(optionalHeaderLength)
	if includePTS {
		encodeMPEGTimestamp(packet[9:14], pts, 0x2)
	}
	copy(packet[9+optionalHeaderLength:], payload)
	return packet
}

func buildMPEG2PackHeader(scr uint64, muxRate uint32) []byte {
	var bits mpeg2BitWriter
	bits.startCode(0xba)
	base := scr & maxMPEGTimestamp
	bits.write(0b01, 2)
	bits.write(base>>30, 3)
	bits.write(1, 1)
	bits.write(base>>15, 15)
	bits.write(1, 1)
	bits.write(base, 15)
	bits.write(1, 1)
	bits.write(0, 9) // SCR extension
	bits.write(1, 1)
	bits.write(uint64(muxRate), 22)
	bits.write(1, 1)
	bits.write(0x1f, 5)
	bits.write(0, 3) // pack stuffing length
	return bits.bytes()
}

func encodeMPEGTimestamp(dst []byte, timestamp uint64, prefix uint64) {
	dst[0] = byte(prefix<<4) | byte((timestamp>>29)&0x0e) | 1
	dst[1] = byte(timestamp >> 22)
	dst[2] = byte((timestamp>>14)&0xfe) | 1
	dst[3] = byte(timestamp >> 7)
	dst[4] = byte(timestamp<<1) | 1
}

func durationToMPEGTimestamp(value time.Duration) (uint64, error) {
	if value < 0 {
		return 0, errors.New("mp4todvd: negative PES presentation time")
	}
	seconds := uint64(value / time.Second)
	remainder := uint64(value % time.Second)
	ticks := seconds*90_000 + remainder*90_000/uint64(time.Second)
	if ticks > maxMPEGTimestamp {
		return 0, errors.New("mp4todvd: PES timestamp exceeds the MPEG 33-bit range")
	}
	return ticks, nil
}

func validProgramStreamID(id byte) bool {
	return id == 0xbd || (id >= 0xc0 && id <= 0xdf) || (id >= 0xe0 && id <= 0xef)
}
