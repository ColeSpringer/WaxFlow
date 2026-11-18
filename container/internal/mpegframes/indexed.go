package mpegframes

import (
	"context"

	"github.com/colespringer/waxflow/container"
)

// Indexed answers container.Indexer and container.Walker for an owner whose
// payload may be a frame run. A nil Reader, as in the zero Indexed, is a
// payload with nothing to index or walk.
type Indexed struct {
	r     *Reader
	track *container.Track
}

// NewIndexed answers for r, nil where the payload is not a frame run, and
// settles track once a walk has counted the run.
func NewIndexed(r *Reader, track *container.Track) Indexed {
	return Indexed{r: r, track: track}
}

// KeepsIndex reports whether there is a frame index to keep.
func (ix Indexed) KeepsIndex() bool {
	return ix.r != nil
}

// IndexSnapshot is the frame index's snapshot, and nil without a frame run.
func (ix Indexed) IndexSnapshot() []byte {
	if ix.r != nil {
		return ix.r.Snapshot()
	}
	return nil
}

// RestoreIndex adopts a snapshot, and declines without a frame run.
func (ix Indexed) RestoreIndex(blob []byte) bool {
	if ix.r != nil {
		return ix.r.Restore(blob)
	}
	return false
}

// Walk finishes the frame index and settles the track at the walk's count,
// and without a frame run has nothing to walk.
func (ix Indexed) Walk(ctx context.Context) error {
	if ix.r == nil {
		return nil
	}
	if err := ix.r.Walk(ctx); err != nil {
		return err
	}
	return ix.r.SettleLength(ix.track)
}

// Walked reports whether the index reaches the end of the run, and true
// without a frame run.
func (ix Indexed) Walked() bool {
	if ix.r != nil {
		return ix.r.Walked()
	}
	return true
}
