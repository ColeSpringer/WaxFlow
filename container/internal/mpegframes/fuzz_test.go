package mpegframes

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// FuzzWalk drives the walk over arbitrary bytes: Begin, the finishing walk a
// strict probe runs, then every frame the index holds. What it pins is the
// walk's own invariants, since nothing here decodes: no panic on any input,
// a finished walk is Done and stable, a hook that refuses stops the walk with
// its error, and the frames the index delivers are whole and kin.
func FuzzWalk(f *testing.F) {
	f.Add(frames(6))
	f.Add(append([]byte("not audio, sorry"), frames(3)...))
	f.Add(append(xingFrame(7, true, 576, 1000), frames(7)...))
	mid := frames(5)
	copy(mid[2*frameLen:], []byte{0, 0, 0, 0})
	f.Add(mid)
	f.Add(frames(3)[:2*frameLen+100])
	f.Add(append(frames(3), []byte("TAG this is an ID3v1 trailer, near enough")...))
	f.Add([]byte{0xFF, 0xFB, 0x00, 0x00, 1, 2, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		var o walkOpts
		w := New(container.BytesSource(data), int64(len(data)), o.options())
		if _, _, err := w.Begin(0); err != nil {
			return
		}
		if err := w.Complete(); err != nil {
			t.Fatalf("a tolerant Complete failed: %v", err)
		}
		if !w.Done() {
			t.Fatal("not Done after Complete")
		}
		found := len(o.msgs)
		if err := w.Complete(); err != nil || len(o.msgs) != found {
			t.Fatalf("a second Complete returned %v and reported %d more findings", err, len(o.msgs)-found)
		}
		ref := w.Header()
		for n := int64(0); ; n++ {
			fr, err := w.Frame(n)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Frame(%d) after Complete: %v", n, err)
			}
			if len(fr.Data) != fr.Header.Size() || !ref.Kin(fr.Header) {
				t.Fatalf("Frame(%d) is %d bytes for a %d-byte header, kin %v", n, len(fr.Data), fr.Header.Size(), ref.Kin(fr.Header))
			}
		}
		// The strict half: a refusing hook stops at the first finding, and
		// only where there was one.
		strict := walkOpts{fail: errors.New("strict")}
		ws := New(container.BytesSource(data), int64(len(data)), strict.options())
		if _, _, err := ws.Begin(0); err != nil {
			return
		}
		if err := ws.Complete(); (err != nil) != (found > 0) {
			t.Fatalf("strict Complete returned %v where the tolerant walk found %d things", err, found)
		}
	})
}
