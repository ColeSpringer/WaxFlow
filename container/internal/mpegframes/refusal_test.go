package mpegframes

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestARefusalIsKept: a walk the hook refused on its last arm (trailing
// bytes) is not Done, and Complete refuses again rather than settling.
func TestARefusalIsKept(t *testing.T) {
	data := append(frames(6), bytes.Repeat([]byte{0x11}, 16)...)
	boom := errors.New("strict")
	o := walkOpts{fail: boom}
	w, _, _ := begin(t, data, &o)
	if err := w.Complete(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Complete = %v, want the hook's refusal", err)
	}
	if w.Done() {
		t.Error("a walk the hook refused reports Done")
	}
	if err := w.Complete(context.Background()); !errors.Is(err, boom) {
		t.Errorf("the second Complete = %v, want the refusal again", err)
	}
	if w.Done() {
		t.Error("the second Complete turned a refused walk Done")
	}
}

// TestADeclinedRestoreKeepsAnEarlierRefusal: a walk the hook refused stays
// refused when a snapshot it declines is offered afterwards.
func TestADeclinedRestoreKeepsAnEarlierRefusal(t *testing.T) {
	const n = IdxMinFrames + 50
	data := append(xingFrame(n+9, false, 0, 0), frames(n)...)
	build, _, _ := begin(t, data, &walkOpts{})
	if err := build.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	blob := build.Snapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}

	// Junk after frame 0, so a strict walk refuses at the first extension
	// and the index holds one frame, which Restore still accepts an offer on.
	damaged := append(append(xingFrame(n+9, false, 0, 0), frames(1)...), bytes.Repeat([]byte{0x11}, 16)...)
	damaged = append(damaged, frames(n-1)...)
	boom := errors.New("strict")
	w, _, _ := begin(t, damaged, &walkOpts{fail: boom})
	if err := w.Complete(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Complete = %v, want the refusal", err)
	}
	if w.Restore(blob) {
		t.Fatal("a strict walker adopted a blob whose count it refuses")
	}
	if err := w.Complete(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Complete after the declined Restore = %v, want the earlier refusal kept", err)
	}
}

// TestADeadContextStopsNoLandingThatWalksNothing: a landing on a finished
// index, past the end included, walks nothing and answers whatever ctx says.
func TestADeadContextStopsNoLandingThatWalksNothing(t *testing.T) {
	w, _, _ := begin(t, frames(6), &walkOpts{})
	if err := w.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want, err := w.Landing(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.Landing(ctx, 1000); err != nil || got != want {
		t.Errorf("Landing past the end under an ended context = %d, %v; want %d and nil", got, err, want)
	}
}
