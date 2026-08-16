package aac

import (
	"math"
	"testing"
)

// buildTestElement assembles an SBR element with the header shape the
// conformance streams use (48 kHz, kx=22, patches 2..21 -> 22..41 and
// 18..20 -> 42..44) and a flat single-envelope frame, bypassing the
// bitstream: the transposition and adjustment chain under test is pure
// DSP.
func buildTestElement(t *testing.T) *sbrElement {
	t.Helper()
	el := newSBRElement(1, 48000, false, false)
	h := sbrHeader{
		ampRes: 1, startFreq: 12, stopFreq: 9, xoverBand: 0,
		freqScale: 2, alterScale: 1, noiseBands: 3,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	el.applyHeader(h)
	if !el.haveTbl {
		t.Fatal("test header derived no tables")
	}
	if el.tbl.kx != 22 || el.tbl.m != 23 {
		t.Fatalf("test tables kx=%d m=%d, want 22/23", el.tbl.kx, el.tbl.m)
	}
	return el
}

// flatFrame fills a channel's frame data with one flat envelope: constant
// energy across bands, negligible noise floor, no sinusoids.
func flatFrame(fd *sbrFrameData, tbl *sbrFreqTables) {
	*fd = sbrFrameData{}
	fd.grid = sbrGrid{
		frameClass: fixfix, numEnv: 1, lA: -1, numNoise: 1,
	}
	fd.grid.tE[0], fd.grid.tE[1] = 0, 16
	fd.grid.tQ[0], fd.grid.tQ[1] = 0, 16
	fd.ampRes = 0 // single-envelope FIXFIX forces 1.5 dB
	for b := 0; b < tbl.nLow; b++ {
		fd.env[0][b] = 60
	}
	for b := 0; b < tbl.nQ; b++ {
		fd.noise[0][b] = 30 // essentially no noise floor
	}
}

// TestSBRPatchFrequencyShift pins the defining property of spectral
// patching: a tone in a source band comes out shifted by exactly the
// integer band distance of its patch, 7500 Hz here, not mirrored within
// its band and not anywhere else.
func TestSBRPatchFrequencyShift(t *testing.T) {
	el := buildTestElement(t)
	const coreRate = 24000.0
	const tone = 3100.0 // source band 8, 100 Hz above its lower border
	const frames = 24
	in := make([]float32, 1024)
	out := make([]float32, 2048)
	var got []float32
	n0 := 0
	for f := 0; f < frames; f++ {
		for i := range in {
			in[i] = float32(0.5 * math.Sin(2*math.Pi*tone/coreRate*float64(n0+i)))
		}
		n0 += 1024
		el.beginFrame()
		flatFrame(&el.ch[0].frame, &el.tbl)
		el.dataOK = true
		el.process([][]float32{in}, [][]float32{out})
		got = append(got, out...)
	}
	// Goertzel-style power probe over the steady-state tail.
	probe := func(freq float64) float64 {
		var re, im float64
		seg := got[len(got)-16384:]
		for i, v := range seg {
			s, c := math.Sincos(2 * math.Pi * freq / 48000 * float64(i))
			re += float64(v) * c
			im += float64(v) * s
		}
		return re*re + im*im
	}
	direct := probe(tone + 7500) // patch shift: (22-2) bands * 375 Hz
	mirror := probe(10500 + 375 - 100)
	low := probe(tone)
	var others float64
	for _, f := range []float64{9000, 9750, 11250, 12000, 13000, 14000} {
		others = math.Max(others, probe(f))
	}
	t.Logf("power: direct(10600)=%.3g mirror(10775)=%.3g low(3100)=%.3g maxOther=%.3g",
		direct, mirror, low, others)
	if direct < 100*mirror || direct < 100*others {
		t.Errorf("patched tone is not at the shifted frequency: direct=%.3g mirror=%.3g other=%.3g",
			direct, mirror, others)
	}
	if low < direct/100 {
		t.Errorf("low band lost the source tone: low=%.3g direct=%.3g", low, direct)
	}
}
