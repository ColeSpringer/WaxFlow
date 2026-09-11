package wmalossless

// Hooks for the tests in this package's external test file, for the two
// invariants that cannot be reached from a bitstream without an encoder.

// PoisonGolomb sets every channel's moving-average state, so a test can prove
// a seekable tile clears it.
func PoisonGolomb(d *Decoder, sum uint32, scaling uint) {
	for c := range d.ch {
		d.ch[c].g = golomb{aveSum: sum, scaling: scaling}
	}
}

// GolombState reports one channel's moving-average state.
func GolombState(d *Decoder, c int) (uint32, uint) {
	return d.ch[c].g.aveSum, d.ch[c].g.scaling
}

// PacketHeader parses a packet header, so a test can check PacketIsSync
// against the decoder's own reading of the same bits.
func PacketHeader(pkt []byte, frameSizeBits int) (seq int, seek, spliced bool, n int) {
	var r bitReader
	r.resetBits(pkt, len(pkt)*8)
	seq = int(r.bits(4))
	seek = r.bit() == 1
	spliced = r.bit() == 1
	n = int(r.bits(frameSizeBits))
	return
}
