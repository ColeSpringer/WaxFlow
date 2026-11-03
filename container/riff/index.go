package riff

import (
	"context"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
)

var (
	_ container.Indexer = (*Demuxer)(nil)
	_ container.Walker  = (*Demuxer)(nil)
)

// KeepsIndex implements container.Indexer: a frame-walked payload keeps one.
// A byte-linear one has nothing worth persisting, since its unit geometry
// puts every position in arithmetic reach, with no walk to skip.
func (d *Demuxer) KeepsIndex() bool {
	_, ok := d.payload.(*mpegframes.Reader)
	return ok
}

// IndexSnapshot implements container.Indexer for a frame-walked payload, and
// answers nil for a byte-linear one.
func (d *Demuxer) IndexSnapshot() []byte {
	if r, ok := d.payload.(*mpegframes.Reader); ok {
		return r.Snapshot()
	}
	return nil
}

// RestoreIndex implements container.Indexer. The blob is validated against
// the open source by the walk that would otherwise build the index.
func (d *Demuxer) RestoreIndex(blob []byte) bool {
	if r, ok := d.payload.(*mpegframes.Reader); ok {
		return r.Restore(blob)
	}
	return false
}

// Walk implements container.Walker for a frame-walked payload: the lazy
// index is finished, so damage past the head reaches Warnings, or under
// Strict the error, and the track reports the walk's count exact, which is
// the length a read of it delivers. A byte-linear or block payload has
// nothing to walk and answers nil.
func (d *Demuxer) Walk(ctx context.Context) error {
	r, ok := d.payload.(*mpegframes.Reader)
	if !ok {
		return nil
	}
	if err := r.Walk(ctx); err != nil {
		return err
	}
	return r.SettleLength(&d.track)
}

// Walked implements container.Walker: true for a payload with nothing to
// walk, and for a frame run whose index already reaches its end.
func (d *Demuxer) Walked() bool {
	if r, ok := d.payload.(*mpegframes.Reader); ok {
		return r.Walked()
	}
	return true
}
