package mp4

import (
	"fmt"

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
	// perAU is the decoded output samples per access unit for AAC-family
	// tracks (1024 for LC and downsampled SBR, 2048 for dual-rate HE), 0
	// elsewhere; it is what distinguishes a doubled timeline from the
	// codec ID alone, which spans both shapes.
	perAU int64

	// unitBytes and unitDur describe a byte-linear codec's sample: how many
	// bytes it occupies and how many output samples it decodes to, both
	// constant for the whole track. Set by the stsd setter for PCM alone; zero
	// for every codec whose samples differ in size or in length, which is the
	// gate on the uniform sample table (see parseStbl). PCM's unit is one
	// frame, so unitDur is 1 and unitBytes the frame's width across channels.
	unitBytes int64
	unitDur   int64

	// sourceBits is the depth the file stores samples at when the pipeline
	// cannot carry it: audio.Format holds floats as float32, so a 64-bit float
	// track decodes at BitDepth 32 and the source's own depth would otherwise
	// be lost. Zero when Fmt.BitDepth already is the source's. Decided by the
	// stsd setter, which is where the wire config is known.
	sourceBits int

	// note is a decoder-limitation Warning for this track (an HE-AAC band
	// limit, a sample rate taken from the media timescale), recorded at parse
	// and emitted by selectAudio only if this track is the one chosen. Empty
	// for a track with nothing to say.
	note string

	// stsdErr is why parseStsd failed on this track, deferred rather than
	// propagated. Failing the open outright would reject a file that also
	// carries a decodable audio track, which opens today; but the failure is
	// exactly what would have set codec, so without this the track arrives at
	// selectAudio indistinguishable from one with no sample description at
	// all, and the codec layer's reason ("audio object type 1 is not
	// AAC-LC") is lost behind "found: unknown". selectAudio surfaces it only
	// when nothing at all was selectable.
	stsdErr error

	st sampleTable

	// First edit-list entry: a nonzero media time is the encoder-delay
	// priming for gapless (iTunes writes it), segment duration the playable
	// length in movie ticks.
	hasEdit    bool
	editMedia  int64 // media_time in media timescale ticks (-1 = empty edit)
	editSegDur int64 // segment_duration in movie timescale ticks

	// emptyEdit is the blank presentation time the edit list inserts before
	// the first sample, in movie ticks: the standard way a track states that
	// it starts late. A chapter track's first chapter start rides here,
	// because stts times its samples as deltas from zero and so cannot hold
	// an absolute start; readTextChapters adds it back.
	emptyEdit int64

	// chapRefs are track_IDs referenced as chapter tracks ('chap' tref).
	chapRefs []int
}

// addNote records a limitation of this track, which selectAudio emits only if
// this is the track it chose. More than one can apply at once (a high-rate
// multichannel PCM entry states neither its rate nor a layout this build
// reads), so they accumulate rather than overwrite.
func (t *track) addNote(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if t.note == "" {
		t.note = msg
		return
	}
	t.note += "; " + msg
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
	// Pre-scan for the two boxes the sample table is read against, the same
	// way parseMoov pre-scans for mvex. minf holds the table; mdhd states the
	// timescale it is timed in and hdlr says whether it is audio at all, and
	// both are minf's SIBLINGS. Nothing orders siblings, so a movie that
	// writes minf first would otherwise reach parseStbl with no timescale to
	// rescale against and no handler to decide by, and the track would come
	// out with an unrescaled timeline or no sample map at all.
	_ = walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "mdhd":
			t.timescale, t.duration = mdhdTime(payload)
		case "hdlr":
			t.handler = hdlrType(payload)
		}
		return nil
	})
	return walkBoxes(body, func(typ string, payload []byte) error {
		if typ == "minf" {
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
// non-empty edits, in movie ticks). An empty edit (media_time -1) consumes no
// media and so contributes no delay, but its segment duration is kept: it is
// how a track states that its presentation starts late, which is the whole of
// a chapter track's first start.
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
	var totalSeg, empty int64
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
			// An empty edit: blank time, no media consumed. Only a leading run
			// of them offsets the first sample, which is the offset wanted here;
			// one between two real edits is a gap mid-track, and a sample table
			// read straight through cannot express it anyway.
			if mediaTime < 0 && segDur > 0 {
				empty += segDur
			}
			continue
		}
		if mediaTime < 0 {
			mediaTime = mt // first real edit's start offset
		}
		totalSeg += segDur
	}
	t.emptyEdit = empty
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
