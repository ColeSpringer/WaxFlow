package asf

import (
	"fmt"
	"math"
	"time"

	"github.com/colespringer/waxflow/container"
)

// The Marker Object names points on the presentation timeline, which is how a
// WMA audiobook carries its chapters and how ffmpeg's ASF muxer writes an
// input's chapter list. Its entries go into the chapter list as stored,
// presentation times with the pre-roll still in them, and move onto the
// playback timeline once the header walk is done: the File Properties Object
// holding the pre-roll may come before or after this one, and the
// specification fixes no order between them.

// Marker Object layout. The body opens with a reserved GUID, the marker
// count, a reserved word, and the object's own name behind its length; each
// entry is an offset, the presentation time, an entry length, a send time,
// flags, and the description length in WCHARs, then the description. The walk
// steps by the description length alone and never by the entry length field:
// ffprobe lists the same chapters whatever that field says
// (TestEntryLengthIsIgnoredLikeFFprobe), so neither does this.
const (
	mkCount      = 16 // the marker count
	mkNameLen    = 22 // the name length, in bytes
	mkPrologue   = 24 // the fixed part of the body, ahead of the name
	mkPresTime   = 8  // an entry's presentation time
	mkDescLen    = 26 // an entry's description length, in WCHARs
	mkEntryFixed = 30 // the fixed part of an entry, ahead of the description
)

// parseMarkers reads the Marker Object into the chapter list, times as
// stored. Every entry counts against the marker cap whether or not it is
// kept, so a run of unplaceable entries cannot spend unbounded work, and each
// fault is reported once for the object rather than once per entry, since the
// warning list has a budget the whole file shares. A count the object cannot
// back is damage, kept entries and all, and so is a description that runs
// past the object, which ends the walk since nothing readable follows it
// (ffprobe drops that entry; it is kept here with what it holds, its time
// being intact). The cap is a limit of this build, so it is a note, unless
// the count past it is one the object's own size rules out.
func (d *Demuxer) parseMarkers(b []byte, off int64) error {
	if d.markerObject {
		return d.warn(off, "a second Marker Object; its markers are not read")
	}
	d.markerObject, d.markersOff = true, off
	if len(b) < mkPrologue {
		return d.warn(off, "Marker Object is %d bytes, not %d", len(b), mkPrologue)
	}
	count := le.Uint32(b[mkCount:])
	p := mkPrologue + int(le.Uint16(b[mkNameLen:]))
	if p > len(b) {
		return d.warn(off+mkNameLen, "Marker Object name of %d bytes runs past the object", p-mkPrologue)
	}
	// The object holds at most as many entries as its bytes allow, which
	// bounds the list's capacity by its size rather than by a count that may
	// lie.
	room := uint64(len(b)-p) / mkEntryFixed
	d.chapters = make([]container.Chapter, 0, min(uint64(count), room, maxMarkers))
	var fault error
	var past, long, firstPast uint32 // times past any duration; descriptions past the tag cap
	for i := uint32(0); i < count; i++ {
		if i >= maxMarkers {
			if uint64(count) > room {
				fault = d.warn(off+mkCount, "the Marker Object declares %d markers and has room for at most %d", count, room)
			} else {
				d.note(off+int64(p), "more than %d markers; the rest are not read", maxMarkers)
			}
			break
		}
		at := p
		if at+mkEntryFixed > len(b) {
			fault = d.warn(off+mkCount, "the Marker Object declares %d markers and holds %d", count, i)
			break
		}
		hns := le.Uint64(b[at+mkPresTime:])
		// The length is a 32-bit field and int is 32 bits on some builds, so
		// it is doubled and bounded in 64 bits before it is narrowed.
		descLen := uint64(le.Uint32(b[at+mkDescLen:])) * 2
		p = at + mkEntryFixed
		cut := descLen > uint64(len(b)-p)
		if cut {
			descLen = uint64(len(b) - p)
		}
		desc := b[p : p+int(descLen)]
		p += int(descLen)
		if hns > math.MaxInt64/100 {
			if past == 0 {
				firstPast = i + 1
			}
			past++
		} else {
			if len(desc) > maxTagBytes {
				desc = desc[:maxTagBytes]
				long++
			}
			d.chapters = append(d.chapters, container.Chapter{Start: time.Duration(hns) * 100, Title: utf16String(desc)})
		}
		if cut {
			fault = d.warn(off+int64(at+mkDescLen), "marker %d's description runs past the Marker Object; nothing after it is read", i+1)
			break
		}
	}
	if past > 0 {
		if err := d.warn(off, "marker %d is at a presentation time past any duration; skipped%s", firstPast, andMore(past)); err != nil {
			return err
		}
	}
	if long > 0 {
		d.note(off, "%d marker descriptions were cut at %d bytes", long, maxTagBytes)
	}
	return fault
}

// andMore words the rest of a fault reported once for the object.
func andMore(n uint32) string {
	if n <= 1 {
		return ""
	}
	return fmt.Sprintf(" (and %d more)", n-1)
}

// resolveChapters moves the chapters onto the playback timeline once the
// header walk has read the pre-roll: it comes off every presentation time, as
// it does off the play duration. A marker inside the pre-roll lands at the
// start, with a note: ffprobe reports it at a negative time, and no output
// has such a place. A marker past the play duration is kept and reported, as
// container/mpc keeps a chapter past the end. The list is then sorted by
// start, which the Chapterer contract promises and the object does not. End
// stays zero, the start-only form: a marker names a point.
func (d *Demuxer) resolveChapters() error {
	if len(d.chapters) == 0 {
		return nil
	}
	preroll := time.Duration(d.prerollMS) * time.Millisecond
	end := time.Duration(-1)
	if d.playHNS != 0 && d.playHNS <= math.MaxInt64/100 {
		end = time.Duration(d.playHNS) * 100
	}
	var early, late int
	for i, ch := range d.chapters {
		if ch.Start < preroll {
			early++
		}
		if end >= 0 && ch.Start > end {
			late++
		}
		d.chapters[i].Start = max(0, ch.Start-preroll)
	}
	if early > 0 {
		d.note(d.markersOff, "%d markers sit inside the pre-roll and were placed at the start", early)
	}
	if late > 0 {
		if err := d.warn(d.markersOff, "%d markers start past the end of the stream", late); err != nil {
			return err
		}
	}
	container.SortChapters(d.chapters)
	return nil
}
