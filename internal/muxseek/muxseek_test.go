package muxseek

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestPatchesAreStreamOffsets: every offset a Patcher takes is measured from
// the muxer's first byte, so the same offset reaches the same field whatever
// the destination started at.
func TestPatchesAreStreamOffsets(t *testing.T) {
	for _, base := range []int64{0, 1, 97} {
		w := &testutil.MemWriteSeeker{}
		w.SeekTo(base)
		for i := range w.Buf {
			w.Buf[i] = 0xAA
		}
		p := New(w, "test")
		if !p.Seekable() {
			t.Fatal("an in-memory writer was taken for unseekable")
		}
		if _, err := w.Write([]byte("HEADERbody")); err != nil {
			t.Fatal(err)
		}
		if err := p.At(0, []byte("PATCH!")); err != nil {
			t.Fatalf("At: %v", err)
		}
		if err := p.Resume(10); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if _, err := w.Write([]byte("tail")); err != nil {
			t.Fatal(err)
		}
		if got, want := string(w.Buf[base:]), "PATCH!bodytail"; got != want {
			t.Errorf("base %d wrote %q, want %q", base, got, want)
		}
		for i, v := range w.Buf[:base] {
			if v != 0xAA {
				t.Errorf("base %d: byte %d ahead of the stream was overwritten (%#x)", base, i, v)
			}
		}
	}
}

// TestUnseekableDestinations: a writer with no Seek, and one whose Seek fails
// the way a pipe's does, are both destinations that cannot be patched. Resume
// is still safe to call on them, since nothing moved the writer off the end.
func TestUnseekableDestinations(t *testing.T) {
	for name, w := range map[string]io.Writer{
		"plain writer": io.Discard,
		"pipe":         &testutil.PipeWriteSeeker{},
	} {
		p := New(w, "test")
		if p.Seekable() {
			t.Errorf("%s: reported as patchable", name)
		}
		if CanSeek(w) {
			t.Errorf("%s: CanSeek reported true", name)
		}
		if err := p.Resume(10); err != nil {
			t.Errorf("%s: Resume on an unseekable destination: %v", name, err)
		}
		if err := p.At(0, []byte("x")); waxerr.CodeOf(err) != waxerr.CodeInternal {
			t.Errorf("%s: At returned %v, want an internal error", name, err)
		}
	}
}

// TestNegativeOffsetIsRefused: -1 is the live "slot never reserved" sentinel
// in four muxers, and every guard against it sits at a call site. Seeking to
// base-1 would write into whatever the caller put in front of the stream, so
// the patcher refuses rather than trusting each of those guards.
func TestNegativeOffsetIsRefused(t *testing.T) {
	w := &testutil.MemWriteSeeker{}
	p := New(w, "test")
	if err := p.At(-1, []byte("x")); waxerr.CodeOf(err) != waxerr.CodeInternal {
		t.Errorf("At(-1) returned %v, want an internal error", err)
	}
	if err := p.Resume(-1); waxerr.CodeOf(err) != waxerr.CodeInternal {
		t.Errorf("Resume(-1) returned %v, want an internal error", err)
	}
	if len(w.Buf) != 0 {
		t.Errorf("%d bytes were written by a refused patch", len(w.Buf))
	}
}

// TestFieldNamesTheError: a muxer with several patch sites tells them apart in
// the one error that arrives after a whole encode is already on disk.
func TestFieldNamesTheError(t *testing.T) {
	p := New(&failingSeeker{}, "mp4")
	err := p.Field("elst").At(0, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "elst") {
		t.Errorf("error %v does not name the field", err)
	}
}

// TestSeekThatDoesNotLandIsAnError: the return says where the destination now
// is, and a writer that reports somewhere else would put every later byte
// somewhere else too.
func TestSeekThatDoesNotLandIsAnError(t *testing.T) {
	w := &driftingSeeker{}
	p := New(w, "test")
	if !p.Seekable() {
		t.Fatal("the probe refused a writer whose Seek works")
	}
	if err := p.At(4, []byte("x")); waxerr.CodeOf(err) != waxerr.CodeOutputUnwritable {
		t.Errorf("At returned %v, want an unwritable-output error", err)
	}
}

// failingSeeker reports a position, then fails every seek that asks for one.
type failingSeeker struct{ at int64 }

func (f *failingSeeker) Write(p []byte) (int, error) { f.at += int64(len(p)); return len(p), nil }

func (f *failingSeeker) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekCurrent && off == 0 {
		return f.at, nil
	}
	return 0, errors.New("no seeking here")
}

// driftingSeeker accepts every seek and lands somewhere else.
type driftingSeeker struct{ buf []byte }

func (d *driftingSeeker) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	return len(p), nil
}

func (d *driftingSeeker) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekCurrent && off == 0 {
		return 0, nil
	}
	return off + 8, nil
}
