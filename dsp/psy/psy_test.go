package psy

import (
	"math"
	"testing"
)

// testBands is a plausible 21-band table over 576 lines at 44.1 kHz,
// shaped like the MP3 long-block edges (narrow low, wide high).
var testBands = []int{0, 4, 8, 12, 16, 20, 24, 30, 36, 44, 52, 62, 74, 90,
	110, 134, 162, 196, 238, 288, 342, 576}

func testModel(t *testing.T, offsetDB float64) *Model {
	t.Helper()
	m, err := New(Config{Rate: 44100, Lines: 576, FFTSize: 1024,
		BandOffsets: testBands, OffsetDB: offsetDB})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sine(freq float64, rate, n int, amp float64, phase0 float64) []float32 {
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(amp * math.Sin(phase0+2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return x
}

// noise is a deterministic full-scale-ish uniform noise block.
func noise(n int, amp float64, seed uint64) []float32 {
	x := make([]float32, n)
	s := seed
	for i := range x {
		s = s*6364136223846793005 + 1442695040888963407
		v := float64(int64(s>>11))/float64(1<<52) - 1 // roughly [-1,1)
		x[i] = float32(amp * v)
	}
	return x
}

// warm runs several blocks so prediction history and pre-echo memory
// reach steady state, then returns the last result.
func warm(t *testing.T, m *Model, block func(i int) []float32, n int) Result {
	t.Helper()
	var res Result
	var err error
	for i := 0; i < n; i++ {
		res, err = m.Analyze(block(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	return res
}

// bandFor maps a frequency onto the test band table (MDCT line grid).
func bandFor(freq float64, rate int) int {
	line := int(freq / (float64(rate) / (2 * 576)))
	for b := 0; b+1 < len(testBands); b++ {
		if line < testBands[b+1] {
			return b
		}
	}
	return len(testBands) - 2
}

func TestToneMasking(t *testing.T) {
	m := testModel(t, 0)
	// Continuous phase across blocks keeps the tone predictable.
	phase := 0.0
	step := 2 * math.Pi * 1000 / 44100
	res := warm(t, m, func(i int) []float32 {
		x := sine(1000, 44100, 1024, 1.0, phase)
		phase += step * 1024
		return x
	}, 6)

	b := bandFor(1000, 44100)
	if res.Energy[b] <= 0 {
		t.Fatalf("no energy in tone band %d", b)
	}
	ratio := res.Thr[b] / res.Energy[b]
	// A predictable tone demands a large SNR: the threshold must sit
	// far below the band energy, but well above nothing.
	if ratio > 1e-2 || ratio < 1e-5 {
		t.Fatalf("tone threshold ratio %.3g outside (1e-5, 1e-2)", ratio)
	}
	// Spreading: the neighbor band's threshold is elevated over a band
	// far above the tone (which only has its ATH floor).
	far := bandFor(12000, 44100)
	if res.Thr[b+1] <= res.Thr[far] {
		t.Fatalf("no spreading: neighbor thr %.3g <= far thr %.3g", res.Thr[b+1], res.Thr[far])
	}
}

func TestNoiseDemandsLessSNRThanTone(t *testing.T) {
	band := bandFor(5000, 44100)

	mt := testModel(t, 0)
	phase := 0.0
	step := 2 * math.Pi * 5000 / 44100
	tone := warm(t, mt, func(i int) []float32 {
		x := sine(5000, 44100, 1024, 0.5, phase)
		phase += step * 1024
		return x
	}, 6)
	toneRatio := tone.Thr[band] / tone.Energy[band]

	mn := testModel(t, 0)
	nz := warm(t, mn, func(i int) []float32 { return noise(1024, 0.5, uint64(i)*977+3) }, 6)
	noiseRatio := nz.Thr[band] / nz.Energy[band]

	if noiseRatio < 10*toneRatio {
		t.Fatalf("noise ratio %.3g not well above tone ratio %.3g", noiseRatio, toneRatio)
	}
}

func TestPreEchoControl(t *testing.T) {
	band := bandFor(3000, 44100)

	// Steady loud noise: the settled threshold.
	ms := testModel(t, 0)
	steady := warm(t, ms, func(i int) []float32 { return noise(1024, 0.8, uint64(i)+1) }, 8)

	// Silence, then the same loud noise: the first loud block's
	// threshold is clamped by the silent history.
	ma := testModel(t, 0)
	warm(t, ma, func(i int) []float32 { return make([]float32, 1024) }, 6)
	attack, err := ma.Analyze(noise(1024, 0.8, 42))
	if err != nil {
		t.Fatal(err)
	}
	if attack.Thr[band] >= steady.Thr[band]/10 {
		t.Fatalf("pre-echo clamp missing: attack thr %.3g vs steady %.3g",
			attack.Thr[band], steady.Thr[band])
	}
}

func TestPerceptualEntropy(t *testing.T) {
	m := testModel(t, 0)
	res, err := m.Analyze(make([]float32, 1024))
	if err != nil {
		t.Fatal(err)
	}
	if res.PE != 0 {
		t.Fatalf("silence PE = %g, want 0", res.PE)
	}
	loud := warm(t, m, func(i int) []float32 { return noise(1024, 0.8, uint64(i)+7) }, 6)
	m2 := testModel(t, 0)
	quiet := warm(t, m2, func(i int) []float32 { return noise(1024, 1e-4, uint64(i)+7) }, 6)
	if loud.PE <= quiet.PE {
		t.Fatalf("PE not increasing with level: loud %g <= quiet %g", loud.PE, quiet.PE)
	}
	if loud.PE < 100 {
		t.Fatalf("loud noise PE %g implausibly small", loud.PE)
	}
}

func TestOffsetLowersThresholds(t *testing.T) {
	base := warm(t, testModel(t, 0), func(i int) []float32 { return noise(1024, 0.5, uint64(i)+1) }, 4)
	baseThr := append([]float64(nil), base.Thr...)
	hi := warm(t, testModel(t, 6), func(i int) []float32 { return noise(1024, 0.5, uint64(i)+1) }, 4)
	for b := range hi.Thr {
		// A 6 dB offset lowers thresholds by up to 3.98x and never
		// raises them; ATH-floored bands may move less.
		if hi.Thr[b] > baseThr[b] || baseThr[b] > hi.Thr[b]*4.2 {
			t.Fatalf("band %d: offset thr %.3g vs base %.3g", b, hi.Thr[b], baseThr[b])
		}
	}
}

func TestATHFloor(t *testing.T) {
	m := testModel(t, 0)
	res, err := m.Analyze(make([]float32, 1024))
	if err != nil {
		t.Fatal(err)
	}
	for b, thr := range res.Thr {
		if thr <= 0 {
			t.Fatalf("band %d: silent threshold %g, want positive ATH floor", b, thr)
		}
	}
}

func TestAttackDetector(t *testing.T) {
	d := NewAttackDetector(0)
	// Silence block establishes (no) history.
	if a, _ := d.Scan(make([]float32, 1024), 8); a {
		t.Fatal("attack on silence")
	}
	// Impulse burst in sub-window 5.
	x := make([]float32, 1024)
	for i := 5 * 128; i < 6*128; i++ {
		x[i] = 0.5
	}
	a, pos := d.Scan(x, 8)
	if !a || pos != 5 {
		t.Fatalf("burst: attack=%v pos=%d, want true 5", a, pos)
	}
	// Steady tone: one attack on onset at most, then quiet.
	d2 := NewAttackDetector(0)
	s := sine(1000, 44100, 1024, 0.5, 0)
	d2.Scan(s, 8)
	if a, _ := d2.Scan(s, 8); a {
		t.Fatal("attack on steady tone")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{Rate: 0, Lines: 576, FFTSize: 1024, BandOffsets: testBands},
		{Rate: 44100, Lines: 576, FFTSize: 1000, BandOffsets: testBands},
		{Rate: 44100, Lines: 576, FFTSize: 1024, BandOffsets: []int{0, 100}},
		{Rate: 44100, Lines: 576, FFTSize: 1024, BandOffsets: []int{4, 576}},
		{Rate: 44100, Lines: 576, FFTSize: 1024, BandOffsets: []int{0, 8, 8, 576}},
		{Rate: 44100, Lines: 576, FFTSize: 1024, BandOffsets: testBands, NoPredict: true, FixedC: 2},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
	if _, err := New(Config{Rate: 48000, Lines: 128, FFTSize: 256,
		BandOffsets: []int{0, 4, 8, 16, 32, 64, 128}, NoPredict: true, FixedC: 0.4}); err != nil {
		t.Errorf("short-block config rejected: %v", err)
	}
}

func TestFFTImpulseAndSine(t *testing.T) {
	p := newFFTPlan(64)
	re := make([]float64, 64)
	im := make([]float64, 64)
	re[0] = 1
	p.transform(re, im)
	for k := range re {
		if math.Abs(re[k]-1) > 1e-12 || math.Abs(im[k]) > 1e-12 {
			t.Fatalf("impulse bin %d = (%g,%g)", k, re[k], im[k])
		}
	}
	// A bin-exact cosine concentrates at its bin with amplitude N/2.
	for i := range re {
		re[i] = math.Cos(2 * math.Pi * 4 * float64(i) / 64)
		im[i] = 0
	}
	p.transform(re, im)
	for k := 0; k < 32; k++ {
		want := 0.0
		if k == 4 {
			want = 32
		}
		if math.Abs(re[k]-want) > 1e-9 || math.Abs(im[k]) > 1e-9 {
			t.Fatalf("cosine bin %d = (%g,%g), want (%g,0)", k, re[k], im[k], want)
		}
	}
}
