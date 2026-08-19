package aac

import "math"

// SBR parameter extraction: the encoder half of the SBR tool. The encoder
// analyzes the full-rate input in the decoder's 64-band QMF domain,
// decides each frame's envelope time grid (FIXFIX for steady content,
// FIXVAR with a border snapped to a detected transient), measures
// envelope energies and per-band LPC tonality, derives the noise floors
// and inverse-filtering levels from the tonality of the original high
// band against what the transposer's patch will carry up, and quantizes
// into the decoder's coded domain.
//
// Timing: access unit m carries core content of input block m-1, and the
// decoder maps the frame's envelope timeline so that envelope border t
// (time slots, 2 QMF slots each) shapes output slot 2t, which holds input
// QMF slot 32*(m-1) + 2t - 6. All extraction windows below are anchored
// on that abs-slot mapping, which the delay pin verifies end to end.
//
// Grids only ever end at border 16 (FIXFIX by construction, FIXVAR with
// absBord fixed at 16), so consecutive frames tile the envelope timeline
// with no cross-frame border state; VARFIX and VARVAR are never emitted.
// FIXVAR interior borders are even (relative widths come in steps of two
// time slots), so a transient snaps down to the even border before it.

// sbrEncRing is the analysis slot ring length: frame m reads back to abs
// slot 32(m-1)-24 (transient trailing window) while the input feed runs
// ahead to about 32(m+2) (the core's deferred window decision holds each
// AU one block past its window), a span of ~120 slots. The readers'
// ringGuard turns a span overrun into a panic instead of a silent
// wrong-slot read, so the headroom here is margin, not the only line of
// defense.
const sbrEncRing = 192

// ringGuard panics when a read at abs would land on a slot the feed has
// already overwritten. The live span between the newest pushed slot and
// the oldest read is a structural invariant (the ring constants size it
// from the feed chop and the deferred pull), and breaking it must fail
// on the frame that did rather than read a newer slot as history; the
// only symptom otherwise is a byte diff in a chunk-invariance test.
func ringGuard(pushed, abs int64, ring int) {
	if pushed-abs > int64(ring) {
		panic("aac: analysis slot ring span exceeded (slot overwritten before read)")
	}
}

// sbrEnc is the SBR side of one HE-AAC encoder element (SCE or CPE).
type sbrEnc struct {
	chans int

	hdr sbrHeader
	tbl sbrFreqTables

	// The analysis rings are allocated per channel (newSBREncFrom): a
	// mono element, which is every v2 encoder, would otherwise carry a
	// dead 65 KB second channel.
	ana    [2]qmfAnalyzer64
	ringRe [2]*[sbrEncRing][64]float32
	ringIm [2]*[sbrEncRing][64]float32
	pushed int64 // abs index of the next slot to be analyzed

	anch     [2]sbrEncAnchors
	invfPrev [5]int
	bwState  [5]float64 // mirror of the decoder's smoothed chirp factors
	sineHold [2][64]int // bounded sinusoid hold countdowns
	srcOf    [64]int    // patch source subband per target subband

	// refreshCountdown reaches 0 on frames that force freq-coded envelope
	// and noise starts, which is what makes the 12-AU seek preroll
	// sufficient to rebuild the delta chain. (The sbr_header itself rides
	// EVERY access unit; see buildFrame.)
	refreshCountdown int

	fd      [2]sbrFrameData
	payload bitWriter

	// Snapshots of the delta-time mirrors taken at the top of payloadFor,
	// so a dropped payload can roll the encoder back (see rollbackPayload).
	snapAnch    [2]sbrEncAnchors
	snapRefresh int

	// ps carries the v2 parametric-stereo extractor for a mono element
	// whose ps_data rides the extension stream; nil for v1.
	ps *psEnc
}

// sbrRefreshInterval is the delta-refresh repetition in access units
// (freq-coded envelope and noise starts): about 0.43 s, under the 12-AU
// HESeekPreroll.
const sbrRefreshInterval = 10

// sineHoldFrames bounds the sinusoid hold: how long a band stays marked
// on borderline tonality after its last strong detection.
const sineHoldFrames = 8

// newSBREnc derives the stream-constant header and tables for the rate
// and bitrate. chans is the element's channel count (1 or 2).
func newSBREnc(extRate, chans, bitrate int) (*sbrEnc, error) {
	hdr, tbl, err := chooseSBRHeader(extRate, bitrate, chans)
	if err != nil {
		return nil, err
	}
	return newSBREncFrom(chans, hdr, tbl), nil
}

// newSBREncFrom builds the extractor over already-derived parameters.
func newSBREncFrom(chans int, hdr sbrHeader, tbl sbrFreqTables) *sbrEnc {
	s := &sbrEnc{chans: chans, hdr: hdr, tbl: tbl}
	for c := 0; c < chans; c++ {
		s.ringRe[c] = new([sbrEncRing][64]float32)
		s.ringIm[c] = new([sbrEncRing][64]float32)
	}
	for k := range s.srcOf {
		s.srcOf[k] = patchSource(&s.tbl, k)
	}
	return s
}

// chooseSBRHeader picks bs_start_freq and bs_stop_freq for the rate and
// per-channel bitrate: the crossover rises with the budget (more core
// bits push the coded band up), the stop band reaches for the top of the
// audible range, and every candidate is validated through the decoder's
// own table derivation so encoder and decoder cannot disagree about the
// band structure.
func chooseSBRHeader(extRate, bitrate, chans int) (sbrHeader, sbrFreqTables, error) {
	// The crossover rides the per-channel budget: waveform coding beats
	// parametric replication wherever the core can afford real bits, so
	// the patch takes over exactly where the core starts starving. The
	// line is steep (24 kb/s a channel reaches about 6.3 kHz, 32 kb/s
	// about 9 kHz, the fdk-class operating points): the per-band NMR
	// diagnostic showed a 7.5 kHz crossover at 48 kb/s stereo leaving the
	// core at ~0 dB NMR across 3-7.5 kHz on transients, worse than the
	// parametric band it was holding back.
	perCh := float64(bitrate) / float64(chans)
	xoverHz := perCh*0.3375 - 1800
	xoverHz = math.Min(math.Max(xoverHz, 4000), 10500)
	stopHz := math.Min(0.85*float64(extRate)/2, 16500.0)
	bandHz := float64(extRate) / 128

	hdr := sbrHeader{
		ampRes: 1,
		// Parser defaults; restated here because the encoder's grid and
		// quantization decisions read them from this struct.
		freqScale: 2, alterScale: 1, noiseBands: 2,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	bestStart, bestDiff := -1, math.Inf(1)
	for sf := 0; sf < 16; sf++ {
		k0, ok := sbrStartK0(extRate, sf)
		if !ok || k0 < 1 || k0 > 32 {
			continue
		}
		if d := math.Abs(float64(k0)*bandHz - xoverHz); d < bestDiff {
			bestStart, bestDiff = sf, d
		}
	}
	if bestStart < 0 {
		return hdr, sbrFreqTables{}, malformed("no SBR start-frequency row for %d Hz", extRate)
	}
	hdr.startFreq = bestStart
	k0, _ := sbrStartK0(extRate, bestStart)

	bestStop, bestDiff := -1, math.Inf(1)
	for sf := 0; sf < 14; sf++ { // 14/15 are the k0-relative escapes; the direct rows suffice
		k2 := sbrStopK2(extRate, sf, k0)
		if k2 <= k0 {
			continue
		}
		if d := math.Abs(float64(k2)*bandHz - stopHz); d < bestDiff {
			bestStop, bestDiff = sf, d
		}
	}
	hdr.stopFreq = bestStop
	// The spec bounds the SBR span per rate; walk the stop band down until
	// the derivation accepts (it rejects a too-wide k2-k0 by name).
	for ; hdr.stopFreq >= 0; hdr.stopFreq-- {
		tbl, err := deriveFreqTables(hdr, extRate)
		if err == nil {
			return hdr, tbl, nil
		}
	}
	return hdr, sbrFreqTables{}, malformed("no usable SBR band configuration for %d Hz", extRate)
}

// pushSlot analyzes one channel's next 64 input samples into the ring.
// Channel 0 advances the shared slot counter.
func (s *sbrEnc) pushSlot(ch int, x []float32) {
	idx := s.pushed % sbrEncRing
	s.ana[ch].analyze(x, s.ringRe[ch][idx][:], s.ringIm[ch][idx][:])
	if ch == s.chans-1 {
		s.pushed++
	}
}

// slotEnergy is the summed high-band energy of one abs slot across
// channels, the transient detector's per-slot statistic.
func (s *sbrEnc) slotEnergy(abs int64) float64 {
	if abs < 0 || abs >= s.pushed {
		return 0
	}
	ringGuard(s.pushed, abs, sbrEncRing)
	idx := abs % sbrEncRing
	var e float64
	for c := 0; c < s.chans; c++ {
		re, im := &s.ringRe[c][idx], &s.ringIm[c][idx]
		for k := s.tbl.kx; k < s.tbl.kx+s.tbl.m && k < 64; k++ {
			e += float64(re[k])*float64(re[k]) + float64(im[k])*float64(im[k])
		}
	}
	return e
}

// bandEnergy is the mean per-slot per-subband energy of channel ch over
// abs slots [s0,s1) and subbands [lo,hi). Slots outside the pushed range
// read as silence, which is what frames before the stream head see.
func (s *sbrEnc) bandEnergy(ch int, s0, s1 int64, lo, hi int) float64 {
	if s1 <= s0 || hi <= lo {
		return 0
	}
	var sum float64
	for abs := s0; abs < s1; abs++ {
		if abs < 0 || abs >= s.pushed {
			continue
		}
		ringGuard(s.pushed, abs, sbrEncRing)
		idx := abs % sbrEncRing
		re, im := &s.ringRe[ch][idx], &s.ringIm[ch][idx]
		for k := lo; k < hi && k < 64; k++ {
			sum += float64(re[k])*float64(re[k]) + float64(im[k])*float64(im[k])
		}
	}
	return sum / (float64(s1-s0) * float64(hi-lo))
}

// tonality solves the second-order covariance LPC for one channel's
// subband over abs slots [s0,s1) and returns the prediction gain above
// unity: 0 for noise, large for a stable tone. The solve mirrors the
// decoder's predictCoef so both sides agree about what the transposer
// can whiten.
func (s *sbrEnc) tonality(ch, k int, s0, s1 int64) float64 {
	var p00, p01r, p01i, p02r, p02i, p11, p12r, p12i, p22 float64
	sample := func(abs int64) (float64, float64) {
		if abs < 0 || abs >= s.pushed {
			return 0, 0
		}
		ringGuard(s.pushed, abs, sbrEncRing)
		idx := abs % sbrEncRing
		return float64(s.ringRe[ch][idx][k]), float64(s.ringIm[ch][idx][k])
	}
	for abs := s0; abs < s1; abs++ {
		x0r, x0i := sample(abs)
		x1r, x1i := sample(abs - 1)
		x2r, x2i := sample(abs - 2)
		p00 += x0r*x0r + x0i*x0i
		p01r += x0r*x1r + x0i*x1i
		p01i += x0i*x1r - x0r*x1i
		p02r += x0r*x2r + x0i*x2i
		p02i += x0i*x2r - x0r*x2i
		p11 += x1r*x1r + x1i*x1i
		p12r += x1r*x2r + x1i*x2i
		p12i += x1i*x2r - x1r*x2i
		p22 += x2r*x2r + x2i*x2i
	}
	var a0r, a0i, a1r, a1i float64
	d := p22*p11 - (p12r*p12r+p12i*p12i)/1.000001
	if d != 0 {
		a1r = (p01r*p12r - p01i*p12i - p02r*p11) / d
		a1i = (p01r*p12i + p01i*p12r - p02i*p11) / d
	}
	if p11 != 0 {
		a0r = -(p01r + a1r*p12r + a1i*p12i) / p11
		a0i = -(p01i + a1i*p12r - a1r*p12i) / p11
	}
	if !(a0r*a0r+a0i*a0i < 16) || !(a1r*a1r+a1i*a1i < 16) {
		return 0
	}
	// Orthogonality-principle residual of the optimal predictor.
	err := p00 + a0r*p01r + a0i*p01i + a1r*p02r + a1i*p02i
	if !(err > p00*1e-9) {
		return 1e6
	}
	return math.Min(math.Max(p00/err-1, 0), 1e6)
}

// patchSource maps an absolute high subband to the low subband the
// transposer copies it from.
func patchSource(t *sbrFreqTables, k int) int {
	target := t.kx
	for p := range t.patchNum {
		if k < target+t.patchNum[p] {
			return t.patchStart[p] + (k - target)
		}
		target += t.patchNum[p]
	}
	return max(t.kx-1, 0)
}

// buildGrid decides the frame's time grid from the transient detector.
// s0 is the frame's envelope-timeline origin in abs slots.
func (s *sbrEnc) buildGrid(g *sbrGrid, s0 int64) {
	// Transient scan: a slot whose high-band energy jumps an order of
	// magnitude over the trailing average. The floor keeps silence and
	// fade-ins from triggering on noise.
	const ratio, floor = 8.0, 1e-7
	transient := -1
	trail := 0.0
	for i := -16; i < 0; i++ {
		trail += s.slotEnergy(s0 + int64(i))
	}
	for u := 0; u < 32; u++ {
		e := s.slotEnergy(s0 + int64(u))
		avg := trail / 16
		if e > ratio*avg+floor {
			transient = u
			break
		}
		trail += e - s.slotEnergy(s0+int64(u)-16)
	}

	*g = sbrGrid{}
	if transient < 0 {
		// FIXFIX; two envelopes when the frame halves differ by ~3 dB.
		g.frameClass = fixfix
		e0 := s.slotEnergy16(s0)
		e1 := s.slotEnergy16(s0 + 16)
		g.numEnv = 1
		if e0 > 2*e1+floor || e1 > 2*e0+floor {
			g.numEnv = 2
		}
		for i := 0; i <= g.numEnv; i++ {
			g.tE[i] = i * 16 / g.numEnv
		}
		for i := 0; i < g.numEnv; i++ {
			g.freqRes[i] = 1
		}
		g.lA = -1
	} else {
		// FIXVAR with a border snapped down to the even time slot at the
		// transient, a two-slot attack envelope, and pre/tail regions split
		// to the class's four-envelope, eight-slot-width limits.
		tT := (transient / 2) &^ 1
		g.frameClass = fixvar
		borders := []int{0}
		addSplit := func(from, to int) {
			for to-from > 8 {
				from += 8
				borders = append(borders, from)
			}
			if to > from {
				borders = append(borders, to)
			}
		}
		if tT > 0 {
			addSplit(0, tT)
		}
		attackEnd := min(tT+2, 16)
		borders = append(borders, attackEnd)
		addSplit(attackEnd, 16)
		g.numEnv = len(borders) - 1
		copy(g.tE[:], borders)
		lA := 0
		for i, b := range borders {
			if b == tT {
				lA = i
			}
		}
		// lA rides the FIXVAR pointer, whose field is ceilLog2(numEnv+1)
		// bits: an attack envelope at index 0 needs pointer numEnv+1, which
		// only fits when numEnv+1 is not a power of two. Dropping the
		// signal (lA -1) is legal and costs only the attack's noise gate.
		pointer := g.numEnv + 1 - lA
		if pointer >= 1<<ceilLog2(g.numEnv+1) {
			lA = -1
		}
		g.lA = lA
		// Short envelopes take the low-resolution band set.
		for i := 0; i < g.numEnv; i++ {
			if g.tE[i+1]-g.tE[i] >= 6 {
				g.freqRes[i] = 1
			}
		}
	}

	g.numNoise = 2
	if g.numEnv == 1 {
		g.numNoise = 1
	}
	pointer := 0
	if g.frameClass == fixvar && g.lA >= 0 {
		pointer = g.numEnv + 1 - g.lA
	}
	g.tQ[0] = g.tE[0]
	g.tQ[g.numNoise] = g.tE[g.numEnv]
	if g.numNoise == 2 {
		g.tQ[1] = g.tE[middleBorder(g.frameClass, pointer, g.numEnv)]
	}
}

// slotEnergy16 sums slotEnergy over 16 consecutive slots.
func (s *sbrEnc) slotEnergy16(s0 int64) float64 {
	var e float64
	for i := int64(0); i < 16; i++ {
		e += s.slotEnergy(s0 + i)
	}
	return e
}

// quantEnvValue quantizes a linear band energy into the coded envelope
// domain at the frame's effective amplitude resolution.
func quantEnvValue(e float64, ampRes int) int32 {
	if !(e > 0) {
		return 0
	}
	a := 2.0
	if ampRes == 1 {
		a = 1.0
	}
	v := int32(math.Round(a * (math.Log2(e) + 24)))
	return min(max(v, 0), 127)
}

// clampSeq clamps a quantized value sequence so every step is codable by
// the table and every value stays in [0, limit], walking left to right so
// a clamp propagates. step keeps balance sequences on their even grid.
func clampSeq(v []int32, tab *sbrHuffEncTable, limit, step int32) {
	maxD := int32(tab.max)
	for i := range v {
		lo, hi := int32(0), limit
		if i > 0 {
			lo = max(lo, v[i-1]-maxD)
			hi = min(hi, v[i-1]+maxD)
		}
		x := min(max(v[i], lo), hi)
		if step == 2 {
			x = lo + (x-lo)/2*2
		}
		v[i] = x
	}
}

// clampToRef clamps each value into codable range of its time reference.
// step keeps balance sequences on their even grid explicitly, like
// clampSeq: the balance tables carry only even deltas, so an odd offset
// from the (even) reference would look up a zero-length code and emit
// nothing (the round-trip test's coverage check pins that invariant).
func clampToRef(v []int32, ref []int32, tab *sbrHuffEncTable, limit, step int32) {
	maxD := int32(tab.max)
	for i := range v {
		lo := max(int32(0), ref[i]-maxD)
		hi := min(limit, ref[i]+maxD)
		x := min(max(v[i], lo), hi)
		if step == 2 {
			x = lo + (x-lo)/2*2
		}
		v[i] = x
	}
}

// chooseDTDF picks freq or time coding for one value run: the candidate
// with the smaller clamping distortion, bits breaking the tie. ideal is
// the unclamped target; the chosen, clamped values land back in out.
func chooseDTDF(ideal []int32, refMapped []int32, haveRef bool,
	fTab, tTab *sbrHuffEncTable, startBits uint, mul, limit int32) (timeCoded bool, out []int32) {
	freq := append([]int32(nil), ideal...)
	clampSeq(freq, fTab, limit, mul)
	freqDist, freqBits := int32(0), int(startBits)
	for i := range freq {
		d := freq[i] - ideal[i]
		freqDist += max(d, -d)
		if i > 0 {
			freqBits += int(fTab.bits[freq[i]-freq[i-1]+64])
		}
	}
	if !haveRef {
		return false, freq
	}
	tc := append([]int32(nil), ideal...)
	clampToRef(tc, refMapped, tTab, limit, mul)
	timeDist, timeBits := int32(0), 0
	for i := range tc {
		d := tc[i] - ideal[i]
		timeDist += max(d, -d)
		timeBits += int(tTab.bits[tc[i]-refMapped[i]+64])
	}
	if timeDist < freqDist || (timeDist == freqDist && timeBits < freqBits) {
		return true, tc
	}
	return false, freq
}

// buildFrame extracts and quantizes one frame's SBR data for the element.
// au is the access unit index; the envelope timeline's origin is abs slot
// 32*(au-1) - 6. It returns whether this frame carries the header, which
// is every frame: the header is 17 bits (~0.4 kb/s at 48 kHz), and any
// join point -- a segment boundary, a restarted segment worker whose
// priming AUs are discarded, a mid-stream ADTS tune-in -- decodes its
// first frame's high band instead of muting until the next multiple of
// some interval. The band tables never change mid-stream, so repeats
// cost the decoder nothing (freqFieldsEqual short-circuits its reset).
func (s *sbrEnc) buildFrame(au int64) bool {
	t := &s.tbl
	s0 := 32*(au-1) - 6
	// The delta chain still refreshes on a cadence: freq-coded starts, so
	// a seek preroll spanning one refresh interval rebuilds every anchor.
	refresh := s.refreshCountdown == 0
	if refresh {
		s.refreshCountdown = sbrRefreshInterval
	}
	s.refreshCountdown--

	fd0 := &s.fd[0]
	*fd0 = sbrFrameData{}
	s.buildGrid(&fd0.grid, s0)
	fd0.ampRes = s.hdr.ampRes
	if fd0.grid.frameClass == fixfix && fd0.grid.numEnv == 1 {
		fd0.ampRes = 0
	}
	if s.chans == 2 {
		fd1 := &s.fd[1]
		*fd1 = sbrFrameData{}
		fd1.grid = fd0.grid
		fd1.ampRes = fd0.ampRes
	}
	g := &fd0.grid

	// Whole-frame per-subband energy and tonality, shared by the noise
	// floor, inverse filtering, and sinusoid decisions.
	f0, f1 := s0, s0+32
	var tone [2][64]float64
	var energy [2][64]float64
	for c := 0; c < s.chans; c++ {
		for k := 0; k < t.kx+t.m && k < 64; k++ {
			energy[c][k] = s.bandEnergy(c, f0, f1, k, k+1)
		}
		for k := 0; k < t.kx+t.m && k < 64; k++ {
			tone[c][k] = s.tonality(c, k, f0, f1)
		}
	}
	// Inverse filtering and noise floors per noise band: the per-channel
	// band sums are accumulated once and the combined view derives from
	// the same numbers.
	var noiseLin [2][5]float64
	for q := 0; q < t.nQ; q++ {
		lo, hi := t.fNoise[q], t.fNoise[q+1]
		var eoC, toC, esC, tsC [2]float64
		for c := 0; c < s.chans; c++ {
			for k := lo; k < hi; k++ {
				eoC[c] += energy[c][k]
				toC[c] += energy[c][k] * tone[c][k]
				sk := s.srcOf[k]
				esC[c] += energy[c][sk]
				tsC[c] += energy[c][sk] * tone[c][sk]
			}
		}
		ratio := func(num, den float64) float64 {
			if den > 0 {
				return num / den
			}
			return 0
		}
		to := ratio(toC[0]+toC[1], eoC[0]+eoC[1])
		ts := ratio(tsC[0]+tsC[1], esC[0]+esC[1])
		rho := (ts + 1) / (to + 1)
		target := 0
		switch {
		case rho >= 16:
			target = 3
		case rho >= 5:
			target = 2
		case rho >= 1.8:
			target = 1
		}
		lvl := s.invfPrev[q]
		if target > lvl {
			lvl++
		} else if target < lvl {
			lvl--
		}
		fd0.invf[q] = lvl
		if s.chans == 2 {
			s.fd[1].invf[q] = lvl
		}

		// The chirp factor the decoder applies this frame is not the
		// level's nominal value: hfGenerate takes 0.6 on an off<->low
		// transition and smooths toward the new value across frames.
		// Model the noise floor against that evolving factor (the field's
		// doctrine: what the adjuster does with it decides what to put in
		// it), or a tonal-to-noisy transition over-noises the band for the
		// frames the decoder's smoother takes to catch up.
		nb := sbrNewBw[lvl]
		if lvl+s.invfPrev[q] == 1 {
			nb = 0.6
		}
		s.invfPrev[q] = lvl
		bw := 0.90625*nb + 0.09375*s.bwState[q]
		if nb < s.bwState[q] {
			bw = 0.75*nb + 0.25*s.bwState[q]
		}
		if bw < 0.015625 {
			bw = 0
		}
		if bw > 0.99609375 {
			bw = 0.99609375
		}
		s.bwState[q] = bw

		// Noise floor from the tonality mismatch: the fraction of the
		// original band that is noise, minus what the (inverse-filtered)
		// patch already carries as noise.
		for c := 0; c < s.chans; c++ {
			toCh := ratio(toC[c], eoC[c])
			tPatch := ratio(tsC[c], esC[c]) * (1 - bw*bw)
			nO := 1 / (1 + toCh)
			nP := 1 / (1 + tPatch)
			q1 := math.Max(0, (nO-nP)*(1+toCh)/math.Max(toCh, 1e-6))
			noiseLin[c][q] = math.Min(math.Max(q1, math.Exp2(-24)), 64)
		}
	}

	// Sinusoids: a strongly tonal original band whose patch source cannot
	// supply the tone. The hold is a bounded countdown, not a latch: a
	// strong detection arms it for sineHoldFrames and merely-borderline
	// frames run it down, so it cannot flicker on a hovering tone AND two
	// encoders joining the same content at different points agree within
	// sineHoldFrames (a pure latch never converged: a restarted segment
	// worker disagreed with the continuous encoder for the tone's whole
	// lifetime).
	for c := 0; c < s.chans; c++ {
		for i := 0; i < t.nHigh; i++ {
			lo, hi := t.fHigh[i], t.fHigh[i+1]
			var e, tn float64
			for k := lo; k < hi; k++ {
				e += energy[c][k]
				tn += energy[c][k] * tone[c][k]
			}
			if e > 0 {
				tn /= e
			}
			src := s.srcOf[(lo+hi)/2]
			on := tn >= 30 && tn >= 6*(tone[c][src]+1)
			weak := tn >= 15 && tn >= 3*(tone[c][src]+1)
			switch {
			case on:
				s.sineHold[c][i] = sineHoldFrames
			case weak && s.sineHold[c][i] > 0:
				s.sineHold[c][i]--
			default:
				s.sineHold[c][i] = 0
			}
			s.fd[c].addHarmonic[i] = on || (weak && s.sineHold[c][i] > 0)
		}
	}

	// Envelopes: per envelope, per band of its resolution, the mean
	// energy over the envelope's slots, coupled into level/balance.
	// pan is the decoder's balance centre (dequantElement's 24/12, NOT the
	// coded range's top, which is twice it: writing around the doubled
	// value once shifted every pair's image 12 balance steps right-to-left
	// and silenced channel 1).
	aQ := 2.0
	pan := int32(24)
	if fd0.ampRes == 1 {
		aQ = 1.0
		pan = 12
	}
	for l := 0; l < g.numEnv; l++ {
		bands := t.fLow
		if g.freqRes[l] == 1 {
			bands = t.fHigh
		}
		n := envBands(t, g.freqRes[l])
		e0, e1 := s0+2*int64(g.tE[l]), s0+2*int64(g.tE[l+1])
		for b := 0; b < n; b++ {
			lo, hi := bands[b], bands[b+1]
			eL := s.bandEnergy(0, e0, e1, lo, hi)
			if s.chans == 1 {
				fd0.env[l][b] = quantEnvValue(eL, fd0.ampRes)
				continue
			}
			eR := s.bandEnergy(1, e0, e1, lo, hi)
			sum := eL + eR
			var lvl int32
			if sum > 0 {
				lvl = int32(math.Round(aQ * (math.Log2(sum) + 23)))
			}
			fd0.env[l][b] = min(max(lvl, 0), 127)
			r := math.Min(math.Max(eR/math.Max(eL, 1e-30), math.Exp2(-24)), math.Exp2(24))
			bal := int32(math.Round((float64(pan)-aQ*math.Log2(r))/2)) * 2
			s.fd[1].env[l][b] = min(max(bal, 0), 2*pan)
		}
	}

	// Noise floors: the per-band ratio is scale-free, so both noise
	// envelopes carry the frame's values; delta-time makes the repeat
	// nearly free.
	for c := 0; c < s.chans; c++ {
		for l := 0; l < g.numNoise; l++ {
			for q := 0; q < t.nQ; q++ {
				if s.chans == 1 {
					v := int32(math.Round(6 - math.Log2(noiseLin[c][q])))
					s.fd[c].noise[l][q] = min(max(v, 0), 30)
					continue
				}
				if c == 1 {
					continue
				}
				qL, qR := noiseLin[0][q], noiseLin[1][q]
				v0 := int32(math.Round(7 - math.Log2(qL+qR)))
				s.fd[0].noise[l][q] = min(max(v0, 0), 30)
				r := math.Min(math.Max(qR/math.Max(qL, 1e-30), math.Exp2(-12)), math.Exp2(12))
				v1 := int32(math.Round((12-math.Log2(r))/2)) * 2
				s.fd[1].noise[l][q] = min(max(v1, 0), 24)
			}
		}
	}

	// Delta-coding decisions, walking envelopes in order so each choice
	// anchors on the previous one's final (clamped) values.
	for c := 0; c < s.chans; c++ {
		fd := &s.fd[c]
		balance := s.chans == 2 && c == 1
		tTab, fTab, startBits, mul := sbrEnvTables(fd.ampRes, balance)
		for l := 0; l < g.numEnv; l++ {
			n := envBands(t, g.freqRes[l])
			ideal := fd.env[l][:n]
			var refMapped []int32
			haveRef := !(refresh && l == 0)
			if haveRef {
				ref := &s.anch[c].prevEnv
				refRes := s.anch[c].prevRes
				if l > 0 {
					ref = &fd.env[l-1]
					refRes = g.freqRes[l-1]
				}
				refMapped = make([]int32, n)
				for b := 0; b < n; b++ {
					refMapped[b] = ref[mapPrevBand(t, g.freqRes[l], refRes, b)]
				}
			}
			timeCoded, vals := chooseDTDF(ideal, refMapped, haveRef, fTab, tTab, startBits, mul, 127)
			fd.dfEnv[l] = timeCoded
			copy(fd.env[l][:n], vals)
		}

		ntTab, nfTab, nmul := sbrNoiseTables(balance)
		limit := int32(30)
		if balance {
			limit = 24
		}
		for l := 0; l < g.numNoise; l++ {
			ideal := fd.noise[l][:t.nQ]
			var refMapped []int32
			haveRef := !(refresh && l == 0)
			if haveRef {
				ref := &s.anch[c].prevNoise
				if l > 0 {
					ref = &fd.noise[l-1]
				}
				refMapped = ref[:t.nQ]
			}
			timeCoded, vals := chooseDTDF(ideal, refMapped, haveRef, nfTab, ntTab, 5, nmul, limit)
			fd.dfNoise[l] = timeCoded
			copy(fd.noise[l][:t.nQ], vals)
		}
	}
	// The anchors are NOT advanced here: the serializer owns that
	// (writeSBREnvelope and writeSBRNoise advance them as they write, the
	// exact mirror of the parser), so this phase reads the previous
	// frame's values and cannot double-advance.
	return true
}

// payloadFor renders access unit au's fill-element payload into the
// element's scratch writer and returns it. The caller (the core encoder's
// frame loop) writes it via writeFIL before the END element.
func (s *sbrEnc) payloadFor(au int64) *bitWriter {
	// ps_data is defined on sbr_single_channel_element only; the pair
	// branch of the serializer has no extension writer, so a coupled
	// element with a ps set would ship a legal CPE stream whose AOT-29
	// config promises parametric stereo it never carries. Programmer
	// error, caught here rather than shipped.
	if s.ps != nil && s.chans != 1 {
		panic("aac: parametric stereo requires a mono SBR element")
	}
	s.snapAnch = s.anch
	s.snapRefresh = s.refreshCountdown
	sendHeader := s.buildFrame(au)
	if s.ps != nil {
		s.ps.buildFrame(au)
	}
	s.payload.reset()
	serializeSBRPayload(&s.payload, s.hdr, sendHeader, &s.tbl, s.chans, &s.fd, &s.anch, s.ps)
	return &s.payload
}

// rollbackPayload restores the delta-time mirrors (envelope and noise
// anchors, the refresh countdown) to their state before the last
// payloadFor, the encoder-side twin of parsePayload's rollback: a
// caller that drops the rendered payload (the fill-element size
// ceiling) hands the decoder a concealed frame, and the decoder keeps
// its anchors through concealment, so the encoder must too or every
// delta-time frame until the next refresh decodes against the wrong
// references. ps_data carries no cross-frame state (freq-coded every
// frame) and the signal-side smoothers (invfPrev, bwState, sineHold)
// stay advanced: they mirror smoothed decoder state that reconverges,
// not exact anchors.
func (s *sbrEnc) rollbackPayload() {
	s.anch = s.snapAnch
	s.refreshCountdown = s.snapRefresh
}
