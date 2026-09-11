//go:build !wmaprotablesgen

package wmapro_test

// The state machine, which is where a codec whose frames span packets keeps
// everything that can go wrong.
//
// Nothing here is reachable from the corpus: Windows' encoder writes clean,
// gapless, in-order packets, so recovery from a discontinuity, a refusal that
// must not be fatal, a caller that gives up mid-frame and the trims no corpus
// frame carries all need streams built by hand.

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmapro"
	"github.com/colespringer/waxflow/waxerr"
)

// count decodes a run of packets and reports how many samples came out.
func count(t *testing.T, dec *wmapro.Decoder, pkts ...[]byte) (int, error) {
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

func newDec(t *testing.T, raw []byte) (*wmapro.Decoder, wmapro.Config) {
	t.Helper()
	cfg := parse(t, raw)
	dec, err := wmapro.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dec.Release)
	return dec, cfg
}

// silentPacket is one packet holding one silent frame.
func silentPacket(t *testing.T, cfg wmapro.Config, seq int) []byte {
	t.Helper()
	var w bitWriter
	silentFrame(cfg, &w)
	return packet(t, cfg, seq, 0, frame(cfg, &w, false, 0), 0)
}

// TestOneFrameOfLeadInPerDecodeStart pins the delay model: the first frame
// after a start is dropped and every frame after it is delivered on the call
// that completes it, so three packets of one frame each deliver two.
func TestOneFrameOfLeadInPerDecodeStart(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	got, err := count(t, dec, silentPacket(t, cfg, 0), silentPacket(t, cfg, 1), silentPacket(t, cfg, 2))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * cfg.SamplesPerFrame(); got != want {
		t.Fatalf("%d samples from three frames, want %d", got, want)
	}
	// A Reset starts over and owes the lead-in again.
	dec.Reset()
	got, err = count(t, dec, silentPacket(t, cfg, 0), silentPacket(t, cfg, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg.SamplesPerFrame() {
		t.Fatalf("%d samples after a reset, want one frame (%d)", got, cfg.SamplesPerFrame())
	}
}

// TestSequenceJumpOwesALeadIn gives the recovery its teeth. Packets were lost,
// so the frame the jumped packet begins overlap-adds against a tail the
// decoder does not have; it is dropped like any other first frame, and the
// one after it is delivered.
func TestSequenceJumpOwesALeadIn(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	got, err := count(t, dec,
		silentPacket(t, cfg, 0), silentPacket(t, cfg, 1),
		silentPacket(t, cfg, 7), silentPacket(t, cfg, 8))
	if err != nil {
		t.Fatalf("a sequence jump was reported as damage: %v", err)
	}
	// Frame 0 is the lead-in, frame 1 is delivered, the frame after the jump
	// is lead-in again, and the last is delivered. A decoder that ignored the
	// jump would deliver three.
	if want := 2 * cfg.SamplesPerFrame(); got != want {
		t.Fatalf("%d samples, want %d: the frame after the jump must be dropped", got, want)
	}
}

// TestADamagedPacketDoesNotLatch: every packet of this format is a decode
// start point, so a refused packet costs its own frames and a lead-in and
// nothing else. That is the opposite of codec/wma, and it is measured, not a
// preference.
func TestADamagedPacketDoesNotLatch(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	var bad bitWriter
	frameHead(cfg, &bad)
	bad.put(0, 1)
	bad.put(1, 1) // the reserved bit
	got := 0
	emit := func(b *audio.Buffer) error { got += b.N; return nil }
	if err := dec.Decode(silentPacket(t, cfg, 0), emit); err != nil {
		t.Fatal(err)
	}
	err := dec.Decode(packet(t, cfg, 1, 0, frame(cfg, &bad, false, 0), 0), emit)
	if err == nil || !strings.Contains(err.Error(), "reserved subframe bit") {
		t.Fatalf("error = %v, want the reserved-bit refusal", err)
	}
	for seq := 2; seq < 5; seq++ {
		if err := dec.Decode(silentPacket(t, cfg, seq), emit); err != nil {
			t.Fatalf("packet %d after a refusal: %v (the decoder latched)", seq, err)
		}
	}
	// Frame 0 was lead-in, frame 1 refused, frame 2 lead-in again, and
	// frames 3 and 4 delivered.
	if want := 2 * cfg.SamplesPerFrame(); got != want {
		t.Errorf("%d samples after the refusal, want %d", got, want)
	}
}

// TestHeaderRefusalsDoNotLatch: a refusal taken from the packet alone has
// consumed no frame bit. Nothing has drifted, and the next packet decodes.
func TestHeaderRefusalsDoNotLatch(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	got := 0
	emit := func(b *audio.Buffer) error { got += b.N; return nil }
	if err := dec.Decode(silentPacket(t, cfg, 0), emit); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(make([]byte, cfg.BlockAlign+1), emit); err == nil ||
		!strings.Contains(err.Error(), "longer than nBlockAlign") {
		t.Fatalf("error = %v, want the over-length refusal", err)
	}
	if err := dec.Decode(silentPacket(t, cfg, 1), emit); err != nil {
		t.Fatalf("the decoder latched on a header refusal: %v", err)
	}
	if got != cfg.SamplesPerFrame() {
		t.Errorf("%d samples, want the frame after the refusal (%d)", got, cfg.SamplesPerFrame())
	}
}

// TestEmitErrorDoesNotLatch: a callback failure is the caller's, not the
// stream's. It propagates unchanged and the decoder stays usable, at the cost
// of the frames the failed packet had not delivered and the lead-in after
// them, which is what a caller that carries on pays for a damaged packet too.
func TestEmitErrorDoesNotLatch(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	mine := errors.New("the caller stopped")
	if err := dec.Decode(silentPacket(t, cfg, 0), drop); err != nil {
		t.Fatal(err)
	}
	err := dec.Decode(silentPacket(t, cfg, 1), func(*audio.Buffer) error { return mine })
	if !errors.Is(err, mine) {
		t.Fatalf("error = %v, want the caller's own", err)
	}
	got, err := count(t, dec, silentPacket(t, cfg, 2), silentPacket(t, cfg, 3))
	if err != nil {
		t.Fatalf("the decoder latched a caller's error: %v", err)
	}
	if got != cfg.SamplesPerFrame() {
		t.Errorf("%d samples after the callback failed, want one lead-in and one frame (%d)", got, cfg.SamplesPerFrame())
	}
}

// TestAShortPacketDoesNotCarryItsTail: a packet shorter than nBlockAlign is
// a container shape rather than damage, but its tail is not the head of the
// frame the next packet continues, since the bits between it and the boundary
// are missing. Joining the two would decode bits that do not follow each
// other; the tail is dropped and a lead-in owed instead.
func TestAShortPacketDoesNotCarryItsTail(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	var silent bitWriter
	silentFrame(cfg, &silent)
	first, tail := splitFrame(cfg, &silent, 100)
	short := packet(t, cfg, 1, 0, first, 0)
	short = short[:len(short)-8]
	var next bitWriter
	next.append(tail)
	next.append(frame(cfg, &silent, true, 0))
	next.append(frame(cfg, &silent, false, 0))
	got, err := count(t, dec, silentPacket(t, cfg, 0), short, packet(t, cfg, 2, tail.bits, &next, 0))
	if err != nil {
		t.Fatalf("a short packet's dropped tail was reported as damage: %v", err)
	}
	// Frame 0 is lead-in; the short packet's whole frame is delivered; the
	// continued frame is lost with the tail; the frame after it is lead-in;
	// the last is delivered.
	if want := 2 * cfg.SamplesPerFrame(); got != want {
		t.Errorf("%d samples, want %d", got, want)
	}
}

// TestAnInconsistentCarryRecovers: the packet header's count says the carried
// frame ends here and the frame's own length says it does not. The frames
// that begin in the packet are still where the count says, so they decode;
// the inconsistent frame is the discontinuity and costs a lead-in.
func TestAnInconsistentCarryRecovers(t *testing.T) {
	dec, cfg := newDec(t, smallConfig())
	var silent bitWriter
	silentFrame(cfg, &silent)
	first, tail := splitFrame(cfg, &silent, 300)
	var next bitWriter
	for i := 0; i < 100; i++ { // a hundred of the three hundred bits owed
		next.put(uint32(tail.buf[i>>3]>>(7-uint(i&7))&1), 1)
	}
	next.append(frame(cfg, &silent, true, 0))
	next.append(frame(cfg, &silent, false, 0))
	got, err := count(t, dec, packet(t, cfg, 0, 0, first, 0), packet(t, cfg, 1, 100, &next, 0))
	if err != nil {
		t.Fatalf("an inconsistent carry was reported as damage: %v", err)
	}
	// Frame 0 is lead-in and the carried frame is dropped, which owes a
	// second lead-in; only the last frame is delivered. A decoder that
	// abandoned the packet would deliver nothing.
	if got != cfg.SamplesPerFrame() {
		t.Errorf("%d samples, want the packet's last frame (%d)", got, cfg.SamplesPerFrame())
	}
}

// TestTrimsAreApplied covers the two fields the corpus reaches only at the
// edges of a file: a start trim on the first frame, which the lead-in drop
// hides, and an end trim on the last. Here both sit on a delivered frame.
func TestTrimsAreApplied(t *testing.T) {
	cfg := parse(t, smallConfig())
	trimmed := func(start, end int) *bitWriter {
		var w bitWriter
		tilingOne(cfg, &w)
		w.put(0, 1) // no post-processing
		w.put(0, 8) // gain
		w.put(1, 1) // trims present
		w.put(1, 1)
		w.put(uint32(start), cfg.FrameLenBits()+1)
		w.put(1, 1)
		w.put(uint32(end), cfg.FrameLenBits()+1)
		silentSubframe(cfg, &w)
		return frame(cfg, &w, false, 0)
	}
	for _, tc := range []struct {
		name       string
		start, end int
		want       int
	}{
		{"both", 100, 50, 512 - 150},
		{"end-only", 0, 100, 512 - 100},
		{"whole-frame", 512, 0, 0},
		{"end-past-the-frame", 0, 864, 0},
		{"past-the-frame", 600, 600, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec, _ := newDec(t, smallConfig())
			got, err := count(t, dec, silentPacket(t, cfg, 0),
				packet(t, cfg, 1, 0, trimmed(tc.start, tc.end), 0))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("start %d end %d: %d samples, want %d", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

// TestNewDecoderRefusesAnUnvalidatedConfig: Config is public and a caller may
// build one by hand. Every geometry is computed from it, so an unchecked one
// would reach the band walk or the buffer pool with nothing to lay out.
func TestNewDecoderRefusesAnUnvalidatedConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  wmapro.Config
	}{
		{"zero", wmapro.Config{}},
		{"no-rate", wmapro.Config{Channels: 2, BitsPerSample: 16, BlockAlign: 4096}},
		{"no-block-align", wmapro.Config{Rate: 44100, Channels: 2, BitsPerSample: 16}},
		{"bad-depth", wmapro.Config{Rate: 44100, Channels: 2, BitsPerSample: 20, BlockAlign: 4096}},
		{"too-many-channels", wmapro.Config{Rate: 44100, Channels: 9, BitsPerSample: 16, BlockAlign: 4096}},
		// A block align no WAVEFORMATEX can carry, and a field width past
		// what the reader returns in one call.
		{"absurd-block-align", wmapro.Config{Rate: 44100, Channels: 2, BitsPerSample: 16, BlockAlign: 1 << 22}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wmapro.NewDecoder(tc.cfg, tc.cfg.Format()); err == nil {
				t.Fatal("accepted a config no ParseConfig would produce")
			} else if code := waxerr.CodeOf(err); code == waxerr.CodeInvalidRequest {
				t.Errorf("refused as %q rather than by the config's own check: %v", code, err)
			}
		})
	}
	// A hand-built config that IS valid works, which is the other half.
	cfg := wmapro.Config{Rate: 44100, Channels: 2, BitsPerSample: 16,
		Layout: audio.DefaultLayout(2), BlockAlign: 5945, DecodeFlags: testFlags}
	dec, err := wmapro.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("refused a valid hand-built config: %v", err)
	}
	dec.Release()
	// And a format that is not the config's own is a disagreement caught
	// here rather than downstream.
	other := cfg.Format()
	other.Rate = 48000
	if _, err := wmapro.NewDecoder(cfg, other); err == nil {
		t.Fatal("accepted a track format that is not the config's")
	}
}
