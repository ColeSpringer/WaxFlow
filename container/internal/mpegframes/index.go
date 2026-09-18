package mpegframes

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec/mp3"
)

// Index snapshot format, versioned independently of anything else:
// magic, a completeness flag, the entry count, then the frame offsets
// as unsigned varint deltas (the first entry is absolute).
//
// The magic is the one container/mpa wrote before the walk moved here, and
// it stays: the blob describes a run of frame offsets in a source, which is
// the same thing it always was, so sidecars written by an older build still
// restore.
const idxMagic = "WXMPAIDX1\x00"

// IdxMinFrames is the snapshot threshold: below this, rebuilding the
// index costs less than a disk round trip (a header-hop walk covers
// thousands of frames per millisecond).
const IdxMinFrames = 4096

// idxProbes is how many restored offsets get header-verified beyond the
// endpoints.
const idxProbes = 8

// Snapshot serializes the index built so far, or returns nil when it is not
// worth keeping (too small, or unchanged since Restore).
func (w *Walker) Snapshot() []byte {
	if len(w.idx) < IdxMinFrames || !w.grew {
		return nil
	}
	buf := make([]byte, 0, len(idxMagic)+2+10+len(w.idx)*2)
	buf = append(buf, idxMagic...)
	// A run that ended on damage is never persisted as complete, and that is
	// what keeps a sidecar from changing a verdict. The blob has no room for
	// the finding itself, so a restore of one would compare the declared count
	// against the run with the damage forgotten and call a one-frame shortfall
	// a Note where the cold walk called it damage. Marking it incomplete costs
	// the restore only the tail: the walk resumes from the last entry, finds
	// the same damage, and reports it the same way.
	if w.done && !w.endFinding {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = binary.AppendUvarint(buf, uint64(len(w.idx)))
	prev := int64(0)
	for _, off := range w.idx {
		buf = binary.AppendUvarint(buf, uint64(off-prev))
		prev = off
	}
	return buf
}

// Restore adopts a previously snapshotted index and reports whether the blob
// was accepted. The blob is untrusted data: beyond shape checks (monotonic,
// in bounds, count plausible for the source size), the first offset must
// equal the parsed run head and a spread of sampled offsets (endpoints
// included) must parse as kin frame headers, so a blob for a different or
// changed file is rejected and the walk just runs. The completeness flag is
// verified, not adopted: a blob claiming done while a frame continues past
// its last entry only downgrades to an extendable index, never to a
// truncated stream.
func (w *Walker) Restore(blob []byte) bool {
	if len(w.idx) > 1 {
		return false // restoring over a progressed walk would move delivered frames
	}
	if len(blob) < len(idxMagic)+2 || string(blob[:len(idxMagic)]) != idxMagic {
		return false
	}
	rest := blob[len(idxMagic):]
	done := rest[0] == 1
	rest = rest[1:]
	count, n := binary.Uvarint(rest)
	if n <= 0 || count == 0 || count > uint64(w.w.DataEnd()/minFrameLen)+2 {
		return false
	}
	rest = rest[n:]
	// The claimed count is bounded loosely by the source size, which for
	// a large source would let a tiny poisoned blob force a huge
	// allocation; cap the pre-size and let append grow against the
	// blob's actual content (one varint per entry bounds it naturally).
	idx := make([]int64, 0, min(count, 4096))
	prev := int64(-1)
	pos := int64(0)
	for i := uint64(0); i < count; i++ {
		delta, n := binary.Uvarint(rest)
		if n <= 0 || delta > uint64(w.w.DataEnd()) {
			return false
		}
		rest = rest[n:]
		// pos stayed at or below dataEnd last iteration and the delta is
		// bounded by it, so the sum cannot overflow int64.
		pos += int64(delta)
		if pos <= prev || pos > w.w.DataEnd()-mp3.HeaderLen {
			return false
		}
		prev = pos
		idx = append(idx, pos)
	}
	if idx[0] != w.first {
		return false
	}
	// Sample interior offsets as well as the endpoints: full validation
	// would cost the walk the sidecar exists to avoid, but eight header
	// probes catch gross interior corruption. Each probe is an exact read,
	// so a restore costs nine headers rather than nine windows.
	for i := 0; i <= idxProbes; i++ {
		probe := idx[i*(len(idx)-1)/idxProbes]
		if _, ok := w.headerPeek(probe); !ok {
			return false
		}
	}
	// The last entry's frame must fit inside the data, which extend() would
	// have required before indexing it: a blob whose does not describes a
	// source that has since been truncated, and Frame would fail on that
	// entry where a fresh walk drops it with a warning. The probe loop above
	// has already parsed this header; what is new is its length.
	last := idx[len(idx)-1]
	h, ok := w.headerPeek(last)
	if !ok || last+int64(h.Size()) > w.w.DataEnd() {
		return false
	}
	if done {
		// Trust but verify: if a kin frame parses right after the last
		// indexed frame, the run continues and done is a lie.
		if next := last + int64(h.Size()); next <= w.w.DataEnd()-mp3.HeaderLen {
			if _, more := w.headerPeek(next); more {
				done = false
			}
		}
	}
	prevIdx, prevDone := w.idx, w.done
	w.idx, w.done, w.grew = idx, done, false
	if done {
		// A complete blob makes the run's count known without walking it, so
		// the declared count is settled against it here rather than at an end
		// the walk will never reach. A strict owner that refuses the result
		// declines the blob instead, and the rebuild reports it at its own
		// end, where the walk can also say what the run actually holds.
		//
		// Declining costs a good sidecar nothing on the path that keeps one:
		// the engine's index cache rides on OpenStream, which is always
		// tolerant, so this arm is a direct caller's to reach.
		if err := w.compare(); err != nil {
			w.idx, w.done, w.compared = prevIdx, prevDone, false
			return false
		}
	}
	return true
}
