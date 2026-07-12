package mp4

import (
	"math"
	"math/bits"
	"slices"
	"sort"
)

// sampleTable is a track's flattened sample map: per-sample file offset and
// byte size, a run-encoded time base in output samples, and the sync set.
type sampleTable struct {
	offsets []int64  // per-sample file offset
	sizes   []uint32 // per-sample byte size
	total   int64    // sample count (== len(offsets))

	// Time base in output samples (rescaled from mdhd ticks to the codec
	// rate), run-encoded so uniform audio costs a handful of entries.
	runStart []int64 // sample index at each run's start
	runPTS   []int64 // output-sample position at each run's start
	runDelta []int64 // per-sample output duration within the run
	runCount []int64 // samples in the run
	totalDur int64   // total output samples across all runs (raw timeline)

	// sync holds the 0-based sync sample indices in ascending order; nil
	// means every sample is a sync point (the audio norm).
	sync []int64
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
	)
	err := walkBoxes(body, func(typ string, payload []byte) error {
		switch typ {
		case "stsd":
			haveStsd = true
			return d.parseStsd(t, payload, depth+1)
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

	st := &t.st
	if err := d.flatten(st, sampleN, sizes, constSize, stsc, chunks); err != nil {
		if isAudio {
			return err
		}
		return nil
	}
	rate := t.timescale
	if t.fmt.Rate > 0 {
		rate = int64(t.fmt.Rate)
	}
	d.buildTimeBase(st, stts, t.timescale, rate)
	buildSync(st, stss)
	return nil
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
	truncated := int64(-1)
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
					truncated = base
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
	if truncated >= 0 {
		if err := d.warn(truncated, "sample data runs past end of file, %d of %d samples kept", idx, sampleN); err != nil {
			return err
		}
	} else if idx < sampleN {
		if err := d.warn(0, "sample-to-chunk map yields %d of %d samples", idx, sampleN); err != nil {
			return err
		}
	}
	st.offsets = offsets
	st.sizes = outSizes
	st.total = idx
	return nil
}

// buildTimeBase converts the stts runs (in media ticks) to output-sample
// runs. When the media timescale equals the codec rate (the audio norm)
// the conversion is exact and free; otherwise each run is rescaled.
func (d *Demuxer) buildTimeBase(st *sampleTable, stts []sttsEntry, timescale, rate int64) {
	rescale := timescale > 0 && rate > 0 && timescale != rate
	if rescale {
		_ = d.warn(0, "media timescale %d differs from sample rate %d; timing rescaled", timescale, rate)
	}
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
		if count > maxSamples {
			return nil, 0, 0, malformed("stsz declares %d samples", count)
		}
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
