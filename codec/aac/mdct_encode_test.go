package aac

import (
	"math"
	"testing"
)

// mdctDirect is the reference O(N²) forward MDCT at the encoder's scale
// of 2 (the complement of the decoder's 2/N inverse).
func mdctDirect(x, spec []float64) {
	n := len(x)
	n0 := (float64(n)/2 + 1) / 2
	for k := 0; k < n/2; k++ {
		var sum float64
		for i := 0; i < n; i++ {
			sum += x[i] * math.Cos(2*math.Pi/float64(n)*(float64(i)+n0)*(float64(k)+0.5))
		}
		spec[k] = 2 * sum
	}
}

func lcgBlock(n int, seed uint32) []float64 {
	x := make([]float64, n)
	state := seed
	for i := range x {
		state = state*1664525 + 1013904223
		x[i] = float64(int32(state)) / (1 << 31)
	}
	return x
}

// TestMDCTFastMatchesDirect checks the FFT-based forward MDCT against the
// direct transform for both sizes.
func TestMDCTFastMatchesDirect(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan *mdctPlan
	}{
		{"long", mdctLong},
		{"short", mdctShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.plan.inv.n
			x := lcgBlock(n, 0x9e3779b9)
			want := make([]float64, n/2)
			got := make([]float64, n/2)
			mdctDirect(x, want)
			tc.plan.mdct(x, got)
			var maxErr float64
			for i := range want {
				if e := math.Abs(got[i] - want[i]); e > maxErr {
					maxErr = e
				}
			}
			// Direct-sum rounding grows with N; 1e-8 is ~13 digits at N=2048.
			if maxErr > 1e-8 {
				t.Fatalf("%s MDCT max error %g exceeds 1e-8", tc.name, maxErr)
			}
		})
	}
}

// TestMDCTRoundTripTDAC drives the encoder's window+MDCT and the decoder's
// IMDCT+window+overlap-add over a window-sequence walk covering every
// transition (long, start, eight-short, stop), asserting the middle frames
// reconstruct the input to numerical precision. This pins the forward
// transform's scale and every taper offset against the decoder's.
func TestMDCTRoundTripTDAC(t *testing.T) {
	seqs := []int{onlyLong, onlyLong, longStart, eightShort, eightShort, longStop, onlyLong, longStart, eightShort, longStop, onlyLong}
	frames := len(seqs)
	total := (frames + 1) * 1024
	src := lcgBlock(total, 0x2545f491)

	// Encode: frame m windows src[m*1024 : m*1024+2048).
	specs := make([][1024]float64, frames)
	for m := 0; m < frames; m++ {
		var tblk [2048]float64
		copy(tblk[:], src[m*1024:m*1024+2048])
		mdctFrame(&tblk, seqs[m], &specs[m])
	}

	// Decode: the decoder's filterbank path, overlap-add by hand.
	overlap := make([]float64, 1024)
	out := make([]float64, 0, frames*1024)
	for m := 0; m < frames; m++ {
		var cur [2048]float64
		if seqs[m] == eightShort {
			cd := &channelData{}
			cd.spec = specs[m]
			cd.info = icsInfo{windowSequence: eightShort, numWindows: 8}
			shortFilterbank(cd, shapeSine, shapeSine, &cur)
		} else {
			var z [2048]float64
			planLong.imdct(specs[m][:], z[:])
			longWindowApply(&z, &cur, seqs[m], shapeSine, shapeSine)
		}
		for i := 0; i < 1024; i++ {
			out = append(out, cur[i]+overlap[i])
			overlap[i] = cur[1024+i]
		}
	}

	// Frame m's decoded block overlap-adds frames m-1 and m, reconstructing
	// src[m*1024 : (m+1)*1024). The first frame has no left partner.
	var maxErr float64
	for i := 1024; i < frames*1024; i++ {
		if e := math.Abs(out[i] - src[i]); e > maxErr {
			maxErr = e
		}
	}
	if maxErr > 1e-9 {
		t.Fatalf("TDAC round-trip max error %g exceeds 1e-9", maxErr)
	}
}
