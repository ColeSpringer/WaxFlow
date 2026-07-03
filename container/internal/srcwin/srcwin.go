// Package srcwin is the shared read-ahead window demuxers scan through:
// byte access over a container.Source with chunked read-ahead, forward
// extension, rebasing, and a sticky I/O error that the owner surfaces on
// its packet and seek paths. flacn and mpa walk streams the same way
// (find a boundary, read a frame, advance); this is that walk's memory.
//
// The package is internal to the container tree: it is plumbing shared
// by demuxers, not API, and it must not become one (the v1.0 surface
// audit prunes exactly this kind of helper when exported).
package srcwin

import (
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Chunk is the read-ahead granularity.
const Chunk = 128 << 10

// Window provides windowed byte access over a source. The zero value is
// unusable; construct with New.
type Window struct {
	src     container.Source
	dataEnd int64 // logical end of readable data; the owner may shrink it
	errWrap string

	win    []byte
	winOff int64
	ioErr  error // sticky read failure
}

// New returns a Window over src reading [0, dataEnd), wrapping read
// failures with the owner's package prefix (for example "flacn: reading
// frame data").
func New(src container.Source, dataEnd int64, errWrap string) Window {
	return Window{src: src, dataEnd: dataEnd, errWrap: errWrap}
}

// Err returns the sticky read failure, nil while reads work.
func (w *Window) Err() error { return w.ioErr }

// DataEnd returns the current logical end of data.
func (w *Window) DataEnd() int64 { return w.dataEnd }

// SetDataEnd shrinks (or restores) the logical end of data; owners use
// it to strip trailing tags once confirmed.
func (w *Window) SetDataEnd(end int64) { w.dataEnd = end }

// BytesAt returns up to n bytes starting at off, clamped to the data
// end. A short or empty result means end of data or a read failure;
// failures stick in Err. The view is full-capacity sliced: appending to
// it cannot scribble over neighboring window bytes.
func (w *Window) BytesAt(off int64, n int) []byte {
	if n <= 0 || off >= w.dataEnd || w.ioErr != nil {
		return nil
	}
	if left := w.dataEnd - off; int64(n) > left {
		n = int(left)
	}
	if off >= w.winOff && off+int64(n) <= w.winOff+int64(len(w.win)) {
		i := off - w.winOff
		return w.win[i : i+int64(n) : i+int64(n)]
	}
	if err := w.load(off, n); err != nil {
		w.ioErr = err
		return nil
	}
	i := off - w.winOff
	return w.win[i : i+int64(n) : i+int64(n)]
}

// load makes [off, off+n) resident. Forward extension appends so earlier
// bytes of the current frame stay addressable; anything else rebases the
// window.
func (w *Window) load(off int64, n int) error {
	want := max(int64(n), Chunk)
	if off+want > w.dataEnd {
		want = w.dataEnd - off
	}
	winEnd := w.winOff + int64(len(w.win))
	if off >= w.winOff && off <= winEnd {
		need := off + want - winEnd
		if need <= 0 {
			return nil
		}
		grown := append(w.win, make([]byte, need)...)
		if err := container.ReadFull(w.src, grown[len(w.win):], winEnd); err != nil {
			return waxerr.Wrap(waxerr.CodeSourceUnreadable, w.errWrap, err)
		}
		w.win = grown
		return nil
	}
	buf := make([]byte, want)
	if err := container.ReadFull(w.src, buf, off); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, w.errWrap, err)
	}
	w.win, w.winOff = buf, off
	return nil
}

// Trim drops window bytes before off so the window tracks the stream
// position instead of accreting the whole file.
func (w *Window) Trim(off int64) {
	if off-w.winOff < Chunk {
		return
	}
	if off >= w.winOff+int64(len(w.win)) {
		w.win, w.winOff = w.win[:0], off
		return
	}
	kept := w.win[off-w.winOff:]
	w.win = append(w.win[:0:0], kept...)
	w.winOff = off
}
