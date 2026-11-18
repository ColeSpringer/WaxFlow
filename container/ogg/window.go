package ogg

import (
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

const (
	// scanStep is how far a resync scans at a time, and the most the window
	// reads ahead.
	scanStep = 64 << 10
	// windowCap bounds what the window holds: a scan step or a page, and the
	// read-ahead behind it.
	windowCap = scanStep + maxPageSize
	// firstSpan is the read-ahead after a jump; each read that extends the
	// window doubles it, up to scanStep.
	firstSpan = 1 << 10
	// sumStride is how far apart the window keeps its running checksums.
	sumStride = 16
)

// window holds the source's bytes [off, off+len(buf)), which every page read
// and resync take from. It reads forward and never reads a byte it holds. A
// failed read is the error of the call that needed the bytes, and the next
// call reads again.
type window struct {
	src  container.Source
	size int64
	off  int64
	buf  []byte
	span int64
	// short is where the last read that failed reached, until a read gets
	// there. A read-ahead that would reach it goes half the way instead, so a
	// walk toward an unreadable byte closes in on it.
	short int64

	// sums[i] is the running checksum at sumOff+i*sumStride, from an origin
	// at or before sumOff. summed counts the bytes fed to any checksum.
	sumOff int64
	sums   []uint32
	summed int64
}

// newWindow is a window over src that holds nothing yet.
func newWindow(src container.Source) window { return window{src: src, size: src.Size()} }

func (w *window) end() int64 { return w.off + int64(len(w.buf)) }

// view is the held bytes [from, to).
func (w *window) view(from, to int64) []byte { return w.buf[from-w.off : to-w.off] }

// need makes the window hold [from, to) and returns those bytes, valid until
// the next need. keep, at or before from, is the first byte the caller still
// needs; a read that fails is wrapped in msg.
func (w *window) need(from, to, keep int64, msg string) ([]byte, error) {
	if to <= from {
		return nil, nil
	}
	if from >= w.off && to <= w.end() {
		return w.view(from, to), nil
	}
	start := w.end()
	if from < w.off || from > start {
		// A jump: start over at from.
		w.off, w.buf, w.sums = from, w.buf[:0], w.sums[:0]
		start, w.span = from, firstSpan
	} else {
		w.span = min(max(2*w.span, firstSpan), scanStep)
	}
	stop := min(max(to, start+w.span), w.size)
	if w.short > start && stop >= w.short {
		stop = max(to, start+(w.short-start)/2)
	}
	if stop-w.off > windowCap {
		w.drop(keep)
		stop = min(stop, w.off+windowCap)
	}
	if err := w.read(start, stop, to); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, msg, err)
	}
	return w.view(from, to), nil
}

// read appends [start, stop) to the window. When a read past to fails, it
// reads [start, to) alone before failing, so read-ahead never fails bytes
// that can be read, and the read-ahead starts small again.
func (w *window) read(start, stop, to int64) error {
	n := len(w.buf)
	if want := n + int(stop-start); want > cap(w.buf) {
		grown := make([]byte, n, min(max(2*cap(w.buf), want), windowCap))
		copy(grown, w.buf)
		w.buf = grown
	}
	err := container.ReadFull(w.src, w.buf[n:n+int(stop-start)], start)
	if err != nil && stop > to {
		w.short, w.span, stop = stop, 0, to
		err = container.ReadFull(w.src, w.buf[n:n+int(stop-start)], start)
	}
	if err != nil {
		w.short = stop
		return err
	}
	if start < w.short && w.short <= stop {
		w.short = 0
	}
	w.buf = w.buf[:n+int(stop-start)]
	return nil
}

// drop lets go of the bytes in front of keep, short of the last sum kept at
// or in front of it, and of the sums there.
func (w *window) drop(keep int64) {
	if k := (keep - w.sumOff) / sumStride; len(w.sums) > 0 && keep > w.sumOff {
		if k >= int64(len(w.sums)) {
			w.sums = w.sums[:0]
		} else {
			w.sums = w.sums[:copy(w.sums, w.sums[k:])]
			w.sumOff += k * sumStride
			keep = w.sumOff
		}
	}
	n := keep - w.off
	if n <= 0 {
		return
	}
	w.buf = w.buf[:copy(w.buf, w.buf[n:])]
	w.off = keep
}

// sum feeds b to the checksum crc.
func (w *window) sum(crc uint32, b []byte) uint32 {
	w.summed += int64(len(b))
	return crc32(crc, b)
}

// prefix is the running checksum at x, a held offset at or past sumOff,
// keeping the sums up to it.
func (w *window) prefix(x int64) uint32 {
	i := (x - w.sumOff) / sumStride
	for int64(len(w.sums)) <= i {
		at := w.sumOff + int64(len(w.sums)-1)*sumStride
		w.sums = append(w.sums, w.sum(w.sums[len(w.sums)-1], w.view(at, at+sumStride)))
	}
	at := w.sumOff + i*sumStride
	return w.sum(w.sums[i], w.view(at, x))
}

// rangeSum is the checksum of the held bytes [a, b). With R the running sum,
// crc(A||B) = shift(crc(A), len(B)) ^ crc(B), so it is R(b) ^ shift(R(a), b-a).
// The sums start over at a when none is kept at or in front of it.
func (w *window) rangeSum(a, b int64) uint32 {
	if len(w.sums) == 0 || a < w.sumOff || a > w.sumOff+int64(len(w.sums)-1)*sumStride {
		w.sumOff, w.sums = a, append(w.sums[:0], 0)
	}
	return w.prefix(b) ^ crcShift(w.prefix(a), b-a)
}

// pageSum is the checksum of the held page of size bytes at off as the
// format sums it, with the checksum field zeroed: the field's own sum,
// carried to the page's end, cancels it.
func (w *window) pageSum(off, size int64) uint32 {
	field := w.sum(0, w.view(off+22, off+26))
	return w.rangeSum(off, off+size) ^ crcShift(field, size-26)
}
