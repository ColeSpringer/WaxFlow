package aac

// SBR bitstream writing (the encoder-side mirror of sbr_bitstream.go): the
// fill-element payload carrying sbr_header and sbr_data. The huffman
// encode tables are derived from the decode trees at init, so the two
// directions cannot drift; serializeSBRData recomputes the same deltas the
// decision phase costed, and the round-trip test parses every written
// payload back through the decoder's parser.

// sbrHuffEncTable maps a delta value to its code, indexed value+64 (the
// decode trees' leaves are value+64 over int8, so the value domain is
// [-64, 63] and the offset must cover its full lower edge, not just the
// deltas today's tables carry).
type sbrHuffEncTable struct {
	code [128]uint32
	bits [128]uint8
	max  int // largest |delta| the table codes (balance tables: even only)
}

func (t *sbrHuffEncTable) write(w *bitWriter, d int32) {
	w.writeBits(uint(t.bits[d+64]), uint64(t.code[d+64]))
}

// buildHuffEnc walks a binary-tree pair table into its encode table.
func buildHuffEnc(tab [][2]int8) (enc sbrHuffEncTable) {
	var walk func(row int, code uint32, bits uint8)
	walk = func(row int, code uint32, bits uint8) {
		for b := 0; b < 2; b++ {
			v := tab[row][b]
			c, n := code<<1|uint32(b), bits+1
			if v < 0 {
				d := int(v) + 64
				enc.code[d+64] = c
				enc.bits[d+64] = n
				if d > enc.max {
					enc.max = d
				}
				if -d > enc.max {
					enc.max = -d
				}
				continue
			}
			walk(int(v), c, n)
		}
	}
	walk(0, 0, 0)
	return enc
}

var (
	sbrEncTEnv15     = buildHuffEnc(sbrHuffTEnv15[:])
	sbrEncFEnv15     = buildHuffEnc(sbrHuffFEnv15[:])
	sbrEncTEnvBal15  = buildHuffEnc(sbrHuffTEnvBal15[:])
	sbrEncFEnvBal15  = buildHuffEnc(sbrHuffFEnvBal15[:])
	sbrEncTEnv30     = buildHuffEnc(sbrHuffTEnv30[:])
	sbrEncFEnv30     = buildHuffEnc(sbrHuffFEnv30[:])
	sbrEncTEnvBal30  = buildHuffEnc(sbrHuffTEnvBal30[:])
	sbrEncFEnvBal30  = buildHuffEnc(sbrHuffFEnvBal30[:])
	sbrEncTNoise30   = buildHuffEnc(sbrHuffTNoise30[:])
	sbrEncTNoiseBal3 = buildHuffEnc(sbrHuffTNoiseBal30[:])
)

// sbrEnvTables resolves the encode tables and start-value width for a
// channel's envelope run, the writer-side twin of parseEnvelope's table
// pick. mul is the balance doubling on the start value.
func sbrEnvTables(ampRes int, balance bool) (tTab, fTab *sbrHuffEncTable, startBits uint, mul int32) {
	mul = 1
	if balance {
		mul = 2
	}
	if ampRes == 1 {
		if balance {
			return &sbrEncTEnvBal30, &sbrEncFEnvBal30, 5, mul
		}
		return &sbrEncTEnv30, &sbrEncFEnv30, 6, mul
	}
	if balance {
		return &sbrEncTEnvBal15, &sbrEncFEnvBal15, 6, mul
	}
	return &sbrEncTEnv15, &sbrEncFEnv15, 7, mul
}

// sbrNoiseTables is the noise twin: floors share the 3 dB envelope tables
// in frequency and their own in time.
func sbrNoiseTables(balance bool) (tTab, fTab *sbrHuffEncTable, mul int32) {
	if balance {
		return &sbrEncTNoiseBal3, &sbrEncFEnvBal30, 2
	}
	return &sbrEncTNoise30, &sbrEncFEnv30, 1
}

// writeSBRHeader emits sbr_header with every extra field at its default
// (the parser's defaults are the values this encoder wants, so both extra
// blocks stay absent).
func writeSBRHeader(w *bitWriter, h sbrHeader) {
	w.writeBits(1, uint64(h.ampRes))
	w.writeBits(4, uint64(h.startFreq))
	w.writeBits(4, uint64(h.stopFreq))
	w.writeBits(3, uint64(h.xoverBand))
	w.writeBits(2, 0) // bs_reserved
	w.writeBits(1, 0) // bs_header_extra_1
	w.writeBits(1, 0) // bs_header_extra_2
}

// writeSBRGrid emits sbr_grid for the two classes this encoder produces.
// A FIXVAR grid's pointer is derived back from lA (the parse direction
// inverted); freqRes is emitted in the reversed order the parser reads.
func writeSBRGrid(w *bitWriter, g *sbrGrid) {
	w.writeBits(2, uint64(g.frameClass))
	switch g.frameClass {
	case fixfix:
		tmp := 0
		for 1<<tmp < g.numEnv {
			tmp++
		}
		w.writeBits(2, uint64(tmp))
		w.writeBits(1, uint64(g.freqRes[0]))
	case fixvar:
		w.writeBits(2, uint64(g.tE[g.numEnv]-16))
		w.writeBits(2, uint64(g.numEnv-1))
		for i := 0; i < g.numEnv-1; i++ {
			width := g.tE[g.numEnv-i] - g.tE[g.numEnv-1-i]
			w.writeBits(2, uint64((width-2)/2))
		}
		pointer := 0
		if g.lA >= 0 {
			pointer = g.numEnv + 1 - g.lA
		}
		w.writeBits(ceilLog2(g.numEnv+1), uint64(pointer))
		for i := 0; i < g.numEnv; i++ {
			w.writeBits(1, uint64(g.freqRes[g.numEnv-1-i]))
		}
	}
}

// writeSBRDtdf emits the per-envelope and per-noise-floor delta-coding
// direction flags.
func writeSBRDtdf(w *bitWriter, fd *sbrFrameData) {
	for l := 0; l < fd.grid.numEnv; l++ {
		v := uint64(0)
		if fd.dfEnv[l] {
			v = 1
		}
		w.writeBits(1, v)
	}
	for l := 0; l < fd.grid.numNoise; l++ {
		v := uint64(0)
		if fd.dfNoise[l] {
			v = 1
		}
		w.writeBits(1, v)
	}
}

// writeSBREnvelope emits sbr_envelope, advancing the caller's delta-time
// anchors exactly as parseEnvelope does. Every value was clamped
// representable by the decision phase, so the table lookups cannot miss.
func writeSBREnvelope(w *bitWriter, t *sbrFreqTables, fd *sbrFrameData,
	prevEnv *[64]int32, prevRes *int, balance bool) {
	tTab, fTab, startBits, mul := sbrEnvTables(fd.ampRes, balance)
	for l := 0; l < fd.grid.numEnv; l++ {
		n := envBands(t, fd.grid.freqRes[l])
		if !fd.dfEnv[l] {
			w.writeBits(startBits, uint64(fd.env[l][0]/mul))
			for b := 1; b < n; b++ {
				fTab.write(w, fd.env[l][b]-fd.env[l][b-1])
			}
		} else {
			ref := prevEnv
			refRes := *prevRes
			if l > 0 {
				ref = &fd.env[l-1]
				refRes = fd.grid.freqRes[l-1]
			}
			for b := 0; b < n; b++ {
				pb := mapPrevBand(t, fd.grid.freqRes[l], refRes, b)
				tTab.write(w, fd.env[l][b]-ref[pb])
			}
		}
	}
	*prevEnv = fd.env[fd.grid.numEnv-1]
	*prevRes = fd.grid.freqRes[fd.grid.numEnv-1]
}

// writeSBRNoise emits sbr_noise, advancing the noise anchors.
func writeSBRNoise(w *bitWriter, t *sbrFreqTables, fd *sbrFrameData,
	prevNoise *[5]int32, balance bool) {
	tTab, fTab, mul := sbrNoiseTables(balance)
	for l := 0; l < fd.grid.numNoise; l++ {
		if !fd.dfNoise[l] {
			w.writeBits(5, uint64(fd.noise[l][0]/mul))
			for b := 1; b < t.nQ; b++ {
				fTab.write(w, fd.noise[l][b]-fd.noise[l][b-1])
			}
		} else {
			ref := prevNoise
			if l > 0 {
				ref = &fd.noise[l-1]
			}
			for b := 0; b < t.nQ; b++ {
				tTab.write(w, fd.noise[l][b]-ref[b])
			}
		}
	}
	*prevNoise = fd.noise[fd.grid.numNoise-1]
}

// writeSBRInvf emits the per-noise-band inverse filtering levels.
func writeSBRInvf(w *bitWriter, t *sbrFreqTables, fd *sbrFrameData) {
	for b := 0; b < t.nQ; b++ {
		w.writeBits(2, uint64(fd.invf[b]))
	}
}

// writeSBRAddHarmonic emits bs_add_harmonic_flag and the per-band markers.
func writeSBRAddHarmonic(w *bitWriter, t *sbrFreqTables, fd *sbrFrameData) {
	any := false
	for b := 0; b < t.nHigh; b++ {
		any = any || fd.addHarmonic[b]
	}
	if !any {
		w.writeBits(1, 0)
		return
	}
	w.writeBits(1, 1)
	for b := 0; b < t.nHigh; b++ {
		v := uint64(0)
		if fd.addHarmonic[b] {
			v = 1
		}
		w.writeBits(1, v)
	}
}

// sbrEncAnchors is one channel's writer-side delta-time anchor mirror,
// the counterpart of the decoder's sbrChanState anchors. The encoder
// advances it once per written frame and the decision phase reads it to
// cost and clamp time-coded candidates.
type sbrEncAnchors struct {
	prevEnv   [64]int32
	prevRes   int
	prevNoise [5]int32
}

// serializeSBRData emits sbr_single_channel_element or
// sbr_channel_pair_element (always coupled) into w: everything between
// the header flag and the extension padding.
func serializeSBRData(w *bitWriter, t *sbrFreqTables, chans int,
	fd *[2]sbrFrameData, anch *[2]sbrEncAnchors) {
	if chans == 1 {
		w.writeBits(1, 0) // bs_data_extra
		writeSBRGrid(w, &fd[0].grid)
		writeSBRDtdf(w, &fd[0])
		writeSBRInvf(w, t, &fd[0])
		writeSBREnvelope(w, t, &fd[0], &anch[0].prevEnv, &anch[0].prevRes, false)
		writeSBRNoise(w, t, &fd[0], &anch[0].prevNoise, false)
		writeSBRAddHarmonic(w, t, &fd[0])
		w.writeBits(1, 0) // bs_extended_data
		return
	}
	w.writeBits(1, 0) // bs_data_extra
	w.writeBits(1, 1) // bs_coupling
	writeSBRGrid(w, &fd[0].grid)
	writeSBRDtdf(w, &fd[0])
	writeSBRDtdf(w, &fd[1])
	writeSBRInvf(w, t, &fd[0])
	writeSBREnvelope(w, t, &fd[0], &anch[0].prevEnv, &anch[0].prevRes, false)
	writeSBRNoise(w, t, &fd[0], &anch[0].prevNoise, false)
	writeSBREnvelope(w, t, &fd[1], &anch[1].prevEnv, &anch[1].prevRes, true)
	writeSBRNoise(w, t, &fd[1], &anch[1].prevNoise, true)
	writeSBRAddHarmonic(w, t, &fd[0])
	writeSBRAddHarmonic(w, t, &fd[1])
	w.writeBits(1, 0) // bs_extended_data
}

// serializeSBRPayload renders one frame's whole fill-element payload
// (extension type, header when due, sbr_data) into w, from its start.
func serializeSBRPayload(w *bitWriter, hdr sbrHeader, sendHeader bool,
	t *sbrFreqTables, chans int, fd *[2]sbrFrameData, anch *[2]sbrEncAnchors) {
	w.writeBits(4, extSBRData)
	if sendHeader {
		w.writeBits(1, 1)
		writeSBRHeader(w, hdr)
	} else {
		w.writeBits(1, 0)
	}
	serializeSBRData(w, t, chans, fd, anch)
}

// maxFILPayloadBytes is the largest payload the fill element's escaped
// count can declare: 14 + 255. The worst reachable SBR payload under the
// current band derivations is ~216 bytes (measured by enumeration), so
// the ceiling only guards a future derivation change; encodeFrame drops
// the fill (one concealed frame) rather than let writeFIL's 8-bit escape
// wrap and desynchronize the whole access unit.
const maxFILPayloadBytes = 14 + 255

// writeFIL wraps a rendered payload in a fill element: the escaped byte
// count, the payload bits, and zero padding to the declared count. The
// padding stays under a byte, which is what keeps the decoder's
// extended-data consumption rule from re-entering on it.
func writeFIL(w *bitWriter, payload *bitWriter) {
	bits := payload.bitLen()
	count := (bits + 7) / 8
	w.writeBits(3, elFIL)
	if count < 15 {
		w.writeBits(4, uint64(count))
	} else {
		w.writeBits(4, 15)
		w.writeBits(8, uint64(count-14))
	}
	for _, b := range payload.buf {
		w.writeBits(8, uint64(b))
	}
	if payload.n > 0 {
		w.writeBits(payload.n, payload.cache&(1<<payload.n-1))
	}
	w.writeBits(uint(count*8-bits), 0)
}

// filBits is the whole cost of writeFIL for a payload of the given bit
// length: element id, escaped count, and the padded payload.
func filBits(payloadBits int) int {
	count := (payloadBits + 7) / 8
	head := 3 + 4
	if count >= 15 {
		head += 8
	}
	return head + count*8
}
