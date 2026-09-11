package wmalossless_test

// The state machine, which is where a codec whose frames span packets keeps
// everything that can go wrong.
//
// Nothing here is reachable from the corpus: Windows' encoder writes clean,
// gapless, in-order packets, so recovery from a discontinuity, a refusal that
// should not be fatal and a caller that gives up mid-frame all need streams
// built by hand.

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
	"github.com/colespringer/waxflow/waxerr"
)

// smallConfig is a short frame in a small packet, so a hand-built frame fits
// and the residual bodies stay legible.
func smallConfig() []byte { return config(16000, 2, 16, 0x03, 4096, testFlags) }

// codedFrame writes a frame of one subframe whose channels are all coded with
// nothing but zero residuals. A moving-average accumulator of zero makes the
// Golomb parameter zero, so every sample is a single terminating bit and the
// decode comes out silent, which is what makes these cells about the state
// machine rather than about arithmetic.
func codedFrame(cfg wmalossless.Config, w *bitWriter, seekable, more bool) {
	w.put(0, 1) // tile aligned
	for i := 0; i < cfg.Channels; i++ {
		w.put(1, 1)
	}
	w.put(uint32(cfg.MaxSubframes()-1), ratioBits(cfg.MaxSubframes()))
	w.put(0, 8) // gain
	w.put(0, 1) // no skips
	if seekable {
		w.put(1, 1) // seekable tile
		w.put(0, 4) // no arithmetic coding, no AC filter, no decorrelation, no MCLMS
		cdlmsDef(w, cfg.Channels)
		w.put(0, 3) // moving-average scaling
		w.put(0, 8) // quantisation step 1
	} else {
		w.put(0, 1)
	}
	w.put(0, 1)                                  // not raw PCM
	w.put(1<<uint(cfg.Channels)-1, cfg.Channels) // every channel coded
	w.put(0, 1)                                  // no LPC
	w.put(0, 1)                                  // no padding
	for c := 0; c < cfg.Channels; c++ {
		w.put(0, 1) // no transient
		first := 0
		if seekable {
			w.put(0, cfg.BitsPerSample) // the transmitted mean: zero
			w.put(0, cfg.BitsPerSample) // the first sample, a literal
			first = 1
		}
		for i := first; i < cfg.SamplesPerFrame(); i++ {
			w.put(0, 1) // a unary run of zero, so the residual is zero
		}
	}
	if more {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
}

// count decodes a run of packets and reports how many samples came out.
func count(t *testing.T, dec *wmalossless.Decoder, pkts ...[]byte) (int, error) {
	t.Helper()
	got := 0
	emit := func(b *audio.Buffer) error { got += b.N; return nil }
	for _, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			return got, err
		}
	}
	if err := dec.Drain(emit); err != nil {
		return got, err
	}
	return got, nil
}

func newDec(t *testing.T, raw []byte) (*wmalossless.Decoder, wmalossless.Config) {
	t.Helper()
	cfg, err := wmalossless.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	return dec, cfg
}

// TestSequenceJumpWaitsForASeekableTile gives the recovery its teeth.
//
// A sequence jump means packets were lost, so the filter state belongs to
// samples never seen and the decoder has to wait for a tile that restates it.
// Waiting is observable only when the frame after the jump is one that NEEDS
// state: a raw PCM tile decodes from nothing and would make "recover" and "do
// nothing" the same test.
func TestSequenceJumpWaitsForASeekableTile(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	defer dec.Release()
	var seek, plain bitWriter
	codedFrame(cfg, &seek, true, false)
	codedFrame(cfg, &plain, false, false)

	// In order: the seekable frame establishes the filters and the next frame
	// decodes from them.
	got, err := count(t, dec, packet(t, cfg, 0, &seek), packet(t, cfg, 1, &plain))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * cfg.SamplesPerFrame(); got != want {
		t.Fatalf("in order: %d samples, want %d", got, want)
	}

	// The same run with the third packet's sequence number jumped. The frames
	// waiting in the carry are missing whatever the lost packets held, and the
	// frame the jumped packet begins needs filter state the decoder must now
	// assume it does not have, so the run stops at the one frame that was
	// already complete. A decoder that did not recover would decode both of
	// them and deliver three frames.
	dec2, _ := newDec(t, smallConfig())
	defer dec2.Release()
	got, err = count(t, dec2,
		packet(t, cfg, 0, &seek), packet(t, cfg, 1, &plain), packet(t, cfg, 6, &plain))
	if err != nil {
		t.Fatalf("a sequence jump was reported as damage: %v", err)
	}
	if got != cfg.SamplesPerFrame() {
		t.Fatalf("after a jump: %d samples, want only the frame that was already complete (%d)",
			got, cfg.SamplesPerFrame())
	}
}

// TestHeaderRefusalsDoNotLatch: a refusal taken from the packet header alone
// has consumed no frame bit, so nothing has drifted. Latching it would kill a
// healthy decoder and take the frames still waiting in the carry with it.
func TestHeaderRefusalsDoNotLatch(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	defer dec.Release()
	var seek bitWriter
	codedFrame(cfg, &seek, true, false)

	got := 0
	emit := func(b *audio.Buffer) error { got += b.N; return nil }
	if err := dec.Decode(packet(t, cfg, 0, &seek), emit); err != nil {
		t.Fatal(err)
	}
	// A spliced packet, refused from its header.
	spliced := make([]byte, cfg.BlockAlign)
	spliced[0] = 0x14 // sequence 1, spliced
	err := dec.Decode(spliced, emit)
	if err == nil || !strings.Contains(err.Error(), "spliced") {
		t.Fatalf("error = %v, want the spliced refusal", err)
	}
	// A packet longer than nBlockAlign, refused the same way.
	if err := dec.Decode(make([]byte, cfg.BlockAlign+1), emit); err == nil ||
		!strings.Contains(err.Error(), "longer than nBlockAlign") {
		t.Fatalf("error = %v, want the over-length refusal", err)
	}
	// Neither latched: the frames of the first packet still drain.
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain after a header refusal: %v", err)
	}
	if got != cfg.SamplesPerFrame() {
		t.Errorf("%d samples drained, want the %d the first packet carried",
			got, cfg.SamplesPerFrame())
	}
}

// TestAShortPacketIsNotDamage: container/asf assembles compressed payloads
// independently of nBlockAlign, so a packet shorter than it is a container
// shape rather than a broken file. codec/wma refuses only an empty one.
func TestAShortPacketIsNotDamage(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	defer dec.Release()
	var seek bitWriter
	codedFrame(cfg, &seek, true, false)
	full := packet(t, cfg, 0, &seek)
	// Half a packet: the payload is shorter, so the frame in it simply spans
	// more of them. Nothing here may refuse it.
	if _, err := count(t, dec, full[:len(full)/2]); err != nil {
		t.Fatalf("a short packet was refused: %v", err)
	}
}

// TestEmitErrorDoesNotLatch: a callback failure is the caller's, not the
// stream's. It propagates unchanged, and the decoder stays usable.
func TestEmitErrorDoesNotLatch(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	defer dec.Release()
	var seek bitWriter
	codedFrame(cfg, &seek, true, false)
	mine := errors.New("the caller stopped")

	if err := dec.Decode(packet(t, cfg, 0, &seek), func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := dec.Drain(func(*audio.Buffer) error { return mine }); !errors.Is(err, mine) {
		t.Fatalf("drain error = %v, want the caller's own", err)
	}
	// Usable afterwards: a fresh seekable frame decodes.
	got, err := count(t, dec, packet(t, cfg, 1, &seek))
	if err != nil {
		t.Fatalf("the decoder latched a caller's error: %v", err)
	}
	if got != cfg.SamplesPerFrame() {
		t.Errorf("%d samples after the callback failed, want %d", got, cfg.SamplesPerFrame())
	}
}

// TestNewDecoderRefusesAnUnvalidatedConfig: Config is public and a caller may
// build one by hand. Every geometry is computed from it, so an unchecked one
// used to reach the buffer pool as a zero frame count and panic there.
func TestNewDecoderRefusesAnUnvalidatedConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  wmalossless.Config
	}{
		{"zero", wmalossless.Config{}},
		{"no-rate", wmalossless.Config{Channels: 2, BitsPerSample: 16, BlockAlign: 4096}},
		{"no-block-align", wmalossless.Config{Rate: 44100, Channels: 2, BitsPerSample: 16}},
		{"bad-depth", wmalossless.Config{Rate: 44100, Channels: 2, BitsPerSample: 20, BlockAlign: 4096}},
		{"too-many-channels", wmalossless.Config{Rate: 44100, Channels: 9, BitsPerSample: 16, BlockAlign: 4096}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wmalossless.NewDecoder(tc.cfg, tc.cfg.Format()); err == nil {
				t.Fatal("accepted a config no ParseConfig would produce")
			} else if code := waxerr.CodeOf(err); code == waxerr.CodeInvalidRequest {
				// A format complaint means the check happened downstream of
				// the config, which is the shape that used to panic instead.
				t.Errorf("refused as %q rather than by the config's own check: %v", code, err)
			}
		})
	}
	// A hand-built config that IS valid works, which is the other half: the
	// check must not simply refuse everything it did not parse itself.
	cfg := wmalossless.Config{Rate: 44100, Channels: 2, BitsPerSample: 16,
		Layout: audio.DefaultLayout(2), BlockAlign: 13375, DecodeFlags: testFlags}
	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("refused a valid hand-built config: %v", err)
	}
	dec.Release()
}

// TestStaleCarryAtZeroContinuationRecovers is the case the notes describe twice
// and inconsistently: a mid-stream continuation count of zero with a carry
// still open. See the comment at that branch. What must not happen is a
// permanently dead decoder, since the cause is packet loss.
func TestStaleCarryAtZeroContinuationRecovers(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	defer dec.Release()
	var seek, plain bitWriter
	codedFrame(cfg, &seek, true, false)
	codedFrame(cfg, &plain, false, false)

	// A packet whose payload is the front half of a frame, so the carry it
	// leaves is incomplete, followed by one that says nothing continues. The
	// sequence numbers are consecutive, which is what a loss of exactly
	// sixteen packets looks like.
	half := packet(t, cfg, 0, &plain)
	truncated := make([]byte, cfg.BlockAlign)
	copy(truncated, half[:len(half)/3])
	got := 0
	emit := func(b *audio.Buffer) error { got += b.N; return nil }
	if err := dec.Decode(packet(t, cfg, 0, &seek), emit); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(truncated, emit); err != nil {
		t.Fatalf("a truncated carry was reported as damage: %v", err)
	}
	// Whatever that produced, the decoder has to still be alive: a seekable
	// tile after it decodes.
	if err := dec.Decode(packet(t, cfg, 2, &seek), emit); err != nil {
		t.Fatalf("the decoder latched on a stale carry: %v", err)
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got < cfg.SamplesPerFrame() {
		t.Errorf("%d samples, want at least the frames either side of the gap", got)
	}
}
