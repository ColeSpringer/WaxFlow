//go:build !wmavoicetablesgen

package wmavoice

// The pitch-adaptive window pulse coder (note 8.2), used only by frame type 2,
// which is a two-block frame of 80 samples a block. The literal 80s and 160s
// below are that frame's geometry and the note states them the same way.
//
// This is the least verified part of the notes and so of this decoder. Two
// cells reach the coder at all and both through one arm: window range 24,
// positive pulse counts, no extended index, no concealment. The other arms
// ship implemented from the description and are named as deficits in
// docs/quality-gates.md.

// awBlockSize is a type 2 frame's block, and awMaskWords holds the exclusion
// mask: five words for the 80 positions, two of front padding because the
// clearing walk starts at a negative index, and two of back padding because it
// clears a window's width past its last position.
const (
	awBlockSize = 80
	awMaskWords = 9
	awMaskBias  = 32 // absolute bit position of logical position 0
)

// awCoords is what the once-per-frame coordinate field yields.
type awCoords struct {
	rng      int
	extended bool
	nPulses  [2]int
	firstOff [2]int
}

// awReadCoords reads the frame's window coordinates, which need both blocks'
// pitches and so are read after the pitch field rather than before it.
func (d *Decoder) awReadCoords(r *bitReader) error {
	b := int(r.bits(6))
	var extended bool
	if b >= 54 {
		// DEFICIT: the extension never occurs on any cell; every real frame
		// reads a plain 6-bit value below 54.
		extended = true
		b += (b-54)*3 + int(r.bits(2))
	}
	if r.err != nil {
		return r.err
	}
	start := int(awStartOffsets[b])

	p0, p1 := d.pitch[0], d.pitch[1]
	if p0 < 1 || p1 < 1 {
		// Unreachable: every pitch this codec produces is at least minPitch,
		// which is at least 19 at any rate the config accepts. Checked anyway
		// because the walks below step by these and a zero would not end.
		return malformed("window pulse coder reached with pitch %d, %d", p0, p1)
	}
	c := awCoords{rng: 16, extended: extended}
	if min(p0, p1) > 32 {
		c.rng = 24
	}
	offset := start
	for offset < 0 {
		offset += p0
	}
	c.nPulses[0] = (p0 - 1 + awBlockSize - offset) / p0
	c.firstOff[0] = offset - c.rng/2
	offset += c.nPulses[0] * p0
	c.nPulses[1] = (p1 - 1 + frameSamples - offset) / p1
	c.firstOff[1] = offset - (frameSamples+c.rng)/2
	if start < awBlockSize {
		for c.firstOff[1]-p1+c.rng > 0 {
			c.firstOff[1] -= p1
		}
		if start < 0 {
			for c.firstOff[0]-p0+c.rng > 0 {
				c.firstOff[0] -= p0
			}
		}
	}
	d.aw = c
	return nil
}

// awFirstSet reads a block's first pulse field and scatters what it packs.
// Every pulse from this set repeats at the pitch lag, laying a comb.
//
// A SET sign bit is -1 here, the opposite of the innovation scheme's.
func (d *Decoder) awFirstSet(r *bitReader, pulses []float32, block, pitchLag int) {
	width := 12
	if d.aw.extended && block == 0 {
		width = 10
	}
	field := int(r.bits(width))
	if d.aw.nPulses[block] > 0 {
		count, signMask, idxMask, sh := 3, 8, 7, 4
		if d.aw.rng != 24 {
			// DEFICIT: no cell reaches a window range of 16.
			count, signMask, idxMask, sh = 4, 4, 3, 3
		}
		for n := count - 1; n >= 0; n-- {
			sign := float32(1)
			if field&signMask != 0 {
				sign = -1
			}
			pos := (field&idxMask)*count + n + d.aw.firstOff[block]
			for pos < 0 {
				pos += pitchLag
			}
			// Past the block after the wrap the pulse is dropped, not clamped.
			if pos < awBlockSize {
				scatter(pulses, pos, sign, pitchLag, true)
			}
			field >>= sh
		}
		return
	}
	// DEFICIT: never reached. With no positive pulse count the same field is
	// a pair of non-repeating pulses separated by 1, 3, 5 or 7 samples, whose
	// second sign is independent of the first.
	v := (field & 0x1FF) >> 1
	sign := float32(1)
	if field&0x200 != 0 {
		sign = -1
	}
	var gap, i int
	switch {
	case v < 79:
		gap, i = 1, v+1
	case v < 156:
		gap, i = 3, v+1-77
	case v < 231:
		gap, i = 5, v+1-152
	default:
		gap, i = 7, v+1-225
	}
	if i-gap >= 0 && i < awBlockSize {
		scatter(pulses, i-gap, sign, pitchLag, false)
		s2 := sign
		if field&1 != 0 {
			s2 = -sign
		}
		scatter(pulses, i, s2, pitchLag, false)
	}
}

// awSecondSet adds one more pulse at a position the first set did not already
// cover, chosen through an exclusion mask. It reports false when no free
// position is left, which is the concealment case the caller handles.
func (d *Decoder) awSecondSet(r *bitReader, pulses []float32, block, pitchLag int) bool {
	var mask [awMaskWords]uint16
	for i := range awBlockSize {
		mask[(i+awMaskBias)>>4] |= 1 << uint(15-(i+awMaskBias)&15)
	}

	pulseOff := d.aw.firstOff[block]
	if d.aw.nPulses[block] > 0 {
		for pulseOff+d.aw.rng < 1 {
			pulseOff += pitchLag
		}
	}
	span := 16
	if d.aw.nPulses[0] > 0 {
		if block == 0 {
			span = 32
		} else {
			span = 8
			if d.aw.nPulses[1] > 0 {
				pulseOff = d.awNextOff
			}
		}
	}
	pulseStart := 0
	if d.aw.nPulses[block] > 0 {
		pulseStart = pulseOff - span/2
		for idx := pulseOff; idx < awBlockSize; idx += pitchLag {
			for i := range d.aw.rng {
				at := idx + i + awMaskBias
				mask[at>>4] &^= 1 << uint(15-at&15)
			}
		}
	}

	width := 4
	if d.aw.nPulses[0] > 0 {
		width = 5 - 2*block
	}
	wanted := int(r.bits(width))

	n, start := 0, 0
	for p := pulseStart; n <= wanted; p++ {
		idx := p
		for idx < 0 {
			idx += pitchLag
		}
		if idx >= awBlockSize {
			// Past the block, take the lowest free position anywhere. The
			// first non-empty word's highest set bit is that position, since
			// the mask runs most significant bit first.
			idx = -1
			for w := range 5 {
				if mask[w+2] != 0 {
					idx = 16*w + 15 - floorLog2(uint32(mask[w+2]))
					break
				}
			}
			if idx < 0 {
				return false
			}
		}
		at := idx + awMaskBias
		if mask[at>>4]&(1<<uint(15-at&15)) != 0 {
			mask[at>>4] &^= 1 << uint(15-at&15)
			n++
			start = idx
		}
	}

	sign := float32(1)
	if r.bit() != 0 {
		sign = -1
	}
	scatter(pulses, start, sign, pitchLag, true)

	if rem := (awBlockSize - start) % pitchLag; rem != 0 {
		d.awNextOff = pitchLag - rem
	} else {
		d.awNextOff = 0
	}
	return true
}

// scatter lays one pulse into the block, repeating at the pitch lag when the
// scheme says so (note 8).
func scatter(pulses []float32, pos int, sign float32, pitchLag int, repeat bool) {
	for x := pos; x < len(pulses); x += pitchLag {
		pulses[x] += sign
		if !repeat {
			return
		}
	}
}
