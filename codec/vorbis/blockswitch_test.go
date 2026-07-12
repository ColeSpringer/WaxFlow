package vorbis

import (
	"math"
	"math/rand"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// TestBlockSwitchTDAC drives a long/short/long block sequence through the
// encoder's analysis window + forward MDCT and the decoder's imdct + applyWindow
// + overlap-add (the exact decode.go arithmetic), verifying the transition
// windows reconstruct. It bypasses floor/residue to isolate the transform path,
// so a windowing or overlap regression in block switching fails here loudly,
// separately from any coding change.
func TestBlockSwitchTDAC(t *testing.T) {
	const long, short = 2048, 256
	// Block-size sequence (by size). Centers advance by (prev+cur)/4.
	sizes := []int{long, long, long, short, short, short, short, short, short, short, short, long, long, long}

	// Build centers.
	centers := make([]int64, len(sizes))
	centers[0] = 0
	for i := 1; i < len(sizes); i++ {
		centers[i] = centers[i-1] + int64((sizes[i-1]+sizes[i])/4)
	}
	total := int(centers[len(centers)-1]) + long
	rng := rand.New(rand.NewSource(7))
	x := make([]float64, total+long)
	for i := range x {
		x[i] = 0.3*math.Sin(2*math.Pi*float64(i)*0.01) + 0.1*(rng.Float64()*2-1)
	}

	planFor := func(n int) *imdctPlan { return getPlan(n) }
	fwdFor := map[int]*mdctForward{long: newMDCTForward(long), short: newMDCTForward(short)}

	recon := make([]float64, total+2*long)
	var prev []float64
	var prevN int
	var outPos int64
	wbuf := make([]float32, long)
	spec := make([]float32, long/2)
	tbuf := make([]float64, long)
	cr := make([]float64, long)
	ci := make([]float64, long)

	neighbour := func(n, other int) int {
		if other < n {
			return other
		}
		return n
	}
	analysisWindow := func(dst []float32, center int64, n, ln, rn int) {
		leftWin := planFor(ln).window
		rightWin := planFor(rn).window
		leftBegin := n/4 - ln/4
		leftEnd := leftBegin + ln/2
		rightBegin := 3*n/4 - rn/4
		rightEnd := rightBegin + rn/2
		start := center - int64(n/2)
		at := func(i int) float64 {
			idx := int(start) + i
			if idx < 0 || idx >= len(x) {
				return 0
			}
			return x[idx]
		}
		for i := 0; i < leftBegin; i++ {
			dst[i] = 0
		}
		for i := leftBegin; i < leftEnd; i++ {
			dst[i] = float32(at(i)) * leftWin[i-leftBegin]
		}
		for i := leftEnd; i < rightBegin; i++ {
			dst[i] = float32(at(i))
		}
		for i := rightBegin; i < rightEnd; i++ {
			dst[i] = float32(at(i)) * rightWin[rightEnd-1-i]
		}
		for i := rightEnd; i < n; i++ {
			dst[i] = 0
		}
	}

	for bi, n := range sizes {
		ln := neighbour(n, prevN)
		rn := n
		if bi+1 < len(sizes) {
			rn = neighbour(n, sizes[bi+1])
		}
		if bi == 0 {
			ln = neighbour(n, long) // virtual long left neighbour
		}
		analysisWindow(wbuf[:n], centers[bi], n, ln, rn)
		fwdFor[n].forward(wbuf[:n], spec[:n/2])

		// Decoder side: imdct, applyWindow, overlap-add (decode.go arithmetic).
		plan := planFor(n)
		leftW := planFor(ln).window
		rightW := planFor(rn).window
		plan.imdct(spec[:n/2], tbuf[:n], cr[:n], ci[:n])
		applyWindow(tbuf[:n], n, ln, rn, leftW, rightW)
		cur := make([]float64, n)
		copy(cur, tbuf[:n])

		if prev == nil {
			prev = cur
			prevN = n
			continue
		}
		l := (prevN + n) / 4
		shift := (prevN - n) / 4
		for j := 0; j < l; j++ {
			var v float64
			if pi := j + prevN/2; pi < prevN {
				v += prev[pi]
			}
			if ci := j - shift; ci >= 0 && ci < n {
				v += cur[ci]
			}
			recon[outPos+int64(j)] = v
		}
		outPos += int64(l)
		prev = cur
		prevN = n
	}

	// Compare reconstruction to input. The decoder's output stream starts at
	// input sample 0 (first block primes). Find the best small alignment.
	best := math.Inf(1)
	bestLag := 0
	for lag := 0; lag <= 4; lag++ {
		var e, s float64
		for i := 2048; i < int(outPos)-2048; i++ {
			d := recon[i] - x[i+lag]
			e += d * d
			s += x[i+lag] * x[i+lag]
		}
		if e/s < best {
			best = e / s
			bestLag = lag
		}
	}
	t.Logf("mixed TDAC NRMSE=%.5f at lag %d over %d samples", math.Sqrt(best), bestLag, int(outPos))
	if math.Sqrt(best) > 1e-3 {
		t.Fatalf("mixed-block TDAC failed: NRMSE %.4f", math.Sqrt(best))
	}
}

// TestBlockSwitchRoundTrip runs a transient signal (sharp pulses) through the
// full encoder and decoder, so the block-size planner actually switches to short
// blocks, and confirms the gapless count is exact and the decoded stream aligns
// with the source at zero lag (the priming delay stays 0 despite the switches).
func TestBlockSwitchRoundTrip(t *testing.T) {
	const rate = 44100
	const n = rate
	f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	src := make([]float32, n)
	period := rate / 8
	for i := 0; i < n; i++ {
		phase := i % period
		env := math.Exp(-float64(phase) / float64(rate) * 60)
		src[i] = float32(env * math.Sin(2*math.Pi*2000*float64(phase)/float64(rate)) * 0.6)
	}

	e, err := NewEncoder(f, &EncoderOptions{Quality: 4})
	if err != nil {
		t.Fatal(err)
	}
	packets, _, tr := encodeSignal(t, e, [][]float32{src})
	cfg, err := ParseConfig(e.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	// The planner must have used short blocks on this transient content.
	mb := ModeBits(cfg)
	nShort := 0
	for _, p := range packets {
		if bs, ok := PacketBlockSize(cfg, mb, p); ok && bs == cfg.blockSizes[0] {
			nShort++
		}
	}
	if nShort == 0 {
		t.Fatalf("no short blocks emitted for a transient signal (%d packets)", len(packets))
	}
	// The first block is always long (priming): a short first block would shift
	// the whole stream.
	if bs, _ := PacketBlockSize(cfg, mb, packets[0]); bs != cfg.blockSizes[1] {
		t.Fatalf("first block is not long (size %d)", bs)
	}

	dec := decodePackets(t, cfg, packets)
	framesOut := int64(len(dec))
	if framesOut-tr.Padding != tr.Samples {
		t.Fatalf("gapless: %d frames - %d padding != %d samples", framesOut, tr.Padding, tr.Samples)
	}
	trimmed := dec[:framesOut-tr.Padding]
	var sqErr, sqSig float64
	for i := 0; i < len(trimmed) && i < n; i++ {
		d := float64(trimmed[i]) - float64(src[i])
		sqErr += d * d
		sqSig += float64(src[i]) * float64(src[i])
	}
	nrmse := math.Sqrt(sqErr / sqSig)
	t.Logf("%d packets (%d short), NRMSE=%.4f at lag 0", len(packets), nShort, nrmse)
	if nrmse > 0.35 {
		t.Errorf("block-switch reconstruction NRMSE %.4f too high (misaligned or mis-coded)", nrmse)
	}
}
