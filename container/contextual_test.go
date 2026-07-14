package container_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// ctxSource is a network-backed Source's shape: reads bound to a context held
// in a field, because io.ReaderAt has nowhere else to put one.
type ctxSource struct {
	data []byte
	ctx  context.Context
}

func (s *ctxSource) Size() int64 { return int64(len(s.data)) }

func (s *ctxSource) ReadAt(p []byte, off int64) (int, error) {
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return copy(p, s.data[off:]), nil
}

func (s *ctxSource) WithContext(ctx context.Context) container.Source {
	return &ctxSource{data: s.data, ctx: ctx}
}

func TestBindContextBindsAContextualSource(t *testing.T) {
	base := &ctxSource{data: []byte("hello")}
	ctx, cancel := context.WithCancel(context.Background())
	bound := container.BindContext(ctx, base)

	buf := make([]byte, 5)
	if _, err := bound.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt on a live ctx: %v", err)
	}
	// The receiver is unchanged, so the caller may still hold both.
	if base.ctx != nil {
		t.Error("WithContext mutated the receiver")
	}

	cancel()
	if _, err := bound.ReadAt(buf, 0); err == nil {
		t.Error("ReadAt after cancel returned nil error; the bound ctx is not honored")
	}
	if _, err := base.ReadAt(buf, 0); err != nil {
		t.Errorf("the unbound receiver must be unaffected by the cancel: %v", err)
	}
}

// offsetSource is the shape of the wrappers that exist inside format: it hides
// a leading region from drivers by offsetting reads inward. It does not
// implement Contextual, which is the point of the test below.
type offsetSource struct {
	src container.Source
	off int64
}

func (s offsetSource) Size() int64 { return s.src.Size() - s.off }

func (s offsetSource) ReadAt(p []byte, off int64) (int, error) {
	return s.src.ReadAt(p, off+s.off)
}

// TestWrappingABoundSourceKeepsTheContext pins the ordering invariant the
// Contextual doc states: bind at the outermost Source, then wrap. A wrapper
// carries the binding by delegating reads inward and needs no WithContext of
// its own, so the internal wrappers do not implement Contextual and do not have
// to. Reversing the order is what would drop the ctx.
func TestWrappingABoundSourceKeepsTheContext(t *testing.T) {
	base := &ctxSource{data: []byte("hello world")}
	ctx, cancel := context.WithCancel(context.Background())

	// Bind first, wrap second: the order the engine uses.
	wrapped := offsetSource{src: container.BindContext(ctx, base), off: 6}

	buf := make([]byte, 5)
	if _, err := wrapped.ReadAt(buf, 0); err != nil {
		t.Fatalf("read through the wrapper on a live ctx: %v", err)
	}
	if string(buf) != "world" {
		t.Errorf("read %q through the offset wrapper, want %q", buf, "world")
	}

	cancel()
	if _, err := wrapped.ReadAt(buf, 0); err == nil {
		t.Error("read through the wrapper succeeded after cancel: the binding did not survive wrapping")
	}
}

// TestBindContextPassesThroughAPlainSource is the honest half of the gate: a
// file-backed source has no ctx to bind, so it does not implement Contextual
// and BindContext must hand it back untouched rather than wrap it.
func TestBindContextPassesThroughAPlainSource(t *testing.T) {
	src := container.BytesSource([]byte("hello"))
	if got := container.BindContext(context.Background(), src); got != src {
		t.Error("BindContext wrapped a non-Contextual source; it must pass through unchanged")
	}
	if _, ok := src.(container.Contextual); ok {
		t.Error("BytesSource implements Contextual; the gate would stop being a capability check")
	}
}
