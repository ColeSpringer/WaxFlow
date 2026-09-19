package mp4

import (
	"math/bits"

	"github.com/colespringer/waxflow/container"
)

// A segment index (sidx) is the one place a fragmented movie routinely states
// its own length. The default fragmented shape carries no edit list and zeroed
// header durations, so without this a file that says twice how long it is opens
// with no length at all and every consumer measures it by hand.
//
// maxSidxBytes bounds one in-memory sidx payload. A sidx is 12 bytes per
// reference, so four MiB is over 300000 subsegments: larger is a hostile input
// rather than an index, and is skipped rather than read.
const maxSidxBytes = 4 << 20

// sidxSumCap saturates the byte and duration sums. It is far past any real
// index and far short of the int64 ceiling, so coverageEnd's three-term sum
// cannot overflow whatever a crafted box declares; a sum that reaches it is
// damage, which the coverage rule then reports as such.
const sidxSumCap = 1 << 60

// sidxInfo is one parsed segment index, reduced to what a length resolver
// needs: which track it describes, the time base its durations are in, the
// sums, and where its coverage starts.
type sidxInfo struct {
	referenceID uint32
	timescale   uint32
	// firstOffset is the distance from the byte after this box to the first
	// referenced item, so coverage runs [end+firstOffset, end+firstOffset+sumSize).
	firstOffset int64
	end         int64 // the byte just past this sidx box
	sumDuration int64
	sumSize     int64
	refs        int
}

// coverageEnd is the byte the last referenced item ends at.
func (s sidxInfo) coverageEnd() int64 { return s.end + s.firstOffset + s.sumSize }

// parseSidx reads a sidx payload into its sums.
//
// Both reference types sum the same way. A type-1 reference points at a child
// sidx rather than at media, but its subsegment_duration still covers the same
// span of the presentation and its referenced_size still covers the same span
// of bytes (the child box included), so a hierarchical index totals to exactly
// what a flat one over the same media would.
//
// The sums saturate rather than wrap. reference_count is a uint16 and
// referenced_size a uint31, so a real index cannot come close to the cap; the
// saturation is so a crafted one cannot wrap into a small plausible number.
func parseSidx(payload []byte) (sidxInfo, error) {
	version, _, rest, ok := fullBox(payload)
	if !ok {
		return sidxInfo{}, malformed("sidx truncated")
	}
	var s sidxInfo
	if len(rest) < 8 {
		return sidxInfo{}, malformed("sidx truncated")
	}
	s.referenceID, s.timescale = be32(rest), be32(rest[4:])
	rest = rest[8:]
	// version 0 times the box in 32 bits, version 1 in 64. Only first_offset
	// is read: earliest_presentation_time is where the segment sits on the
	// presentation timeline, which a length does not need.
	if version == 0 {
		if len(rest) < 8 {
			return sidxInfo{}, malformed("sidx truncated")
		}
		s.firstOffset = int64(be32(rest[4:]))
		rest = rest[8:]
	} else {
		if len(rest) < 16 {
			return sidxInfo{}, malformed("sidx truncated")
		}
		off := be64(rest[8:])
		if off > sidxSumCap {
			return sidxInfo{}, malformed("sidx first_offset %d", off)
		}
		s.firstOffset = int64(off)
		rest = rest[8+8:]
	}
	if len(rest) < 4 {
		return sidxInfo{}, malformed("sidx truncated")
	}
	count := int(be32(rest) & 0xFFFF) // reserved(16) || reference_count(16)
	rest = rest[4:]
	// Validated against the payload before the loop, as parseTrun does: a
	// count the bytes cannot hold is a malformed box, not a short read.
	if 12*count > len(rest) {
		return sidxInfo{}, malformed("sidx declares %d references for %d bytes", count, len(rest))
	}
	s.refs = count
	for range count {
		size := int64(be32(rest) & 0x7FFFFFFF) // reference_type(1) || referenced_size(31)
		dur := int64(be32(rest[4:]))
		rest = rest[12:]
		s.sumSize = addSat(s.sumSize, size)
		s.sumDuration = addSat(s.sumDuration, dur)
	}
	return s, nil
}

// addSat adds two non-negative int64s, saturating rather than wrapping.
func addSat(a, b int64) int64 {
	sum, carry := bits.Add64(uint64(a), uint64(b), 0)
	if carry != 0 || sum > sidxSumCap {
		return sidxSumCap
	}
	return int64(sum)
}

// readSidxAhead finds the segment index describing sel, if the movie carries
// one that covers the whole file.
//
// It reads box headers from fragStart and stops at the first moof: a sidx that
// describes a movie's fragments is written ahead of them, and running on into
// the fragments would be the whole-file walk the open exists not to do. Every
// other box is skipped by its header, an mdat included, because a hybrid
// movie's own sample data sits between its moov and its fragments and a sidx
// written after that must still be found.
//
// The coverage rule is what separates an index of the movie from an index of
// its first fragment: complete means coverage reaches the end of the file, or
// the box it stops at is not more of the same segment stream. It is a denylist
// (moof, styp, sidx) rather than a list of trailing boxes to accept, because
// the boxes that may follow the last fragment are open-ended (mfra, free, skip,
// udta, a writer's own) while the ones that mean "more segments follow" are
// three. Both shapes are real: ffmpeg's +global_sidx writes one index over
// every fragment and then an mfra, so its coverage stops one box short of the
// file, and its +dash writes one index per fragment, so the first covers a
// sixth of the length and must not be believed.
//
// Unlike the header-duration tier below, this is not gated on bareSegments, and
// the difference is what each number counts. A header duration describes a
// presentation that a bare segment source holds one piece of, so reading it
// there states the whole presentation's length for one segment. A sidx counts
// subsegments of this source: over a stream of several it covers a prefix and
// is ignored, and over a single segment it is that source's own length, which
// is the right answer.
//
// A read failure ends the scan with no index, the way nextFragment's does:
// nothing here is worth failing an open over, and the caller falls through to
// the next tier. The one error it does return is the strict-mode escalation of
// the truncation warning below, which is the open refusing as Strict asks.
func (d *Demuxer) readSidxAhead(sel *track) (sidxInfo, bool, error) {
	var found sidxInfo
	have := false
	off := d.fragStart
	for off < d.size {
		b, err := readBox(d.src, off, d.size)
		if err != nil {
			// The box chain past the index is damaged. That ends the scan, it
			// does not discard what the scan already found: an index whose
			// fragments are truncated is exactly the case the coverage rule
			// below reports, and throwing it away here would lose the only
			// statement the file makes about its own length.
			break
		}
		if b.typ == "moof" {
			break
		}
		if b.typ == "sidx" && !have && b.payloadLen() <= maxSidxBytes {
			buf := make([]byte, b.payloadLen())
			if err := container.ReadFull(d.src, buf, b.payloadOff()); err != nil {
				break
			}
			if s, err := parseSidx(buf); err == nil && (sel.id <= 0 || s.referenceID == uint32(sel.id)) {
				s.end = b.off + b.size
				found, have = s, true
			}
		}
		if b.toEnd {
			break
		}
		off = b.off + b.size
	}
	if !have || found.sumDuration <= 0 || found.sumSize <= 0 {
		return sidxInfo{}, false, nil
	}
	end := found.coverageEnd()
	switch {
	case end == d.size:
		return found, true, nil
	case end > d.size:
		// The index describes more than the file holds: a truncated download.
		// The count is kept (it is what the writer meant to deliver) and the
		// shortfall is named, so a strict open refuses and a tolerant one says
		// how much is missing rather than silently promising it.
		if err := d.warn(found.end, "sidx describes %d bytes past the end of the source", end-d.size); err != nil {
			return sidxInfo{}, false, err
		}
		return found, true, nil
	}
	// Coverage ends inside the file. It is complete only if what follows is not
	// more of the same segment stream: ffmpeg's trailing mfra, an mdat of some
	// other track, anything the index was never describing. A moof, a styp or
	// another sidx there means this index covered a prefix.
	b, err := readBox(d.src, end, d.size)
	if err != nil {
		return sidxInfo{}, false, nil
	}
	switch b.typ {
	case "moof", "styp", "sidx":
		return sidxInfo{}, false, nil
	}
	return found, true, nil
}
