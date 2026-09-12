package mp4

import (
	"math"
	"math/bits"
	"slices"
	"sort"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/mp3"
)

// sampleTable is a track's flattened sample map: per-sample file offset and
// byte size, a run-encoded time base in output samples, and the sync set.
//
// A byte-linear codec takes a second representation instead (uniform, below):
// its samples are all one size and one duration, so an index per chunk
// answers every question the per-sample arrays do, at a cost the file's own
// chunk count bounds rather than its sample count. The distinction matters
// because a PCM sample is one FRAME: three minutes of 48 kHz stereo is 8.6
// million of them, and a table with two entries each would be a hundred
// megabytes of index over eight hundred bytes of chunk offsets.
type sampleTable struct {
	offsets []int64  // per-sample file offset
	sizes   []uint32 // per-sample byte size
	total   int64    // sample count (== len(offsets) unless uniform)

	// The uniform representation. unitBytes and unitDur are the constant
	// size and duration of one sample; chunkOff is each chunk's file offset
	// and chunkFirst the index of its first sample, with a trailing sentinel
	// equal to total so chunk k spans [chunkFirst[k], chunkFirst[k+1]).
	// Samples within a chunk are contiguous, which is what the format
	// guarantees and what lets one packet carry many of them.
	uniform    bool
	unitBytes  int64
	unitDur    int64
	chunkOff   []int64
	chunkFirst []int64

	// Time base in output samples (rescaled from mdhd ticks to the codec
	// rate), run-encoded so uniform audio costs a handful of entries.
	runStart []int64 // sample index at each run's start
	runPTS   []int64 // output-sample position at each run's start
	runDelta []int64 // per-sample output duration within the run
	runCount []int64 // samples in the run
	totalDur int64   // total output samples across all runs (raw timeline)
	// rescaled reports that totalDur was converted from a media timescale that
	// is not the codec rate, so it is a rounded total rather than a counted
	// one: the conversion floors each run and the sum carries every floor. A
	// length derived from it is advisory, and gapless says so.
	rescaled bool

	// sync holds the 0-based sync sample indices in ascending order; nil
	// means every sample is a sync point (the audio norm).
	sync []int64
}

// The Layer III seek backoff, container/mpa's rule restated for a sample
// table. Both halves exist because a frame is not independently decodable.
const (
	// mp3SeekPreroll is the samples half: three frames for the filterbank's
	// IMDCT overlap and synthesis window, counted at the longest frame MPEG
	// audio has (1152 samples), so a 576-sample MPEG-2 stream backs off six
	// frames rather than three. Overshooting costs discarded decode.
	mp3SeekPreroll = 3 * 1152
	// mp3ReservoirBytes is the bytes half: a frame's main data may begin up
	// to 511 bytes before its own header, so the frames a landing skips have
	// to carry at least that much main data before the reservoir is
	// trustworthy. The 64-byte margin is container/mpa's.
	mp3ReservoirBytes = 511 + 64
	// mp3BackoffFrames bounds the walk. At the smallest compliant frame (24
	// bytes, 8 kbit/s at 24 kHz) the main data left after the overhead is
	// about 9 bytes, so 64 frames is what the reservoir's reach can actually
	// need; the cap is twice that, and it is what stops a crafted table of
	// one-byte samples making every seek walk to the top of the file. Hitting
	// it is not a failure: the decoder answers an unsatisfiable reservoir
	// reference with silence and recovers as the frames refill it.
	mp3BackoffFrames = 128
)

// mp3FrameOverhead is what a frame spends ahead of its main data: a 4-byte
// header, the CRC when present, and the side info.
//
// It is computed from the track rather than fixed, and the fixed version was
// wrong in a way that mattered: MPEG-1 stereo's 32-byte side info is the
// largest of four (mono is 17, and MPEG-2/2.5 is 9 or 17), so charging every
// frame 38 bytes credits nothing at all to any stream whose frames are
// smaller than that, and mp3Backoff then walks to the start of the file. An 8
// kbit/s frame is 24 bytes.
//
// The CRC is assumed present because the sample entry does not say, and
// overstating the overhead undercounts each frame's contribution, which backs
// off further: the safe direction. Only the MPEG-1-or-not distinction reaches
// SideInfoLen, and Layer III defines MPEG-1 at 32 kHz and above alone.
func mp3FrameOverhead(f audio.Format) int64 {
	h := mp3.Header{Channels: f.Channels, Version: mp3.MPEG2}
	if f.Rate >= 32000 {
		h.Version = mp3.MPEG1
	}
	return int64(mp3.HeaderLen + 2 + h.SideInfoLen())
}

// mp3Backoff walks back from idx until the frames in between carry enough
// main data to satisfy any bit-reservoir reference at idx itself.
//
// The caller has already backed the target off by mp3SeekPreroll, so this
// covers the reservoir alone: the frames it skips are the ones that refill it
// before the filterbank frames begin.
func (s *sampleTable) mp3Backoff(idx, overhead int64) int64 {
	land := idx
	cover := int64(0)
	for n := 0; land > 0 && cover < mp3ReservoirBytes && n < mp3BackoffFrames; n++ {
		land--
		cover += max(int64(s.sizes[land])-overhead, 0)
	}
	return land
}

// stscEntry is one sample-to-chunk run.
type stscEntry struct {
	first int64 // first chunk (1-based) this run applies to
	spc   int64 // samples per chunk
}

// sttsEntry is one time-to-sample run in media ticks.
type sttsEntry struct {
	count int64
	delta int64
}

// parseStbl parses the sample table box into t. stsd sets the codec,
// config, and format; the remaining boxes build the sample map.
func (d *Demuxer) parseStbl(t *track, body []byte, depth int) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	var (
		stts      []sttsEntry
		stsc      []stscEntry
		sizes     []uint32
		constSize uint32
		sampleN   int64
		chunks    []int64
		stss      []int64
		haveStsd  bool
		haveStsz  bool
		parseErr  error
		stsdErr   error
	)
	err := walkBoxes(body, func(typ string, payload []byte) error {
		if stsdErr != nil {
			// The sample description failed, so this track has no codec and
			// is going to be discarded. stsd leads the box order in practice,
			// so stopping here is what keeps a rejected track from allocating
			// a sample-size table for nobody.
			return nil
		}
		switch typ {
		case "stsd":
			haveStsd = true
			// Held back rather than returned: stsd is the box that sets
			// t.codec, so a failure here makes isAudio below false and the
			// error would be dropped whole, leaving selectAudio to report
			// "unknown" where the codec layer had a reason. Handled after the
			// walk so the audio-track rule still applies.
			stsdErr = d.parseStsd(t, payload, depth+1)
			return nil
		case "stts":
			stts, parseErr = parseStts(payload)
		case "stsc":
			stsc, parseErr = parseStsc(payload)
		case "stsz":
			sizes, constSize, sampleN, parseErr = parseStsz(payload, d.size)
			haveStsz = true
		case "stz2":
			sizes, sampleN, parseErr = parseStz2(payload)
			haveStsz = true
		case "stco":
			chunks, parseErr = parseStco(payload, false)
		case "co64":
			chunks, parseErr = parseStco(payload, true)
		case "stss":
			stss, parseErr = parseStss(payload)
		}
		return parseErr
	})
	isAudio := t.handler == "soun" && t.codec != ""
	isText := t.handler == "text" || t.handler == "sbtl"
	if stsdErr != nil {
		// Keep the reason for selectAudio, which reports it when no track at
		// all was selectable.
		t.stsdErr = stsdErr
		if isAudio {
			// Damaged audio we would otherwise decode stays fatal, and this
			// is reachable: every setter in stsd.go assigns t.codec only once
			// it has succeeded, but walkBoxes rejects a malformed box on its
			// own, so a first sample entry that parsed cleanly followed by a
			// truncated second one arrives here with the codec set.
			return stsdErr
		}
		// No codec means no sample map worth building, for a sound track or
		// a text one.
		return nil
	}
	if err != nil {
		// A damaged sample table in the audio track we would decode is fatal,
		// but a broken stco/stsz in a sibling video or text track must not
		// reject an otherwise-decodable file.
		if isAudio {
			return err
		}
		return nil
	}
	if !isAudio && !isText {
		return nil // video and other tracks need no sample map here
	}
	// A fragmented movie's sample tables are empty by design (samples live in
	// moof fragments); the stsd alone gives the codec, config, and format. So
	// require the full table only for a progressive movie.
	if d.fragmented {
		return nil
	}
	if !haveStsd || !haveStsz || len(stts) == 0 || len(chunks) == 0 {
		if isAudio {
			return malformed("audio track %d missing a sample table box", t.id)
		}
		return nil // an incomplete text track simply yields no chapters
	}

	// One table per track, whatever the file offers. Nothing orders or limits
	// the stbl boxes inside a minf, so this can run twice, and the two
	// representations do not overwrite each other field for field: a uniform
	// table left standing beside a flat one is read through a chunk index that
	// no longer spans it, which is a packet the walk cannot advance past.
	st := &t.st
	*st = sampleTable{}

	rate := t.timescale
	if t.fmt.Rate > 0 {
		rate = int64(t.fmt.Rate)
	}
	// isAudio is part of the gate, not just of the error handling: a chunk
	// index leaves no per-sample offsets or sizes behind, and readTextChapters
	// reads exactly those. A text track whose sample description happens to
	// carry a PCM fourcc would otherwise open with a table whose two halves
	// disagree, and the chapter walk would index arrays that are not there.
	byteLinear := t.unitBytes > 0 && t.unitDur > 0
	// What one frame lasts on the media clock, when a whole number of ticks
	// can say it. The gate below needs that number rather than the unit's own
	// duration, because the stts is written in the file's ticks and the file
	// need not clock its samples at the sample rate: a 16000-tick clock over
	// 8000 Hz audio writes 2 there, and the two statements agree. A timescale
	// that is not a whole multiple of the rate cannot state a frame at all, so
	// such a file is not a per-frame table and keeps the flat one.
	perFrameTicks := int64(0)
	if rate > 0 && t.timescale > 0 && t.timescale%rate == 0 {
		perFrameTicks = t.timescale / rate
	}
	if isAudio && byteLinear && constSize == uint32(t.unitBytes) &&
		perFrameTicks >= 1 && uniformStts(stts, perFrameTicks) {
		// Byte-linear samples of a constant size, each lasting exactly the
		// one frame it holds: the chunk index describes the whole table. A
		// track excluded from this over its timescale alone would carry a
		// per-frame index instead, and past maxSamples would be refused
		// outright, which is why the clock is converted rather than required
		// to be the rate.
		if err := d.buildChunkIndex(st, t, sampleN, stsc, chunks); err != nil {
			return err
		}
		// The time base comes from the same builder the flat path uses, and
		// the unit's output duration is read back from it rather than assumed:
		// the gate makes that conversion exact (the ticks per frame divide the
		// timescale exactly), so it comes back as the unit's own duration, and
		// reading it is what keeps the packet path honest if that ever stops
		// being true. No sync set, here or below: a byte-linear sample is
		// independently decodable, so every one is a sync point, which is what
		// a nil set means, and an stss listing a subset of PCM frames says
		// nothing true.
		d.buildTimeBase(st, stts, t.timescale, rate)
		if len(st.runDelta) > 0 {
			st.unitDur = st.runDelta[0]
		}
		return nil
	}
	if err := d.flatten(st, sampleN, sizes, constSize, stsc, chunks); err != nil {
		if isAudio {
			return err
		}
		return nil
	}
	d.buildTimeBase(st, stts, t.timescale, rate)
	if !byteLinear {
		buildSync(st, stss)
	}
	if isAudio && byteLinear && !st.rescaled {
		// A byte-linear track states its length twice, in the time-to-sample
		// table and in the bytes its samples occupy, and the two are the same
		// number. A file where they are not has a length that cannot be read
		// off either one, so it is named rather than believed: without this a
		// crafted stts delta turns a tenth of a second of audio into a
		// 27-hour track that --strict accepts. (The chunk-indexed path above
		// cannot reach this: the two agreeing is its gate.)
		//
		// Skipped when the timeline was rescaled, where the two are not the
		// same number by construction: the conversion floors every run, so
		// totalDur is a rounded total and comparing an exact frame count
		// against it would refuse well-formed files.
		var payload int64
		for _, sz := range st.sizes {
			payload += int64(sz)
		}
		if frames := payload / t.unitBytes; frames != st.totalDur {
			if err := d.warn(0, "sample table times %d frames, its samples hold %d", st.totalDur, frames); err != nil {
				return err
			}
		}
	}
	return nil
}

// uniformStts reports whether every time-to-sample run gives its samples the
// duration one unit of this track lasts, in the media clock's own ticks. A
// byte-linear track whose stts says anything else is describing a timeline the
// chunk index cannot express, so it keeps the flattened table: correct either
// way, and the per-sample arrays are what a non-uniform table costs.
func uniformStts(stts []sttsEntry, ticks int64) bool {
	if len(stts) == 0 || ticks < 1 {
		return false
	}
	for _, e := range stts {
		if e.delta != ticks {
			return false
		}
	}
	return true
}

// buildChunkIndex builds the uniform representation: one offset and one
// first-sample index per chunk, plus a sentinel.
//
// It mirrors flatten's bounds, per chunk rather than per sample. A chunk that
// would read past the end of the file keeps the whole units that fit and ends
// the table with the same warning flatten gives, so a truncated source plays
// what it holds. Memory is bounded by the chunk-offset box's own payload,
// never by the sample count, which is the whole reason this path exists.
func (d *Demuxer) buildChunkIndex(st *sampleTable, t *track, sampleN int64, stsc []stscEntry, chunks []int64) error {
	if t.unitBytes < 1 || t.unitDur < 1 {
		// The caller's gate establishes both, and the packet path divides by
		// unitDur. Stated here too because the distance between the two is
		// where a later edit puts a zero.
		return malformed("byte-linear track with a %d-byte unit of %d samples", t.unitBytes, t.unitDur)
	}
	unitBytes := t.unitBytes
	numChunks := int64(len(chunks))
	// Sized by what the table can actually yield, not by what it declares:
	// every chunk kept holds at least one sample, so the sample count bounds
	// the chunk count as tightly as the chunk-offset box does. Capped again
	// beyond that, because both of those are numbers the file chooses and the
	// stsz sample cap no longer stands behind them: append grows to the real
	// count, so a crafted table pays for the chunks it actually has and a real
	// one reserves once.
	capacity := min(numChunks, sampleN, maxReserveChunks)
	offs := make([]int64, 0, capacity)
	firsts := make([]int64, 0, capacity+1)
	idx := int64(0)
	truncated, truncAt := false, int64(0)
	for k := 0; k < len(stsc) && idx < sampleN; k++ {
		first := stsc[k].first
		spc := stsc[k].spc
		if first < 1 || first > numChunks+1 {
			return malformed("stsc first_chunk %d outside 1..%d", first, numChunks+1)
		}
		last := numChunks
		if k+1 < len(stsc) {
			last = stsc[k+1].first - 1
		}
		if last > numChunks {
			last = numChunks
		}
		for c := first; c <= last && idx < sampleN; c++ {
			base := chunks[c-1]
			n := min(spc, sampleN-idx)
			if n == 0 {
				// A chunk holding no samples reads no bytes, so its offset is
				// not checked against anything: flatten never evaluates the
				// bound for one either, and the two builders have to answer
				// the same way about the same table.
				continue
			}
			// base > d.size-bytes, not base+bytes > d.size: a co64 offset
			// near 2^63 would overflow the sum and slip past the guard. The
			// product cannot overflow, since sampleN is capped at what the
			// file could hold at this unit size (parseStsz).
			if base < 0 || base > d.size-n*unitBytes {
				if base >= 0 && base < d.size {
					if fit := (d.size - base) / unitBytes; fit > 0 {
						offs = append(offs, base)
						firsts = append(firsts, idx)
						idx += fit
					}
				}
				truncated, truncAt = true, base
				goto done
			}
			offs = append(offs, base)
			firsts = append(firsts, idx)
			idx += n
		}
	}
done:
	if err := d.warnShortTable(truncated, truncAt, idx, sampleN); err != nil {
		return err
	}
	st.uniform = true
	st.unitBytes = unitBytes
	st.unitDur = t.unitDur
	st.chunkOff = offs
	st.chunkFirst = append(firsts, idx)
	st.total = idx
	return nil
}

// chunkOf returns the index of the chunk holding sample idx, in a uniform
// table.
func (st *sampleTable) chunkOf(idx int64) int {
	if len(st.chunkOff) == 0 {
		return 0 // an empty table; total is 0 and no caller reads a chunk
	}
	// chunkFirst ends with the sentinel, so the search runs over the chunks
	// themselves and the answer is the last one starting at or before idx.
	k := sort.Search(len(st.chunkFirst), func(j int) bool { return st.chunkFirst[j] > idx }) - 1
	return min(max(k, 0), len(st.chunkOff)-1)
}

// flatten builds the per-sample offset and size arrays from the
// sample-to-chunk map, bounded at every step. A sample that would read
// past the end of the file truncates the table with a warning rather than
// letting playback over-read a damaged or truncated source.
func (d *Demuxer) flatten(st *sampleTable, sampleN int64, sizes []uint32, constSize uint32, stsc []stscEntry, chunks []int64) error {
	if sampleN > maxSamples {
		return malformed("%d samples exceed the %d cap", sampleN, int64(maxSamples))
	}
	if constSize == 0 && int64(len(sizes)) < sampleN {
		return malformed("sample size table has %d entries for %d samples", len(sizes), sampleN)
	}
	sizeAt := func(i int64) uint32 {
		if constSize != 0 {
			return constSize
		}
		return sizes[i]
	}
	numChunks := int64(len(chunks))
	offsets := make([]int64, 0, sampleN)
	outSizes := make([]uint32, 0, sampleN)
	idx := int64(0)
	truncated, truncAt := false, int64(0)
	for k := 0; k < len(stsc) && idx < sampleN; k++ {
		first := stsc[k].first
		spc := stsc[k].spc
		if first < 1 || first > numChunks+1 {
			return malformed("stsc first_chunk %d outside 1..%d", first, numChunks+1)
		}
		last := numChunks
		if k+1 < len(stsc) {
			last = stsc[k+1].first - 1
		}
		if last > numChunks {
			last = numChunks
		}
		for c := first; c <= last && idx < sampleN; c++ {
			base := chunks[c-1]
			for s := int64(0); s < spc && idx < sampleN; s++ {
				sz := sizeAt(idx)
				// base > d.size-sz, not base+sz > d.size: a co64 offset near
				// 2^63 would overflow the sum and slip past the guard.
				if base < 0 || base > d.size-int64(sz) {
					truncated, truncAt = true, base
					goto done
				}
				offsets = append(offsets, base)
				outSizes = append(outSizes, sz)
				base += int64(sz)
				idx++
			}
		}
	}
done:
	if err := d.warnShortTable(truncated, truncAt, idx, sampleN); err != nil {
		return err
	}
	st.offsets = offsets
	st.sizes = outSizes
	st.total = idx
	return nil
}

// warnShortTable reports a sample map that yielded fewer samples than the
// table declared, in the two ways it can happen: a chunk whose data runs past
// the end of the file, and a sample-to-chunk map that simply does not reach
// them. Shared by both builders so the two cannot drift into saying the same
// thing differently.
//
// truncated is a flag rather than a sentinel offset because a chunk offset can
// legitimately be negative (a co64 entry past 2^63 reads that way), and a
// sentinel of -1 reported that case as the wrong one of the two.
func (d *Demuxer) warnShortTable(truncated bool, at, got, want int64) error {
	switch {
	case truncated:
		return d.warn(at, "sample data runs past end of file, %d of %d samples kept", got, want)
	case got < want:
		return d.warn(0, "sample-to-chunk map yields %d of %d samples", got, want)
	}
	return nil
}

// buildTimeBase converts the stts runs (in media ticks) to output-sample
// runs. When the media timescale equals the codec rate (the audio norm)
// the conversion is exact and free; otherwise each run is rescaled.
func (d *Demuxer) buildTimeBase(st *sampleTable, stts []sttsEntry, timescale, rate int64) {
	rescale := timescale > 0 && rate > 0 && timescale != rate
	if rescale {
		// A note, not damage: a timescale that is not the codec rate is legal
		// and common, and this is what the demuxer did about it. It was the
		// tree's only discarded warn return, because the strict escalation it
		// would otherwise cause was wrong.
		d.note(0, "media timescale %d differs from sample rate %d; timing rescaled", timescale, rate)
	}
	st.rescaled = rescale
	var sample, pts int64
	for _, e := range stts {
		if sample >= st.total {
			break
		}
		count := min(e.count, st.total-sample)
		delta := e.delta
		if rescale {
			delta = rescaleTicks(e.delta, rate, timescale)
		}
		if delta < 1 {
			// Every frame must advance the timeline. A raw stts delta of zero,
			// or a rescale that floored to nothing (a media timescale far above
			// the sample rate), would stall PTS and hand ReadPacket a
			// non-positive duration.
			delta = 1
		}
		st.runStart = append(st.runStart, sample)
		st.runPTS = append(st.runPTS, pts)
		st.runDelta = append(st.runDelta, delta)
		st.runCount = append(st.runCount, count)
		sample += count
		pts += count * delta
	}
	st.totalDur = pts
	// stts may cover fewer samples than the table; extend the last run's
	// cadence over the remainder so every sample has a time.
	if sample < st.total && len(st.runDelta) > 0 {
		delta := st.runDelta[len(st.runDelta)-1]
		st.runStart = append(st.runStart, sample)
		st.runPTS = append(st.runPTS, pts)
		st.runDelta = append(st.runDelta, delta)
		st.runCount = append(st.runCount, st.total-sample)
		st.totalDur = pts + (st.total-sample)*delta
	}
}

// rescaleTicks converts a media-tick sample delta to output samples as
// delta*rate/timescale, evaluated in 128 bits so a crafted rate or timescale
// cannot overflow the multiply. The quotient is capped at the 32-bit tick
// domain: a rate far above the timescale could otherwise yield a single delta
// large enough to overflow count*delta during PTS accumulation. The caller
// floors the returned value at one output sample.
func rescaleTicks(delta, rate, timescale int64) int64 {
	hi, lo := bits.Mul64(uint64(delta), uint64(rate))
	if hi >= uint64(timescale) {
		return math.MaxUint32 // quotient would exceed 64 bits: degenerate ratio
	}
	q, _ := bits.Div64(hi, lo, uint64(timescale))
	if q > math.MaxUint32 {
		return math.MaxUint32
	}
	return int64(q)
}

// buildSync stores the sync set from an stss table (1-based sample numbers)
// as sorted 0-based indices; an absent or empty stss means all-sync.
func buildSync(st *sampleTable, stss []int64) {
	if len(stss) == 0 {
		return // all samples are sync points
	}
	sync := make([]int64, 0, len(stss))
	for _, n := range stss {
		if n >= 1 && n-1 < st.total {
			sync = append(sync, n-1)
		}
	}
	if len(sync) == 0 {
		return // no in-range entries: fall back to all-sync (nil), not no-sync
	}
	// A malformed stss may repeat sample numbers; sort then drop the
	// duplicates so isSync and syncAtOrBefore search a minimal set.
	slices.Sort(sync)
	st.sync = slices.Compact(sync)
}

// timeOf returns sample i's output position and duration.
func (st *sampleTable) timeOf(i int64) (pts, dur int64) {
	k := sort.Search(len(st.runStart), func(j int) bool { return st.runStart[j] > i }) - 1
	if k < 0 {
		return 0, 0
	}
	return st.runPTS[k] + (i-st.runStart[k])*st.runDelta[k], st.runDelta[k]
}

// sampleAt returns the index of the sample whose span contains output
// position pts, clamped to the last sample for past-the-end targets.
func (st *sampleTable) sampleAt(pts int64) int64 {
	if pts <= 0 || len(st.runPTS) == 0 {
		return 0
	}
	k := sort.Search(len(st.runPTS), func(j int) bool { return st.runPTS[j] > pts }) - 1
	if k < 0 {
		return 0
	}
	if st.runDelta[k] <= 0 {
		return st.runStart[k]
	}
	idx := st.runStart[k] + (pts-st.runPTS[k])/st.runDelta[k]
	if idx >= st.total {
		idx = st.total - 1
	}
	return idx
}

// syncAtOrBefore returns the greatest sync sample index at or before i.
func (st *sampleTable) syncAtOrBefore(i int64) int64 {
	if st.sync == nil {
		return i // every sample is a sync point
	}
	k := sort.Search(len(st.sync), func(j int) bool { return st.sync[j] > i }) - 1
	if k < 0 {
		if len(st.sync) > 0 {
			return st.sync[0] // no sync at or before: earliest available
		}
		return 0
	}
	return st.sync[k]
}

// isSync reports whether sample i is a sync point.
func (st *sampleTable) isSync(i int64) bool {
	if st.sync == nil {
		return true
	}
	k := sort.Search(len(st.sync), func(j int) bool { return st.sync[j] >= i })
	return k < len(st.sync) && st.sync[k] == i
}

// parseStts reads a time-to-sample box.
func parseStts(payload []byte) ([]sttsEntry, error) {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return nil, malformed("stts truncated")
	}
	count := int64(be32(rest))
	rest = rest[4:]
	if count > int64(len(rest))/8 {
		return nil, malformed("stts declares %d entries for %d bytes", count, len(rest))
	}
	out := make([]sttsEntry, count)
	for i := range out {
		out[i] = sttsEntry{count: int64(be32(rest[i*8:])), delta: int64(be32(rest[i*8+4:]))}
	}
	return out, nil
}

// parseStsc reads a sample-to-chunk box, keeping first_chunk monotonic.
func parseStsc(payload []byte) ([]stscEntry, error) {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return nil, malformed("stsc truncated")
	}
	count := int64(be32(rest))
	rest = rest[4:]
	if count > int64(len(rest))/12 {
		return nil, malformed("stsc declares %d entries for %d bytes", count, len(rest))
	}
	out := make([]stscEntry, count)
	prev := int64(0)
	for i := range out {
		first := int64(be32(rest[i*12:]))
		if first <= prev {
			return nil, malformed("stsc first_chunk %d not increasing", first)
		}
		prev = first
		out[i] = stscEntry{first: first, spc: int64(be32(rest[i*12+4:]))}
	}
	return out, nil
}

// parseStsz reads a sample-size box: a constant size, or a per-sample
// table. sampleCount is capped against the file size so a crafted constant
// size cannot force a huge allocation.
func parseStsz(payload []byte, fileSize int64) (sizes []uint32, constSize uint32, count int64, err error) {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 8 {
		return nil, 0, 0, malformed("stsz truncated")
	}
	constSize = be32(rest)
	count = int64(be32(rest[4:]))
	rest = rest[8:]
	if constSize != 0 {
		// Every sample is constSize bytes; the count cannot exceed what the
		// file could hold plus slack.
		if maxN := fileSize/int64(constSize) + 1; count > maxN {
			count = maxN
		}
		// No maxSamples refusal here, unlike the per-sample table below: a
		// constant size is exactly the shape the chunk index reads without
		// per-sample memory, and a PCM sample is one frame, so an hour of
		// 48 kHz audio legitimately declares 173 million of them. The clamp
		// above is what bounds this against a crafted count, and flatten
		// keeps its own cap for the tracks that still need a flat table.
		return nil, constSize, count, nil
	}
	if count > int64(len(rest))/4 {
		return nil, 0, 0, malformed("stsz declares %d samples for %d bytes", count, len(rest))
	}
	sizes = make([]uint32, count)
	for i := range sizes {
		sizes[i] = be32(rest[i*4:])
	}
	return sizes, 0, count, nil
}

// parseStz2 reads a compact sample-size box (4-, 8-, or 16-bit fields).
func parseStz2(payload []byte) (sizes []uint32, count int64, err error) {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 8 {
		return nil, 0, malformed("stz2 truncated")
	}
	fieldSize := int(rest[3])
	count = int64(be32(rest[4:]))
	rest = rest[8:]
	if count > maxSamples {
		// Cap before allocating: the per-field byte bounds still allow a
		// ~64 MB moov to size a half-gigabyte slice without this backstop.
		return nil, 0, malformed("stz2 declares %d samples", count)
	}
	switch fieldSize {
	case 4:
		if count > int64(len(rest))*2 {
			return nil, 0, malformed("stz2 declares %d 4-bit samples for %d bytes", count, len(rest))
		}
		sizes = make([]uint32, count)
		for i := range sizes {
			b := rest[i/2]
			if i%2 == 0 {
				sizes[i] = uint32(b >> 4)
			} else {
				sizes[i] = uint32(b & 0xF)
			}
		}
	case 8:
		if count > int64(len(rest)) {
			return nil, 0, malformed("stz2 declares %d 8-bit samples for %d bytes", count, len(rest))
		}
		sizes = make([]uint32, count)
		for i := range sizes {
			sizes[i] = uint32(rest[i])
		}
	case 16:
		if count > int64(len(rest))/2 {
			return nil, 0, malformed("stz2 declares %d 16-bit samples for %d bytes", count, len(rest))
		}
		sizes = make([]uint32, count)
		for i := range sizes {
			sizes[i] = uint32(be16(rest[i*2:]))
		}
	default:
		return nil, 0, malformed("stz2 field size %d", fieldSize)
	}
	return sizes, count, nil
}

// parseStco reads a chunk-offset box (32- or 64-bit).
func parseStco(payload []byte, wide bool) ([]int64, error) {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return nil, malformed("chunk offset box truncated")
	}
	count := int64(be32(rest))
	rest = rest[4:]
	width := int64(4)
	if wide {
		width = 8
	}
	if count > int64(len(rest))/width {
		return nil, malformed("chunk offset box declares %d entries for %d bytes", count, len(rest))
	}
	out := make([]int64, count)
	for i := range out {
		if wide {
			out[i] = int64(be64(rest[i*8:]))
		} else {
			out[i] = int64(be32(rest[i*4:]))
		}
	}
	return out, nil
}

// parseStss reads a sync-sample box.
func parseStss(payload []byte) ([]int64, error) {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return nil, malformed("stss truncated")
	}
	count := int64(be32(rest))
	rest = rest[4:]
	if count > int64(len(rest))/4 {
		return nil, malformed("stss declares %d entries for %d bytes", count, len(rest))
	}
	out := make([]int64, count)
	for i := range out {
		out[i] = int64(be32(rest[i*4:]))
	}
	return out, nil
}
