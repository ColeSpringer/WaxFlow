package musepack

import "math/bits"

// prng is the noise-substitution generator (mpc_random_int in the reference's
// synth_filter.c): two polynomial counters rotating opposite ways with coprime
// periods, XORed. It is decoder state in the fullest sense: seeded once at
// setup, advanced 36 draws per noise-coded band and channel, and never reset
// by any frame, block or seek in the reference, so a decode resumed mid-stream
// reproduces a linear one only if the container hands the state over.
type prng struct{ r1, r2 uint32 }

// newPRNG is the reference's setup state.
func newPRNG() prng { return prng{1, 1} }

// next draws one value.
func (p *prng) next() uint32 {
	t1 := uint32(bits.OnesCount32(p.r1&0xF5)&1) << 31
	t2 := uint32(bits.OnesCount32(p.r2>>25&0x63) & 1)
	p.r1 = p.r1>>1 | t1
	p.r2 = p.r2<<1 | t2
	return p.r1 ^ p.r2
}

// noise draws one quantised noise sample: the four bytes of a draw summed
// and centred, a roughly Gaussian value in -510..510.
func (p *prng) noise() int16 {
	v := p.next()
	return int16(v>>24&0xFF+v>>16&0xFF+v>>8&0xFF+v&0xFF) - 510
}

// advance draws n values and discards them, which is how a scanner follows a
// noise band without decoding it.
func (p *prng) advance(n int) {
	for range n {
		p.next()
	}
}
