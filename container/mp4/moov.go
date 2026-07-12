package mp4

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// track is one parsed trak: its handler, media timescale, sample entry
// codec/config/format, flattened sample table, and edit list.
type track struct {
	id        int    // tkhd track_ID
	handler   string // mdia/hdlr handler_type: "soun", "vide", "text", ...
	timescale int64  // mdhd timescale (ticks per second)
	duration  int64  // mdhd duration in timescale ticks

	codec       codec.ID
	codecConfig []byte
	fmt         audio.Format

	st sampleTable

	// First edit-list entry: a nonzero media time is the encoder-delay
	// priming for gapless (iTunes writes it), segment duration the playable
	// length in movie ticks.
	hasEdit    bool
	editMedia  int64 // media_time in media timescale ticks (-1 = empty edit)
	editSegDur int64 // segment_duration in movie timescale ticks

	// chapRefs are track_IDs referenced as chapter tracks ('chap' tref).
	chapRefs []int
}

// parseMoov walks the movie box, returning the parsed tracks. It records
// the movie timescale and Nero chapter list on the demuxer.
func (d *Demuxer) parseMoov(moov []byte) ([]*track, error) {
	// Pre-scan for mvex so parseStbl knows the movie is fragmented (its sample
	// tables are empty by design) before it parses any trak; mvex trails the
	// traks in the box order, so it cannot be discovered during the main walk.
	_ = walkBoxes(moov, func(typ string, payload []byte) error {
		if typ == "mvex" {
			d.parseMvex(payload)
		}
		return nil
	})
	var tracks []*track
	err := walkBoxes(moov, func(typ string, payload []byte) error {
		switch typ {
		case "mvhd":
			d.movieTimescale = mvhdTimescale(payload)
		case "trak":
			if len(tracks) >= maxTracks {
				return malformed("more than %d tracks", maxTracks)
			}
			t := &track{editMedia: -1}
			if err := d.parseTrak(t, payload, 1); err != nil {
				return err
			}
			tracks = append(tracks, t)
		case "udta":
			return d.parseUdta(payload, 1)
		}
		return nil
	})
	return tracks, err
}

// mvhdTimescale extracts the movie timescale from an mvhd box.
func mvhdTimescale(payload []byte) int64 {
	version, _, rest, ok := fullBox(payload)
	if !ok {
		return 0
	}
	// version 0: creation(4) modification(4) timescale(4) duration(4)
	// version 1: creation(8) modification(8) timescale(4) duration(8)
	off := 8
	if version == 1 {
		off = 16
	}
	if len(rest) < off+4 {
		return 0
	}
	return int64(be32(rest[off:]))
}

func (d *Demuxer) parseTrak(t *track, body []byte, depth int) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	return walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "tkhd":
			t.id = tkhdTrackID(payload)
		case "edts":
			return walkBoxes(payload, func(t2 string, p2 []byte) error {
				if t2 == "elst" {
					parseElst(t, p2)
				}
				return nil
			})
		case "tref":
			parseTref(t, payload)
		case "mdia":
			return d.parseMdia(t, payload, depth+1)
		}
		return nil
	})
}

// tkhdTrackID extracts the track_ID from a tkhd box.
func tkhdTrackID(payload []byte) int {
	version, _, rest, ok := fullBox(payload)
	if !ok {
		return 0
	}
	// version 0: creation(4) modification(4) track_ID(4)
	// version 1: creation(8) modification(8) track_ID(4)
	off := 8
	if version == 1 {
		off = 16
	}
	if len(rest) < off+4 {
		return 0
	}
	return int(be32(rest[off:]))
}

func (d *Demuxer) parseMdia(t *track, body []byte, depth int) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	return walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "mdhd":
			t.timescale, t.duration = mdhdTime(payload)
		case "hdlr":
			t.handler = hdlrType(payload)
		case "minf":
			return d.parseMinf(t, payload, depth+1)
		}
		return nil
	})
}

// mdhdTime extracts the media timescale and duration from an mdhd box.
func mdhdTime(payload []byte) (timescale, duration int64) {
	version, _, rest, ok := fullBox(payload)
	if !ok {
		return 0, 0
	}
	if version == 1 {
		// creation(8) modification(8) timescale(4) duration(8)
		if len(rest) < 28 {
			return 0, 0
		}
		return int64(be32(rest[16:])), int64(be64(rest[20:]))
	}
	// creation(4) modification(4) timescale(4) duration(4)
	if len(rest) < 16 {
		return 0, 0
	}
	return int64(be32(rest[8:])), int64(be32(rest[12:]))
}

// hdlrType extracts the four-character handler type from an hdlr box.
func hdlrType(payload []byte) string {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 8 {
		return ""
	}
	// pre_defined(4) handler_type(4)
	return trimBrand(rest[4:8])
}

func (d *Demuxer) parseMinf(t *track, body []byte, depth int) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	return walkBoxes(body, func(typ string, payload []byte) error {
		if typ == "stbl" {
			return d.parseStbl(t, payload, depth+1)
		}
		return nil
	})
}

// parseElst reads the edit list. For gapless the meaningful values are the
// first non-empty edit's media_time (the encoder-delay priming iTunes
// writes) and the total played duration (sum of segment durations over
// non-empty edits, in movie ticks). An empty edit (media_time -1) inserts
// blank presentation time and is skipped for the delay.
func parseElst(t *track, payload []byte) {
	version, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return
	}
	count := int64(be32(rest))
	rest = rest[4:]
	entrySize := int64(12)
	if version == 1 {
		entrySize = 20
	}
	if count > int64(len(rest))/entrySize {
		count = int64(len(rest)) / entrySize
	}
	var totalSeg int64
	mediaTime := int64(-1)
	for i := int64(0); i < count; i++ {
		e := rest[i*entrySize:]
		var segDur, mt int64
		if version == 1 {
			segDur = int64(be64(e[0:]))
			mt = int64(be64(e[8:]))
		} else {
			segDur = int64(be32(e[0:]))
			mt = int64(int32(be32(e[4:])))
		}
		if mt < 0 {
			continue // empty edit: blank time, no media consumed
		}
		if mediaTime < 0 {
			mediaTime = mt // first real edit's start offset
		}
		totalSeg += segDur
	}
	if mediaTime < 0 {
		return // only empty edits; nothing to trim
	}
	t.hasEdit = true
	t.editMedia = mediaTime
	t.editSegDur = totalSeg
}

// parseTref records chapter track references so a text chapter track can
// be matched to its audio track.
func parseTref(t *track, body []byte) {
	_ = walkBoxes(body, func(typ string, payload []byte) error {
		if typ == "chap" {
			for i := 0; i+4 <= len(payload); i += 4 {
				t.chapRefs = append(t.chapRefs, int(be32(payload[i:])))
			}
		}
		return nil
	})
}
