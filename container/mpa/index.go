package mpa

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
)

var _ container.Indexer = (*Demuxer)(nil)

// Index snapshot format, versioned independently of anything else:
// magic, a completeness flag, the entry count, then the frame offsets
// as unsigned varint deltas (the first entry is absolute).
const idxMagic = "WXMPAIDX1\x00"

// idxMinFrames is the snapshot threshold: below this, rebuilding the
// index costs less than a disk round trip (a header-hop walk covers
// thousands of frames per millisecond).
const idxMinFrames = 4096

// idxProbes is how many restored offsets get header-verified beyond the
// endpoints.
const idxProbes = 8

// IndexSnapshot implements container.Indexer for the lazy frame index.
func (d *Demuxer) IndexSnapshot() []byte {
	if len(d.idx) < idxMinFrames || !d.grew {
		return nil
	}
	buf := make([]byte, 0, len(idxMagic)+2+10+len(d.idx)*2)
	buf = append(buf, idxMagic...)
	if d.done {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = binary.AppendUvarint(buf, uint64(len(d.idx)))
	prev := int64(0)
	for _, off := range d.idx {
		buf = binary.AppendUvarint(buf, uint64(off-prev))
		prev = off
	}
	return buf
}

// RestoreIndex implements container.Indexer. The blob is untrusted data:
// beyond shape checks (monotonic, in bounds, count plausible for the
// source size), the first offset must equal the parsed stream head and a
// spread of sampled offsets (endpoints included) must parse as kin frame
// headers, so a blob for a different or changed file is rejected and the
// demuxer just walks. The completeness flag is verified, not adopted: a
// blob claiming done while a frame continues past its last entry only
// downgrades to an extendable index, never to a truncated stream.
func (d *Demuxer) RestoreIndex(blob []byte) bool {
	if len(d.idx) > 1 || d.cur != 0 {
		return false // restoring over a progressed walk would move delivered frames
	}
	if len(blob) < len(idxMagic)+2 || string(blob[:len(idxMagic)]) != idxMagic {
		return false
	}
	rest := blob[len(idxMagic):]
	done := rest[0] == 1
	rest = rest[1:]
	count, n := binary.Uvarint(rest)
	if n <= 0 || count == 0 || count > uint64(d.w.DataEnd()/minFrameLen)+2 {
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
		if n <= 0 || delta > uint64(d.w.DataEnd()) {
			return false
		}
		rest = rest[n:]
		// pos stayed at or below dataEnd last iteration and the delta is
		// bounded by it, so the sum cannot overflow int64.
		pos += int64(delta)
		if pos <= prev || pos > d.w.DataEnd()-mp3.HeaderLen {
			return false
		}
		prev = pos
		idx = append(idx, pos)
	}
	if idx[0] != d.firstFrame {
		return false
	}
	// Sample interior offsets as well as the endpoints: full validation
	// would cost the walk the sidecar exists to avoid, but eight header
	// probes catch gross interior corruption.
	for i := 0; i <= idxProbes; i++ {
		probe := idx[i*(len(idx)-1)/idxProbes]
		if _, ok := d.frameAt(probe); !ok {
			return false
		}
	}
	if done {
		// Trust but verify: if a kin frame parses right after the last
		// indexed frame, the stream continues and done is a lie.
		last := idx[len(idx)-1]
		if h, ok := d.frameAt(last); ok {
			if next := last + int64(h.Size()); next <= d.w.DataEnd()-mp3.HeaderLen {
				if _, more := d.frameAt(next); more {
					done = false
				}
			}
		}
	}
	d.idx = idx
	d.done = done
	d.grew = false
	return true
}
