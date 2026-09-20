package loudness

import (
	"fmt"

	"github.com/colespringer/waxflow/waxerr"
)

// GroupVersion identifies the group accumulator's revision, for a cache
// key that stores a group measurement (ADR-0004). It rides beside Version
// rather than replacing it: a group is the same meter over more blocks, so
// a change to either moves the answer.
//
// Like Version, nothing in this tree reads it. A cache key is the caller's
// to build, and a result reports numbers rather than the revision that
// produced them.
const GroupVersion = "r128-group-1"

// Group measures a set of streams as one programme, which is what an album
// normalize asks for: one gain for every member, from the gates run over
// all of them at once.
//
// It is not a concatenation, and the difference is deliberate. Each member
// is metered on its own, at its own delivered width, and Add folds in the
// blocks that survived its absolute gate. Members therefore do not share
// 100 ms sub-blocks: a member's partial final sub-block is dropped, exactly
// as the standard already drops a stream's own. The cost is at most 100 ms
// of audio per member against a true concatenation, which is far inside the
// tolerance any two conformant meters differ by.
//
// Measuring per member is the point rather than a convenience. A member is
// delivered at its own width, and a fold of one width is not a fold of
// another (ADR-0010): measuring a mono member inside a stereo envelope
// reads it 3.01 dB hot, because the envelope duplicates it. Add takes the
// meter the member was actually measured on, so the width question is
// settled before the group ever sees a number.
//
// The zero Group is ready to use. Not safe for concurrent use.
type Group struct {
	blocks []float64
	st     []float64
	maxSP  float64
	maxTP  float64
	n      int
}

// Add folds a member's finished measurement into the group. The meter must
// have been flushed: an unflushed one is still holding a true-peak tail and
// a partial sub-block, so its numbers are not the member's.
//
// Each member goes in once. A meter added twice counts its blocks twice,
// which is not detected here: one meter legitimately belongs to more than
// one group (an album and the disc inside it), so there is no per-meter flag
// that could tell the two apart, and a per-group set would pin every
// member's blocks in memory for the group's lifetime.
func (g *Group) Add(m *Meter) error {
	if m == nil {
		return waxerr.New(waxerr.CodeInternal, "loudness: nil meter added to a group")
	}
	if !m.flushed {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("loudness: member %d added to a group before Flush", g.n))
	}
	g.blocks = append(g.blocks, m.blocks...)
	g.st = append(g.st, m.st...)
	g.maxSP = max(g.maxSP, m.maxSP)
	g.maxTP = max(g.maxTP, m.maxTP)
	g.n++
	return nil
}

// Members reports how many meters have been added.
func (g *Group) Members() int { return g.n }

// Integrated returns the group's gated integrated loudness in LUFS, the
// same computation Meter.Integrated runs over one stream's blocks. Returns
// math.Inf(-1) when no member had a block pass the absolute gate.
func (g *Group) Integrated() float64 { return integratedOf(g.blocks) }

// Range returns the group's loudness range in LU over every member's
// short-term windows. Returns 0 when there are not enough windows to have a
// range, which Meter.Range's doc explains.
func (g *Group) Range() float64 { return rangeOf(g.st) }

// TruePeak returns the highest true peak any member reached, in dBTP.
func (g *Group) TruePeak() float64 { return dbOrNegInf(g.maxTP) }

// SamplePeak returns the highest sample magnitude any member reached, in
// dBFS.
func (g *Group) SamplePeak() float64 { return dbOrNegInf(g.maxSP) }
