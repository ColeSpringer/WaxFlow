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
// Four entry points read through it: BytesAt rounds a miss up to a Chunk, for
// a packet's bytes; Peek loads exactly what it is asked for, for an open path;
// Ahead grows with the window, for a Scan's confirm and a Chunk-per-step scan;
// Back grows it in front, for a scan that steps backward.
//
// The package is internal to the container tree: it is plumbing shared
// by demuxers, not API, and it must not become one (the v1.0 surface
// audit prunes exactly this kind of helper when exported).
package srcwin

import (
	"bytes"
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
	shared bool  // win is another window's storage, never written
}

// New returns a Window over src reading [0, dataEnd), wrapping read
// failures with the owner's public container name (for example "flac:
// reading frame data"), since these messages reach users verbatim.
func New(src container.Source, dataEnd int64, errWrap string) Window {
	return Window{src: src, dataEnd: dataEnd, errWrap: errWrap}
}

// aside returns a second window over w's source and data end, holding what
// w holds, with no error. Nothing done through it moves w, writes into w's
// storage or sticks on w; it is used while w rests, then kept or dropped.
func (w *Window) aside() Window {
	n := len(w.win)
	return Window{src: w.src, dataEnd: w.dataEnd, errWrap: w.errWrap, win: w.win[:n:n], winOff: w.winOff, shared: n > 0}
}

// Try runs look with w set aside: through w, look reads an aside while the
// window w held rests. When every read in look works, Try returns look's
// answer, and w keeps what look read if keep says so and is otherwise again
// what it held. A read that fails leaves w as it was, sticks nowhere, and is
// returned beside false. A look that only strengthens a claim drops that
// error, for a failed read is no answer; a scan that decides its answer
// returns it. A failure stuck on w before the look stays stuck when w keeps.
func (w *Window) Try(keep bool, look func() bool) (bool, error) {
	held := *w
	*w = held.aside()
	ok := look()
	if err := w.ioErr; err != nil {
		*w = held
		return false, err
	}
	if !keep {
		*w = held
		return ok, nil
	}
	w.ioErr = held.ioErr
	return ok, nil
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
//
// A miss at an off the window holds or ends at reads at least a Chunk at the
// window's end; any other off rebases to read at least a Chunk from it. A
// linear read then costs a read per Chunk, whatever its step.
func (w *Window) BytesAt(off int64, n int) []byte {
	if end := w.winOff + int64(len(w.win)); off >= w.winOff && off <= end {
		return w.view(off, n, end-off+max(off+int64(n)-end, Chunk))
	}
	return w.view(off, n, max(int64(n), Chunk))
}

// Peek returns up to n bytes starting at off with BytesAt's contract, but a
// miss loads exactly those bytes: a forward one extends the window by what
// is missing, a backward one rebases into a buffer of n. It is for the fixed
// structures an open path reads once (a magic, a header, a tag size, one
// frame), never for a per-packet loop: a Peek at the window's edge inside a
// loop would turn one read per window into three, and BytesAt's round-up is
// what the loop's cost rests on.
func (w *Window) Peek(off int64, n int) []byte {
	return w.view(off, n, int64(n))
}

// Ahead returns up to n bytes at an off the window holds or ends at, with
// BytesAt's contract. When [off, off+n) is not all resident it reads
//
//	max(missing, min(held, Chunk))
//
// bytes at the window's end, missing being what the request lacks and held
// what the window holds. A run of requests that each reach a little past
// the end then costs one read per doubling up to a Chunk and one per Chunk
// after, and the window never holds more than twice what its requests
// reach. For any other off it is BytesAt. It is the confirming read inside
// a Scan, and the view of a loop that asks for a Chunk at each step.
func (w *Window) Ahead(off int64, n int) []byte {
	end := w.winOff + int64(len(w.win))
	if off < w.winOff || off > end {
		return w.BytesAt(off, n)
	}
	missing := off + int64(n) - end
	return w.view(off, n, end-off+max(missing, min(int64(len(w.win)), Chunk)))
}

// Back returns up to n bytes at off with Peek's contract. A range that starts
// in front of the window and reaches it reads only the bytes in front, and the
// window then holds from off to its end, in fresh storage, so an earlier view
// still reads its bytes; a range that also runs past the end then extends it.
func (w *Window) Back(off int64, n int) []byte {
	if w.ioErr == nil && off < w.dataEnd && off < w.winOff && off+min(int64(n), w.dataEnd-off) >= w.winOff {
		buf := make([]byte, w.winOff+int64(len(w.win))-off)
		if err := container.ReadFull(w.src, buf[:w.winOff-off], off); err != nil {
			w.ioErr = waxerr.Wrap(waxerr.CodeSourceUnreadable, w.errWrap, err)
			return nil
		}
		copy(buf[w.winOff-off:], w.win)
		w.win, w.winOff, w.shared = buf, off, false
	}
	return w.Peek(off, n)
}

// Verdict is what a scan's try makes of one candidate.
type Verdict int

const (
	Skip Verdict = iota // not it: keep scanning
	Take                // it: the scan returns this offset
	Stop                // give up: the scan returns not found
)

// Scan offers try each occurrence of pat in [from, limit), ascending, with
// a view of need bytes from it (shorter only where the data ends; try
// rejects a view too short for it). A candidate below limit may carry its
// view past limit. It reads the range in spans that start at 1 KiB and
// double up to Chunk, so the reads grow with the range and not with what
// it holds; a pattern or a view that straddles a span is retried from the
// next. It never trims: the window holds everything from from to the
// furthest byte read until the caller trims, so a caller bounds limit-from.
// try must not Trim. It reads through Ahead from its candidate or back to
// from, all a confirm needs in one call: a read off either end of the
// window is BytesAt's, at least a Chunk, and rebases the window there,
// dropping what it held. A read failure ends the scan and stays in Err.
func (w *Window) Scan(from, limit int64, pat []byte, need int, try func(off int64, hdr []byte) Verdict) (int64, bool) {
	if len(pat) == 0 {
		return 0, false
	}
	limit = min(limit, w.dataEnd)
	need = max(need, len(pat))
	span := int64(1 << 10)
	for off := from; off < limit; span = min(span*2, Chunk) {
		// A span is at least one view, or it could offer nothing.
		n := min(max(span, int64(need)), limit-off+int64(need)-1)
		buf := w.Peek(off, int(n))
		ended := int64(len(buf)) < n // the data ends in this span, or the read failed
		// A pattern the span's end cuts is whole from the next span.
		next := off + int64(len(buf)-len(pat)+1)
		for i := 0; ; i++ {
			j := bytes.Index(buf[i:], pat)
			if j < 0 {
				break
			}
			i += j
			cand := off + int64(i)
			if cand >= limit {
				return 0, false
			}
			to := min(i+need, len(buf))
			hdr := buf[i:to:to]
			if len(hdr) < need && !ended {
				next = cand // its view is whole from the next span
				break
			}
			v := try(cand, hdr)
			if w.ioErr != nil {
				return 0, false // whatever try made of it
			}
			switch v {
			case Take:
				return cand, true
			case Stop:
				return 0, false
			}
		}
		if ended {
			return 0, false
		}
		off = next
	}
	return 0, false
}

// view serves [off, off+n) from the window, loading want bytes at off when
// it is not resident; load clamps want to the data end.
func (w *Window) view(off int64, n int, want int64) []byte {
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
	if err := w.load(off, want); err != nil {
		w.ioErr = err
		return nil
	}
	i := off - w.winOff
	return w.win[i : i+int64(n) : i+int64(n)]
}

// load makes [off, off+want) resident, want being the read-ahead the entry
// point chose (at least a Chunk for BytesAt, the exact request for Peek).
// Forward extension grows in place so earlier bytes of the current frame stay
// addressable; anything else rebases the window into fresh storage.
//
// The fresh storage is for a caller holding a view while it reads elsewhere,
// as a Scan holds its span while try reads: a read that starts before the
// window or past its end rebases, and reusing the array would rewrite the
// bytes that caller is still scanning.
func (w *Window) load(off, want int64) error {
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
		// Shared storage has no spare capacity, so it grows into fresh.
		grown := slices.Grow(w.win, int(need))[:len(w.win)+int(need)]
		if err := container.ReadFull(w.src, grown[len(w.win):], winEnd); err != nil {
			return waxerr.Wrap(waxerr.CodeSourceUnreadable, w.errWrap, err)
		}
		w.win, w.shared = grown, false
		return nil
	}
	buf := make([]byte, want)
	if err := container.ReadFull(w.src, buf, off); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, w.errWrap, err)
	}
	w.win, w.winOff, w.shared = buf, off, false
	return nil
}

// Resident reports the bytes the window holds and the storage behind them,
// for the cost pins outside this package: an owner's walk must leave the
// window bounded whatever the stream's length.
func (w *Window) Resident() (n, capacity int) { return len(w.win), cap(w.win) }

// Holds reports whether [off, off+n) is resident, so that reading it costs
// nothing and moves nothing.
func (w *Window) Holds(off int64, n int) bool {
	return off >= w.winOff && off+int64(n) <= w.winOff+int64(len(w.win))
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
	if w.shared {
		// Another window's storage is resliced, never moved.
		if off >= w.winOff+int64(len(w.win)) {
			w.win = nil
		} else {
			w.win = w.win[off-w.winOff:]
		}
		w.winOff = off
		return
	}
	if off >= w.winOff+int64(len(w.win)) {
		w.win, w.winOff = w.win[:0], off
		return
	}
	n := copy(w.win, w.win[off-w.winOff:])
	w.win, w.winOff = w.win[:n], off
}
