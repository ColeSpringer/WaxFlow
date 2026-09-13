package adpcm

// Microsoft ADPCM (WAVE format tag 0x0002), from Microsoft's "Multimedia
// Programming Interface and Data Specifications 1.0" and the "Multimedia
// Data Standards Update" that defines the compressed WAVE formats.
//
// Where IMA keeps one predictor, this keeps two previous samples and a pair
// of coefficients, so a nibble codes the residual of a second-order
// prediction. The coefficients come from the file: the specification says a
// stream carries its own table, and DefaultCoefs is only what encoders
// write.
//
// The two tables below are that specification's data.

// adaptTable scales the step size after each nibble, in 1/256ths.
var adaptTable = [16]int64{
	230, 230, 230, 230, 307, 409, 512, 614,
	768, 614, 512, 409, 307, 230, 230, 230,
}

// DefaultCoefs is the predictor coefficient table every known encoder
// writes, in 1/256ths. A file states its own, and one that states a
// different one decodes differently here than in readers that ignore the
// field; the demuxers say so when they see it.
var DefaultCoefs = [7][2]int16{
	{256, 0}, {512, -256}, {0, 0}, {192, 64}, {240, 0}, {460, -208}, {392, -232},
}

// msDeltaMin is the floor the step size adapts down to. Without it the step
// reaches zero and the predictor stops tracking the signal for good.
const msDeltaMin = 16

// msDeltaMax is a ceiling the specification does not state and no stream
// reaches. The step grows by up to 3x per nibble with nothing in the format
// to stop it, so a crafted block runs it past any fixed width; a header can
// only state one as an int16, and a step this large saturates every sample it
// touches. The whole state is 64-bit for the same reason: the coefficients
// are the file's own int16 pair, so the second-order prediction alone can
// reach 2^31 on samples a decoder legitimately produced.
const msDeltaMax = 1 << 30

// msState is one channel's coder state. It is 64-bit throughout; see
// msDeltaMax for why the 16-bit domain the samples live in is not enough to
// compute them in.
type msState struct {
	coef1, coef2 int64
	delta        int64
	sample1      int64
	sample2      int64
}

// next advances one channel by one nibble and returns the sample.
//
// The prediction divides by 256 with truncation toward zero, which is the
// expression the specification writes. An arithmetic shift agrees with it on
// every fixture in this tree and differs on a negative odd quotient, so the
// spec's form is what is implemented rather than the cheaper one.
func (s *msState) next(nib byte) int32 {
	predicted := trunc256(s.sample1*s.coef1 + s.sample2*s.coef2)
	// The nibble is a signed 4-bit multiplier of the current step.
	val := clip16(predicted + s.delta*int64(int8(nib<<4)>>4))
	s.sample2, s.sample1 = s.sample1, int64(val)
	// A plain shift, not trunc256: both factors are positive (every
	// adaptation entry is, and the step is floored at msDeltaMin), so the two
	// agree bit for bit and the branch trunc256 carries for the negative case
	// is what keeps this out of the inliner's budget.
	s.delta = min(max(adaptTable[nib]*s.delta>>8, msDeltaMin), msDeltaMax)
	return val
}

// decodeMS decodes one Microsoft ADPCM block into per-channel output.
//
// The header's fields are grouped by field and not by channel: every
// channel's predictor index, then every channel's step size, then sample1
// for each and sample2 for each. The nibbles that follow are dealt one at a
// time across the channels, high nibble of each byte first. Output order per
// channel is the older header sample, the newer one, then the nibbles.
func (d *Decoder) decodeMS(blk []byte) error {
	ch := d.cfg.Channels
	for c := range ch {
		idx := blk[c]
		if int(idx) >= len(d.cfg.Coefs) {
			return malformed("predictor index %d in a block header (the table holds %d)", idx, len(d.cfg.Coefs))
		}
		st := &d.ms[c]
		st.coef1 = int64(d.cfg.Coefs[idx][0])
		st.coef2 = int64(d.cfg.Coefs[idx][1])
		st.delta = int64(int16(le16(blk[ch+2*c:])))
		st.sample1 = int64(int16(le16(blk[3*ch+2*c:])))
		st.sample2 = int64(int16(le16(blk[5*ch+2*c:])))
		d.scratch[c][0] = int32(st.sample2)
		d.scratch[c][1] = int32(st.sample1)
	}
	body := blk[msHeaderBytes*ch:]
	c, n := 0, 2
	for _, b := range body {
		for _, nib := range [2]byte{b >> 4, b & 0x0F} {
			if n >= len(d.scratch[c]) {
				// Unreachable through a config this package validates:
				// BlockFrames counts exactly these nibbles, and its division
				// by the channel count is exact for the one and two channels
				// this format defines. An ERROR rather than a quiet stop,
				// because a quiet one would emit a block's worth of whatever
				// the previous block left in the scratch row: the coverage
				// arithmetic is what makes that unreachable, so the day it
				// stops holding is the day this has to be loud.
				return malformed("block of %d bytes decodes past the %d samples its geometry states",
					len(blk), len(d.scratch[c]))
			}
			// In place through the slice, not through a local: the channel
			// alternates every nibble here, so a hoisted copy is a 40-byte
			// store and load per sample and measures 2.2x slower than this.
			d.scratch[c][n] = d.ms[c].next(nib)
			if c++; c == ch {
				c, n = 0, n+1
			}
		}
	}
	return nil
}
