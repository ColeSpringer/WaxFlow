package ape_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// decodeFile decodes a whole .ape file through the codec and returns the
// samples interleaved at the stream's own depth.
func decodeFile(t *testing.T, raw []byte) (ape.Config, []int32) {
	t.Helper()
	cfg, pkts := frames(t, raw)
	dec, err := ape.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer dec.Release()
	var got []int32
	emit := func(b *audio.Buffer) error {
		for i := range b.N {
			for c := range b.Fmt.Channels {
				got = append(got, b.ChanI(c)[i])
			}
		}
		return nil
	}
	for i, pkt := range pkts {
		if err := dec.Decode(pkt, emit); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return cfg, got
}

// TestFixturesBitExact is the decoder's tool-free gate: every committed
// fixture decodes to exactly the samples the reference encoder was handed.
// Nothing external runs, so this is the assertion that holds on a machine with
// no encoder and no ffmpeg.
func TestFixturesBitExact(t *testing.T) {
	for _, f := range apeFixtures {
		t.Run(filepath.Base(f.path), func(t *testing.T) {
			raw, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, got := decodeFile(t, raw)
			if cfg.Format() != f.fmt {
				t.Fatalf("format = %v, want %v", cfg.Format(), f.fmt)
			}
			want := f.samples()
			if idx := testutil.DiffI32(got, want); idx != -1 {
				if idx >= len(got) || idx >= len(want) {
					t.Fatalf("decoded %d samples, want %d", len(got), len(want))
				}
				t.Fatalf("sample %d = %d, want %d (of %d)", idx, got[idx], want[idx], len(want))
			}
		})
	}
}

// TestFixturesCoverTheCascades is not a decode check but a coverage check on
// the committed set. Without it the tool-free gate could quietly shrink to one
// filter chain and one depth: the level IS the cascade, so a fixture set that
// is all one level would leave four of the five untested on a machine with no
// encoder, and reversing the order the cascade comes off in would still pass.
func TestFixturesCoverTheCascades(t *testing.T) {
	levels, depths, channels := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for _, f := range apeFixtures {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		h, err := ape.ParseHeader(raw)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(f.path), err)
		}
		if h.CompressionLevel != f.level {
			t.Errorf("%s: level %d, the table says %d", filepath.Base(f.path), h.CompressionLevel, f.level)
		}
		levels[h.CompressionLevel] = true
		depths[h.BitsPerSample] = true
		channels[h.Channels] = true
		// The recorded deficit in docs/quality-gates.md says the 3950..3989
		// bitstream ships unfixtured because nothing can encode it. If that
		// ever stops being true, the doc is the thing to fix.
		if h.FileVersion < 3990 {
			t.Errorf("%s declares version %d: the unfixtured-versions deficit in "+
				"docs/quality-gates.md has closed, so delete it", filepath.Base(f.path), h.FileVersion)
		}
	}
	for _, level := range []int{1000, 2000, 3000, 4000, 5000} {
		if !levels[level] {
			t.Errorf("no committed fixture at level %d; its filter cascade is untested without an encoder", level)
		}
	}
	for _, depth := range []int{8, 16, 24} {
		if !depths[depth] {
			t.Errorf("no committed fixture at %d-bit; its sample packing is untested without an encoder", depth)
		}
	}
	if !channels[1] || !channels[2] {
		t.Errorf("committed fixtures cover channel counts %v, want both 1 and 2", channels)
	}
}

// TestSeekFixtureShape pins what the seek tests assume of their fixture: a
// regenerated one that quietly became single-frame would leave them passing on
// nothing.
func TestSeekFixtureShape(t *testing.T) {
	raw, err := os.ReadFile(repoPath("container", "apen", "testdata", "seek.ape"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.TotalFrames < 3 {
		t.Errorf("the seek fixture has %d frames; a seek test needs several", h.TotalFrames)
	}
	if h.FinalFrameBlocks >= h.BlocksPerFrame {
		t.Errorf("the seek fixture's last frame is full (%d blocks); a short one is what proves "+
			"the block count comes from the header", h.FinalFrameBlocks)
	}
	if h.Samples() != seekFixtureFrames {
		t.Errorf("the seek fixture holds %d samples, want %d", h.Samples(), seekFixtureFrames)
	}
}

// TestReleaseIsIdempotentAfterUse pins the Releaser contract: a decoder that
// borrowed a pooled buffer gives it back exactly once.
func TestReleaseIsIdempotentAfterUse(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "sine-s16.ape"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, pkts := frames(t, raw)
	dec, err := ape.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(pkts[0], func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	dec.Release()
	dec.Release()
}

// TestDamagedFrameDoesNotPoisonTheNextOne is the interim latch's failure mode.
// A 24-bit frame that fails its CRC is retried with the wider accumulators the
// 2019-to-2022 encoders used, and if that retry is latched unconditionally one
// damaged frame decodes every later one with the wrong arithmetic. The latch
// only sticks when the retry works.
func TestDamagedFrameDoesNotPoisonTheNextOne(t *testing.T) {
	raw, err := os.ReadFile(repoPath("codec", "ape", "testdata", "noise-c4000-s24.ape"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, pkts := frames(t, raw)
	if cfg.BitsPerSample != 24 {
		t.Fatalf("the interim path is 24-bit only; fixture is %d-bit", cfg.BitsPerSample)
	}
	dec, err := ape.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()

	damaged := append([]byte(nil), pkts[0]...)
	damaged[len(damaged)/2] ^= 0xff
	if err := dec.Decode(damaged, func(*audio.Buffer) error { return nil }); err == nil {
		t.Fatal("a corrupted frame decoded clean")
	}
	// The same decoder must still read an intact frame.
	if err := dec.Decode(pkts[0], func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatalf("an intact frame after a damaged one: %v", err)
	}
}

// TestFrameCRCFailureKeepsItsErrorShape pins what the interim retry keys on:
// the CRC failure is wrapped to carry the mismatch, and the wrapping must not
// cost it the error code or the container name callers see.
func TestFrameCRCFailureKeepsItsErrorShape(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "sine-s16.ape"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, pkts := frames(t, raw)
	dec, err := ape.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	damaged := append([]byte(nil), pkts[0]...)
	damaged[len(damaged)-16] ^= 0xff
	err = dec.Decode(damaged, func(*audio.Buffer) error { return nil })
	if err == nil {
		t.Fatal("a corrupted frame decoded clean")
	}
	if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v", code, waxerr.CodeUnsupportedFormat)
	}
	if !strings.HasPrefix(err.Error(), "ape: ") {
		t.Errorf("error %q lost its container name", err)
	}
}
