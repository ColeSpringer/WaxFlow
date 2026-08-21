//go:build !wmatablesgen

package wma

// The decoder's cross-packet state machine, which is where this codec differs
// from the self-contained ones in this tree. The block-length walk, the
// exponent curve, the reservoir carry and the noise index all outlive a
// packet, so a packet is not a unit of recovery: nothing in the next one
// re-anchors a reader that drifted in this one. Everything below is about what
// happens at the seams -- a refusal, a seek, a packet that completes no frame
// -- rather than about decoding a well-formed stream, which the differentials
// cover.

import (
	"errors"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

func discard(*audio.Buffer) error { return nil }

// varBlockNoReservoir is the shape whose block-length walk nothing in the
// bitstream restates: selectors per block, but no reservoir to re-seed them
// per packet. Three block sizes, so selector value 3 is out of range.
func varBlockNoReservoir(t *testing.T) Config {
	t.Helper()
	c := Config{V2: true, Rate: 16000, Channels: 1, BitRate: 16000, BlockAlign: 64,
		Flags2: flagExpVLC | flagVarBlkLen | 3<<flagBlkDepthSh}
	if err := c.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// TestARefusalIsFinalUntilReset. A refused packet leaves the walk half
// updated, and the walk is what the block-size table is indexed by: the third
// selector's zero return used to stand as a block length, and zero is not one.
// A caller that read on past the error -- format.Media returns it from
// ReadChunk without latching, so one that retries does exactly that -- reached
// a 1-sample block and an index nine past a three-entry table.
func TestARefusalIsFinalUntilReset(t *testing.T) {
	cfg := varBlockNoReservoir(t)
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()

	// Selectors 0, 0, 3: the first two are in range and commit, the third is
	// not.
	bad := make([]byte, cfg.BlockAlign)
	bad[0] = 0x0c
	first := d.Decode(bad, discard)
	if first == nil {
		t.Fatal("a selector past the block-size table was accepted")
	}
	second := d.Decode(bad, discard)
	if !errors.Is(second, first) {
		t.Errorf("the second Decode returned %v, want the latched %v", second, first)
	}
	if err := d.Drain(discard); !errors.Is(err, first) {
		t.Errorf("Drain after a refusal returned %v, want the latched %v", err, first)
	}
}

// TestAGoodStreamSurvivesAResetAfterARefusal is the other half: the latch is
// released by Reset and only by Reset, so a seek past damage still plays.
func TestAGoodStreamSurvivesAResetAfterARefusal(t *testing.T) {
	cfg := synthConfig(t, flagExpVLC)
	n := cfg.FrameLen()
	good := func() ([]byte, int) {
		s := &synth{cfg: cfg}
		s.block(t, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
		return s.packet(t)
	}
	bad := func() []byte {
		s := &synth{cfg: cfg, expSym: 63} // walks the exponent index off the table
		s.block(t, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
		p, _ := s.packet(t)
		return p
	}
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()
	ok, _ := good()
	if err := d.Decode(ok, discard); err != nil {
		t.Fatalf("the good frame: %v", err)
	}
	refused := d.Decode(bad(), discard)
	if refused == nil {
		t.Fatal("an exponent index past the table was accepted")
	}
	if err := d.Decode(ok, discard); !errors.Is(err, refused) {
		t.Fatalf("a good packet after a refusal returned %v, want the latched %v", err, refused)
	}
	d.Reset()
	if err := d.Decode(ok, discard); err != nil {
		t.Errorf("after Reset the decoder is still refusing: %v", err)
	}
}

// TestResumeIsRefusedWhereTheWalkCannotBeRecovered. Reset re-arms the
// block-length walk, which is a guess everywhere and an impossible one here:
// this stream states its three lengths once, at the start of decoding, so
// re-arming mid-file reads two selectors that were never written and
// desynchronises every block after them. A named refusal beats a decode that
// is silently wrong from the landing to the end of the file.
func TestResumeIsRefusedWhereTheWalkCannotBeRecovered(t *testing.T) {
	cfg := varBlockNoReservoir(t)
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()
	// Selectors 0, 0, 0 then an uncoded full-length block: a legal frame.
	ok := make([]byte, cfg.BlockAlign)
	if err := d.Decode(ok, discard); err != nil {
		t.Fatalf("a legal frame from the start: %v", err)
	}
	d.Reset()
	err = d.Decode(ok, discard)
	if err == nil {
		t.Fatal("a resume was accepted on a stream whose walk cannot be recovered")
	}
	if !contains(err.Error(), "cannot resume") {
		t.Errorf("refusal %q does not name what it refuses", err)
	}

	// The same stream WITH a reservoir restates the walk on the first frame
	// that begins in each packet, so there a resume is honest.
	rcfg := cfg
	rcfg.Flags2 |= flagReservoir
	rd, err := NewDecoder(rcfg, rcfg.Format())
	if err != nil {
		t.Fatalf("new reservoir decoder: %v", err)
	}
	defer rd.Release()
	rd.Reset()
	if rd.noResume {
		t.Error("a reservoir stream was refused a resume it can recover from")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// resHeader writes a reservoir superframe header.
func resHeader(t *testing.T, w *synthBits, cfg Config, frames, bitOff int) {
	t.Helper()
	offBits, err := cfg.offsetBits()
	if err != nil {
		t.Fatal(err)
	}
	if bitOff >= 1<<(offBits+3) {
		t.Fatalf("bit offset %d does not fit %d bits", bitOff, offBits+3)
	}
	w.put(0, 4) // superframe index, which readers ignore
	w.put(uint32(frames), 4)
	w.put(uint32(bitOff), offBits+3)
}

// putBits copies n bits out of src starting at bit at.
func putBits(w *synthBits, src []byte, at, n int) {
	for ; n > 0; n-- {
		var bit uint32
		if at>>3 < len(src) && src[at>>3]>>(7-at&7)&1 != 0 {
			bit = 1
		}
		w.put(bit, 1)
		at++
	}
}

// resStream lays a run of synthetic frames into reservoir superframes sized so
// that every frame spans about three of them, which is what makes packets that
// complete no frame at all. It returns the packets and the frame count field
// each one states.
func resStream(t *testing.T, cfg Config, nFrames int) (pkts [][]byte, counts []int) {
	t.Helper()
	offBits, err := cfg.offsetBits()
	if err != nil {
		t.Fatal(err)
	}
	headerBits := 8 + offBits + 3

	// Every frame is one full-length block, so no selectors are written and
	// the frame's bits do not depend on where it lands.
	n := cfg.FrameLen()
	var all synthBits
	var ends []int
	for range nFrames {
		s := &synth{cfg: cfg}
		s.block(t, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
		putBits(&all, s.bw.b, 0, s.bw.n)
		ends = append(ends, all.n)
	}

	size := (ends[0]/3 + headerBits + 7) / 8
	payloadCap := size*8 - headerBits
	if payloadCap <= 8 {
		t.Fatalf("a %d-byte superframe leaves %d payload bits", size, payloadCap)
	}

	cursor, fi, lastEnd := 0, 0, 0
	for cursor < all.n {
		avail := min(payloadCap, all.n-cursor)
		end := cursor + avail
		carry := cursor > lastEnd
		done, bitOff := 0, 0
		if carry && fi < len(ends) && ends[fi] <= end {
			bitOff = ends[fi] - cursor
			lastEnd, fi, done = ends[fi], fi+1, done+1
		}
		for fi < len(ends) && ends[fi] <= end {
			lastEnd, fi, done = ends[fi], fi+1, done+1
		}
		f := done
		if !carry {
			f = done + 1
		}
		if f > 15 {
			t.Fatalf("frame count %d does not fit the 4-bit field", f)
		}
		var w synthBits
		resHeader(t, &w, cfg, f, bitOff)
		putBits(&w, all.b, cursor, avail)
		pkt := make([]byte, size)
		copy(pkt, w.b)
		pkts = append(pkts, pkt)
		counts = append(counts, f)
		cursor = end
	}
	return pkts, counts
}

// reservoirConfig is the synthetic config with the reservoir bit set. Packets
// are laid at a third of a frame below, so every frame spans three of them.
func reservoirConfig(t *testing.T) Config {
	t.Helper()
	c := synthConfig(t, flagExpVLC|flagReservoir)
	return c
}

// TestASeekOntoAContinuationPacketIsALanding. "Frame count zero with no carry
// pending is damage" is a rule about a LINEAR decode: it says the writer
// claimed a frame the reader never saw begin. After a seek the same shape is
// the ordinary case -- the decoder has no carry but the stream does -- and
// container/asf offers every media object as a landing, so bisection reaches
// one whenever a frame spans packets. Refusing it turned a file that plays
// into a file that will not seek.
func TestASeekOntoAContinuationPacketIsALanding(t *testing.T) {
	cfg := reservoirConfig(t)
	const frames = 6
	pkts, counts := resStream(t, cfg, frames)

	land := -1
	for i, f := range counts {
		if f == 0 && i > 0 {
			land = i
			break
		}
	}
	if land < 0 {
		t.Fatalf("no all-continuation packet in %d; the layout proves nothing", len(pkts))
	}

	// A used decoder, Reset where a seek would Reset it. Resetting a decoder
	// that has never decoded is not the same call: a fresh one already holds
	// everything Reset sets, so it cannot tell a working Reset from an empty
	// one.
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()
	for i := range land {
		if err := d.Decode(pkts[i], discard); err != nil {
			t.Fatalf("linear packet %d: %v", i, err)
		}
	}
	d.Reset()
	for i := land; i < len(pkts); i++ {
		if err := d.Decode(pkts[i], discard); err != nil {
			t.Fatalf("landing at packet %d, packet %d: %v", land, i, err)
		}
	}
	if err := d.Drain(discard); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// And the same shape at the start of a file is still damage: there the
	// writer really is claiming a frame that never began.
	fresh, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer fresh.Release()
	if err := fresh.Decode(pkts[land], discard); err == nil {
		t.Error("frame count zero with no carry was accepted at the start of a decode")
	}
}

// TestAFrameBegunInAContinuationPacketReArmsTheWalk. A packet that outputs
// nothing can still BEGIN a frame: frame count 1 with no carry pending is the
// legal shape for it. The frame is decoded a packet or more later, out of the
// carry, and by then the packet that began it is gone -- so the block-length
// reset it is owed has to be armed here, exactly as the path that decodes
// frames in place arms it.
//
// Reaching this from a linear stream needs the previous packet to have ended
// on its own last bit, which is a coincidence rather than something a layout
// can be built around. The walk state is set here instead, which is what that
// coincidence produces and nothing else about it matters.
func TestAFrameBegunInAContinuationPacketReArmsTheWalk(t *testing.T) {
	cfg := reservoirConfig(t)
	pkts, counts := resStream(t, cfg, 3)
	if counts[0] != 1 {
		t.Fatalf("the first packet states %d frames, want the 1 that begins one and completes none", counts[0])
	}
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()
	d.resetLens = false // as if the previous packet's frames ended on its last bit
	if err := d.decode(pkts[0], discard); err != nil {
		t.Fatalf("the packet that begins a frame: %v", err)
	}
	if !d.resetLens {
		t.Error("a frame began in this packet and the block-length walk was not re-armed for it")
	}
}

// TestTheCarryStopsAtItsLastPayloadBit. The carry is not byte aligned -- what
// a packet leaves unread stops mid-byte -- so the buffer holding it has up to
// seven padding bits past the payload. A reader whose length is the buffer's
// cannot tell them apart from payload, and this is the one reader in the tree
// whose whole contract is that an overrun reaches the caller: with no sync
// word, a walk that reads seven bits of padding instead of overrunning is a
// frame that silently means something else.
func TestTheCarryStopsAtItsLastPayloadBit(t *testing.T) {
	var w bitAppender
	src := []byte{0xff, 0xff, 0xff, 0xff, 0xff}
	const n = 21 // ends mid-byte, so the buffer carries three padding bits
	w.appendFrom(src, 0, n)
	if w.bits != n {
		t.Fatalf("the appender holds %d bits, want %d", w.bits, n)
	}
	if len(w.buf)*8 == n {
		t.Fatal("the carry is byte aligned; the test proves nothing")
	}

	var r bitReader
	r.resetBits(w.buf, w.bits)
	if got := r.bits(n); got != 1<<n-1 {
		t.Errorf("read back %#x, want %#x", got, 1<<n-1)
	}
	if r.err != nil {
		t.Fatalf("reading the payload overran: %v", r.err)
	}
	if r.bit(); r.err == nil {
		t.Error("a bit of padding past the carry was read as payload")
	}
}
