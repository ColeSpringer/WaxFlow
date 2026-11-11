package mp4

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/satmath"
)

// gapless resolves the audio track's trims in output samples: the iTunes
// iTunSMPB tag first (exact priming, padding, and length), then an edit
// list (media_time as priming), else no trim. format.Media consumes
// Delay/Padding/Samples to deliver the trimmed timeline and to map seeks.
//
// It also puts what the table times past the file's data (shortDur) on the
// played timeline, which is the one the length is on: the length the whole
// table would have played, less the one this run does. A cut that took only
// end padding lost no audio.
func (d *Demuxer) gapless(t *track) (delay, padding, samples int64, advisory bool) {
	totalRaw := t.st.totalDur
	front, stated := d.statedTrims(t)
	// A length taken from totalRaw inherits its rounding; one stated by
	// iTunSMPB or an edit list segment does not, which is why this follows
	// where the length came from rather than t.st.rescaled alone.
	delay, samples, fromRaw := playedIn(totalRaw, front, stated)
	padding = totalRaw - delay - samples
	if t.st.shortDur > 0 {
		_, whole, _ := playedIn(satmath.Add(totalRaw, t.st.shortDur), front, stated)
		t.st.truncated = max(whole-samples, 0)
	}
	return delay, padding, samples, fromRaw && t.st.rescaled
}

// statedTrims is what the track's tags state of its gapless shape, before
// any run bounds them: the front trim, and the played length, negative when
// they state none.
func (d *Demuxer) statedTrims(t *track) (delay, length int64) {
	if d.smpbOK {
		delay, length = d.smpbDelay, d.smpbTotal
		// Some encoders write HE-AAC gapless fields in core (1024) units,
		// half the output timeline. When the three fields add up to exactly
		// half the raw track duration, they are half-units: rescale. Keyed
		// on the doubled timeline, not the codec ID alone: downsampled SBR
		// is also codec.HEAAC but decodes 1024 samples per AU, the same
		// units the tag uses, and a half-sum there is a coincidence this
		// heuristic must not double.
		if t.codec == codec.HEAAC && t.perAU == 2048 {
			if sum := delay + d.smpbPad + length; sum > 0 && sum*2 == t.st.totalDur {
				delay *= 2
				length *= 2
				d.note(0, "mp4: iTunSMPB in core half-units; rescaled to the output rate")
			}
		}
		return delay, length
	}

	// A nonzero edit-list media_time is the encoder-delay priming. The
	// progressive path trusts it only for a real (nonzero) delay, falling back
	// to the stbl raw total for the length; a zero-delay edit is ignored here
	// because the sample table already gives the exact length. (The fragmented
	// path in fragdemux.go has no sample table, so it trusts the edit's
	// duration even at zero delay; both share editListTrims for the rescale.)
	if t.hasEdit && t.editMedia > 0 {
		delay, seg, haveSeg := editListTrims(t, d.movieTimescale)
		if !haveSeg {
			seg = -1
		}
		return delay, seg
	}
	return 0, -1
}

// playedIn fits stated trims to a run of raw samples: the front trim held
// inside the run, and the played length the tags state where the run can hold
// it behind that trim. Where they state none, or more than the run holds, the
// run's own remainder is the length, and fromRaw says so.
func playedIn(raw, front, stated int64) (delay, samples int64, fromRaw bool) {
	delay = clamp(front, 0, raw)
	if stated < 0 || stated > raw-delay {
		return delay, raw - delay, true
	}
	return delay, stated, false
}

// editListTrims rescales a track's first real edit into gapless trims in output
// samples: the front delay from its media_time (media timescale) and the played
// length from its segment_duration (movie timescale). haveSeg is false when the
// edit declares no usable duration, leaving the length to the caller's fallback.
// The progressive (gapless) and fragmented (fragmentedLength) readers share
// this rescale and differ only in that fallback: the stbl raw total, against
// the fragmented tiers (a segment index, then a header duration, then nothing),
// which each applies around this call.
func editListTrims(t *track, movieTimescale int64) (delay, segSamples int64, haveSeg bool) {
	rate := int64(t.fmt.Rate)
	delay = t.editMedia
	if t.timescale > 0 && rate > 0 && t.timescale != rate {
		delay = satmath.MulDiv(t.editMedia, rate, t.timescale)
	}
	if delay < 0 {
		delay = 0
	}
	if t.editSegDur > 0 && movieTimescale > 0 && rate > 0 {
		return delay, satmath.MulDiv(t.editSegDur, rate, movieTimescale), true
	}
	return delay, 0, false
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

// ilstTextKeys maps the iTunes text atoms onto canonical tag keys. It is
// the inverse of muxmeta.go's ilstText and bounded by it: what the muxer
// writes is what the demuxer reads back.
var ilstTextKeys = map[string]string{
	"\xa9nam": "TITLE",
	"\xa9ART": "ARTIST",
	"\xa9alb": "ALBUM",
	"aART":    "ALBUMARTIST",
	"\xa9wrt": "COMPOSER",
	"\xa9gen": "GENRE",
	"\xa9day": "RECORDINGDATE",
	"\xa9cmt": "COMMENT",
	"\xa9lyr": "LYRICS",
}

// ilstFreeformKeys are the freeform (----:com.apple.iTunes:KEY) names read
// back as tags: the ReplayGain fields, muxmeta.go's ilstFreeform. iTunSMPB
// is freeform too but is muxer-owned gapless state, not tag data, and is
// handled separately in parseFreeform.
var ilstFreeformKeys = map[string]bool{
	"REPLAYGAIN_TRACK_GAIN": true,
	"REPLAYGAIN_TRACK_PEAK": true,
	"REPLAYGAIN_ALBUM_GAIN": true,
	"REPLAYGAIN_ALBUM_PEAK": true,
}

// parseILST reads the item list: the descriptive text atoms, the trkn/disk
// number pairs, and the freeform atoms (ReplayGain, plus the iTunSMPB
// gapless descriptor). Tolerance is the parseUdta contract: a malformed
// item is skipped, never an error, since none of it gates playback.
func (d *Demuxer) parseILST(body []byte) {
	_ = walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "----":
			d.parseFreeform(payload)
		case "trkn":
			d.addNumberPair(payload, "TRACKNUMBER", "TRACKTOTAL")
		case "disk":
			d.addNumberPair(payload, "DISCNUMBER", "DISCTOTAL")
		default:
			if key, ok := ilstTextKeys[typ]; ok {
				if v, ok := itemText(payload); ok {
					d.addTag(key, v)
				}
			}
		}
		return nil
	})
}

// addTag records one canonical tag value, in file order.
func (d *Demuxer) addTag(key, value string) {
	if value == "" {
		return
	}
	if d.tags == nil {
		d.tags = make(map[string][]string)
	}
	d.tags[key] = append(d.tags[key], value)
}

// itemText reads an ilst item's text value: its data box's UTF-8 payload.
// The type indicator must say UTF-8 (1), which is what every writer of
// these atoms sets; a binary payload read as text would land mojibake in
// the tag map.
//
// Multi-valued keys are not split back apart. The muxer joins them with
// "; " because an ilst data atom holds a single string, so what the file
// carries is one value, and splitting on a separator a value may itself
// contain would invent fields the file does not have.
func itemText(body []byte) (string, bool) {
	var out string
	var ok bool
	_ = walkBoxes(body, func(typ string, p []byte) error {
		// data box: type_indicator(4) locale(4) value.
		if typ == "data" && !ok && len(p) >= 8 && be32(p) == 1 {
			if s := strings.TrimRight(string(p[8:]), "\x00"); utf8.ValidString(s) {
				out, ok = s, true
			}
		}
		return nil
	})
	return out, ok
}

// addNumberPair reads a trkn/disk atom back into its two canonical keys:
// a raw data box holding a 16-bit index then a 16-bit total, either of
// which is absent when it is zero (the muxer writes no atom at all when
// both are).
//
// The type indicator must say binary (0), the same gate itemText applies
// for text. This reader runs on every MP4 input, not only files this
// muxer wrote, and a writer that stored trkn as UTF-8 ("3/9") is not
// hypothetical: read as big-endian words those bytes become a track
// number in the thousands, which would then become the container-tag
// floor and get embedded onto the output. A shape this cannot read is
// skipped, per the tolerance contract, rather than guessed at.
func (d *Demuxer) addNumberPair(body []byte, numKey, totalKey string) {
	_ = walkBoxes(body, func(typ string, p []byte) error {
		if typ != "data" || len(p) < 14 || be32(p) != 0 {
			return nil
		}
		payload := p[8:]
		if n := be16(payload[2:]); n != 0 {
			d.addTag(numKey, strconv.Itoa(int(n)))
		}
		if t := be16(payload[4:]); t != 0 {
			d.addTag(totalKey, strconv.Itoa(int(t)))
		}
		return nil
	})
}

// parseFreeform reads a '----' freeform atom: the iTunSMPB gapless
// descriptor, or one of the ReplayGain tag keys.
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
	if data == nil {
		return
	}
	if name == "iTunSMPB" {
		d.parseSMPB(string(data))
		return
	}
	if ilstFreeformKeys[name] {
		if s := strings.TrimRight(string(data), "\x00"); utf8.ValidString(s) {
			d.addTag(name, s)
		}
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
	// padding is retained only for the HE-AAC half-unit sanity check;
	// gapless still derives the effective padding from totalRaw, delay,
	// and the true sample count.
	d.smpbDelay, d.smpbPad, d.smpbTotal, d.smpbOK = delay, padding, total, true
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

// resolveChapters picks the chapter source: a text chapter track referenced
// by the audio track if present, else the Nero chpl list.
//
// The text track wins because it is the lossless form: a real track whose
// sample table is unbounded and whose stts times every chapter to its end.
// chpl is a Nero extension whose one-byte count caps the list at 255, and
// whose entries are starts only, ends discarded by construction. A file
// carrying both (the common case, ours included: the muxer writes every
// chapter to the text track and the first 255 to chpl) would otherwise read
// back through chpl and lose everything past chapter 255.
//
// A fragmented movie resolves here too, and lands on chpl. Only the text track
// is out of reach: its samples would live in the fragments, and a fragmented
// moov's sample tables are empty by design, so chapterTrack passes it over for
// having no samples. chpl is in the udta, which a fragmented moov carries like
// any other, and the fragmented muxer writes one. Skipping this whole resolve
// for a fragmented file threw away chapters the file plainly held.
//
// Either list ends up in start order, stable, as the Chapterer contract
// promises: the text track's samples are timed in sequence, but a chpl box
// lists entries in whatever order its writer gave them.
func (d *Demuxer) resolveChapters(tracks []*track, audio *track) {
	d.chapters = d.chplChapters
	if ct := d.chapterTrack(tracks, audio); ct != nil {
		if chapters := d.readTextChapters(ct); len(chapters) > 0 {
			d.chapters = chapters
		}
	}
	container.SortChapters(d.chapters)
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
	n := min(st.total, maxChapters)
	// The track's leading empty edit, on the track's own timeline. stts times
	// the samples as deltas accumulated from zero, so the first chapter's start
	// is in none of them: it is the edit list's blank presentation time, and a
	// reader that ignores it reports every chapter in the file early by that
	// much. A track whose first chapter starts at zero carries no edit and
	// shifts by nothing.
	//
	// The rescale rounds because the two grids do not divide each other: the
	// edit is in movie ticks, 44.1 of which make a millisecond at 44.1 kHz, and
	// truncating would move the anchor a millisecond rather than a tick.
	shift := satmath.MulDivRound(ct.emptyEdit, ct.timescale, d.movieTimescale)
	var out []Chapter
	for i := range n {
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
		pts, dur := st.timeOf(i)
		// pts*time.Second can overflow int64 for a hostile pts or a tiny
		// timescale; the saturating rescale keeps chapter times from wrapping
		// negative. mulDivSat returns 0 when ct.timescale is not positive.
		start := time.Duration(satmath.MulDiv(pts+shift, int64(time.Second), ct.timescale))
		// A text track times every chapter to its end, which is why this
		// source outranks chpl: chpl has nowhere to put an end. A
		// zero-duration sample leaves End zero, which reads as "until the
		// next chapter" exactly as a chpl entry does.
		end := time.Duration(0)
		if dur > 0 {
			end = time.Duration(satmath.MulDiv(pts+shift+dur, int64(time.Second), ct.timescale))
		}
		out = append(out, Chapter{Start: start, End: end, Title: sanitizeTitle(buf[2 : 2+textLen])})
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
