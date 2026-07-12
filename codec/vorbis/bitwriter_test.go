package vorbis

import (
	"math/rand"
	"testing"
)

// The bitWriter is the one genuinely new primitive under the encoder, and it
// packs LSB-first (the opposite of codec/flac's MSB-first writer): a subtle bug
// here corrupts every header and every packet. So it is proven against the
// existing bitReader in isolation, before any floor or residue serialization is
// built on it, exactly as the plan prescribes.

// field is one write in a mixed-width round-trip.
type field struct {
	k uint
	v uint32
}

func writeReadFields(t *testing.T, fields []field) {
	t.Helper()
	var w bitWriter
	for _, f := range fields {
		w.writeBits(f.k, f.v)
	}
	r := newBitReader(w.bytes())
	for i, f := range fields {
		got := r.read(int(f.k))
		want := f.v & mask32(f.k)
		if got != want {
			t.Fatalf("field %d (k=%d): read %#x, wrote %#x", i, f.k, got, want)
		}
	}
	if r.eof {
		t.Fatalf("reader hit eof reading back %d fields", len(fields))
	}
}

func TestBitWriterSingleBits(t *testing.T) {
	// A run of single bits with a known pattern crosses byte boundaries and
	// checks that bit 0 of the first field lands in bit 0 of byte 0.
	pattern := []uint32{1, 0, 1, 1, 0, 0, 0, 1, 1, 1, 0, 1, 0, 1, 0, 0, 1}
	var fields []field
	for _, b := range pattern {
		fields = append(fields, field{1, b})
	}
	writeReadFields(t, fields)

	// The first byte's bits, LSB-first, must be the first eight pattern bits.
	var w bitWriter
	for _, b := range pattern[:8] {
		w.writeBit(b)
	}
	got := w.bytes()[0]
	var want byte
	for i, b := range pattern[:8] {
		want |= byte(b) << uint(i)
	}
	if got != want {
		t.Fatalf("first byte %#08b, want %#08b (LSB-first packing)", got, want)
	}
}

func TestBitWriterWidths(t *testing.T) {
	// Every width 1..32 at a value that exercises the high bit, plus a couple
	// of odd offsets so the fields do not all start byte-aligned.
	var fields []field
	fields = append(fields, field{3, 0b101})
	for k := uint(1); k <= 32; k++ {
		fields = append(fields, field{k, 0xffffffff}) // all ones, masked to k
		fields = append(fields, field{k, 1})          // low bit only
		if k >= 2 {
			fields = append(fields, field{k, 1 << (k - 1)}) // top bit of the field
		}
	}
	writeReadFields(t, fields)
}

func TestBitWriterZeroWidth(t *testing.T) {
	// A zero-width write must be a no-op and not disturb the stream.
	var w bitWriter
	w.writeBits(5, 0b10110)
	w.writeBits(0, 0xdeadbeef)
	w.writeBits(7, 0b1001011)
	if w.bits() != 12 {
		t.Fatalf("zero-width write changed the bit count: %d, want 12", w.bits())
	}
	r := newBitReader(w.bytes())
	if got := r.read(5); got != 0b10110 {
		t.Fatalf("first field %#b", got)
	}
	if got := r.read(7); got != 0b1001011 {
		t.Fatalf("second field %#b", got)
	}
}

func TestBitWriterMask(t *testing.T) {
	// High bits above k must be dropped, matching the reader which can only
	// return k bits.
	var w bitWriter
	w.writeBits(4, 0xfff3) // only 0x3 survives
	r := newBitReader(w.bytes())
	if got := r.read(4); got != 0x3 {
		t.Fatalf("masked field %#x, want 0x3", got)
	}
}

func TestBitWriterRoundTripRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		n := rng.Intn(40)
		fields := make([]field, n)
		for i := range fields {
			k := uint(rng.Intn(32) + 1)
			fields[i] = field{k, rng.Uint32()}
		}
		writeReadFields(t, fields)
	}
}

// TestBitWriterBits tracks the running bit count across byte boundaries.
func TestBitWriterBits(t *testing.T) {
	var w bitWriter
	steps := []uint{1, 7, 8, 3, 32, 5}
	var want int64
	for _, k := range steps {
		w.writeBits(k, 0)
		want += int64(k)
		if w.bits() != want {
			t.Fatalf("after writing %d bits, count is %d", want, w.bits())
		}
	}
}
