package mp4

import (
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
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

// parseMvex records the trex per-sample defaults and marks the movie
// fragmented. It is called from parseMoov when the moov carries an mvex box.
func (d *Demuxer) parseMvex(payload []byte) {
	d.fragmented = true
	_ = walkBoxes(payload, func(typ string, body []byte) error {
		if typ != "trex" {
			return nil
		}
		if _, _, rest, ok := fullBox(body); ok && len(rest) >= 20 {
			d.trex = trexDefaults{
				trackID:      be32(rest[0:]),
				defaultDur:   be32(rest[8:]),
				defaultSize:  be32(rest[12:]),
				defaultFlags: be32(rest[16:]),
				have:         true,
			}
		}
		return nil
	})
}

// fragmentedGapless resolves the gapless trims for a fragmented track from the
// init segment's edit list (the fMP4 convention: the encoder delay and the
// playable length ride in edts/elst, read by the same parseElst the progressive
// path uses). It shares editListTrims with the progressive gapless resolver but
// trusts the edit at any media_time, since a fragmented movie has no sample
// table to fall back to: the edit's segment_duration is the authoritative
// length (SamplesExact). Without a usable edit the length is left unknown and
// the track decodes to end of stream.
func (d *Demuxer) fragmentedGapless(t *track) (delay, samples int64, exact bool) {
	if !t.hasEdit {
		return 0, -1, false
	}
	delay, seg, haveSeg := editListTrims(t, d.movieTimescale)
	if haveSeg {
		return delay, seg, true
	}
	return delay, -1, false
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
	fi, err := parseFragment(buf, d.trex, d.sel.id)
	if err != nil {
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
	d.fragDecode = fi.baseDecodeTime
	if rescale {
		d.fragDecode = mulDivSat(fi.baseDecodeTime, rate, d.sel.timescale)
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
func parseFragment(buf []byte, trex trexDefaults, selID int) (fragInfo, error) {
	var fi fragInfo
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
		perr = parseTraf(&fi, body, trex)
		return nil
	})
	return fi, perr
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
			fi.baseDecodeTime = int64(be64(rest))
		}
		return
	}
	if len(rest) >= 4 {
		fi.baseDecodeTime = int64(be32(rest))
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
	fi.samples = make([]fragSampleInfo, count)
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
// span contains sample (or the earliest fragment when it precedes them all),
// scanning moof headers from the top. It returns the landed decode time; the
// remaining pre-roll is decode-and-discard in format.Media. Every audio sample
// is a sync point, so landing on a fragment boundary is landing on a sync point.
func (d *Demuxer) seekFragmented(sample int64) (int64, error) {
	off := d.fragStart
	landedOff, landed := off, int64(0)
	// One scratch buffer, grown as needed and reused across the whole moof
	// scan: a long file has thousands of fragments, and a fresh allocation per
	// moof would be needless GC pressure. Only the tfdt is read, not the sample
	// runs, so no per-sample slice is allocated either.
	rate := int64(d.sel.fmt.Rate)
	rescale := d.sel.timescale > 0 && rate > 0 && d.sel.timescale != rate
	var scratch []byte
	for off < d.size {
		b, err := readBox(d.src, off, d.size)
		if err != nil {
			return 0, err
		}
		if b.typ == "moof" {
			if b.payloadLen() > maxMoofBytes {
				return 0, malformed("moof exceeds cap during seek")
			}
			if int64(cap(scratch)) < b.payloadLen() {
				scratch = make([]byte, b.payloadLen())
			}
			buf := scratch[:b.payloadLen()]
			if err := container.ReadFull(d.src, buf, b.payloadOff()); err != nil {
				return 0, err
			}
			base := moofBaseTime(buf) // media ticks
			if rescale {
				base = mulDivSat(base, rate, d.sel.timescale) // to output samples
			}
			if base > sample {
				break // this fragment starts after the target; keep the previous
			}
			landedOff, landed = b.off, base
		}
		if b.toEnd {
			break
		}
		off = b.off + b.size
	}
	d.fragOff = landedOff
	d.fragQueue = d.fragQueue[:0]
	d.fragIdx = 0
	d.fragDecode = landed
	return landed, nil
}

// moofBaseTime extracts a fragment's base media decode time (tfdt) without
// parsing its sample runs, for the seek scan where only the fragment's start
// time is needed. A moof with no tfdt reports 0 (the movie start); a run that
// is itself malformed is caught later by loadFragment when the fragment is
// actually read.
func moofBaseTime(buf []byte) int64 {
	var fi fragInfo
	_ = walkBoxes(buf, func(typ string, body []byte) error {
		if typ != "traf" {
			return nil
		}
		return walkBoxes(body, func(inner string, p []byte) error {
			if inner == "tfdt" {
				parseTfdt(&fi, p)
			}
			return nil
		})
	})
	return fi.baseDecodeTime
}
