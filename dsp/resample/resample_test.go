package resample

import (
	"math"
	"math/rand/v2"
	"sync"
	"testing"
)

// resampleAll pushes the whole input through r in one pass and drains,
// returning the complete output per channel.
func resampleAll(t *testing.T, r *Resampler, in [][]float32, chunk int) [][]float32 {
	t.Helper()
	channels := len(in)
	l, m := r.Ratio()
	outCap := int(OutputLen(int64(len(in[0])), m, l)) + 8
	out := make([][]float32, channels)
	dst := make([][]float32, channels)
	for c := range out {
		out[c] = make([]float32, 0, outCap)
		dst[c] = make([]float32, chunk)
	}
	pos := 0
	for pos < len(in[0]) {
		src := make([][]float32, channels)
		end := min(pos+chunk, len(in[0]))
		for c := range src {
			src[c] = in[c][pos:end]
		}
		for {
			produced, consumed := r.Process(dst, src)
			for c := range out {
				out[c] = append(out[c], dst[c][:produced]...)
			}
			for c := range src {
				src[c] = src[c][consumed:]
			}
			if len(src[0]) == 0 {
				break
			}
		}
		pos = end
	}
	for {
		produced := r.Drain(dst)
		if produced == 0 {
			break
		}
		for c := range out {
			out[c] = append(out[c], dst[c][:produced]...)
		}
	}
	return out
}

func sineAt(rate int, freq float64, frames int, amp float64) []float32 {
	s := make([]float32, frames)
	for i := range s {
		s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return s
}

// toneAmp estimates the amplitude of the freq component by Hann-windowed
// quadrature correlation. The window matters: measuring a -110 dB alias
// next to a full-scale fundamental needs sidelobe rolloff a rectangular
// window does not have (its 1/(N*df) leakage floors out near -75 dB at
// these lengths).
func toneAmp(x []float32, rate int, freq float64) float64 {
	var a, b, wsum float64
	n := float64(len(x))
	for i, v := range x {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/n)
		ph := 2 * math.Pi * freq * float64(i) / float64(rate)
		a += float64(v) * w * math.Cos(ph)
		b += float64(v) * w * math.Sin(ph)
		wsum += w
	}
	return 2 * math.Hypot(a, b) / wsum
}

func dB(x float64) float64 { return 20 * math.Log10(x) }

// steady trims the filter transient from both ends of an output stream,
// leaving the steady-state middle for measurements.
func steady(t *testing.T, r *Resampler, out []float32, outRate int) []float32 {
	t.Helper()
	num, den := r.GroupDelay()
	l, m := r.Ratio()
	// Group delay in output samples, doubled for margin.
	trim := 2 * int(int64(num)*int64(l)/(int64(den)*int64(m))+1)
	if len(out) < 3*trim {
		t.Fatalf("output too short for steady-state analysis: %d frames, trim %d", len(out), trim)
	}
	return out[trim : len(out)-trim]
}

// fold maps a frequency present at the input rate onto where it lands in
// the output band after decimation aliasing.
func fold(freq float64, outRate int) float64 {
	r := math.Mod(freq, float64(outRate))
	if r > float64(outRate)/2 {
		r = float64(outRate) - r
	}
	return r
}

type rateCase struct {
	in, out int
}

var rateCases = []rateCase{
	{96000, 44100},
	{48000, 44100},
	{44100, 48000},
	{44100, 96000},
	{192000, 48000},
}

type profileGate struct {
	profile   Profile
	passEdge  float64 // fraction of the narrower Nyquist
	rippleDB  float64
	rejectDB  float64
	stopStart float64 // fraction of the narrower Nyquist where rejection is owed
}

// The gates under test are the documented profile guarantees:
// hq passband 0.91x Nyquist at <=0.05 dB ripple with >=110 dB rejection,
// fast at 0.85x / ~70 dB.
var profileGates = []profileGate{
	{HQ, 0.91, 0.05, 110, 1.09},
	{Fast, 0.85, 0.10, 70, 1.15},
}

// TestPassbandRipple sweeps tones across the passband and asserts the
// level is preserved within the profile's ripple bound.
func TestPassbandRipple(t *testing.T) {
	for _, g := range profileGates {
		for _, rc := range rateCases {
			nyq := float64(min(rc.in, rc.out)) / 2
			edge := g.passEdge * nyq
			for _, frac := range []float64{0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 1.0} {
				freq := frac * edge
				r, err := New(rc.in, rc.out, 1, g.profile)
				if err != nil {
					t.Fatal(err)
				}
				in := [][]float32{sineAt(rc.in, freq, rc.in/2, 0.5)}
				out := resampleAll(t, r, in, 4096)
				got := toneAmp(steady(t, r, out[0], rc.out), rc.out, freq)
				if ripple := math.Abs(dB(got / 0.5)); ripple > g.rippleDB {
					t.Errorf("%s %d->%d: tone %.0f Hz level error %.4f dB, want <= %.2f",
						g.profile, rc.in, rc.out, freq, ripple, g.rippleDB)
				}
			}
		}
	}
}

// TestAliasRejection feeds tones from the stopband and asserts their
// folded images in the output passband sit below the rejection gate.
// Frequencies between the narrower Nyquist and the stopband edge are
// exempt by design: they fold into the transition band, never the
// passband.
func TestAliasRejection(t *testing.T) {
	for _, g := range profileGates {
		for _, rc := range rateCases {
			if rc.out >= rc.in {
				continue // imaging covered separately
			}
			nyq := float64(rc.out) / 2
			lo := g.stopStart * nyq
			hi := float64(rc.in) / 2
			if lo >= hi {
				// Near-unity ratios (48k -> 44.1k): every input frequency
				// folds into the passband-plus-transition region, so no
				// rejection is owed anywhere. By design, nothing to test.
				continue
			}
			for _, frac := range []float64{0.0, 0.2, 0.5, 0.8, 1.0} {
				freq := lo + frac*(hi-lo)
				if freq >= hi {
					freq = hi - 100 // a tone at exactly Nyquist is degenerate
				}
				target := fold(freq, rc.out)
				if target < 20 || target > g.passEdge*nyq {
					continue // folds outside the guaranteed passband
				}
				r, err := New(rc.in, rc.out, 1, g.profile)
				if err != nil {
					t.Fatal(err)
				}
				in := [][]float32{sineAt(rc.in, freq, rc.in/2, 1.0)}
				out := resampleAll(t, r, in, 4096)
				alias := toneAmp(steady(t, r, out[0], rc.out), rc.out, target)
				if rej := -dB(alias); rej < g.rejectDB {
					t.Errorf("%s %d->%d: tone %.0f Hz folds to %.0f Hz at %.1f dB rejection, want >= %.0f",
						g.profile, rc.in, rc.out, freq, target, rej, g.rejectDB)
				}
			}
		}
	}
}

// TestImageRejection covers the upsampling direction: spectral images of
// a passband tone must be suppressed to the profile gate wherever they
// land inside the output band (post-decimation folding included).
func TestImageRejection(t *testing.T) {
	for _, g := range profileGates {
		for _, rc := range rateCases {
			if rc.out <= rc.in {
				continue
			}
			nyqIn := float64(rc.in) / 2
			for _, frac := range []float64{0.2, 0.5, 0.8} {
				freq := frac * g.passEdge * nyqIn
				// First image around the input rate; where it lands in the
				// output band after decimation.
				image := fold(float64(rc.in)-freq, rc.out)
				if image > float64(rc.out)/2-20 || math.Abs(image-freq) < 50 {
					continue
				}
				r, err := New(rc.in, rc.out, 1, g.profile)
				if err != nil {
					t.Fatal(err)
				}
				in := [][]float32{sineAt(rc.in, freq, rc.in/2, 1.0)}
				out := resampleAll(t, r, in, 4096)
				img := toneAmp(steady(t, r, out[0], rc.out), rc.out, image)
				if rej := -dB(img); rej < g.rejectDB {
					t.Errorf("%s %d->%d: tone %.0f Hz images at %.0f Hz with %.1f dB rejection, want >= %.0f",
						g.profile, rc.in, rc.out, freq, image, rej, g.rejectDB)
				}
			}
		}
	}
}

// TestImpulseLatency is the latency-compensation exit criterion: an
// impulse at input sample k must peak at output sample k*L/M exactly
// (k chosen so that lands on the output grid).
func TestImpulseLatency(t *testing.T) {
	for _, p := range []Profile{HQ, Fast} {
		for _, rc := range rateCases {
			r, err := New(rc.in, rc.out, 1, p)
			if err != nil {
				t.Fatal(err)
			}
			l, m := r.Ratio()
			k := 10 * m // k*L/M = 10*L, exactly on the output grid
			in := make([]float32, k+10*m+1)
			in[k] = 1
			out := resampleAll(t, r, [][]float32{in}, 4096)

			want := 10 * l
			argmax, peak := 0, float64(0)
			for i, v := range out[0] {
				if a := math.Abs(float64(v)); a > peak {
					argmax, peak = i, a
				}
			}
			if argmax != want {
				t.Errorf("%s %d->%d: impulse at %d peaks at output %d, want %d",
					p, rc.in, rc.out, k, argmax, want)
			}
			// On-grid impulse peak: unity when upsampling, L/M when
			// downsampling (bandlimiting spreads the energy).
			wantPeak := math.Min(1, float64(l)/float64(m))
			if math.Abs(peak-wantPeak) > 0.01*wantPeak {
				t.Errorf("%s %d->%d: impulse peak %.5f, want %.5f",
					p, rc.in, rc.out, peak, wantPeak)
			}
		}
	}
}

// TestOutputLenAndDrain pins the sample-count invariant: a T-frame input
// yields exactly ceil(T*L/M) output frames, for lengths that do not
// divide evenly.
func TestOutputLenAndDrain(t *testing.T) {
	for _, rc := range rateCases {
		for _, frames := range []int{1, 7, 320, 4095, 4096, 44100, 44101} {
			r, err := New(rc.in, rc.out, 2, HQ)
			if err != nil {
				t.Fatal(err)
			}
			in := [][]float32{
				sineAt(rc.in, 997, frames, 0.5),
				sineAt(rc.in, 1499, frames, 0.5),
			}
			out := resampleAll(t, r, in, 4096)
			want := OutputLen(int64(frames), rc.in, rc.out)
			if int64(len(out[0])) != want || int64(len(out[1])) != want {
				t.Errorf("%d->%d %d frames: got %d/%d out, want %d",
					rc.in, rc.out, frames, len(out[0]), len(out[1]), want)
			}
		}
	}
}

// TestChunkingInvariance asserts the stream is bit-identical no matter
// how the input is chunked, including single-sample feeds: the polyphase
// arithmetic must not depend on buffer boundaries.
func TestChunkingInvariance(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	frames := 9973
	in := make([]float32, frames)
	for i := range in {
		in[i] = float32(rng.Float64()*2 - 1)
	}

	ref := func() [][]float32 {
		r, err := New(44100, 48000, 1, HQ)
		if err != nil {
			t.Fatal(err)
		}
		return resampleAll(t, r, [][]float32{in}, frames)
	}()

	for _, chunk := range []int{1, 3, 17, 100, 4096} {
		r, err := New(44100, 48000, 1, HQ)
		if err != nil {
			t.Fatal(err)
		}
		got := resampleAll(t, r, [][]float32{in}, chunk)
		if len(got[0]) != len(ref[0]) {
			t.Fatalf("chunk %d: length %d, want %d", chunk, len(got[0]), len(ref[0]))
		}
		for i := range got[0] {
			if got[0][i] != ref[0][i] {
				t.Fatalf("chunk %d: sample %d differs: %g vs %g", chunk, i, got[0][i], ref[0][i])
			}
		}
	}
}

// TestOffsetFor checks the mid-stream anchor math: a segment resampled
// from input position pos must produce the same samples the full stream
// produces from output position ceil(pos*L/M), once both are past the
// history the segment cannot see.
func TestOffsetFor(t *testing.T) {
	const inRate, outRate = 48000, 44100
	freq := 1000.0
	full := func() [][]float32 {
		r, err := New(inRate, outRate, 1, HQ)
		if err != nil {
			t.Fatal(err)
		}
		return resampleAll(t, r, [][]float32{sineAt(inRate, freq, 48000, 0.5)}, 4096)
	}()

	r, err := New(inRate, outRate, 1, HQ)
	if err != nil {
		t.Fatal(err)
	}
	var pos int64 = 12347
	outPos, off := r.OffsetFor(pos)
	r.Reset(off)

	// The segment's input is the same sine, starting at pos.
	seg := make([]float32, 24000)
	for i := range seg {
		seg[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(pos+int64(i))/float64(inRate)))
	}
	out := resampleAll(t, r, [][]float32{seg}, 4096)

	// Skip the segment's warmup (its history is silence, the full
	// stream's is signal), then require close agreement. Bit-equality is
	// not expected: the anchored phase evaluates different polyphase rows.
	num, den := r.GroupDelay()
	warm := 4 * int(int64(num)/int64(den)+1)
	for i := warm; i < len(out[0])-warm; i++ {
		fullIdx := int(outPos) + i
		if fullIdx >= len(full[0]) {
			break
		}
		if diff := math.Abs(float64(out[0][i] - full[0][fullIdx])); diff > 1e-4 {
			t.Fatalf("segment sample %d (full %d) differs by %g", i, fullIdx, diff)
		}
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		in, out, ch int
		p           Profile
	}{
		{0, 44100, 2, HQ},
		{44100, 0, 2, HQ},
		{44100, 44100, 2, HQ},
		{44100, 48000, 0, HQ},
		{44100, 48000, 2, Profile("ultra")},
	}
	for _, c := range cases {
		if _, err := New(c.in, c.out, c.ch, c.p); err == nil {
			t.Errorf("New(%d, %d, %d, %q): want error", c.in, c.out, c.ch, c.p)
		}
	}
}

// TestExtremeRatios: absurd but parseable rates must fail with the
// unsupported-format error, not overflow the tap sizing into a panic.
// The max-int64 cases overflow the float-to-int length conversion (and,
// separately, the phase arithmetic) without the design-time bounds.
func TestExtremeRatios(t *testing.T) {
	const maxInt = int(^uint(0) >> 1)
	cases := [][2]int{
		{48000, maxInt},
		{maxInt, 48000},
		{44100, 1234567}, // coprime pair over the cap for both profiles
	}
	for _, c := range cases {
		for _, p := range []Profile{HQ, Fast} {
			if _, err := New(c[0], c[1], 2, p); err == nil {
				t.Errorf("New(%d, %d, %s): want error, got resampler", c[0], c[1], p)
			}
		}
	}
}

// TestBankConcurrent hammers the bank cache from many goroutines across
// mixed ratios and profiles: every construction must succeed, and all
// resamplers of one key must share the identical coefficient bank (the
// per-key Once guarantee, checked under the race detector).
func TestBankConcurrent(t *testing.T) {
	type req struct {
		in, out int
		p       Profile
	}
	reqs := []req{
		{96000, 44100, HQ}, {44100, 48000, HQ}, {48000, 44100, HQ},
		{96000, 44100, Fast}, {192000, 48000, HQ}, {44100, 96000, Fast},
	}
	const workers = 8
	got := make([][]*Resampler, len(reqs))
	for i := range got {
		got[i] = make([]*Resampler, workers)
	}
	var wg sync.WaitGroup
	for i, rq := range reqs {
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(i, w int, rq req) {
				defer wg.Done()
				r, err := New(rq.in, rq.out, 2, rq.p)
				if err != nil {
					t.Errorf("New(%d, %d, %s): %v", rq.in, rq.out, rq.p, err)
					return
				}
				got[i][w] = r
			}(i, w, rq)
		}
	}
	wg.Wait()
	for i := range got {
		for w := 1; w < workers; w++ {
			if got[i][w] != nil && got[i][0] != nil && got[i][w].bank != got[i][0].bank {
				t.Errorf("request %d: workers got different banks for one key", i)
			}
		}
	}
}

func TestOutputLenUnknown(t *testing.T) {
	if got := OutputLen(-1, 44100, 48000); got != -1 {
		t.Errorf("OutputLen(-1) = %d, want -1 (unknown in, unknown out)", got)
	}
}

func BenchmarkHQ96to44(b *testing.B) { benchResample(b, 96000, 44100, HQ) }
func BenchmarkHQ44to48(b *testing.B) { benchResample(b, 44100, 48000, HQ) }
func BenchmarkFast96to44(b *testing.B) {
	benchResample(b, 96000, 44100, Fast)
}

// benchResample reports x-realtime for a stereo stream, the unit the
// quality-gates floors use (resampler HQ >= 200x per core).
func benchResample(b *testing.B, inRate, outRate int, p Profile) {
	r, err := New(inRate, outRate, 2, p)
	if err != nil {
		b.Fatal(err)
	}
	const chunk = 4096
	in := [][]float32{sineAt(inRate, 997, chunk, 0.5), sineAt(inRate, 1499, chunk, 0.5)}
	dst := [][]float32{make([]float32, 2*chunk), make([]float32, 2*chunk)}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src := [][]float32{in[0], in[1]}
		for len(src[0]) > 0 {
			_, consumed := r.Process(dst, src)
			src[0], src[1] = src[0][consumed:], src[1][consumed:]
		}
	}
	b.StopTimer()
	seconds := float64(b.N) * chunk / float64(inRate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}
