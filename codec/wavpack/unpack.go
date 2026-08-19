package wavpack

import (
	"encoding/binary"
	"math/bits"
)

// Decorrelation. Each block carries a cascade of up to 16 passes, applied in
// order; the encoder ran them in reverse, so the metadata stores them in the
// reverse of the order used here. A pass predicts each sample from one earlier
// one (term 1 to 8 selects the lag), from a two-tap extrapolation (terms 17 and
// 18), or from the other channel at the same instant (terms -1 to -3), adds the
// residual, and nudges its weight toward whichever sign the prediction should
// have had. The weight adaptation is the whole point: no weights are
// transmitted per sample, only their starting values.

// decorrPass is one pass of the cascade.
type decorrPass struct {
	term     int
	delta    int32
	weightA  int32
	weightB  int32
	samplesA [maxTerm]int32
	samplesB [maxTerm]int32
}

// applyWeight scales a sample by a weight in 1/1024ths. The reference keeps
// two forms and picks by magnitude: the wide one avoids a 32-bit overflow that
// the narrow one cannot hit, and they do not round identically, so which one
// runs is part of the bitstream contract.
func applyWeight(weight, sample int32) int32 {
	if sample != int32(int16(sample)) {
		return ((((sample & 0xffff) * weight) >> 9) + (((sample &^ 0xffff) >> 9) * weight) + 1) >> 1
	}
	return (weight*sample + 512) >> 10
}

// updateWeight moves a weight one delta toward the sign the prediction wanted.
// A zero on either side leaves it alone, which is what keeps silence from
// walking the weights.
func updateWeight(weight *int32, delta, source, result int32) {
	if source != 0 && result != 0 {
		s := (source ^ result) >> 31
		*weight = (delta ^ s) + (*weight - s)
	}
}

// updateWeightClip is updateWeight for the cross-channel terms, whose weights
// are clamped: those passes can otherwise run away on correlated channels.
func updateWeightClip(weight *int32, delta, source, result int32) {
	if source != 0 && result != 0 {
		s := (source ^ result) >> 31
		w := (*weight ^ s) + (delta - s)
		if w > 1024 {
			w = 1024
		}
		*weight = (w ^ s) - s
	}
}

// decorrMonoPass runs one pass over a mono block, in place.
//
// The reference normalizes the sample history afterward for terms 1 to 8, so
// a pass can be resumed over the rest of the same block; nothing here splits a
// pass, and every block reloads its history, so that step is omitted.
func decorrMonoPass(dpp *decorrPass, buf []int32) {
	delta, weight := dpp.delta, dpp.weightA
	switch dpp.term {
	case 17:
		for i := range buf {
			sam := 2*dpp.samplesA[0] - dpp.samplesA[1]
			dpp.samplesA[1] = dpp.samplesA[0]
			dpp.samplesA[0] = applyWeight(weight, sam) + buf[i]
			updateWeight(&weight, delta, sam, buf[i])
			buf[i] = dpp.samplesA[0]
		}
	// The reference spells term 18 differently in the mono and stereo passes,
	// (3a-b)>>1 here against a+((a-b)>>1) there. The two agree in infinite
	// precision but not once 3a wraps int32, so each pass keeps the form its
	// own encoder half used. This is not a typo to unify.
	case 18:
		for i := range buf {
			sam := (3*dpp.samplesA[0] - dpp.samplesA[1]) >> 1
			dpp.samplesA[1] = dpp.samplesA[0]
			dpp.samplesA[0] = applyWeight(weight, sam) + buf[i]
			updateWeight(&weight, delta, sam, buf[i])
			buf[i] = dpp.samplesA[0]
		}
	default:
		m, k := 0, dpp.term&(maxTerm-1)
		for i := range buf {
			sam := dpp.samplesA[m]
			dpp.samplesA[k] = applyWeight(weight, sam) + buf[i]
			updateWeight(&weight, delta, sam, buf[i])
			buf[i] = dpp.samplesA[k]
			m = (m + 1) & (maxTerm - 1)
			k = (k + 1) & (maxTerm - 1)
		}
	}
	dpp.weightA = weight
}

// decorrStereoPass runs one pass over an interleaved stereo block, in place.
func decorrStereoPass(dpp *decorrPass, buf []int32) {
	switch dpp.term {
	case 17:
		for i := 0; i < len(buf); i += 2 {
			sam := 2*dpp.samplesA[0] - dpp.samplesA[1]
			dpp.samplesA[1] = dpp.samplesA[0]
			tmp := buf[i]
			dpp.samplesA[0] = applyWeight(dpp.weightA, sam) + tmp
			buf[i] = dpp.samplesA[0]
			updateWeight(&dpp.weightA, dpp.delta, sam, tmp)

			sam = 2*dpp.samplesB[0] - dpp.samplesB[1]
			dpp.samplesB[1] = dpp.samplesB[0]
			tmp = buf[i+1]
			dpp.samplesB[0] = applyWeight(dpp.weightB, sam) + tmp
			buf[i+1] = dpp.samplesB[0]
			updateWeight(&dpp.weightB, dpp.delta, sam, tmp)
		}
	case 18:
		for i := 0; i < len(buf); i += 2 {
			sam := dpp.samplesA[0] + ((dpp.samplesA[0] - dpp.samplesA[1]) >> 1)
			dpp.samplesA[1] = dpp.samplesA[0]
			tmp := buf[i]
			dpp.samplesA[0] = applyWeight(dpp.weightA, sam) + tmp
			buf[i] = dpp.samplesA[0]
			updateWeight(&dpp.weightA, dpp.delta, sam, tmp)

			sam = dpp.samplesB[0] + ((dpp.samplesB[0] - dpp.samplesB[1]) >> 1)
			dpp.samplesB[1] = dpp.samplesB[0]
			tmp = buf[i+1]
			dpp.samplesB[0] = applyWeight(dpp.weightB, sam) + tmp
			buf[i+1] = dpp.samplesB[0]
			updateWeight(&dpp.weightB, dpp.delta, sam, tmp)
		}
	case -1:
		for i := 0; i < len(buf); i += 2 {
			sam := buf[i] + applyWeight(dpp.weightA, dpp.samplesA[0])
			updateWeightClip(&dpp.weightA, dpp.delta, dpp.samplesA[0], buf[i])
			buf[i] = sam
			dpp.samplesA[0] = buf[i+1] + applyWeight(dpp.weightB, sam)
			updateWeightClip(&dpp.weightB, dpp.delta, sam, buf[i+1])
			buf[i+1] = dpp.samplesA[0]
		}
	case -2:
		for i := 0; i < len(buf); i += 2 {
			sam := buf[i+1] + applyWeight(dpp.weightB, dpp.samplesB[0])
			updateWeightClip(&dpp.weightB, dpp.delta, dpp.samplesB[0], buf[i+1])
			buf[i+1] = sam
			dpp.samplesB[0] = buf[i] + applyWeight(dpp.weightA, sam)
			updateWeightClip(&dpp.weightA, dpp.delta, sam, buf[i])
			buf[i] = dpp.samplesB[0]
		}
	case -3:
		for i := 0; i < len(buf); i += 2 {
			samA := buf[i] + applyWeight(dpp.weightA, dpp.samplesA[0])
			updateWeightClip(&dpp.weightA, dpp.delta, dpp.samplesA[0], buf[i])
			samB := buf[i+1] + applyWeight(dpp.weightB, dpp.samplesB[0])
			updateWeightClip(&dpp.weightB, dpp.delta, dpp.samplesB[0], buf[i+1])
			buf[i] = samA
			buf[i+1] = samB
			dpp.samplesB[0] = samA
			dpp.samplesA[0] = samB
		}
	default:
		m, k := 0, dpp.term&(maxTerm-1)
		for i := 0; i < len(buf); i += 2 {
			sam := dpp.samplesA[m]
			dpp.samplesA[k] = applyWeight(dpp.weightA, sam) + buf[i]
			updateWeight(&dpp.weightA, dpp.delta, sam, buf[i])
			buf[i] = dpp.samplesA[k]

			sam = dpp.samplesB[m]
			dpp.samplesB[k] = applyWeight(dpp.weightB, sam) + buf[i+1]
			updateWeight(&dpp.weightB, dpp.delta, sam, buf[i+1])
			buf[i+1] = dpp.samplesB[k]

			m = (m + 1) & (maxTerm - 1)
			k = (k + 1) & (maxTerm - 1)
		}
	}
}

// blockState holds everything one block's metadata sub-blocks configure. It
// is reused across blocks; every field is rewritten from the block being
// decoded, so nothing carries over.
type blockState struct {
	h     BlockHeader
	terms [maxTerms]decorrPass
	nterm int
	w     wordCoder
	wv    bitReader
	wvx   bitReader

	// The extended-integer fields describe how samples wider than the coder
	// handles were narrowed: sent bits ride in the wvx stream, and the zero,
	// one, and duplicate counts are redundant low bits to synthesize.
	int32Sent, int32Zeros, int32Ones, int32Dups uint
	int32MaxWidth                               uint
	crcWVX                                      uint32
}

// unpackBlock decodes one whole block into out, which must hold
// h.BlockSamples frames for the block's channel count, interleaved. The header
// is the caller's already-validated parse of the same bytes. It returns the
// number of frames written.
func (s *blockState) unpackBlock(h BlockHeader, block []byte, out []int32) (int, error) {
	if h.Size > int64(len(block)) {
		return 0, malformed("block declares %d bytes but only %d are present", h.Size, len(block))
	}
	*s = blockState{h: h}
	if err := s.readMetadata(block[:h.Size]); err != nil {
		return 0, err
	}
	if !s.wv.open() {
		return 0, malformed("block has no wv bitstream")
	}

	n := h.BlockSamples
	mono := h.Mono()
	span := n
	if !mono {
		span *= 2
	}
	buf := out[:span]
	if got := s.w.getWordsLossless(&s.wv, buf, n, mono); got != n {
		return 0, malformed("bitstream ends after %d of %d samples", got, n)
	}
	// A truncated bitstream usually decodes a full count of garbage rather
	// than stopping short (reads past the end return zero bits, which are a
	// legal code), so the overrun latch is what names it. The CRC below would
	// catch it too, less usefully.
	if s.wv.over {
		return 0, malformed("block at sample %d reads past the end of its bitstream", h.BlockIndex)
	}

	crc := uint32(0xffffffff)
	if mono {
		for i := range s.terms[:s.nterm] {
			decorrMonoPass(&s.terms[i], buf)
		}
		for _, v := range buf {
			crc = crcMono(crc, v)
		}
	} else {
		for i := range s.terms[:s.nterm] {
			decorrStereoPass(&s.terms[i], buf)
		}
		joint := h.Flags&flagJointStereo != 0
		for i := 0; i < span; i += 2 {
			if joint {
				buf[i+1] -= buf[i] >> 1
				buf[i] += buf[i+1]
			}
			crc = crcStereo(crc, buf[i], buf[i+1])
		}
	}
	if err := s.fixup(buf); err != nil {
		return 0, err
	}
	if crc != h.CRC {
		return 0, malformed("block at sample %d fails its CRC", h.BlockIndex)
	}
	// A false-stereo block codes one channel and says both are equal; expand
	// it in place, back to front so the source is never overwritten first.
	if h.Flags&flagFalseStereo != 0 {
		for i := n - 1; i >= 0; i-- {
			out[i*2], out[i*2+1] = buf[i], buf[i]
		}
	}
	return n, nil
}

// readMetadata walks the block's sub-blocks and applies the ones that
// configure decoding. Unknown and optional ids are skipped, which is what
// lets a stream carry an encoder signature or a RIFF wrapper we ignore.
func (s *blockState) readMetadata(block []byte) error {
	mono := s.h.Mono()
	return walkMeta(block, func(id byte, data []byte) error {
		switch id {
		case idDecorrTerms:
			return s.readDecorrTerms(data, mono)
		case idDecorrWeights:
			return s.readDecorrWeights(data, mono)
		case idDecorrSamples:
			return s.readDecorrSamples(data, mono)
		case idEntropyVars:
			return s.readEntropyVars(data, mono)
		case idInt32Info:
			if len(data) != 4 {
				return malformed("int32 info of %d bytes, want 4", len(data))
			}
			// Masked, not range-checked, because the reference masks these
			// same fields at every use: a value it would fold to a small
			// count has to fold the same way here or the two decoders part
			// company on a stream neither should accept. The block CRC is
			// what rejects the result.
			s.int32Sent = uint(data[0]) & 0x1f
			s.int32Zeros = uint(data[1]) & 0x1f
			s.int32Ones = uint(data[2]) & 0x1f
			s.int32Dups = uint(data[3]) & 0x1f
		case idWVBitstream:
			if len(data) == 0 || len(data)&1 != 0 {
				return malformed("wv bitstream of %d bytes", len(data))
			}
			s.wv.reset(data)
		case idWVXBitstream, idWVXNew:
			if len(data) <= 4 || len(data)&1 != 0 {
				return malformed("wvx bitstream of %d bytes", len(data))
			}
			s.crcWVX = binary.LittleEndian.Uint32(data)
			s.wvx.reset(data[4:])
			if id == idWVXNew {
				s.int32MaxWidth = uint(s.wvx.getBits(5)) & 0x1f
			}
		}
		return nil
	})
}

func (s *blockState) readDecorrTerms(data []byte, mono bool) error {
	if len(data) > maxTerms {
		return malformed("%d decorrelation terms exceeds %d", len(data), maxTerms)
	}
	s.nterm = len(data)
	for i, b := range data {
		dpp := &s.terms[s.nterm-1-i]
		dpp.term = int(b&0x1f) - 5
		dpp.delta = int32(b>>5) & 0x7
		if dpp.term == 0 || dpp.term < -3 || (dpp.term > maxTerm && dpp.term < 17) || dpp.term > 18 ||
			(mono && dpp.term < 0) {
			return malformed("decorrelation term %d", dpp.term)
		}
	}
	return nil
}

func (s *blockState) readDecorrWeights(data []byte, mono bool) error {
	n := len(data)
	if !mono {
		n /= 2
	}
	if n > s.nterm {
		return malformed("%d decorrelation weights for %d terms", n, s.nterm)
	}
	p := 0
	for j := 0; j < n; j++ {
		dpp := &s.terms[s.nterm-1-j]
		dpp.weightA = restoreWeight(int8(data[p]))
		p++
		if !mono {
			dpp.weightB = restoreWeight(int8(data[p]))
			p++
		}
	}
	return nil
}

func (s *blockState) readDecorrSamples(data []byte, mono bool) error {
	p := 0
	log := func() int32 {
		v := exp2s(int(int16(binary.LittleEndian.Uint16(data[p:]))))
		p += 2
		return v
	}
	for j := 0; j < s.nterm && p < len(data); j++ {
		dpp := &s.terms[s.nterm-1-j]
		switch {
		case dpp.term > maxTerm:
			// The two-tap terms keep two samples of history per channel.
			need := 4
			if !mono {
				need = 8
			}
			if p+need > len(data) {
				return malformed("decorrelation samples truncated")
			}
			dpp.samplesA[0], dpp.samplesA[1] = log(), log()
			if !mono {
				dpp.samplesB[0], dpp.samplesB[1] = log(), log()
			}
		case dpp.term < 0:
			// The cross-channel terms keep one sample of each channel.
			if p+4 > len(data) {
				return malformed("decorrelation samples truncated")
			}
			dpp.samplesA[0], dpp.samplesB[0] = log(), log()
		default:
			for m := 0; m < dpp.term; m++ {
				need := 2
				if !mono {
					need = 4
				}
				if p+need > len(data) {
					return malformed("decorrelation samples truncated")
				}
				dpp.samplesA[m] = log()
				if !mono {
					dpp.samplesB[m] = log()
				}
			}
		}
	}
	if p != len(data) {
		return malformed("%d unread decorrelation sample bytes", len(data)-p)
	}
	return nil
}

func (s *blockState) readEntropyVars(data []byte, mono bool) error {
	want := 12
	if mono {
		want = 6
	}
	if len(data) != want {
		return malformed("entropy vars of %d bytes, want %d", len(data), want)
	}
	chans := 1
	if !mono {
		chans = 2
	}
	for ch := 0; ch < chans; ch++ {
		for m := 0; m < 3; m++ {
			raw := binary.LittleEndian.Uint16(data[(ch*3+m)*2:])
			s.w.c[ch].median[m] = uint32(exp2s(int(raw)))
		}
	}
	return nil
}

// fixup applies the two post-decorrelation restorations: the extended-integer
// bits that did not fit the coder, then the constant left shift an encoder
// stripped from every sample.
func (s *blockState) fixup(buf []int32) error {
	shift := s.h.shift()
	if s.h.Flags&flagInt32Data != 0 {
		sent, zeros, ones, dups := s.int32Sent, s.int32Zeros, s.int32Ones, s.int32Dups
		switch {
		case s.wvx.open():
			crc := uint32(0xffffffff)
			for i, v := range buf {
				if sent != 0 {
					v = s.sendBits(v, sent)
				}
				v = synthesizeLowBits(v, zeros, ones, dups)
				buf[i] = v
				crc = crcExtension(crc, v)
			}
			if crc != s.crcWVX {
				return malformed("block at sample %d fails its extension CRC", s.h.BlockIndex)
			}
		case sent == 0 && zeros+ones+dups != 0:
			for i, v := range buf {
				buf[i] = synthesizeLowBits(v, zeros, ones, dups)
			}
		default:
			// No extension stream and nothing to synthesize: the missing bits
			// were all zeros, so one wider shift restores them.
			shift += zeros + sent + ones + dups
		}
	}
	if shift &= 0x1f; shift != 0 {
		for i, v := range buf {
			buf[i] = int32(uint32(v) << shift)
		}
	}
	return nil
}

// sendBits appends the low bits held back in the wvx stream. When the encoder
// declared a maximum width, samples that would exceed it carry fewer bits and
// the rest are padded with zeros, so the read width depends on the sample.
//
// A zero width means no width was declared, which is the reference's own
// sentinel: only ID_WVX_NEW_BITSTREAM carries the field, so an absent
// sub-block and a declared zero are the same state to both decoders. No file
// in the pinned suite writes that sub-block, so the width-limited branch is
// unexercised there; a mistake in it fails the extension CRC rather than
// passing silently.
func (s *blockState) sendBits(v int32, sent uint) int32 {
	if s.int32MaxWidth == 0 {
		return int32(uint32(v)<<sent | s.wvx.getBits(int(sent)))
	}
	pos := v
	if pos < 0 {
		pos = ^pos
	}
	width := uint(bits.Len32(uint32(pos))) + sent
	read := sent
	if width > s.int32MaxWidth {
		if width-s.int32MaxWidth >= sent {
			return int32(uint32(v) << sent)
		}
		read = sent - (width - s.int32MaxWidth)
	}
	return int32((uint32(v)<<read | s.wvx.getBits(int(read))) << (sent - read))
}

// synthesizeLowBits restores low bits an encoder found redundant: a run of
// zeros, a run of ones, or a run duplicating the sample's own low bit.
func synthesizeLowBits(v int32, zeros, ones, dups uint) int32 {
	switch {
	case zeros != 0:
		return int32(uint32(v) << zeros)
	case ones != 0:
		return int32(uint32(v+1)<<ones - 1)
	case dups != 0:
		low := v & 1
		return int32(uint32(v+low)<<dups - uint32(low))
	}
	return v
}
