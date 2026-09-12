// Package srcwin is the shared read-ahead window demuxers scan through:
// byte access over a container.Source with chunked read-ahead, forward
// extension, rebasing, and a sticky I/O error that the owner surfaces on
// its packet and seek paths. flacn and mpa walk streams the same way
// (find a boundary, read a frame, advance); this is that walk's memory.
//
// A linear read costs one window and nothing per packet: the window reaches
// its size early and is then reused, so walking a gigabyte allocates what
// walking a megabyte does. That is a property of Trim and load together rather
// than of either alone, and srcwin_test.go pins it, because losing it is
// invisible (every byte still arrives, at every offset, correct) and costs
// every demuxer in the tree at once.
//
// The package is internal to the container tree: it is plumbing shared
// by demuxers, not API, and it must not become one (the v1.0 surface
// audit prunes exactly this kind of helper when exported).
package srcwin

import (
	"slices"

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
// failures with the owner's public container name (for example "flac:
// reading frame data"), since these messages reach users verbatim.
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

// load makes [off, off+n) resident. Forward extension grows in place so
// earlier bytes of the current frame stay addressable; anything else rebases
// the window into fresh storage.
//
// Fresh storage on the rebase, and it is deliberate rather than the one
// allocation left unswept. A caller may hold a view while reading elsewhere:
// flacn's frame-boundary scan walks a Chunk-sized view for sync candidates and
// reads a CRC range behind it on every candidate, which is a backward read and
// so a rebase. Reusing the array under it would rewrite the buffer that loop is
// still scanning, turning a sync search into garbage. A rebase is rare by
// construction (a seek, not a step), so this costs one window per jump and buys
// every caller the right to keep a view across an unrelated read.
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
		// slices.Grow rather than append(w.win, make([]byte, need)...), and
		// the difference is not the append: Grow is written as that same
		// append, and reaches it only when the capacity is short. The bare
		// form appends need bytes unconditionally, so where the compiler does
		// not elide the slice it is handed (that elision is not applied on
		// every target) it allocates and zeroes them on every extension and
		// then immediately overwrites them, which measures as a full copy of
		// the stream on a 32-bit build. With the window reused rather than
		// rebuilt, the capacity is usually already there and the right amount
		// of work is none.
		grown := slices.Grow(w.win, int(need))[:len(w.win)+int(need)]
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
// position instead of accreting the whole file. Views taken before a Trim do
// not survive it: the bytes it keeps are moved to the front of the window's
// own storage rather than copied into new storage, so a caller takes its views
// after trimming, which every caller in the tree does.
//
// Moving them in place rather than allocating is what makes a linear read
// cost O(1) instead of O(stream), and it fixes both halves of that cost at
// once. Copying into fresh storage spent one window-sized allocation per Chunk
// walked; it also threw the old storage away, so the extension that followed
// had no capacity to grow into and allocated again. Together that was about
// two allocations of ~128 KiB for every 128 KiB of stream, forever. With the
// bytes moved in place the window reaches its size in two growths and then
// stays there, whatever the stream's length.
func (w *Window) Trim(off int64) {
	if off-w.winOff < Chunk {
		return
	}
	if off >= w.winOff+int64(len(w.win)) {
		w.win, w.winOff = w.win[:0], off
		return
	}
	n := copy(w.win, w.win[off-w.winOff:])
	w.win, w.winOff = w.win[:n], off
}
