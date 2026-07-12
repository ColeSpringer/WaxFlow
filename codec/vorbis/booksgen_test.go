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

	coarseHist := make([]float64, resCoarseEntries)
	fineHist := make([]float64, resFineEntries)

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
	// block of a signal into resid (the exact front-end emitBlock runs).
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
		maskResidue(spec, resid, n2)
	}

	// accumPair histograms one residue pair (v0,v1) onto the coarse lattice and
	// its coarse-quantization residual onto the fine lattice, matching the cascade
	// the encoder codes: coarse index, then the leftover onto the fine index.
	accumPair := func(v0, v1 float64) {
		i0, i1 := coarseLatIndex(v0), coarseLatIndex(v1)
		coarseHist[i0+i1*resCoarseL]++
		r0 := v0 - coarseLatValue(i0)
		r1 := v1 - coarseLatValue(i1)
		fineHist[fineLatIndex(r0)+fineLatIndex(r1)*resFineL]++
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

	coarseLengths := huffmanLengths(coarseHist)
	fineLengths := huffmanLengths(fineHist)
	if _, ok := assignCodewords(coarseLengths); !ok {
		t.Fatal("coarse book over-subscribed")
	}
	if _, ok := assignCodewords(fineLengths); !ok {
		t.Fatal("fine book over-subscribed")
	}

	src := emitBooksFile(nBlocks, coarseLengths, fineLengths)
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatalf("gofmt generated source: %v\n%s", err, src)
	}
	if err := os.WriteFile("books_gen.go", formatted, 0o644); err != nil {
		t.Fatalf("write books_gen.go: %v", err)
	}
	t.Logf("trained on %d blocks; wrote books_gen.go (coarse %d, fine %d entries)",
		nBlocks, len(coarseLengths), len(fineLengths))
}

// Lattice helpers duplicate the geometry from books.go so the generator bins
// values exactly where the encoder and decoder place them.
func coarseLatIndex(v float64) int {
	return clampIdx(math.Round((v-resCoarseMin)/resCoarseDelta), resCoarseL)
}
func coarseLatValue(i int) float64 { return resCoarseMin + float64(i)*resCoarseDelta }
func fineLatIndex(v float64) int   { return clampIdx(math.Round((v-resFineMin)/resFineDelta), resFineL) }
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

func emitBooksFile(nBlocks int, coarse, fine []uint8) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by booksgen; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Regenerate with `go generate ./codec/vorbis`.\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Clean-room product-lattice residue-book codeword lengths, Huffman-trained on\n")
	fmt.Fprintf(&b, "// a self-synthesized corpus (%d analysis blocks of tones, chords, noise, and\n", nBlocks)
	fmt.Fprintf(&b, "// sweeps). No external or libvorbis-derived tables. Geometry is in books.go.\n\n")
	fmt.Fprintf(&b, "package vorbis\n\n")
	emitLengths(&b, "resCoarseLengths", coarse)
	b.WriteString("\n")
	emitLengths(&b, "resFineLengths", fine)
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
