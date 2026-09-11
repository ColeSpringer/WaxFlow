//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
)

// Hand-built streams. The committed corpus is the whole of what Windows'
// encoder can write, so every shape the format allows and that encoder does
// not reach has to be built here or go untested: the refusals, the structural
// conditions, and the pulse schemes' sign polarity.

// bitWriter writes most significant bit first, which is how this format reads.
type bitWriter struct {
	buf  []byte
	bits int
}

func (w *bitWriter) put(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.bits&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.buf[w.bits>>3] |= 1 << uint(7-w.bits&7)
		}
		w.bits++
	}
}

// pad fills to n bytes, which is what nBlockAlign asks of a packet. A packet
// that already overran it is a bug in the test rather than something to
// truncate into silence.
func (w *bitWriter) pad(n int) []byte {
	if len(w.buf) > n {
		panic("wmavoice test: hand-built packet longer than nBlockAlign")
	}
	for len(w.buf) < n {
		w.buf = append(w.buf, 0)
	}
	return w.buf
}

// stream is a hand-built track: the WAVEFORMATEX plus extra bytes a decoder
// parses, and the geometry a packet builder needs.
type stream struct {
	rate    int
	align   int
	flags   uint32
	classes [17]int
	cfg     wmavoice.Config
}

// defaultClasses gives every frame type a class, three to a class, in index
// order. Type 0 lands in class 0 slot 0, so a silence frame is the two-bit
// codeword 00 and a hand-built frame can be ten bits long.
func defaultClasses() [17]int {
	var c [17]int
	for i := range c {
		c[i] = i / 3
	}
	return c
}

// newStream builds a track. flags carries the postfilter off by default, so a
// hand-built stream exercises the frame layer without the postfilter's
// arithmetic standing between the bits and the samples.
func newStream(t testing.TB, rate, align int, flags uint32, classes [17]int) stream {
	t.Helper()
	s := stream{rate: rate, align: align, flags: flags, classes: classes}
	cfg, err := wmavoice.ParseConfig(s.config())
	if err != nil {
		t.Fatalf("hand-built config: %v", err)
	}
	s.cfg = cfg
	return s
}

// config is the 64-byte blob container/asf hands over as Track.CodecConfig.
func (s stream) config() []byte {
	b := make([]byte, 18+46)
	le := binary.LittleEndian
	le.PutUint16(b, 0x000A)
	le.PutUint16(b[2:], 1)
	le.PutUint32(b[4:], uint32(s.rate))
	le.PutUint32(b[8:], uint32(s.align))
	le.PutUint16(b[12:], uint16(s.align))
	le.PutUint16(b[14:], 16)
	le.PutUint16(b[16:], 46)
	le.PutUint32(b[18+18:], s.flags)
	var w bitWriter
	for _, c := range s.classes {
		w.put(uint64(c), 3)
	}
	copy(b[18+22:], w.buf)
	return b
}

func (s stream) decoder(t testing.TB) *wmavoice.Decoder {
	t.Helper()
	dec, err := wmavoice.NewDecoder(s.cfg, s.cfg.Format())
	if err != nil {
		t.Fatalf("hand-built decoder: %v", err)
	}
	return dec
}

// silenceFrame writes one comfort-noise frame: the two-bit codeword for frame
// type 0 under defaultClasses, then its eight-bit gain index.
func (s stream) silenceFrame(w *bitWriter, gain int) { w.put(0, 2); w.put(uint64(gain), 8) }

// residualLSPs writes a superframe's LSP block with every index zero, which is
// a legal set and the cheapest one to write.
func (s stream) residualLSPs(w *bitWriter) {
	if s.cfg.LSPs() == 10 {
		for _, n := range []int{8, 6, 5, 5, 5, 7, 6, 6} {
			w.put(0, n)
		}
		return
	}
	for _, n := range []int{8, 6, 7, 6, 7, 5, 7, 7, 7} {
		w.put(0, n)
	}
}

// silentSuperframe writes a whole superframe of comfort noise. samples of -1
// leaves the length field absent, which means the full 480.
func (s stream) silentSuperframe(w *bitWriter, samples int) {
	w.put(1, 1) // speech, not a WMA Pro payload
	if samples < 0 {
		w.put(0, 1)
	} else {
		w.put(1, 1)
		w.put(uint64(samples), 12)
	}
	s.residualLSPs(w)
	for range wmavoice.FramesPerSuperframe {
		s.silenceFrame(w, 128)
	}
	w.put(0, 1) // no statistics block
}

// packet writes a packet header and n superframes, declaring a count of n. The
// decoder takes n-1 out of the body and carries the last, which is the packet
// layer's own rule: the count is one MORE than the body yields, because the
// last superframe always becomes the carry even when the packet holds it
// complete. A packet written with n = 1 therefore decodes nothing itself.
//
// The padding behind the last superframe travels in the carry too and is never
// read: the next packet decodes the carry's superframe and stops.
func (s stream) packet(spill, n int, samples ...int) []byte {
	var w bitWriter
	w.put(0, 4) // the sequence number, which nothing reads
	w.put(1, 1) // residual LSPs
	w.put(uint64(n), 6)
	w.put(uint64(spill), wmavoice.SpilloverBits(s.cfg))
	for i := range n {
		k := -1
		if i < len(samples) {
			k = samples[i]
		}
		s.silentSuperframe(&w, k)
	}
	return w.pad(s.align)
}

// decode runs a run of packets through one decoder and returns the samples.
func decodeStream(t testing.TB, dec *wmavoice.Decoder, pkts ...[]byte) ([]float32, error) {
	t.Helper()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
		return nil
	}
	for _, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			return got, err
		}
	}
	err := dec.Drain(emit)
	return got, err
}
