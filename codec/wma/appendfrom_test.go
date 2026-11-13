//go:build !wmatablesgen

package wma

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// appendFromBitLoop is appendFrom as it was before it copied by bytes: one
// bit at a time, a bit past the end of src reading as zero. It stays here as
// the reference the byte-wise form is held to.
func appendFromBitLoop(w *bitAppender, src []byte, at, n int) {
	for ; n > 0; n-- {
		if w.bits&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		if at>>3 < len(src) && src[at>>3]>>(7-at&7)&1 != 0 {
			w.buf[w.bits>>3] |= 1 << (7 - w.bits&7)
		}
		at++
		w.bits++
	}
}

// TestAppendFromMatchesTheBitLoop holds appendFrom to the loop it replaced,
// bit for bit, over every shape of run: nothing to copy, a destination that
// starts mid-byte, a source read from mid-byte, a tail that ends mid-byte,
// and a run that starts or ends past the source, which reads as zeroes.
func TestAppendFromMatchesTheBitLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x77a1, 0xc0de))
	shapes := map[string]int{}
	for i := range 10000 {
		src := make([]byte, rng.IntN(48))
		for j := range src {
			src[j] = byte(rng.Uint32())
		}
		// Both appenders start from the same bits, so the destination is
		// unaligned as often as not.
		prefix := rng.IntN(40)
		seed := []byte{byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32())}
		var got, want bitAppender
		appendFromBitLoop(&got, seed, 0, prefix)
		appendFromBitLoop(&want, seed, 0, prefix)

		at := rng.IntN(len(src)*8 + 24)
		n := rng.IntN(len(src)*8+40) - 4
		switch {
		case n <= 0:
			shapes["nothing to copy"]++
		case at >= len(src)*8:
			shapes["starts past the source"]++
		case at+n > len(src)*8:
			shapes["ends past the source"]++
		}
		if n > 0 {
			if prefix&7 != 0 {
				shapes["destination mid-byte"]++
			}
			if at&7 != 0 {
				shapes["source mid-byte"]++
			}
			if (prefix+n)&7 != 0 {
				shapes["tail mid-byte"]++
			}
			if prefix&7 == 0 && at&7 == 0 {
				shapes["both aligned"]++
			}
		}

		got.appendFrom(src, at, n)
		appendFromBitLoop(&want, src, at, n)
		if got.bits != want.bits || !bytes.Equal(got.buf, want.buf) {
			t.Fatalf("case %d: %d bits of a %d-byte source from bit %d onto %d bits:\n got %d bits %x\nwant %d bits %x",
				i, n, len(src), at, prefix, got.bits, got.buf, want.bits, want.buf)
		}
	}
	for _, shape := range []string{
		"nothing to copy", "starts past the source", "ends past the source",
		"destination mid-byte", "source mid-byte", "tail mid-byte", "both aligned",
	} {
		if shapes[shape] < 100 {
			t.Errorf("only %d cases were %q; the generator no longer covers it", shapes[shape], shape)
		}
	}
}

// BenchmarkAppendFrom is the carry's cost on a packet: 4 KiB appended from
// mid-byte onto a destination that is mid-byte too.
func BenchmarkAppendFrom(b *testing.B) {
	src := make([]byte, 4096+2)
	for i := range src {
		src[i] = byte(i * 131)
	}
	var w bitAppender
	b.SetBytes(4096)
	for b.Loop() {
		w.reset()
		w.appendFrom(src, 0, 3)
		w.appendFrom(src, 5, 4096*8)
	}
}
