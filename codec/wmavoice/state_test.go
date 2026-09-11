//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
	"github.com/colespringer/waxflow/waxerr"
)

// What the corpus cannot reach, built by hand: the pulse schemes' two opposite
// sign polarities, the packet layer's flush, the sample-count trim, the frame
// counter a resumed decode owes, and the two paths Windows' encoder never
// writes.

// frameCode is the codeword for a frame type under defaultClasses, where a
// type's class is its index over three and its position within the class its
// index modulo three, so the symbol and the frame type coincide.
func frameCode(t testing.TB, ft int) (uint64, int) {
	t.Helper()
	_, runs := wmavoice.TypeRuns()
	for n, r := range runs {
		if r.Count > 0 && ft >= r.Symbol && ft < r.Symbol+r.Count {
			return r.First + uint64(ft-r.Symbol), n
		}
	}
	t.Fatalf("frame type %d has no codeword", ft)
	return 0, 0
}

// noPostfilter is a flags word with the postfilter off, 16 LSPs and a denoise
// strength the table has a row for. With the postfilter off the frame's output
// is the synthesis filter's verbatim, which is what makes the first sample of a
// decode equal to the first sample of its excitation and so makes a pulse's
// sign readable.
const noPostfilter uint32 = 1<<12 | 1<<15

// TestInnovationPulseSignIsPositive: in the innovation scheme a SET sign bit is
// +1. In both pitch-adaptive window schemes it is -1. The two are opposite and
// the note calls it the easiest thing in this codec to get backwards, so each
// gets a test that would fail on a negated pulse train.
//
// The first output sample is the first excitation sample exactly: at a decode
// start the synthesis filter's history is zero, so the recursion has nothing to
// subtract, and with the postfilter off nothing is between them.
func TestInnovationPulseSignIsPositive(t *testing.T) {
	for _, tc := range []struct {
		bit  uint64
		want float64
	}{{1, +1}, {0, -1}} {
		s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
		var w bitWriter
		w.put(0, 4)
		w.put(1, 1)
		w.put(1, 6) // one superframe, carried, so Drain emits it
		w.put(0, wmavoice.SpilloverBits(s.cfg))
		w.put(1, 1)
		w.put(0, 1)
		s.residualLSPs(&w)
		// Frame type 5: four blocks, the asymmetric codebook, innovation
		// pulses and no doubles. Its first pulse lands at 5*0+0, which is
		// sample zero of the frame.
		code, bits := frameCode(t, 5)
		w.put(code, bits)
		w.put(0, wmavoice.Geometry(s.cfg).PitchBits)
		for range 4 {
			for range 5 {
				w.put(tc.bit, 1)
				w.put(0, 3)
			}
			w.put(64, 7) // a middling gain index
		}
		s.silenceFrame(&w, 128)
		s.silenceFrame(&w, 128)
		w.put(0, 1)

		dec := s.decoder(t)
		got, err := decodeStream(t, dec, w.pad(s.align))
		dec.Release()
		if err != nil {
			t.Fatalf("sign bit %d: %v", tc.bit, err)
		}
		if len(got) == 0 {
			t.Fatalf("sign bit %d: nothing decoded", tc.bit)
		}
		if got[0] == 0 || math.Signbit(float64(got[0])) != math.Signbit(tc.want) {
			t.Errorf("sign bit %d gives a first sample of %v, want it %+.0f", tc.bit, got[0], tc.want)
		}
	}
}

// TestWindowPulseSignIsNegative is the same statement for the other scheme,
// where a set sign bit means the opposite. The pulse's position there depends
// on the window coordinates, so the test sweeps them for a frame whose first
// pulse lands on sample zero rather than asserting one.
func TestWindowPulseSignIsNegative(t *testing.T) {
	build := func(s stream, coord int, sign uint64) []byte {
		var w bitWriter
		w.put(0, 4)
		w.put(1, 1)
		w.put(1, 6)
		w.put(0, wmavoice.SpilloverBits(s.cfg))
		w.put(1, 1)
		w.put(0, 1)
		s.residualLSPs(&w)
		// Frame type 2: two blocks, the asymmetric codebook, window pulses.
		code, bits := frameCode(t, 2)
		w.put(code, bits)
		w.put(0, wmavoice.Geometry(s.cfg).PitchBits)
		w.put(uint64(coord), 6)
		for b := range 2 {
			// The first set: three pulses of four bits, most significant
			// first, each a sign bit and a three-bit index. All three carry
			// the same sign and index zero.
			field := uint64(0)
			for range 3 {
				field = field<<4 | sign<<3
			}
			w.put(field, 12)
			w.put(0, 5-2*b) // the second set's free-position index, narrower on block 1
			w.put(0, 1)     // its sign, which is + here
			w.put(64, 7)
		}
		s.silenceFrame(&w, 128)
		s.silenceFrame(&w, 128)
		w.put(0, 1)
		return w.pad(s.align)
	}
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	found := false
	for coord := range 54 {
		dec := s.decoder(t)
		set, err := decodeStream(t, dec, build(s, coord, 1))
		dec.Release()
		if err != nil || len(set) == 0 || set[0] == 0 {
			continue
		}
		dec = s.decoder(t)
		clear, err := decodeStream(t, dec, build(s, coord, 0))
		dec.Release()
		if err != nil || len(clear) == 0 || clear[0] == 0 {
			continue
		}
		found = true
		if !math.Signbit(float64(set[0])) {
			t.Errorf("coordinate %d: a SET sign bit gives %v, want a negative first sample", coord, set[0])
		}
		if math.Signbit(float64(clear[0])) {
			t.Errorf("coordinate %d: a clear sign bit gives %v, want a positive first sample", coord, clear[0])
		}
		break
	}
	if !found {
		t.Fatal("no window coordinate put a pulse on sample zero")
	}
}

// TestDrainEmitsTheCarriedSuperframe: the packet layer always leaves one
// superframe behind, so a decode that never drains is exactly one superframe
// short. That is not a rounding error, it is the last 480 samples of every
// file.
func TestDrainEmitsTheCarriedSuperframe(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	pkts := [][]byte{s.packet(0, 3), s.packet(0, 3)}
	dec := s.decoder(t)
	defer dec.Release()
	n := 0
	emit := func(b *audio.Buffer) error { n += b.N; return nil }
	for _, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	// Two packets of three superframes each: the first carries one into the
	// second, which then decodes three of its own and carries its last.
	if want := 5 * wmavoice.SuperframeSamples; n != want {
		t.Errorf("before the drain %d samples, want %d", n, want)
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatal(err)
	}
	if want := 6 * wmavoice.SuperframeSamples; n != want {
		t.Errorf("after the drain %d samples, want %d", n, want)
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatal(err)
	}
	if want := 6 * wmavoice.SuperframeSamples; n != want {
		t.Errorf("a second drain produced more samples (%d)", n)
	}
}

// TestSampleCountTrimsTheLastSuperframe: the optional twelve-bit count is the
// only thing that ever makes a superframe shorter, and it is the whole reason a
// decode comes out exactly as long as its source.
func TestSampleCountTrimsTheLastSuperframe(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	for _, n := range []int{1, 160, 183, 479, 480} {
		dec := s.decoder(t)
		got, err := decodeStream(t, dec, s.packet(0, 1, n))
		dec.Release()
		if err != nil {
			t.Fatalf("%d samples: %v", n, err)
		}
		if len(got) != n {
			t.Errorf("a superframe declaring %d samples emitted %d", n, len(got))
		}
	}
	// Zero is legal and emits nothing, which is not the same as a refusal.
	dec := s.decoder(t)
	defer dec.Release()
	got, err := decodeStream(t, dec, s.packet(0, 1, 0))
	if err != nil {
		t.Fatalf("a superframe declaring no samples: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a superframe declaring no samples emitted %d", len(got))
	}
}

// TestResetClearsEverything: a decoder run over real packets and then Reset
// must produce what a fresh one does. The decoder is DIRTIED first on purpose:
// a test that resets a fresh decoder tests nothing, and an empty Reset body
// would pass it.
func TestResetClearsEverything(t *testing.T) {
	track, pkts := demux(t, corpusPath(t, "voice-16000-16k"))
	fresh := decodeAll(t, track, pkts)

	dec := newDecoder(t, track)
	defer dec.Release()
	// Dirty it first. A test that resets a FRESH decoder tests nothing: an
	// empty Reset body would pass it. Four packets leave an excitation
	// history, a synthesis history, an LSF set, a pitch state, a gain
	// predictor, three postfilter memories and a carry behind; the frame
	// counter is moved separately, since Reset owns that too.
	emit := func(*audio.Buffer) error { return nil }
	for _, p := range pkts[:4] {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	dec.SetPosition(9999 * wmavoice.SuperframeSamples)
	dec.Reset()

	var got []float32
	keep := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
		return nil
	}
	for _, p := range pkts {
		if err := dec.Decode(p, keep); err != nil {
			t.Fatal(err)
		}
	}
	if err := dec.Drain(keep); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fresh) {
		t.Fatalf("after Reset %d samples, a fresh decoder gives %d", len(got), len(fresh))
	}
	for i := range got {
		if got[i] != fresh[i] {
			t.Fatalf("after Reset sample %d is %v, a fresh decoder gives %v", i, got[i], fresh[i])
		}
	}
}

// TestSetPositionDrivesTheComfortNoise: the comfort-noise codebook offset is a
// function of the frame index since the decoder started, so a resumed decode
// that starts counting at zero draws different noise in every silent passage,
// for good. SetPosition is what restores it, and this is what says the counter
// reaches the generator at all.
func TestSetPositionDrivesTheComfortNoise(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	pkt := s.packet(0, 1)
	at := func(sample int64) []float32 {
		dec := s.decoder(t)
		defer dec.Release()
		dec.Reset()
		dec.SetPosition(sample)
		got, err := decodeStream(t, dec, pkt)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	zero := at(0)
	same := at(0)
	for i := range zero {
		if zero[i] != same[i] {
			t.Fatalf("the same landing gives different noise at sample %d", i)
		}
	}
	// Three superframes on is nine frames on, which the generator sees.
	later := at(3 * wmavoice.SuperframeSamples)
	// A landing off the grid rounds to the nearest superframe: the container
	// lands on it for every real file, and the fallback for a file whose
	// first object is off it is a millisecond-derived time either side.
	for _, off := range []int64{-1, -239, 239} {
		near := at(3*wmavoice.SuperframeSamples + off)
		for i := range near {
			if near[i] != later[i] {
				t.Fatalf("a landing %d off the grid draws different noise at sample %d", off, i)
			}
		}
	}
	if len(later) != len(zero) {
		t.Fatalf("lengths %d and %d differ", len(later), len(zero))
	}
	differs := false
	for i := range zero {
		if later[i] != zero[i] {
			differs = true
			break
		}
	}
	if !differs {
		t.Error("a landing three superframes on draws the same comfort noise, so the counter never reached the generator")
	}
	// A negative landing is a caller's error, not a panic.
	at(-1)
}

// TestIndependentLSPPathDecodes covers the largest untested surface in the
// codec: with the packet header's residual flag clear, each frame carries its
// own LSP set instead of sharing the superframe's. Every packet of every file
// in this tree sets that flag, so this hand-built stream is the only thing that
// runs the path at all.
//
// DEFICIT: what it checks is that the path decodes and produces finite samples
// of the right count, not that its samples are right. Nothing in reach can say
// what they should be.
func TestIndependentLSPPathDecodes(t *testing.T) {
	for _, lsps := range []uint32{0, 1 << 12} {
		s := newStream(t, 16000, 450, noPostfilter|lsps, defaultClasses())
		var w bitWriter
		w.put(0, 4)
		w.put(0, 1) // independent, not residual
		w.put(1, 6)
		w.put(0, wmavoice.SpilloverBits(s.cfg))
		w.put(1, 1)
		w.put(0, 1)
		for range wmavoice.FramesPerSuperframe {
			// Each frame's own set, then the frame.
			if s.cfg.LSPs() == 10 {
				for _, n := range []int{8, 6, 5, 5} {
					w.put(0, n)
				}
			} else {
				for _, n := range []int{8, 6, 7, 6, 7} {
					w.put(0, n)
				}
			}
			s.silenceFrame(&w, 128)
		}
		w.put(0, 1)

		dec := s.decoder(t)
		got, err := decodeStream(t, dec, w.pad(s.align))
		p := dec.Paths()
		dec.Release()
		if err != nil {
			t.Fatalf("%d LSPs: %v", s.cfg.LSPs(), err)
		}
		if len(got) != wmavoice.SuperframeSamples {
			t.Errorf("%d LSPs: %d samples, want %d", s.cfg.LSPs(), len(got), wmavoice.SuperframeSamples)
		}
		if p.IndependentLSP != wmavoice.FramesPerSuperframe {
			t.Errorf("%d LSPs: %d frames took the independent path, want %d",
				s.cfg.LSPs(), p.IndependentLSP, wmavoice.FramesPerSuperframe)
		}
		if p.ResidualLSP != 0 {
			t.Errorf("%d LSPs: %d superframes took the residual path", s.cfg.LSPs(), p.ResidualLSP)
		}
		for i, v := range got {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("%d LSPs: sample %d is %v", s.cfg.LSPs(), i, v)
			}
		}
	}
}

// TestSuperframeCountEscape covers the other packet-layer path nothing reaches:
// a six-bit count field of 63 is followed by another, and the total is their
// sum. The largest count any real packet carries is 24.
func TestSuperframeCountEscape(t *testing.T) {
	s := newStream(t, 16000, 4096, noPostfilter, defaultClasses())
	var w bitWriter
	w.put(0, 4)
	w.put(1, 1)
	w.put(63, 6) // the escape
	w.put(2, 6)  // and the remainder, so 65 in total
	w.put(0, wmavoice.SpilloverBits(s.cfg))
	for range 65 {
		s.silentSuperframe(&w, -1)
	}
	dec := s.decoder(t)
	defer dec.Release()
	got, err := decodeStream(t, dec, w.pad(s.align))
	if err != nil {
		t.Fatalf("the escape was refused: %v", err)
	}
	if want := 65 * wmavoice.SuperframeSamples; len(got) != want {
		t.Errorf("%d samples, want %d", len(got), want)
	}
	if n := dec.Paths().CountEscape; n != 1 {
		t.Errorf("%d escapes counted, want 1", n)
	}
}

// TestCarriedSuperframeSpansTwoPackets is the packet layer's whole reason for
// existing: a superframe's bits can stop in one packet and finish in the next,
// and the carry that holds them is a BIT string whose length is not a multiple
// of eight. A byte-aligned carry would resume the superframe up to seven bits
// out, which is not a rounding error but a different superframe.
func TestCarriedSuperframeSpansTwoPackets(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	hdr := 4 + 1 + 6 + wmavoice.SpilloverBits(s.cfg)

	var one bitWriter
	s.silentSuperframe(&one, -1)
	sfBits := one.bits

	// The split lands where the first packet's own bytes end, which is what a
	// fixed-size packet does to a superframe that does not divide it.
	cut := 0
	for k := 1; k < sfBits; k++ {
		if (hdr+sfBits+k)%8 == 0 && k%8 != 0 {
			cut = k
			break
		}
	}
	if cut == 0 {
		t.Fatal("no split of this superframe lands off a byte boundary")
	}

	bitAt := func(w bitWriter, i int) uint64 { return uint64(w.buf[i>>3] >> uint(7-i&7) & 1) }

	var a bitWriter
	a.put(0, 4)
	a.put(1, 1)
	a.put(2, 6) // one superframe out of the body, one carried
	a.put(0, wmavoice.SpilloverBits(s.cfg))
	s.silentSuperframe(&a, -1)
	for i := range cut {
		a.put(bitAt(one, i), 1)
	}

	var b bitWriter
	b.put(0, 4)
	b.put(1, 1)
	b.put(2, 6)
	b.put(uint64(sfBits-cut), wmavoice.SpilloverBits(s.cfg))
	for i := cut; i < sfBits; i++ {
		b.put(bitAt(one, i), 1)
	}
	s.silentSuperframe(&b, -1)

	// The same three superframes with nothing split.
	var whole bitWriter
	whole.put(0, 4)
	whole.put(1, 1)
	whole.put(4, 6)
	whole.put(0, wmavoice.SpilloverBits(s.cfg))
	for range 3 {
		s.silentSuperframe(&whole, -1)
	}

	split := decodeNoDrain(t, s, a.buf, b.buf)
	ref := decodeNoDrain(t, s, whole.buf)
	if len(split) != len(ref) {
		t.Fatalf("the split stream gave %d samples, the whole one %d", len(split), len(ref))
	}
	if len(ref) != 3*wmavoice.SuperframeSamples {
		t.Fatalf("%d samples, want three superframes", len(ref))
	}
	for i := range ref {
		if split[i] != ref[i] {
			t.Fatalf("sample %d differs across the split: %v against %v", i, split[i], ref[i])
		}
	}
}

// decodeNoDrain runs packets through a fresh decoder without the flush, which
// hand-built packets need: their padding travels in the final carry and is not
// a superframe.
func decodeNoDrain(t testing.TB, s stream, pkts ...[]byte) []float32 {
	t.Helper()
	dec := s.decoder(t)
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
		return nil
	}
	for i, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	return got
}

// TestRunawaySynthesisIsRefused: the adaptive codebook is a gain times a copy
// of the decoder's own past excitation and the gain table reaches 1.36, so a
// stream that holds the top gain for long enough is a recursion with no bound
// but float32's. The reference emits whatever that makes. This decoder refuses
// the superframe the moment a sample leaves MaxMagnitude, drops the histories
// the runaway poisoned, and decodes the next packet as audio again. The fuzzer
// reached the same end through a crafted LSF set whose float32 LPC the
// synthesis filters cannot hold stable (testdata/fuzz/FuzzDecode); the guard is
// the same for both.
func TestRunawaySynthesisIsRefused(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	g := wmavoice.Geometry(s.cfg)
	code, bits := frameCode(t, 8) // two blocks, Hamming codebook, innovation pulses

	// Every block at the shortest pitch and gain index 125, where the adaptive
	// codebook gain table peaks.
	runaway := func(w *bitWriter) {
		w.put(1, 1) // speech
		w.put(0, 1) // the full 480 samples
		s.residualLSPs(w)
		for range wmavoice.FramesPerSuperframe {
			w.put(code, bits)
			for b := range 2 {
				if b == 0 {
					w.put(0, g.BlockPitchBits) // conversion arm 0: lag minPitch, phase 0
				} else {
					w.put(0, g.DeltaPitchBits) // a delta of minus the half-range: the same lag
				}
				// Five positive pulses in the block's last five positions, which
				// the copy at a lag of half the block carries into the next
				// block's tail; a pulse in the head would die with its block.
				for range 5 {
					w.put(1, 1)
					w.put(15, 4)
				}
				w.put(125, 7)
			}
		}
		w.put(0, 1) // no statistics block
	}
	const perPacket = 8
	packet := func() []byte {
		var w bitWriter
		w.put(0, 4)
		w.put(1, 1) // residual LSPs
		w.put(perPacket, 6)
		w.put(0, wmavoice.SpilloverBits(s.cfg))
		for range perPacket {
			runaway(&w)
		}
		return w.pad(s.align)
	}

	dec := s.decoder(t)
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
		return nil
	}
	var err error
	pkts := 0
	for pkts < 12 && err == nil {
		err = dec.Decode(packet(), emit)
		pkts++
	}
	peak := 0.0
	for _, v := range got {
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	if err == nil {
		t.Fatalf("%d packets of a runaway decoded without a refusal, peaking at %v", pkts, peak)
	}
	if !strings.Contains(err.Error(), "diverged") || waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Fatalf("refused with %v, want the divergence named as malformed input", err)
	}
	// The refused superframe was decoded in full, so it counts: the frame
	// counter stands where a decode that emitted it would stand.
	if want := wmavoice.FramesPerSuperframe * (len(got)/wmavoice.SuperframeSamples + 1); dec.FrameCounter() != want {
		t.Fatalf("frame counter %d after the refusal, want %d", dec.FrameCounter(), want)
	}
	// Everything emitted before the refusal stayed inside the bound, and had
	// grown well past full scale: the refusal is the runaway's, not an early
	// one for some other reason.
	for _, v := range got {
		if !(math.Abs(float64(v)) <= wmavoice.MaxMagnitude) {
			t.Fatalf("an emitted sample of %v is past the bound", v)
		}
	}
	if peak < 1 {
		t.Fatalf("the emitted run peaks at %v: the refusal came before the runaway", peak)
	}
	// The next packet decodes as audio. With the poisoned histories kept, the
	// synthesis filter would ring on from the runaway's last samples instead.
	got = got[:0]
	if err := dec.Decode(s.packet(0, 2), emit); err != nil {
		t.Fatalf("the packet after the refusal: %v", err)
	}
	if len(got) != wmavoice.SuperframeSamples {
		t.Fatalf("the packet after the refusal produced %d samples, want a superframe", len(got))
	}
	after := 0.0
	for _, v := range got {
		after = math.Max(after, math.Abs(float64(v)))
	}
	if after > 1 {
		t.Fatalf("the silent superframe after the refusal peaks at %v", after)
	}
}

// TestAHoleCostsOnlyItsPacket: a refused or over-long media object is a hole,
// and the carry the packet before it left behind is the head of a superframe
// whose tail went with the hole. Kept, that carry is joined with the NEXT
// packet's spillover bits, which finish a different superframe, and the
// splice reads as damage in the packet after the hole or, worse, as audio.
// The carry goes with the hole; what follows is a resume, converging as a
// seek does, and nothing after the hole is refused.
func TestAHoleCostsOnlyItsPacket(t *testing.T) {
	track, pkts := demux(t, corpusPath(t, "voice-16000-16k"))
	ref := decodeAll(t, track, pkts)
	total := (len(ref) + wmavoice.SuperframeSamples - 1) / wmavoice.SuperframeSamples

	// during[k] is how many superframes a linear decode emits while it
	// decodes packet k: the one packet k finishes, then its own body.
	during := make([]int, len(pkts))
	{
		dec := newDecoder(t, track)
		defer dec.Release()
		for k, p := range pkts {
			n := 0
			if err := dec.Decode(p, func(*audio.Buffer) error { n++; return nil }); err != nil {
				t.Fatalf("packet %d: %v", k, err)
			}
			during[k] = n
		}
	}

	for _, k := range []int{2, 5, 7} {
		t.Run(fmt.Sprintf("an over-long object at packet %d", k), func(t *testing.T) {
			dec := newDecoder(t, track)
			defer dec.Release()
			var got []float32
			emit := func(b *audio.Buffer) error {
				got = append(got, b.ChanF(0)[:b.N]...)
				return nil
			}
			for i, p := range pkts {
				if i == k {
					err := dec.Decode(make([]byte, len(p)+1), emit)
					if err == nil || !strings.Contains(err.Error(), "longer than nBlockAlign") {
						t.Fatalf("the over-long object: %v", err)
					}
					continue
				}
				if err := dec.Decode(p, emit); err != nil {
					t.Fatalf("packet %d, after the hole: %v", i, err)
				}
			}
			if err := dec.Drain(emit); err != nil {
				t.Fatalf("drain: %v", err)
			}
			// Lost: everything packet k would have emitted, and the
			// superframe it carried into packet k+1, whose spillover bits
			// are skipped unread because nothing is there to finish.
			lost := during[k] + 1
			if want := len(ref) - lost*wmavoice.SuperframeSamples; len(got) != want {
				t.Fatalf("decoded %d samples, want the linear %d less the hole's %d superframes (%d)",
					len(got), len(ref), lost, want)
			}
			before := 0
			for _, n := range during[:k] {
				before += n
			}
			at := before * wmavoice.SuperframeSamples
			if total-(before+lost) < seekTail {
				return
			}
			if convergedAfter(got[at:], ref[at+lost*wmavoice.SuperframeSamples:]) < 0 {
				t.Errorf("the decode after the hole never matches the linear one")
			}
		})
	}
}

// TestAnEmptyPacketFlushesTheCarry: the reference's own end-of-stream is a
// call with an empty packet, which decodes the carried superframe with no
// spillover. An empty packet here does the same, at any point, so the carry
// never lives on across one: what it holds is emitted if it is complete and
// refused if it is not, and the next packet's spillover is skipped either way.
func TestAnEmptyPacketFlushesTheCarry(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	want := decodeNoDrain(t, s, s.packet(0, 2))
	wantFlush, err := decodeStream(t, s.decoder(t), s.packet(0, 2))
	if err != nil {
		t.Fatal(err)
	}

	dec := s.decoder(t)
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
		return nil
	}
	if err := dec.Decode(s.packet(0, 2), emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("the packet itself gave %d samples, want %d", len(got), len(want))
	}
	if err := dec.Decode(nil, emit); err != nil {
		t.Fatalf("the empty packet: %v", err)
	}
	if len(got) != len(wantFlush) {
		t.Fatalf("after the empty packet %d samples, a drain gives %d", len(got), len(wantFlush))
	}
	for i := range got {
		if got[i] != wantFlush[i] {
			t.Fatalf("sample %d is %v flushed by an empty packet and %v by Drain", i, got[i], wantFlush[i])
		}
	}
	// Nothing is left to flush, twice over.
	if err := dec.Decode(nil, emit); err != nil || len(got) != len(wantFlush) {
		t.Fatalf("a second empty packet: %v, %d samples", err, len(got))
	}
	if err := dec.Drain(emit); err != nil || len(got) != len(wantFlush) {
		t.Fatalf("a drain after the flush: %v, %d samples", err, len(got))
	}
}

// TestSuperframeCountZeroIsRefused: the count is one more than the body
// yields, since the last superframe always becomes the carry, so zero
// describes no packet. Accepted, it would put the whole body into the carry
// and decode one superframe of it at the top of the next packet, dropping the
// rest without a word.
func TestSuperframeCountZeroIsRefused(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	p := s.packet(0, 2)
	// The count is the six bits after the four-bit sequence number and the
	// one-bit LSP flag.
	p[0] &^= 0x07
	p[1] &^= 0xE0
	_, err := decodeStream(t, s.decoder(t), p)
	if err == nil || !strings.Contains(err.Error(), "count") || waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Fatalf("error %v, want the zero count named as malformed input", err)
	}
}

// TestTruncationAtASuperframeIsDamage: a count that claims a superframe the
// packet does not hold makes the walk start one at the end of the payload.
// The first bit it reads there is an overrun, which is damage, and it must
// not come back as the WMA Pro refusal a clear speech bit means, which is an
// "unsupported" answer with a different exit code.
func TestTruncationAtASuperframeIsDamage(t *testing.T) {
	s := newStream(t, 16000, 450, noPostfilter, defaultClasses())
	var w bitWriter
	w.put(0, 4)
	w.put(1, 1)
	w.put(3, 6) // three superframes claimed
	w.put(0, wmavoice.SpilloverBits(s.cfg))
	// One superframe with its sample-count field, which brings the packet to
	// exactly 128 bits: the second body superframe then starts at the end of
	// the payload with nothing to read, not even padding.
	s.silentSuperframe(&w, wmavoice.SuperframeSamples)
	if w.bits != 128 {
		t.Fatalf("the packet is %d bits, the test needs a byte-aligned 128", w.bits)
	}
	_, err := decodeStream(t, s.decoder(t), w.buf)
	if err == nil || !strings.Contains(err.Error(), "past the end") || waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Fatalf("error %v, want the overrun named as malformed input", err)
	}
}
