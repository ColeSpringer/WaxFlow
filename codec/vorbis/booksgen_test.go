//go:build booksgen

package vorbis

// Offline codebook generator (clean-room). Run with:
//
//	go generate ./codec/vorbis
//
// which is wired to `go test -tags booksgen -run ^TestGenerateBooks$`. It
// synthesizes a deterministic corpus of tones, chords, noise, and sweeps from
// scratch (no external or libvorbis-derived audio), runs it through the encoder's
// real analysis front-end (windowed forward MDCT, peak-envelope floor fit,
// residue normalization and masking, and square-polar coupling for stereo), and
// trains the two product-lattice residue books by histogramming the floor-
// normalized residue onto their lattices and Huffman-coding the result. The
// lattice geometry is fixed in books.go; only the codeword lengths are trained.
// It writes books_gen.go, which is checked in.

import (
	"bytes"
	"fmt"
	"go/format"
	"math"
	"math/rand"
	"os"
	"testing"
)

func TestGenerateBooks(t *testing.T) {
	const rate = 44100
	n := longBlock
	n2 := longBlock / 2
	fwd := newMDCTForward(n)
	fl, _ := buildFloor1(n2, floorPartitions)
	win := fullWindow(n)

	noiseHist := make([]float64, resNoiseEntries)
	coarseHist := make([]float64, resCoarseEntries)
	r1Hist := make([]float64, resR1Entries)
	r2Hist := make([]float64, resR2Entries)
	r3Hist := make([]float64, resR3Entries)

	// scratch reused per block
	windowed := make([]float32, n)
	spec := make([]float32, n2)
	curve := make([]float32, n2)
	resid := make([]float32, n2)
	targets := make([]int, len(fl.xs))
	vals := make([]int, len(fl.xs))
	fFinal := make([]int, len(fl.xs))
	fStep2 := make([]bool, len(fl.xs))

	// residues computes the masked, floor-normalized residue of one windowed
	// block of a signal into resid. It runs the same MDCT/floor/normalize
	// front-end as emitBlock, but masks with a nil threshold (floor-only): the
	// generator has no psy model, so it cannot reproduce emitBlock's per-line
	// psy test. That makes it zero a superset of the lines the encoder zeros (it
	// drops the below-floor-but-above-threshold detail the encoder keeps), so the
	// smallest-residue tail is under-represented in training. The effect is on
	// codeword lengths only (size), not on any decoded value; tightening it by
	// feeding the generator a representative threshold is a known size lever.
	residues := func(sig []float32, start int) {
		for i := 0; i < n; i++ {
			j := start + i
			var x float32
			if j >= 0 && j < len(sig) {
				x = sig[j]
			}
			windowed[i] = x * win[i]
		}
		fwd.forward(windowed, spec)
		floor1Fit(fl, spec, targets, n2)
		floor1EncodeVals(fl, targets, vals, fFinal)
		fl.curve(vals, curve, fFinal, fStep2, n2)
		normalizeResidue(spec, curve, resid, n2)
		maskResidue(spec, resid, nil, n2)
	}

	// accumPair histograms one residue pair (v0,v1) onto every book's lattice,
	// walking the same coarse -> r1 -> r2 -> r3 cascade the encoder codes: the raw
	// pair onto the noise and coarse lattices, then each stage's rounding residual
	// onto the next refinement lattice. The generator does not classify, so each
	// book's table is a prior over all coded partitions rather than class-
	// conditional, trained at the front-end's nominal masking (see residues).
	refine := func(v0, v1, min, delta float64, hist []float64) (float64, float64) {
		i0 := refineLatIndex(v0, min, delta)
		i1 := refineLatIndex(v1, min, delta)
		hist[i0+i1*resRefineL]++
		return v0 - (min + float64(i0)*delta), v1 - (min + float64(i1)*delta)
	}
	accumPair := func(v0, v1 float64) {
		noiseHist[noiseLatIndex(v0)+noiseLatIndex(v1)*resNoiseL]++
		c0, c1 := coarseLatIndex(v0), coarseLatIndex(v1)
		coarseHist[c0+c1*resCoarseL]++
		r0 := v0 - coarseLatValue(c0)
		r1 := v1 - coarseLatValue(c1)
		r0, r1 = refine(r0, r1, resR1Min, resR1Delta, r1Hist)
		r0, r1 = refine(r0, r1, resR2Min, resR2Delta, r2Hist)
		refine(r0, r1, resR3Min, resR3Delta, r3Hist)
	}

	// partitionCoded reports whether a 32-line partition holds any coded line, so
	// training sees the residue of coded partitions (skips are never coded).
	partitionCoded := func(a []float32, base int) bool {
		for i := 0; i < resPartSize && base+i < len(a); i++ {
			if a[base+i] != 0 {
				return true
			}
		}
		return false
	}

	nBlocks := 0
	// Mono corpus: adjacent-bin pairs, the type-1 vector layout.
	for _, sig := range monoCorpus(rate) {
		for start := 0; start+n <= len(sig); start += n2 {
			residues(sig, start)
			for p := 0; p*resPartSize < n2; p++ {
				base := p * resPartSize
				if !partitionCoded(resid, base) {
					continue
				}
				for m := 0; m+1 < resPartSize && base+m+1 < n2; m += resCoarseDim {
					accumPair(float64(resid[base+m]), float64(resid[base+m+1]))
				}
			}
			nBlocks++
		}
	}
	// Stereo corpus: coupled magnitude and angle, each coded as its own type-1
	// channel, so each contributes adjacent-bin pairs exactly as the encoder emits
	// them (the magnitude like a mono channel, the angle where it is nonzero).
	mag := make([]float32, n2)
	ang := make([]float32, n2)
	accumChannel := func(v []float32) {
		for p := 0; p*resPartSize < n2; p++ {
			base := p * resPartSize
			if !partitionCoded(v, base) {
				continue
			}
			for m := 0; m+1 < resPartSize && base+m+1 < n2; m += resCoarseDim {
				accumPair(float64(v[base+m]), float64(v[base+m+1]))
			}
		}
	}
	for _, pair := range stereoCorpus(rate) {
		l, r := pair[0], pair[1]
		for start := 0; start+n <= len(l); start += n2 {
			residues(l, start)
			copy(mag, resid)
			residues(r, start)
			copy(ang, resid)
			for i := 0; i < n2; i++ {
				mag[i], ang[i] = coupleForward(mag[i], ang[i])
			}
			accumChannel(mag)
			accumChannel(ang)
			nBlocks++
		}
	}

	noiseLengths := huffmanLengths(noiseHist)
	coarseLengths := huffmanLengths(coarseHist)
	r1Lengths := huffmanLengths(r1Hist)
	r2Lengths := huffmanLengths(r2Hist)
	r3Lengths := huffmanLengths(r3Hist)
	for _, b := range []struct {
		name    string
		lengths []uint8
	}{
		{"noise", noiseLengths}, {"coarse", coarseLengths},
		{"r1", r1Lengths}, {"r2", r2Lengths}, {"r3", r3Lengths},
	} {
		if _, ok := assignCodewords(b.lengths); !ok {
			t.Fatalf("%s book over-subscribed", b.name)
		}
	}

	src := emitBooksFile(nBlocks, noiseLengths, coarseLengths, r1Lengths, r2Lengths, r3Lengths)
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatalf("gofmt generated source: %v\n%s", err, src)
	}
	if err := os.WriteFile("books_gen.go", formatted, 0o644); err != nil {
		t.Fatalf("write books_gen.go: %v", err)
	}
	t.Logf("trained on %d blocks; wrote books_gen.go (noise %d, coarse %d, refine %d entries)",
		nBlocks, len(noiseLengths), len(coarseLengths), len(r1Lengths))
}

// Lattice helpers duplicate the geometry from books.go so the generator bins
// values exactly where the encoder and decoder place them.
func coarseLatIndex(v float64) int {
	return clampIdx(math.Round((v-resCoarseMin)/resCoarseDelta), resCoarseL)
}
func coarseLatValue(i int) float64 { return resCoarseMin + float64(i)*resCoarseDelta }
func noiseLatIndex(v float64) int {
	return clampIdx(math.Round((v-resNoiseMin)/resNoiseDelta), resNoiseL)
}
func refineLatIndex(v, min, delta float64) int {
	return clampIdx(math.Round((v-min)/delta), resRefineL)
}
func clampIdx(f float64, L int) int {
	i := int(f)
	if i < 0 {
		return 0
	}
	if i >= L {
		return L - 1
	}
	return i
}

// monoCorpus synthesizes clean-room mono training signals: chords across the
// band, harmonic tones, pink-ish noise, and sweeps. Deterministic seeds keep the
// generated book reproducible.
func monoCorpus(rate int) [][]float32 {
	dur := rate // 1 s each
	var out [][]float32
	chords := [][]float64{
		{130.81, 164.81, 196.00}, {220, 277.18, 329.63}, {440, 554.37, 659.25},
		{523.25, 1046.5, 1568}, {880, 1108.73, 1318.51}, {1000, 3000, 6000},
	}
	for ci, ch := range chords {
		sig := make([]float32, dur)
		for i := range sig {
			var v float64
			for _, f := range ch {
				for h := 1; h <= 5; h++ {
					v += (0.25 / float64(h)) * math.Sin(2*math.Pi*f*float64(h)*float64(i)/float64(rate))
				}
			}
			sig[i] = float32(v * 0.3)
		}
		_ = ci
		out = append(out, sig)
	}
	for seed := int64(1); seed <= 3; seed++ {
		rng := rand.New(rand.NewSource(seed))
		sig := make([]float32, dur)
		var lp float64
		for i := range sig {
			lp = 0.95*lp + 0.05*(rng.Float64()*2-1)
			sig[i] = float32(lp * 0.7)
		}
		out = append(out, sig)
	}
	for _, span := range [][2]float64{{80, 8000}, {200, 16000}} {
		sig := make([]float32, dur)
		for i := range sig {
			t := float64(i) / float64(dur)
			f := span[0] * math.Pow(span[1]/span[0], t)
			sig[i] = float32(0.5 * math.Sin(2*math.Pi*f*float64(i)/float64(rate)))
		}
		out = append(out, sig)
	}
	return out
}

// stereoCorpus synthesizes clean-room stereo pairs whose channels differ (detune,
// pan, decorrelated noise) so the coupled magnitude/angle residues that feed the
// type-2 books are represented, not just the dual-mono (zero-angle) case.
func stereoCorpus(rate int) [][2][]float32 {
	dur := rate
	var out [][2][]float32
	bases := [][]float64{{220, 330, 440}, {523.25, 784, 1046.5}, {440, 880, 1760}}
	for _, base := range bases {
		l := make([]float32, dur)
		r := make([]float32, dur)
		for i := range l {
			var lv, rv float64
			for k, f := range base {
				lv += 0.3 * math.Sin(2*math.Pi*f*float64(i)/float64(rate))
				rv += 0.3 * math.Sin(2*math.Pi*f*(1+0.004*float64(k+1))*float64(i)/float64(rate))
			}
			l[i] = float32(lv * 0.3)
			r[i] = float32(rv * 0.3)
		}
		out = append(out, [2][]float32{l, r})
	}
	rng := rand.New(rand.NewSource(99))
	l := make([]float32, dur)
	r := make([]float32, dur)
	var ll, rl float64
	for i := range l {
		ll = 0.9*ll + 0.1*(rng.Float64()*2-1)
		rl = 0.9*rl + 0.1*(rng.Float64()*2-1)
		l[i] = float32(ll * 0.6)
		r[i] = float32(rl * 0.6)
	}
	out = append(out, [2][]float32{l, r})
	return out
}

func emitBooksFile(nBlocks int, noise, coarse, r1, r2, r3 []uint8) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by booksgen; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Regenerate with `go generate ./codec/vorbis`.\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Clean-room product-lattice residue-book codeword lengths, Huffman-trained on\n")
	fmt.Fprintf(&b, "// a self-synthesized corpus (%d analysis blocks of tones, chords, noise, and\n", nBlocks)
	fmt.Fprintf(&b, "// sweeps). No external or libvorbis-derived tables. Geometry is in books.go.\n\n")
	fmt.Fprintf(&b, "package vorbis\n\n")
	emitLengths(&b, "resNoiseLengths", noise)
	b.WriteString("\n")
	emitLengths(&b, "resCoarseLengths", coarse)
	b.WriteString("\n")
	emitLengths(&b, "resR1Lengths", r1)
	b.WriteString("\n")
	emitLengths(&b, "resR2Lengths", r2)
	b.WriteString("\n")
	emitLengths(&b, "resR3Lengths", r3)
	return b.Bytes()
}

func emitLengths(b *bytes.Buffer, name string, v []uint8) {
	fmt.Fprintf(b, "var %s = []uint8{\n", name)
	for i, x := range v {
		if i%20 == 0 {
			b.WriteString("\t")
		}
		fmt.Fprintf(b, "%d,", x)
		if i%20 == 19 {
			b.WriteString("\n")
		} else {
			b.WriteString(" ")
		}
	}
	if len(v)%20 != 0 {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
}
