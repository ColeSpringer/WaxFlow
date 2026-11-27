package mp4

import (
	"math"
	"time"

	"github.com/colespringer/waxflow/container"
)

// An MP4 holds chapters two ways: a Nero chpl list under moov.udta, which
// states starts only and at most 255 of them, and a QuickTime text track that
// another track names in a tref chap entry, which times every chapter to its
// end. The two are read as waxlabel reads them, so a file gives one chapter
// list whichever reader speaks for it.

// chplStartUnit is the chpl start-time resolution, ticks per second.
const chplStartUnit = 10_000_000

// maxChapterTextBytes bounds the title bytes read from one chapter track. A
// sample table can point every one of its samples at the same 64 KiB, so
// the samples' sizes are no bound on what reading them costs.
const maxChapterTextBytes = 4 << 20

// chapterEdits is where a chapter track's edit list puts its media on the
// movie timeline, in movie ticks: lead is the segment of a first entry that
// is an empty edit, played the summed segments that play media (0 when
// unknown), and clamped a lead that sat at the 32-bit ceiling when written.
type chapterEdits struct {
	lead, played uint64
	clamped      bool
}

// parseChapterEdits reads an elst payload as chapterEdits. A 32-bit segment
// of exactly MaxUint32 was clamped by its writer: such a span leaves played
// unknown.
func parseChapterEdits(payload []byte) chapterEdits {
	if len(payload) < 8 {
		return chapterEdits{}
	}
	var width int
	switch payload[0] {
	case 0:
		width = 12
	case 1:
		width = 20
	default:
		return chapterEdits{}
	}
	count := be32(payload[4:])
	var ed chapterEdits
	unknown := false
	for i, e := uint32(0), payload[8:]; i < count && len(e) >= width; i, e = i+1, e[width:] {
		seg, empty := uint64(be32(e)), int32(be32(e[4:])) == -1
		if width == 20 {
			seg, empty = be64(e), int64(be64(e[8:])) == -1
		}
		atCeiling := width == 12 && seg == math.MaxUint32
		switch {
		case !empty:
			unknown = unknown || atCeiling
			if ed.played += seg; ed.played < seg {
				ed.played = math.MaxUint64
			}
		case i == 0:
			ed.lead, ed.clamped = seg, atCeiling
		}
	}
	if unknown {
		ed.played = 0
	}
	return ed
}

// textTable is a text track's sample tables as the file states them: the
// time-to-sample runs, the stsz sample sizes (one constant, or four bytes
// each), the sample-to-chunk runs and the chunk offsets. It is read the way
// the tag library reads a chapter track, which is more forgiving than the
// table a sound track is decoded through: the runs need not be in order, a
// size table need only hold the samples asked for, and nothing is cut at
// the end of the file, where a chapter whose title is gone is still a
// chapter. A track sized by stz2 has no such table.
type textTable struct {
	stts      []sttsEntry
	constSize uint32
	count     int64  // the stsz box's own sample count
	sizes     []byte // its entries, as far as the box holds them
	stsc      []stscEntry
	chunks    []int64
}

// parseTextTable reads a text track's stbl into a textTable, nil when a box
// it needs is missing or does not hold what it declares. Of several boxes
// of one type the first is read.
func parseTextTable(stbl []byte) *textTable {
	var stts, stsz, stsc, stco, co64 []byte
	first := func(dst *[]byte, payload []byte) {
		if *dst == nil {
			*dst = payload[:len(payload):len(payload)]
		}
	}
	err := walkBoxes(stbl, func(typ string, payload []byte) error {
		switch typ {
		case "stts":
			first(&stts, payload)
		case "stsz":
			first(&stsz, payload)
		case "stsc":
			first(&stsc, payload)
		case "stco":
			first(&stco, payload)
		case "co64":
			first(&co64, payload)
		}
		return nil
	})
	if err != nil || stts == nil || len(stsz) < 12 || len(stsc) < 8 {
		return nil
	}
	tt := &textTable{constSize: be32(stsz[4:]), count: int64(be32(stsz[8:])), sizes: stsz[12:]}
	if tt.stts, err = parseStts(stts); err != nil {
		return nil
	}
	// The runs are kept as written: a first chunk of 0, or runs out of
	// order, still say how many samples a chunk holds.
	runs := stsc[8:]
	if n := int64(be32(stsc[4:])); n <= int64(len(runs))/12 {
		tt.stsc = make([]stscEntry, n)
		for i := range tt.stsc {
			tt.stsc[i] = stscEntry{first: int64(be32(runs[i*12:])), spc: int64(be32(runs[i*12+4:]))}
		}
	} else {
		return nil
	}
	chunks, wide := stco, false
	if chunks == nil {
		chunks, wide = co64, true
	}
	if chunks == nil {
		return nil
	}
	if tt.chunks, err = parseStco(chunks, wide); err != nil {
		return nil
	}
	return tt
}

// textSampleAt is where one text sample sits.
type textSampleAt struct{ off, size int64 }

// locate places the first n samples: each chunk's samples run back to back
// from its offset, as many as the sample-to-chunk run covering it says.
func (tt *textTable) locate(n int) []textSampleAt {
	n = int(min(int64(n), tt.count))
	if n <= 0 || len(tt.stsc) == 0 || tt.constSize == 0 && n > len(tt.sizes)/4 {
		return nil
	}
	out := make([]textSampleAt, 0, n)
	run := 0
	for c := 0; c < len(tt.chunks) && len(out) < n; c++ {
		for run+1 < len(tt.stsc) && tt.stsc[run+1].first <= int64(c)+1 {
			run++
		}
		if tt.stsc[run].first > int64(c)+1 {
			continue
		}
		off := tt.chunks[c]
		for k := int64(0); k < tt.stsc[run].spc && len(out) < n; k++ {
			size := int64(tt.constSize)
			if size == 0 {
				size = int64(be32(tt.sizes[len(out)*4:]))
			}
			out = append(out, textSampleAt{off, size})
			off += size
		}
	}
	return out
}

// resolveChapters reads both chapter stores into one list. Where they agree
// the chpl gives the titles and starts and the track the last end; where they
// disagree the track wins, and a store standing alone is read as it is.
//
// A fragmented movie lands on chpl: its text track's sample table is empty
// by design, so the track reads as absent.
//
// The list ends up in start order, stable, as the Chapterer contract
// promises: a chpl lists entries in whatever order its writer gave them.
func (d *Demuxer) resolveChapters(tracks []*track) {
	// A Nero list comes from Windows tools and a chapter track from QuickTime,
	// and a track beside a list of 8-bit titles holds the same bytes as it.
	chplTitles := titleReader{page: &windows1252}
	var chpl []Chapter
	if d.chpl != nil {
		chpl, _ = parseChpl(d.chpl, &chplTitles)
	}
	trackTitles := titleReader{page: &macRoman}
	if chplTitles.legacy {
		trackTitles.page = &windows1252
	}
	var qt []Chapter
	var haveQT, saturated bool
	if ct := findChapterTrack(tracks); ct != nil {
		qt, saturated, haveQT = d.readTextChapters(ct, &trackTitles)
	}
	d.chapters = mergeChapters(chpl, qt, haveQT, saturated)
	container.SortChapters(d.chapters)
}

// parseChpl reads a Nero chapter list: version and flags, a reserved word in
// every version but 0, a one-byte count, then each entry as a 64-bit start
// and a length-prefixed title. A list cut short inside an entry is no list.
// Interior chapters end where the next one starts.
func parseChpl(payload []byte, titles *titleReader) ([]Chapter, bool) {
	if len(payload) < 5 {
		return nil, false
	}
	pos := 4
	if payload[0] != 0 {
		pos += 4
	}
	if pos+1 > len(payload) {
		return nil, false
	}
	count := int(payload[pos])
	pos++
	out := make([]Chapter, 0, count)
	for range count {
		if pos+9 > len(payload) {
			return nil, false
		}
		start := be64(payload[pos:])
		n := int(payload[pos+8])
		pos += 9
		if pos+n > len(payload) {
			return nil, false
		}
		out = append(out, Chapter{Start: ticksToDuration(start, chplStartUnit), Title: titles.read(payload[pos : pos+n])})
		pos += n
	}
	fillInteriorEnds(out)
	return out, true
}

// fillInteriorEnds ends each open chapter but the last at the next start,
// where that is later.
func fillInteriorEnds(chs []Chapter) {
	for i := range chs {
		if chs[i].End == 0 && i+1 < len(chs) && chs[i+1].Start > chs[i].Start {
			chs[i].End = chs[i+1].Start
		}
	}
}

// findChapterTrack finds the chapter track: the first text or subtitle track
// that another track's tref chap entry names. A text track nothing names is
// subtitles, and one only its own list names is none.
func findChapterTrack(tracks []*track) *track {
	text := make(map[int]*track)
	for _, t := range tracks {
		if !t.idKnown || t.handler != "text" && t.handler != "sbtl" {
			continue
		}
		if _, dup := text[t.id]; !dup {
			text[t.id] = t
		}
	}
	for _, t := range tracks {
		for _, id := range t.chapRefs {
			if ct := text[id]; ct != nil && ct != t {
				return ct
			}
		}
	}
	return nil
}

// readTextChapters reads a chapter track: each sample's decode time is a
// chapter start, shifted by the track's leading empty edit, and its bytes
// are a 16-bit length then the title. A sample the file does not hold is a
// chapter with no title. ok is false for a track with no usable timing or
// sample table. saturated says a time in it was clamped when
// written, so its starts cannot be trusted over a chpl's.
func (d *Demuxer) readTextChapters(ct *track, titles *titleReader) (out []Chapter, saturated, ok bool) {
	if ct.timescale <= 0 || ct.text == nil {
		return nil, false, false
	}
	// stts times the samples as deltas from zero, in media ticks.
	var times []uint64
	var end uint64
	complete := true
walk:
	for _, e := range ct.text.stts {
		for range e.count {
			if len(times) >= maxChapters {
				complete = false
				break walk
			}
			times = append(times, end)
			end += uint64(e.delta)
		}
	}
	at := ct.text.locate(len(times))
	n := len(at)
	if n == 0 {
		return nil, false, false
	}
	for i := 1; i < len(times); i++ {
		if times[i]-times[i-1] == math.MaxUint32 {
			saturated = true
			break
		}
	}
	// The edit list is in movie ticks and says nothing without that scale.
	var ed chapterEdits
	if d.movieTimescale > 0 {
		ed = ct.chapEdits
	}
	offset := ticksToDuration(ed.lead, d.movieTimescale)
	played := ticksToDuration(ed.played, d.movieTimescale)
	saturated = saturated || ed.clamped

	out = make([]Chapter, 0, n)
	var read int64
	for i := range n {
		title, size, err := d.readTextSample(at[i].off, at[i].size, titles)
		if read += size; err != nil || read > maxChapterTextBytes {
			return nil, false, false
		}
		ch := Chapter{Start: addClamp(ticksToDuration(times[i], ct.timescale), offset), Title: title}
		if i+1 < len(times) {
			ch.End = addClamp(ticksToDuration(times[i+1], ct.timescale), offset)
		}
		out = append(out, ch)
	}
	if complete && n == len(times) {
		last := &out[n-1]
		last.End = 0
		if !placeholderTail(last.Start, end-times[n-1], ct.timescale, d.movieTimescale, d.movieDuration) {
			slack := ticksToDuration(1, d.movieTimescale) + ticksToDuration(1, ct.timescale)
			last.End = trackEnd(ticksToDuration(end, ct.timescale), last.Start, offset, played, slack)
		}
	}
	return out, saturated, true
}

// readTextSample reads a text sample's title: a 16-bit length, then that
// many bytes, cut to the sample. A title the file does not hold whole reads
// as empty. n is the title bytes read.
func (d *Demuxer) readTextSample(off, size int64, titles *titleReader) (title string, n int64, err error) {
	if size < 2 || off < 0 || off > d.size-2 {
		return "", 0, nil
	}
	var head [2]byte
	if err := container.ReadFull(d.src, head[:], off); err != nil {
		return "", 0, err
	}
	n = min(int64(be16(head[:])), size-2)
	if n > d.size-off-2 {
		return "", 0, nil
	}
	raw := make([]byte, n)
	if err := container.ReadFull(d.src, raw, off+2); err != nil {
		return "", 0, err
	}
	return titles.read(raw), n, nil
}

// placeholderTail reports whether the last chapter's span is the one-tick
// (or one-second) tail a writer gives an open last chapter that starts at or
// past the movie's end: such a chapter has no end to state.
func placeholderTail(lastStart time.Duration, lastDelta uint64, mediaTimescale, movieTimescale, movieDuration int64) bool {
	if movieTimescale <= 0 || movieDuration <= 0 {
		return false
	}
	if lastStart < ticksToDuration(uint64(movieDuration), movieTimescale) {
		return false
	}
	return lastDelta == 1 || lastDelta == uint64(mediaTimescale)
}

// trackEnd places the end of a chapter track's media, which is where its
// last chapter ends, on the movie timeline. Media that ends inside its edits
// ends the track; media that runs past them ends with them, unless it
// reaches the track's own end, as a track written to span the movie does.
// Within the slack of the two grids the nearer end decides. An end at or
// before lastStart is none, and the media's own stands.
func trackEnd(mediaEnd, lastStart, offset, played, slack time.Duration) time.Duration {
	own, end := addClamp(mediaEnd, offset), addClamp(played, offset)
	over, past := mediaEnd-played, mediaEnd-end
	placed := end
	switch {
	case played == 0, over <= slack && over.Abs() <= past.Abs():
		return own
	case past.Abs() <= slack:
		placed = mediaEnd
	}
	if placed <= lastStart {
		return own
	}
	return placed
}

// mergeChapters projects the two stores into one list. They agree when they
// hold as many chapters with matching titles and starts within half a
// second, or matching titles alone where the track's times were clamped.
func mergeChapters(chpl, qt []Chapter, haveQT, saturated bool) []Chapter {
	switch {
	case haveQT && len(chpl) > 0:
		if chaptersAgree(chpl, qt, !saturated) {
			return chplWithTrackEnd(chpl, qt)
		}
		return qt
	case haveQT:
		return qt
	}
	return chpl
}

// chaptersAgree reports whether a chpl and a chapter track describe the same
// chapters. A track title that reads empty matches the chpl's. The two use
// different time bases, so starts are held to a tolerance when timed is set.
func chaptersAgree(chpl, qt []Chapter, timed bool) bool {
	if len(chpl) != len(qt) {
		return false
	}
	const tol = 500 * time.Millisecond
	for i := range chpl {
		if qt[i].Title != "" && qt[i].Title != chpl[i].Title {
			return false
		}
		if d := chpl[i].Start - qt[i].Start; timed && (d < -tol || d > tol) {
			return false
		}
	}
	return true
}

// chplWithTrackEnd is the chpl's chapters with the last end the track alone
// states.
func chplWithTrackEnd(chpl, qt []Chapter) []Chapter {
	out := append([]Chapter(nil), chpl...)
	if n := len(out); n > 0 && n == len(qt) && qt[n-1].End >= out[n-1].Start {
		out[n-1].End = qt[n-1].End
	}
	return out
}

// ticksToDuration converts ticks of a timescale to a Duration, rounding to
// the nearest nanosecond and clamping rather than overflowing. It is the
// arithmetic waxlabel times chapters with, so both readers land on the same
// nanosecond.
func ticksToDuration(units uint64, timescale int64) time.Duration {
	if timescale <= 0 {
		return 0
	}
	secs := float64(units) / float64(timescale)
	if secs <= 0 {
		return 0
	}
	if secs >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(math.Round(secs * float64(time.Second)))
}

// addClamp is a + b held at the largest Duration.
func addClamp(a, b time.Duration) time.Duration {
	if s := a + b; s >= a {
		return s
	}
	return time.Duration(math.MaxInt64)
}
