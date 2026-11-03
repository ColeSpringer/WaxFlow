package testutil

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// CountingSource wraps a Source and counts the reads served through it, so a
// test can pin what a demuxer asked the source for and not only what it got
// back. A byte count alone cannot see a scan that asks for the same window
// over and over; the read count can.
type CountingSource struct {
	Src   container.Source
	Reads int
	Bytes int64
	// Low is the lowest offset read since the last Reset, when Reads says
	// there was a read.
	Low int64
	// Log keeps each read's range in Spans, so a test can ask which reads
	// covered a given byte.
	Log   bool
	Spans [][2]int64
}

func (c *CountingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Src.ReadAt(p, off)
	if c.Reads == 0 || off < c.Low {
		c.Low = off
	}
	c.Reads++
	c.Bytes += int64(n)
	if c.Log {
		c.Spans = append(c.Spans, [2]int64{off, off + int64(len(p))})
	}
	return n, err
}

func (c *CountingSource) Size() int64 { return c.Src.Size() }

// Reset zeroes the counters, so one phase of a read can be measured alone.
func (c *CountingSource) Reset() { c.Reads, c.Bytes, c.Low, c.Spans = 0, 0, 0, nil }

// CancelAfterChecks returns a context that ends on its n-th Err call, so a
// test can stop a loop that reads its context per step at a chosen step,
// whatever the source reads.
func CancelAfterChecks(t testing.TB, n int) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &countdownContext{Context: ctx, cancel: cancel, left: n}
}

type countdownContext struct {
	context.Context
	cancel context.CancelFunc
	left   int
}

func (c *countdownContext) Err() error {
	if c.left > 0 {
		if c.left--; c.left == 0 {
			c.cancel()
		}
	}
	return c.Context.Err()
}

// Walkable is a demuxer with a deferred walk, as WalkPauses drives it.
type Walkable interface {
	Walk(ctx context.Context) error
	Walked() bool
	Tracks() []container.Track
	Warnings() []container.Warning
}

// WalkPauses drives container.Walker's cancel contract over one file: a walk
// whose context ends on its checks-th step is canceled, not Walked and settles
// nothing, and the walk after it settles a fresh walk's length and findings,
// the two together reading exactly what one fresh walk reads. open opens the
// file over src.
func WalkPauses(t *testing.T, raw []byte, checks int, open func(src container.Source) (Walkable, error)) {
	t.Helper()
	fresh := &CountingSource{Src: container.BytesSource(raw)}
	full, err := open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := full.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := full.Tracks()[0]

	src := &CountingSource{Src: container.BytesSource(raw)}
	d, err := open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(CancelAfterChecks(t, checks)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Walk = %v, want canceled", err)
	}
	if d.Walked() {
		t.Error("a canceled walk reports Walked")
	}
	if tr := d.Tracks()[0]; tr.SamplesExact {
		t.Errorf("a canceled walk settled the length at %d", tr.Samples)
	}
	if err := d.Walk(t.Context()); err != nil {
		t.Fatalf("Walk after the cancel: %v", err)
	}
	if !d.Walked() {
		t.Error("not Walked after the walk finished")
	}
	got := d.Tracks()[0]
	if !got.SamplesExact || got.Samples != want.Samples || got.Padding != want.Padding ||
		got.MidPadding != want.MidPadding || !slices.Equal(got.MidTrims, want.MidTrims) {
		t.Errorf("the resumed walk settled %+v, a fresh walk %+v", got, want)
	}
	if !slices.Equal(d.Warnings(), full.Warnings()) {
		t.Errorf("the resumed walk reported %v, a fresh walk %v", d.Warnings(), full.Warnings())
	}
	if src.Reads != fresh.Reads {
		t.Errorf("the paused and resumed walks read %d times, a fresh walk %d: the resume re-read or skipped", src.Reads, fresh.Reads)
	}
}
