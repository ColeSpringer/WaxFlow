package wv

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
)

// TestPutTotalRoundTrip pins the writer of the total-samples field against the
// header parser that reads it back, at the boundaries where the two could
// disagree.
//
// The field is not simply the count's high and low words: the writer skips
// every value whose low word would collide with the unknown-length escape and
// the reader subtracts that skip back off, which is what keeps a length of
// exactly 2^32-1 distinguishable from "unknown". It is also what puts the
// largest writable count 257 short of the forty-bit field's range rather than
// one, since past that the skip carries into a ninth bit of the high byte and
// the byte wraps to zero. wavpack.MaxSamples is that bound, and the muxer and
// the encoder both refuse anything above it, so the top of this range is the
// top of what can be written.
func TestPutTotalRoundTrip(t *testing.T) {
	head := oneHeader(t)
	const escape = int64(0xffffffff)
	for _, n := range []int64{-1, 0, 1, escape - 1, escape, escape + 1, escape + 2,
		2 * escape, 2*escape + 1, wavpack.MaxSamples - 1, wavpack.MaxSamples} {
		putTotal(head[11:16], n)
		h, err := wavpack.ParseBlockHeader(head)
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if h.TotalSamples != n {
			t.Errorf("wrote %d, header reads back %d", n, h.TotalSamples)
		}
	}
}

// oneHeader returns a real block header to patch, so the round trip runs
// through the parser rather than a hand-built imitation of one.
func oneHeader(t *testing.T) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1),
		Type: audio.Int, BitDepth: 16}
	enc, err := wavpack.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := audio.Get(f, 64)
	defer audio.Put(buf)
	buf.N = 64
	var block []byte
	err = enc.Encode(buf, func(p codec.Packet) error {
		block = append([]byte(nil), p.Data...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return block
}
