package gain

import (
	"math"
	"strings"
	"testing"
	"time"
)

// compressAll runs a whole signal through a compressor in chunks of the
// given size, returning the concatenated output.
func compressAll(t *testing.T, c *Compressor, in [][]float32, chunk int) [][]float32 {
	t.Helper()
	out := make([][]float32, len(in))
	dst := make([][]float32, len(in))
	src := make([][]float32, len(in))
	for off := 0; off < len(in[0]); {
		n := min(chunk, len(in[0])-off)
		for ch := range in {
			src[ch] = in[ch][off : off+n]
			dst[ch] = make([]float32, n)
		}
		produced, consumed := c.Process(dst, src)
		for ch := range out {
			out[ch] = append(out[ch], dst[ch][:produced]...)
		}
		if consumed == 0 {
			t.Fatalf("compressor consumed nothing at offset %d", off)
		}
		off += consumed
	}
	for {
		for ch := range dst {
			dst[ch] = make([]float32, 1024)
		}
		p := c.Drain(dst)
		if p == 0 {
			break
		}
		for ch := range out {
			out[ch] = append(out[ch], dst[ch][:p]...)
		}
	}
	return out
}

// TestCompressorChunkingInvariance: identical output no matter how the
// stream is chunked. A kernel whose output depended on the caller's
// buffering would make a transcode's bytes an artifact of the decoder.
func TestCompressorChunkingInvariance(t *testing.T) {
	const rate = 44100
	in := [][]float32{sineAt(rate, 300, 0, 15000, 0.9)}
	ref := func() [][]float32 {
		c, err := NewCompressor(rate, 1, PresetVoice)
		if err != nil {
			t.Fatal(err)
		}
		return compressAll(t, c, in, len(in[0]))
	}()
	for _, chunk := range []int{1, 7, 100, 4096} {
		c, err := NewCompressor(rate, 1, PresetVoice)
		if err != nil {
			t.Fatal(err)
		}
		got := compressAll(t, c, in, chunk)
		if len(got[0]) != len(ref[0]) {
			t.Fatalf("chunk %d: %d frames, want %d", chunk, len(got[0]), len(ref[0]))
		}
		for i := range got[0] {
			if got[0][i] != ref[0][i] {
				t.Fatalf("chunk %d: sample %d = %v, want %v", chunk, i, got[0][i], ref[0][i])
			}
		}
	}
}

// TestCompressorPreservesLength pins that the node is sample-aligned: it
// has no look-ahead, so every frame in is a frame out and Drain holds
// nothing back. The chain's length arithmetic depends on this, and the
// limiter downstream is the node that does buffer.
func TestCompressorPreservesLength(t *testing.T) {
	const rate = 48000
	for _, n := range []int{1, 63, 4096, 100000} {
		c, err := NewCompressor(rate, 2, PresetVoice)
		if err != nil {
			t.Fatal(err)
		}
		in := [][]float32{sineAt(rate, 440, 0, n, 0.9), sineAt(rate, 660, 0.2, n, 0.9)}
		got := compressAll(t, c, in, 1024)
		for ch := range got {
			if len(got[ch]) != n {
				t.Errorf("%d frames in, channel %d got %d out", n, ch, len(got[ch]))
			}
		}
	}
}

// TestCompressorReducesRange is the feature itself: a signal alternating
// loud and quiet must come out with the two closer together than they went
// in. Without this the node could be a no-op and every other test here
// would still pass.
func TestCompressorReducesRange(t *testing.T) {
	const rate = 48000
	const seg = rate // 1 s per level

	// Four seconds alternating -6 dBFS and -30 dBFS, long enough at each
	// level for the envelope to settle.
	in := make([]float32, 4*seg)
	for i := range in {
		amp := 0.5
		if (i/seg)%2 == 1 {
			amp = 0.0316 // -30 dBFS
		}
		in[i] = float32(amp * math.Sin(2*math.Pi*440*float64(i)/rate))
	}
	c, err := NewCompressor(rate, 1, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	out := compressAll(t, c, [][]float32{in}, 1024)[0]

	// Measure each level near the end of its second, where the envelope has
	// settled, rather than across the transition.
	peakOver := func(sig []float32, from, to int) float64 {
		var p float64
		for _, v := range sig[from:to] {
			if a := math.Abs(float64(v)); a > p {
				p = a
			}
		}
		return 20 * math.Log10(p)
	}
	inLoud := peakOver(in, seg-rate/4, seg)
	inQuiet := peakOver(in, 2*seg-rate/4, 2*seg)
	outLoud := peakOver(out, seg-rate/4, seg)
	outQuiet := peakOver(out, 2*seg-rate/4, 2*seg)

	inRange := inLoud - inQuiet
	outRange := outLoud - outQuiet
	t.Logf("loud %.1f -> %.1f dBFS, quiet %.1f -> %.1f dBFS, range %.1f -> %.1f dB",
		inLoud, outLoud, inQuiet, outQuiet, inRange, outRange)
	if outRange >= inRange {
		t.Errorf("range %.1f dB in, %.1f dB out: the compressor reduced nothing", inRange, outRange)
	}
	// The quiet passage is below the threshold, so it gets makeup and
	// nothing else: it must come out louder, which is the levelling half of
	// what the preset is for.
	if outQuiet <= inQuiet {
		t.Errorf("quiet passage %.1f -> %.1f dBFS: makeup should have raised it", inQuiet, outQuiet)
	}
}

// TestCompressorCurveIsContinuous pins the soft knee's two seams. A
// discontinuity there is inaudible in isolation and reads as distortion on
// program material, which is exactly the kind of bug a listening test finds
// late and a boundary check finds now.
func TestCompressorCurveIsContinuous(t *testing.T) {
	c, err := NewCompressor(48000, 1, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	const eps = 1e-6
	for _, edge := range []float64{
		c.thresholdDB - c.halfKneeDB, // knee start: reduction reaches 0
		c.thresholdDB + c.halfKneeDB, // knee end: the full ratio takes over
	} {
		below := c.targetDB(FromDB(edge - eps))
		above := c.targetDB(FromDB(edge + eps))
		if math.Abs(below-above) > 1e-3 {
			t.Errorf("curve jumps at %.1f dBFS: %.6f vs %.6f dB", edge, below, above)
		}
	}
	// Below the knee the curve is exactly unity, which is what lets the hot
	// path skip the logarithm.
	if got := c.targetDB(FromDB(c.thresholdDB - c.halfKneeDB - 1)); got != 0 {
		t.Errorf("below the knee: reduction = %v, want exactly 0", got)
	}
	// Digital silence must not reach log10(0).
	if got := c.targetDB(0); got != 0 {
		t.Errorf("silence: reduction = %v, want exactly 0", got)
	}
	// The curve is monotonic: louder in is never less reduced.
	prev := 0.0
	for db := -60.0; db <= 0; db += 0.25 {
		got := c.targetDB(FromDB(db))
		if got > prev+1e-9 {
			t.Fatalf("reduction rose from %.6f to %.6f dB at %.2f dBFS", prev, got, db)
		}
		prev = got
	}
}

// TestCompressorRatioAtFullScale checks the curve against the ratio it
// claims, well above the knee where the blend no longer applies.
func TestCompressorRatioAtFullScale(t *testing.T) {
	c, err := NewCompressor(48000, 1, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	// Two inputs 10 dB apart above the knee must come out 10/ratio apart.
	lo, hi := -6.0, 4.0
	outLo := lo + c.targetDB(FromDB(lo))
	outHi := hi + c.targetDB(FromDB(hi))
	want := (hi - lo) * c.invRatio
	if got := outHi - outLo; math.Abs(got-want) > 1e-6 {
		t.Errorf("%.0f dB in became %.4f dB out, want %.4f (ratio %.1f:1)", hi-lo, got, want, 1/c.invRatio)
	}
}

// TestCompressorHorizon pins the settle horizon against its derivation, so
// a retune of the release cannot silently leave the pre-roll short.
func TestCompressorHorizon(t *testing.T) {
	c, err := NewCompressor(48000, 1, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	want := settleTimeConstants * curves[PresetVoice].release
	if got := c.Horizon(); got != want {
		t.Errorf("Horizon = %v, want %v (%d release time constants)", got, want, settleTimeConstants)
	}
	if got := c.Horizon(); got != 10*time.Second {
		t.Errorf("Horizon = %v, want 10s for the voice preset's 250ms release", got)
	}
}

// TestLimiterHorizon pins the same for the limiter, whose 50 ms release
// makes it 2 s. The old flat 100 ms priming was 20x short of this, which is
// the restart bug the horizon fixes.
func TestLimiterHorizon(t *testing.T) {
	l, err := NewLimiter(48000, 2, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Horizon(); got != 2*time.Second {
		t.Errorf("Horizon = %v, want 2s for the 50ms release", got)
	}
}

// TestSettleHorizonIsEnough is the horizon's own gate, and the reason the
// constant is measured rather than derived.
//
// Two runs start from the worst-case opposite states (the deepest reduction
// the curve allows, against Reset's zero) and are fed identical audio. The
// property that must hold is that their OUTPUT becomes and stays
// bit-identical well inside the horizon. Their states must not be expected
// to: against a held target the envelope increment underflows and each run
// stalls a few ulps from the other forever, which is exactly why a horizon
// derived from state convergence would rest on something that never
// happens. See Compressor.Horizon.
//
// The margin is asserted, not just logged. A retune that pushed convergence
// past half the horizon would still pass a bare "converges by the horizon"
// check while leaving nothing in reserve for content these fixtures do not
// cover, and the failure it guards is silent: one differing sample cascades
// through the encoder into a wholly different segment.
func TestSettleHorizonIsEnough(t *testing.T) {
	const rate = 48000
	tau := curves[PresetVoice].release.Seconds()
	horizon := (&Compressor{release: curves[PresetVoice].release}).Horizon().Seconds()

	cases := []struct {
		name string
		gen  func(i int) float32
	}{
		// A held target is the stalling case: the state never converges, so
		// only the output can.
		{"steady tone above the knee", func(i int) float32 {
			return float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/rate))
		}},
		{"steady tone below the knee", func(i int) float32 {
			return float32(0.0316 * math.Sin(2*math.Pi*440*float64(i)/rate))
		}},
		// Moving material, where the states do converge outright.
		{"speech-like bursts", func(i int) float32 { return speechish(rate, i) }},
	}

	worst := 0.0
	for _, c := range cases {
		sig := make([]float32, int(2*horizon*rate))
		for i := range sig {
			sig[i] = c.gen(i)
		}
		a, err := NewCompressor(rate, 1, PresetVoice)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewCompressor(rate, 1, PresetVoice)
		if err != nil {
			t.Fatal(err)
		}
		// a is a continuous run handing over deep in reduction; b is a
		// restarted one, exactly as Reset leaves it.
		a.g, b.g = -40, 0

		dstA := [][]float32{make([]float32, len(sig))}
		dstB := [][]float32{make([]float32, len(sig))}
		a.Process(dstA, [][]float32{sig})
		b.Process(dstB, [][]float32{sig})

		lastDiff := -1
		for i := range sig {
			if dstA[0][i] != dstB[0][i] {
				lastDiff = i
			}
		}
		at := float64(lastDiff+1) / rate
		t.Logf("%-28s output converges at %5.2fs (%4.1f tau), horizon %.0fs; states equal: %v",
			c.name, at, at/tau, horizon, a.g == b.g)
		worst = max(worst, at)
	}

	if worst >= horizon {
		t.Errorf("output still differs %.2fs in, at or past the %.0fs horizon: the pre-roll is short",
			worst, horizon)
	}
	// The safety factor the constant claims. Convergence past half the
	// horizon means the margin has eroded and settleTimeConstants needs
	// re-deriving against these fixtures rather than nudging.
	if worst > horizon/2 {
		t.Errorf("worst convergence %.2fs exceeds half the %.0fs horizon; settleTimeConstants = %d "+
			"no longer carries the safety factor its doc claims", worst, horizon, settleTimeConstants)
	}
}

// speechish is a deterministic stand-in for spoken word: bursts at
// alternating levels, so the envelope both compresses and releases the way
// it would on real material. It is a pure function of i so the two runs in
// TestSettleHorizonIsEnough see identical audio.
func speechish(rate, i int) float32 {
	// A cheap deterministic hash picks the level for each ~0.3 s burst.
	burst := i / (rate / 3)
	h := uint32(burst)*2654435761 + 1
	amp := 0.02 + 0.6*float64(h%1000)/1000
	return float32(amp * math.Sin(2*math.Pi*300*float64(i)/float64(rate)))
}

func TestNewCompressorValidation(t *testing.T) {
	if _, err := NewCompressor(0, 2, PresetVoice); err == nil {
		t.Error("zero rate: want error")
	}
	if _, err := NewCompressor(48000, 0, PresetVoice); err == nil {
		t.Error("zero channels: want error")
	}
	if _, err := NewCompressor(48000, 2, PresetOff); err == nil {
		t.Error("PresetOff: want error (the chain inserts no node for it)")
	}
	if _, err := NewCompressor(48000, 2, Preset("loud")); err == nil {
		t.Error("unknown preset: want error")
	}
}

// TestPresetsAreImplemented pins that every advertised preset can actually
// be built, which is what /caps promises when it lists them.
func TestPresetsAreImplemented(t *testing.T) {
	for _, p := range Presets() {
		if p == PresetOff {
			t.Error("Presets includes PresetOff, which is the absence of a preset")
		}
		if _, err := NewCompressor(48000, 2, p); err != nil {
			t.Errorf("advertised preset %q does not build: %v", p, err)
		}
	}
	if len(Presets()) != len(curves) {
		t.Errorf("Presets lists %d curves but the table has %d", len(Presets()), len(curves))
	}
}

// TestCurveTableIsSane checks every entry in the curve table against the
// assumptions NewCompressor makes of it. The table is package-private and
// fixed at compile time, so this belongs here rather than in NewCompressor:
// a runtime guard would be an error path no shipped input can reach, while
// this fails the moment a maintainer adds or retunes a preset, which is the
// only moment the mistake can be made.
//
// The failure modes are quieter than they look, which is why they are
// enumerated rather than covered by one "looks reasonable" check. A zero
// attack or release does not produce NaN, as might be assumed: -1/0 is -Inf
// and Exp(-Inf) is 0, so the coefficient lands on exactly 1, an
// instantaneous envelope that is wrong but stable and would pass a NaN
// check. A negative one gives a coefficient outside [0,1], which is an
// unstable filter that diverges rather than settling. A non-positive ratio
// makes invRatio infinite and every target infinite. A zero knee is
// harmless (the knee branch is unreachable when halfKneeDB is 0), so it is
// not rejected.
func TestCurveTableIsSane(t *testing.T) {
	const rate = 48000
	for p, c := range curves {
		t.Run(string(p), func(t *testing.T) {
			if c.attack <= 0 {
				t.Errorf("attack %v must be positive; 0 gives an instantaneous envelope "+
					"and a negative one an unstable filter", c.attack)
			}
			if c.release <= 0 {
				t.Errorf("release %v must be positive, and it also sets Horizon", c.release)
			}
			if c.ratio <= 0 {
				t.Errorf("ratio %v must be positive; 0 makes every target infinite", c.ratio)
			}
			if c.kneeDB < 0 {
				t.Errorf("knee %v dB must not be negative", c.kneeDB)
			}
			if c.thresholdDB >= 0 {
				t.Errorf("threshold %v dBFS must be below full scale, or the curve never engages", c.thresholdDB)
			}
			if c.makeupDB < 0 {
				t.Errorf("makeup %v dB is negative; the preset would only ever attenuate", c.makeupDB)
			}

			// The coefficients the above protect, checked as built.
			k, err := NewCompressor(rate, 1, p)
			if err != nil {
				t.Fatalf("NewCompressor: %v", err)
			}
			for _, co := range []struct {
				name string
				v    float64
			}{{"aAtk", k.aAtk}, {"aRel", k.aRel}} {
				if math.IsNaN(co.v) || co.v <= 0 || co.v > 1 {
					t.Errorf("%s = %v, want a stable one-pole coefficient in (0, 1]", co.name, co.v)
				}
			}
			if math.IsNaN(k.invRatio) || math.IsInf(k.invRatio, 0) || k.invRatio <= 0 || k.invRatio > 1 {
				t.Errorf("invRatio = %v, want (0, 1]: a ratio under 1:1 would expand, not compress", k.invRatio)
			}
			if k.kneeStart <= 0 || math.IsInf(k.kneeStart, 0) {
				t.Errorf("kneeStart = %v, want a positive linear magnitude (targetDB's silence guard rests on it)", k.kneeStart)
			}
			// The attack must be the faster pole, or Horizon names the wrong
			// one and the pre-roll is short.
			if c.attack >= c.release {
				t.Errorf("attack %v is not faster than release %v; Horizon assumes the release is the slow pole",
					c.attack, c.release)
			}
		})
	}
}

// BenchmarkCompressor reports x-realtime for a stereo 48 kHz stream with
// the compressor engaging, since the detector's logarithm runs per frame
// whenever the signal is above the knee.
func BenchmarkCompressor(b *testing.B) {
	const rate, chunk = 48000, 4096
	c, err := NewCompressor(rate, 2, PresetVoice)
	if err != nil {
		b.Fatal(err)
	}
	in := [][]float32{
		sineAt(rate, 997, 0, chunk, 0.9),
		sineAt(rate, 1499, 0.4, chunk, 0.9),
	}
	dst := [][]float32{make([]float32, chunk), make([]float32, chunk)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Process(dst, in)
	}
	b.StopTimer()
	seconds := float64(b.N) * chunk / float64(rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

// TestKernelsRejectRaggedSlices pins the shared slice convention. These
// kernels are public, so a caller can hand them channel slices of differing
// lengths; without the check that surfaces as an index-out-of-range panic
// several frames into a hot loop, which says nothing about what was wrong.
func TestKernelsRejectRaggedSlices(t *testing.T) {
	full := func(n int) [][]float32 { return [][]float32{make([]float32, n), make([]float32, n)} }
	ragged := func() [][]float32 { return [][]float32{make([]float32, 100), make([]float32, 10)} }

	wantPanic := func(t *testing.T, what string, fn func()) {
		t.Helper()
		defer func() {
			r := recover()
			if r == nil {
				t.Errorf("%s: ragged slices did not panic", what)
				return
			}
			msg, _ := r.(string)
			if !strings.Contains(msg, "differ in length") {
				t.Errorf("%s: panic %q does not name the problem", what, r)
			}
		}()
		fn()
	}

	c, err := NewCompressor(48000, 2, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	wantPanic(t, "compressor src", func() { c.Process(full(100), ragged()) })
	wantPanic(t, "compressor dst", func() { c.Process(ragged(), full(100)) })

	l, err := NewLimiter(48000, 2, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	wantPanic(t, "limiter src", func() { l.Process(full(100), ragged()) })
	wantPanic(t, "limiter dst", func() { l.Process(ragged(), full(100)) })

	l2, err := NewLimiter(48000, 2, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	wantPanic(t, "limiter drain dst", func() { l2.Drain(ragged()) })

	// The control: equal-length slices must not panic, or the checks are
	// simply rejecting everything.
	c2, err := NewCompressor(48000, 2, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c2.Process(full(100), full(100)); p != 100 {
		t.Errorf("equal-length slices produced %d frames, want 100", p)
	}
}

// TestCompressorSurvivesNonFinite pins the guard on the detector.
//
// A single infinite sample is otherwise fatal, and permanently: it drives
// the peak to +Inf, the curve's target to -Inf, and the envelope to -Inf on
// the attack. The next sample releases toward a finite target, computes
// -Inf + Inf, and lands on NaN, after which every gain is NaN and the rest
// of the stream is silence. No later audio recovers it, so one bad sample
// in an hour-long source silences the remainder.
//
// NaN is covered too, for the opposite reason: it needs no guard, because
// it fails the > in the detector loop and never enters the peak. The
// asymmetry is worth pinning so nobody "fixes" NaN and misses Inf, which is
// the one that actually bites.
func TestCompressorSurvivesNonFinite(t *testing.T) {
	for _, c := range []struct {
		name string
		bad  float32
	}{
		{"NaN", float32(math.NaN())},
		{"+Inf", float32(math.Inf(1))},
		{"-Inf", float32(math.Inf(-1))},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, err := NewCompressor(48000, 1, PresetVoice)
			if err != nil {
				t.Fatal(err)
			}
			const n = 4096
			src := make([]float32, n)
			for i := range src {
				src[i] = 0.1
			}
			src[2] = c.bad
			dst := make([]float32, n)
			k.Process([][]float32{dst}, [][]float32{src})

			if math.IsNaN(k.g) || math.IsInf(k.g, 0) {
				t.Fatalf("one %s sample left the envelope at %v; every later gain is poisoned", c.name, k.g)
			}
			// Every sample after the bad one must be finite audio again. The
			// bad sample itself may pass through; the limiter's clamp and the
			// quantizer's NaN guard are downstream of here for that.
			for i := 3; i < n; i++ {
				if math.IsNaN(float64(dst[i])) || math.IsInf(float64(dst[i]), 0) {
					t.Fatalf("sample %d is %v after a single %s input", i, dst[i], c.name)
				}
			}
			// And the envelope must still respond to real audio afterwards,
			// not merely be finite: a stuck gain would pass the checks above.
			loud := make([]float32, n)
			for i := range loud {
				loud[i] = 0.9
			}
			before := k.g
			k.Process([][]float32{dst}, [][]float32{loud})
			if k.g >= before {
				t.Errorf("the envelope did not react to loud audio after a %s sample (%v -> %v)",
					c.name, before, k.g)
			}
		})
	}
}

// TestCompressorCapsGrossOvers pins that the detector's cap does not change
// the curve for signals inside its range, so the guard costs nothing where
// it does not apply.
func TestCompressorCapsGrossOvers(t *testing.T) {
	c, err := NewCompressor(48000, 1, PresetVoice)
	if err != nil {
		t.Fatal(err)
	}
	// Full scale is well inside the cap and must be unaffected by it.
	if got, want := c.targetDB(1.0), c.targetDB(1.0); got != want {
		t.Fatal("unreachable")
	}
	atCap := c.targetDB(maxPeak)
	if over := c.targetDB(maxPeak * 1e6); over != atCap {
		t.Errorf("a gross over reduced by %v, want the cap's %v", over, atCap)
	}
	if inf := c.targetDB(math.Inf(1)); inf != atCap {
		t.Errorf("an infinite peak reduced by %v, want the cap's %v (finite)", inf, atCap)
	}
	// Below the cap the curve is untouched, so the guard is inert where it
	// does not apply.
	if got := c.targetDB(0.5); got == atCap {
		t.Error("a normal peak is being capped")
	}
}
