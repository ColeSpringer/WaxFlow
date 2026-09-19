package mp4

import (
	"io"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
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
// segment, which is what the HLS client uses. A bare segment has no magic to
// sniff, so it is deliberately not a drivers row.

// Fragmented-input caps (the ADR-0005 hostile-input invariants).
const (
	// maxMoofBytes bounds an in-memory moof. A moof holds only per-sample
	// metadata (duration/size/flags), so even a fragment of many thousands of
	// samples stays small; larger is refused rather than read.
	maxMoofBytes = 8 << 20
	// maxSamplesPerFragment bounds a single trun's sample count.
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
// read by the same parseElst the progressive path uses. This shares
// editListTrims with the progressive gapless resolver but trusts the edit at
// any media_time, since a fragmented movie has no sample table to fall back to,
// and the edit's segment_duration is a measurement of the content
// (SamplesExact).
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
		delay0, seg, haveSeg := editListTrims(t, d.movieTimescale)
		delay = delay0
		if haveSeg {
			return delay, seg, true, false, nil
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
	n := mulDivSat(s.sumDuration, rate, int64(s.timescale))
	exactRescale := s.sumDuration <= sidxSumCap && mulDivSat(n, int64(s.timescale), rate) == s.sumDuration
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
// source holds one piece of it, and the caller (the HLS client) owns the
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
		return mulDivSat(t.duration, rate, t.timescale), "the media header (mdhd)"
	}
	if d.fragmentDuration > 0 && d.movieTimescale > 0 {
		return mulDivSat(d.fragmentDuration, rate, d.movieTimescale), "the movie-extends header (mehd)"
	}
	if d.movieDuration > 0 && d.movieTimescale > 0 {
		return mulDivSat(d.movieDuration, rate, d.movieTimescale), "the movie header (mvhd)"
	}
	return 0, ""
}

// NewFragmentedDemuxer reads a bare CMAF/HLS media segment (moof+mdat with no
// ftyp/moov) using an out-of-band init segment for the codec config, sample
// entry, mvex defaults, and edit list. The HLS client calls it with the init it
// fetched from the playlist's EXT-X-MAP; the media Source holds one or more
// concatenated media segments. A bare segment has no magic to sniff, so it is
// not in the drivers table and `probe segment.m4s` is not expected to work.
func NewFragmentedDemuxer(init []byte, media container.Source) (*Demuxer, error) {
	d := &Demuxer{
		src:  media,
		size: media.Size(),
		w:    srcwin.New(media, media.Size(), "mp4: reading sample data"),
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
		return malformed("fragment sample at %d truncated (want %d bytes)", s.off, s.size)
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
		b, err := readBox(d.src, d.fragOff, d.size)
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
	return io.EOF
}

// loadFragment reads one moof into memory, parses its traf/trun into a sample
// queue anchored at the adjacent mdat, and advances the cursor past the mdat.
func (d *Demuxer) loadFragment(moof box) error {
	if moof.payloadLen() > maxMoofBytes {
		return malformed("moof of %d bytes exceeds the %d cap", moof.payloadLen(), int64(maxMoofBytes))
	}
	buf := make([]byte, moof.payloadLen())
	if err := container.ReadFull(d.src, buf, moof.payloadOff()); err != nil {
		return err
	}
	var fi fragInfo
	if err := parseFragment(&fi, buf, d.trex, d.sel.id); err != nil {
		return err
	}
	// default-base-is-moof: the data reference base is the moof start unless a
	// tfhd base_data_offset overrides it; trun's data_offset is relative to that.
	base := moof.off
	if fi.haveBaseOffset {
		base = fi.baseDataOffset
	}
	off := base + int64(fi.dataOffset)

	// The trun durations and the tfdt base are in media-timescale ticks, but
	// the packet timeline is output samples. Rescale when the media timescale
	// is not the sample rate, matching the progressive stbl reader; our own
	// output and most audio CMAF set them equal, so the common path is a no-op.
	rate := int64(d.sel.fmt.Rate)
	rescale := d.sel.timescale > 0 && rate > 0 && d.sel.timescale != rate

	d.fragQueue = d.fragQueue[:0]
	for _, s := range fi.samples {
		if off < 0 || off > d.size-int64(s.size) {
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
		d.fragQueue = append(d.fragQueue, fragSample{off: off, size: s.size, dur: uint32(dur), sync: s.sync})
		off += int64(s.size)
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
		d.fragDecode = mulDivSat(d.fragTicks, rate, d.sel.timescale)
	}
	for _, s := range fi.samples {
		d.fragTicks += int64(s.dur)
	}

	// Advance past the moof and the mdat that follows it.
	d.fragOff = moof.off + moof.size
	if mdat, err := readBox(d.src, d.fragOff, d.size); err == nil && mdat.typ == "mdat" {
		d.fragOff = mdat.off + mdat.size
	}
	return nil
}

// fragInfo is one parsed fragment: its base decode time, the trun data offset,
// an optional tfhd base_data_offset, and the per-sample metadata.
type fragInfo struct {
	baseDecodeTime int64
	// haveBaseTime says the traf carried a tfdt. Without one the fragment does
	// not state where it begins, so the reader continues from where the last
	// one ended rather than restarting at zero: a movie whose fragments carry
	// no tfdt otherwise replays its whole timeline from 0 at every fragment,
	// and every PTS after the first goes backwards.
	haveBaseTime   bool
	dataOffset     int32
	baseDataOffset int64
	haveBaseOffset bool
	samples        []fragSampleInfo
}

// fragSampleInfo is one sample's timing/size/sync before its byte offset is
// resolved against the mdat.
type fragSampleInfo struct {
	dur, size uint32
	sync      bool
}

// parseFragment parses a moof body into a fragInfo, taking the traf whose tfhd
// track_ID is the selected audio track's. A moof for a different track (a video
// traf in an interleaved stream, or a whole moof for another track) yields an
// empty fragInfo, so the reader skips it rather than feeding the wrong track's
// samples to the audio decoder. selID <= 0 (an unknown selected id) or a traf
// with no readable id falls back to the first traf, the single-track case.
// It fills fi rather than returning one, and keeps its sample slice's capacity:
// a trun may declare up to maxSamplesPerFragment entries, so a scan over a file
// of many fragments would otherwise allocate that run once per moof. Every
// other field is reset, since a reused fi must not carry the previous
// fragment's base time or data offset into this one.
func parseFragment(fi *fragInfo, buf []byte, trex trexDefaults, selID int) error {
	*fi = fragInfo{samples: fi.samples[:0]}
	matched := false
	var perr error
	_ = walkBoxes(buf, func(typ string, body []byte) error {
		if typ != "traf" || matched {
			return nil
		}
		if id, ok := trafTrackID(body); selID > 0 && ok && int(id) != selID {
			return nil // a traf for a different track
		}
		matched = true
		perr = parseTraf(fi, body, trex)
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

// parseTraf parses a traf: tfhd (flags and per-sample defaults), tfdt (base
// decode time), and trun (the sample list).
func parseTraf(fi *fragInfo, body []byte, trex trexDefaults) error {
	defaultDur, defaultSize, defaultFlags := trex.defaultDur, trex.defaultSize, trex.defaultFlags
	var trun []byte
	haveTrun := false
	err := walkBoxes(body, func(typ string, p []byte) error {
		switch typ {
		case "tfhd":
			parseTfhd(fi, p, &defaultDur, &defaultSize, &defaultFlags)
		case "tfdt":
			parseTfdt(fi, p)
		case "trun":
			if !haveTrun {
				trun, haveTrun = p, true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !haveTrun {
		return malformed("traf has no trun")
	}
	return parseTrun(fi, trun, defaultDur, defaultSize, defaultFlags)
}

// parseTfhd reads the track-fragment header: its base_data_offset (when the
// flag is set) and any per-sample defaults it overrides.
func parseTfhd(fi *fragInfo, payload []byte, defaultDur, defaultSize, defaultFlags *uint32) {
	_, flags, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return
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
			fi.baseDataOffset = int64(be64(b))
			fi.haveBaseOffset = true
		}
	}
	if flags&0x000002 != 0 { // sample-description-index-present
		take(4)
	}
	if flags&0x000008 != 0 { // default-sample-duration-present
		if b := take(4); b != nil {
			*defaultDur = be32(b)
		}
	}
	if flags&0x000010 != 0 { // default-sample-size-present
		if b := take(4); b != nil {
			*defaultSize = be32(b)
		}
	}
	if flags&0x000020 != 0 { // default-sample-flags-present
		if b := take(4); b != nil {
			*defaultFlags = be32(b)
		}
	}
	// 0x020000 default-base-is-moof: the base stays the moof start (loadFragment
	// uses moof.off unless a base_data_offset above overrode it), so no field.
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

// parseTrun reads the track-fragment run: the sample count, the data offset,
// and each sample's duration, size, and flags (defaulting to the trex/tfhd
// fallbacks for fields the run omits). Composition-time offsets are skipped
// (audio has none that matter here).
func parseTrun(fi *fragInfo, trun []byte, defaultDur, defaultSize, defaultFlags uint32) error {
	_, flags, rest, ok := fullBox(trun)
	if !ok || len(rest) < 4 {
		return malformed("trun truncated")
	}
	count := be32(rest)
	rest = rest[4:]
	if count > maxSamplesPerFragment {
		return malformed("trun declares %d samples", count)
	}
	if flags&0x000001 != 0 { // data-offset-present
		if len(rest) < 4 {
			return malformed("trun data offset truncated")
		}
		fi.dataOffset = int32(be32(rest))
		rest = rest[4:]
	}
	var firstFlags uint32
	haveFirstFlags := false
	if flags&0x000004 != 0 { // first-sample-flags-present
		if len(rest) < 4 {
			return malformed("trun first-sample flags truncated")
		}
		firstFlags = be32(rest)
		rest = rest[4:]
		haveFirstFlags = true
	}
	perSample := 0
	for _, f := range []uint32{0x000100, 0x000200, 0x000400, 0x000800} {
		if flags&f != 0 {
			perSample += 4
		}
	}
	if int64(count)*int64(perSample) > int64(len(rest)) {
		return malformed("trun declares %d samples for %d bytes", count, len(rest))
	}
	fi.samples = slices.Grow(fi.samples[:0], int(count))[:count]
	for i := uint32(0); i < count; i++ {
		dur, size, sflags := defaultDur, defaultSize, defaultFlags
		if flags&0x000100 != 0 {
			dur = be32(rest)
			rest = rest[4:]
		}
		if flags&0x000200 != 0 {
			size = be32(rest)
			rest = rest[4:]
		}
		if flags&0x000400 != 0 {
			sflags = be32(rest)
			rest = rest[4:]
		}
		if flags&0x000800 != 0 {
			rest = rest[4:] // sample_composition_time_offset
		}
		if i == 0 && haveFirstFlags {
			sflags = firstFlags
		}
		// sample_is_non_sync_sample is bit 0x00010000 of the sample flags; audio
		// frames are all sync, but honor the flag when present.
		fi.samples[i] = fragSampleInfo{dur: dur, size: size, sync: sflags&0x00010000 == 0}
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
func (d *Demuxer) seekFragmented(sample int64) (int64, bool, error) {
	if err := d.ensureFragmentIndex(); err != nil {
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
		e, err := d.scanForward(d.fragIndex[n-1], sample)
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
func (d *Demuxer) scanForward(from fragEntry, sample int64) (fragEntry, error) {
	rate := int64(d.sel.fmt.Rate)
	rescale := d.sel.timescale > 0 && rate > 0 && d.sel.timescale != rate
	landed := from
	ticks := from.ticks
	var fi fragInfo
	var scratch []byte
	off := from.offset
	for off < d.size {
		b, err := readBox(d.src, off, d.size)
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
			if err := parseFragment(&fi, buf, d.trex, d.sel.id); err != nil {
				return landed, nil
			}
			if len(fi.samples) > 0 {
				if fi.haveBaseTime {
					ticks = fi.baseDecodeTime
				}
				base := ticks
				if rescale {
					base = mulDivSat(ticks, rate, d.sel.timescale)
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
