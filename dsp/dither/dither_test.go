package dither

import (
	"math"
	"testing"
)

func sine(rate int, freq float64, frames int, amp float64) []float32 {
	s := make([]float32, frames)
	for i := range s {
		s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return s
}

// errSignal quantizes src and returns the error in LSB units.
func errSignal(t *testing.T, q *Quantizer, src []float32) []float64 {
	t.Helper()
	dst := make([]int32, len(src))
	q.Quantize(dst, src, 0)
	scale := math.Ldexp(1, q.Bits()-1)
	e := make([]float64, len(src))
	for i := range src {
		e[i] = float64(dst[i]) - float64(src[i])*scale
	}
	return e
}

// toneAmp is a Hann-windowed quadrature amplitude estimate, for picking
// individual distortion harmonics out of the error signal.
func toneAmp(x []float64, rate int, freq float64) float64 {
	var a, b, wsum float64
	n := float64(len(x))
	for i, v := range x {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/n)
		ph := 2 * math.Pi * freq * float64(i) / float64(rate)
		a += v * w * math.Cos(ph)
		b += v * w * math.Sin(ph)
		wsum += w
	}
	return 2 * math.Hypot(a, b) / wsum
}

func rms(x []float64) float64 {
	var sum float64
	for _, v := range x {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

// TestErrorMoments: TPDF-dithered quantization error has RMS
// sqrt(1/12 + 1/6) = 0.5 LSB and near-zero mean.
func TestErrorMoments(t *testing.T) {
	q, err := NewQuantizer(16, 1, TPDF, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	e := errSignal(t, q, sine(44100, 997, 1<<16, 0.25))
	if got := rms(e); got < 0.45 || got > 0.55 {
		t.Errorf("error RMS %.4f LSB, want 0.5 +- 0.05", got)
	}
	var mean float64
	for _, v := range e {
		mean += v
	}
	mean /= float64(len(e))
	if math.Abs(mean) > 0.02 {
		t.Errorf("error mean %.4f LSB, want ~0", mean)
	}
}

// TestDecorrelation: undithered quantization of a low-level tone has
// harmonic distortion; TPDF buries it. Compare the third harmonic.
func TestDecorrelation(t *testing.T) {
	const rate, freq = 44100, 997.0
	// About 3 LSB peak at 16 bits: the regime where truncation
	// distortion is at its ugliest relative to signal.
	src := sine(rate, freq, 1<<16, 3.0/32768)

	qNone, err := NewQuantizer(16, 1, None, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	qTPDF, err := NewQuantizer(16, 1, TPDF, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	h3None := toneAmp(errSignal(t, qNone, src), rate, 3*freq)
	h3TPDF := toneAmp(errSignal(t, qTPDF, src), rate, 3*freq)
	if h3TPDF > h3None/4 {
		t.Errorf("third harmonic: dithered %.4f LSB vs undithered %.4f LSB; dither must decorrelate",
			h3TPDF, h3None)
	}
}

// TestShapedSpectrum: F-weighted shaping trades a higher total noise
// floor for a quieter audible band. Compare band levels against flat
// TPDF at 44.1k.
func TestShapedSpectrum(t *testing.T) {
	const rate = 44100
	src := make([]float32, 1<<17) // silence: pure noise floor out

	flat, err := NewQuantizer(16, 1, TPDF, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	shaped, err := NewQuantizer(16, 1, Shaped, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	eFlat := errSignal(t, flat, src)
	eShaped := errSignal(t, shaped, src)

	// Average level across sensitive-band probe tones vs high band.
	level := func(e []float64, freqs []float64) float64 {
		var sum float64
		for _, f := range freqs {
			sum += toneAmp(e, rate, f)
		}
		return sum / float64(len(freqs))
	}
	lowFreqs := []float64{500, 1000, 2000, 3000, 4000}
	lowGain := 20 * math.Log10(level(eShaped, lowFreqs)/level(eFlat, lowFreqs))
	if lowGain > -6 {
		t.Errorf("shaped noise in the sensitive band is %.1f dB vs flat, want <= -6 dB", lowGain)
	}
	hiFreqs := []float64{19000, 20000, 21000}
	hiGain := 20 * math.Log10(level(eShaped, hiFreqs)/level(eFlat, hiFreqs))
	if hiGain < 6 {
		t.Errorf("shaped noise at the top of the band is %.1f dB vs flat, want >= +6 dB (energy must go somewhere)", hiGain)
	}
	// Total power stays bounded: the published filter costs ~11 dB.
	if totalGain := 20 * math.Log10(rms(eShaped)/rms(eFlat)); totalGain > 14 {
		t.Errorf("shaped total noise %.1f dB over flat, want <= 14 dB", totalGain)
	}
}

// TestDeterminism: same seed, same output, chunking included; different
// seed differs.
func TestDeterminism(t *testing.T) {
	src := sine(44100, 441, 8192, 0.5)
	quantize := func(seed uint64, chunk int) []int32 {
		q, err := NewQuantizer(16, 1, TPDF, seed)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int32, len(src))
		for pos := 0; pos < len(src); pos += chunk {
			end := min(pos+chunk, len(src))
			q.Quantize(out[pos:end], src[pos:end], 0)
		}
		return out
	}
	a := quantize(DefaultSeed, len(src))
	b := quantize(DefaultSeed, 100)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sample %d differs across chunkings", i)
		}
	}
	c := quantize(12345, len(src))
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different seeds produced identical dither")
	}
}

// TestChannelIndependence: channels must not share a dither stream.
func TestChannelIndependence(t *testing.T) {
	q, err := NewQuantizer(16, 2, TPDF, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	src := make([]float32, 4096) // silence exposes raw dither
	l := make([]int32, len(src))
	r := make([]int32, len(src))
	q.Quantize(l, src, 0)
	q.Quantize(r, src, 1)
	same := 0
	for i := range l {
		if l[i] == r[i] {
			same++
		}
	}
	// Independent TPDF on silence collides often (small alphabet), but
	// identical streams collide always.
	if same == len(l) {
		t.Error("channel dither streams are identical")
	}
}

func TestClampAndNaN(t *testing.T) {
	q, err := NewQuantizer(16, 1, TPDF, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	src := []float32{2, -2, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))}
	dst := make([]int32, len(src))
	q.Quantize(dst, src, 0)
	want := []int32{32767, -32768, 0, 32767, -32768}
	for i := range want {
		if dst[i] != want[i] {
			t.Errorf("dst[%d] = %d, want %d", i, dst[i], want[i])
		}
	}
}

// TestShapedAnomalyRecovery: NaN and infinite samples must not poison
// the noise-shaping feedback loop. A NaN quantizes to 0 and advances the
// history with zero error; infinities clamp to the rails with a bounded
// feedback error. Every sample after the anomalies must quantize sanely.
func TestShapedAnomalyRecovery(t *testing.T) {
	q, err := NewQuantizer(16, 1, Shaped, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	const n = 4096
	src := sine(44100, 997, n, 0.25)
	src[100] = float32(math.NaN())
	src[200] = float32(math.Inf(1))
	src[300] = float32(math.Inf(-1))

	dst := make([]int32, n)
	q.Quantize(dst, src, 0)

	if dst[100] != 0 {
		t.Errorf("NaN quantized to %d, want 0", dst[100])
	}
	if dst[200] != 32767 || dst[300] != -32768 {
		t.Errorf("infinities quantized to %d/%d, want rails", dst[200], dst[300])
	}
	// The shaped quantizer's worst-case sample deviation is bounded by
	// the feedback filter: |out - v| <= (1 + sum|h|) * |e|max, about 14
	// LSB. Anything beyond that after the anomalies means the history
	// was poisoned.
	for i := 320; i < n; i++ {
		ideal := float64(src[i]) * 32768
		if d := math.Abs(float64(dst[i]) - ideal); d > 16 {
			t.Fatalf("sample %d off by %.0f LSB after anomalies (feedback poisoned)", i, d)
		}
	}
}

// TestWideningExact: a 16-bit-exact value quantized to 24 bits with
// shaping off reproduces the shifted value exactly.
func TestWideningExact(t *testing.T) {
	q, err := NewQuantizer(24, 1, None, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	src := []float32{0, 0.5, -0.5, 12345.0 / 32768, -32768.0 / 32768}
	dst := make([]int32, len(src))
	q.Quantize(dst, src, 0)
	want := []int32{0, 1 << 22, -(1 << 22), 12345 << 8, -(32768 << 8)}
	for i := range want {
		if dst[i] != want[i] {
			t.Errorf("dst[%d] = %d, want %d", i, dst[i], want[i])
		}
	}
}

func TestSupportsShaping(t *testing.T) {
	yes := []int{44100, 48000}
	no := []int{8000, 22050, 32000, 46000, 88200, 96000, 176400, 192000}
	for _, r := range yes {
		if !SupportsShaping(r) {
			t.Errorf("SupportsShaping(%d) = false, want true", r)
		}
	}
	for _, r := range no {
		if SupportsShaping(r) {
			t.Errorf("SupportsShaping(%d) = true, want false", r)
		}
	}
}

func benchQuantize(b *testing.B, shaping Shaping) {
	q, err := NewQuantizer(16, 1, shaping, DefaultSeed)
	if err != nil {
		b.Fatal(err)
	}
	const chunk = 4096
	src := sine(48000, 997, chunk, 0.5)
	dst := make([]int32, chunk)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Quantize(dst, src, 0)
	}
	b.StopTimer()
	seconds := float64(b.N) * chunk / 48000
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkQuantizeTPDF(b *testing.B)   { benchQuantize(b, TPDF) }
func BenchmarkQuantizeShaped(b *testing.B) { benchQuantize(b, Shaped) }

func TestNewQuantizerValidation(t *testing.T) {
	if _, err := NewQuantizer(1, 1, TPDF, 1); err == nil {
		t.Error("1-bit target: want error")
	}
	if _, err := NewQuantizer(33, 1, TPDF, 1); err == nil {
		t.Error("33-bit target: want error")
	}
	if _, err := NewQuantizer(16, 0, TPDF, 1); err == nil {
		t.Error("zero channels: want error")
	}
	if _, err := NewQuantizer(16, 1, Shaping(9), 1); err == nil {
		t.Error("unknown shaping: want error")
	}
}
