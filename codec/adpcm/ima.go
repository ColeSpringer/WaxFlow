package adpcm

// IMA ADPCM, from the IMA Digital Audio Special Interest Group's
// "Recommended Practices for Enhancing Digital Audio Compatibility in
// Multimedia Systems", revision 3.00 (1992).
//
// The coder is one predictor and one step index per channel. A nibble's low
// three bits scale the current step into a difference, its top bit signs it,
// and the nibble then moves the index along a fixed table so the step tracks
// the signal's level.
//
// The two tables below are that specification's data, transcribed rather
// than computed: the step table is nominally a 1.1 geometric series, but its
// published values are individually rounded and no closed form reproduces
// all 89 of them.

// stepTable is the quantizer step size for each index.
var stepTable = [89]int32{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17,
	19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118,
	130, 143, 157, 173, 190, 209, 230, 253, 279, 307,
	337, 371, 408, 449, 494, 544, 598, 658, 724, 796,
	876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066,
	2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358,
	5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899,
	15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

// maxStepIndex is the last valid index into stepTable.
const maxStepIndex int32 = int32(len(stepTable)) - 1

// indexTable is how far a nibble moves the step index. The low four nibbles
// and the high four repeat, since the sign bit does not change the step.
var indexTable = [16]int32{
	-1, -1, -1, -1, 2, 4, 6, 8,
	-1, -1, -1, -1, 2, 4, 6, 8,
}

// imaState is one channel's coder state.
type imaState struct {
	predictor int32
	index     int32
}

// stepMul turns a nibble into a difference the way the WAV layout's
// reference decoder does: the magnitude bits plus a half, times the step,
// over eight.
//
// This is NOT the arithmetic the IMA document describes, and the difference
// is measurable rather than academic. Reading the specification alone would
// produce stepShiftAdd below, and on real audio that decodes 92 percent of a
// WAV-layout stream's samples wrong by up to 37 LSB: small errors that read
// like a bug somewhere else rather than like the wrong formula. Measured
// against ffmpeg 8.0.1 (adpcm_ima_wav), this form is bit-exact and the other
// is not, which is why a differential mismatch here is checked against BOTH
// forms before the rest of the decoder is doubted.
func stepMul(nib byte, step int32) int32 {
	diff := (int32(nib&7)*2 + 1) * step >> 3
	if nib&8 != 0 {
		return -diff
	}
	return diff
}

// stepShiftAdd turns a nibble into a difference the way the IMA document
// writes it, as a sum of shifted steps. It is what the QuickTime layout
// uses, measured the same way (ffmpeg 8.0.1, adpcm_ima_qt: bit-exact with
// this form, 99 percent of samples wrong by up to 127 LSB with the other).
func stepShiftAdd(nib byte, step int32) int32 {
	diff := step >> 3
	if nib&4 != 0 {
		diff += step
	}
	if nib&2 != 0 {
		diff += step >> 1
	}
	if nib&1 != 0 {
		diff += step >> 2
	}
	if nib&8 != 0 {
		return -diff
	}
	return diff
}

// next advances one channel by one nibble and returns the sample.
func (s *imaState) next(nib byte, form func(byte, int32) int32) int32 {
	s.predictor = clip16(int64(s.predictor + form(nib, stepTable[s.index])))
	s.index = clamp32(s.index+indexTable[nib], 0, maxStepIndex)
	return s.predictor
}

// decodeIMAWav decodes one WAV-layout block into per-channel output.
//
// The block is a 4-byte header per channel (a 16-bit predictor, a step index,
// and a reserved byte), then nibble data in 4-byte words taken round-robin
// across the channels, low nibble of each byte first. The header's predictor
// is also the block's first output sample, which is why a block holds an odd
// number of them. A tail too short for a whole round is padding, at every
// channel count; see Config.BlockFrames for what the reference does with it.
func (d *Decoder) decodeIMAWav(blk []byte) error {
	ch := d.cfg.Channels
	for c := range ch {
		hdr := blk[c*imaWavHeaderBytes:]
		st := &d.ima[c]
		st.predictor = int32(int16(le16(hdr)))
		st.index = int32(hdr[2])
		if st.index > maxStepIndex {
			return malformed("step index %d in a block header (max %d)", st.index, maxStepIndex)
		}
		d.scratch[c][0] = st.predictor
	}
	body := blk[imaWavHeaderBytes*ch:]
	word := 4 * ch
	n := 1 // samples written per channel so far
	for len(body) >= word {
		for c := range ch {
			// The coder state rides in locals for the four bytes: through the
			// pointer it is a load and a store per nibble, and this inner loop
			// is the whole cost of the codec.
			st := d.ima[c]
			out := d.scratch[c][n : n+8 : n+8]
			for i, b := range body[4*c : 4*c+4] {
				out[2*i] = st.next(b&0x0F, stepMul)
				out[2*i+1] = st.next(b>>4, stepMul)
			}
			d.ima[c] = st
		}
		n += 8
		body = body[word:]
	}
	return nil
}

// decodeIMAQuickTime decodes one QuickTime 'ima4' block: 34 bytes per
// channel, each holding a 2-byte big-endian header and 32 bytes of nibbles,
// low nibble first, with no header sample.
//
// The header stores only the predictor's top nine bits, and the reference
// decoder treats it as a checkpoint rather than a reload: when the step index
// matches the running one and the stored predictor is within the 127 its low
// seven bits drop, the running predictor is kept, because it is the more
// precise version of the same number. Reloading unconditionally puts every sample in the block
// up to 127 LSB from where the reference has it, and the error never decays,
// which is measurable on any real file (ffmpeg 8.0.1, adpcm_ima_qt).
//
// That is also what makes a seek into the middle of one of these streams a
// different thing from a seek into a WAV-layout one: see the demuxers, which
// restart an 'ima4' decode from the file's first block.
func (d *Decoder) decodeIMAQuickTime(blk []byte) error {
	for c := range d.cfg.Channels {
		part := blk[c*QuickTimeBlockBytes:]
		v := int32(int16(be16(part)))
		index := v & 0x7F
		predictor := v &^ 0x7F
		if index > maxStepIndex {
			return malformed("step index %d in a block header (max %d)", index, maxStepIndex)
		}
		// The state rides in a local for the block's 32 bytes, which is what
		// keeps this out of a load and a store per nibble; a QuickTime block
		// is short enough that the header work around it already shows.
		st := d.ima[c]
		if index != st.index || abs32(st.predictor-predictor) > 127 {
			st.predictor, st.index = predictor, index
		}
		out := d.scratch[c][:QuickTimeBlockFrames]
		for i, b := range part[2:QuickTimeBlockBytes] {
			out[2*i] = st.next(b&0x0F, stepShiftAdd)
			out[2*i+1] = st.next(b>>4, stepShiftAdd)
		}
		d.ima[c] = st
	}
	return nil
}
