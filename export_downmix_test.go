package video2dvd

// Test-only exports for the multichannel downmix. They live apart from
// export_test.go so that unrelated shims do not collide.

// ExportVorbisStereoDownmix returns the normalised left and right gains that
// fold an Opus channel mapping family 1 layout with the given channel count.
func ExportVorbisStereoDownmix(channels int) (left, right []float64, err error) {
	mix, err := newStereoDownmix(vorbisChannelOrder[channels])
	if err != nil {
		return nil, nil, err
	}
	return mix.left, mix.right, nil
}

// ExportApplyVorbisStereoDownmix folds interleaved float PCM in the Vorbis
// order for its channel count into interleaved 16-bit stereo.
func ExportApplyVorbisStereoDownmix(channels int, src []float32) ([]int16, error) {
	mix, err := newStereoDownmix(vorbisChannelOrder[channels])
	if err != nil {
		return nil, err
	}
	dst := make([]int16, len(src)/channels*2)
	if err := mix.apply(dst, src); err != nil {
		return nil, err
	}
	return dst, nil
}
