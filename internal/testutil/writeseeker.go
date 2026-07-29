package testutil

import "io"

// MemWriteSeeker is an in-memory io.WriteSeeker, for exercising a muxer's
// back-patch path without a temp file.
type MemWriteSeeker struct {
	Buf []byte
	pos int64
}

func (w *MemWriteSeeker) Write(p []byte) (int, error) {
	if need := w.pos + int64(len(p)); need > int64(len(w.Buf)) {
		grown := make([]byte, need)
		copy(grown, w.Buf)
		w.Buf = grown
	}
	copy(w.Buf[w.pos:], p)
	w.pos += int64(len(p))
	return len(p), nil
}

func (w *MemWriteSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = off
	case io.SeekCurrent:
		w.pos += off
	case io.SeekEnd:
		w.pos = int64(len(w.Buf)) + off
	}
	return w.pos, nil
}

// SeekTo positions the writer past the start of its destination.
func (w *MemWriteSeeker) SeekTo(off int64) {
	if int64(len(w.Buf)) < off {
		w.Buf = append(w.Buf, make([]byte, off-int64(len(w.Buf)))...)
	}
	w.pos = off
}
