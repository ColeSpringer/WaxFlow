package gain

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/loudness"
)

// The ceiling gate for the true-peak limiter, in two assertions: kernelTruePeak
// re-runs the limiter's own 4x interpolator over its output (the quantity the
// construction bounds, tolerance ceilEpsilon), and dsp/loudness measures it with
// an independently designed 4x detector at a deliberately loose 0.20 dB. Both
// are 4x, so both under-read against an 8x detector. See docs/quality-gates.md.

// ceilEpsilon is how far over the ceiling a correct limiter measures at 4x. The
// min-hold bound is exact in the sample domain; an interpolated point
// reconstructs the gain-modulated signal, so what leaks is the gain's curvature
// across the 16 taps, which falls as 1/look^2. Constant fitted to measurement
// with 3x margin (table in docs/quality-gates.md); the floor is float32 noise.
func ceilEpsilon(look int) float64 {
	return math.Max(8/(float64(look)*float64(look)), 1e-5)
}

// ceilingDiagnosis names which failure produced an over-ceiling reading: they
// look identical in the true-peak number alone.
//
// It reads the clamp counter rather than looking for samples sitting at the
// ceiling. A correct min-hold puts them there routinely (g == ceil/peaks
// exactly, at every sample of a sustained over-ceiling tone) without the clamp
// ever firing, so the value alone names the wrong cause. Float32 rounding on
// the product trips the clamp a handful of times per stream even when the
// construction is sound; a broken one trips it in proportion to how long the
// gain stayed too high.
func ceilingDiagnosis(l *Limiter, out [][]float32) string {
	var sp float64
	for _, ch := range out {
		for _, v := range ch {
			if a := math.Abs(float64(v)); a > sp {
				sp = a
			}
		}
	}
	if l.clamped > clampRoundingBudget {
		return fmt.Sprintf("the clamp modified %d samples (sample peak %.9f): the gain exceeded "+
			"ceil/peaks for a stretch, so this is a broken min-hold, not a narrow envelope. "+
			"Suspect the construction or its priming", l.clamped, sp)
	}
	return fmt.Sprintf("the clamp modified %d samples, within float32 rounding, so the min-hold "+
		"held: the excess is gain curvature across the interpolator span. Re-derive ceilEpsilon "+
		"against its measured table rather than widening it", l.clamped)
}

// clampRoundingBudget is how many clamped samples float32 rounding on x*g alone
// can produce: measured worst case 6 over 600 random runs, against 0 on every
// fixture here and 62 for a min-hold broken only at the stream head.
const clampRoundingBudget = 16

// kernelTruePeak returns the largest 4x-interpolated magnitude in out, using the
// limiter's own interpolator. Off-end samples read as silence, matching detect.
func kernelTruePeak(t *testing.T, rate int, out [][]float32) float64 {
	t.Helper()
	l, err := NewLimiter(rate, len(out), DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	const half = interpTaps / 2
	var peak float64
	for _, ch := range out {
		for j := range ch {
			if v := math.Abs(float64(ch[j])); v > peak {
				peak = v
			}
			for p := range l.interp {
				phase := &l.interp[p]
				var acc float64
				for tap := 0; tap < interpTaps; tap++ {
					if i := j - half + 1 + tap; i >= 0 && i < len(ch) {
						acc += float64(phase[tap]) * float64(ch[i])
					}
				}
				if a := math.Abs(acc); a > peak {
					peak = a
				}
			}
		}
	}
	return peak
}

// crestSignal builds a high-crest fixture: sparse transients over a quiet bed.
// The transients are short relative to the look-ahead on purpose. A long decay
// lets the sliding max fall before the peak's own window opens, handing the old
// attack pole a head start and hiding most of the defect (0.20 dB over the
// ceiling against 0.80 dB for these clicks).
func crestSignal(rate, frames int) []float32 {
	s := make([]float32, frames)
	for i := range s {
		s[i] = float32(0.062 * math.Sin(2*math.Pi*220*float64(i)/float64(rate)))
	}
	for at := rate / 8; at < frames; at += rate / 2 {
		for k := 0; k < 4 && at+k < frames; k++ {
			s[at+k] += float32(math.Cos(math.Pi * float64(k) / 4))
		}
	}
	return s
}

// crestDB is the crest factor of s in dB, so a fixture can state what it is
// rather than assert a number nobody can check.
func crestDB(s []float32) float64 {
	var peak, sq float64
	for _, v := range s {
		a := math.Abs(float64(v))
		if a > peak {
			peak = a
		}
		sq += a * a
	}
	if sq == 0 || peak == 0 {
		return 0
	}
	return 20 * math.Log10(peak/math.Sqrt(sq/float64(len(s))))
}

// TestLimiterCeilingUnderGain is the F2 regression: a high-crest signal at every
// gain the maxGainDB = 12 policy permits and well past it must come out at or
// under the ceiling. The old attack reached 0.80 dB over, with the clamp pinning
// the sample peak at -1.000 dBFS so nothing downstream could see it.
func TestLimiterCeilingUnderGain(t *testing.T) {
	const rate = 48000
	ceil := FromDB(DefaultCeilingDB)
	base := crestSignal(rate, 3*rate)
	t.Logf("fixture crest factor %.1f dB", crestDB(base))

	for _, gainDB := range []float64{3, 7, 11, 12, 15, 19, 22, 25} {
		g := FromDB(gainDB)
		in := make([]float32, len(base))
		for i, v := range base {
			in[i] = float32(float64(v) * g)
		}
		l, err := NewLimiter(rate, 1, DefaultCeilingDB)
		if err != nil {
			t.Fatal(err)
		}
		out := limitAll(t, l, [][]float32{in}, 4096)
		tp := kernelTruePeak(t, rate, out)
		t.Logf("gain %+.0f dB: true peak %+.4f dBTP rel-excess %.3e", gainDB, 20*math.Log10(tp), tp/ceil-1)
		if tp > ceil*(1+ceilEpsilon(l.look)) {
			t.Errorf("gain %+.0f dB: true peak %+.4f dBTP exceeds ceiling %+.2f dBTP; %s",
				gainDB, 20*math.Log10(tp), DefaultCeilingDB, ceilingDiagnosis(l, out))
		}
		// The clamp is a backstop, not the mechanism: it must never fire.
		if l.clamped != 0 {
			t.Errorf("gain %+.0f dB: clamp modified %d samples, want 0 (it is inert by construction)",
				gainDB, l.clamped)
		}
	}
}

// TestLimiterCeilingDominatedPeak covers the shape a head-steered ramp fails: an
// over-ceiling peak a few samples ahead of a far larger one in the same window.
// The deque drops the smaller one entirely, so a head-steered design never sees
// it; the min-hold bounds it anyway.
func TestLimiterCeilingDominatedPeak(t *testing.T) {
	const rate = 48000
	ceil := FromDB(DefaultCeilingDB)
	l, err := NewLimiter(rate, 1, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]float32, rate)
	// look is rate/200 = 240 frames here; both peaks sit well inside one window.
	const at = rate / 2
	in[at] = 1.2     // over the ceiling, but dominated
	in[at+40] = 6.0  // dominates the window
	in[at+41] = -6.0 // and again, so the interpolator has something to ring on
	out := limitAll(t, l, [][]float32{in}, 4096)
	tp := kernelTruePeak(t, rate, out)
	t.Logf("dominated-peak true peak %+.4f dBTP rel-excess %.3e", 20*math.Log10(tp), tp/ceil-1)
	if tp > ceil*(1+ceilEpsilon(l.look)) {
		t.Errorf("true peak %+.4f dBTP exceeds ceiling %+.2f dBTP; %s",
			20*math.Log10(tp), DefaultCeilingDB, ceilingDiagnosis(l, out))
	}
	if l.clamped != 0 {
		t.Errorf("clamp modified %d samples, want 0 (it is inert by construction)", l.clamped)
	}
	// The dominated peak must still be audible, not notched to nothing: a
	// per-sample min(g, ceil/peak) floor would have gouged a one-sample hole
	// here, which is the worst possible local gain slope.
	if v := math.Abs(float64(out[0][at])); v < 0.01 {
		t.Errorf("dominated peak attenuated to %g: the gain has a notch at it", v)
	}
}

// TestLimiterCeilingExternalDetector cross-checks against dsp/loudness so the
// gate is not one kernel grading its own homework. The bound is loose because
// the two 4x designs differ (16 taps at beta 3.67 against 12 at beta 6) and
// disagree by ~0.04 dB on transients; the defect it catches was 1.80 dB.
func TestLimiterCeilingExternalDetector(t *testing.T) {
	const rate = 48000
	// Set from measurement, not assumed. The observed spread between the two
	// detectors on these fixtures is 0.036 dB; 0.20 dB is the plan's
	// budget and leaves an order of margin.
	const allow = 0.20
	base := crestSignal(rate, 3*rate)
	for _, gainDB := range []float64{3, 12, 22} {
		g := FromDB(gainDB)
		in := make([]float32, len(base))
		for i, v := range base {
			in[i] = float32(float64(v) * g)
		}
		l, err := NewLimiter(rate, 1, DefaultCeilingDB)
		if err != nil {
			t.Fatal(err)
		}
		out := limitAll(t, l, [][]float32{in}, 4096)

		m, err := loudness.NewMeter(rate, 1, audio.DefaultLayout(1))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Process(out); err != nil {
			t.Fatal(err)
		}
		m.Flush()
		tp := m.TruePeak()
		t.Logf("gain %+.0f dB: loudness meter reads %+.4f dBTP (limiter kernel %+.4f dBTP)",
			gainDB, tp, 20*math.Log10(kernelTruePeak(t, rate, out)))
		if tp > DefaultCeilingDB+allow {
			t.Errorf("gain %+.0f dB: independent detector reads %+.4f dBTP, over ceiling %+.2f + %.2f",
				gainDB, tp, DefaultCeilingDB, allow)
		}
	}
}

// FuzzLimiterCeiling backs the word "any" in the gate: over random gain, rate,
// crest and chunking, the output true peak stays at or under the ceiling.
func FuzzLimiterCeiling(f *testing.F) {
	f.Add(uint64(1), 12.0, 0, 4096)
	f.Add(uint64(7), 24.0, 2, 1)
	f.Add(uint64(99), 3.5, 1, 577)
	f.Fuzz(func(t *testing.T, seed uint64, gainDB float64, rateIdx, chunk int) {
		if !(gainDB >= 0 && gainDB <= 40) {
			t.Skip()
		}
		rates := []int{8000, 22050, 44100, 48000, 96000}
		rate := rates[((rateIdx%len(rates))+len(rates))%len(rates)]
		// Clamped, not wrapped: wrapping maps the seed corpus's 4096 (the
		// whole-buffer, one-Process-call path) onto 1, so the committed seeds
		// that run in every `go test` would never exercise a large chunk.
		chunk = min(max(chunk, 1), 4096)
		rng := rand.New(rand.NewSource(int64(seed)))

		// A random signal with a random crest: a bed plus sparse spikes.
		n := rate / 4
		bed := math.Pow(10, -3*rng.Float64()) // 0.001 .. 1 RMS-ish
		in := make([]float32, n)
		for i := range in {
			in[i] = float32(bed * (2*rng.Float64() - 1))
		}
		for k := 0; k < 1+rng.Intn(20); k++ {
			in[rng.Intn(n)] += float32(2*rng.Float64() - 1)
		}
		g := FromDB(gainDB)
		for i := range in {
			in[i] = float32(float64(in[i]) * g)
		}

		l, err := NewLimiter(rate, 1, DefaultCeilingDB)
		if err != nil {
			t.Fatal(err)
		}
		out := limitAll(t, l, [][]float32{in}, chunk)
		if len(out[0]) != n {
			t.Fatalf("produced %d frames, want %d", len(out[0]), n)
		}
		ceil := FromDB(DefaultCeilingDB)
		if tp := kernelTruePeak(t, rate, out); tp > ceil*(1+ceilEpsilon(l.look)) {
			t.Errorf("rate=%d gain=%.2f chunk=%d: true peak %+.4f dBTP exceeds ceiling %+.2f dBTP by %.3e relative, "+
				"over the %.3e look=%d allows; %s",
				rate, gainDB, chunk, 20*math.Log10(tp), DefaultCeilingDB, tp/ceil-1, ceilEpsilon(l.look), l.look,
				ceilingDiagnosis(l, out))
		}
	})
}

// TestLimiterSettlerRejoin is the limiter's dsp.Settler contract, which nothing
// covered before (TestSettleHorizonIsEnough is compressor-only). The ring
// contents converge on their own, but a float64 window sum depends on how many
// samples preceded it, so the two runs settled a few ulps apart and stayed
// there: one differing sample in 48000 after 400 s, and one differing sample is
// a wholly different encoded segment. The handover is deep into a long stream
// because the defect grows with the continuous run's length.
func TestLimiterSettlerRejoin(t *testing.T) {
	const rate = 48000
	l, err := NewLimiter(rate, 1, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	horizon := int(l.Horizon().Seconds() * rate)
	const handover = 400 * rate // where the restarted run takes over
	const compare = 10 * rate   // how much output to compare past it
	// Continuously over the ceiling, so the smoother never idles back to unity
	// (which would resynchronize the sums for free and hide the defect).
	gen := func(k int) float32 {
		return float32(1.8*math.Sin(2*math.Pi*220*float64(k)/rate) +
			0.6*math.Sin(2*math.Pi*3011*float64(k)/rate))
	}
	cont := limitRange(t, rate, gen, 0, handover+compare, handover)
	rest := limitRange(t, rate, gen, handover-horizon, handover+compare, handover)

	n := min(len(cont), len(rest))
	if n < compare/2 {
		t.Fatalf("compared only %d frames, want about %d", n, compare)
	}
	diffs, first := 0, -1
	var maxDiff float64
	for i := 0; i < n; i++ {
		if cont[i] != rest[i] {
			diffs++
			if first < 0 {
				first = i
			}
			maxDiff = math.Max(maxDiff, math.Abs(float64(cont[i]-rest[i])))
		}
	}
	if diffs != 0 {
		t.Errorf("restarted run does not rejoin: %d of %d samples differ (first at %d, max %.3e). "+
			"The pre-roll is %d s and the ring contents converge inside a look-ahead window, so a "+
			"residual difference here is state that depends on history rather than on the window.",
			diffs, n, first, maxDiff, horizon/rate)
	}
}

// limitRange runs gen over [from, end) through a fresh limiter, returning output
// at or past keepFrom so two runs can be compared over the same audio.
func limitRange(t *testing.T, rate int, gen func(int) float32, from, end, keepFrom int) []float32 {
	t.Helper()
	l, err := NewLimiter(rate, 1, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	const chunk = 4096
	dst := [][]float32{make([]float32, chunk)}
	in := make([]float32, chunk)
	var out []float32
	pos := from // absolute index of the next frame the limiter will emit
	keep := func(n int) {
		for i := 0; i < n; i++ {
			if pos+i >= keepFrom {
				out = append(out, dst[0][i])
			}
		}
		pos += n
	}
	for at := from; at < end; at += chunk {
		nn := min(chunk, end-at)
		for i := 0; i < nn; i++ {
			in[i] = gen(at + i)
		}
		src := [][]float32{in[:nn]}
		for len(src[0]) > 0 {
			produced, consumed := l.Process(dst, src)
			keep(produced)
			src[0] = src[0][consumed:]
		}
	}
	for {
		produced := l.Drain(dst)
		if produced == 0 {
			break
		}
		keep(produced)
	}
	return out
}
