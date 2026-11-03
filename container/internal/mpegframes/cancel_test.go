package mpegframes

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestCompleteStopsWithItsContext: a walk whose context has ended returns
// canceled at its next frame and is not Done; the frames indexed so far
// stand, and Complete again finishes the run from there.
func TestCompleteStopsWithItsContext(t *testing.T) {
	const n = 2000 // several windows of frames
	data := frames(n)

	t.Run("before the first step", func(t *testing.T) {
		w, _, _ := begin(t, data, &walkOpts{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := w.Complete(ctx); waxerr.CodeOf(err) != waxerr.CodeCanceled {
			t.Fatalf("Complete = %v, want canceled", err)
		}
		if w.Done() {
			t.Error("a canceled walk reports Done")
		}
		if w.Frames() != 1 {
			t.Errorf("the index holds %d frames after a canceled walk, want Begin's one", w.Frames())
		}
		if err := w.Complete(t.Context()); err != nil || !w.Done() || w.Frames() != n {
			t.Fatalf("Complete after the cancel: %v, Done %v, %d frames; want nil, true, %d", err, w.Done(), w.Frames(), n)
		}
	})

	t.Run("partway through the run", func(t *testing.T) {
		full, counted := countedWalk(t, data)
		if err := full.Complete(t.Context()); err != nil {
			t.Fatal(err)
		}
		fresh := counted.Reads

		src := &testutil.CountingSource{Src: container.BytesSource(data)}
		w := New(src, src.Size(), (&walkOpts{}).options())
		if _, _, err := w.Begin(0); err != nil {
			t.Fatal(err)
		}
		if err := w.Complete(testutil.CancelAfterChecks(t, 700)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
			t.Fatalf("Complete = %v, want canceled", err)
		}
		if w.Done() || w.Frames() <= 1 || w.Frames() >= n {
			t.Fatalf("after the cancel: Done %v with %d of %d frames, want a paused walk partway through", w.Done(), w.Frames(), n)
		}
		if err := w.Complete(t.Context()); err != nil || !w.Done() || w.Frames() != n {
			t.Fatalf("Complete after the cancel: %v, Done %v, %d frames; want nil, true, %d", err, w.Done(), w.Frames(), n)
		}
		if src.Reads != fresh {
			t.Errorf("the paused and resumed walks read %d times, a fresh walk %d: the resume re-read or skipped", src.Reads, fresh)
		}
	})

	t.Run("a finished walk has nothing to stop", func(t *testing.T) {
		w, _, _ := begin(t, data, &walkOpts{})
		if err := w.Complete(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := w.Complete(ctx); err != nil {
			t.Errorf("Complete on a finished walk = %v, want nil whatever the context", err)
		}
	})
}
