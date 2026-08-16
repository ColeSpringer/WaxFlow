package loudness

import (
	"math"
	"testing"
)

// peakFixture is stereo content exercising every peak path: a mid-level
// tone, one planted over, and an off-crest quarter-rate burst whose true
// peak sits between samples.
func peakFixture(frames int) [][]float32 {
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, frames)
		for i := range chans[c] {
			chans[c][i] = float32(0.7 * math.Sin(2*math.Pi*997*float64(i)/48000))
		}
		chans[c][300] = 1.3
		for i := 1000; i < 1064; i++ {
			chans[c][i] = float32(0.9 * math.Sin(math.Pi/2*float64(i)+math.Pi/4))
		}
	}
	return chans
}

// TestPeakMeterMatchesMeter: the two meters share one interpolator, so on
// identical samples they must read the identical true peak, bit for bit.
// This is what lets a transcode's TruePeak be cross-checked against an
// Analyze of its own output.
func TestPeakMeterMatchesMeter(t *testing.T) {
	chans := peakFixture(9600)
	m, err := NewMeter(48000, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPeakMeter(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Process(chans); err != nil {
		t.Fatal(err)
	}
	pm.Process(chans)
	m.Flush()
	pm.Flush()
	if got, want := 20*math.Log10(pm.Peak()), m.TruePeak(); got != want {
		t.Errorf("PeakMeter reads %.9f dBTP, Meter reads %.9f; they share the interpolator and must agree exactly", got, want)
	}
}

// TestPeakMeterReadsBetweenSamples: a quarter-rate sine phased so no sample
// lands on a crest. The stored peak is amplitude/sqrt(2); the meter must
// read the amplitude, which exists only between samples. The band is the
// interpolator's passband ripple at half Nyquist, which measures a hair
// high (+0.11 dB) because each phase is normalized to exact unity at DC.
func TestPeakMeterReadsBetweenSamples(t *testing.T) {
	const amp = 0.5
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, 4800)
		for i := range chans[c] {
			chans[c][i] = float32(amp * math.Sin(math.Pi/2*float64(i)+math.Pi/4))
		}
	}
	pm, err := NewPeakMeter(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	pm.Process(chans)
	pm.Flush()
	if got := pm.Peak(); got < 0.49 || got > 0.515 {
		t.Errorf("Peak = %.4f, want about %.2f; the crest between samples was not read", got, amp)
	}
	if stored := amp / math.Sqrt2; pm.Peak() <= stored {
		t.Errorf("Peak = %.4f does not exceed the stored peak %.4f; no interpolation happened", pm.Peak(), stored)
	}
}

// TestPeakMeterIntFloatAgree: ProcessInt's normalization is the audio
// package's exact power-of-two domain contract, so an integer chunk and
// its float image must read the same peak bit for bit at depths whose
// samples are exact in float32.
func TestPeakMeterIntFloatAgree(t *testing.T) {
	for _, bits := range []int{16, 24} {
		scale := math.Ldexp(1, bits-1)
		ints := make([][]int32, 2)
		floats := make([][]float32, 2)
		for c := range ints {
			ints[c] = make([]int32, 2000)
			floats[c] = make([]float32, 2000)
			for i := range ints[c] {
				v := int32(math.RoundToEven(0.9 * (scale - 1) * math.Sin(2*math.Pi*997*float64(i)/48000)))
				ints[c][i] = v
				floats[c][i] = float32(float64(v) / scale)
			}
		}
		pi, err := NewPeakMeter(48000, 2)
		if err != nil {
			t.Fatal(err)
		}
		pf, err := NewPeakMeter(48000, 2)
		if err != nil {
			t.Fatal(err)
		}
		pi.ProcessInt(ints, bits)
		pf.Process(floats)
		pi.Flush()
		pf.Flush()
		if pi.Peak() != pf.Peak() {
			t.Errorf("%d-bit: ProcessInt reads %.9f, Process reads %.9f; the domains must agree exactly",
				bits, pi.Peak(), pf.Peak())
		}
	}
}

// TestPeakMeterChunkInvariance: the reading is a property of the stream,
// not of how it was sliced.
func TestPeakMeterChunkInvariance(t *testing.T) {
	chans := peakFixture(4800)
	whole, err := NewPeakMeter(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	whole.Process(chans)
	whole.Flush()
	for _, chunk := range []int{1, 7, 160} {
		pm, err := NewPeakMeter(48000, 2)
		if err != nil {
			t.Fatal(err)
		}
		views := make([][]float32, 2)
		for off := 0; off < len(chans[0]); off += chunk {
			end := min(off+chunk, len(chans[0]))
			for c := range views {
				views[c] = chans[c][off:end]
			}
			pm.Process(views)
		}
		pm.Flush()
		if pm.Peak() != whole.Peak() {
			t.Errorf("chunk %d: Peak = %.9f, whole-stream %.9f", chunk, pm.Peak(), whole.Peak())
		}
	}
}

// TestPeakMeterFlushCatchesTail: the interpolation point trails the input
// by half a window, so a crest between the stream's final samples is only
// computable once Flush pushes the silence after them. A meter read
// without Flush under-reports exactly that tail.
func TestPeakMeterFlushCatchesTail(t *testing.T) {
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, 48)
		for i := 44; i < 48; i++ {
			chans[c][i] = float32(0.8485 * math.Sin(math.Pi/2*float64(i)+math.Pi/4))
		}
	}
	pm, err := NewPeakMeter(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	pm.Process(chans)
	before := pm.Peak()
	pm.Flush()
	after := pm.Peak()
	if before > 0.7 {
		t.Errorf("pre-Flush Peak = %.4f; the tail crest leaked out before the drain, so this proves nothing", before)
	}
	if after < 0.8 {
		t.Errorf("post-Flush Peak = %.4f, want about 0.85; the drain did not count the tail", after)
	}
}

// TestPeakMeterResumesAfterFlush: Flush is not terminal. A metered chain
// wraps a caller-owned source, and a caller may read to EOF (which
// flushes), seek, and read again; the meter must keep accumulating as a
// fresh segment rather than fail, and a second Flush must still drain the
// second segment's tail.
func TestPeakMeterResumesAfterFlush(t *testing.T) {
	quiet := make([][]float32, 2)
	loud := make([][]float32, 2)
	for c := range quiet {
		quiet[c] = make([]float32, 480)
		loud[c] = make([]float32, 480)
		for i := range quiet[c] {
			quiet[c][i] = float32(0.3 * math.Sin(math.Pi/2*float64(i)+math.Pi/4))
			loud[c][i] = float32(0.9 * math.Sin(math.Pi/2*float64(i)+math.Pi/4))
		}
	}
	pm, err := NewPeakMeter(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	pm.Process(quiet)
	pm.Flush()
	first := pm.Peak()
	pm.Process(loud)
	pm.Flush()
	if pm.Peak() <= first || pm.Peak() < 0.88 {
		t.Errorf("Peak = %.4f after resuming (first segment read %.4f); the second segment was not measured", pm.Peak(), first)
	}
}

// TestPeakMeterHighRateUsesSampleGrid: above 192 kHz there is no
// interpolator (newTruePeak returns nil), so the reading is the sample
// peak and a between-sample crest is invisible. That is the documented
// contract, pinned so a change to the factor table shows up here.
func TestPeakMeterHighRateUsesSampleGrid(t *testing.T) {
	chans := make([][]float32, 2)
	var maxAbs float64
	for c := range chans {
		chans[c] = make([]float32, 2000)
		for i := range chans[c] {
			chans[c][i] = float32(1.2 * math.Sin(math.Pi/2*float64(i)+math.Pi/4))
			if a := math.Abs(float64(chans[c][i])); a > maxAbs {
				maxAbs = a
			}
		}
	}
	pm, err := NewPeakMeter(200000, 2)
	if err != nil {
		t.Fatal(err)
	}
	pm.Process(chans)
	pm.Flush()
	if pm.Peak() != maxAbs {
		t.Errorf("Peak = %.9f, want the sample peak %.9f exactly; no oversampling runs above 192 kHz", pm.Peak(), maxAbs)
	}
}
