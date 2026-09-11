//go:build !wmavoicetablesgen

package wmavoice

import "math"

// Pitch, gains, pulses, the adaptive codebook and the synthesis filter: notes
// 6 through 9 and 11. Everything on this path is float32, and it is recursive,
// so a rounding difference does not stay local.

// stdCodebookLen is the comfort-noise and hardcoded codebook's length, which
// the comfort-noise generator's modulus is taken against.
const stdCodebookLen = 1000

// The fixed codebook gain predictor (note 7.3), an AMR-style log-domain
// moving average.
var gainCoeff = [6]float32{0.8169, -0.06545, 0.1726, 0.0185, -0.0359, 0.0458}

const (
	gainPredBias = 5.2409161640
	// The predictor error is clipped to log(0.05) and log(5.0) before it
	// enters the history.
	gainPredLo = -2.9957322736
	gainPredHi = 1.6094379124
)

// framePitch reads the one pitch field an asymmetric frame carries and lays
// out the per-block pitches and the per-sample slope (note 6.1).
func (d *Decoder) framePitch(r *bitReader, desc frameDesc) {
	cur := d.g.minPitch + int(r.bits(d.g.pitchBits))
	if cur >= d.g.maxPitch {
		cur = d.g.maxPitch - 1
		d.paths.PitchClamp++
	}

	// A jump of more than about five percent, or the first frame after one
	// with no adaptive codebook, resets the interpolation origin so the frame
	// does not sweep from an unrelated pitch. The reset writes the state, so
	// the interpolation below then runs from cur to cur.
	last := d.pitchState
	jumped := 20*abs(cur-last) > cur+last
	if jumped {
		d.paths.PitchReset++
	}
	if d.prevACB == acbNone || jumped {
		last = cur
	}

	b := desc.blocks
	logB2 := desc.log2 + 1
	for n := range b {
		w := 2*n + 1
		d.pitch[n] = (w*cur + (2*b-w)*last + b) >> logB2
	}
	d.asymBase = last
	d.curPitch = cur
	d.pitchSlope = (cur - last) * 65536 / frameSamples
}

// blockPitch reads one block's pitch field and converts it off the piecewise
// scale it is coded on (note 6.2). The return is a quarter-sample pitch: the
// integer lag is the top and the fractional phase the low two bits.
//
// The four arms of the ladder are quarter-, half- and whole-sample resolution
// and then saturation. DEFICIT: the saturating arm is never reached by any
// cell, though the other three are reached by every cell that uses this
// codebook.
func (d *Decoder) blockPitch(r *bitReader, first bool) int {
	var field int
	if first {
		field = int(r.bits(d.g.blockPitchBits))
	} else {
		field = d.lastPitchField - d.g.deltaPitchHalf + int(r.bits(d.g.deltaPitchBits))
	}
	// The clip applies to the STATE, not to the value used, so a block can
	// decode a pitch outside the clip range while the next block's delta is
	// still taken from inside it.
	d.lastPitchField = clip(field, d.g.deltaPitchHalf, d.g.blockPitchRange-d.g.deltaPitchHalf)

	t1 := (d.g.conv[1] - d.g.conv[0]) << 2
	t2 := (d.g.conv[2] - d.g.conv[1]) << 1
	t3 := d.g.conv[3] - d.g.conv[2] + 1
	if field < t1 {
		d.paths.ConvArm[0]++
		return d.g.conv[0]<<2 + field
	}
	field -= t1
	if field < t2 {
		d.paths.ConvArm[1]++
		return d.g.conv[1]<<2 + field<<1
	}
	field -= t2
	if field < t3 {
		d.paths.ConvArm[2]++
		return (d.g.conv[2] + field) << 2
	}
	d.paths.ConvArm[3]++
	return d.g.conv[3] << 2
}

// blockGains reads the one 7-bit index that carries both of a block's gains
// and pushes the block's contribution into the predictor history (note 7.3).
func (d *Decoder) blockGains(r *bitReader, log2Blocks int) (acb, fcb float32) {
	idx := int(r.bits(7))
	acb = gainACB[idx]

	var pred float32
	for i, c := range gainCoeff {
		pred += d.gainPredErr[i] * c
	}
	fcb = float32(math.Exp(float64(pred) - gainPredBias + float64(gainFCB[idx])))

	// A frame with fewer, longer blocks writes each block's error into more
	// history slots, so the predictor's time constant stays the same in
	// samples rather than in blocks.
	predErr := min(max(gainFCB[idx], gainPredLo), gainPredHi)
	// A weight of 8 would fill the whole history and is unreachable: it needs a
	// one-block frame, and the only one-block frame type is comfort noise,
	// which never gets here. The bound is here because the alternative failure
	// is a slice-bounds panic rather than a wrong sample.
	weight := min(8>>log2Blocks, len(d.gainPredErr))
	copy(d.gainPredErr[weight:], d.gainPredErr[:len(d.gainPredErr)-weight])
	for i := range weight {
		d.gainPredErr[i] = predErr
	}
	return acb, fcb
}

// innovationPulses reads the five-plus-five pulse set of note 8.1 into pulses,
// which the caller has already zeroed.
//
// A SET sign bit is +1 here. In both pitch-adaptive window schemes it is -1.
// The two are opposite and it is the easiest thing in this codec to get
// backwards.
func (d *Decoder) innovationPulses(r *bitReader, pulses []float32, desc frameDesc) {
	w := 5 - desc.log2
	for n := range 5 {
		sign := float32(-1)
		if r.bit() != 0 {
			sign = 1
		}
		p1 := int(r.bits(w))
		pulses[5*p1+n] += sign
		if n < desc.double {
			p2 := int(r.bits(w))
			// The second pulse of a pair takes the opposite sign when it lands
			// after the first and the same sign when it lands before or on it.
			s2 := sign
			if p1 < p2 {
				s2 = -sign
			}
			pulses[5*p2+n] += s2
		}
	}
}

// comfortNoise fills a block from the fixed codebook at an offset a generator
// picks, which is what a silence frame has instead of pulses (note 11).
//
// The generator is not random and is not seeded from anything the encoder
// controls: it is a deterministic function of the frame index since the
// decoder started, so two decoders that agree on that index produce identical
// noise. That is also why a resumed decode has to be told where it landed.
func (d *Decoder) comfortNoise(out []float32, blockIndex int, gain float32) {
	// The counter is the frame index since the decoder started, so it advances
	// WITHIN a superframe as well as across one: the superframe's three frames
	// draw from three different offsets.
	x := 1877*blockIndex + d.frameCounter + d.frameIdx
	if x >= 0xFFFF {
		x -= 0xFFFF
	}
	y := x % 9
	// The numerator reaches about 3.3e9, so the intermediate is int64: in an
	// int it would wrap negative on a 32-bit build. The truncation to sixteen
	// bits after it is load-bearing, not tidiness: it is what makes the
	// sequence look random.
	z := int(int64(x) * 49995 / int64(5*y+6) & 0xFFFF)
	off := z % (stdCodebookLen - len(out))
	for m := range out {
		out[m] = stdCodebook[off+m] * gain
	}
}

// acbAsymmetric fills a block's excitation from the history at a pitch that
// varies within the block, covering the block in runs of constant fractional
// phase (note 9).
//
// base is the absolute index of the frame's first sample in the excitation
// buffer, so the run arithmetic's frame-relative position and the buffer's
// absolute one stay apart.
func (d *Decoder) acbAsymmetric(base, block, size int) {
	for n := 0; n < size; {
		at := block*size + n
		pSh16 := d.asymBase<<16 + d.pitchSlope*at
		p := (pSh16 + 0x6FFF) >> 16
		iSh16 := (p<<16-pSh16)*8 + 0x58000
		frac := iSh16 >> 16
		run := size
		if d.pitchSlope != 0 {
			next := iSh16 &^ 0xFFFF
			if d.pitchSlope < 0 {
				next = (iSh16 + 0x10000) &^ 0xFFFF
			}
			run = clip((iSh16-next)/d.pitchSlope/8, 1, size-n)
		}
		d.paths.AsymRuns++
		if frac >= 0 && frac < len(d.paths.AsymPhase) {
			d.paths.AsymPhase[frac] = true
		}
		d.interpAsym(base+at, p, frac, run)
		n += run
	}
}

// interpAsym runs the 17-tap asymmetric sinc: nine taps forward at phase frac
// and nine backward at phase 17-frac, both out of the same 17-entry group.
func (d *Decoder) interpAsym(at, p, frac, run int) {
	exc := d.exc
	for m := range run {
		src := at + m - p
		var acc float32
		for k := range 9 {
			acc += exc[src+k]*interpAsym[17*k+frac] + exc[src-k-1]*interpAsym[17*(k+1)-frac]
		}
		exc[at+m] = acc
	}
}

// acbHammingBlock fills a block's excitation at a pitch constant across it. A
// whole-sample lag is a forward copy, deliberately overlapping when the lag is
// shorter than the block; anything else takes the 4-tap Hamming sinc.
func (d *Decoder) acbHammingBlock(at, size, pitchQ int) {
	exc := d.exc
	p := pitchQ >> 2
	frac := pitchQ & 3
	if frac == 0 {
		for m := range size {
			exc[at+m] = exc[at+m-p]
		}
		return
	}
	for m := range size {
		src := at + m - p
		var acc float32
		for k := range 8 {
			acc += exc[src+k]*interpHamming[4*k+frac] + exc[src-k-1]*interpHamming[4*(k+1)-frac]
		}
		exc[at+m] = acc
	}
}

// synthesise runs the all-pole filter whose denominator is lpc over one block
// of excitation, into the synthesis buffer at the same position (note 9, step
// 4). at is superframe-relative; both buffers carry their own history in front
// of the superframe and the bases differ.
func (d *Decoder) synthesise(at, size int, lpc []float32) {
	sAt := d.synthBase + at
	eAt := d.excBase + at
	for m := range size {
		acc := d.exc[eAt+m]
		for j := range lpc {
			acc -= lpc[j] * d.synth[sAt+m-j-1]
		}
		d.synth[sAt+m] = acc
	}
}

// allPole is the same recursion over an arbitrary buffer, which the
// postfilter's re-synthesis stage needs (note 10.4).
func allPole(dst []float32, at, size int, lpc []float32) {
	for m := range size {
		acc := dst[at+m]
		for j := range lpc {
			acc -= lpc[j] * dst[at+m-j-1]
		}
		dst[at+m] = acc
	}
}

// allZero is the FIR of the same denominator, which the postfilter's zero
// synthesis needs (note 10.2). It reads src history rather than its own
// output, so it is not recursive.
func allZero(dst, src []float32, dstAt, srcAt, size int, lpc []float32) {
	for m := range size {
		acc := src[srcAt+m]
		for j := range lpc {
			acc += lpc[j] * src[srcAt+m-j-1]
		}
		dst[dstAt+m] = acc
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func clip(x, lo, hi int) int { return max(lo, min(hi, x)) }
