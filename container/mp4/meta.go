package mp4

import (
	"strconv"
	"strings"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/internal/satmath"
)

// gapless resolves the audio track's trims in output samples: the iTunes
// iTunSMPB tag first (exact priming, padding, and length), then an edit
// list (media_time as priming, duration as length), else no trim.
// format.Media consumes Delay/Padding/Samples to deliver the trimmed timeline
// and to map seeks.
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

	// An edit list states the front trim in its media_time and the played
	// length in its duration, so one edit from media_time 0 states an end
	// trim alone.
	if t.hasEdit {
		delay, seg, _, haveSeg := editListTrims(t, d.movieTimescale)
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

// editListTrims rescales a track's edit list into gapless trims in output
// samples: the front delay from its first media_time (media timescale) and
// the played length from its segment_duration (movie timescale), as the
// longest length that duration can be the rounding of. slack is how far under
// it the shortest such length lies: 0 where the duration names one length.
// haveSeg is false when the edit declares no usable duration, or the list
// holds several edits: that is a cut list, which no length stands for. The
// length is then the caller's fallback, which is where the progressive
// (gapless) and fragmented (fragmentedLength) readers differ: the stbl raw
// total, against the fragmented tiers (a segment index, then a header
// duration, then nothing).
func editListTrims(t *track, movieTimescale int64) (delay, segSamples, slack int64, haveSeg bool) {
	rate := int64(t.fmt.Rate)
	delay = t.editMedia
	if t.timescale > 0 && rate > 0 && t.timescale != rate {
		delay = satmath.MulDiv(t.editMedia, rate, t.timescale)
	}
	if delay < 0 {
		delay = 0
	}
	if t.editSegDur > 0 && movieTimescale > 0 && rate > 0 && t.edits <= 1 {
		longest := longestIn(t.editSegDur, rate, movieTimescale)
		// The shortest is the first length past the tick before.
		shortest := satmath.Add(satmath.MulDiv(t.editSegDur-1, rate, movieTimescale), 1)
		return delay, longest, max(longest-shortest, 0), true
	}
	return delay, 0, 0, false
}

// longestIn is the longest run of samples at rate that ticks at timescale
// can be the rounding of: the ticks themselves where a tick is a sample.
func longestIn(ticks, rate, timescale int64) int64 {
	n := satmath.MulDiv(satmath.Add(ticks, 1), rate, timescale)
	if satmath.MulDiv(n, timescale, rate) == satmath.Add(ticks, 1) {
		n-- // a whole number of samples reaches the next tick
	}
	return n
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

// parseUdta walks a user-data box for the iTunSMPB gapless descriptor, and
// the movie's first one for its Nero chapter list as well: the first chpl
// that holds chapters. Tags are read from the first udta once the walk is
// over (readMovieTags). Malformed metadata yields no markers, never an
// error, since none of it gates playback.
func (d *Demuxer) parseUdta(body []byte, depth int, first bool) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	return walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "chpl":
			if !first || d.chpl != nil {
				break
			}
			if chs, ok := parseChpl(payload, &titleReader{page: &windows1252}); ok && len(chs) > 0 {
				d.chpl = payload
			}
		case "meta":
			d.scanGapless(payload, depth+1)
		}
		return nil
	})
}

// scanGapless walks a meta box's item lists for iTunSMPB. The box is a
// FullBox in ISO files but a plain box in some QuickTime files; the hdlr
// child's position disambiguates.
func (d *Demuxer) scanGapless(payload []byte, depth int) {
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
		if typ != "ilst" {
			return nil
		}
		_ = walkBoxes(p, func(item string, ip []byte) error {
			if item == "----" {
				d.scanSMPB(ip)
			}
			return nil
		})
		return nil
	})
}

// scanSMPB reads a freeform item as the iTunSMPB gapless descriptor, if it
// is one. It is muxer-owned state and not tag data, so it is read from any
// namespace and data type, where a tag is held to the iTunes ones.
func (d *Demuxer) scanSMPB(body []byte) {
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
	if data != nil && name == "iTunSMPB" {
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
	// padding is retained only for the HE-AAC half-unit sanity check;
	// gapless still derives the effective padding from totalRaw, delay,
	// and the true sample count.
	d.smpbDelay, d.smpbPad, d.smpbTotal, d.smpbOK = delay, padding, total, true
}
