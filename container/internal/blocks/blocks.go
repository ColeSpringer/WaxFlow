// Package blocks reads a payload laid out as a flat array of fixed-size
// units, where a sample's position is arithmetic, and names the seam such a
// payload shares with a run of MPEG audio frames.
package blocks

import (
	"context"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

var _ Payload = (*Reader)(nil)

// Payload is the walk over a container's audio payload. Two shapes reach it,
// and the difference between them is not a detail of the walk but its whole
// geometry: a flat array of fixed-size units, where position is arithmetic,
// and a run of self-framing MPEG audio frames, where it is an index built by
// walking. That is what earns a seam rather than a flag.
type Payload interface {
	// ReadPacket yields the next run of payload, io.EOF past the end.
	ReadPacket(pkt *container.Packet) error
	// SeekSample repositions to the unit or frame holding a sample and
	// reports the sample that unit starts on.
	SeekSample(ctx context.Context, sample int64) (landed int64, err error)
}

// Reader walks a payload laid out as a flat array of fixed-size units, whose
// sample timeline is the unit index times what a unit decodes to.
type Reader struct {
	src        container.Source
	dataOff    int64
	unitBytes  int64
	unitFrames int64
	units      int64
	samples    int64 // the track's length, at most units times unitFrames
	pos        int64 // next unit to read
	buf        []byte
	readErr    string // what a failed payload read is reported as
}

// NewReader walks units whole units of unitBytes bytes from dataOff, each
// decoding to unitFrames samples, for a track samples long. readErr is the
// text a failed read is reported under.
func NewReader(src container.Source, dataOff, unitBytes, unitFrames, units, samples int64, readErr string) *Reader {
	return &Reader{src: src, dataOff: dataOff, unitBytes: unitBytes,
		unitFrames: unitFrames, units: units, samples: samples, readErr: readErr}
}

// Units is how many whole units the payload holds.
func (r *Reader) Units() int64 { return r.units }

// UnitFrames is how many samples one unit decodes to.
func (r *Reader) UnitFrames() int64 { return r.unitFrames }

// PerPacket is how many units one packet carries: the pipeline's working
// size, in units, and never fewer than one.
func (r *Reader) PerPacket() int64 {
	return max(int64(audio.StandardChunk)/r.unitFrames, 1)
}

// ReadPacket yields the next run of units: up to audio.StandardChunk frames
// of raw interleaved PCM, or the coded units that decode to about as many.
func (r *Reader) ReadPacket(pkt *container.Packet) error {
	remaining := r.units - r.pos
	if remaining <= 0 {
		return io.EOF
	}
	n := min(r.PerPacket(), remaining)
	need := int(n * r.unitBytes)
	if cap(r.buf) < need {
		r.buf = make([]byte, need)
	}
	r.buf = r.buf[:need]
	if err := container.ReadFull(r.src, r.buf, r.dataOff+r.pos*r.unitBytes); err != nil {
		return container.ShortRead(r.readErr, err)
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: r.buf,
			PTS:  r.pos * r.unitFrames,
			Dur:  n * r.unitFrames,
			// Every unit is a place a walk may begin; an owner whose decoder
			// carries state across units lands its seeks at zero.
			Sync: true,
		},
	}
	r.pos += n
	return nil
}

// SeekSample lands on the unit boundary at or before the target.
func (r *Reader) SeekSample(_ context.Context, sample int64) (int64, error) {
	// Clamped to the track's length and not to the payload's, which is larger
	// where the track stops inside the last unit: container.Seeker's landing
	// is authoritative for its callers, and one past the end of the track is a
	// position the track does not have.
	r.pos = min(min(sample, r.samples)/r.unitFrames, r.units)
	return r.pos * r.unitFrames, nil
}
