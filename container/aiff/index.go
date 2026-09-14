package aiff

import (
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
)

var (
	_ container.Indexer = (*Demuxer)(nil)
	_ container.Walker  = (*Demuxer)(nil)
)

// IndexSnapshot implements container.Indexer for a frame-walked payload.
//
// A byte-linear one answers nil, and the answer matters: format.Media
// advertises the capability by type assertion, so this type carries it
// whatever its payload holds and a PCM track has to say for itself that
// there is nothing worth persisting. There is not: its unit geometry puts
// every position in arithmetic reach, with no walk to skip.
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
// Strict the error, and a track whose length was unknown or advisory
// reports the walk's count exact. A byte-linear or block payload has
// nothing to walk and answers nil.
func (d *Demuxer) Walk() error {
	r, ok := d.payload.(*mpegframes.Reader)
	if !ok {
		return nil
	}
	if err := r.Walk(); err != nil {
		return err
	}
	if n := r.Measured(); n >= 0 && (d.track.Samples < 0 || d.track.SamplesAdvisory) {
		d.track.Samples, d.track.SamplesExact, d.track.SamplesAdvisory = n, true, false
	}
	return nil
}

// Walked implements container.Walker: true for a payload with nothing to
// walk, and for a frame run whose index already reaches its end.
func (d *Demuxer) Walked() bool {
	if r, ok := d.payload.(*mpegframes.Reader); ok {
		return r.Walked()
	}
	return true
}
