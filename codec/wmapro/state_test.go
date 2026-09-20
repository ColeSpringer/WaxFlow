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

// longConfig is eight channels in a 32-byte packet: 2048-sample frames at a
// subframe depth of 16, so a frame of sixteen silent subframes with the
// post-processing matrix present is 644 bits against a 241-bit payload, and
// the length prefix cannot say so. That is the shape Windows' encoder writes
// for its final frame on some material (the committed long cell), where the
// prefix and the continuation count both saturate at the packet size.
func longConfig() []byte { return config(testRate, 8, 16, 0x63f, 32, testFlags, 0) }

// longSilentFrame writes the frame body: a uniform tiling of sixteen
// 128-sample subframes, the post-processing matrix present and all zero, and
// sixteen subframes in which no channel transmits.
func longSilentFrame(cfg wmapro.Config, w *bitWriter) {
	w.put(1, 1) // uniform tiling
	// Fifteen steps read a shift of 4 (a set bit, then 3 in the two-bit
	// field); the sixteenth sits at the last frontier and reads nothing.
	for step := 0; step < 15; step++ {
		w.put(1, 1)
		w.put(3, 2)
	}
	w.put(1, 1) // post-processing transform present
	w.put(1, 1) // its matrix present
	w.put(0, 4*cfg.Channels*cfg.Channels)
	if cfg.DecodeFlags&0x80 != 0 {
		w.put(0, 8)
	}
	w.put(0, 1) // no trim fields
	for i := 0; i < 16; i++ {
		silentSubframe(cfg, w)
	}
}

// clampedFrame is frame with the length prefix the encoder writes for a frame
// longer than a packet: the packet size in bits, whatever the frame's length.
func clampedFrame(cfg wmapro.Config, body *bitWriter, more bool) *bitWriter {
	return prefixed(cfg, uint32(cfg.BlockAlign*8), body, more, 0)
}

// slice returns bits [from, to) of w.
func slice(w *bitWriter, from, to int) *bitWriter {
	out := &bitWriter{}
	for i := from; i < to; i++ {
		out.put(uint32(w.buf[i>>3]>>(7-uint(i&7))&1), 1)
	}
	return out
}

// TestAFrameLongerThanAPacketDecodes pins the one frame whose extent is NOT
// known before it is decoded. Measured on Windows' encoder: a frame longer
// than a packet carries a length prefix of exactly nBlockAlign*8, and the
// packets it continues into carry a continuation count of the same value,
// whatever the frame's real length. The decoder can only find its end by
// walking it, and the walk is complete when the padding bit and the trailer
// bit follow it; until then the frame is accumulated, however many packets
// that takes, and an attempt that ran out of bits leaves no trace.
func TestAFrameLongerThanAPacketDecodes(t *testing.T) {
	cfg := parse(t, longConfig())
	var body bitWriter
	longSilentFrame(cfg, &body)
	long := clampedFrame(cfg, &body, false)
	var silent bitWriter
	silentFrame(cfg, &silent)
	lead := frame(cfg, &silent, true, 0)
	payload := cfg.BlockAlign*8 - 6 - cfg.FrameSizeBits()
	if long.bits <= 2*payload {
		t.Fatalf("the long frame is %d bits; it has to outrun two payloads of %d to prove anything", long.bits, payload)
	}
	head := payload - lead.bits
	var first bitWriter
	first.append(lead)
	first.append(slice(long, 0, head))
	clamp := cfg.BlockAlign * 8

	t.Run("saturated-counts-to-the-end", func(t *testing.T) {
		// Every continuation packet claims the whole payload, as the
		// encoder writes it; the frame ends partway through the third and
		// the rest of that packet is a stale copy the decoder never reads.
		dec, _ := newDec(t, longConfig())
		var second, third bitWriter
		second.append(slice(long, head, head+payload))
		third.append(slice(long, head+payload, long.bits))
		third.put(0xAAAAAAAA, min(32, payload-third.bits))
		got, err := count(t, dec,
			packet(t, cfg, 0, 0, &first, 0),
			packet(t, cfg, 1, clamp, &second, 0),
			packet(t, cfg, 2, clamp, &third, 0),
			silentPacket(t, cfg, 3))
		if err != nil {
			t.Fatalf("a frame longer than a packet was reported as damage: %v", err)
		}
		// Frame 0 is lead-in; the long frame and the last frame deliver.
		if want := 2 * cfg.SamplesPerFrame(); got != want {
			t.Errorf("%d samples, want %d", got, want)
		}
		if p := wmapro.PathsForTest(dec); p.LongFrame != 1 || p.Frames != 3 {
			t.Errorf("long frames %d of %d frames, want 1 of 3", p.LongFrame, p.Frames)
		}
	})

	t.Run("exact-count-then-frames", func(t *testing.T) {
		// The last continuation count is exact, and the frames that begin
		// after it in the same packet decode as usual. The long frame opens
		// the stream here so that the tail and a whole frame share a packet;
		// it is the lead-in, so only the frame after it is delivered, and
		// the counts say the long frame was decoded.
		dec, _ := newDec(t, longConfig())
		var one, two, three bitWriter
		one.append(slice(long, 0, payload))
		two.append(slice(long, payload, 2*payload))
		rest := long.bits - 2*payload
		three.append(slice(long, 2*payload, long.bits))
		three.append(frame(cfg, &silent, false, 0))
		got, err := count(t, dec,
			packet(t, cfg, 0, 0, &one, 0),
			packet(t, cfg, 1, clamp, &two, 0),
			packet(t, cfg, 2, rest, &three, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != cfg.SamplesPerFrame() {
			t.Errorf("%d samples, want the frame after the long one (%d)", got, cfg.SamplesPerFrame())
		}
		if p := wmapro.PathsForTest(dec); p.LongFrame != 1 || p.Frames != 2 {
			t.Errorf("long frames %d of %d frames, want 1 of 2", p.LongFrame, p.Frames)
		}
	})

	t.Run("truncated-is-not-damage", func(t *testing.T) {
		// The stream ends before the frame does: nothing is delivered for
		// it and nothing is refused, which is what a truncated file costs.
		dec, _ := newDec(t, longConfig())
		var second bitWriter
		second.append(slice(long, head, head+payload))
		got, err := count(t, dec, packet(t, cfg, 0, 0, &first, 0), packet(t, cfg, 1, clamp, &second, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Errorf("%d samples from a frame the stream never finished", got)
		}
		if p := wmapro.PathsForTest(dec); p.LongFrame != 0 || p.Frames != 1 {
			t.Errorf("long frames %d of %d frames, want 0 of 1: an incomplete attempt must leave no count behind", p.LongFrame, p.Frames)
		}
	})

	t.Run("short-continuation-packet-drops-the-frame", func(t *testing.T) {
		// A packet shorter than nBlockAlign under a saturating count: the
		// bits between its end and the boundary are missing, so the frame
		// cannot be finished from what follows. It goes the way of every
		// short packet's carry, dropped with a lead-in owed and no error.
		dec, _ := newDec(t, longConfig())
		var second, third bitWriter
		second.append(slice(long, head, head+payload))
		third.append(slice(long, head+payload, long.bits))
		short := packet(t, cfg, 1, clamp, &second, 0)
		short = short[:len(short)-4]
		got, err := count(t, dec,
			packet(t, cfg, 0, 0, &first, 0),
			short,
			packet(t, cfg, 2, clamp, &third, 0),
			silentPacket(t, cfg, 3),
			silentPacket(t, cfg, 4))
		if err != nil {
			t.Fatalf("a short packet under a long frame was reported as damage: %v", err)
		}
		// Frame 0 is lead-in, the long frame is lost with the short packet,
		// packet 3's frame is the lead-in that costs, and packet 4's
		// delivers.
		if got != cfg.SamplesPerFrame() {
			t.Errorf("%d samples, want one frame (%d)", got, cfg.SamplesPerFrame())
		}
		if p := wmapro.PathsForTest(dec); p.LongFrame != 0 {
			t.Errorf("%d long frames, want none: the frame was never completed", p.LongFrame)
		}
	})

	t.Run("damage-is-still-refused", func(t *testing.T) {
		// The bit that must be clear in the tenth subframe's head is set,
		// deep enough into the frame that the first attempt ran out of bits
		// before reaching it. The walk that reaches it has bits to spare, so
		// this is the frame and not the carry.
		var bad bitWriter
		longSilentFrame(cfg, &bad)
		// The subframe heads follow the fixed prefix: the tiling, the
		// post-processing matrix, the gain byte and the trim bit. The tenth
		// head is nine heads in, and its second bit is the reserved one.
		subHead := 2 + 1 + cfg.Channels + 1 + cfg.Channels
		at := 1 + 45 + 2 + 4*cfg.Channels*cfg.Channels + 8 + 1 + 9*subHead
		bad.buf[(at+1)>>3] |= 1 << (7 - uint((at+1)&7))
		badLong := clampedFrame(cfg, &bad, false)
		var one, two, three bitWriter
		one.append(lead)
		one.append(slice(badLong, 0, head))
		two.append(slice(badLong, head, head+payload))
		three.append(slice(badLong, head+payload, badLong.bits))
		dec, _ := newDec(t, longConfig())
		_, err := count(t, dec, packet(t, cfg, 0, 0, &one, 0), packet(t, cfg, 1, clamp, &two, 0), packet(t, cfg, 2, clamp, &three, 0))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("error = %v, want the subframe head's reserved bit named", err)
		}
	})
}
