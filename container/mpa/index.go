package mpa

import "github.com/colespringer/waxflow/container"

var (
	_ container.Indexer = (*Demuxer)(nil)
	_ container.Walker  = (*Demuxer)(nil)
)

// IndexSnapshot implements container.Indexer for the lazy frame index.
func (d *Demuxer) IndexSnapshot() []byte { return d.pkts.Snapshot() }

// RestoreIndex implements container.Indexer. The blob is validated against
// the open source by the walk that would otherwise build the index.
func (d *Demuxer) RestoreIndex(blob []byte) bool { return d.pkts.Restore(blob) }

// Walk implements container.Walker: the lazy frame index is finished, so
// damage past the head of the run reaches Warnings, or under Strict the
// error, without a packet being read. A finished walk has counted the run,
// and the track reports that count exact from then on, whatever a metadata
// frame claimed: the walk is the measurement, and a count it disagrees with
// has already been reported as damage (a shortfall) or a note (an overrun).
func (d *Demuxer) Walk() error {
	if err := d.pkts.Walk(); err != nil {
		return err
	}
	return d.pkts.SettleLength(&d.track)
}

// Walked implements container.Walker: whether the index already reaches the
// end of the run, by a read, a walk, or a restored complete sidecar.
func (d *Demuxer) Walked() bool { return d.pkts.Walked() }
