package musepack

// frameState is everything a frame's bitstream fills in and everything the
// next frame reads back: the per-band resolutions and scalefactors, the
// mid/side flags, the quantised samples, and the two pieces of state the
// format carries across frames without ever restating them (the SV8 key-frame
// scalefactor flags and the noise generator). Both stream versions fill the
// same struct so one requantiser serves both.
type frameState struct {
	res  [2][MaxBands]int32
	scfi [2][MaxBands]int32
	scf  [2][MaxBands][3]int32
	ms   [MaxBands]bool
	q    [MaxBands][2][36]int16

	// dscfFlag marks a band whose next scalefactor is coded absolutely: every
	// band at an SV8 key frame, cleared by the first coding after it.
	dscfFlag [2][MaxBands]bool
	// lastMaxBand is the band count the previous SV8 frame coded, which the
	// next non-key frame codes a delta against.
	lastMaxBand int32

	rng prng
}

// init puts the state where the reference's setup leaves it.
func (s *frameState) init() {
	*s = frameState{rng: newPRNG()}
}

var (
	errBook   = malformed("frame carries a codeword outside its Huffman book")
	errOver   = malformed("frame reads past the end of its packet")
	errResRng = malformed("frame codes a quantiser resolution outside -1..17")
)

// scfMute is the value the SV7 reader substitutes for a scalefactor index
// past 1024, whose low byte selects the quietest table entry.
const scfMute = 0x8080

// readSV7Header reads an SV7 frame up to its samples: resolutions, mid/side
// flags, scalefactor patterns and scalefactors (mpc_decoder_read_bitstream_sv7,
// header part). It returns the band count in use.
func (s *frameState) readSV7Header(r *bitReader, cfg *Config) (int, error) {
	b := sv7Books()
	maxUsed := 0

	s.res[0][0] = int32(r.bits(4))
	s.res[1][0] = int32(r.bits(4))
	if s.res[0][0] != 0 || s.res[1][0] != 0 {
		if cfg.MS {
			s.ms[0] = r.bit() != 0
		}
		maxUsed = 1
	}
	for n := 1; n <= cfg.MaxBand; n++ {
		for ch := range 2 {
			idx, ok := b.hdr.decode(r)
			if !ok {
				return 0, errBook
			}
			if idx != 4 {
				s.res[ch][n] = s.res[ch][n-1] + int32(idx)
			} else {
				s.res[ch][n] = int32(r.bits(4))
			}
		}
		if s.res[0][n] != 0 || s.res[1][n] != 0 {
			if cfg.MS {
				s.ms[n] = r.bit() != 0
			}
			maxUsed = n + 1
		}
	}
	// The delta chain is unbounded on hostile input. The reference indexes
	// its coefficient table with whatever came out, so the range is checked
	// here instead.
	for n := 0; n < maxUsed; n++ {
		for ch := range 2 {
			if v := s.res[ch][n]; v < -1 || v > 17 {
				return 0, errResRng
			}
		}
	}

	for n := 0; n < maxUsed; n++ {
		for ch := range 2 {
			if s.res[ch][n] != 0 {
				v, ok := b.scfi.decode(r)
				if !ok {
					return 0, errBook
				}
				s.scfi[ch][n] = int32(v)
			}
		}
	}

	for n := 0; n < maxUsed; n++ {
		for ch := range 2 {
			if s.res[ch][n] == 0 {
				continue
			}
			scf := &s.scf[ch][n]
			// The first delta of a band is against that band's last coded
			// scalefactor, however many frames ago; nothing resets it.
			switch s.scfi[ch][n] {
			case 1:
				if !s.dscf7(r, b, &scf[0], scf[2]) || !s.dscf7(r, b, &scf[1], scf[0]) {
					return 0, errBook
				}
				scf[2] = scf[1]
			case 3:
				if !s.dscf7(r, b, &scf[0], scf[2]) {
					return 0, errBook
				}
				scf[1] = scf[0]
				scf[2] = scf[1]
			case 2:
				if !s.dscf7(r, b, &scf[0], scf[2]) {
					return 0, errBook
				}
				scf[1] = scf[0]
				if !s.dscf7(r, b, &scf[2], scf[1]) {
					return 0, errBook
				}
			default:
				if !s.dscf7(r, b, &scf[0], scf[2]) || !s.dscf7(r, b, &scf[1], scf[0]) || !s.dscf7(r, b, &scf[2], scf[1]) {
					return 0, errBook
				}
			}
			for i := range scf {
				if scf[i] > 1024 {
					scf[i] = scfMute
				}
			}
		}
	}
	if r.over {
		return 0, errOver
	}
	return maxUsed, nil
}

// dscf7 reads one SV7 scalefactor: a Huffman delta against prev, or the raw
// 6-bit absolute the delta book escapes to.
func (s *frameState) dscf7(r *bitReader, b *sv7BookSet, dst *int32, prev int32) bool {
	idx, ok := b.dscf.decode(r)
	if !ok {
		return false
	}
	if idx != 8 {
		*dst = prev + int32(idx)
	} else {
		*dst = int32(r.bits(6))
	}
	return true
}

// readSV7Samples reads the quantised samples of the bands in use.
func (s *frameState) readSV7Samples(r *bitReader, maxUsed int) error {
	b := sv7Books()
	for n := 0; n < maxUsed; n++ {
		for ch := range 2 {
			q := &s.q[n][ch]
			switch res := s.res[ch][n]; {
			case res < -1 || res > 17:
				// The header reader already refused these; the switch is kept
				// safe on its own terms rather than by that ordering.
				return errResRng
			case res == 0:
			case res == -1:
				for k := range q {
					q[k] = s.rng.noise()
				}
			case res == 1:
				t := b.q[0][r.bit()]
				for k := 0; k < 36; k += 3 {
					idx, ok := t.decode(r)
					if !ok {
						return errBook
					}
					q[k], q[k+1], q[k+2] = int16(idx30[idx]), int16(idx31[idx]), int16(idx32[idx])
				}
			case res == 2:
				t := b.q[1][r.bit()]
				for k := 0; k < 36; k += 2 {
					idx, ok := t.decode(r)
					if !ok {
						return errBook
					}
					q[k], q[k+1] = int16(idx50[idx]), int16(idx51[idx])
				}
			case res <= 7:
				t := b.q[res-1][r.bit()]
				for k := range q {
					v, ok := t.decode(r)
					if !ok {
						return errBook
					}
					q[k] = int16(v)
				}
			default:
				width := int(resBit[res])
				off := dc[res+1]
				for k := range q {
					q[k] = int16(int32(r.bits(width)) - int32(off))
				}
			}
		}
	}
	if r.over {
		return errOver
	}
	return nil
}

// readSV8 reads one SV8 frame (mpc_decoder_read_bitstream_sv8). A key frame
// codes its band count absolutely and re-arms every band's absolute
// scalefactor; a later frame in the block codes deltas against what the
// block has coded so far.
func (s *frameState) readSV8(r *bitReader, cfg *Config, key bool) error {
	b := sv8Books()

	var maxUsed int32
	if key {
		maxUsed = int32(r.logDec(cfg.MaxBand + 1))
	} else {
		v, ok := b.bands.decode(r)
		if !ok {
			return errBook
		}
		maxUsed = s.lastMaxBand + int32(v)
		if maxUsed > 32 {
			maxUsed -= 33
		}
	}
	s.lastMaxBand = maxUsed

	if maxUsed > 0 {
		for ch := range 2 {
			v, ok := b.res[0].decode(r)
			if !ok {
				return errBook
			}
			s.res[ch][maxUsed-1] = int32(v)
			if s.res[ch][maxUsed-1] > 15 {
				s.res[ch][maxUsed-1] -= 17
			}
		}
		for n := maxUsed - 2; n >= 0; n-- {
			for ch := range 2 {
				t := b.res[0]
				if s.res[ch][n+1] > 2 {
					t = b.res[1]
				}
				v, ok := t.decode(r)
				if !ok {
					return errBook
				}
				s.res[ch][n] = int32(v) + s.res[ch][n+1]
				if s.res[ch][n] > 15 {
					s.res[ch][n] -= 17
				}
			}
		}
		if cfg.MS {
			tot := 0
			for n := int32(0); n < maxUsed; n++ {
				if s.res[0][n] != 0 || s.res[1][n] != 0 {
					tot++
				}
			}
			cnt := r.logDec(tot)
			var flags uint32
			if cnt != 0 && cnt != tot {
				flags = r.enumDec(min(cnt, tot-cnt), tot)
			}
			if cnt*2 > tot {
				flags = ^flags
			}
			for n := maxUsed - 1; n >= 0; n-- {
				if s.res[0][n] != 0 || s.res[1][n] != 0 {
					s.ms[n] = flags&1 != 0
					flags >>= 1
				}
			}
		}
	}
	for n := maxUsed; n <= int32(cfg.MaxBand); n++ {
		s.res[0][n], s.res[1][n] = 0, 0
	}

	if key {
		for n := range MaxBands {
			s.dscfFlag[0][n], s.dscfFlag[1][n] = true, true
		}
	}
	for n := int32(0); n < maxUsed; n++ {
		cnt := -1
		if s.res[0][n] != 0 {
			cnt++
		}
		if s.res[1][n] != 0 {
			cnt++
		}
		if cnt < 0 {
			continue
		}
		v, ok := b.scfi[cnt].decode(r)
		if !ok {
			return errBook
		}
		if s.res[0][n] != 0 {
			s.scfi[0][n] = int32(v) >> (2 * cnt)
		}
		if s.res[1][n] != 0 {
			s.scfi[1][n] = int32(v) & 3
		}
	}

	for n := int32(0); n < maxUsed; n++ {
		for ch := range 2 {
			if s.res[ch][n] == 0 {
				continue
			}
			scf := &s.scf[ch][n]
			if s.dscfFlag[ch][n] {
				scf[0] = int32(r.bits(7)) - 6
				s.dscfFlag[ch][n] = false
			} else {
				v, ok := b.dscf[1].decode(r)
				if !ok {
					return errBook
				}
				d := int32(v)
				if d == 64 {
					d += int32(r.bits(6))
				}
				scf[0] = ((scf[2] - 25 + d) & 127) - 6
			}
			for m := 0; m < 2; m++ {
				if (s.scfi[ch][n]<<m)&2 == 0 {
					v, ok := b.dscf[0].decode(r)
					if !ok {
						return errBook
					}
					d := int32(v)
					if d == 31 {
						d = 64 + int32(r.bits(6))
					}
					scf[m+1] = ((scf[m] - 25 + d) & 127) - 6
				} else {
					scf[m+1] = scf[m]
				}
			}
		}
	}

	for n := int32(0); n < maxUsed; n++ {
		for ch := range 2 {
			q := &s.q[n][ch]
			switch res := s.res[ch][n]; {
			case res == 0:
			case res == 2:
				idx := 2 * thres[2]
				for k := 0; k < 36; k += 3 {
					t := b.q[0][0]
					if idx > thres[2] {
						t = b.q[0][1]
					}
					v, ok := t.decode(r)
					if !ok {
						return errBook
					}
					q[k], q[k+1], q[k+2] = int16(idx8_50[v]), int16(idx8_51[v]), int16(idx8_52[v])
					idx = idx>>1 + huffQ2Var[v]
				}
			case res == 1:
				for k := 0; k < 36; k += 18 {
					cnt, ok := b.q1.decode(r)
					if !ok {
						return errBook
					}
					var set uint32
					if cnt > 0 && cnt < 18 {
						set = r.enumDec(min(cnt, 18-cnt), 18)
					}
					if cnt > 9 {
						set = ^set
					}
					for i := k; i < k+18; i++ {
						q[i] = 0
						if set&(1<<17) != 0 {
							q[i] = int16(r.bit()<<1) - 1
						}
						set <<= 1
					}
				}
			case res == -1:
				for k := range q {
					q[k] = s.rng.noise()
				}
			case res <= 4:
				t := b.q[1][res-3]
				for k := 0; k < 36; k += 2 {
					v, ok := t.decode(r)
					if !ok {
						return errBook
					}
					// Two samples in one signed byte, low nibble first.
					q[k] = int16(int8(uint8(v)<<4) >> 4)
					q[k+1] = int16(int8(v) >> 4)
				}
			case res <= 8:
				pair := &b.q[res-3]
				idx := 2 * thres[res]
				for k := range q {
					t := pair[0]
					if idx > thres[res] {
						t = pair[1]
					}
					v, ok := t.decode(r)
					if !ok {
						return errBook
					}
					q[k] = int16(v)
					if v < 0 {
						v = -v
					}
					idx = idx>>1 + uint32(v)
				}
			default:
				extra := int(res) - 9
				off := dc[res+1]
				for k := range q {
					v, ok := b.q9up.decode(r)
					if !ok {
						return errBook
					}
					hi := int32(uint8(v))
					if extra > 0 {
						hi = hi<<extra | int32(r.bits(extra))
					}
					q[k] = int16(hi - int32(off))
				}
			}
		}
	}
	if r.over {
		return errOver
	}
	return nil
}
