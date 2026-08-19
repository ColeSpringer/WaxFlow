package testutil

import (
	"math"
	"testing"
)

// TestODGProxyMonotonic checks the proxy orders distortion correctly: an
// identical signal grades 0, and increasing noise grades progressively worse,
// all within [-4, 0]. The gate relies on this ordering, not on absolute PEAQ
// agreement.
func TestODGProxyMonotonic(t *testing.T) {
	const n = 44100
	rate := 44100
	ref := make([]float32, n)
	for i := range ref {
		x := float64(i)
		ref[i] = float32(0.3*math.Sin(2*math.Pi*440*x/44100) +
			0.2*math.Sin(2*math.Pi*2500*x/44100) +
			0.1*math.Sin(2*math.Pi*9000*x/44100))
	}
	noisy := func(amp float64, seed uint64) []float32 {
		out := make([]float32, n)
		s := seed
		for i := range out {
			s = s*6364136223846793005 + 1442695040888963407
			r := float64(int64(s>>11)) / float64(1<<53)
			out[i] = ref[i] + float32((2*r-1)*amp)
		}
		return out
	}
	identical := ODGProxy(ref, ref, rate, 1)
	good := ODGProxy(ref, noisy(0.0003, 1), rate, 1)
	mid := ODGProxy(ref, noisy(0.004, 2), rate, 1)
	bad := ODGProxy(ref, noisy(0.04, 3), rate, 1)
	t.Logf("identical=%.3f good=%.3f mid=%.3f bad=%.3f", identical, good, mid, bad)
	if !(identical >= good && good > mid && mid > bad) {
		t.Errorf("ODG proxy not monotonic: identical=%.3f good=%.3f mid=%.3f bad=%.3f", identical, good, mid, bad)
	}
	for _, v := range []float64{identical, good, mid, bad} {
		if v > 0 || v < -4 {
			t.Errorf("ODG %.3f out of [-4, 0]", v)
		}
	}
}

// TestODGProxyFailsClosed checks that unmeasurable input (a catastrophically
// broken encoder producing empty or too-short output) scores the worst grade,
// not the best: the gate must fail closed so a broken encoder cannot pass it.
func TestODGProxyFailsClosed(t *testing.T) {
	ref := make([]float32, 44100)
	for i := range ref {
		ref[i] = float32(0.3 * math.Sin(2*math.Pi*440*float64(i)/44100))
	}
	for _, tc := range []struct {
		name string
		test []float32
	}{
		{"empty", nil},
		{"short", ref[:100]},
	} {
		if got := ODGProxy(ref, tc.test, 44100, 1); got != -4 {
			t.Errorf("%s output ODG %.3f, want -4 (fail closed)", tc.name, got)
		}
	}
}

// TestAlignLagFindsTruePriming pins the alignment search at the primings
// the gated encoders actually ship (HE-AAC's 3010, libfdk v1's ~5058 and
// v2's ~7106), on both aperiodic and purely periodic content. The
// periodic case is what makes odgMaxLag raises safe to reason about: a
// wider search window can only displace the true lag if a later one is
// strictly 5% better (alignLag returns the SMALLEST lag within
// tolerance), so a period-aliased equal-error lag must lose. Before this
// pin the only evidence a maxLag change moved no gate was prose.
func TestAlignLagFindsTruePriming(t *testing.T) {
	const n = 44100
	aperiodic := make([]float32, n)
	periodic := make([]float32, n)
	seed := uint64(9)
	for i := range aperiodic {
		seed = seed*6364136223846793005 + 1442695040888963407
		env := 0.5 + 0.5*math.Sin(2*math.Pi*3.1*float64(i)/44100)
		aperiodic[i] = float32(env*0.3*math.Sin(2*math.Pi*440*float64(i)/44100) +
			0.05*(2*float64(seed>>11)/float64(1<<53)-1))
		periodic[i] = float32(0.3 * math.Sin(2*math.Pi*441*float64(i)/44100)) // exact 100-sample period
	}
	for _, content := range []struct {
		name string
		ref  []float32
	}{{"aperiodic", aperiodic}, {"periodic", periodic}} {
		for _, priming := range []int{3010, 5058, 7106} {
			test := make([]float32, priming+n)
			copy(test[priming:], content.ref)
			if got := alignLag(content.ref, test, odgMaxLag); got != priming {
				t.Errorf("%s content, priming %d: alignLag found %d", content.name, priming, got)
			}
		}
	}
}
