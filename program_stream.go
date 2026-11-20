package video2dvd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	DVDVOBSectorSize        = 2048
	defaultProgramMuxRate   = 1_260_000 // bytes per second (10.08 Mbit/s), the DVD-Video mux rate.
	maxProgramStreamMuxRate = (1<<22 - 1) * 50
	maxMPEGTimestamp        = (1 << 33) - 1
	mpegSystemClockHz       = 27_000_000
	mpegSCRExtensionRange   = 300 // 27 MHz ticks per 90 kHz tick.
	mpegPackHeaderSize      = 14
	mpegPESFixedHeaderSize  = 9 // start code, length, flags and header_data_length.
	// A remainder shorter than this is absorbed by PES header stuffing; a
	// padding packet needs six bytes of header and DVD streams never use one
	// for fewer than eight.
	mpegMinPaddingPacketSize = 8
	// programStreamMaxLead bounds how long before its PTS WritePES delivers a
	// packet, in 90 kHz ticks, so a sparse stream does not overflow a decoder.
	programStreamMaxLead = 27_000
)

// mpegSystemClock derives the system clock reference from the bytes already
// delivered at the program mux rate. Time otherwise moves only through wait,
// which models the sender staying idle; it is never set from a timestamp.
type mpegSystemClock struct {
	ticksPerByteNum uint64
	ticksPerByteDen uint64
	bytes           uint64
	idle            uint64 // 27 MHz ticks spent without sending data.
}

func newMPEGSystemClock(bytesPerSecond uint64) mpegSystemClock {
	divisor := uint64(mpegSystemClockHz)
	for remainder := bytesPerSecond; remainder != 0; {
		divisor, remainder = remainder, divisor%remainder
	}
	return mpegSystemClock{ticksPerByteNum: mpegSystemClockHz / divisor, ticksPerByteDen: bytesPerSecond / divisor}
}

// now returns the clock in 27 MHz ticks.
func (c *mpegSystemClock) now() uint64 {
	return c.after(0)
}

// after returns the clock once count more bytes have been delivered.
func (c *mpegSystemClock) after(count uint64) uint64 {
	return c.idle + (c.bytes+count)*c.ticksPerByteNum/c.ticksPerByteDen
}

func (c *mpegSystemClock) advance(count uint64) {
	c.bytes += count
}

func (c *mpegSystemClock) wait(until uint64) {
	if now := c.now(); until > now {
		c.idle += until - now
	}
}

// MPEGProgramStreamWriter packetizes PES data into 2048-byte MPEG-2 program
// stream sectors. It is a muxing primitive, not a complete DVD-Video authorer:
// DVD NAV packs, system/navigation tables, LPCM substream headers, and IFO/BUP
// files must be supplied by a higher layer.
type MPEGProgramStreamWriter struct {
	dst       io.Writer
	muxRate   uint32
	clock     mpegSystemClock
	lastPTS   uint64
	havePTS   bool
	closed    bool
	closedErr error
}

// NewMPEGProgramStreamWriter creates a sector writer with a 1.26 MB/s declared
// program mux rate. PES packets must be submitted in nondecreasing PTS order.
//
// The system clock reference starts at zero and advances with the bytes
// written, pausing only so that a packet is not delivered more than 0.3 s
// before its PTS. The writer never moves a timestamp: callers that need a
// decoder start-up delay must add it to the timestamps they pass in.
func NewMPEGProgramStreamWriter(dst io.Writer) (*MPEGProgramStreamWriter, error) {
	if dst == nil {
		return nil, errors.New("video2dvd: program stream destination is required")
	}
	rateUnits := defaultProgramMuxRate / 50
	if rateUnits == 0 || rateUnits > maxProgramStreamMuxRate/50 {
		return nil, errors.New("video2dvd: invalid program mux rate")
	}
	return &MPEGProgramStreamWriter{
		dst:     dst,
		muxRate: uint32(rateUnits),
		clock:   newMPEGSystemClock(defaultProgramMuxRate),
	}, nil
}

// WritePES writes a PES payload using a video, audio, or private_stream_1
// stream id. The first PES fragment carries PTS; continuation fragments do
// not. Each fragment is placed in its own fixed-size pack sector with padding.
func (w *MPEGProgramStreamWriter) WritePES(streamID byte, pts time.Duration, payload []byte) error {
	if w == nil || w.dst == nil {
		return errors.New("video2dvd: program stream writer is not configured")
	}
	if w.closedErr != nil {
		return w.closedErr
	}
	if w.closed {
		return errors.New("video2dvd: program stream writer is already closed")
	}
	if !validProgramStreamID(streamID) {
		return fmt.Errorf("video2dvd: invalid PES stream id 0x%02x", streamID)
	}
	ptsTicks, err := durationToMPEGTimestamp(pts)
	if err != nil {
		return err
	}
	if w.havePTS && ptsTicks < w.lastPTS {
		return errors.New("video2dvd: PES timestamps must be nondecreasing")
	}
	w.havePTS = true
	w.lastPTS = ptsTicks
	if ptsTicks > programStreamMaxLead {
		w.clock.wait((ptsTicks - programStreamMaxLead) * mpegSCRExtensionRange)
	}

	header := mpegPESHeader{streamID: streamID, hasPTS: true, pts: ptsTicks}
	for first := true; first || len(payload) > 0; first = false {
		count := min(len(payload), header.capacity())
		pack, err := buildMPEGPESPack(w.clock.now(), w.muxRate, header, payload[:count])
		if err == nil {
			err = writeAll(w.dst, pack)
		}
		if err != nil {
			w.closedErr = fmt.Errorf("video2dvd: write program stream pack: %w", err)
			return w.closedErr
		}
		w.clock.advance(DVDVOBSectorSize)
		payload = payload[count:]
		header.hasPTS = false
	}
	return nil
}

// Close writes the MPEG program_end_code in its own fixed-size pack. It does
// not close the destination writer.
func (w *MPEGProgramStreamWriter) Close() error {
	if w == nil || w.dst == nil {
		return errors.New("video2dvd: program stream writer is not configured")
	}
	if w.closed {
		return w.closedErr
	}
	w.closed = true
	if w.closedErr != nil {
		return w.closedErr
	}
	pack, err := buildMPEGProgramEndPack(w.clock.now(), w.muxRate)
	if err == nil {
		err = writeAll(w.dst, pack)
	}
	if err != nil {
		w.closedErr = fmt.Errorf("video2dvd: finish program stream: %w", err)
	}
	return w.closedErr
}

// mpegPESHeader describes the optional fields of one PES packet header.
type mpegPESHeader struct {
	streamID byte
	hasPTS   bool
	hasDTS   bool // only valid together with hasPTS.
	pts      uint64
	dts      uint64
	// pstdBuffer, when nonzero, is the 16-bit P-STD_buffer field: '01', the
	// buffer scale bit and the 13-bit buffer size.
	pstdBuffer uint16
}

func (h mpegPESHeader) dataLength() int {
	length := 0
	if h.hasPTS {
		length += 5
	}
	if h.hasDTS {
		length += 5
	}
	if h.pstdBuffer != 0 {
		length += 3
	}
	return length
}

// capacity returns the largest payload one pack can carry with this header.
func (h mpegPESHeader) capacity() int {
	return DVDVOBSectorSize - mpegPackHeaderSize - mpegPESFixedHeaderSize - h.dataLength()
}

// buildMPEGPESPack builds one sector holding a pack header and a single PES
// packet. scr is in 27 MHz ticks. A payload shorter than the capacity is
// completed with PES header stuffing when the gap is too small for a padding
// packet, and with a padding packet otherwise.
func buildMPEGPESPack(scr uint64, muxRate uint32, header mpegPESHeader, payload []byte) ([]byte, error) {
	spare := header.capacity() - len(payload)
	if spare < 0 || header.hasDTS && !header.hasPTS {
		return nil, errors.New("video2dvd: PES packet does not fit a program stream sector")
	}
	stuffing := 0
	if spare < mpegMinPaddingPacketSize {
		stuffing = spare
	}
	pack := make([]byte, DVDVOBSectorSize)
	if err := putMPEG2PackHeader(pack, scr, muxRate); err != nil {
		return nil, err
	}
	packet := pack[mpegPackHeaderSize:]
	headerData := header.dataLength() + stuffing
	packet[2], packet[3] = 1, header.streamID
	binary.BigEndian.PutUint16(packet[4:6], uint16(3+headerData+len(payload)))
	packet[6] = 0x80
	packet[8] = byte(headerData)
	offset := mpegPESFixedHeaderSize
	switch {
	case header.hasDTS:
		packet[7] |= 0xc0
		encodeMPEGTimestamp(packet[offset:offset+5], header.pts, 0x3)
		encodeMPEGTimestamp(packet[offset+5:offset+10], header.dts, 0x1)
		offset += 10
	case header.hasPTS:
		packet[7] |= 0x80
		encodeMPEGTimestamp(packet[offset:offset+5], header.pts, 0x2)
		offset += 5
	}
	if header.pstdBuffer != 0 {
		packet[7] |= 0x01 // PES_extension_flag
		packet[offset] = 0x1e
		binary.BigEndian.PutUint16(packet[offset+1:offset+3], header.pstdBuffer)
		offset += 3
	}
	for range stuffing {
		packet[offset] = 0xff
		offset++
	}
	offset += copy(packet[offset:], payload)
	if offset < len(packet) {
		putMPEGPaddingPacket(packet[offset:])
	}
	return pack, nil
}

// putMPEGPaddingPacket fills dst, which must hold at least the six header
// bytes, with one padding_stream packet.
func putMPEGPaddingPacket(dst []byte) {
	dst[0], dst[1], dst[2], dst[3] = 0, 0, 1, 0xbe
	binary.BigEndian.PutUint16(dst[4:6], uint16(len(dst)-6))
	for i := 6; i < len(dst); i++ {
		dst[i] = 0xff
	}
}

// buildMPEGProgramEndPack builds a sector that carries only padding and ends
// with the program_end_code.
func buildMPEGProgramEndPack(scr uint64, muxRate uint32) ([]byte, error) {
	pack := make([]byte, DVDVOBSectorSize)
	if err := putMPEG2PackHeader(pack, scr, muxRate); err != nil {
		return nil, err
	}
	putMPEGPaddingPacket(pack[mpegPackHeaderSize : DVDVOBSectorSize-4])
	copy(pack[DVDVOBSectorSize-4:], []byte{0, 0, 1, 0xb9})
	return pack, nil
}

// putMPEG2PackHeader writes the 14-byte MPEG-2 pack header for a system clock
// reference given in 27 MHz ticks and a mux rate in units of 50 bytes/s.
func putMPEG2PackHeader(dst []byte, scr uint64, muxRate uint32) error {
	base := scr / mpegSCRExtensionRange
	extension := scr % mpegSCRExtensionRange
	if base > maxMPEGTimestamp {
		return errors.New("video2dvd: program stream clock exceeds the MPEG 33-bit range")
	}
	dst[0], dst[1], dst[2], dst[3] = 0, 0, 1, 0xba
	dst[4] = 0x44 | byte(base>>30&0x07)<<3 | byte(base>>28&0x03)
	dst[5] = byte(base >> 20)
	dst[6] = 0x04 | byte(base>>15&0x1f)<<3 | byte(base>>13&0x03)
	dst[7] = byte(base >> 5)
	dst[8] = 0x04 | byte(base&0x1f)<<3 | byte(extension>>7&0x03)
	dst[9] = byte(extension&0x7f)<<1 | 0x01
	dst[10] = byte(muxRate >> 14)
	dst[11] = byte(muxRate >> 6)
	dst[12] = byte(muxRate&0x3f)<<2 | 0x03
	dst[13] = 0xf8 // reserved bits and a zero pack_stuffing_length.
	return nil
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
		return 0, errors.New("video2dvd: negative PES presentation time")
	}
	seconds := uint64(value / time.Second)
	remainder := uint64(value % time.Second)
	// Round to the nearest tick: frame times such as 1001/30000 s are not a
	// whole number of nanoseconds, and truncating them again loses a tick.
	ticks := seconds*90_000 + (remainder*90_000+uint64(time.Second)/2)/uint64(time.Second)
	if ticks > maxMPEGTimestamp {
		return 0, errors.New("video2dvd: PES timestamp exceeds the MPEG 33-bit range")
	}
	return ticks, nil
}

func validProgramStreamID(id byte) bool {
	return id == 0xbd || (id >= 0xc0 && id <= 0xdf) || (id >= 0xe0 && id <= 0xef)
}
