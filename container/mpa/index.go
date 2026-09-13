package mpa

import "github.com/colespringer/waxflow/container"

var _ container.Indexer = (*Demuxer)(nil)

// IndexSnapshot implements container.Indexer for the lazy frame index.
func (d *Demuxer) IndexSnapshot() []byte { return d.pkts.Snapshot() }

// RestoreIndex implements container.Indexer. The blob is validated against
// the open source by the walk that would otherwise build the index.
func (d *Demuxer) RestoreIndex(blob []byte) bool { return d.pkts.Restore(blob) }
