package mp4

import (
	"math"
	"math/bits"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/colespringer/waxflow/container"
)

// gapless resolves the audio track's trims in output samples: the iTunes
// iTunSMPB tag first (exact priming, padding, and length), then an edit
// list (media_time as priming), else no trim. format.Media consumes
// Delay/Padding/Samples to deliver the trimmed timeline and to map seeks.
func (d *Demuxer) gapless(t *track) (delay, padding, samples int64) {
	totalRaw := t.st.totalDur
	rate := int64(t.fmt.Rate)

	if d.smpbOK {
		delay = clamp(d.smpbDelay, 0, totalRaw)
		samples = d.smpbTotal
		if samples < 0 || samples > totalRaw-delay {
			samples = totalRaw - delay
		}
		padding = totalRaw - delay - samples
		return delay, padding, samples
	}

	if t.hasEdit && t.editMedia > 0 {
		delay = t.editMedia
		if t.timescale > 0 && rate > 0 && t.timescale != rate {
			delay = mulDivSat(t.editMedia, rate, t.timescale)
		}
		delay = clamp(delay, 0, totalRaw)
		if t.editSegDur > 0 && d.movieTimescale > 0 && rate > 0 {
			samples = mulDivSat(t.editSegDur, rate, d.movieTimescale)
		} else {
			samples = totalRaw - delay
		}
		if samples < 0 || samples > totalRaw-delay {
			samples = totalRaw - delay
		}
		padding = totalRaw - delay - samples
		return delay, padding, samples
	}

	return 0, 0, totalRaw
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mulDivSat computes a*b/c in 128 bits, saturating at math.MaxInt64 so a
// hostile edit-list time or chapter timescale cannot overflow the product.
// A non-positive operand yields 0. It backs the movie-timeline rescales,
// whose results are int64 sample counts or nanosecond durations; the
// per-sample delta path in stbl.go uses rescaleTicks instead, which caps
// tighter to keep count*delta from overflowing during PTS accumulation.
func mulDivSat(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return math.MaxInt64 // quotient would exceed 64 bits
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// parseUdta walks the user-data box for Nero chapters (chpl) and the
// metadata item list (meta > ilst) carrying iTunSMPB. Chapter and tag
// parsing is tolerant: malformed metadata yields no markers, never an
// error, since none of it gates playback.
func (d *Demuxer) parseUdta(body []byte, depth int) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	return walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "chpl":
			d.parseChpl(payload)
		case "meta":
			d.parseMeta(payload, depth+1)
		}
		return nil
	})
}

// parseMeta walks a meta box for its ilst. The box is a FullBox in ISO
// files but a plain box in some QuickTime files; the hdlr child's position
// disambiguates.
func (d *Demuxer) parseMeta(payload []byte, depth int) {
	if depth > maxDepth {
		return
	}
	body := payload
	switch {
	case len(payload) >= 12 && string(payload[8:12]) == "hdlr":
		body = payload[4:] // ISO FullBox: skip version/flags
	case len(payload) >= 8 && string(payload[4:8]) == "hdlr":
		body = payload // QuickTime plain box
	case len(payload) >= 4:
		body = payload[4:] // default to the ISO shape
	}
	_ = walkBoxes(body, func(typ string, p []byte) error {
		if typ == "ilst" {
			d.parseILST(p)
		}
		return nil
	})
}

// parseILST scans the item list for the iTunSMPB freeform atom.
func (d *Demuxer) parseILST(body []byte) {
	_ = walkBoxes(body, func(typ string, payload []byte) error {
		if typ == "----" {
			d.parseFreeform(payload)
		}
		return nil
	})
}

// parseFreeform reads a '----' freeform atom, extracting the iTunSMPB
// gapless descriptor when present.
func (d *Demuxer) parseFreeform(body []byte) {
	var name string
	var data []byte
	_ = walkBoxes(body, func(typ string, p []byte) error {
		switch typ {
		case "name":
			if _, _, rest, ok := fullBox(p); ok {
				name = string(rest)
			}
		case "data":
			// data box: type_indicator(4) locale(4) value.
			if len(p) >= 8 {
				data = p[8:]
			}
		}
		return nil
	})
	if name == "iTunSMPB" && data != nil {
		d.parseSMPB(string(data))
	}
}

// parseSMPB parses an iTunSMPB value: space-separated hex fields whose
// second, third, and fourth are the encoder delay, padding, and true
// sample count.
func (d *Demuxer) parseSMPB(s string) {
	fields := strings.Fields(s)
	if len(fields) < 4 {
		return
	}
	delay, e1 := strconv.ParseInt(fields[1], 16, 64)
	padding, e2 := strconv.ParseInt(fields[2], 16, 64)
	total, e3 := strconv.ParseInt(fields[3], 16, 64)
	if e1 != nil || e2 != nil || e3 != nil || delay < 0 || padding < 0 || total < 0 {
		return
	}
	// padding is validated above but not retained: gapless derives it from
	// totalRaw, delay, and the true sample count.
	d.smpbDelay, d.smpbTotal, d.smpbOK = delay, total, true
}

// parseChpl reads a Nero chapter list. Times are 100-nanosecond units.
func (d *Demuxer) parseChpl(payload []byte) {
	version, _, rest, ok := fullBox(payload)
	if !ok {
		return
	}
	if version == 1 && len(rest) >= 4 {
		rest = rest[4:] // some encoders add a reserved word before the count
	}
	if len(rest) < 1 {
		return
	}
	count := int(rest[0])
	rest = rest[1:]
	for i := 0; i < count && i < maxChapters; i++ {
		if len(rest) < 9 {
			return
		}
		start := int64(be64(rest))
		titleLen := int(rest[8])
		rest = rest[9:]
		if titleLen > len(rest) {
			titleLen = len(rest)
		}
		d.chplChapters = append(d.chplChapters, Chapter{
			Start: time.Duration(start) * 100,
			Title: sanitizeTitle(rest[:titleLen]),
		})
		rest = rest[titleLen:]
	}
}

// resolveChapters picks the chapter source: Nero chpl if present, else a
// text chapter track referenced by the audio track.
func (d *Demuxer) resolveChapters(tracks []*track, audio *track) {
	if len(d.chplChapters) > 0 {
		d.chapters = d.chplChapters
		return
	}
	ct := d.chapterTrack(tracks, audio)
	if ct != nil {
		d.chapters = d.readTextChapters(ct)
	}
}

// chapterTrack finds the text track holding chapter titles: one referenced
// by the audio track's 'chap' tref, or failing that any text track.
func (d *Demuxer) chapterTrack(tracks []*track, audio *track) *track {
	byID := func(id int) *track {
		for _, t := range tracks {
			if t.id == id && (t.handler == "text" || t.handler == "sbtl") {
				return t
			}
		}
		return nil
	}
	for _, id := range audio.chapRefs {
		if t := byID(id); t != nil && t.st.total > 0 {
			return t
		}
	}
	for _, t := range tracks {
		if (t.handler == "text" || t.handler == "sbtl") && t.st.total > 0 {
			return t
		}
	}
	return nil
}

// readTextChapters reads chapter titles from a QuickTime text track: each
// sample is a 16-bit length followed by the UTF-8 title, timed by the
// sample's presentation time.
func (d *Demuxer) readTextChapters(ct *track) []Chapter {
	st := &ct.st
	n := st.total
	if n > maxChapters {
		n = maxChapters
	}
	var out []Chapter
	for i := int64(0); i < n; i++ {
		size := int(st.sizes[i])
		if size < 2 || size > 1<<16 {
			continue
		}
		buf := make([]byte, size)
		if container.ReadFull(d.src, buf, st.offsets[i]) != nil {
			break
		}
		textLen := int(be16(buf))
		if 2+textLen > len(buf) {
			textLen = len(buf) - 2
		}
		pts, _ := st.timeOf(i)
		// pts*time.Second can overflow int64 for a hostile pts or a tiny
		// timescale; the saturating rescale keeps chapter times from wrapping
		// negative. mulDivSat returns 0 when ct.timescale is not positive.
		start := time.Duration(mulDivSat(pts, int64(time.Second), ct.timescale))
		out = append(out, Chapter{Start: start, Title: sanitizeTitle(buf[2 : 2+textLen])})
	}
	return out
}

// sanitizeTitle renders a chapter title, dropping a leading UTF-16 BOM's
// worth of noise and trailing NULs, keeping only printable content.
func sanitizeTitle(b []byte) string {
	// A UTF-16 BE BOM marks a wide title; decode it so the common
	// wide-encoded case is not returned as mojibake.
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return decodeUTF16BE(b[2:])
	}
	s := strings.TrimRight(string(b), "\x00")
	return strings.TrimSpace(s)
}

// decodeUTF16BE decodes big-endian UTF-16 (chapter titles occasionally use
// it), pairing surrogates and ignoring a trailing odd byte.
func decodeUTF16BE(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, be16(b[i:]))
	}
	s := strings.TrimRight(string(utf16.Decode(u)), "\x00")
	return strings.TrimSpace(s)
}
