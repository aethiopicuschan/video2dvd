package video2dvd

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// This file is a pure-Go AC-3 (ATSC A/52:2018, Dolby Digital) encoder for
// 48 kHz 16-bit stereo. DVD-Video allows up to eight audio streams, but LPCM
// stereo alone takes 1.536 Mbit/s of the 10.08 Mbit/s mux rate, so a disc
// with several language tracks needs a compressed format, and AC-3 is the
// one every DVD player decodes.
//
// The encoder is deliberately simple and fixed: long blocks only, no
// coupling, one bit allocation parameter set and one SNR offset per frame.
// What it does adapt per frame is what matters most for quality at the DVD
// rates: exponent strategy, L/R or M/S rematrixing per band, and the SNR
// offset, which is searched so that each frame uses its whole bit budget.

const (
	// ac3SamplesPerFrame is the number of samples per channel in one AC-3
	// syncframe: six audio blocks of 256 new samples. Frame k is presented at
	// sample k*ac3SamplesPerFrame of the input.
	ac3SamplesPerFrame = 1536
	// ac3BlockSamples is the hop of one audio block; its transform window is
	// twice as long.
	ac3BlockSamples = 256
	ac3Blocks       = ac3SamplesPerFrame / ac3BlockSamples
	ac3Channels     = 2
	// ac3EncoderDelay is the delay of decoded audio relative to the input,
	// in samples: the writer starts every stream with one block of silence.
	// A decoder overlaps the first block of a stream with an all-zero
	// previous block, which leaves the block's time-domain aliasing
	// uncancelled: started on the input itself, the first 5.3 ms would
	// decode as the input mixed with its own time-reversed copy, a click on
	// a loud start. Behind a silent block every input sample decodes
	// exactly. ffmpeg's encoder has the same delay; the multiplexer presents
	// syncframes that much early (dvdAC3PresentationLead).
	ac3EncoderDelay = ac3BlockSamples
	// ac3FrameInputSamples is how many input samples per channel one frame
	// needs: its own 1536 plus the second half of the last block's window.
	ac3FrameInputSamples = ac3SamplesPerFrame + ac3BlockSamples
	ac3SampleRate        = 48000
	// ac3MaxCoefficients is the number of MDCT bins per block.
	ac3MaxCoefficients = 256
)

// Exponent strategies (chexpstr, A/52 Table 5.21). The number is also the
// log2 of how many bins share one exponent, plus one, for the D strategies.
const (
	ac3ExpReuse = 0
	ac3ExpD15   = 1
	ac3ExpD25   = 2
	ac3ExpD45   = 3
)

// The bit allocation parameters sent in block 0 of every frame. They are the
// common choices of AC-3 encoders: moderate masking decay and gain, and no
// low-level floor (floorcod 7), so that the SNR offset alone decides how
// close quantization noise comes to the masking curve.
const (
	ac3SlowDecayCode = 2
	ac3FastDecayCode = 1
	ac3SlowGainCode  = 1
	ac3DBPerBitCode  = 3
	ac3FloorCode     = 7
	ac3FastGainCode  = 4
)

// ac3ExponentChangeThreshold is the summed absolute difference between a
// block's exponents and the previous block's above which the block sends new
// exponents instead of reusing the previous ones. Spending exponent bits on
// a block whose spectrum barely moved buys almost nothing, while a stale
// envelope on a changing signal wastes mantissa bits or clips.
const ac3ExponentChangeThreshold = 500

// ac3TrailerBits is the fixed tail of a syncframe: auxdatae (no auxiliary
// data), crcrsv and crc2.
const ac3TrailerBits = 1 + 1 + 16

// ac3FrameBytes returns the syncframe size in bytes at 48 kHz, or 0 for an
// unsupported bitrate. At 48 kHz a frame lasts 32 ms, so the size is exact.
func ac3FrameBytes(bitrate int) int {
	if _, ok := ac3FrameSizeCodes[bitrate/1000]; !ok || bitrate%1000 != 0 {
		return 0
	}
	return bitrate * ac3SamplesPerFrame / (ac3SampleRate * 8)
}

// ac3OutputFrames returns how many syncframes cover inputSamples samples
// per channel, padded with silence to a whole frame. An ac3Writer given N
// samples writes ac3OutputFrames(N+ac3EncoderDelay): its silent block comes
// first, and the transform needs no further frame to flush.
func ac3OutputFrames(inputSamples int64) int64 {
	if inputSamples <= 0 {
		return 0
	}
	return (inputSamples + ac3SamplesPerFrame - 1) / ac3SamplesPerFrame
}

// ac3BandwidthCode returns chbwcod for a bitrate. The coded bandwidth is
// (73+3*chbwcod) bins of 93.75 Hz: code 46 ends at 19.78 kHz, which the
// lower DVD rates can still afford, and code 50 at 20.9 kHz.
func ac3BandwidthCode(bitrate int) int {
	if bitrate >= 320000 {
		return 50
	}
	return 46
}

// ac3Writer encodes big-endian 16-bit stereo PCM, the DVD LPCM byte layout,
// into a raw AC-3 elementary stream of complete syncframes.
type ac3Writer struct {
	dst     io.Writer
	encoder *ac3FrameEncoder
	frame   []byte
	// input holds the samples of the frame being collected plus the lookahead
	// of its last block; input[ch][0] is input sample 1536*framesWritten.
	input [ac3Channels][ac3FrameInputSamples]float64
	fill  int
	// pending keeps the bytes of a sample split across Write calls.
	pending       [4]byte
	pendingLen    int
	inputSamples  int64
	framesWritten int64
	err           error
	closed        bool
}

// newAC3Writer returns a writer that encodes to dst at one of ac3Bitrates.
func newAC3Writer(dst io.Writer, bitrate int) (*ac3Writer, error) {
	if dst == nil {
		return nil, errors.New("video2dvd: AC-3 destination writer is required")
	}
	supported := false
	for _, candidate := range ac3Bitrates {
		supported = supported || candidate == bitrate
	}
	if !supported {
		return nil, fmt.Errorf("video2dvd: unsupported AC-3 bitrate %d bit/s", bitrate)
	}
	w := &ac3Writer{
		dst:     dst,
		encoder: newAC3FrameEncoder(bitrate),
		frame:   make([]byte, ac3FrameBytes(bitrate)),
	}
	// The silent block of ac3EncoderDelay; nothing is written to dst yet.
	if _, err := w.Write(make([]byte, ac3EncoderDelay*ac3Channels*2)); err != nil {
		return nil, err
	}
	return w, nil
}

// Write consumes interleaved big-endian signed 16-bit stereo samples at
// 48 kHz in any byte split, and writes every syncframe whose input is
// complete.
func (w *ac3Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("video2dvd: write to a closed AC-3 writer")
	}
	if w.err != nil {
		return 0, w.err
	}
	consumed := 0
	for consumed < len(p) {
		if w.pendingLen > 0 || len(p)-consumed < 4 {
			copied := copy(w.pending[w.pendingLen:], p[consumed:])
			w.pendingLen += copied
			consumed += copied
			if w.pendingLen < len(w.pending) {
				break
			}
			w.appendSample(w.pending[:])
			w.pendingLen = 0
		} else {
			w.appendSample(p[consumed : consumed+4])
			consumed += 4
		}
		if w.fill == ac3FrameInputSamples {
			if err := w.encodeFrame(); err != nil {
				return consumed, err
			}
		}
	}
	return consumed, nil
}

// appendSample stores one stereo sample, scaled so that full scale is 1.
func (w *ac3Writer) appendSample(sample []byte) {
	w.input[0][w.fill] = float64(int16(uint16(sample[0])<<8|uint16(sample[1]))) / 32768
	w.input[1][w.fill] = float64(int16(uint16(sample[2])<<8|uint16(sample[3]))) / 32768
	w.fill++
	w.inputSamples++
}

// encodeFrame encodes the frame at the start of input, writes it, and keeps
// the lookahead as the start of the next frame's input.
func (w *ac3Writer) encodeFrame() error {
	if err := w.encoder.encode(&w.input, w.frame); err != nil {
		w.err = err
		return err
	}
	if _, err := w.dst.Write(w.frame); err != nil {
		w.err = err
		return err
	}
	w.framesWritten++
	for ch := range w.input {
		copy(w.input[ch][:], w.input[ch][ac3SamplesPerFrame:w.fill])
	}
	w.fill -= ac3SamplesPerFrame
	return nil
}

// Close pads the input with silence to a whole frame, writes the remaining
// ac3OutputFrames(n) frames, and leaves dst open. A trailing partial sample
// is an error, since it means the caller's PCM was misaligned.
func (w *ac3Writer) Close() error {
	if w.closed {
		return errors.New("video2dvd: AC-3 writer closed twice")
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	if w.pendingLen != 0 {
		return errors.New("video2dvd: AC-3 input ends with a partial sample")
	}
	for w.framesWritten < ac3OutputFrames(w.inputSamples) {
		for ch := range w.input {
			clear(w.input[ch][w.fill:])
		}
		w.fill = ac3FrameInputSamples
		if err := w.encodeFrame(); err != nil {
			return err
		}
	}
	return nil
}

// ac3FrameEncoder holds the per-frame working state, kept between frames
// only to avoid reallocating it.
type ac3FrameEncoder struct {
	mdct          ac3MDCT
	frameBytes    int
	frameSizeCode int
	bandwidthCode int
	endMantissa   int
	coefficients  [ac3Blocks][ac3Channels][ac3MaxCoefficients]float64
	rawExponents  [ac3Blocks][ac3Channels][ac3MaxCoefficients]int
	// exponents are the exponents a decoder reconstructs, which is what the
	// mantissas are scaled by and what the bit allocation runs on.
	exponents [ac3Blocks][ac3Channels][ac3MaxCoefficients]int
	strategy  [ac3Blocks][ac3Channels]int
	// codedExponents are the grouped exponent values of a block that sends
	// new exponents: the absolute first exponent then one value per group.
	codedExponents [ac3Blocks][ac3Channels][1 + 3*84]int
	exponentGroups [ac3Blocks][ac3Channels]int
	rematrix       [ac3Blocks][4]bool
	psd            [ac3Blocks][ac3Channels][ac3MaxCoefficients]int
	mask           [ac3Blocks][ac3Channels][len(ac3BandStart)]int
	bap            [ac3Blocks][ac3Channels][ac3MaxCoefficients]int
	sideBits       [ac3Blocks]int
}

// newAC3FrameEncoder prepares the transform and the bitrate-dependent
// header fields.
func newAC3FrameEncoder(bitrate int) *ac3FrameEncoder {
	e := &ac3FrameEncoder{
		frameBytes:    ac3FrameBytes(bitrate),
		frameSizeCode: ac3FrameSizeCodes[bitrate/1000],
		bandwidthCode: ac3BandwidthCode(bitrate),
	}
	e.endMantissa = 73 + 3*e.bandwidthCode
	e.mdct.init()
	return e
}

// encode turns 1792 samples per channel (the frame and its lookahead) into
// one syncframe in frame.
func (e *ac3FrameEncoder) encode(input *[ac3Channels][ac3FrameInputSamples]float64, frame []byte) error {
	for block := range ac3Blocks {
		for ch := range ac3Channels {
			start := block * ac3BlockSamples
			e.mdct.forward(input[ch][start:start+2*ac3BlockSamples], &e.coefficients[block][ch])
		}
		e.chooseRematrix(block)
		for ch := range ac3Channels {
			for bin := range e.endMantissa {
				e.rawExponents[block][ch][bin] = ac3Exponent(e.coefficients[block][ch][bin])
			}
		}
	}
	for ch := range ac3Channels {
		e.chooseExponents(ch)
	}
	for block := range ac3Blocks {
		for ch := range ac3Channels {
			ac3MaskingCurve(e.exponents[block][ch][:e.endMantissa], &e.psd[block][ch], &e.mask[block][ch])
		}
		var counter ac3BitWriter
		e.writeSideInfo(&counter, block, 0)
		e.sideBits[block] = counter.bits
	}

	frameBits := 8 * e.frameBytes
	words := e.frameBytes / 2
	fiveEighthsBytes := 2 * (words>>1 + words>>3)
	const headerBits = 40 + 27
	// fits reports whether an SNR offset keeps the frame within its size and
	// audio blocks 0 and 1 within the first 5/8 of it, as A/52 requires so
	// that a decoder can start on them once crc1 has been checked.
	fits := func(offset int) bool {
		total := headerBits
		for block := range ac3Blocks {
			total += e.sideBits[block] + e.allocate(block, offset)
			if block == 1 && total > 8*fiveEighthsBytes {
				return false
			}
		}
		return total <= frameBits-ac3TrailerBits
	}
	// The combined offset is csnroffst*16+fsnroffst. Zero is skipped because
	// some decoders treat an all-zero offset as "no bits at all" rather than
	// running the allocation; 1 is so coarse that it always fits.
	low, high := 1, 63*16+15
	if !fits(low) {
		return errors.New("video2dvd: AC-3 frame side information exceeds the frame size")
	}
	for low < high {
		middle := (low + high + 1) / 2
		if fits(middle) {
			low = middle
		} else {
			high = middle - 1
		}
	}
	for block := range ac3Blocks {
		e.allocate(block, low)
	}

	clear(frame)
	bits := ac3BitWriter{buf: frame}
	bits.put(0x0b77, 16)
	bits.put(0, 16) // crc1, filled in below
	bits.put(0, 2)  // fscod: 48 kHz
	bits.put(e.frameSizeCode, 6)
	e.writeBSI(&bits)
	for block := range ac3Blocks {
		e.writeSideInfo(&bits, block, low)
		e.writeMantissas(&bits, block)
	}
	if bits.bits > frameBits-ac3TrailerBits {
		return errors.New("video2dvd: AC-3 frame overflowed its bit budget")
	}
	// The unused bits are zero auxbits; auxdatae and crcrsv are zero too.
	crc1 := ac3CRC1(frame[4:fiveEighthsBytes])
	frame[2], frame[3] = byte(crc1>>8), byte(crc1)
	crc2 := ac3CRC16(frame[fiveEighthsBytes : e.frameBytes-2])
	if crc2 == 0x0b77 {
		// A frame that ends in a syncword could make a reader that has
		// lost sync start in the wrong place; crcrsv, the bit before crc2,
		// changes it.
		frame[e.frameBytes-3] |= 0x01
		crc2 = ac3CRC16(frame[fiveEighthsBytes : e.frameBytes-2])
	}
	frame[e.frameBytes-2], frame[e.frameBytes-1] = byte(crc2>>8), byte(crc2)
	return nil
}

// writeBSI writes the bit stream information: a plain stereo program at the
// -31 dB dialogue level, which decoders leave unattenuated.
func (e *ac3FrameEncoder) writeBSI(bits *ac3BitWriter) {
	bits.put(8, 5)  // bsid
	bits.put(0, 3)  // bsmod: complete main service
	bits.put(2, 3)  // acmod: L, R
	bits.put(0, 2)  // dsurmod: not indicated
	bits.put(0, 1)  // lfeon
	bits.put(31, 5) // dialnorm
	bits.put(0, 1)  // no heavy compression word
	bits.put(0, 1)  // langcode
	bits.put(0, 1)  // audprodie
	bits.put(0, 1)  // copyrightb
	bits.put(1, 1)  // origbs
	bits.put(0, 1)  // timecod1e
	bits.put(0, 1)  // timecod2e
	bits.put(0, 1)  // addbsie
}

// writeSideInfo writes everything of an audio block before its mantissas.
// Its length does not depend on the SNR offset, so the same routine counts
// the side information for the bit budget and writes it.
func (e *ac3FrameEncoder) writeSideInfo(bits *ac3BitWriter, block, offset int) {
	bits.put(0, 2) // blksw: long blocks
	bits.put(3, 2) // dithflag: dither zero-bit bins
	bits.put(0, 1) // dynrnge
	if block == 0 {
		bits.put(1, 1) // cplstre
		bits.put(0, 1) // cplinu
	} else {
		bits.put(0, 1)
	}
	bits.put(1, 1) // rematstr
	for _, flag := range e.rematrix[block] {
		bits.putBool(flag)
	}
	for ch := range ac3Channels {
		bits.put(e.strategy[block][ch], 2)
	}
	for ch := range ac3Channels {
		if e.strategy[block][ch] != ac3ExpReuse {
			bits.put(e.bandwidthCode, 6)
		}
	}
	for ch := range ac3Channels {
		if e.strategy[block][ch] == ac3ExpReuse {
			continue
		}
		coded := e.codedExponents[block][ch][:1+3*e.exponentGroups[block][ch]]
		bits.put(coded[0], 4)
		for group := range e.exponentGroups[block][ch] {
			d := coded[3*group : 3*group+4]
			bits.put(25*(d[1]-d[0]+2)+5*(d[2]-d[1]+2)+(d[3]-d[2]+2), 7)
		}
		bits.put(0, 2) // gainrng
	}
	if block == 0 {
		bits.put(1, 1) // baie
		bits.put(ac3SlowDecayCode, 2)
		bits.put(ac3FastDecayCode, 2)
		bits.put(ac3SlowGainCode, 2)
		bits.put(ac3DBPerBitCode, 2)
		bits.put(ac3FloorCode, 3)
		bits.put(1, 1) // snroffste
		bits.put(offset>>4, 6)
		for range ac3Channels {
			bits.put(offset&15, 4)
			bits.put(ac3FastGainCode, 3)
		}
	} else {
		bits.put(0, 1) // baie
		bits.put(0, 1) // snroffste
	}
	bits.put(0, 1) // deltbaie
	bits.put(0, 1) // skiple
}

// chooseRematrix decides per rematrixing band whether to code L and R or
// their half sum and half difference, whichever leaves the weaker of the two
// channels with less energy, and applies the choice to the coefficients. A
// near-mono band then costs little more than one channel.
func (e *ac3FrameEncoder) chooseRematrix(block int) {
	left, right := &e.coefficients[block][0], &e.coefficients[block][1]
	for band := range e.rematrix[block] {
		low, high := ac3RematrixBands[band], min(ac3RematrixBands[band+1], e.endMantissa)
		var separate, combined float64
		for bin := low; bin < high; bin++ {
			l, r := left[bin], right[bin]
			sum, difference := (l+r)/2, (l-r)/2
			separate += min(l*l, r*r)
			combined += min(sum*sum, difference*difference)
		}
		flag := low < high && combined < separate
		e.rematrix[block][band] = flag
		if flag {
			for bin := low; bin < high; bin++ {
				l, r := left[bin], right[bin]
				left[bin], right[bin] = (l+r)/2, (l-r)/2
			}
		}
	}
}

// chooseExponents picks the exponent strategy of one channel for the frame
// and computes the exponents a decoder will reconstruct. A block sends new
// exponents when its spectrum moved; a run of blocks sharing one exponent
// set gets finer frequency resolution the longer it is, since its cost is
// spread over more blocks.
func (e *ac3FrameEncoder) chooseExponents(ch int) {
	var fresh [ac3Blocks]bool
	fresh[0] = true
	for block := 1; block < ac3Blocks; block++ {
		difference := 0
		for bin := range e.endMantissa {
			delta := e.rawExponents[block][ch][bin] - e.rawExponents[block-1][ch][bin]
			difference += max(delta, -delta)
		}
		fresh[block] = difference > ac3ExponentChangeThreshold
	}
	for start := 0; start < ac3Blocks; {
		end := start + 1
		for end < ac3Blocks && !fresh[end] {
			end++
		}
		strategy := ac3ExpD15
		switch end - start {
		case 1:
			strategy = ac3ExpD45
		case 2, 3:
			strategy = ac3ExpD25
		}
		// Reused exponents must not be larger than any block's own, or that
		// block's mantissas would overflow.
		shared := e.rawExponents[start][ch]
		for block := start + 1; block < end; block++ {
			for bin := range e.endMantissa {
				shared[bin] = min(shared[bin], e.rawExponents[block][ch][bin])
			}
		}
		groups := e.encodeExponents(shared[:e.endMantissa], strategy, &e.codedExponents[start][ch], &e.exponents[start][ch])
		e.exponentGroups[start][ch] = groups
		e.strategy[start][ch] = strategy
		for block := start + 1; block < end; block++ {
			e.strategy[block][ch] = ac3ExpReuse
			e.exponents[block][ch] = e.exponents[start][ch]
		}
		start = end
	}
}

// encodeExponents groups exponents for a D15, D25 or D45 strategy (A/52
// 7.1.3) and returns the number of groups. Group values only ever go down
// from the raw exponents: a smaller exponent gives a mantissa more headroom,
// while a larger one would clip it. The result also has to keep successive
// differences within +-2 and the first exponent within 4 bits.
func (e *ac3FrameEncoder) encodeExponents(raw []int, strategy int, coded *[1 + 3*84]int, decoded *[ac3MaxCoefficients]int) int {
	end := len(raw)
	groupSize := 1 << (strategy - 1)
	var groups int
	switch strategy {
	case ac3ExpD15:
		groups = (end - 1) / 3
	case ac3ExpD25:
		groups = (end - 1 + 3) / 6
	default:
		groups = (end - 1 + 9) / 12
	}
	values := coded[:1+3*groups]
	values[0] = min(raw[0], 15)
	for group := 1; group < len(values); group++ {
		low := 1 + (group-1)*groupSize
		if low >= end {
			values[group] = values[group-1]
			continue
		}
		value := raw[low]
		for bin := low + 1; bin < min(low+groupSize, end); bin++ {
			value = min(value, raw[bin])
		}
		values[group] = value
	}
	for i := 1; i < len(values); i++ {
		values[i] = min(values[i], values[i-1]+2)
	}
	for i := len(values) - 2; i >= 0; i-- {
		values[i] = min(values[i], values[i+1]+2)
	}
	decoded[0] = values[0]
	for bin := 1; bin < end; bin++ {
		decoded[bin] = values[1+(bin-1)/groupSize]
	}
	return groups
}

// allocate runs the final step of the A/52 bit allocation (7.2.2.7) for one
// block at a combined SNR offset of csnroffst*16+fsnroffst, storing the
// pointers, and returns the mantissa bits they cost.
func (e *ac3FrameEncoder) allocate(block, offset int) int {
	snrOffset := (offset - 15*16) << 2
	floor := ac3Floor[ac3FloorCode]
	end := e.endMantissa
	var grouped [16]int
	bits := 0
	for ch := range ac3Channels {
		psd, mask, bap := &e.psd[block][ch], &e.mask[block][ch], &e.bap[block][ch]
		bin := 0
		for band := ac3MaskTab[0]; bin < end; band++ {
			last := min(ac3BandStart[band]+ac3BandSize[band], end)
			level := max(mask[band]-snrOffset-floor, 0)&0x1fe0 + floor
			for ; bin < last; bin++ {
				address := min(max((psd[bin]-level)>>5, 0), 63)
				pointer := ac3BAPTable[address]
				bap[bin] = pointer
				bits += ac3MantissaBits[pointer]
				grouped[pointer]++
			}
		}
	}
	return bits + 5*((grouped[1]+2)/3) + 7*((grouped[2]+2)/3) + 7*((grouped[4]+1)/2)
}

// ac3MaskingCurve computes the power spectral density and the masking curve
// of one channel block from its exponents, A/52 7.2.2.2 to 7.2.2.5 with the
// frame's fixed parameters. The curve does not depend on the SNR offset, so
// the offset search reuses it.
func ac3MaskingCurve(exponents []int, psd *[ac3MaxCoefficients]int, mask *[len(ac3BandStart)]int) {
	end := len(exponents)
	for bin, exponent := range exponents {
		psd[bin] = 3072 - exponent<<7
	}
	var bandPSD, excite [len(ac3BandStart)]int
	bin := 0
	for band := ac3MaskTab[0]; bin < end; band++ {
		last := min(ac3BandStart[band]+ac3BandSize[band], end)
		bandPSD[band] = psd[bin]
		for bin++; bin < last; bin++ {
			bandPSD[band] = ac3LogAddition(bandPSD[band], psd[bin])
		}
	}

	bandEnd := ac3MaskTab[end-1] + 1
	fastGain := ac3FastGain[ac3FastGainCode]
	slowGain := ac3SlowGain[ac3SlowGainCode]
	fastDecay := ac3FastDecay[ac3FastDecayCode]
	slowDecay := ac3SlowDecay[ac3SlowDecayCode]
	lowComp := ac3LowComp(0, bandPSD[0], bandPSD[1], 0)
	excite[0] = bandPSD[0] - fastGain - lowComp
	lowComp = ac3LowComp(lowComp, bandPSD[1], bandPSD[2], 1)
	excite[1] = bandPSD[1] - fastGain - lowComp
	begin := 7
	var fastLeak, slowLeak int
	for band := 2; band < 7; band++ {
		if bandEnd != 7 || band != 6 {
			lowComp = ac3LowComp(lowComp, bandPSD[band], bandPSD[band+1], band)
		}
		fastLeak = bandPSD[band] - fastGain
		slowLeak = bandPSD[band] - slowGain
		excite[band] = fastLeak - lowComp
		if (bandEnd != 7 || band != 6) && bandPSD[band] <= bandPSD[band+1] {
			begin = band + 1
			break
		}
	}
	for band := begin; band < min(bandEnd, 22); band++ {
		if bandEnd != 7 || band != 6 {
			lowComp = ac3LowComp(lowComp, bandPSD[band], bandPSD[band+1], band)
		}
		fastLeak = max(fastLeak-fastDecay, bandPSD[band]-fastGain)
		slowLeak = max(slowLeak-slowDecay, bandPSD[band]-slowGain)
		excite[band] = max(fastLeak-lowComp, slowLeak)
	}
	for band := 22; band < bandEnd; band++ {
		fastLeak = max(fastLeak-fastDecay, bandPSD[band]-fastGain)
		slowLeak = max(slowLeak-slowDecay, bandPSD[band]-slowGain)
		excite[band] = max(fastLeak, slowLeak)
	}

	knee := ac3DBPerBit[ac3DBPerBitCode]
	for band := range bandEnd {
		if bandPSD[band] < knee {
			excite[band] += (knee - bandPSD[band]) >> 2
		}
		mask[band] = max(excite[band], ac3HearingThreshold[band])
	}
}

// ac3LogAddition adds two powers in the allocator's log domain (A/52 logadd).
func ac3LogAddition(a, b int) int {
	difference := a - b
	address := min(max(difference, -difference)>>1, 255)
	if difference >= 0 {
		return a + ac3LogAdd[address]
	}
	return b + ac3LogAdd[address]
}

// ac3LowComp is calc_lowcomp of A/52 7.2.2.4: extra masking compensation in
// the lowest bands, where a strong tone in the next band would otherwise
// mask too much.
func ac3LowComp(a, b0, b1, band int) int {
	switch {
	case band < 7:
		if b0+256 == b1 {
			return 384
		} else if b0 > b1 {
			return max(0, a-64)
		}
	case band < 20:
		if b0+256 == b1 {
			return 320
		} else if b0 > b1 {
			return max(0, a-64)
		}
	default:
		return max(0, a-128)
	}
	return a
}

// writeMantissas quantizes and writes the mantissas of one block, both
// channels in order. Mantissas of bap 1, 2 and 4 are packed in groups of
// three, three and two; a group is written where its first member falls,
// and groups do not span blocks, so an unfinished group is completed with
// zero-index members.
func (e *ac3FrameEncoder) writeMantissas(bits *ac3BitWriter, block int) {
	var quantized [ac3Channels][ac3MaxCoefficients]int
	var groupCodes [5][ac3Channels * ac3MaxCoefficients]int
	var counts [5]int
	for ch := range ac3Channels {
		for bin := range e.endMantissa {
			pointer := e.bap[block][ch][bin]
			if pointer == 0 {
				continue
			}
			q := ac3Quantize(e.coefficients[block][ch][bin], e.exponents[block][ch][bin], pointer)
			quantized[ch][bin] = q
			switch pointer {
			case 1:
				groupCodes[1][counts[1]/3] += q * [3]int{9, 3, 1}[counts[1]%3]
				counts[1]++
			case 2:
				groupCodes[2][counts[2]/3] += q * [3]int{25, 5, 1}[counts[2]%3]
				counts[2]++
			case 4:
				groupCodes[4][counts[4]/2] += q * [2]int{11, 1}[counts[4]%2]
				counts[4]++
			}
		}
	}
	counts = [5]int{}
	for ch := range ac3Channels {
		for bin := range e.endMantissa {
			pointer := e.bap[block][ch][bin]
			switch pointer {
			case 0:
			case 1:
				if counts[1]%3 == 0 {
					bits.put(groupCodes[1][counts[1]/3], 5)
				}
				counts[1]++
			case 2:
				if counts[2]%3 == 0 {
					bits.put(groupCodes[2][counts[2]/3], 7)
				}
				counts[2]++
			case 4:
				if counts[4]%2 == 0 {
					bits.put(groupCodes[4][counts[4]/2], 7)
				}
				counts[4]++
			default:
				width := ac3MantissaBits[pointer]
				bits.put(quantized[ch][bin]&(1<<width-1), width)
			}
		}
	}
}

// ac3Quantize returns the coded mantissa of a coefficient: a level index of
// the symmetric quantizer for bap 1 to 5, or a two's-complement value for
// the wider pointers (A/52 7.3.3). The coefficient is normalized by its
// exponent first, which leaves it in [-1, 1).
func ac3Quantize(coefficient float64, exponent, pointer int) int {
	mantissa := math.Ldexp(coefficient, exponent)
	if pointer <= 5 {
		levels := ac3QuantizerLevels[pointer]
		index := int(math.Floor((mantissa + 1) * float64(levels) / 2))
		return min(max(index, 0), levels-1)
	}
	scale := 1 << (ac3MantissaBits[pointer] - 1)
	value := int(math.Round(mantissa * float64(scale)))
	return min(max(value, -scale), scale-1)
}

// ac3Exponent returns the exponent of a coefficient: the number of left
// shifts that bring its magnitude into [0.5, 1), limited to the 0..24 range
// that AC-3 can code.
func ac3Exponent(coefficient float64) int {
	if coefficient == 0 {
		return 24
	}
	_, exponent := math.Frexp(coefficient)
	return min(max(-exponent, 0), 24)
}

// ac3BitWriter packs bits most significant first into a zeroed buffer. With
// a nil buffer it only counts, so that side information can be measured by
// the code that writes it.
type ac3BitWriter struct {
	buf  []byte
	bits int
}

// put appends the low width bits of value.
func (w *ac3BitWriter) put(value, width int) {
	if w.buf == nil {
		w.bits += width
		return
	}
	for bit := width - 1; bit >= 0; bit-- {
		if value>>bit&1 != 0 {
			w.buf[w.bits>>3] |= 0x80 >> (w.bits & 7)
		}
		w.bits++
	}
}

// putBool appends one flag bit.
func (w *ac3BitWriter) putBool(flag bool) {
	if flag {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
}

// ac3CRCTable drives the A/52 CRC: generator x^16+x^15+x^2+1, most
// significant bit first, zero initial value.
var ac3CRCTable = func() (table [256]uint16) {
	for value := range table {
		crc := uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
		table[value] = crc
	}
	return table
}()

// ac3CRC16 returns the CRC register after data. Appending the result big
// endian makes the CRC of the whole zero, which is how crc2 is checked.
func ac3CRC16(data []byte) uint16 {
	var crc uint16
	for _, value := range data {
		crc = crc<<8 ^ ac3CRCTable[byte(crc>>8)^value]
	}
	return crc
}

// ac3CRC1 returns the crc1 word for the data that follows it up to the 5/8
// point of the frame. crc1 precedes the data it protects, so it cannot be
// appended like crc2: it is the value C whose CRC together with the data is
// zero, C*x^(8n+16) = CRC(data) modulo the generator for n data bytes. The
// generator has a constant term, so x is invertible and C is CRC(data)
// multiplied by x^-1 that many times; multiplying by x^-1 adds the generator
// when the constant term is set and then shifts right.
func ac3CRC1(data []byte) uint16 {
	crc := uint32(ac3CRC16(data))
	for range 8*len(data) + 16 {
		if crc&1 != 0 {
			crc ^= 0x18005
		}
		crc >>= 1
	}
	return uint16(crc)
}

// ac3MDCT is the forward transform of A/52 8.2.3 for 512-sample blocks,
// computed with a 128-point complex FFT. The window is the Kaiser-Bessel
// derived window with alpha 5 that A/52 decoders apply on the inverse, so
// the overlapped halves of consecutive blocks cancel their time-domain
// aliasing.
type ac3MDCT struct {
	window           [2 * ac3BlockSamples]float64
	preCos, preSin   [ac3MaxCoefficients / 2]float64
	postCos, postSin [ac3MaxCoefficients / 2]float64
	fftCos, fftSin   [ac3MaxCoefficients / 4]float64
	bitReverse       [ac3MaxCoefficients / 2]int
}

// init computes the window and the twiddle factors.
func (m *ac3MDCT) init() {
	const half = ac3BlockSamples
	var kernel [half + 1]float64
	var total float64
	for n := range kernel {
		x := 2*float64(n)/half - 1
		kernel[n] = ac3BesselI0(math.Pi * 5 * math.Sqrt(max(0, 1-x*x)))
		total += kernel[n]
	}
	var cumulative float64
	for n := range half {
		cumulative += kernel[n]
		m.window[n] = math.Sqrt(cumulative / total)
		m.window[2*half-1-n] = m.window[n]
	}
	const size = ac3MaxCoefficients // M, the number of coefficients
	for n := range size / 2 {
		angle := math.Pi * (float64(n) + 0.25) / size
		m.preCos[n], m.preSin[n] = math.Cos(angle), math.Sin(angle)
		angle = math.Pi * float64(n) / size
		m.postCos[n], m.postSin[n] = math.Cos(angle), math.Sin(angle)
	}
	for k := range size / 4 {
		angle := 2 * math.Pi * float64(k) / (size / 2)
		m.fftCos[k], m.fftSin[k] = math.Cos(angle), math.Sin(angle)
	}
	for i := range m.bitReverse {
		reversed := 0
		for bit := 1; bit < size/2; bit <<= 1 {
			reversed <<= 1
			if i&bit != 0 {
				reversed |= 1
			}
		}
		m.bitReverse[i] = reversed
	}
}

// ac3BesselI0 is the modified Bessel function of the first kind, order 0,
// summed from its power series to full float64 precision.
func ac3BesselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; term > sum*1e-17; k++ {
		factor := x / (2 * float64(k))
		term *= factor * factor
		sum += term
	}
	return sum
}

// forward transforms 512 samples into 256 coefficients,
// X[k] = -2/N * sum x[n]w[n]cos(2pi/(4N)(2n+1)(2k+1) + pi/4(2k+1)), N = 512.
// The MDCT is folded into a 256-point DCT-IV, which is computed as a
// 128-point complex FFT between two twiddle rotations.
func (m *ac3MDCT) forward(input []float64, output *[ac3MaxCoefficients]float64) {
	const size = ac3MaxCoefficients
	var folded [size]float64
	for n := range size / 2 {
		folded[n] = -input[3*size/2-1-n]*m.window[3*size/2-1-n] - input[3*size/2+n]*m.window[3*size/2+n]
		folded[size/2+n] = input[n]*m.window[n] - input[size-1-n]*m.window[size-1-n]
	}
	var re, im [size / 2]float64
	for n := range size / 2 {
		a, b := folded[2*n], folded[size-1-2*n]
		c, s := m.preCos[n], m.preSin[n]
		j := m.bitReverse[n]
		re[j], im[j] = a*c+b*s, b*c-a*s
	}
	for length := 2; length <= size/2; length <<= 1 {
		stride := size / 2 / length
		for start := 0; start < size/2; start += length {
			for k := range length / 2 {
				c, s := m.fftCos[k*stride], m.fftSin[k*stride]
				i, j := start+k, start+k+length/2
				tr := re[j]*c + im[j]*s
				ti := im[j]*c - re[j]*s
				re[j], im[j] = re[i]-tr, im[i]-ti
				re[i], im[i] = re[i]+tr, im[i]+ti
			}
		}
	}
	const scale = -2.0 / (2 * size)
	for k := range size / 2 {
		c, s := m.postCos[k], m.postSin[k]
		yr := re[k]*c + im[k]*s
		yi := im[k]*c - re[k]*s
		output[2*k] = scale * yr
		output[size-1-2*k] = -scale * yi
	}
}
