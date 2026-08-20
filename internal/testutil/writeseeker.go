package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// MemWriteSeeker is an in-memory io.WriteSeeker, for exercising a muxer's
// back-patch path without a temp file.
type MemWriteSeeker struct {
	Buf []byte
	pos int64
}

func (w *MemWriteSeeker) Write(p []byte) (int, error) {
	// Grown through append, so a muxer writing a long stream in small packets
	// costs what a file would rather than a copy of everything so far per
	// write.
	if need := w.pos + int64(len(p)); need > int64(len(w.Buf)) {
		w.Buf = append(w.Buf, make([]byte, need-int64(len(w.Buf)))...)
	}
	copy(w.Buf[w.pos:], p)
	w.pos += int64(len(p))
	return len(p), nil
}

// Seek refuses a negative position, which is what a file does: a muxer that
// patches at a slot it never reserved gets an error at the seek rather than a
// panic in the next Write.
func (w *MemWriteSeeker) Seek(off int64, whence int) (int64, error) {
	pos := off
	switch whence {
	case io.SeekCurrent:
		pos += w.pos
	case io.SeekEnd:
		pos += int64(len(w.Buf))
	}
	if pos < 0 {
		return w.pos, errors.New("testutil: seek to a negative offset")
	}
	w.pos = pos
	return w.pos, nil
}

// SeekTo positions the writer past the start of its destination.
func (w *MemWriteSeeker) SeekTo(off int64) {
	if int64(len(w.Buf)) < off {
		w.Buf = append(w.Buf, make([]byte, off-int64(len(w.Buf)))...)
	}
	w.pos = off
}

// Pos reports where the writer sits, which a muxer that back-patches must
// leave at the end of what it wrote.
func (w *MemWriteSeeker) Pos() int64 { return w.pos }

// MuxAtOffset drives mux twice: once into a writer at the start of its
// destination, once into one already positioned base bytes in. It returns the
// stream the second run wrote, so a caller can read back what an offset
// destination holds.
//
// The two streams must be identical, which is the contract rather than a
// convenience: a muxer states stream offsets about itself, so nothing it
// writes may vary with where the destination started.
//
// The two runs differ only when a back-patch takes a stream offset for a file
// offset, which lands base bytes early: in front of the stream, where the
// recognizable filler is.
func MuxAtOffset(t testing.TB, base int64, mux func(w io.Writer)) []byte {
	t.Helper()
	var flat MemWriteSeeker
	mux(&flat)

	var off MemWriteSeeker
	off.SeekTo(base)
	for i := range off.Buf {
		off.Buf[i] = 0xAA
	}
	mux(&off)

	for i, v := range off.Buf[:base] {
		if v != 0xAA {
			t.Fatalf("byte %d ahead of the stream was overwritten (%#x): a patch offset is missing the writer's start", i, v)
		}
	}
	if got := off.Buf[base:]; !bytes.Equal(got, flat.Buf) {
		t.Fatalf("muxing at offset %d wrote %d bytes, %d at offset zero%s",
			base, len(got), len(flat.Buf), firstDiff(got, flat.Buf))
	}
	if want := int64(len(off.Buf)); off.Pos() != want {
		t.Errorf("the muxer left the writer at %d of %d bytes; a patch must seek back to the end", off.Pos(), want)
	}
	return off.Buf[base:]
}

// firstDiff names where two streams part, for MuxAtOffset's message.
func firstDiff(a, b []byte) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Sprintf(" (first difference at byte %d: %#x against %#x)", i, a[i], b[i])
		}
	}
	return ""
}

// PipeWriteSeeker satisfies io.WriteSeeker the way *os.File does for a pipe:
// the method is there whether or not the object behind it can seek. A muxer
// that reads seekability off the method set takes it for a file.
type PipeWriteSeeker struct{ Buf []byte }

func (w *PipeWriteSeeker) Write(p []byte) (int, error) {
	w.Buf = append(w.Buf, p...)
	return len(p), nil
}

func (w *PipeWriteSeeker) Seek(int64, int) (int64, error) { return 0, errors.New("illegal seek") }
