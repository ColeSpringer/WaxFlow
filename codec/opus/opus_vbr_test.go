package opus

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// TestOpusVBREncode checks that VBR encoding sizes each frame to its content:
// alternating loud harmonic and near-silent blocks must produce packets whose
// sizes track the block loudness, and every packet must decode cleanly with our
// full Opus decoder. The overall mean may sit above the nominal CBR budget:
// the analyser's tonality boost raises the target on harmonic content exactly
// as libopus does (the reference averages ~144 kb/s on this signal at a 96k
// target), so the bound asserted here is a loose in-family ceiling.
func TestOpusVBREncode(t *testing.T) {
	const (
		C    = 2
		sr   = SampleRate
		secs = 3
		n    = secs * sr
		kbps = 96
	)
	f := audio.Format{Rate: sr, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}

	// 0.2 s loud harmonic stacks alternating with 0.2 s near-silence.
	src := make([]float32, C*n)
	for c := 0; c < C; c++ {
		for i := 0; i < n; i++ {
			ts := float64(i) / sr
			loud := (i/(sr/5))%2 == 0
			var v float64
			if loud {
				for k := 1; k <= 8; k++ {
					v += 0.09 * math.Sin(2*math.Pi*float64(400*k)*ts)
				}
			} else {
				v = 0.001 * math.Sin(2*math.Pi*300*ts)
			}
			src[c*n+i] = float32(v)
		}
	}

	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: kbps * 1000, VBR: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkts [][]byte
	total := 0
	emit := func(p codec.Packet) error {
		pkts = append(pkts, append([]byte(nil), p.Data...))
		total += len(p.Data)
		return nil
	}
	buf := &audio.Buffer{Fmt: f, F: src, Stride: n, N: n}
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	if len(pkts) == 0 {
		t.Fatal("no packets emitted")
	}

	minSz, maxSz := len(pkts[0]), 0
	for _, p := range pkts {
		minSz = min(minSz, len(p))
		maxSz = max(maxSz, len(p))
	}
	cbrFrame := kbps * 1000 / 8 / 50
	mean := float64(total) / float64(len(pkts))
	// Mean packet size per 0.2 s block class (10 packets per block; the encoder
	// trails the input by its delay, well under one block).
	var loudSum, loudN, quietSum, quietN int
	for i, p := range pkts {
		if (i/10)%2 == 0 {
			loudSum += len(p)
			loudN++
		} else {
			quietSum += len(p)
			quietN++
		}
	}
	loudMean := float64(loudSum) / float64(loudN)
	quietMean := float64(quietSum) / float64(quietN)
	t.Logf("VBR: %d packets, size min=%d max=%d mean=%.1f loud=%.1f quiet=%.1f (CBR would be ~%d B/frame)",
		len(pkts), minSz, maxSz, mean, loudMean, quietMean, cbrFrame)
	if maxSz <= minSz {
		t.Fatalf("VBR produced constant-size packets (min=max=%d): rate control not active", minSz)
	}
	if quietMean > 0.75*loudMean {
		t.Errorf("VBR quiet-block mean %.1f B not well below loud-block mean %.1f B: sizes do not track content", quietMean, loudMean)
	}
	if mean >= 2*float64(cbrFrame) {
		t.Errorf("VBR mean %.1f B/frame above twice the nominal budget %d: tonality boost overshooting", mean, cbrFrame)
	}

	// Every VBR packet must decode with our full decoder (TOC + framing + CELT).
	cfg, err := ParseOpusHead(enc.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	decoded := 0
	sink := func(b *audio.Buffer) error { decoded += b.N; return nil }
	for i, p := range pkts {
		if err := dec.Decode(p, sink); err != nil {
			t.Fatalf("decode VBR packet %d (%d bytes): %v", i, len(p), err)
		}
	}
	if decoded == 0 {
		t.Fatal("decoder produced no samples")
	}
}

// TestOpusConstrainedVBREncode checks the constrained-VBR reservoir: over a
// sustained loud broadband signal (which unconstrained VBR would code well
// over target), the windowed short-term rate must stay bounded near the
// target, the long-term average must converge onto it, and every packet must
// still decode with our full decoder.
func TestOpusConstrainedVBREncode(t *testing.T) {
	const (
		C    = 2
		sr   = SampleRate
		secs = 4
		n    = secs * sr
		kbps = 96
	)
	f := audio.Format{Rate: sr, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}

	// A dense harmonic stack with slow vibrato: hard enough that VBR wants to
	// exceed the target continuously.
	src := make([]float32, C*n)
	for c := 0; c < C; c++ {
		for i := 0; i < n; i++ {
			ts := float64(i) / sr
			var v float64
			for k := 1; k <= 12; k++ {
				v += 0.05 * math.Sin(2*math.Pi*(float64(220*k)+4*math.Sin(2*math.Pi*1.5*ts))*ts)
			}
			src[c*n+i] = float32(v)
		}
	}

	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: kbps * 1000, VBR: true, ConstrainedVBR: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkts [][]byte
	total := 0
	emit := func(p codec.Packet) error {
		pkts = append(pkts, append([]byte(nil), p.Data...))
		total += len(p.Data)
		return nil
	}
	buf := &audio.Buffer{Fmt: f, F: src, Stride: n, N: n}
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	if len(pkts) == 0 {
		t.Fatal("no packets emitted")
	}

	// The long-term average must sit at or below the target (the drift loop
	// corrects overshoot), and no 1-second window may exceed it by more than
	// half the reservoir bound.
	cbrFrame := float64(kbps*1000) / 8 / 50
	mean := float64(total) / float64(len(pkts))
	worstWin := 0
	for i := 0; i+50 <= len(pkts); i++ {
		w := 0
		for _, p := range pkts[i : i+50] {
			w += len(p)
		}
		worstWin = max(worstWin, w)
	}
	t.Logf("CVBR: %d packets, mean %.1f B/frame (target %.0f), worst 1 s window %d B (target %d B)",
		len(pkts), mean, cbrFrame, worstWin, kbps*1000/8)
	if mean > 1.05*cbrFrame {
		t.Errorf("CVBR long-term mean %.1f B/frame exceeds the target %.0f by more than 5%%", mean, cbrFrame)
	}
	if float64(worstWin) > 1.25*float64(kbps*1000/8) {
		t.Errorf("CVBR worst 1 s window %d B exceeds the per-second target %d B by more than 25%%", worstWin, kbps*1000/8)
	}

	cfg, err := ParseOpusHead(enc.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	decoded := 0
	sink := func(b *audio.Buffer) error { decoded += b.N; return nil }
	for i, p := range pkts {
		if err := dec.Decode(p, sink); err != nil {
			t.Fatalf("decode CVBR packet %d (%d bytes): %v", i, len(p), err)
		}
	}
	if decoded == 0 {
		t.Fatal("decoder produced no samples")
	}
}
