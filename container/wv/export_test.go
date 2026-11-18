package wv

import (
	"context"
	"sort"
)

// Bounds the tests refer to.
const (
	MaxResyncForTest   = maxResync
	MaxTailScanForTest = maxTailScan
)

// ResidentForTest is how many bytes the read window holds.
func (d *Demuxer) ResidentForTest() int {
	n, _ := d.w.Resident()
	return n
}

// WindowStartForTest is where the read window starts, for a window that
// holds the byte in front of at: the lowest s with [s, at) resident. It reads
// nothing.
func (d *Demuxer) WindowStartForTest(at int64) int64 {
	return int64(sort.Search(int(at), func(s int) bool { return d.w.Holds(int64(s), int(at)-s) }))
}

// WalkResidentForTest walks the stream's headers and returns the most bytes
// the walk's own window held between two steps.
func (d *Demuxer) WalkResidentForTest() (most int) {
	d.w.Try(false, func() bool {
		return d.walkPath(stepContext{context.Background(), func() { most = max(most, d.ResidentForTest()) }}) == nil
	})
	return most
}

// stepContext is a context that is asked at every step of a walk.
type stepContext struct {
	context.Context
	step func()
}

func (c stepContext) Err() error {
	c.step()
	return nil
}

// ScanTailForTest runs the tail scan alone and returns its answer: the index
// one past the last block's samples.
func (d *Demuxer) ScanTailForTest() (int64, bool) {
	t, ok := d.scanTail()
	return t.end, ok
}

// ProbeForTest runs one bisection probe from mid, short of hi, and returns
// the offset of the block it found.
func (d *Demuxer) ProbeForTest(mid, hi int64) (int64, bool) {
	off, _, ok := d.probe(mid, hi)
	return off, ok
}

// BacksForTest is how many blocks a read placed behind a step back, less
// the block behind the first where the open checked its index (no total).
func (d *Demuxer) BacksForTest() int {
	if o := d.outrun; o.headBack && d.declared < 0 {
		return o.backs - 1
	}
	return d.outrun.backs
}

// TailUnreadForTest says the length was exact for its total and tail
// agreeing alone, and the read did not deliver that tail block.
func (d *Demuxer) TailUnreadForTest() bool { return d.outrun.tail >= 0 && !d.outrun.tailRead }

// PathForTest is what the walk of the stream's headers cost and kept: the
// blocks it stepped to, the bytes its resyncs scanned, the gaps it holds, and
// whether it reached the stream's end.
func (d *Demuxer) PathForTest() (hops int, resynced int64, gaps int, done bool) {
	return d.path.hops, d.path.resynced, len(d.path.gaps), d.path.done
}

// Bounds of the walk the tests refer to.
const (
	MaxHopsForTest = maxHops
	MaxGapsForTest = maxGaps
)
