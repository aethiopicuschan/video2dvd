package mp4todvd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// MuxDVDTitleVOB combines one MPEG-2 elementary stream and one raw DVD LPCM
// stream into fixed-size MPEG program-stream sectors. The result is a VOB
// media stream, but does not contain DVD navigation packs or IFO files.
func MuxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer) error {
	if ctx == nil {
		return errors.New("mp4todvd: context must not be nil")
	}
	if dst == nil {
		return errors.New("mp4todvd: VOB destination writer is required")
	}
	if title.VideoCodec != VideoCodecMPEG2 || title.AudioCodec != AudioCodecLPCM {
		return errors.New("mp4todvd: VOB muxer requires MPEG-2 video and LPCM audio")
	}
	if !validDVDFrameRate(title.FrameRateNumerator, title.FrameRateDenominator) {
		return fmt.Errorf("mp4todvd: unsupported DVD frame rate %d/%d", title.FrameRateNumerator, title.FrameRateDenominator)
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
		return errors.New("mp4todvd: MPEG-2 stream metadata does not match the encoded title")
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
	program, err := NewMPEGProgramStreamWriter(dst)
	if err != nil {
		return err
	}
	audio := &DVDLPCMPacketWriter{program: program, substream: 0xa0}
	videoPacket, videoErr := pictures.Next()
	if videoErr == io.EOF {
		return errors.New("mp4todvd: MPEG-2 stream contains no pictures")
	}
	if videoErr != nil {
		return videoErr
	}
	frameNumber := int64(0)
	var audioSampleNumber int64
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
		if haveAudio && (videoPacket == nil || audioChunk.Start <= videoPTS) {
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

		if err := program.WritePES(0xe0, videoPTS, videoPacket); err != nil {
			return err
		}
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
	return program.Close()
}

func validDVDFrameRate(numerator, denominator int) bool {
	return denominator == 1 && numerator == 25 || denominator == 1001 && (numerator == 24000 || numerator == 30000)
}

func dvdVideoFramePTS(frame int64, numerator, denominator int) (time.Duration, error) {
	if frame < 0 || !validDVDFrameRate(numerator, denominator) {
		return 0, errors.New("mp4todvd: invalid MPEG-2 frame timestamp")
	}
	whole := frame / int64(numerator)
	remainder := frame % int64(numerator)
	if whole > math.MaxInt64/int64(denominator) {
		return 0, errors.New("mp4todvd: video timestamp overflows duration")
	}
	wholeSeconds := whole * int64(denominator)
	fraction := remainder * int64(denominator)
	wholeSeconds += fraction / int64(numerator)
	fraction %= int64(numerator)
	if wholeSeconds > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("mp4todvd: video timestamp overflows duration")
	}
	wholeNanos := wholeSeconds * int64(time.Second)
	fractionNanos := fraction * int64(time.Second) / int64(numerator)
	if fractionNanos > math.MaxInt64-wholeNanos {
		return 0, errors.New("mp4todvd: video timestamp overflows duration")
	}
	return time.Duration(wholeNanos + fractionNanos), nil
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
		return PCMChunk{}, false, false, errors.New("mp4todvd: LPCM input ends in an incomplete stereo sample")
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

type mpeg2PictureReader struct {
	ctx          context.Context
	src          io.Reader
	buffer       []byte
	searchOffset int
	foundPicture bool
	finished     bool
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
				picture := append([]byte(nil), r.buffer[:start]...)
				r.buffer = append(r.buffer[:0], r.buffer[start:]...)
				r.searchOffset = 4
				return picture, nil
			}
			r.foundPicture = true
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
