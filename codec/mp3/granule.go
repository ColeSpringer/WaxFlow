package mp3

// Granule reconstruction: scalefactors, Huffman spectrum, requantization,
// short block reordering, stereo processing, and alias reduction. All
// band walks are driven by the width tables in tables.go, which is what
// keeps the MPEG-2/2.5 shapes (different mixed block splits, different
// region geometry at low rates) on the same code path as MPEG-1.

// istIllegal marks a scalefactor read whose value was the field maximum
// in the low-sampling-frequency intensity layout: that position is
// "illegal" and the band falls back to mid/side (or nothing).
const istIllegal = 255

// granule is the per-granule working state for one channel pair.
type granule struct {
	// raw is the Huffman-decoded integer spectrum.
	raw [2][576]int32
	// spec is the requantized (then stereo-processed, reordered,
	// alias-reduced) spectrum.
	spec [2][576]float32
	// iscf holds the folded integer scalefactors per width entry
	// (subblock gain and preamp already applied).
	iscf [2][40]int32
	// istPos holds the raw scalefactor values for intensity positions.
	istPos [2][40]uint8
}

// readScalefactors decodes one granule-channel's scalefactors into
// g.iscf[ch] and g.istPos[ch]. scfsi, when non-nil (MPEG-1 granule 1
// with long blocks on both granules), names the groups copied from
// granule 0's raw values in gr0Ist instead of being read.
func readScalefactors(r *bitReader, h Header, gi *grInfo, b bands, g *granule, ch int, scfsi *[4]bool, gr0Ist *[2][40]uint8) {
	var sizes [4]int
	counts := scfPartitions[partitionIndex(b)][:]
	istOn := false

	if h.Version == MPEG1 {
		packed := scfcDecode[gi.scfCompress]
		sizes[0] = int(packed >> 2)
		sizes[1] = sizes[0]
		sizes[2] = int(packed & 3)
		sizes[3] = sizes[2]
	} else {
		// The 9-bit scalefac_compress decodes through mixed-radix moduli;
		// which radix row applies depends on the value's range, and the
		// matching partition group rides along (ISO 13818-3
		// section 2.4.3.2). The right channel under intensity stereo uses
		// its own rows and halves the field first.
		istOn = h.Mode == ModeJoint && h.ModeExt&1 != 0 && ch == 1
		sfc := gi.scfCompress
		k := 0
		if istOn {
			sfc >>= 1
			k = 12
		}
		for ; sfc >= 0; k += 4 {
			prod := 1
			for i := 3; i >= 0; i-- {
				m := int(scfMod[k+i])
				sizes[i] = sfc / prod % m
				prod *= m
			}
			sfc -= prod
		}
		counts = counts[k:]
	}

	// Read the four partitions; a set scfsi bit copies the group from
	// granule 0 instead. The copy sources istPos, which always holds the
	// raw transmitted values (the gain folds below never touch it), so
	// granule 1 shares raw scalefactors and applies its own preflag
	// exactly once; copying the folded iscf would double the pretab, or
	// inherit granule 0's when granule 1's preflag is off.
	band := 0
	for p := 0; p < 4 && counts[p] != 0; p++ {
		cnt := int(counts[p])
		if scfsi != nil && scfsi[p] {
			for i := 0; i < cnt; i++ {
				g.iscf[ch][band+i] = int32(gr0Ist[ch][band+i])
			}
			copy(g.istPos[ch][band:band+cnt], gr0Ist[ch][band:band+cnt])
			band += cnt
			continue
		}
		bits := uint(sizes[p])
		if bits == 0 {
			for i := 0; i < cnt; i++ {
				g.iscf[ch][band] = 0
				g.istPos[ch][band] = 0
				band++
			}
			continue
		}
		maxVal := uint32(1)<<bits - 1
		for i := 0; i < cnt; i++ {
			v := r.bits(bits)
			g.iscf[ch][band] = int32(v)
			if istOn && v == maxVal {
				g.istPos[ch][band] = istIllegal
			} else {
				g.istPos[ch][band] = uint8(v)
			}
			band++
		}
	}
	// The never-transmitted trailing bands (long band 21, the last short
	// triplet, and any entries past what the partitions cover) read as
	// zero; the arrays are reused across granules, so the fill runs to
	// the end rather than trusting the shape to line up.
	for i := band; i < len(g.iscf[ch]); i++ {
		g.iscf[ch][i] = 0
		g.istPos[ch][i] = 0
	}

	// Fold subblock gain into the short entries and the pretab
	// amplification into the long tail, so requantization sees one
	// integer per width entry.
	if b.nShort > 0 {
		sh := 2 - gi.scfScale // subblock gain is 8 quarter steps after scaling
		for i := b.nLong; i+2 < b.nLong+b.nShort; i += 3 {
			g.iscf[ch][i+0] += int32(gi.subblockGain[0] << sh)
			g.iscf[ch][i+1] += int32(gi.subblockGain[1] << sh)
			g.iscf[ch][i+2] += int32(gi.subblockGain[2] << sh)
		}
	} else if gi.preflag {
		for i, p := range preamp {
			g.iscf[ch][11+i] += int32(p)
		}
	}
}

// partitionIndex selects the scfPartitions row for a band shape: long
// only, mixed, or short only.
func partitionIndex(b bands) int {
	idx := 0
	if b.nShort > 0 {
		idx++
	}
	if b.nLong == 0 {
		idx++
	}
	return idx
}

// readSpectrum Huffman-decodes one granule-channel's spectrum into
// g.raw[ch], bounded by part23End (an absolute bit position). A corrupt
// walk zeroes the granule: for a lossy stream the honest damage unit is
// silence, never a hard stop. The reader always lands on part23End so
// the next granule stays aligned.
func readSpectrum(r *bitReader, gi *grInfo, b bands, g *granule, ch int, part23End int) {
	raw := &g.raw[ch]

	// Region boundaries in spectral lines, counted in width entries of
	// the granule's own band table (the window-switching defaults in
	// grInfo.regionCount count the same way).
	r1Entries := gi.regionCount[0] + 1
	r2Entries := r1Entries + gi.regionCount[1] + 1
	region1, region2 := 576, 576
	sum, entry := 0, 0
	for _, w := range b.widths {
		if w == 0 {
			break
		}
		sum += int(w)
		entry++
		if entry == r1Entries {
			region1 = min(sum, 576)
		}
		if entry == r2Entries {
			region2 = min(sum, 576)
		}
	}

	// Big values: pairs across the three table regions.
	n := gi.bigValues * 2
	table := gi.tableSelect[0]
	for i := 0; i < n; i += 2 {
		if i >= region1 {
			table = gi.tableSelect[1]
			if i >= region2 {
				table = gi.tableSelect[2]
			}
		}
		raw[i], raw[i+1] = decodePair(r, table)
	}
	if r.err || r.bitPos() > part23End {
		for i := range raw {
			raw[i] = 0
		}
		r.setPos(part23End)
		return
	}

	// count1: quadruples until the granule's bit budget runs out. A quad
	// that crosses the boundary was stuffing, not data: unwind it.
	i := n
	for i <= 572 && r.bitPos() < part23End && !r.err {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = decodeQuad(r, gi.count1Table)
		i += 4
	}
	if r.bitPos() > part23End {
		i = max(i-4, n)
	}
	for j := i; j < 576; j++ {
		raw[j] = 0
	}
	r.setPos(part23End)
}

// requantize turns g.raw[ch] into g.spec[ch] (ISO 11172-3
// section 2.4.3.4.7.1): value^(4/3) scaled by 2^(q/4) where q combines
// the global gain, the folded scalefactor, and the mid/side fold (the
// sqrt(1/2) halves live here so the stereo butterfly stays a plain sum,
// compensated inside intensity bands).
func requantize(gi *grInfo, b bands, g *granule, ch int, msActive bool) {
	base := gi.globalGain - 210
	if msActive {
		base -= 2
	}
	shift := uint(gi.scfScale) + 1

	raw := &g.raw[ch]
	spec := &g.spec[ch]
	pos := 0
	for band := 0; pos < 576; band++ {
		w := int(b.widths[band])
		if w == 0 { // ragged mixed rows undershoot 576; the tail is zero
			break
		}
		q := max(base-int(g.iscf[ch][band]<<shift), -pow2qBias)
		mult := pow2q[q+pow2qBias]
		for end := min(pos+w, 576); pos < end; pos++ {
			switch v := raw[pos]; {
			case v == 0:
				spec[pos] = 0
			case v > 0:
				spec[pos] = float32(mult * pow43[v])
			default:
				spec[pos] = float32(-mult * pow43[-v])
			}
		}
	}
	for ; pos < 576; pos++ {
		spec[pos] = 0
	}
}

// reorder rearranges the short block region from three sequential
// windows per band to line-interleaved order, in place via scratch. base
// is the first short spectral line (0 for pure short blocks, the long
// region's end for mixed).
func reorder(b bands, g *granule, ch int, base int, scratch *[576]float32) {
	spec := g.spec[ch][:]
	pos := base
	out := 0
	for e := b.nLong; ; e += 3 {
		w := int(b.widths[e])
		if w == 0 || pos+3*w > 576 {
			break
		}
		for i := 0; i < w; i++ {
			scratch[out+3*i+0] = spec[pos+0*w+i]
			scratch[out+3*i+1] = spec[pos+1*w+i]
			scratch[out+3*i+2] = spec[pos+2*w+i]
		}
		pos += 3 * w
		out += 3 * w
	}
	copy(spec[base:base+out], scratch[:out])
}

// stereo applies joint stereo decoding to the granule pair (ISO 11172-3
// section 2.4.3.4.9, 13818-3 section 2.4.3.2 for the low-sampling-
// frequency intensity variant). It runs on the requantized, un-reordered
// spectrum, walking width entries. giR is the right channel's granule
// info (its scalefac_compress low bit selects the intensity scale in the
// LSF layout).
func stereo(h Header, b bands, g *granule, giR *grInfo) {
	ms := h.ModeExt&2 != 0
	intensity := h.ModeExt&1 != 0
	if !intensity {
		if ms {
			midsideBand(g, 0, 576)
		}
		return
	}

	// Intensity stereo applies to bands entirely above the right
	// channel's last nonzero line, tracked per short window.
	var maxBand [3]int
	maxBand[0], maxBand[1], maxBand[2] = -1, -1, -1
	pos := 0
	nBands := 0
	for e := 0; ; e++ {
		w := int(b.widths[e])
		if w == 0 || pos+w > 576 {
			break
		}
		nBands++
		for i := pos; i < pos+w; i++ {
			if g.spec[1][i] != 0 {
				maxBand[e%3] = e
				break
			}
		}
		pos += w
	}
	if b.nLong > 0 {
		m := max(maxBand[0], maxBand[1], maxBand[2])
		maxBand[0], maxBand[1], maxBand[2] = m, m, m
	}
	// The last transmitted band's position extends to the never-
	// transmitted tail bands (one long band, or one triplet).
	nBlocks := 1
	if b.nShort > 0 {
		nBlocks = 3
	}
	for i := 0; i < nBlocks; i++ {
		itop := nBands - nBlocks + i
		prev := itop - nBlocks
		if prev < 0 || itop < 0 {
			continue
		}
		defaultPos := uint8(0)
		if h.Version == MPEG1 {
			defaultPos = 3 // the equal-power position
		}
		if maxBand[i] >= prev {
			g.istPos[1][itop] = defaultPos
		} else {
			g.istPos[1][itop] = g.istPos[1][prev]
		}
	}

	maxPos := uint8(7)
	if h.Version != MPEG1 {
		maxPos = 64
	}
	scale := float32(1)
	if ms {
		scale = 1.41421356 // undo the requantizer's mid/side fold in intensity bands
	}
	lsfShift := giR.scfCompress & 1
	pos = 0
	for e := 0; e < nBands; e++ {
		w := int(b.widths[e])
		ipos := g.istPos[1][e]
		if e > maxBand[e%3] && ipos < maxPos {
			var kl, kr float32
			if h.Version == MPEG1 {
				kl, kr = panPos[ipos][0], panPos[ipos][1]
			} else {
				k := float32(pow2q[pow2qBias-int(ipos+1)>>1<<lsfShift])
				kl, kr = 1, k
				if ipos&1 != 0 {
					kl, kr = k, 1
				}
			}
			intensityBand(g, pos, w, kl*scale, kr*scale)
		} else if ms {
			midsideBand(g, pos, w)
		}
		pos += w
	}
}

// midsideBand reconstructs left/right from mid/side over [pos, pos+n).
// The 1/sqrt(2) normalization was already folded in by the requantizer.
func midsideBand(g *granule, pos, n int) {
	l := g.spec[0][pos : pos+n]
	r := g.spec[1][pos : pos+n]
	for i := range l {
		a, b := l[i], r[i]
		l[i], r[i] = a+b, a-b
	}
}

// intensityBand projects the left channel onto both channels with the
// band's pan factors.
func intensityBand(g *granule, pos, n int, kl, kr float32) {
	l := g.spec[0][pos : pos+n]
	r := g.spec[1][pos : pos+n]
	for i := range l {
		v := l[i]
		l[i], r[i] = v*kl, v*kr
	}
}

// antialias runs the alias reduction butterflies across subband
// boundaries (ISO 11172-3 section 2.4.3.4.10.1). boundaries is the
// number of subband edges to process: 31 for long granules, fewer for
// the long prefix of mixed blocks, none for pure short.
func antialias(g *granule, ch int, boundaries int) {
	spec := &g.spec[ch]
	for sb := 1; sb <= boundaries; sb++ {
		edge := 18 * sb
		for i := 0; i < 8; i++ {
			lo, hi := edge-1-i, edge+i
			a, b := float64(spec[lo]), float64(spec[hi])
			spec[lo] = float32(a*csTab[i] - b*caTab[i])
			spec[hi] = float32(b*csTab[i] + a*caTab[i])
		}
	}
}
