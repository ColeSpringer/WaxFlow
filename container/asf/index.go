package asf

import (
	"math"

	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/internal/trailer"
)

// Simple Index Object field offsets within the object body.
const (
	siInterval = 16
	siCount    = 28
	siEntries  = 32
	siEntryLen = 6 // packet number (4) + packet count (2)
)

// parseIndex scans the objects behind the Data Object for a Simple Index
// Object. Only the first is read: a file with one per stream indexes them all
// on the same time grid, so the first answers for the audio too.
//
// What sits back here is not audio, so nothing found or not found is damage.
// A tagger that appended an ID3v1 block to a .wma is the ordinary case, and it
// is peeled rather than reported; anything else that will not parse as an
// object ends the scan quietly, since a file with no index seeks by bisection
// and needs nothing from this at all.
func (d *Demuxer) parseIndex(from int64) error {
	// Padding is deliberately not in the set. It has no magic, so peeling it
	// takes a caller that can confirm the peel afterwards, and this one
	// cannot: an index table ending in a zero byte is indistinguishable from
	// one with a byte of padding behind it, and peeling that byte makes the
	// index it was part of read as truncated.
	end, _ := trailer.PeelAll(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2, from, d.size)
	// The tags a peel parses are deliberately dropped. ASF states its own in
	// the header, and a block some other tool bolted on the end is not a
	// second opinion the container asked for.
	for off := from; off+objectHeaderLen <= end; {
		b := d.w.BytesAt(off, objectHeaderLen)
		if len(b) < objectHeaderLen {
			return d.w.Err()
		}
		id := guidAt(b)
		size := le.Uint64(b[16:])
		if size < objectHeaderLen || size > uint64(end-off) {
			d.note(off, "trailing object %s declares %d bytes with %d left; the scan for an index stopped there", id, size, end-off)
			return nil
		}
		if id == guidSimpleIndex {
			d.readSimpleIndex(off, int64(size))
			return nil
		}
		off += int64(size)
	}
	return nil
}

// readSimpleIndex loads the index's packet numbers. Anything wrong with the
// table drops it rather than failing the open: seeking falls back to bisecting
// the packets themselves, which needs no table at all.
func (d *Demuxer) readSimpleIndex(off, size int64) {
	head := d.w.BytesAt(off+objectHeaderLen, siEntries)
	if len(head) < siEntries {
		d.note(off, "Simple Index Object is too short to hold its own header; seeking bisects instead")
		return
	}
	interval := int64(le.Uint64(head[siInterval:]))
	count := int64(le.Uint32(head[siCount:]))
	if interval <= 0 || count <= 0 {
		return
	}
	if count > maxIndexEntries {
		d.note(off, "Simple Index Object holds %d entries, past the %d cap; seeking bisects instead", count, maxIndexEntries)
		return
	}
	if want := int64(objectHeaderLen) + siEntries + count*siEntryLen; want > size {
		d.note(off, "Simple Index Object declares %d entries in %d bytes; seeking bisects instead", count, size)
		return
	}
	base := off + objectHeaderLen + siEntries
	index := make([]uint32, 0, count)
	for i := int64(0); i < count; {
		n := min(count-i, int64(srcwin.Chunk/siEntryLen))
		b := d.w.BytesAt(base+i*siEntryLen, int(n*siEntryLen))
		if int64(len(b)) < n*siEntryLen {
			d.note(base, "Simple Index Object is truncated at entry %d; seeking bisects instead", i)
			return
		}
		for j := int64(0); j < n; j++ {
			index = append(index, le.Uint32(b[j*siEntryLen:]))
		}
		i += n
	}
	d.index, d.indexInterval = index, interval
}

// packetFor returns a data packet whose send time is at or before ms, the
// last one where it can find it. ms is in the send-time domain, which is
// presentation time less the pre-roll.
func (d *Demuxer) packetFor(ms int64) int64 {
	if i, ok := d.indexPacket(ms); ok {
		// The index is a position the file supplies, so it is checked against
		// the packet it names rather than trusted. SeekSample would catch a
		// landing past the target anyway, but only by restarting at the top,
		// which turns a seek into a rewind; rejecting the entry here leaves
		// bisection to give a useful answer instead. Pointing early is always
		// legal, so only the one side is tested.
		if t, ok := d.packetSendTime(i); ok && t <= ms {
			return i
		}
	}
	// Send times rise across the file, so bisection finds the boundary. A
	// packet whose header will not parse is treated as past the target, which
	// keeps the landing at or before it.
	lo, hi := int64(0), d.packets-1
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if t, ok := d.packetSendTime(mid); !ok || t > ms {
			hi = mid - 1
		} else {
			lo = mid
		}
	}
	return max(lo, 0)
}

// indexPacket reads the landing out of the Simple Index Object, whose entry i
// names the packet to start at for presentation times in [i, i+1) intervals.
//
// Those are absolute presentation times, which count from the start of the
// pre-roll, so the pre-roll goes back on before the lookup: without it every
// entry is short by preroll/interval, and a file whose pre-roll exceeds its
// own length rewinds fully on every indexed seek.
func (d *Demuxer) indexPacket(ms int64) (int64, bool) {
	if len(d.index) == 0 || d.indexInterval <= 0 || ms < 0 {
		return 0, false
	}
	i := min(scaleToHNS(ms+min(d.prerollMS, math.MaxInt64-ms))/d.indexInterval, int64(len(d.index)-1))
	p := int64(d.index[i])
	if p >= d.packets {
		return 0, false
	}
	return p, true
}

// scaleToHNS converts milliseconds to 100-nanosecond units, saturating rather
// than wrapping. A seek target reaches here from the caller unbounded (the
// engine's own end-of-stream probe is a near-maximum sample position), and a
// wrapped product indexes a table backwards.
func scaleToHNS(ms int64) int64 {
	const scale = hundredNS / 1000
	if ms < 0 {
		return 0
	}
	if ms > math.MaxInt64/scale {
		return math.MaxInt64
	}
	return ms * scale
}
