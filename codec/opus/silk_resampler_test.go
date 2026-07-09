package opus

import (
	"math"
	"testing"
)

// TestSilkResamplerExactOutput pins the resampler's output count at exactly
// inLen*48/fsIn for every SILK internal rate and frame duration. The count is
// only exact because init rounds invRatioQ16 up until invRatio*fsOut covers
// fsIn<<1 (the reference's "round up to avoid undersizing"); without that
// loop the 8 and 16 kHz ratios truncate low and the interpolator would run
// one extra iteration per call, writing past the output. The out slice here
// is sized exactly, so an overrun fails loudly as a bounds panic.
func TestSilkResamplerExactOutput(t *testing.T) {
	wantInvRatio := map[int]int32{8000: 21846, 12000: 32768, 16000: 43691}
	for _, fsIn := range []int{8000, 12000, 16000} {
		var S silkResampler
		S.init(fsIn, 48000)
		if S.invRatioQ16 != wantInvRatio[fsIn] {
			t.Errorf("%d->48000: invRatioQ16 = %d, want %d (round-up missing or changed)",
				fsIn, S.invRatioQ16, wantInvRatio[fsIn])
		}
		for _, ms := range []int{10, 20, 40, 60} {
			inLen := fsIn / 1000 * ms
			in := make([]int16, inLen)
			for i := range in {
				in[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(fsIn)))
			}
			out := make([]int16, inLen*48000/fsIn)
			S.resample(out, in, inLen)
		}
	}
}
