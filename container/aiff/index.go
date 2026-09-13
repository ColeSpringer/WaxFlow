package aiff

import (
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
)

var _ container.Indexer = (*Demuxer)(nil)

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
