package mp4

import (
	"sort"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Walker = (*Demuxer)(nil)

// maxFragments bounds the recorded fragment index. One entry is three int64s,
// so the cap is twenty-four MiB live on a hostile file and eleven days of
// one-second fragments on a real one. Past it the walk keeps counting (the
// length is still measured) and stops recording, and a seek past the last
// recorded entry scans forward from it.
const maxFragments = 1 << 20

// fragEntry is one fragment's place: the offset of its moof, the output sample
// its first sample carries, and the same instant in the media's own ticks.
//
// Both units are recorded because both are needed and neither converts back
// exactly: base is what a seek reports and what the reader's PTS must match,
// ticks is what the next fragment's absent tfdt is reconstructed from. Deriving
// one from the other would reintroduce the rounding the tick sum exists to
// avoid.
type fragEntry struct {
	offset int64
	base   int64
	ticks  int64
}

// Walk measures a fragmented movie by reading its moof headers, and is a no-op
// on a progressive one (container.Walker).
//
// One moof payload per fragment, the mdat behind it skipped by its header, so
// the cost is hundreds of bytes per fragment rather than the whole file. It
// leaves behind both things a fragmented movie lacks: an exact length, and an
// index a seek can land through instead of sweeping every moof again.
//
// # A progressive movie walks nothing and reports Walked
//
// Not "it has nothing to do", which would be the other defensible answer, but
// two concrete consequences. Settling a progressive track through SettleLength
// would flip SamplesExact on a sample table nobody verified, which caps every
// progressive AAC decode at the table's total and hands measureLength a fast
// path it has not earned. And projectedLength asks Walked before a pipe
// transcode commits a length into headers it cannot patch: a progressive movie
// answering false would run a no-op confirm walk, still answer false, and
// project nothing, so a streamed WAV of an mp4 would lose its exact sizes.
//
// # What it settles
//
// container.SettleLength against the raw sample run, on the raw timeline, which
// is the sample table's tickDur plus every fragment's samples. The compare that
// decides whether a declared count was right accepts two forms, because writers
// state their index on two different timelines: sum == raw (a packager that
// summed the media timeline) and Delay + sum == raw (ffmpeg, whose sidx is on
// the presentation timeline with the edit applied). Either is an intact file
// and neither is called damage. A count short of the run is an overrun the
// reader tolerated and gets a Note; a run short of the count is Damage, which
// Strict turns into the error the interface asks for.
func (d *Demuxer) Walk() error {
	if !d.fragmented {
		return nil
	}
	if d.fragWalked {
		// Idempotent, and it still settles: SettleLength on an already-settled
		// track is the identity, and a caller that walks twice must not see the
		// length change under it.
		return nil
	}
	raw, err := d.indexFragments()
	if err == nil {
		d.fragIndexed = true
	}
	if err != nil {
		// The source could not be read to its end. Nothing is settled and
		// Walked stays false, so a later Walk tries again; the partial index
		// stands, since a seek into the part that was readable is better than
		// refusing one.
		return err
	}
	// The latch goes last, after the track is actually settled. Setting it
	// first meant a Strict compare failure left Walked true over an unsettled
	// track, and every later Walk took the idempotent early return and
	// reported success on a measurement that never happened.
	if err := d.compareDeclared(raw); err != nil {
		return err
	}
	d.track = container.SettleLength(d.track, raw)
	d.fragWalked = true
	return nil
}

// Walked reports whether the payload has been measured (container.Walker). A
// progressive movie defers nothing, so it is always true there.
//
// It answers for the settlement, not for the index. A seek builds the index
// (ensureFragmentIndex) and leaves the length alone, which is the only honest
// answer it can give: format.media caches its own copy of the track and
// refreshes it from Walk, not from SeekSample, so a seek that settled the
// demuxer's track would flip this true while every length the Media reports
// stayed the open-time one. A run about to commit a count into headers it
// cannot patch reads exactly this flag (waxflow.confirmableLength), and would
// then commit the unverified number.
func (d *Demuxer) Walked() bool { return !d.fragmented || d.fragWalked }

// ensureFragmentIndex builds the fragment map once, for a seek that needs a
// landing rather than a length. It is Walk without the settlement, which is
// what keeps a seek from answering Walked's question on Walk's behalf.
func (d *Demuxer) ensureFragmentIndex() error {
	if d.fragIndexed {
		return nil
	}
	_, err := d.indexFragments()
	if err == nil {
		d.fragIndexed = true
	}
	return err
}

// walkFragments reads every moof from fragStart, building the index and
// returning the raw sample run the fragments hold plus whatever the moov's own
// table holds ahead of them.
//
// It runs on its own cursor and touches none of the reader's: fragOff,
// fragQueue and fragDecode stay where the read left them, so a Walk taken
// mid-read does not move the position.
// A nil error means the scan reached the end of the fragments, whether that
// end was EOF or a box chain that stopped making sense: an mp4 has no resync,
// so a malformed size is the end of the payload and what was counted is the
// whole of it (mpegframes.Walker.Done draws the same line at its last arm).
// Every such end past the head is Damage, so Strict refuses it and a tolerant
// open records how far the fragments got; silently settling a short count as
// exact would let a truncated file claim to have been measured. An error is
// the source failing to deliver bytes it should have, which is not an end and
// is not a measurement.
func (d *Demuxer) indexFragments() (raw int64, err error) {
	rate := int64(d.sel.fmt.Rate)
	rescale := d.sel.timescale > 0 && rate > 0 && d.sel.timescale != rate
	toSamples := func(ticks int64) int64 {
		if !rescale {
			return ticks
		}
		return mulDivSat(ticks, rate, d.sel.timescale)
	}

	// The fragments continue the sample table's timeline, in both units.
	ticks := d.sel.st.tickDur
	raw = d.sel.st.totalDur
	index := d.fragIndex[:0]
	// One fragInfo across the scan. parseFragment reuses its sample slice, so
	// a file of many fragments allocates one run rather than one per moof;
	// a trun may declare up to maxSamplesPerFragment entries, and the read
	// path pays that once per fragment either way.
	var fi fragInfo
	var scratch []byte
	// stop ends the scan where the box chain does: the index stands, and the
	// damage is named so Strict refuses and tolerance records it.
	stop := func(off int64, format string, args ...any) (int64, error) {
		d.fragIndex = index
		return raw, d.warn(off, format, args...)
	}
	off := d.fragStart
	for off < d.size {
		b, err := readBox(d.src, off, d.size)
		if err != nil {
			if waxerr.CodeOf(err) == waxerr.CodeMalformedInput {
				return stop(off, "fragment chain ends at %d: %s", off, err)
			}
			d.fragIndex = index
			return raw, err
		}
		if b.typ == "moof" {
			if b.payloadLen() > maxMoofBytes {
				return stop(b.off, "moof of %d bytes exceeds the %d cap", b.payloadLen(), int64(maxMoofBytes))
			}
			if int64(cap(scratch)) < b.payloadLen() {
				scratch = make([]byte, b.payloadLen())
			}
			buf := scratch[:b.payloadLen()]
			if err := container.ReadFull(d.src, buf, b.payloadOff()); err != nil {
				d.fragIndex = index
				return raw, err // unreadable, not ended
			}
			if err := parseFragment(&fi, buf, d.trex, d.sel.id); err != nil {
				return stop(b.off, "fragment at %d: %s", b.off, err)
			}
			// A moof with nothing for the selected track (an interleaved
			// stream's video fragment) is not a landing place, so it is not
			// indexed and does not move the timeline.
			if len(fi.samples) > 0 {
				// The same containment rule loadFragment enforces, on the same
				// arithmetic. Without it the walk settles an exact length over
				// samples whose bytes are not in the file, and the read that
				// follows fails on the fragment the walk counted.
				if !d.fragmentFits(b, &fi) {
					return stop(b.off, "fragment at %d runs past the end of the source", b.off)
				}
				if fi.haveBaseTime {
					ticks = fi.baseDecodeTime
				}
				base := toSamples(ticks)
				// Appended only while the index stays sorted. fragmentAt binary
				// searches it, which needs base non-decreasing, and a tfdt is a
				// writer's claim that a crafted file can point backwards. Such a
				// fragment still counts toward the length below; it just is not
				// a landing place, and a seek lands on the one before it, which
				// is at or before the target either way.
				if len(index) < maxFragments && (len(index) == 0 || index[len(index)-1].base <= base) {
					index = append(index, fragEntry{offset: b.off, base: base, ticks: ticks})
				}
				// Two sums per fragment, in the two units. ticks carries the
				// raw durations forward, because that is what the next
				// fragment's tfdt would have stated and what its absence has
				// to be reconstructed from. end is where this fragment's
				// samples actually finish, by loadFragment's own arithmetic:
				// each duration rescaled and floored at one output sample.
				end := base
				for _, s := range fi.samples {
					ticks += int64(s.dur)
					dur := int64(s.dur)
					if rescale {
						dur = rescaleTicks(dur, rate, d.sel.timescale)
					}
					end += max(dur, 1)
				}
				// max rather than assignment: a backwards tfdt leaves the run
				// so far as what the reader produced.
				raw = max(raw, end)
			}
		}
		if b.toEnd {
			break
		}
		off = b.off + b.size
	}
	d.fragIndex = index
	return raw, nil
}

// fragmentFits reports whether every sample a fragment declares lies inside the
// source, by loadFragment's own offset arithmetic: default-base-is-moof unless
// a tfhd states a base, then the trun's data offset, then the sizes in order.
func (d *Demuxer) fragmentFits(moof box, fi *fragInfo) bool {
	base := moof.off
	if fi.haveBaseOffset {
		base = fi.baseDataOffset
	}
	off := base + int64(fi.dataOffset)
	for _, s := range fi.samples {
		if off < 0 || off > d.size-int64(s.size) {
			return false
		}
		off += int64(s.size)
	}
	return true
}

// compareDeclared reports what the walk found against what the head declared,
// in mpegframes.Walker.compare's vocabulary: a shortfall is damage and an
// overrun is a note, and an intact file on either timeline is neither.
func (d *Demuxer) compareDeclared(raw int64) error {
	t := d.track
	if t.Samples < 0 || t.SamplesAdvisory {
		return nil // nothing was claimed, so nothing can disagree
	}
	// Two intact forms. A sidx is timed on the presentation timeline with the
	// edit applied (ffmpeg's +delay_moov+global_sidx sums to exactly the truns
	// less the priming), so the raw run it implies is Delay + Samples. A
	// packager that summed the media timeline states the raw run itself.
	if t.Delay+t.Samples == raw || t.Samples == raw {
		return nil
	}
	if t.Delay+t.Samples > raw {
		return d.warn(d.fragStart, "the head declares %d samples, the fragments hold %d",
			t.Delay+t.Samples, raw)
	}
	d.note(d.fragStart, "the fragments hold %d samples, %d more than the head declares",
		raw, raw-t.Delay-t.Samples)
	return nil
}

// fragmentAt finds the recorded fragment whose span contains sample: the last
// entry whose base is at or below it. ok is false when the index is empty or
// the target sits before the first fragment.
func (d *Demuxer) fragmentAt(sample int64) (fragEntry, bool) {
	if len(d.fragIndex) == 0 {
		return fragEntry{}, false
	}
	i := sort.Search(len(d.fragIndex), func(i int) bool { return d.fragIndex[i].base > sample })
	if i == 0 {
		return d.fragIndex[0], true // before them all: land on the first
	}
	return d.fragIndex[i-1], true
}
