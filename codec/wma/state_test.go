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
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
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

// TestPacketFramesCountsWhatThePacketCompletes. A seek positions a landing by
// counting frames from the packet headers: one per packet without the
// reservoir, and with it the superframe's frame count, the low nibble of the
// first byte. The high nibble is the superframe index, which readers ignore.
func TestPacketFramesCountsWhatThePacketCompletes(t *testing.T) {
	plain := synthConfig(t, flagExpVLC)
	res := reservoirConfig(t)
	for _, c := range []struct {
		name   string
		cfg    Config
		pkt    []byte
		frames int
		ok     bool
	}{
		{"one per packet without the reservoir", plain, []byte{0x3f, 0x00}, 1, true},
		{"an empty packet without the reservoir", plain, nil, 0, false},
		{"count zero", res, []byte{0x30, 0xff}, 0, true},
		{"count fifteen", res, []byte{0x3f}, 15, true},
		{"count one", res, []byte{0x01}, 1, true},
		{"an empty reservoir packet", res, []byte{}, 0, false},
	} {
		frames, ok := c.cfg.PacketFrames(c.pkt)
		if frames != c.frames || ok != c.ok {
			t.Errorf("%s: PacketFrames(% x) = %d, %v, want %d, %v", c.name, c.pkt, frames, ok, c.frames, c.ok)
		}
	}
}

// shortHeadStream lays synthetic frames into reservoir superframes the way
// Windows' encoder counts them, each frame in the packet it begins in, so that
// the second packet finishes the frame the first left open and begins the
// next in its last six bits. quiet is how many one-bit uncoded frames follow
// the first; they exist to make both packets whole bytes.
func shortHeadStream(t *testing.T, cfg Config) (pkts [][]byte, quiet int) {
	t.Helper()
	offBits, err := cfg.offsetBits()
	if err != nil {
		t.Fatal(err)
	}
	hb := 8 + offBits + 3
	n := cfg.FrameLen()
	frame := func(coded bool) synthBits {
		s := &synth{cfg: cfg}
		s.block(t, blockSpec{len: n, next: n, coded: [2]bool{coded, coded}, transmit: true})
		return s.bw
	}
	full, q := frame(true), frame(false)
	if q.n != 1 {
		t.Fatalf("an uncoded frame is %d bits, want 1", q.n)
	}
	const head = 6
	b := full.n
	for (2*hb+2*b+quiet+head)%8 != 0 {
		quiet++
	}
	h1 := 8*((hb+b+quiet+8)/8) - hb - b - quiet // the second frame's head in packet 0
	// Every packet but the last must end on a byte: a reader carries
	// whatever follows a packet's last frame, padding included.
	pack := func(last bool, frames, bitOff int, parts ...func(w *synthBits)) []byte {
		var w synthBits
		resHeader(t, &w, cfg, frames, bitOff)
		for _, p := range parts {
			p(&w)
		}
		if w.n%8 != 0 && !last {
			t.Fatalf("packet %d is %d bits, not whole bytes", len(pkts), w.n)
		}
		return append([]byte(nil), w.b...)
	}
	bits := func(src synthBits, at, n int) func(w *synthBits) {
		return func(w *synthBits) { putBits(w, src.b, at, n) }
	}
	quietRun := func(w *synthBits) {
		for range quiet {
			putBits(w, q.b, 0, 1)
		}
	}
	pkts = append(pkts, pack(false, quiet+2, 0, bits(full, 0, b), quietRun, bits(full, 0, h1)))
	pkts = append(pkts, pack(false, 1, b-h1, bits(full, h1, b-h1), bits(full, 0, head)))
	pkts = append(pkts, pack(true, 2, b-head, bits(full, head, b-head), bits(full, 0, b), bits(full, 0, b)))
	return pkts, quiet
}

// TestResumedDecodeCarriesAShortHead. A frame that begins in its packet's last
// byte is an ordinary carry out in a linear decode. A seek can land on that
// packet: it finishes a frame the decoder never saw begin and begins the next
// in a head of eight bits or fewer, which a linear decode refuses as an
// all-continuation packet with no payload. After a seek that refusal turned a
// legal landing into a seek that fails, and container/asf offers every packet
// as one. The landing decodes the frame the head begins as its lead-in, so it
// emits from the frame after it.
func TestResumedDecodeCarriesAShortHead(t *testing.T) {
	cfg := reservoirConfig(t)
	pkts, quiet := shortHeadStream(t, cfg)
	if f, _ := cfg.PacketFrames(pkts[1]); f != 1 {
		t.Fatalf("the landing packet states %d frames, want 1", f)
	}

	collect := func(dst *[]float32) func(*audio.Buffer) error {
		return func(b *audio.Buffer) error {
			*dst = append(*dst, b.ChanF(0)[:b.N]...)
			return nil
		}
	}
	var linear []float32
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()
	for i, p := range pkts {
		if err := d.Decode(p, collect(&linear)); err != nil {
			t.Fatalf("linear packet %d: %v", i, err)
		}
	}
	if err := d.Drain(collect(&linear)); err != nil {
		t.Fatalf("linear drain: %v", err)
	}

	// A used decoder, Reset where a seek would Reset it.
	s, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer s.Release()
	if err := s.Decode(pkts[0], discard); err != nil {
		t.Fatalf("run-up: %v", err)
	}
	s.Reset()
	var got []float32
	for i := 1; i < len(pkts); i++ {
		if err := s.Decode(pkts[i], collect(&got)); err != nil {
			t.Fatalf("landing at packet 1, packet %d: %v", i, err)
		}
	}
	if err := s.Drain(collect(&got)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Packet 0 began quiet+2 frames, so the landing emits from there.
	at := (quiet + 2) * cfg.FrameLen()
	if len(got) == 0 || len(got) != len(linear)-at {
		t.Fatalf("the landing emitted %d samples, the linear decode has %d past sample %d", len(got), len(linear)-at, at)
	}
	for i := range got {
		if got[i] != linear[at+i] {
			t.Fatalf("sample %d past the landing is %v, the linear decode has %v", i, got[i], linear[at+i])
		}
	}

	// At the start of a decode the same packet is still damage: nothing
	// came before it to finish.
	fresh, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer fresh.Release()
	if err := fresh.Decode(pkts[1], discard); err == nil {
		t.Error("a frame head of six bits was accepted at the start of a decode")
	}
	// A decode Reset mid-stream is a resumed one whatever it is told of its
	// position: after a hole a stamp can read 0 well into a file.
	atZero, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer atZero.Release()
	if err := atZero.Decode(pkts[0], discard); err != nil {
		t.Fatalf("run-up: %v", err)
	}
	atZero.Reset()
	if p, ok := any(atZero).(codec.Positioner); ok {
		p.SetPosition(0)
	}
	if err := atZero.Decode(pkts[1], discard); err != nil {
		t.Errorf("a decode resumed and told it is at sample 0 refused the head: %v", err)
	}
	// Restart, which a caller uses for a landing on the stream's first
	// packet, is a linear decode again.
	atStart, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer atStart.Release()
	if err := atStart.Decode(pkts[0], discard); err != nil {
		t.Fatalf("run-up: %v", err)
	}
	atStart.Restart()
	if err := atStart.Decode(pkts[1], discard); err == nil {
		t.Error("a frame head of six bits was accepted by a restarted decode")
	}
}

// layStarts lays frames back to back into reservoir superframes of payload
// bits each, stating every packet's frame count as Windows' encoder does: the
// frames that begin in it. at[i] is the packet frame i begins in.
func layStarts(t *testing.T, cfg Config, frames []synthBits, payload int) (pkts [][]byte, at []int) {
	t.Helper()
	offBits, err := cfg.offsetBits()
	if err != nil {
		t.Fatal(err)
	}
	if (8+offBits+3+payload)%8 != 0 {
		t.Fatalf("a %d-bit payload does not end a packet on a byte", payload)
	}
	var all synthBits
	var starts, ends []int
	for _, f := range frames {
		starts = append(starts, all.n)
		putBits(&all, f.b, 0, f.n)
		ends = append(ends, all.n)
	}
	for cursor := 0; cursor < all.n; cursor += payload {
		end := cursor + payload
		count, bitOff := 0, 0
		for i := range frames {
			if starts[i] >= cursor && starts[i] < end {
				count++
				at = append(at, len(pkts))
			}
			// A packet the frame only passes through states no offset.
			if starts[i] < cursor && ends[i] > cursor && ends[i] <= end {
				bitOff = ends[i] - cursor
			}
		}
		var w synthBits
		resHeader(t, &w, cfg, count, bitOff)
		putBits(&w, all.b, cursor, min(payload, all.n-cursor))
		pkt := make([]byte, (8+offBits+3+payload)/8)
		copy(pkt, w.b)
		pkts = append(pkts, pkt)
	}
	return pkts, at
}

// TestAResumeMissingACurveAsksForHistory. A block may reuse a channel's
// exponents from the last block that coded the channel, however far back
// that was, and nothing restates them. A decode resumed between the two has
// no curve to reuse. A linear decode that meets the same shape is reading
// damage and refuses it; after a seek it is history the landing did not
// carry, and the decoder says so with codec.ErrNeedsHistory, so the caller
// can land earlier rather than play a wrong channel. container/asf lands an
// object earlier for exactly this, and from there the block is exact.
func TestAResumeMissingACurveAsksForHistory(t *testing.T) {
	cfg := Config{V2: true, Rate: 44100, Channels: 2, BitRate: 128000,
		Flags2: flagExpVLC | flagVarBlkLen | flagReservoir}
	cfg.BlockAlign = cfg.BitRate * cfg.FrameLen() / (cfg.Rate * 8)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, noise := cfg.highFreqMult(); noise {
		t.Fatal("noise coding is on; the frames below do not write its fields")
	}
	n := cfg.FrameLen()
	frame := func(blocks ...blockSpec) synthBits {
		s := &synth{cfg: cfg}
		for _, b := range blocks {
			s.block(t, b)
		}
		return s.bw
	}
	both, mid := [2]bool{true, true}, [2]bool{true, false}
	frames := []synthBits{
		frame(blockSpec{len: n, next: n, coded: both, transmit: true}),
		frame(blockSpec{len: n, next: n, coded: both, transmit: true}), // the run-up: sends channel 1 its curve
		frame(blockSpec{len: n, next: n, coded: mid, transmit: true}),  // the landing: channel 1 uncoded
		frame(blockSpec{len: n / 2, next: n / 2, coded: both}, blockSpec{len: n / 2, next: n, coded: both}),
		frame(blockSpec{len: n, next: n, coded: both, transmit: true}),
		frame(blockSpec{len: n, next: n, coded: both, transmit: true}),
	}
	payload := frames[0].n
	for _, f := range frames {
		payload = min(payload, f.n)
	}
	offBits, _ := cfg.offsetBits()
	payload -= 8 + (8+offBits+3+payload)%8 // every frame spans two packets
	pkts, at := layStarts(t, cfg, frames, payload)
	if at[1] == at[0] || at[2] == at[1] {
		t.Fatal("two frames begin in one packet; the landings below would carry the curve")
	}

	collect := func(dst *[2][]float32) func(*audio.Buffer) error {
		return func(b *audio.Buffer) error {
			for ch := range 2 {
				dst[ch] = append(dst[ch], b.ChanF(ch)[:b.N]...)
			}
			return nil
		}
	}
	var linear [2][]float32
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	for i, p := range pkts {
		if err := d.Decode(p, collect(&linear)); err != nil {
			t.Fatalf("linear packet %d: %v", i, err)
		}
	}
	// The linear decode's own lead-in is the first frame, so the reusing
	// frame, the fourth, is its third emitted.
	if len(linear[0]) < 3*n {
		t.Fatalf("the linear decode emitted %d samples", len(linear[0]))
	}
	reuse := [2][]float32{linear[0][2*n : 3*n], linear[1][2*n : 3*n]}

	// resume decodes from packet land with a decoder that has decoded the
	// whole stream first, a seek resetting a decoder with any history at all,
	// or restarting it for a landing on the stream's first packet. It returns
	// what was emitted and the first error.
	resume := func(land int, restart bool) ([2][]float32, error) {
		r, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		defer r.Release()
		for i := range pkts {
			if err := r.Decode(pkts[i], discard); err != nil {
				t.Fatalf("run-up packet %d: %v", i, err)
			}
		}
		if restart {
			r.Restart()
		} else {
			r.Reset()
		}
		var got [2][]float32
		for i := land; i < len(pkts); i++ {
			if err := r.Decode(pkts[i], collect(&got)); err != nil {
				return got, err
			}
		}
		return got, nil
	}

	// At the landing the third frame is the lead-in, and the reusing frame
	// has no curve for channel 1: the decode asks for history and plays
	// nothing of that frame.
	var side float64
	for i := range n {
		side = max(side, math.Abs(float64(reuse[1][i])))
	}
	if side == 0 {
		t.Fatal("the linear decode's channel 1 is silent there; the test proves nothing")
	}
	got, err := resume(at[2], false)
	if !errors.Is(err, codec.ErrNeedsHistory) {
		t.Fatalf("resumed at the landing: %v, want codec.ErrNeedsHistory", err)
	}
	if waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Errorf("the refusal carries %q, want malformed-input for a caller that cannot land earlier", waxerr.CodeOf(err))
	}
	if len(got[0]) != 0 {
		t.Errorf("the refused landing emitted %d samples", len(got[0]))
	}
	// Restarted, the same decode is a linear one, and there the shape is
	// damage.
	if _, err := resume(at[2], true); err == nil || errors.Is(err, codec.ErrNeedsHistory) {
		t.Errorf("a restarted decode returned %v, want the plain refusal", err)
	}

	// An object earlier, where container/asf lands, the second frame is the
	// lead-in, the curve arrives with it, and the reusing frame is exact.
	got, err = resume(at[1], false)
	if err != nil {
		t.Fatalf("resumed an object earlier: %v", err)
	}
	if len(got[0]) < 2*n {
		t.Fatalf("resumed an object earlier, %d samples", len(got[0]))
	}
	for ch := range 2 {
		for i := range n {
			if got[ch][n+i] != reuse[ch][i] {
				t.Fatalf("resumed an object earlier, channel %d sample %d: %v, the linear decode has %v",
					ch, i, got[ch][n+i], reuse[ch][i])
			}
		}
	}

	// A fresh decoder that begins at the landing without a seek still
	// refuses the reuse.
	fresh, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	var ferr error
	for i := at[2]; i < len(pkts) && ferr == nil; i++ {
		ferr = fresh.Decode(pkts[i], discard)
	}
	if ferr == nil || !contains(ferr.Error(), "never transmitted") {
		t.Errorf("a decode that begins at the landing without a seek returned %v, want the refusal", ferr)
	}
}

// layEnds lays frames back to back into reservoir superframes of payload bits
// each, stating every packet's frame count the other way a writer can: the
// frames that end in it, plus one when no frame is carried in. A frame that
// ends on its packet's last bit is then counted in that packet, and the next
// packet starts with nothing carried.
func layEnds(t *testing.T, cfg Config, frames []synthBits, payload int) [][]byte {
	t.Helper()
	offBits, err := cfg.offsetBits()
	if err != nil {
		t.Fatal(err)
	}
	if (8+offBits+3+payload)%8 != 0 {
		t.Fatalf("a %d-bit payload does not end a packet on a byte", payload)
	}
	var all synthBits
	var starts, ends []int
	for _, f := range frames {
		starts = append(starts, all.n)
		putBits(&all, f.b, 0, f.n)
		ends = append(ends, all.n)
	}
	var pkts [][]byte
	for cursor := 0; cursor < all.n; cursor += payload {
		end := cursor + payload
		done, carried, bitOff := 0, false, 0
		for i := range frames {
			if ends[i] > cursor && ends[i] <= end {
				done++
			}
			if starts[i] < cursor && ends[i] > cursor {
				carried = true
				if ends[i] <= end {
					bitOff = ends[i] - cursor
				}
			}
		}
		count := done
		if !carried {
			count++
		}
		var w synthBits
		resHeader(t, &w, cfg, count, bitOff)
		putBits(&w, all.b, cursor, min(payload, all.n-cursor))
		pkt := make([]byte, (8+offBits+3+payload)/8)
		copy(pkt, w.b)
		pkts = append(pkts, pkt)
	}
	return pkts
}

// TestUncarriedResolvesAFrameEndingOnAPacket. The frame count field is
// counted two ways by writers, and they differ only where a frame ends on its
// packet's last bit: Windows counts it in the packet it began in, so the next
// packet completes it from the carry, and the other way counts it where it
// ends, so the next packet starts clean. The headers of the next packet read
// the same either way, a zero bit offset and frames to count, so the sum of
// PacketFrames over-counts a frame at every such packet laid the second way.
// Uncarried tells them apart by parsing the packet before with the decoder's
// own bit walk, and the corrected sum is where every resumed decode lands and
// what a linear decode delivers.
func TestUncarriedResolvesAFrameEndingOnAPacket(t *testing.T) {
	cfg := reservoirConfig(t)
	n := cfg.FrameLen()
	full := func() synthBits {
		s := &synth{cfg: cfg}
		s.block(t, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
		return s.bw
	}
	quiet := func() synthBits {
		s := &synth{cfg: cfg}
		s.block(t, blockSpec{len: n, next: n})
		return s.bw
	}
	offBits, _ := cfg.offsetBits()
	b := full().n
	q := 0
	for (8+offBits+3+2*b+q)%8 != 0 {
		q++
	}
	// Every packet holds two coded frames and q one-bit ones exactly, so every
	// frame boundary at a packet's end is one.
	var frames []synthBits
	for range 6 {
		frames = append(frames, full(), full())
		for range q {
			frames = append(frames, quiet())
		}
	}
	// A frame that begins in one packet and ends on the last bit of the next
	// leaves that packet completing only the frame carried into it.
	qb := quiet().n
	m := 0
	for (m*qb+b)%2 != 0 || (8+offBits+3+(m*qb+b)/2)%8 != 0 {
		if m++; m*qb >= b {
			t.Fatal("no lead-in puts a coded frame across a whole packet")
		}
	}
	span := (m*qb + b) / 2
	var spanning []synthBits
	for range m {
		spanning = append(spanning, quiet())
	}
	for range 4 {
		spanning = append(spanning, full())
	}
	for total := m*qb + 4*b; total%span != 0; total += qb {
		spanning = append(spanning, quiet())
	}
	filled := layEnds(t, cfg, spanning, span)
	if f, _ := cfg.PacketFrames(filled[1]); f != 1 {
		t.Fatalf("the packet the frame fills states %d frames, want 1", f)
	}
	for _, laid := range []struct {
		name string
		pkts [][]byte
	}{
		{"counted where they end", layEnds(t, cfg, frames, 2*b+q)},
		{"counted where they begin", func() [][]byte { p, _ := layStarts(t, cfg, frames, 2*b+q); return p }()},
		{"counted where they end, one filling a packet", filled},
	} {
		t.Run(laid.name, func(t *testing.T) {
			pkts := laid.pkts
			before := make([]int, len(pkts)+1)
			raw := 0
			for i, p := range pkts {
				f, _ := cfg.PacketFrames(p)
				raw += f
				if i > 0 {
					un, err := cfg.Uncarried(pkts[i-1], p)
					if err != nil {
						t.Fatalf("packet %d: %v", i, err)
					}
					if un {
						before[i]--
					}
				}
				before[i+1] = before[i] + f
			}
			linear := decodeSynthAll(t, cfg, pkts, 0)
			if got := cfg.StreamSamples(int64(len(pkts)), int64(before[len(pkts)])); got != int64(len(linear)) {
				t.Errorf("the corrected count gives %d samples, a linear decode %d", got, len(linear))
			}
			if laid.name != "counted where they begin" && cfg.StreamSamples(int64(len(pkts)), int64(raw)) == int64(len(linear)) {
				t.Fatal("the plain sum is right on this stream; it proves nothing")
			}
			for j := 1; j < len(pkts); j++ {
				got := decodeSynthAll(t, cfg, pkts, j)
				at := before[j] * n
				if len(got) != len(linear)-at {
					t.Fatalf("resumed at packet %d: %d samples, the linear decode has %d past %d", j, len(got), len(linear)-at, at)
				}
				for i := range got {
					if got[i] != linear[at+i] {
						t.Fatalf("resumed at packet %d: sample %d differs", j, i)
					}
				}
			}
		})
	}
}

// TestACarryCheckKeepsItsDecoder: a caller that asks of every packet, as a
// frame count does, gets one decoder for the bit walk rather than one per
// question, and the same answers.
func TestACarryCheckKeepsItsDecoder(t *testing.T) {
	cfg := reservoirConfig(t)
	n := cfg.FrameLen()
	s := &synth{cfg: cfg}
	s.block(t, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
	full := s.bw
	offBits, _ := cfg.offsetBits()
	q := 0
	for (8+offBits+3+2*full.n+q)%8 != 0 {
		q++
	}
	var frames []synthBits
	for range 4 {
		frames = append(frames, full, full)
		for range q {
			s := &synth{cfg: cfg}
			s.block(t, blockSpec{len: n, next: n})
			frames = append(frames, s.bw)
		}
	}
	pkts := layEnds(t, cfg, frames, 2*full.n+q)
	check := cfg.CarryCheck()
	for i := 1; i < len(pkts); i++ {
		want, err := cfg.Uncarried(pkts[i-1], pkts[i])
		if err != nil {
			t.Fatal(err)
		}
		if got, err := check(pkts[i-1], pkts[i]); err != nil || got != want {
			t.Fatalf("packet %d: the kept check says %v (%v), Uncarried %v", i, got, err, want)
		}
	}
	if allocs := testing.AllocsPerRun(20, func() { check(pkts[0], pkts[1]) }); allocs > 1 {
		t.Errorf("an ambiguous packet costs %v allocations a question; the decoder is built again", allocs)
	}
}

// decodeSynthAll decodes pkts from packet from on, after a Reset when from is
// past the start, and returns channel 0.
func decodeSynthAll(t *testing.T, cfg Config, pkts [][]byte, from int) []float32 {
	t.Helper()
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	if from > 0 {
		for i := range from {
			if err := d.Decode(pkts[i], discard); err != nil {
				t.Fatalf("run-up packet %d: %v", i, err)
			}
		}
		d.Reset()
	}
	var out []float32
	emit := func(b *audio.Buffer) error {
		out = append(out, b.ChanF(0)[:b.N]...)
		return nil
	}
	for i := from; i < len(pkts); i++ {
		if err := d.Decode(pkts[i], emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := d.Drain(emit); err != nil {
		t.Fatal(err)
	}
	return out
}
