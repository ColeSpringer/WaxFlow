package mp4

import (
	"context"
	"io"
	"math/bits"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/diag"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/satmath"
	"github.com/colespringer/waxflow/waxerr"
)

// This file adds the read side of the fragmented (CMAF) MP4 the muxer and
// segmenter write: a movie whose per-track sample table is empty in moov (it
// carries an mvex movie-extends box) and whose samples live in a run of
// moof+mdat fragments. The progressive demuxer in demux.go reads flat moov/stbl
// movies; this one iterates fragments with a dynamic per-fragment sample queue,
// so memory stays bounded by one fragment regardless of stream length.
//
// Two entry points reach it: a self-contained file (ftyp+init+fragments, what
// our own muxer writes) routes through the normal driver sniff and the
// fragmented branch in parse(); a bare media segment (moof+mdat, no ftyp/moov)
// is reachable only through NewFragmentedDemuxer with an out-of-band init
// segment, as the tests' HLS client does. A bare segment has no magic to
// sniff, so it is deliberately not a drivers row.

// Fragmented-input caps (the ADR-0005 hostile-input invariants).
const (
	// maxMoofBytes bounds an in-memory moof. A moof holds only per-sample
	// metadata (duration/size/flags), so even a fragment of many thousands of
	// samples stays small; larger is refused rather than read.
	maxMoofBytes = 8 << 20
	// maxSamplesPerFragment bounds the samples of one traf, all its runs
	// together.
	maxSamplesPerFragment = 1 << 20
)

// trexDefaults holds the movie-extends per-sample fallbacks (a fragment's trun
// or tfhd may omit fields, deferring to these).
type trexDefaults struct {
	trackID      uint32
	defaultDur   uint32
	defaultSize  uint32
	defaultFlags uint32
	have         bool
}

// fragSample is one queued sample: its byte range in the source and its output
// duration and sync flag.
type fragSample struct {
	off  int64
	size uint32
	dur  uint32
	sync bool
}

// parseMvex records the per-track trex defaults and marks the movie fragmented.
// It is called from parseMoov when the moov carries an mvex box.
//
// One trex per track, and every one is kept: the box states the per-sample
// defaults a traf may omit, so applying another track's is applying the wrong
// durations and sizes to this one's samples. A single-trex movie (everything
// this tree writes) is unaffected either way; an interleaved video+audio
// fragmented movie is the shape that needs the pick, and which track is
// selected is not known until selectAudio runs, so the choice waits for it.
func (d *Demuxer) parseMvex(payload []byte) {
	d.fragmented = true
	_ = walkBoxes(payload, func(typ string, body []byte) error {
		switch typ {
		case "mehd":
			// The movie-extends header: how long the fragments run to, on the
			// movie timeline. Optional and rare, and one of the three header
			// durations the length resolver falls back to.
			if version, _, rest, ok := fullBox(body); ok {
				if version == 1 && len(rest) >= 8 {
					d.fragmentDuration = unknownAsZero64(be64(rest))
				} else if version == 0 && len(rest) >= 4 {
					d.fragmentDuration = unknownAsZero32(be32(rest))
				}
			}
		case "trex":
			if _, _, rest, ok := fullBox(body); ok && len(rest) >= 20 && len(d.trexes) < maxTracks {
				d.trexes = append(d.trexes, trexDefaults{
					trackID:      be32(rest[0:]),
					defaultDur:   be32(rest[8:]),
					defaultSize:  be32(rest[12:]),
					defaultFlags: be32(rest[16:]),
					have:         true,
				})
			}
		}
		return nil
	})
}

// trexFor picks the selected track's movie-extends defaults. A movie with one
// trex hands it over whatever track_ID it names, since a single-track movie
// that misnumbers its own trex is more likely than one that means it for some
// other track; with several, only an exact match will do, and no match means
// the traf has to state everything itself.
func trexFor(trexes []trexDefaults, trackID int) trexDefaults {
	if len(trexes) == 1 {
		return trexes[0]
	}
	for _, t := range trexes {
		if int(t.trackID) == trackID {
			return t
		}
	}
	return trexDefaults{}
}

// fragmentedLength resolves a fragmented track's gapless trims and length,
// in tiers, from what the head of the file states.
//
// The edit list is the first and the only one that is authoritative. It is the
// fMP4 convention: the encoder delay and the playable length ride in edts/elst,
// read by the same parseElst the progressive path uses and rescaled by the
// same editListTrims. A fragmented movie has no sample table to hold the edit
// against, so a segment_duration that names one length is the length
// outright (SamplesExact). One in a timescale coarser than the samples is
// the longest length it can mean, declared and not exact: fragments that end
// short of it by no more than its rounding are whole (compareDeclared), and
// the end trim under that bound is unread until Walk (Track.TrimsUnread).
//
// A segment index is the second, and it is a declared count rather than an
// authoritative one (both flags false, the tier the progressive sample table
// sits in): the writer stated it, nothing has checked it against the fragments,
// and Walk is what turns it exact. It matters because it is the default
// fragmented shape rather than an oddity: ffmpeg's plain fragmented output
// writes no edit list and zeroes both header durations, and a movie with a
// +global_sidx states its length there and nowhere else.
//
// A sidx is timed on the presentation timeline, edit list applied. Measured on
// ffmpeg's +delay_moov+global_sidx: the truns sum to 265624 while the sidx sums
// to 264600, exactly the 1024 the edit delays. So a delay stated beside a sidx
// stays in Delay and is never subtracted from the count, and the raw timeline
// the pair implies is Delay + Samples, which is what a walk of the truns finds.
//
// Without either, the length is left unknown and the track decodes to end of
// stream.
func (d *Demuxer) fragmentedLength(t *track) (delay, samples int64, exact, advisory bool, err error) {
	if t.hasEdit {
		delay0, seg, slack, haveSeg := editListTrims(t, d.movieTimescale)
		delay = delay0
		if haveSeg {
			// A duration that names one length is the length; a coarser
			// one is a bound, exact once Walk holds it to the fragments.
			d.fragSlack = slack
			return delay, seg, slack == 0, false, nil
		}
	}
	s, ok, err := d.readSidxAhead(t)
	if err != nil {
		return 0, -1, false, false, err
	}
	if !ok {
		n, box := d.headerDuration(t)
		if n <= 0 {
			return delay, -1, false, false, nil
		}
		d.note(0, "fragmented movie length taken from %s (advisory; the fragments have not been counted)", box)
		return delay, n, false, true, nil
	}
	// A hybrid movie's index describes its fragments; the samples in the moov's
	// own table sit ahead of them and are the reader's first packets, so the
	// length is both. A table that had to be rescaled is a rounded total, which
	// makes the sum one too.
	head, headRounded := t.st.totalDur, t.st.rescaled
	rate := int64(t.fmt.Rate)
	if int64(s.timescale) == rate || s.timescale == 0 || rate <= 0 {
		return delay, head + s.sumDuration, false, headRounded, nil
	}
	// A time base that is not the sample rate rescales, and a rescale that is
	// not exact demotes the count to advisory, the same rule the sample table's
	// own rescale follows: a rounded total is fit to display and unfit to sum.
	n := satmath.MulDiv(s.sumDuration, rate, int64(s.timescale))
	exactRescale := s.sumDuration <= sidxSumCap && satmath.MulDiv(n, int64(s.timescale), rate) == s.sumDuration
	return delay, head + n, false, !exactRescale || headRounded, nil
}

// headerDuration is the last length tier: a duration out of the movie or media
// header, advisory, and named in a Note so a reader knows which box it came
// from.
//
// Advisory because it is not a count of anything this file can be held to.
// mdhd and mvhd durations in a fragmented movie are zero in every shape ffmpeg
// writes, and a writer that does fill them (YouTube's itag 140 states the same
// number twice) is describing the presentation rather than the samples. Which
// timeline the number sits on therefore does not matter much, and the tier is
// ordered by how specific the box is: the track's own media header first, then
// the movie-extends header, then the movie header.
//
// Two conditions gate it, and both are about not blessing a number that
// describes something else. A populated sample table means the movie is a
// hybrid, and the spec reading of its mdhd duration is the table's own part
// (ffmpeg writes 45056 for the 44 AUs in the moov of a six-second file), so a
// header duration beside a table is ignored outright. And a bare segment source
// gets no length from its init at all: the init describes a presentation, the
// source holds one piece of it, and the caller (an HLS client) owns the
// length of what it assembled.
func (d *Demuxer) headerDuration(t *track) (samples int64, box string) {
	if d.bareSegments || t.st.total > 0 {
		return 0, ""
	}
	rate := int64(t.fmt.Rate)
	if rate <= 0 {
		return 0, ""
	}
	if t.duration > 0 && t.timescale > 0 {
		return satmath.MulDiv(t.duration, rate, t.timescale), "the media header (mdhd)"
	}
	if d.fragmentDuration > 0 && d.movieTimescale > 0 {
		return satmath.MulDiv(d.fragmentDuration, rate, d.movieTimescale), "the movie-extends header (mehd)"
	}
	if d.movieDuration > 0 && d.movieTimescale > 0 {
		return satmath.MulDiv(d.movieDuration, rate, d.movieTimescale), "the movie header (mvhd)"
	}
	return 0, ""
}

// NewFragmentedDemuxer reads a bare CMAF/HLS media segment (moof+mdat with no
// ftyp/moov) using an out-of-band init segment for the codec config, sample
// entry, mvex defaults, and edit list. An HLS client calls it with the init it
// fetched from the playlist's EXT-X-MAP; the media Source holds one or more
// concatenated media segments. A bare segment has no magic to sniff, so it is
// not in the drivers table and `probe segment.m4s` is not expected to work.
func NewFragmentedDemuxer(init []byte, media container.Source) (*Demuxer, error) {
	d := &Demuxer{
		src:  media,
		size: media.Size(),
		w:    srcwin.New(media, media.Size(), "mp4: reading sample data"),
		log:  diag.New("mp4: ", nil),
	}
	moov, err := findInitMoov(init)
	if err != nil {
		return nil, err
	}
	tracks, err := d.parseMoov(moov)
	if err != nil {
		return nil, err
	}
	// The init declares a fragmented movie by construction; force the flag even
	// if the moov omitted mvex, since the media source is fragments.
	d.fragmented = true
	d.bareSegments = true
	if err := d.selectAudio(tracks); err != nil {
		return nil, err
	}
	// Fragments begin at the top of the media source.
	d.fragOff = 0
	return d, nil
}

// findInitMoov extracts the moov payload from an init segment held in memory.
func findInitMoov(init []byte) ([]byte, error) {
	var moov []byte
	err := walkBoxes(init, func(typ string, payload []byte) error {
		if typ == "moov" && moov == nil {
			moov = payload
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if moov == nil {
		return nil, malformed("init segment has no moov box")
	}
	return moov, nil
}

// readFragmentedPacket delivers the next sample from the fragment queue,
// refilling it from the next moof+mdat when it drains. Sample data aliases the
// read window and is reused across calls.
func (d *Demuxer) readFragmentedPacket(pkt *container.Packet) error {
	for d.fragIdx >= len(d.fragQueue) {
		if err := d.nextFragment(); err != nil {
			return err // io.EOF at end of stream
		}
	}
	s := d.fragQueue[d.fragIdx]
	d.fragIdx++
	d.w.Trim(s.off)
	data := d.w.BytesAt(s.off, int(s.size))
	if len(data) != int(s.size) {
		if err := d.w.Err(); err != nil {
			return err
		}
		return malformed("fragment sample at %d truncated (want %d bytes)", d.log.At(s.off), s.size)
	}
	pts := d.fragDecode
	d.fragDecode += int64(s.dur)
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  pts,
			Dur:  int64(s.dur),
			Sync: s.sync,
		},
	}
	return nil
}

// resetFragments rewinds the fragment iterator to the first fragment and drops
// the queued samples, so the next read past the sample table starts from the
// top. It is the hybrid movie's half of a seek into the moov's own table: the
// table is rewound by d.cur and the fragments behind it by this.
//
// fragDecode is seeded from the table's own end rather than zeroed, which is
// where the fragments' timeline begins when their trafs carry no tfdt to say
// so; a tfdt overrides it in loadFragment, as it does everywhere.
func (d *Demuxer) resetFragments() {
	d.fragOff = d.fragStart
	d.fragQueue = d.fragQueue[:0]
	d.fragIdx = 0
	d.fragDecode, d.fragTicks = d.sel.st.totalDur, d.sel.st.tickDur
}

// nextFragment advances to the next moof+mdat pair from d.fragOff and rebuilds
// the sample queue. Non-fragment boxes (styp, sidx, free) are skipped. It
// returns io.EOF when no more moof boxes remain.
func (d *Demuxer) nextFragment() error {
	for d.fragOff < d.size {
		b, err := d.readBox(d.fragOff, d.size)
		if err != nil {
			return err
		}
		if b.typ == "moof" {
			return d.loadFragment(b)
		}
		if b.toEnd {
			break
		}
		d.fragOff = b.off + b.size
	}
	return d.fragmentsEnded()
}

// fragmentsEnded is the read reaching the end of the fragments. The run is
// held against what the head declared, as Walk holds it, so a movie cut short
// at a fragment boundary is damage the read that reaches it finds, and a
// strict read refuses. The length is left alone: settling it is Walk's (see
// Walked).
//
// The run is the scan's where a seek or a walk has made one, and otherwise
// the reader's own, which began at the first fragment and has loaded every
// one since. Both end each fragment by the same arithmetic.
func (d *Demuxer) fragmentsEnded() error {
	raw := max(d.fragRun, d.sel.st.totalDur)
	if d.fragIndexed {
		raw = d.fragScan.raw
	}
	if err := d.compareDeclared(raw); err != nil {
		return err
	}
	return io.EOF
}

// loadFragment reads one moof into memory, parses the selected track's traf
// into a sample queue, each sample placed where its runs say, and advances
// the cursor past the moof and the mdat behind it.
func (d *Demuxer) loadFragment(moof box) error {
	if moof.payloadLen() > maxMoofBytes {
		return malformed("moof of %d bytes exceeds the %d cap", moof.payloadLen(), int64(maxMoofBytes))
	}
	buf := make([]byte, moof.payloadLen())
	if err := container.ReadFull(d.src, buf, moof.payloadOff()); err != nil {
		return err
	}
	var fi fragInfo
	if err := parseFragment(&fi, buf, d.trexes, d.sel.id, moof.off); err != nil {
		return err
	}

	// The trun durations and the tfdt base are in media-timescale ticks, but
	// the packet timeline is output samples. Rescale when the media timescale
	// is not the sample rate, matching the progressive stbl reader; our own
	// output and most audio CMAF set them equal, so the common path is a no-op.
	rate := int64(d.sel.fmt.Rate)
	rescale := d.sel.timescale > 0 && rate > 0 && d.sel.timescale != rate

	d.fragQueue = d.fragQueue[:0]
	for _, s := range fi.samples {
		if s.off < 0 || s.off > d.size-int64(s.size) {
			return malformed("fragment sample runs past end of source")
		}
		dur := int64(s.dur)
		if rescale {
			dur = rescaleTicks(dur, rate, d.sel.timescale)
		}
		if dur < 1 {
			// Every sample must advance the timeline: a zero trun duration (or a
			// rescale that floored to nothing) would stall PTS and hand a
			// non-positive Dur out, the same clamp the stbl reader applies.
			dur = 1
		}
		d.fragQueue = append(d.fragQueue, fragSample{off: s.off, size: s.size, dur: uint32(dur), sync: s.sync})
	}
	d.fragIdx = 0
	// Where this fragment begins, in ticks: its own tfdt, or the running sum of
	// every prior sample's raw duration. Converted here rather than
	// accumulated in output samples, so a fragment with no tfdt lands exactly
	// where one carrying it would (see Demuxer.fragTicks).
	if fi.haveBaseTime {
		d.fragTicks = fi.baseDecodeTime
	}
	d.fragDecode = d.fragTicks
	if rescale {
		d.fragDecode = satmath.MulDiv(d.fragTicks, rate, d.sel.timescale)
	}
	for _, s := range fi.samples {
		d.fragTicks += int64(s.dur)
	}
	end := d.fragDecode
	for _, s := range d.fragQueue {
		end += int64(s.dur)
	}
	d.fragRun = max(d.fragRun, end)

	// Advance past the moof and the mdat that follows it.
	d.fragOff = moof.off + moof.size
	if mdat, err := d.readBox(d.fragOff, d.size); err == nil && mdat.typ == "mdat" {
		d.fragOff = mdat.off + mdat.size
	}
	return nil
}

// fragInfo is one parsed fragment: its base decode time and the selected
// track's samples, each placed in the source.
type fragInfo struct {
	baseDecodeTime int64
	// haveBaseTime says the traf carried a tfdt. Without one the fragment does
	// not state where it begins, so the reader continues from where the last
	// one ended rather than restarting at zero: a movie whose fragments carry
	// no tfdt otherwise replays its whole timeline from 0 at every fragment,
	// and every PTS after the first goes backwards.
	haveBaseTime bool
	samples      []fragSampleInfo
}

// fragSampleInfo is one sample: where its bytes begin in the source, its
// size, and its timing and sync.
type fragSampleInfo struct {
	off       int64
	dur, size uint32
	sync      bool
}

// parseFragment parses a moof body into a fragInfo, taking the traf whose tfhd
// track_ID is the selected audio track's. A moof for a different track (a video
// traf in an interleaved stream, or a whole moof for another track) yields an
// empty fragInfo, so the reader skips it rather than feeding the wrong track's
// samples to the audio decoder. selID <= 0 (an unknown selected id) or a traf
// with no readable id falls back to the first traf, the single-track case.
// moofOff is the moof's first byte, which places the samples (trafChain).
// It fills fi rather than returning one, and keeps its sample slice's capacity:
// a traf may declare up to maxSamplesPerFragment entries, so a scan over a file
// of many fragments would otherwise allocate that run once per moof. Every
// other field is reset, since a reused fi must not carry the previous
// fragment's base time into this one.
func parseFragment(fi *fragInfo, buf []byte, trexes []trexDefaults, selID int, moofOff int64) error {
	*fi = fragInfo{samples: fi.samples[:0]}
	c := trafChain{moof: moofOff, end: moofOff, known: true}
	matched := false
	var perr error
	_ = walkBoxes(buf, func(typ string, body []byte) error {
		if typ != "traf" || matched {
			return nil
		}
		if id, ok := trafTrackID(body); selID > 0 && ok && int(id) != selID {
			// A traf for a different track: only where its data ends is
			// taken, for a later traf that states no base.
			_ = c.add(nil, body, trexFor(trexes, int(id)))
			return nil
		}
		matched = true
		perr = c.add(fi, body, trexFor(trexes, selID))
		return nil
	})
	return perr
}

// trafTrackID reads the track_ID from a traf's tfhd (the first field after the
// version/flags), so a moof can be routed to the right track before its samples
// are parsed. ok is false when the traf has no readable tfhd.
func trafTrackID(body []byte) (uint32, bool) {
	var id uint32
	found := false
	_ = walkBoxes(body, func(typ string, p []byte) error {
		if typ == "tfhd" && !found {
			if _, _, rest, ok := fullBox(p); ok && len(rest) >= 4 {
				id, found = be32(rest), true
			}
		}
		return nil
	})
	return id, found
}

// trafChain places the trafs of one moof (ISO/IEC 14496-12, 8.8.7). A traf
// is based where its tfhd says: at an offset it states, or at the moof. One
// whose tfhd says neither is based at the moof when it is the moof's first
// traf, and otherwise where the data of the traf before it ends.
type trafChain struct {
	moof  int64 // the moof's first byte
	end   int64 // where the data of the trafs so far ends
	known bool  // end could be worked out
}

// add parses a traf: tfhd (its base and per-sample defaults), tfdt (base
// decode time) and its runs, placing each sample, and moves the chain to the
// end of the traf's data. With fi nil the traf is only measured. A traf that
// fails leaves the chain's end unknown.
func (c *trafChain) add(fi *fragInfo, body []byte, trex trexDefaults) error {
	h := trafHead{dur: trex.defaultDur, size: trex.defaultSize, flags: trex.defaultFlags}
	sawTfhd := false
	err := walkBoxes(body, func(typ string, p []byte) error {
		switch typ {
		case "tfhd":
			if !sawTfhd {
				sawTfhd = true
				h.parse(p)
			}
		case "tfdt":
			if fi != nil {
				parseTfdt(fi, p)
			}
		}
		return nil
	})
	base, known := h.dataBase(*c)
	r := trafRuns{fi: fi, head: h, base: base, at: base}
	if err == nil {
		err = walkBoxes(body, func(typ string, p []byte) error {
			if typ != "trun" {
				return nil
			}
			r.runs++
			return r.add(p)
		})
	}
	switch {
	case err != nil:
	case fi == nil:
	case r.runs == 0:
		err = malformed("traf has no trun")
	case !known:
		err = malformed("traf is based on the data of a traf that cannot be read")
	}
	if err != nil {
		c.known = false
		return err
	}
	// A traf with no run defines no data, and leaves the end where it was.
	if r.runs > 0 {
		c.end, c.known = r.at, known
	}
	return nil
}

// trafHead is what a tfhd states: the base of the traf's data, and the
// per-sample defaults its runs fall back to, which begin as the trex's.
type trafHead struct {
	base             int64
	haveBase         bool // base-data-offset-present
	baseIsMoof       bool // default-base-is-moof
	dur, size, flags uint32
}

// parse reads a tfhd into h. ok is false when it does not hold a track_ID;
// a field past that which is cut off is left as it was.
func (h *trafHead) parse(payload []byte) (ok bool) {
	_, flags, rest, full := fullBox(payload)
	if !full || len(rest) < 4 {
		return false
	}
	q := rest[4:] // after track_ID
	take := func(n int) []byte {
		if len(q) < n {
			q = nil
			return nil
		}
		b := q[:n]
		q = q[n:]
		return b
	}
	if flags&0x000001 != 0 { // base-data-offset-present
		if b := take(8); b != nil {
			h.base, h.haveBase = int64(be64(b)), true
		}
	}
	if flags&0x000002 != 0 { // sample-description-index-present
		take(4)
	}
	if flags&0x000008 != 0 { // default-sample-duration-present
		if b := take(4); b != nil {
			h.dur = be32(b)
		}
	}
	if flags&0x000010 != 0 { // default-sample-size-present
		if b := take(4); b != nil {
			h.size = be32(b)
		}
	}
	if flags&0x000020 != 0 { // default-sample-flags-present
		if b := take(4); b != nil {
			h.flags = be32(b)
		}
	}
	h.baseIsMoof = flags&0x020000 != 0
	return true
}

// dataBase is where the traf's data is based in chain c, and whether that
// is known: a stated offset first, then the moof, then the chain's end.
func (h trafHead) dataBase(c trafChain) (base int64, known bool) {
	switch {
	case h.haveBase:
		return h.base, true
	case h.baseIsMoof:
		return c.moof, true
	}
	return c.end, c.known
}

// parseTfdt reads the base media decode time (version 0: 32-bit; version 1: 64).
func parseTfdt(fi *fragInfo, payload []byte) {
	version, _, rest, ok := fullBox(payload)
	if !ok {
		return
	}
	if version == 1 {
		if len(rest) >= 8 {
			fi.baseDecodeTime, fi.haveBaseTime = int64(be64(rest)), true
		}
		return
	}
	if len(rest) >= 4 {
		fi.baseDecodeTime, fi.haveBaseTime = int64(be32(rest)), true
	}
}

// The trun flags read here.
const (
	trunDataOffset  = 0x000001
	trunFirstFlags  = 0x000004
	trunDuration    = 0x000100
	trunSize        = 0x000200
	trunSampleFlags = 0x000400
	trunComposition = 0x000800
)

// trunHead is a run's fixed fields and the per-sample entries behind them.
type trunHead struct {
	flags      uint32
	count      uint32
	dataOff    int32  // where flags has trunDataOffset
	firstFlags uint32 // where flags has trunFirstFlags
	entries    []byte
}

// parseTrunHead reads a trun's fixed fields. ok is false when they do not
// lie whole in trun, which may be cut off behind them.
func parseTrunHead(trun []byte) (t trunHead, ok bool) {
	_, flags, rest, full := fullBox(trun)
	if !full || len(rest) < 4 {
		return t, false
	}
	t.flags, t.count, rest = flags, be32(rest), rest[4:]
	if flags&trunDataOffset != 0 {
		if len(rest) < 4 {
			return t, false
		}
		t.dataOff, rest = int32(be32(rest)), rest[4:]
	}
	if flags&trunFirstFlags != 0 {
		if len(rest) < 4 {
			return t, false
		}
		t.firstFlags, rest = be32(rest), rest[4:]
	}
	t.entries = rest
	return t, true
}

// entryLen is the bytes one sample takes in the run's table.
func (t trunHead) entryLen() int {
	return 4 * bits.OnesCount32(t.flags&(trunDuration|trunSize|trunSampleFlags|trunComposition))
}

// trafRuns places the runs of one traf in order (ISO/IEC 14496-12, 8.8.8):
// a run begins at its data offset from the traf's base, and one that states
// none where the run before it ended.
type trafRuns struct {
	fi    *fragInfo // nil to measure only
	head  trafHead
	base  int64
	at    int64 // where the data of the runs so far ends
	runs  int
	count int // samples so far, held to maxSamplesPerFragment
}

// add reads one trun: each sample's duration, size and flags, falling back
// to the traf's defaults, and where its bytes lie. Composition-time offsets
// are skipped (audio has none that matter here).
func (r *trafRuns) add(trun []byte) error {
	t, ok := parseTrunHead(trun)
	if !ok {
		return malformed("trun truncated")
	}
	if int64(t.count) > int64(maxSamplesPerFragment-r.count) {
		return malformed("traf declares more than %d samples", maxSamplesPerFragment)
	}
	n := t.entryLen()
	if int64(t.count)*int64(n) > int64(len(t.entries)) {
		return malformed("trun declares %d samples for %d bytes", t.count, len(t.entries))
	}
	r.count += int(t.count)
	if t.flags&trunDataOffset != 0 {
		r.at = satmath.Add(r.base, int64(t.dataOff))
	}
	rest := t.entries
	if r.fi == nil {
		// Measured only: the sizes are all that is read, and where the run
		// states none its length is a product, so a run of a million
		// samples in sixteen bytes costs nothing.
		if t.flags&trunSize == 0 {
			r.at = satmath.Add(r.at, int64(t.count)*int64(r.head.size))
			return nil
		}
		sizeAt := 0
		if t.flags&trunDuration != 0 {
			sizeAt = 4
		}
		for i := range int(t.count) {
			r.at = satmath.Add(r.at, int64(be32(rest[i*n+sizeAt:])))
		}
		return nil
	}
	r.fi.samples = slices.Grow(r.fi.samples, int(t.count))
	for i := range t.count {
		dur, size, sflags := r.head.dur, r.head.size, r.head.flags
		if t.flags&trunDuration != 0 {
			dur = be32(rest)
			rest = rest[4:]
		}
		if t.flags&trunSize != 0 {
			size = be32(rest)
			rest = rest[4:]
		}
		if t.flags&trunSampleFlags != 0 {
			sflags = be32(rest)
			rest = rest[4:]
		}
		if t.flags&trunComposition != 0 {
			rest = rest[4:]
		}
		if i == 0 && t.flags&trunFirstFlags != 0 {
			sflags = t.firstFlags
		}
		// sample_is_non_sync_sample is bit 0x00010000 of the sample flags; audio
		// frames are all sync, but honor the flag when present.
		r.fi.samples = append(r.fi.samples, fragSampleInfo{off: r.at, dur: dur, size: size, sync: sflags&0x00010000 == 0})
		r.at = satmath.Add(r.at, int64(size))
	}
	return nil
}

// seekFragmented lands the fragment iterator on the fragment whose decode-time
// span contains sample (or the earliest fragment when it precedes them all).
// It returns the landed decode time; the remaining pre-roll is
// decode-and-discard in format.Media. Every audio sample is a sync point, so
// landing on a fragment boundary is landing on a sync point.
//
// It lands through the fragment index, built here once if a walk has not
// already built it. That trades a per-seek cost for a one-off one: the scan
// this replaced read every moof payload up to the target on every seek, where
// this reads every moof once and every seek after it is a binary search. The
// open stays head-sized either way; the first seek is where a fragmented movie
// pays for its lack of an index, and it pays once.
//
// It also fixes the landing on a file whose fragments carry no tfdt: that scan
// read each fragment's base as 0, so no fragment ever "started after the
// target" and every seek landed on the last one while reporting position 0.
//
// Building the index is deliberately not Walk: a seek must not settle the
// length, because format.media refreshes its own copy of the track from Walk
// and not from SeekSample (see Walked). A scan stopped by damage still leaves
// the index it built, and seeking into the intact part is better than refusing.
func (d *Demuxer) seekFragmented(ctx context.Context, sample int64) (int64, bool, error) {
	if err := d.ensureFragmentIndex(ctx); err != nil {
		return 0, false, err
	}
	at := fragEntry{offset: d.fragStart, base: d.sel.st.totalDur, ticks: d.sel.st.tickDur}
	if e, ok := d.fragmentAt(sample); ok {
		at = e
	}
	// Past a capped index there are fragments the walk counted and did not
	// record, so the tail is scanned the old way, from the last entry rather
	// than from the top.
	if n := len(d.fragIndex); n == maxFragments && sample > d.fragIndex[n-1].base {
		e, err := d.scanForward(ctx, d.fragIndex[n-1], sample)
		if err != nil {
			return 0, false, err
		}
		at = e
	}
	// Every fragment begins after the target. With a sample table ahead of
	// them the nearest sync point at or before it is in there, so the caller
	// lands in the table instead; the fragments can begin anywhere, since a
	// tfdt is a writer's claim. With no table the earliest fragment is the
	// earliest sync point, which container.Seeker allows to exceed the target.
	if at.base > sample && d.sel.st.total > 0 {
		return 0, false, nil
	}
	landedOff, landed := at.offset, at.base
	d.fragTicks = at.ticks
	d.fragOff = landedOff
	d.fragQueue = d.fragQueue[:0]
	d.fragIdx = 0
	d.fragDecode = landed
	// The sample table is behind us. On an ordinary fragmented movie it is
	// empty and this is a no-op; on a hybrid one it is what stops ReadPacket
	// from serving the moov's samples again from the top, which is the branch
	// it takes while cur is short of the table's end.
	d.cur = d.sel.st.total
	return landed, true, nil
}

// scanForward walks from a recorded fragment to the last one at or before
// sample, for the tail of a file with more fragments than the index records
// (maxFragments). It reads one moof per fragment, as the walk does, and returns
// the entry it landed on.
//
// It re-reads the anchor fragment rather than skipping it, which is what makes
// the running tick sum right: the next fragment's position, when it carries no
// tfdt of its own, is the anchor's start plus the anchor's own sample
// durations, and a scan that began after the anchor would seed the sum with
// the anchor's start and place every fragment behind it one fragment early.
// Re-reading recomputes the anchor's own entry unchanged and then adds its
// durations, which is exactly the walk's arithmetic.
func (d *Demuxer) scanForward(ctx context.Context, from fragEntry, sample int64) (fragEntry, error) {
	rate := int64(d.sel.fmt.Rate)
	rescale := d.sel.timescale > 0 && rate > 0 && d.sel.timescale != rate
	landed := from
	ticks := from.ticks
	var fi fragInfo
	var scratch []byte
	off := from.offset
	for off < d.size {
		if err := container.WalkCanceled(ctx, "mp4: "); err != nil {
			return landed, err
		}
		b, err := d.readBox(off, d.size)
		if err != nil {
			if waxerr.CodeOf(err) == waxerr.CodeMalformedInput {
				return landed, nil // the chain is over; land on the last good one
			}
			return landed, err // the source failed, and a landing is not an answer
		}
		if b.typ == "moof" {
			if b.payloadLen() > maxMoofBytes {
				return landed, nil
			}
			if int64(cap(scratch)) < b.payloadLen() {
				scratch = make([]byte, b.payloadLen())
			}
			buf := scratch[:b.payloadLen()]
			if err := container.ReadFull(d.src, buf, b.payloadOff()); err != nil {
				return landed, err
			}
			if err := parseFragment(&fi, buf, d.trexes, d.sel.id, b.off); err != nil {
				return landed, nil
			}
			if len(fi.samples) > 0 {
				if fi.haveBaseTime {
					ticks = fi.baseDecodeTime
				}
				base := ticks
				if rescale {
					base = satmath.MulDiv(ticks, rate, d.sel.timescale)
				}
				if base > sample {
					return landed, nil
				}
				landed = fragEntry{offset: b.off, base: base, ticks: ticks}
				for _, s := range fi.samples {
					ticks += int64(s.dur)
				}
			}
		}
		if b.toEnd {
			break
		}
		off = b.off + b.size
	}
	return landed, nil
}
