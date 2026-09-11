//go:build !wmaprotablesgen

package wmapro

// The quantisation section, the scale factors, and the coefficient layer.
//
// Every sign bit in this codec means the OPPOSITE of the usual convention: a
// set bit is positive. Getting it backwards produces a bitstream-valid decode
// whose every sample is negated, which reads as a deviation of exactly twice
// the peak and is otherwise silent. TestDecodeIsNotGloballyNegated exists
// because of it.

// Bounds a crafted stream can otherwise drive through the roof. Neither is a
// format constant and neither fires on real material: the measured band
// exponent runs -25 to +115 and the quantisation step escape fires with small
// values, so these sit many decades clear of anything an encoder writes.
const (
	// maxBandExponent is a gain of 10^20. Past it the dequantised spectrum,
	// times the largest magnitude the escape can name and the transform's own
	// accumulation, stops being a finite float32, and a NaN or an Inf leaving
	// this package poisons the loudness meter and the resampler downstream
	// where it would be a long way from its cause.
	maxBandExponent = 400
	// maxStepEscape bounds the quantisation step's 5-bit chunked escape, which
	// is otherwise a run of 31s that only the end of the frame stops.
	maxStepEscape = 1 << 20
)

// quantisation reads the whole quantisation section: the optional vector
// limits, the step and its escape, the per-channel modifiers, and every
// participating channel's scale factors.
func (d *Decoder) quantisation(r *bitReader, part []int, k, length int) error {
	// Measured NEVER transmitted, on any subframe of any file, so the variant
	// of the coefficient phase it enables is implemented from the notes and
	// carried as a deficit rather than verified.
	d.explicitLimit = r.bit() != 0
	if d.explicitLimit {
		d.paths.ExplicitLimit++
		w := floorLog2((length+3)/4) + 1
		for _, c := range part {
			v := int(r.bits(w)) << 2
			if v > length {
				return malformed("a vector coefficient limit of %d in a subframe of %d", v, length)
			}
			d.ch[c].vecLimit = v
		}
	} else {
		for _, c := range part {
			d.ch[c].vecLimit = length
		}
	}

	// 90 at 16 bits and 135 at 24, which is what makes a 16-bit and a 24-bit
	// stream of the same material come out at the same level once the
	// transform's own depth term is applied.
	delta := int(r.bits(6))
	if delta >= 32 {
		delta -= 64
	}
	step := (90*d.cfg.BitsPerSample)>>4 + delta
	if delta == -32 || delta == 31 {
		d.paths.StepEscape++
		acc := 0
		for {
			v := int(r.bits(5))
			if r.err != nil {
				return malformed("the quantisation step escape runs out of bits")
			}
			acc += v
			if v != 31 {
				break
			}
			if acc > maxStepEscape {
				return malformed("a quantisation step escape past %d", maxStepEscape)
			}
		}
		if delta == 31 {
			step += acc
		} else {
			step -= acc
		}
	}

	if len(part) == 1 {
		// One participating channel takes the subframe's step and reads
		// nothing.
		d.ch[part[0]].quantStep = step
	} else {
		w := int(r.bits(3))
		for _, c := range part {
			q := step
			if r.bit() != 0 {
				// The modifier is at least 1 when its bit is set, and a width
				// of zero means a fixed increment of 1 with no value bits.
				d.paths.Modifiers++
				if w != 0 {
					q += int(r.bits(w)) + 1
				} else {
					q++
				}
			}
			d.ch[c].quantStep = q
		}
	}
	if r.err != nil {
		return r.err
	}
	for _, c := range part {
		if err := d.scaleFactors(r, &d.ch[c], k); err != nil {
			return err
		}
	}
	return r.err
}

// scaleFactors reads one channel's per-band scale factors, resampling from the
// last set it transmitted in this frame when it transmits none of its own.
//
// The state below lives for the duration of ONE frame and is reset at the
// start of every frame, which is exactly why one frame of lead-in is enough
// after a seek.
func (d *Decoder) scaleFactors(r *bitReader, ch *channel, k int) error {
	nb := d.lay.numBands(k)
	work := ch.scale[:nb]
	clear(work)
	if ch.reuse {
		m := d.lay.resample[k][ch.savedSize]
		for b := range work {
			work[b] = ch.saved[m[b]]
		}
	}
	// The channel's FIRST subframe in a frame always transmits, with no bit
	// read. That is its first subframe, not its first transmission: a channel
	// whose leading subframes coded nothing reads the bit here even though it
	// has never transmitted this frame, and a clear bit then leaves every
	// band at zero.
	if ch.subIdx == 0 || r.bit() != 0 {
		if !ch.reuse {
			d.paths.DPCM++
			ch.sfRes = int(r.bits(2)) + 1
			v := 45 / ch.sfRes
			for b := range work {
				s := d.bk.scaleDelta.decode(r)
				if s < 0 {
					return malformed("a scale factor delta codeword is not in the book")
				}
				v += s - scaleDeltaBias
				work[b] = v
			}
		} else {
			d.paths.RunLevelDiffs++
			if err := d.scaleDiffs(r, work); err != nil {
				return err
			}
		}
		// saved is updated only on a TRANSMISSION, so a subframe that
		// resamples without transmitting uses the resampled values while the
		// next resample still starts from the last transmitted set, at that
		// set's size index.
		copy(ch.saved[:nb], work)
		ch.savedSize = k
		ch.reuse = true
	} else if ch.reuse {
		d.paths.ResampleOnly++
	} else {
		d.paths.ZeroScale++
	}
	ch.maxScale = work[0]
	for _, v := range work {
		ch.maxScale = max(ch.maxScale, v)
	}
	return r.err
}

// scaleDiffs applies one subframe's run-level differences to the resampled
// scale factors. Bands the run never reaches keep their resampled values.
func (d *Decoder) scaleDiffs(r *bitReader, work []int) error {
	for b := 0; b < len(work); b++ {
		s := d.bk.scaleRunLevel.decode(r)
		if s < 0 {
			return malformed("a scale factor difference codeword is not in the book")
		}
		var skip, val, sign int
		switch s {
		case 0:
			// The 14-bit escape packs three fields into one word: the level in
			// the top 8 bits, a 5-bit run in bits 1 to 5, and the sign in bit
			// 0. It fires often enough to be a hot path, not a corner.
			d.paths.ScaleEscape++
			code := int(r.bits(14))
			val = code >> 6
			sign = (code & 1) - 1
			skip = (code & 0x3f) >> 1
		case 1:
			return r.err
		default:
			skip = int(scaleRunLevelRuns[s])
			val = int(scaleRunLevelLevels[s])
			sign = int(r.bit()) - 1
		}
		b += skip
		if b >= len(work) {
			return malformed("a scale factor run skips past the last of %d bands", len(work))
		}
		// sign is 0 for a SET bit and -1 for a clear one, so this is +val or
		// -val with a set bit meaning positive.
		work[b] += (val ^ sign) - sign
		if r.err != nil {
			return r.err
		}
	}
	return r.err
}

// coefficients decodes one channel's coded magnitudes: a vector phase four at
// a time from the low end up, then a sparse run-level tail. The switch between
// them is a rule rather than a flag.
func (d *Decoder) coefficients(r *bitReader, ch *channel, length int) error {
	book := int(r.bit())
	coefs := ch.coefs[:length]
	cur, zeros := 0, 0
	runLevel := false
	// L>>8, not L/128: at 2048 that is 8, so nine zeros in a row switch, and
	// at any length below 256 it is 0, so a single zero switches.
	threshold := length >> 8
	limit := min(ch.vecLimit, length)

	// runLevel is tested only at the top, so the group of four that trips it
	// is always completed. An explicit vector limit disables the rule outright
	// and the vector phase runs to the limit.
	for (d.explicitLimit || !runLevel) && cur+3 < limit {
		var mags [4]int64
		if err := d.fourMagnitudes(r, &mags); err != nil {
			return err
		}
		for _, m := range mags {
			switch {
			case m != 0:
				if r.bit() != 0 {
					coefs[cur] = float32(m)
				} else {
					coefs[cur] = float32(-m)
				}
				zeros = 0
			default:
				coefs[cur] = 0
				zeros++
				if zeros > threshold {
					runLevel = true
				}
			}
			cur++
		}
		if r.err != nil {
			return r.err
		}
	}
	if cur < length {
		d.paths.TailReached++
		clear(coefs[cur:])
		return d.runLevelTail(r, book, coefs, cur)
	}
	d.paths.VectorCovers++
	return r.err
}

// fourMagnitudes reads one group of four coded magnitudes, through the widest
// book that can carry it. The magnitudes are int64 because the single book's
// escape adds up to 31 bits to 100, which an int does not hold on a 32-bit
// build: the same stream would then decode a huge magnitude negative there
// and positive on a 64-bit build.
func (d *Decoder) fourMagnitudes(r *bitReader, mags *[4]int64) error {
	s4 := d.bk.vec4.decode(r)
	if s4 < 0 {
		return malformed("a four-magnitude codeword is not in the book")
	}
	// The book's symbols carry a bias of -1, so the entry whose raw symbol is
	// zero becomes the escape and every other entry's four nibbles are four
	// magnitudes, high nibble first.
	if v := s4 - vecBias; v >= 0 {
		mags[0], mags[1] = int64((v>>12)&15), int64((v>>8)&15)
		mags[2], mags[3] = int64((v>>4)&15), int64(v&15)
		return nil
	}
	d.paths.Vec4Escape++
	for h := range 2 {
		s2 := d.bk.vec2.decode(r)
		if s2 < 0 {
			return malformed("a two-magnitude codeword is not in the book")
		}
		if v := s2 - vecBias; v >= 0 {
			mags[2*h], mags[2*h+1] = int64((v>>4)&15), int64(v&15)
			continue
		}
		d.paths.Vec2Escape++
		for j := range 2 {
			v := d.bk.vec1.decode(r)
			if v < 0 {
				return malformed("a single-magnitude codeword is not in the book")
			}
			m := int64(v)
			if v == vec1Escape {
				d.paths.LargeValue++
				m += int64(d.largeValue(r))
			}
			mags[2*h+j] = m
		}
	}
	return nil
}

// largeValue is the widening escape the single vector book and the run-level
// escape share: 8, 16, 24 or 31 bits. The last step adds 7, not 8.
func (d *Decoder) largeValue(r *bitReader) uint64 {
	n := 8
	if r.bit() != 0 {
		n += 8
		if r.bit() != 0 {
			n += 8
			if r.bit() != 0 {
				n += 7
			}
		}
	}
	return r.bits(n)
}

// runLevelTail codes the sparse coefficients above the vector phase. End of
// block may be omitted: the walk simply runs out.
//
// An index at or past the subframe length is refused rather than masked. The
// reference masks it into a wrap and then reports the overrun while keeping
// the wrapped samples, which is wrong audio reported as a warning; the
// format's own answer is that such an index is a broken frame, and no file in
// reach produces one.
func (d *Decoder) runLevelTail(r *bitReader, book int, coefs []float32, from int) error {
	length := len(coefs)
	escapeWidth := floorLog2(length-1) + 1
	b := d.bk.coef[book]
	runs, levels := coefRuns[book], coefLevels[book]
	for i := from; i < length; i++ {
		s := b.decode(r)
		if s < 0 {
			return malformed("a run-level codeword is not in the book")
		}
		var level float32
		switch {
		case s > 1:
			i += int(runs[s])
			level = levels[s]
		case s == 1:
			d.paths.EndOfBlock++
			return r.err
		default:
			d.paths.TailEscape++
			level = float32(d.largeValue(r))
			// A clear first bit means a run of zero.
			if r.bit() != 0 {
				if r.bit() == 0 {
					i += int(r.bits(2)) + 1
				} else if r.bit() != 0 {
					return malformed("a run-level escape sets all three run bits")
				} else {
					i += int(r.bits(escapeWidth)) + 4
				}
			}
		}
		if r.err != nil {
			return r.err
		}
		if i >= length {
			return malformed("a run-level run reaches %d in a subframe of %d", i, length)
		}
		if r.bit() != 0 {
			coefs[i] = level
		} else {
			coefs[i] = -level
		}
	}
	return r.err
}
