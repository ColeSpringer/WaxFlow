//go:build !wmaprotablesgen

package wmapro

import "math"

// The tiling walk, the subframe order, and the channel transforms.
//
// A frame is divided into subframes independently per channel, but the
// divisions are coded jointly so that channels agreeing on one pay for it
// once. The walk below is where a paraphrase would lose the format: four
// separate conditions decide whether a presence bit is READ, and getting any
// of them wrong shifts every field after it.

// group is one subframe's channel group and its transform.
type group struct {
	chans []int
	// on reports whether this group's transform is applied at all.
	on bool
	// matrix is row-major with the output channel as the row, n by n.
	matrix []float32
	// allBands is set when every band uses the transform; otherwise bands
	// carries one flag per band.
	allBands bool
	bands    []bool
}

// The two constants the format states as exact rationals rather than as a
// computed cosine: 181/256 is the scaled two-channel matrix entry, a truncated
// cos(pi/4), and 181/128 is the compensation a disabled band of a stereo
// stream carries so that enabled and disabled bands come out at the same
// level.
const (
	scaledStereo   = 181.0 / 256.0
	bandCompensate = 181.0 / 128.0
)

// sinTable is sin(m*pi/64) for m in 0..32, the quarter turn the explicitly
// transmitted rotation matrix is built from. It is computed rather than
// tabulated: one line of arithmetic against 33 numbers in a generated file.
var sinTable = func() [33]float32 {
	var t [33]float32
	for m := range t {
		t[m] = float32(math.Sin(float64(m) * math.Pi / 64))
	}
	return t
}()

// The per-band gain is 10^(exponent/20) for an INTEGER exponent, so the
// values a decode can ask for are a short ladder and math.Pow per band per
// channel per subframe was a twentieth of the decode. The table is built with
// the same expression, so a lookup is the same float32 the call would return;
// an exponent outside it, which no real stream is near, takes the call.
const (
	gainTableMin = -256
	gainTableMax = maxBandExponent
)

var gainTable = func() []float32 {
	t := make([]float32, gainTableMax-gainTableMin+1)
	for i := range t {
		t[i] = float32(math.Pow(10, float64(i+gainTableMin)/20))
	}
	return t
}()

func gainFor(exp int) float32 {
	if exp < gainTableMin || exp > gainTableMax {
		return float32(math.Pow(10, float64(exp)/20))
	}
	return gainTable[exp-gainTableMin]
}

// tiling reads one frame's subframe divisions into every channel's subLens.
func (d *Decoder) tiling(r *bitReader) error {
	nc := d.cfg.Channels
	for c := range d.ch {
		ch := &d.ch[c]
		ch.subLens = ch.subLens[:0]
		ch.placed, ch.decoded, ch.subIdx = 0, 0, 0
		// The scale factor reuse state is per FRAME, not per stream. Resetting
		// it here is what makes a channel's first subframe in every frame
		// transmit unconditionally, and it is the only reason one frame of
		// lead-in is enough after a seek.
		ch.reuse = false
	}
	atFrontier := nc
	frontier := 0
	// One bit at the head: when set every channel takes every step. Measured
	// set on every stereo file and both ways on the multichannel ones, so a
	// real 5.1 file does tile its channels differently from each other.
	uniform := d.maxSubframes == 1 || r.bit() != 0

	for step := 0; frontier < d.frameLen; step++ {
		if step >= nc*maxSubframesPerChannel {
			return malformed("the tiling walk does not terminate")
		}
		// The last possible frontier admits one length only, so no channel at
		// it needs a presence bit and no length field is read either.
		last := frontier == d.frameLen-d.minSubframeLen
		for c := 0; c < nc; c++ {
			switch {
			case d.ch[c].placed != frontier:
				d.takes[c] = false
			case uniform || atFrontier == 1 || last:
				// The presence bit is also skipped when only one channel sits
				// at the frontier, because that channel must take the step.
				d.takes[c] = true
			default:
				d.takes[c] = r.bit() != 0
			}
		}
		length, err := d.subframeLen(r, frontier)
		if err != nil {
			return err
		}
		frontier += length
		for c := 0; c < nc; c++ {
			ch := &d.ch[c]
			if d.takes[c] {
				if len(ch.subLens) >= maxSubframesPerChannel {
					return malformed("channel %d takes more than %d subframes", c, maxSubframesPerChannel)
				}
				ch.subLens = append(ch.subLens, length)
				ch.placed += length
				if ch.placed > d.frameLen {
					return malformed("channel %d overruns the frame at %d samples", c, ch.placed)
				}
				continue
			}
			// atFrontier counts only channels that did NOT take the step, and
			// it is reset only when the frontier moves BACKWARDS. When no
			// non-taker sits at or below the new frontier it keeps the value
			// it had, and a decoder that recomputes it each step reads the
			// wrong number of presence bits.
			if ch.placed <= frontier {
				if ch.placed < frontier {
					atFrontier = 0
					frontier = ch.placed
				}
				atFrontier++
			}
		}
		if r.err != nil {
			return r.err
		}
	}
	for c := range d.ch {
		if d.ch[c].placed != d.frameLen {
			return malformed("channel %d tiles %d of %d samples", c, d.ch[c].placed, d.frameLen)
		}
	}
	return nil
}

// subframeLen reads one step's length as a right shift of the frame length.
func (d *Decoder) subframeLen(r *bitReader, frontier int) (int, error) {
	if frontier == d.frameLen-d.minSubframeLen {
		return d.minSubframeLen, nil
	}
	depth := d.cfg.subframeDepth()
	w := floorLog2(depth) + 1
	var shift int
	if d.maxSubframes == 4 || d.maxSubframes == 16 {
		if r.bit() != 0 {
			shift = 1 + int(r.bits(w-1))
		}
	} else {
		shift = int(r.bits(w))
	}
	// At a depth of 32 the raw field can express a shift of 6 or 7, which
	// names a subframe shorter than the minimum.
	if shift > depth {
		return 0, malformed("a subframe length shift of %d, want at most %d", shift, depth)
	}
	length := d.frameLen >> shift
	if length < d.minSubframeLen || frontier+length > d.frameLen {
		return 0, malformed("a subframe of %d samples at frontier %d of %d", length, frontier, d.frameLen)
	}
	return length, nil
}

// subframes decodes every subframe of the frame in the order the tiling
// implies: always the lowest offset any channel has reached, and at a tie the
// length the lowest-indexed channel there is waiting for.
func (d *Decoder) subframes(r *bitReader) error {
	for step := 0; ; step++ {
		if step >= d.cfg.Channels*maxSubframesPerChannel+1 {
			return malformed("the subframe walk does not terminate")
		}
		offset := d.frameLen
		for c := range d.ch {
			offset = min(offset, d.ch[c].decoded)
		}
		if offset >= d.frameLen {
			return nil
		}
		length := 0
		for c := range d.ch {
			ch := &d.ch[c]
			if ch.decoded == offset {
				if ch.subIdx >= len(ch.subLens) {
					return malformed("channel %d has no subframe at offset %d", c, offset)
				}
				length = ch.subLens[ch.subIdx]
				break
			}
		}
		part := d.part[:0]
		for c := range d.ch {
			ch := &d.ch[c]
			if ch.decoded == offset && ch.subIdx < len(ch.subLens) && ch.subLens[ch.subIdx] == length {
				part = append(part, c)
			}
		}
		d.part = part
		if err := d.subframe(r, part, offset, length); err != nil {
			return err
		}
		for _, c := range part {
			d.ch[c].decoded += length
			d.ch[c].subIdx++
		}
	}
}

// sizeIndex is log2(frameLen / length), which indexes every per-size table.
func (d *Decoder) sizeIndex(length int) int { return floorLog2(d.frameLen / length) }

// subframe decodes one subframe of the participating channels.
func (d *Decoder) subframe(r *bitReader, part []int, off, length int) error {
	d.paths.Subframes++
	k := d.sizeIndex(length)
	if k >= d.sizes {
		return malformed("a subframe of %d samples has no band layout", length)
	}
	edges := d.lay.edges[k]
	nb := d.lay.numBands(k)

	// The low-bit-rate tool's payload, seen from inside the bitstream. A
	// stream that sets it was refused by ParseConfig, so reaching it means the
	// configuration check was bypassed; it is refused again rather than
	// skipped, because skipping it is what makes the reference produce a
	// band-limited decode nothing can check.
	if r.bit() != 0 {
		return unsupported("a subframe carries the low-bit-rate tool's payload")
	}
	if r.bit() != 0 {
		return malformed("a reserved subframe bit is set")
	}
	if d.cfg.Channels > 1 {
		if err := d.channelGroups(r, part, k); err != nil {
			return err
		}
	} else {
		d.nGroups = 0
	}
	any := false
	for _, c := range part {
		d.ch[c].transmits = r.bit() != 0
		any = any || d.ch[c].transmits
	}
	if any {
		if err := d.quantisation(r, part, k, length); err != nil {
			return err
		}
	}
	for _, c := range part {
		ch := &d.ch[c]
		ch.active = ch.transmits
		if !ch.transmits {
			// A channel that does not transmit has all L coefficients zero and
			// reads nothing. Clearing them is not tidiness: they would
			// otherwise be the previous subframe's, and the channel transform
			// below gathers them.
			clear(ch.coefs[:length])
			continue
		}
		if err := d.coefficients(r, ch, length); err != nil {
			return err
		}
	}
	if r.err != nil {
		return r.err
	}
	// A channel that transmitted nothing still comes out of a transform with
	// coefficients, so "transmits" stops meaning "silent" the moment a group
	// it is in has one.
	for gi := range d.nGroups {
		g := &d.groups[gi]
		if !g.on || len(g.chans) < 2 {
			continue
		}
		live := false
		for _, c := range g.chans {
			live = live || d.ch[c].transmits
		}
		if live {
			for _, c := range g.chans {
				d.ch[c].active = true
			}
		}
	}
	d.applyTransforms(k, length, edges, nb)
	for _, c := range part {
		if err := d.reconstruct(c, off, length, edges, nb); err != nil {
			return err
		}
	}
	return nil
}

// channelGroups forms the subframe's groups and reads each one's transform.
func (d *Decoder) channelGroups(r *bitReader, part []int, k int) error {
	if r.bit() != 0 {
		return unsupported("an unknown channel transform")
	}
	d.nGroups = 0
	left := append(d.ungrouped[:0], part...)
	for len(left) > 0 && d.nGroups < len(part) {
		g := &d.groups[d.nGroups]
		g.chans, g.on, g.allBands = g.chans[:0], false, false
		if len(left) > 2 {
			rest := d.rest[:0]
			for _, c := range left {
				if r.bit() != 0 {
					g.chans = append(g.chans, c)
				} else {
					rest = append(rest, c)
				}
			}
			d.rest = rest
			left = append(left[:0], rest...)
		} else {
			// Two or fewer ungrouped channels all join, and no bits are read.
			g.chans = append(g.chans, left...)
			left = left[:0]
		}
		if err := d.groupTransform(r, g); err != nil {
			return err
		}
		// The band enables follow each group's own transform, before the
		// next group is formed. No corpus subframe has two groups with
		// transforms on, so nothing here can tell this order from a second
		// pass over the groups; the notes' structure is the authority.
		if err := d.groupBands(r, g, k); err != nil {
			return err
		}
		d.nGroups++
	}
	d.ungrouped = left
	return r.err
}

// groupTransform reads one group's transform selection.
func (d *Decoder) groupTransform(r *bitReader, g *group) error {
	n := len(g.chans)
	switch {
	case n < 2:
		// A group of one channel, or of zero, reads nothing and has no
		// transform. A group of zero is possible and harmless.
		return nil
	case n == 2:
		if r.bit() == 0 {
			g.on = true
			g.matrix = d.stereoMatrix(g.matrix)
			if d.cfg.Channels == 2 {
				d.paths.StereoMatrix++
			} else {
				d.paths.ScaledMatrix++
			}
			return nil
		}
		if r.bit() != 0 {
			return unsupported("an unknown two-channel channel transform")
		}
		d.paths.NoTransform++
		return nil
	default:
		if r.bit() == 0 {
			return nil
		}
		g.on = true
		if r.bit() != 0 {
			d.paths.ExplicitMatrix++
			return d.explicitMatrix(r, g)
		}
		// The built-in matrices stop at six channels. The reference complains
		// and leaves whatever matrix the group last held, which is undefined
		// output; an eight-channel stream can ask for this.
		if n >= len(defaultDecorrelation) {
			return unsupported("a built-in decorrelation matrix for a group of %d channels", n)
		}
		d.paths.BuiltinMatrix++
		g.matrix = append(g.matrix[:0], defaultDecorrelation[n]...)
		return nil
	}
}

// stereoMatrix is the fixed two-channel transform. Which of the two forms
// applies depends on the STREAM's channel count, not the group's: a
// two-channel group inside a wider stream carries the scaling in the matrix,
// while a stereo stream carries it as the compensation on disabled bands.
func (d *Decoder) stereoMatrix(buf []float32) []float32 {
	m := append(buf[:0], 0, 0, 0, 0)
	if d.cfg.Channels == 2 {
		m[0], m[1], m[2], m[3] = 1, -1, 1, 1
	} else {
		m[0], m[1], m[2], m[3] = scaledStereo, -scaledStereo, scaledStereo, scaledStereo
	}
	return m
}

// explicitMatrix builds the transmitted rotation from its 6-bit angles and its
// sign diagonal.
func (d *Decoder) explicitMatrix(r *bitReader, g *group) error {
	n := len(g.chans)
	angles := d.angles[:0]
	for i := 0; i < n*(n-1)/2; i++ {
		angles = append(angles, int(r.bits(6)))
	}
	d.angles = angles
	if r.err != nil {
		return r.err
	}
	if cap(g.matrix) < n*n {
		g.matrix = make([]float32, n*n)
	}
	m := g.matrix[:n*n]
	clear(m)
	for i := range n {
		// A SET sign bit is +1, the same polarity every other sign bit in this
		// codec uses.
		if r.bit() != 0 {
			m[i*n+i] = 1
		} else {
			m[i*n+i] = -1
		}
	}
	p := 0
	for i := 1; i < n; i++ {
		for x := range i {
			a := angles[p+x]
			var s, c float32
			if a < 32 {
				s, c = sinTable[a], sinTable[32-a]
			} else {
				s, c = sinTable[64-a], -sinTable[a-32]
			}
			// The inner loop runs to i INCLUSIVE, the angle is constant across
			// it, and m[x*n+y] is overwritten before m[i*n+y] would read it,
			// so both are read first.
			for y := 0; y <= i; y++ {
				v1, v2 := m[x*n+y], m[i*n+y]
				m[x*n+y] = v1*s - v2*c
				m[i*n+y] = v1*c + v2*s
			}
		}
		p += i
	}
	g.matrix = m
	return r.err
}

// groupBands reads which bands a group's transform applies to.
func (d *Decoder) groupBands(r *bitReader, g *group, k int) error {
	if !g.on {
		return nil
	}
	if r.bit() != 0 {
		g.allBands = true
		return nil
	}
	d.paths.PerBandEnables++
	nb := d.lay.numBands(k)
	g.bands = g.bands[:0]
	for range nb {
		g.bands = append(g.bands, r.bit() != 0)
	}
	return r.err
}

// applyTransforms runs every group's transform over the coded coefficients,
// which happens after all channels are decoded and BEFORE dequantisation.
func (d *Decoder) applyTransforms(k, length int, edges []int, nb int) {
	for gi := range d.nGroups {
		g := &d.groups[gi]
		n := len(g.chans)
		if !g.on || n < 2 {
			continue
		}
		for b := range nb {
			lo, hi := edges[b], min(edges[b+1], length)
			enabled := g.allBands || (b < len(g.bands) && g.bands[b])
			if !enabled {
				// A disabled band of a STEREO stream carries the compensation
				// that puts it at the same level as an enabled one; in a wider
				// stream the matrix is already scaled and nothing happens.
				if d.cfg.Channels == 2 {
					for _, c := range g.chans {
						co := d.ch[c].coefs
						for y := lo; y < hi; y++ {
							co[y] *= bandCompensate
						}
					}
				}
				continue
			}
			for y := lo; y < hi; y++ {
				// Gather first and write second: every channel is updated from
				// the same input vector.
				for j, c := range g.chans {
					d.vec[j] = d.ch[c].coefs[y]
				}
				for m, c := range g.chans {
					row := g.matrix[m*n : m*n+n]
					var acc float32
					for j := range n {
						acc += d.vec[j] * row[j]
					}
					d.ch[c].coefs[y] = acc
				}
			}
		}
	}
}

// reconstruct dequantises one channel's subframe, transforms it, and folds it
// into the rolling output buffer.
func (d *Decoder) reconstruct(c, off, length int, edges []int, nb int) error {
	ch := &d.ch[c]
	base := d.frameLen/2 + off
	if ch.active {
		for b := range nb {
			lo, hi := edges[b], min(edges[b+1], length)
			// A level in decibels relative to the loudest band, so the loudest
			// band's exponent is the channel's quantisation step. The division
			// by 20 is floating point and the exponent can be negative; the
			// power is taken in double and narrowed, because the exponent
			// reaches +115 and no two float32 pow implementations agree there.
			exp := ch.quantStep - (ch.maxScale-ch.scale[b])*ch.sfRes
			if exp > maxBandExponent {
				return malformed("a band exponent of %d, past the %d this decoder holds finite",
					exp, maxBandExponent)
			}
			gain := gainFor(exp)
			for y := lo; y < hi; y++ {
				d.spec[y] = ch.coefs[y] * gain
			}
		}
		// (2/L) inverts the unnormalised transform and 2^-(bits-1) puts the
		// output at a nominal full scale of 1.0. The second term pairs with
		// the quantisation base scaling with the depth: getting only one of
		// them right is about 48 dB of error.
		scale := 2 / float64(length) * math.Ldexp(1, -(d.cfg.BitsPerSample-1))
		d.plans[d.sizeIndex(length)].imdct(d.spec[:length], ch.buf[base:base+length], scale, d.scratch)
	} else {
		// A subframe in which this channel has nothing still runs the window
		// and the overlap, with an all-zero transform output. The transform of
		// zero is zero, so it is written rather than computed.
		clear(ch.buf[base : base+length])
	}
	// The window transition needs no start or stop shape: the overlap is the
	// shorter of the two adjacent subframes and is always centred exactly on
	// the boundary between them.
	ov := min(ch.prevLen, length)
	overlapButterfly(ch.buf[base-ov/2:base+ov/2], d.plans[d.sizeIndex(ov)].window)
	ch.prevLen = length
	return nil
}
