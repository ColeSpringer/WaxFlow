package resample

import (
	"fmt"
	"math"
	"sync"

	"github.com/colespringer/waxflow/dsp/internal/firwin"
	"github.com/colespringer/waxflow/waxerr"
)

// bank is an immutable polyphase coefficient set for one reduced ratio
// and profile, shared by every Resampler using it.
type bank struct {
	l, m  int
	tpp   int       // taps per phase
	delay int       // filter center D on the upsampled (L-fold) grid
	coef  []float32 // l*tpp entries, phase-major, taps reversed per phase
}

// profileSpec are the design constants behind each Profile's public
// guarantees. attenDB carries margin over the documented gate to absorb
// float32 coefficient quantization and accumulation noise, so the gate is
// met as measured, not as designed.
type profileSpec struct {
	attenDB  float64 // Kaiser design attenuation
	passEdge float64 // passband edge as a fraction of the narrower Nyquist
	stopEdge float64 // stopband edge, likewise (transition straddles 1.0)
}

var profiles = map[Profile]profileSpec{
	HQ:   {attenDB: 115, passEdge: 0.91, stopEdge: 1.09},
	Fast: {attenDB: 72, passEdge: 0.85, stopEdge: 1.15},
}

type bankKey struct {
	l, m int
	p    Profile
}

// bankEntry carries one ratio's design exactly once. The map lock only
// guards entry creation; the design itself runs under the entry's Once,
// so a slow design (large coprime ratios run to tens of milliseconds)
// never blocks lookups of other ratios, and concurrent sessions asking
// for the same ratio share a single computation instead of racing
// duplicates. Errors are cached too: the design is deterministic, so a
// ratio over the bank cap fails identically every time.
type bankEntry struct {
	once sync.Once
	b    *bank
	err  error
}

var (
	bankMu    sync.Mutex
	bankCache = map[bankKey]*bankEntry{}
)

// bankFor returns the cached coefficient bank for a rate pair, designing
// it on first use. Sessions repeat a handful of ratios (the 44.1k and 48k
// families), so after warmup a Resampler costs two window allocations.
func bankFor(inRate, outRate int, p Profile) (*bank, error) {
	g := gcd(inRate, outRate)
	key := bankKey{outRate / g, inRate / g, p}

	bankMu.Lock()
	e, ok := bankCache[key]
	if !ok {
		e = &bankEntry{}
		bankCache[key] = e
	}
	bankMu.Unlock()

	e.once.Do(func() { e.b, e.err = design(key.l, key.m, p) })
	return e.b, e.err
}

// design builds the Kaiser windowed-sinc prototype at the upsampled rate
// L*inRate and slices it into L phases.
//
// Frequencies are normalized to the upsampled rate. The cutoff sits at
// the narrower of the two Nyquists, nuC = min(1, L/M)/(2L); the
// transition band spans [passEdge, stopEdge]*nuC, symmetric around the
// cutoff, so the don't-care region absorbs its own foldover (see the
// package comment).
func design(l, m int, p Profile) (*bank, error) {
	spec := profiles[p]
	nuC := math.Min(1, float64(l)/float64(m)) / (2 * float64(l))
	deltaOmega := 2 * math.Pi * (spec.stopEdge - spec.passEdge) * nuC

	// Kaiser length and shape estimates (Oppenheim & Schafer). Odd length
	// gives an integer center, which keeps delay compensation exact.
	//
	// The length is bounded while still in float64: extreme rate pairs
	// (large coprime L or M) estimate lengths past int range, and the
	// int conversion would overflow negative, mask itself behind the
	// minimum-length clamp, and blow up the arithmetic below. The phase
	// count gets the matching guard before the ceiling division.
	nf := math.Ceil((spec.attenDB-7.95)/(2.285*deltaOmega)) + 1
	if !(nf > 0 && nf <= maxBankFloats) {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("resample: ratio %d/%d needs a %.0f-tap filter, over the supported bound", l, m, nf))
	}
	n := max(int(nf), 9)
	if n%2 == 0 {
		n++
	}
	beta := 0.1102 * (spec.attenDB - 8.7)

	// The padded bank holds tpp*l >= l coefficients, so an over-cap L can
	// never fit; rejecting it here also caps both operands of the ceiling
	// division, making the remaining arithmetic overflow-free (the total
	// is at most n+l-1, comfortably in range).
	if l > maxBankFloats {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("resample: ratio %d/%d needs at least %d phases, over the supported bound", l, m, l))
	}
	tpp := (n + l - 1) / l
	total := tpp * l
	if total > maxBankFloats {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("resample: ratio %d/%d needs a %d-tap filter, over the supported bound", l, m, total))
	}

	// Prototype in float64: sinc at the cutoff, Kaiser window, then a
	// global sum of L for unity passband gain through zero-stuffing.
	// Indices n..total-1 stay zero padding; the center D is unaffected.
	h := make([]float64, total)
	d := (n - 1) / 2
	i0beta := firwin.BesselI0(beta)
	var sum float64
	for i := 0; i < n; i++ {
		x := float64(i - d)
		t := x / float64(d)
		w := firwin.BesselI0(beta*math.Sqrt(1-t*t)) / i0beta
		h[i] = firwin.Sinc(2*nuC*x) * w
		sum += h[i]
	}
	scale := float64(l) / sum

	// Phase-major, taps reversed: coef[p][j] = h[p + (tpp-1-j)*L] makes
	// the inner loop a forward dot product over contiguous history.
	coef := make([]float32, total)
	for p := 0; p < l; p++ {
		for j := 0; j < tpp; j++ {
			coef[p*tpp+j] = float32(h[p+(tpp-1-j)*l] * scale)
		}
	}
	return &bank{l: l, m: m, tpp: tpp, delay: d, coef: coef}, nil
}
