//go:build !wmatablesgen

package wma

// Test-only seams exposed to the external wma_test package.

// HighFreqMultForTest is the noise-coding ladder: whether noise coding is on
// at all and, when it is, how far up the spectrum ordinary coefficients reach.
// It is checked against the corpus's derived column directly, because the
// ladder's arms are chosen by knife-edge thresholds on the bit rate and every
// one of them changes which half of the spectrum is coded.
func HighFreqMultForTest(c Config) (float64, bool) { return c.highFreqMult() }

// CoefBookPairForTest is the coefficient book pair, likewise checked directly:
// the raw-versus-normalised rate distinction is load-bearing at exactly 32 kHz
// on v2 and invisible in a decode that happens to agree anyway.
func CoefBookPairForTest(c Config) int { return c.coefBookPair() }

// FrameBitsForTest is where the frame a non-reservoir packet carried ended, in
// bits from the start of that packet. It is what lets a test re-frame a real
// stream's frames into a bit-reservoir stream, which is the only way to
// exercise the superframe walk: neither ffmpeg encoder ever sets the reservoir
// bit, so no corpus can contain one.
func FrameBitsForTest(d *Decoder) int { return d.r.pos }

// OffsetBitsForTest is the width of the superframe header's bit-offset field,
// less the fixed three, which a re-framer has to agree with the decoder about.
func OffsetBitsForTest(c Config) (int, error) { return c.offsetBits() }
