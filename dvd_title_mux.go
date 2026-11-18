package video2dvd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

const (
	dvdVOBMaxSectors   = (1<<30)/DVDVOBSectorSize - 1
	dvdVOBUMaxDuration = time.Second
)

// MuxDVDTitleVOB combines one MPEG-2 elementary stream and one raw DVD LPCM
// stream into VOBUs with NAV packs and fixed-size MPEG program-stream sectors.
// The result does not include IFO/BUP files or complete DVD navigation tables.
func MuxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer) error {
	return muxDVDTitleVOB(ctx, title, workDir, dst, nil, nil)
}

type dvdVOBURecord struct {
	StartSector uint32
	EndSector   uint32
}

func muxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer, recordVOBU func(dvdVOBURecord) error, buttonPages [][]DVDNAVButton) error {
	if ctx == nil {
		return errors.New("video2dvd: context must not be nil")
	}
	if dst == nil {
		return errors.New("video2dvd: VOB destination writer is required")
	}
	if title.VideoCodec != VideoCodecMPEG2 || title.AudioCodec != AudioCodecLPCM {
		return errors.New("video2dvd: VOB muxer requires MPEG-2 video and LPCM audio")
	}
	if !validDVDFrameRate(title.FrameRateNumerator, title.FrameRateDenominator) {
		return fmt.Errorf("video2dvd: unsupported DVD frame rate %d/%d", title.FrameRateNumerator, title.FrameRateDenominator)
	}
	videoPath, err := validateGeneratedFile(title.VideoPath, workDir, "video")
	if err != nil {
		return err
	}
	audioPath, err := validateGeneratedFile(title.AudioPath, workDir, "audio")
	if err != nil {
		return err
	}
	videoInfo, err := ProbeMPEG2Video(videoPath)
	if err != nil {
		return err
	}
	if videoInfo.Width != title.Width || videoInfo.Height != title.Height || videoInfo.Aspect != title.Aspect ||
		videoInfo.FrameRateNumerator*title.FrameRateDenominator != title.FrameRateNumerator*videoInfo.FrameRateDenominator {
		return errors.New("video2dvd: MPEG-2 stream metadata does not match the encoded title")
	}
	videoFile, err := os.Open(videoPath)
	if err != nil {
		return err
	}
	defer videoFile.Close()
	audioFile, err := os.Open(audioPath)
	if err != nil {
		return err
	}
	defer audioFile.Close()

	pictures := mpeg2PictureReader{ctx: ctx, src: videoFile}
	mediaBuffer := &bytes.Buffer{}
	program, err := NewMPEGProgramStreamWriter(mediaBuffer)
	if err != nil {
		return err
	}
	audio := &DVDLPCMPacketWriter{program: program, substream: 0xa0}
	videoPacket, videoErr := pictures.Next()
	if videoErr == io.EOF {
		return errors.New("video2dvd: MPEG-2 stream contains no pictures")
	}
	if videoErr != nil {
		return videoErr
	}
	firstPictureType, err := mpeg2PictureType(videoPacket)
	if err != nil || firstPictureType != 1 || !mpeg2PictureHasSequenceHeader(videoPacket) {
		return errors.New("video2dvd: the first MPEG-2 picture must be an I picture with a sequence header")
	}
	frameNumber := int64(0)
	var audioSampleNumber int64
	var nextVOBUSector uint32
	var groupStartPTS uint32
	var groupEndPTS uint32
	var groupStartTime time.Duration
	var groupEndTime time.Duration
	var groupCellElapsed [4]byte
	var groupSCR uint64
	var referenceEnds [3]uint32
	var referenceCount int
	menuVOBUIndex := 0
	startVOBU := func(start time.Duration) error {
		startTicks, err := durationToMPEGTimestamp(start)
		if err != nil {
			return err
		}
		groupCellElapsed, err = dvdCellElapsedTime(start, title.FrameRateNumerator, title.FrameRateDenominator)
		if err != nil {
			return err
		}
		if !program.started || startTicks > program.scr {
			program.scr = startTicks
		}
		program.started = true
		groupSCR = program.scr
		groupStartPTS = uint32(startTicks)
		groupEndPTS = groupStartPTS
		groupStartTime = start
		groupEndTime = start
		referenceEnds = [3]uint32{math.MaxUint32, math.MaxUint32, math.MaxUint32}
		referenceCount = 0
		// Account for the NAV sector before writing the VOBU's media packs.
		step := (uint64(DVDVOBSectorSize)*90_000 + uint64(program.muxRate)*50 - 1) / (uint64(program.muxRate) * 50)
		program.scr += step
		mediaBuffer = &bytes.Buffer{}
		program.dst = mediaBuffer
		return nil
	}
	flushVOBU := func() error {
		if mediaBuffer.Len() == 0 || mediaBuffer.Len()%DVDVOBSectorSize != 0 {
			return errors.New("video2dvd: VOBU media is empty or not sector-aligned")
		}
		if groupEndTime < groupStartTime || groupEndTime-groupStartTime > dvdVOBUMaxDuration {
			return errors.New("video2dvd: MPEG-2 GOP exceeds the DVD VOBU duration limit")
		}
		mediaSectors := uint32(mediaBuffer.Len() / DVDVOBSectorSize)
		if uint64(nextVOBUSector)+uint64(mediaSectors)+1 > dvdVOBMaxSectors {
			return errors.New("video2dvd: VOB title exceeds the 1 GiB single-file limit")
		}
		var buttons []DVDNAVButton
		cellID := uint8(1)
		if menuVOBUIndex < len(buttonPages) {
			buttons = buttonPages[menuVOBUIndex]
			if menuVOBUIndex >= math.MaxUint8 {
				return errors.New("video2dvd: menu contains more cells than a DVD VOB can address")
			}
			cellID = uint8(menuVOBUIndex + 1)
		}
		nav, err := BuildDVDNAVPack(DVDNAVPackInfo{
			SCRBase:                   groupSCR,
			LogicalBlockNumber:        nextVOBUSector,
			VOBUEndAddress:            mediaSectors,
			FirstReferenceEndAddress:  referenceEnds[0],
			SecondReferenceEndAddress: referenceEnds[1],
			ThirdReferenceEndAddress:  referenceEnds[2],
			VOBID:                     1,
			CellID:                    cellID,
			StartPTM:                  groupStartPTS,
			EndPTM:                    groupEndPTS,
			CellElapsedTime:           groupCellElapsed,
			Buttons:                   buttons,
		})
		if err != nil {
			return err
		}
		if err := writeAll(dst, nav); err != nil {
			return err
		}
		if err := writeAll(dst, mediaBuffer.Bytes()); err != nil {
			return err
		}
		if recordVOBU != nil {
			if err := recordVOBU(dvdVOBURecord{StartSector: nextVOBUSector, EndSector: nextVOBUSector + mediaSectors}); err != nil {
				return err
			}
		}
		menuVOBUIndex++
		nextVOBUSector += mediaSectors + 1
		return nil
	}
	firstVideoPTS, err := dvdVideoFramePTS(frameNumber, title.FrameRateNumerator, title.FrameRateDenominator)
	if err != nil {
		return err
	}
	if err := startVOBU(firstVideoPTS); err != nil {
		return err
	}
	audioChunk, haveAudio, finalAudioChunk, err := readDVDLPCMAudioChunk(audioFile, audioSampleNumber)
	if err != nil {
		return err
	}
	audioDone := !haveAudio
	if audioDone {
		if err := audio.Close(); err != nil {
			return err
		}
	}
	for videoPacket != nil || haveAudio {
		if err := ctx.Err(); err != nil {
			return err
		}
		videoPTS := time.Duration(math.MaxInt64)
		if videoPacket != nil {
			videoPTS, err = dvdVideoFramePTS(frameNumber, title.FrameRateNumerator, title.FrameRateDenominator)
			if err != nil {
				return err
			}
		}
		if haveAudio && (videoPacket == nil || audioChunk.Start < videoPTS) {
			if err := audio.WriteChunk(audioChunk); err != nil {
				return err
			}
			audioSampleNumber += int64(len(audioChunk.Samples) / 2)
			if finalAudioChunk {
				if err := audio.Close(); err != nil {
					return err
				}
				audioDone = true
				haveAudio = false
			} else {
				audioChunk, haveAudio, finalAudioChunk, err = readDVDLPCMAudioChunk(audioFile, audioSampleNumber)
				if err != nil {
					return err
				}
				if !haveAudio {
					audioDone = true
					if err := audio.Close(); err != nil {
						return err
					}
				}
			}
			continue
		}
		pictureType, err := mpeg2PictureType(videoPacket)
		if err != nil {
			return err
		}
		if pictureType == 3 {
			return errors.New("video2dvd: B pictures are not supported by the DVD VOB muxer")
		}
		if pictureType != 1 && pictureType != 2 {
			return fmt.Errorf("video2dvd: unsupported MPEG-2 picture type %d", pictureType)
		}
		if pictureType == 1 && frameNumber > 0 {
			if !mpeg2PictureHasSequenceHeader(videoPacket) {
				return errors.New("video2dvd: each VOBU I picture must include a sequence header")
			}
			if err := flushVOBU(); err != nil {
				return err
			}
			if err := startVOBU(videoPTS); err != nil {
				return err
			}
		}
		beforeSectors := uint32(mediaBuffer.Len() / DVDVOBSectorSize)

		if err := program.WritePES(0xe0, videoPTS, videoPacket); err != nil {
			return err
		}
		afterSectors := uint32(mediaBuffer.Len() / DVDVOBSectorSize)
		if afterSectors <= beforeSectors {
			return errors.New("video2dvd: MPEG-2 picture did not produce a VOB sector")
		}
		if referenceCount < len(referenceEnds) {
			referenceEnds[referenceCount] = afterSectors
			referenceCount++
		}
		nextFramePTS, err := dvdVideoFramePTS(frameNumber+1, title.FrameRateNumerator, title.FrameRateDenominator)
		if err != nil {
			return err
		}
		endTicks, err := durationToMPEGTimestamp(nextFramePTS)
		if err != nil {
			return err
		}
		groupEndPTS = uint32(endTicks)
		groupEndTime = nextFramePTS
		frameNumber++
		videoPacket, err = pictures.Next()
		if err == io.EOF {
			videoPacket = nil
		} else if err != nil {
			return err
		}
	}
	if !audioDone {
		if err := audio.Close(); err != nil {
			return err
		}
	}
	if err := program.Close(); err != nil {
		return err
	}
	return flushVOBU()
}

func validDVDFrameRate(numerator, denominator int) bool {
	return denominator == 1 && numerator == 25 || denominator == 1001 && (numerator == 24000 || numerator == 30000)
}

func dvdVideoFramePTS(frame int64, numerator, denominator int) (time.Duration, error) {
	if frame < 0 || !validDVDFrameRate(numerator, denominator) {
		return 0, errors.New("video2dvd: invalid MPEG-2 frame timestamp")
	}
	whole := frame / int64(numerator)
	remainder := frame % int64(numerator)
	if whole > math.MaxInt64/int64(denominator) {
		return 0, errors.New("video2dvd: video timestamp overflows duration")
	}
	wholeSeconds := whole * int64(denominator)
	fraction := remainder * int64(denominator)
	wholeSeconds += fraction / int64(numerator)
	fraction %= int64(numerator)
	if wholeSeconds > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("video2dvd: video timestamp overflows duration")
	}
	wholeNanos := wholeSeconds * int64(time.Second)
	fractionNanos := fraction * int64(time.Second) / int64(numerator)
	if fractionNanos > math.MaxInt64-wholeNanos {
		return 0, errors.New("video2dvd: video timestamp overflows duration")
	}
	return time.Duration(wholeNanos + fractionNanos), nil
}

func dvdCellElapsedTime(elapsed time.Duration, numerator, denominator int) ([4]byte, error) {
	if elapsed < 0 || !validDVDFrameRate(numerator, denominator) {
		return [4]byte{}, errors.New("video2dvd: invalid DVD cell elapsed time")
	}
	nominalRate := int64(30)
	frameRateCode := byte(3) // 29.97 fps time-code family.
	if numerator == 25 && denominator == 1 {
		nominalRate = 25
		frameRateCode = 1
	}
	totalSeconds := int64(elapsed / time.Second)
	hours := totalSeconds / 3600
	if hours > 99 {
		return [4]byte{}, errors.New("video2dvd: DVD cell elapsed time exceeds 99 hours")
	}
	minutes := totalSeconds / 60 % 60
	seconds := totalSeconds % 60
	frames := int64(elapsed%time.Second) * nominalRate / int64(time.Second)
	return [4]byte{
		toPackedBCD(hours),
		toPackedBCD(minutes),
		toPackedBCD(seconds),
		frameRateCode<<6 | byte(frames),
	}, nil
}

func toPackedBCD(value int64) byte {
	return byte(value/10<<4 | value%10)
}

func readDVDLPCMAudioChunk(src io.Reader, sampleNumber int64) (PCMChunk, bool, bool, error) {
	const sampleFramesPerPacket = dvdLPCMSamplesPerFrame * dvdLPCMFramesPerPacket
	buffer := make([]byte, sampleFramesPerPacket*2*2)
	n, err := io.ReadFull(src, buffer)
	if err == io.EOF && n == 0 {
		return PCMChunk{}, false, true, nil
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return PCMChunk{}, false, false, err
	}
	if n%4 != 0 {
		return PCMChunk{}, false, false, errors.New("video2dvd: LPCM input ends in an incomplete stereo sample")
	}
	samples := make([]int16, n/2)
	for i := range samples {
		samples[i] = int16(binary.BigEndian.Uint16(buffer[i*2 : i*2+2]))
	}
	start, err := dvdLPCMSamplesDuration(sampleNumber)
	if err != nil {
		return PCMChunk{}, false, false, err
	}
	return PCMChunk{
		Start:      start,
		SampleRate: 48000,
		Channels:   2,
		Samples:    samples,
	}, true, err == io.ErrUnexpectedEOF, nil
}

func mpeg2PictureType(picture []byte) (uint32, error) {
	start := findMPEGStartCode(picture, 0x00, 0)
	if start < 0 {
		return 0, errors.New("video2dvd: MPEG-2 picture start code not found")
	}
	reader := bitReader{data: picture[start+4:]}
	if _, err := reader.read(10); err != nil {
		return 0, err
	}
	return reader.read(3)
}

func mpeg2PictureHasSequenceHeader(picture []byte) bool {
	pictureStart := findMPEGStartCode(picture, 0x00, 0)
	sequenceStart := findMPEGStartCode(picture, 0xb3, 0)
	return sequenceStart >= 0 && pictureStart > sequenceStart
}

type mpeg2PictureReader struct {
	ctx                 context.Context
	src                 io.Reader
	buffer              []byte
	searchOffset        int
	currentPictureStart int
	foundPicture        bool
	finished            bool
}

func (r *mpeg2PictureReader) Next() ([]byte, error) {
	chunk := make([]byte, 32*1024)
	for {
		if r.ctx != nil {
			if err := r.ctx.Err(); err != nil {
				return nil, err
			}
		}
		if start := findMPEGStartCode(r.buffer, 0x00, r.searchOffset); start >= 0 {
			if r.foundPicture {
				boundary := start
				for _, code := range [...]byte{0xb3, 0xb8} {
					preamble := findMPEGStartCode(r.buffer, code, r.currentPictureStart+4)
					if preamble >= 0 && preamble < boundary {
						boundary = preamble
					}
				}
				picture := append([]byte(nil), r.buffer[:boundary]...)
				r.buffer = append(r.buffer[:0], r.buffer[boundary:]...)
				if boundary < start {
					r.foundPicture = false
					r.searchOffset = 0
					r.currentPictureStart = 0
				} else {
					r.searchOffset = start - boundary + 4
					r.currentPictureStart = 0
				}
				return picture, nil
			}
			r.foundPicture = true
			r.currentPictureStart = start
			r.searchOffset = start + 4
			continue
		}
		if r.finished {
			if r.foundPicture && len(r.buffer) != 0 {
				picture := append([]byte(nil), r.buffer...)
				r.buffer = nil
				return picture, nil
			}
			return nil, io.EOF
		}
		if len(r.buffer) > 3 {
			r.searchOffset = max(r.searchOffset, len(r.buffer)-3)
		}
		n, err := r.src.Read(chunk)
		if n > 0 {
			r.buffer = append(r.buffer, chunk[:n]...)
		}
		if err == io.EOF {
			r.finished = true
		} else if err != nil {
			return nil, err
		} else if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}
