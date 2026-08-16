package aac

import (
	"math"
	"testing"
)

// psTestTrees names every PS codebook for the completeness and round-trip
// checks.
var psTestTrees = []struct {
	name    string
	entries [][2]int8
	tree    [][2]int16
}{
	{"iid-fine-df", psHuffIIDFineDF[:], psTreeIIDFineDF},
	{"iid-fine-dt", psHuffIIDFineDT[:], psTreeIIDFineDT},
	{"iid-df", psHuffIIDDF[:], psTreeIIDDF},
	{"iid-dt", psHuffIIDDT[:], psTreeIIDDT},
	{"icc-df", psHuffICCDF[:], psTreeICCDF},
	{"icc-dt", psHuffICCDT[:], psTreeICCDT},
	{"ipd-df", psHuffIPDDF[:], psTreeIPDDF},
	{"ipd-dt", psHuffIPDDT[:], psTreeIPDDT},
	{"opd-df", psHuffOPDDF[:], psTreeOPDDF},
	{"opd-dt", psHuffOPDDT[:], psTreeOPDDT},
}

// TestPSTreesComplete asserts every codebook listing tiles the code space
// exactly (buildPSTree returns nil on any transcription slip: a length
// set violating Kraft equality, a collision, or a descent through a
// leaf).
func TestPSTreesComplete(t *testing.T) {
	for _, tc := range psTestTrees {
		if tc.tree == nil {
			t.Errorf("%s: codebook listing does not build a complete tree", tc.name)
		}
	}
}

// psHuffPath returns the bit path decoding to value, mirroring the SBR
// tests' huffPath for the PS pair trees.
func psHuffPath(tree [][2]int16, value int) []byte {
	var walk func(idx int, path []byte) []byte
	walk = func(idx int, path []byte) []byte {
		for bit := 0; bit < 2; bit++ {
			v := tree[idx][bit]
			p := append(append([]byte(nil), path...), byte(bit))
			if v < 0 {
				if int(v)-psLeafBias == value {
					return p
				}
				continue
			}
			if r := walk(int(v), p); r != nil {
				return r
			}
		}
		return nil
	}
	return walk(0, nil)
}

// TestPSHuffRoundTrip decodes every listed codeword through the built
// tree at its listed length.
func TestPSHuffRoundTrip(t *testing.T) {
	for _, tc := range psTestTrees {
		seen := map[int]bool{}
		for _, e := range tc.entries {
			val, bits := int(e[0]), int(e[1])
			if seen[val] {
				t.Errorf("%s: value %d listed twice", tc.name, val)
			}
			seen[val] = true
			path := psHuffPath(tc.tree, val)
			if path == nil {
				t.Fatalf("%s: value %d unreachable", tc.name, val)
			}
			if len(path) != bits {
				t.Errorf("%s: value %d at depth %d, listed %d", tc.name, val, len(path), bits)
			}
			var w sbrTestBits
			for _, b := range path {
				w.put(uint32(b), 1)
			}
			if got := psHuffDecode(w.reader(), tc.tree); got != val {
				t.Errorf("%s: decoded %d, want %d", tc.name, got, val)
			}
		}
	}
}

// TestPSMixLUT pins the mixing matrices' structural properties: identity
// at IID 0 / full correlation, the energy-preservation invariant of
// procedure A, and the c-to-1/c symmetry that swaps the channel columns
// when the IID sign flips.
func TestPSMixLUT(t *testing.T) {
	const tol = 1e-6
	id := psMixLUT[0][7][0] // coarse IID 0, ICC index 0 (rho 1)
	for j, want := range [4]float32{1, 1, 0, 0} {
		if math.Abs(float64(id[j]-want)) > tol {
			t.Errorf("identity h[%d] = %g, want %g", j, id[j], want)
		}
	}
	for i := 0; i < 46; i++ {
		for icc := 0; icc < 8; icc++ {
			h := psMixLUT[0][i][icc]
			sum := float64(h[0]*h[0] + h[2]*h[2] + h[1]*h[1] + h[3]*h[3])
			if math.Abs(sum-2) > 1e-5 {
				t.Fatalf("row %d icc %d: |h1|^2+|h2|^2 = %g, want 2", i, icc, sum)
			}
		}
	}
	for i := 0; i < 15; i++ {
		// c -> 1/c swaps the channel scales, so the columns swap; the sine
		// components also negate (beta flips with the scale difference).
		a, b := psMixLUT[0][i][3], psMixLUT[0][14-i][3]
		if math.Abs(float64(a[0]-b[1])) > tol || math.Abs(float64(a[2]+b[3])) > tol {
			t.Errorf("coarse rows %d/%d: IID sign flip must swap the columns", i, 14-i)
		}
	}
}

// TestPSHybridIdentity is the filterbank's structural gate: summing every
// sub-subband of a QMF band back together must reproduce the band
// exactly, because the modulated prototypes tile to a unit impulse. Run
// on random spectra in both band modes, it pins the prototypes, the
// modulation, and the passthrough alignment, with no oracle involved.
// The summation is permutation-invariant, so the channel ORDER (which
// decorrelation and mixing consume) is not pinned here; the fdk
// differential covers it, since a reordered channel maps to the wrong
// stereo parameters.
func TestPSHybridIdentity(t *testing.T) {
	for _, is34 := range []int{0, 1} {
		ps := &psDec{}
		seed := uint32(0x2b_11_88)
		rnd := func() float32 {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			return float32(int32(seed)) / float32(math.MaxInt32)
		}
		var want [38][64][2]float32
		for l := range want {
			for k := range want[l] {
				want[l][k] = [2]float32{rnd(), rnd()}
			}
		}
		ps.xBuf = want
		ps.hybridAnalysis(is34)
		ps.rHyb = ps.lHyb // synthesis reads both; mirror so rBuf checks too
		ps.hybridSynthesis(is34)
		for l := 0; l < sbrSlots; l++ {
			for k := 0; k < 64; k++ {
				for c := 0; c < 2; c++ {
					got := ps.xBuf[l][k][c]
					if diff := math.Abs(float64(got - want[l][k][c])); diff > 2e-5 {
						t.Fatalf("is34=%d slot %d band %d: got %g want %g (diff %g)",
							is34, l, k, got, want[l][k][c], diff)
					}
					if got != ps.rBuf[l][k][c] {
						t.Fatalf("is34=%d slot %d band %d: left/right synthesis disagree", is34, l, k)
					}
				}
			}
		}
	}
}
