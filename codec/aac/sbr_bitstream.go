package aac

// SBR bitstream parsing (ISO/IEC 14496-3 4.6.18.2-3): the fill-element
// extension payload carrying sbr_header and per-channel sbr_data. This file
// is the single place extension ids are routed, which is where the eSBR
// keep-out lives: only EXT_SBR_DATA and EXT_SBR_DATA_CRC are parsed, and
// every other extension type, enhanced-SBR signaling included, is skipped
// by its declared length. Any parse inconsistency drops the frame's SBR
// data (the element conceals with a zero high band) rather than failing
// the decode.

// Fill-element extension types (Table 4.51).
const (
	extSBRData    = 13
	extSBRDataCRC = 14
)

// Frame classes (Table 4.77).
const (
	fixfix = 0
	fixvar = 1
	varfix = 2
	varvar = 3
)

// sbrHeader is a parsed sbr_header with the extra-field defaults applied.
type sbrHeader struct {
	ampRes        int
	startFreq     int
	stopFreq      int
	xoverBand     int
	freqScale     int
	alterScale    int
	noiseBands    int
	limiterBands  int
	limiterGains  int
	interpolFreq  int
	smoothingMode int
}

// freqFieldsEqual reports whether two headers derive the same band tables,
// which is what decides an SBR reset on a header change.
func (h sbrHeader) freqFieldsEqual(o sbrHeader) bool {
	return h.startFreq == o.startFreq && h.stopFreq == o.stopFreq &&
		h.xoverBand == o.xoverBand && h.freqScale == o.freqScale &&
		h.alterScale == o.alterScale && h.noiseBands == o.noiseBands
}

// parseSBRHeader reads an sbr_header (4.6.18.2.1).
func parseSBRHeader(r *bitReader) sbrHeader {
	h := sbrHeader{
		freqScale: 2, alterScale: 1, noiseBands: 2,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	h.ampRes = int(r.bit())
	h.startFreq = int(r.read(4))
	h.stopFreq = int(r.read(4))
	h.xoverBand = int(r.read(3))
	r.read(2) // bs_reserved
	extra1 := r.bit() != 0
	extra2 := r.bit() != 0
	if extra1 {
		h.freqScale = int(r.read(2))
		h.alterScale = int(r.bit())
		h.noiseBands = int(r.read(2))
	}
	if extra2 {
		h.limiterBands = int(r.read(2))
		h.limiterGains = int(r.read(2))
		h.interpolFreq = int(r.bit())
		h.smoothingMode = int(r.bit())
	}
	return h
}

// sbrGrid is a decoded envelope/noise time grid: borders in time slots.
type sbrGrid struct {
	frameClass int
	numEnv     int
	freqRes    [5]int
	tE         [6]int
	lA         int
	numNoise   int
	tQ         [3]int
}

// sbrFrameData is one channel's SBR payload for one frame, in the
// quantized domain; dequantization happens in the adjuster.
type sbrFrameData struct {
	grid        sbrGrid
	ampRes      int // effective resolution this frame (1 = 3 dB steps)
	dfEnv       [5]bool
	dfNoise     [2]bool
	invf        [5]int
	env         [5][64]int32
	noise       [2][5]int32
	addHarmonic [64]bool // indexed by high-resolution band
}

func ceilLog2(n int) uint {
	bits := uint(0)
	for 1<<bits < n {
		bits++
	}
	return bits
}

// parseGrid reads sbr_grid (4.6.18.3.3) and derives the border vectors.
func parseGrid(r *bitReader, g *sbrGrid) bool {
	g.frameClass = int(r.read(2))
	pointer := 0
	switch g.frameClass {
	case fixfix:
		tmp := int(r.read(2))
		if tmp == 3 {
			return false // eight envelopes is outside the profile
		}
		g.numEnv = 1 << tmp
		fr := int(r.bit())
		for i := 0; i < g.numEnv; i++ {
			g.freqRes[i] = fr
		}
		for i := 0; i <= g.numEnv; i++ {
			g.tE[i] = i * 16 / g.numEnv
		}
		g.lA = -1
	case fixvar:
		absBord := 16 + int(r.read(2))
		numRel := int(r.read(2))
		g.numEnv = numRel + 1
		g.tE[0] = 0
		g.tE[g.numEnv] = absBord
		for i := 0; i < numRel; i++ {
			w := 2*int(r.read(2)) + 2
			g.tE[g.numEnv-1-i] = g.tE[g.numEnv-i] - w
		}
		pointer = int(r.read(ceilLog2(g.numEnv + 1)))
		for i := 0; i < g.numEnv; i++ {
			g.freqRes[g.numEnv-1-i] = int(r.bit())
		}
		g.lA = -1
		if pointer > 0 {
			g.lA = g.numEnv + 1 - pointer
		}
	case varfix:
		g.tE[0] = int(r.read(2))
		numRel := int(r.read(2))
		g.numEnv = numRel + 1
		for i := 0; i < numRel; i++ {
			w := 2*int(r.read(2)) + 2
			g.tE[i+1] = g.tE[i] + w
		}
		g.tE[g.numEnv] = 16
		pointer = int(r.read(ceilLog2(g.numEnv + 1)))
		for i := 0; i < g.numEnv; i++ {
			g.freqRes[i] = int(r.bit())
		}
		g.lA = -1
		if pointer > 1 {
			g.lA = pointer - 1
		}
	case varvar:
		g.tE[0] = int(r.read(2))
		absBord1 := 16 + int(r.read(2))
		numRel0 := int(r.read(2))
		numRel1 := int(r.read(2))
		g.numEnv = numRel0 + numRel1 + 1
		if g.numEnv > 5 {
			return false
		}
		g.tE[g.numEnv] = absBord1
		for i := 0; i < numRel0; i++ {
			w := 2*int(r.read(2)) + 2
			g.tE[i+1] = g.tE[i] + w
		}
		for i := 0; i < numRel1; i++ {
			w := 2*int(r.read(2)) + 2
			g.tE[g.numEnv-1-i] = g.tE[g.numEnv-i] - w
		}
		pointer = int(r.read(ceilLog2(numRel0 + numRel1 + 2)))
		for i := 0; i < g.numEnv; i++ {
			g.freqRes[i] = int(r.bit())
		}
		g.lA = -1
		if pointer > 0 {
			g.lA = g.numEnv + 1 - pointer
		}
	}
	// One noise envelope for a single-envelope frame, two otherwise; the
	// noise borders reuse the envelope borders.
	g.numNoise = 2
	if g.numEnv == 1 {
		g.numNoise = 1
	}
	g.tQ[0] = g.tE[0]
	g.tQ[g.numNoise] = g.tE[g.numEnv]
	if g.numNoise == 2 {
		g.tQ[1] = g.tE[middleBorder(g.frameClass, pointer, g.numEnv)]
	}
	// Border sanity: ascending, inside the 16+3-slot window the buffers are
	// sized for, and the noise middle on a real border.
	if g.tE[0] < 0 || g.tE[0] > 3 || g.tE[g.numEnv] < 16 || g.tE[g.numEnv] > 19 {
		return false
	}
	for i := 0; i < g.numEnv; i++ {
		if g.tE[i+1] <= g.tE[i] {
			return false
		}
	}
	if g.numNoise == 2 && (g.tQ[1] <= g.tQ[0] || g.tQ[2] <= g.tQ[1]) {
		return false
	}
	if g.lA > g.numEnv {
		g.lA = -1
	}
	return true
}

// middleBorder picks the envelope border the two-envelope noise grid
// splits at (Table 4.78 neighborhood).
func middleBorder(frameClass, pointer, numEnv int) int {
	var mid int
	switch frameClass {
	case fixfix:
		mid = numEnv / 2
	case varfix:
		switch pointer {
		case 0:
			mid = 1
		case 1:
			mid = numEnv - 1
		default:
			mid = pointer - 1
		}
	default: // fixvar, varvar
		if pointer > 1 {
			mid = numEnv + 1 - pointer
		} else {
			mid = numEnv - 1
		}
	}
	return min(max(mid, 0), numEnv)
}

// sbrHuffDecode walks a binary-tree pair table; leaves are value+64.
func sbrHuffDecode(r *bitReader, tab [][2]int8) int {
	idx := 0
	for range 24 { // deepest spec code is under 20 bits
		v := tab[idx][r.bit()]
		if v < 0 {
			return int(v) + 64
		}
		idx = int(v)
	}
	return 0
}

// envBands returns the band count an envelope with the given resolution
// codes over.
func envBands(t *sbrFreqTables, freqRes int) int {
	if freqRes == 1 {
		return t.nHigh
	}
	return t.nLow
}

// mapPrevBand maps a band index of the current resolution onto the
// previous frame's resolution for delta-time coding: the previous band
// whose range contains the current band's lower border.
func mapPrevBand(t *sbrFreqTables, curRes, prevRes, band int) int {
	if curRes == prevRes {
		return band
	}
	var cur, prev []int
	if curRes == 1 {
		cur, prev = t.fHigh, t.fLow
	} else {
		cur, prev = t.fLow, t.fHigh
	}
	if band >= len(cur) {
		return 0
	}
	lo := cur[band]
	for j := len(prev) - 2; j >= 0; j-- {
		if prev[j] <= lo {
			return j
		}
	}
	return 0
}

// parseEnvelope reads sbr_envelope (4.6.18.3.5) for one channel into
// fd.env, resolving delta-frequency and delta-time coding against prevEnv.
// balance selects the coupled channel-1 tables and value doubling. It
// reports false when an accumulated value leaves the 7-bit range, which is
// ffmpeg's validity rule too: a runaway delta chain fails the frame rather
// than dequantizing to an energy past float32 (2^134 at the top of the
// clamped range this replaced).
func parseEnvelope(r *bitReader, t *sbrFreqTables, fd *sbrFrameData,
	prevEnv *[64]int32, prevRes *int, balance bool) bool {
	var tTab, fTab [][2]int8
	var startBits uint
	mul := int32(1)
	if balance {
		mul = 2
	}
	if fd.ampRes == 1 {
		if balance {
			tTab, fTab, startBits = sbrHuffTEnvBal30[:], sbrHuffFEnvBal30[:], 5
		} else {
			tTab, fTab, startBits = sbrHuffTEnv30[:], sbrHuffFEnv30[:], 6
		}
	} else {
		if balance {
			tTab, fTab, startBits = sbrHuffTEnvBal15[:], sbrHuffFEnvBal15[:], 6
		} else {
			tTab, fTab, startBits = sbrHuffTEnv15[:], sbrHuffFEnv15[:], 7
		}
	}
	// The balance doubling applies to the start value only: the balance
	// huffman tables already carry doubled delta values.
	for l := 0; l < fd.grid.numEnv; l++ {
		n := envBands(t, fd.grid.freqRes[l])
		if !fd.dfEnv[l] {
			fd.env[l][0] = int32(r.read(startBits)) * mul
			for b := 1; b < n; b++ {
				d := int32(sbrHuffDecode(r, fTab))
				fd.env[l][b] = fd.env[l][b-1] + d
				if fd.env[l][b] < 0 || fd.env[l][b] > 127 {
					return false
				}
			}
		} else {
			ref := prevEnv
			refRes := *prevRes
			if l > 0 {
				ref = &fd.env[l-1]
				refRes = fd.grid.freqRes[l-1]
			}
			for b := 0; b < n; b++ {
				d := int32(sbrHuffDecode(r, tTab))
				pb := mapPrevBand(t, fd.grid.freqRes[l], refRes, b)
				fd.env[l][b] = ref[pb] + d
				if fd.env[l][b] < 0 || fd.env[l][b] > 127 {
					return false
				}
			}
		}
	}
	*prevEnv = fd.env[fd.grid.numEnv-1]
	*prevRes = fd.grid.freqRes[fd.grid.numEnv-1]
	return true
}

// parseNoise reads sbr_noise (4.6.18.3.6): noise floors share the 3 dB
// envelope tables in the frequency direction and their own in time. Like
// parseEnvelope it reports false past the valid range (0..30, ffmpeg's
// rule for both channels of a coupled pair).
func parseNoise(r *bitReader, t *sbrFreqTables, fd *sbrFrameData,
	prevNoise *[5]int32, balance bool) bool {
	var tTab, fTab [][2]int8
	mul := int32(1)
	if balance {
		tTab, fTab, mul = sbrHuffTNoiseBal30[:], sbrHuffFEnvBal30[:], 2
	} else {
		tTab, fTab = sbrHuffTNoise30[:], sbrHuffFEnv30[:]
	}
	// As with envelopes, the balance doubling lives in the start value and
	// the pre-doubled balance tables, not on decoded deltas.
	for l := 0; l < fd.grid.numNoise; l++ {
		if !fd.dfNoise[l] {
			fd.noise[l][0] = int32(r.read(5)) * mul
			for b := 1; b < t.nQ; b++ {
				d := int32(sbrHuffDecode(r, fTab))
				fd.noise[l][b] = fd.noise[l][b-1] + d
				if fd.noise[l][b] < 0 || fd.noise[l][b] > 30 {
					return false
				}
			}
		} else {
			ref := prevNoise
			if l > 0 {
				ref = &fd.noise[l-1]
			}
			for b := 0; b < t.nQ; b++ {
				d := int32(sbrHuffDecode(r, tTab))
				fd.noise[l][b] = ref[b] + d
				if fd.noise[l][b] < 0 || fd.noise[l][b] > 30 {
					return false
				}
			}
		}
	}
	*prevNoise = fd.noise[fd.grid.numNoise-1]
	return true
}

// parseSBRChannel reads one channel's grid, dtdf, invf, envelope, and
// noise runs in the single-channel element order.
func parseSBRChannel(r *bitReader, t *sbrFreqTables, hdr sbrHeader,
	fd *sbrFrameData, st *sbrChanState) bool {
	if !parseGrid(r, &fd.grid) {
		return false
	}
	fd.ampRes = hdr.ampRes
	if fd.grid.frameClass == fixfix && fd.grid.numEnv == 1 {
		fd.ampRes = 0 // single-envelope frames always use 1.5 dB steps
	}
	parseDtdf(r, fd)
	for b := 0; b < t.nQ; b++ {
		fd.invf[b] = int(r.read(2))
	}
	if !parseEnvelope(r, t, fd, &st.prevEnv, &st.prevRes, false) ||
		!parseNoise(r, t, fd, &st.prevNoise, false) {
		return false
	}
	parseAddHarmonic(r, t, fd)
	return !r.overrun()
}

func parseDtdf(r *bitReader, fd *sbrFrameData) {
	for l := 0; l < fd.grid.numEnv; l++ {
		fd.dfEnv[l] = r.bit() != 0
	}
	for l := 0; l < fd.grid.numNoise; l++ {
		fd.dfNoise[l] = r.bit() != 0
	}
}

func parseAddHarmonic(r *bitReader, t *sbrFreqTables, fd *sbrFrameData) {
	fd.addHarmonic = [64]bool{}
	if r.bit() != 0 {
		for b := 0; b < t.nHigh && b < len(fd.addHarmonic); b++ {
			fd.addHarmonic[b] = r.bit() != 0
		}
	}
}

// parseSBRExtendedData consumes the trailing bs_extended_data block. The
// PS payload (extension id 2) routes to the element's PS state when the
// config signalled explicit PS; with no PS state, and for every other
// extension id (enhanced SBR included), the block is skipped by its
// declared length. It reports whether the block stayed inside the fill
// element: the declared byte count comes from the stream, and one that
// runs past fillEnd would otherwise hand the next element's entropy-coded
// bits to the PS parser as parameter grids, so the read is clamped and
// the lie fails the payload like any other damage.
func (el *sbrElement) parseSBRExtendedData(r *bitReader, fillEnd int) bool {
	if r.bit() == 0 {
		return true
	}
	cnt := int(r.read(4))
	if cnt == 15 {
		cnt += int(r.read(8))
	}
	end := r.pos + cnt*8
	ok := end <= fillEnd
	end = min(end, fillEnd)
	bits := end - r.pos
	// The spec's own consumption rule: another extension follows while at
	// least 8 bits remain, so conformant padding is under a byte and the
	// loop cannot re-enter on it.
	for bits > 7 {
		id := int(r.read(2))
		bits -= 2
		if id != extensionIDPS {
			break // unknown id: no length of its own, skip the block
		}
		// Seen is recorded even when nothing parses it: implicit-signalling
		// detection (DetectSBR) is what turns an ADTS stream stereo.
		el.sawPS = true
		if el.ps == nil {
			break
		}
		used, psOK := el.ps.data.parse(r, bits)
		if !psOK {
			break
		}
		bits -= used
	}
	r.pos = end
	return ok
}

// parseSBRData reads sbr_data for the element's channel count (the
// single-channel and channel-pair element syntaxes). fillEnd is the fill
// element's bit end, bounding the trailing extended-data block.
func (el *sbrElement) parseSBRData(r *bitReader, fillEnd int) bool {
	t := &el.tbl
	if el.chans == 1 {
		if r.bit() != 0 {
			r.read(4) // bs_data_extra reserved
		}
		fd := &el.ch[0].frame
		*fd = sbrFrameData{}
		if !parseSBRChannel(r, t, el.hdr, fd, &el.ch[0]) {
			return false
		}
		if !el.parseSBRExtendedData(r, fillEnd) {
			return false
		}
		return !r.overrun()
	}
	if r.bit() != 0 {
		r.read(8) // bs_data_extra reserved, both channels
	}
	fd0, fd1 := &el.ch[0].frame, &el.ch[1].frame
	*fd0, *fd1 = sbrFrameData{}, sbrFrameData{}
	el.coupled = r.bit() != 0
	if el.coupled {
		if !parseGrid(r, &fd0.grid) {
			return false
		}
		fd1.grid = fd0.grid
		fd0.ampRes = el.hdr.ampRes
		if fd0.grid.frameClass == fixfix && fd0.grid.numEnv == 1 {
			fd0.ampRes = 0
		}
		fd1.ampRes = fd0.ampRes
		parseDtdf(r, fd0)
		parseDtdf(r, fd1)
		for b := 0; b < t.nQ; b++ {
			fd0.invf[b] = int(r.read(2))
			fd1.invf[b] = fd0.invf[b]
		}
		if !parseEnvelope(r, t, fd0, &el.ch[0].prevEnv, &el.ch[0].prevRes, false) ||
			!parseNoise(r, t, fd0, &el.ch[0].prevNoise, false) ||
			!parseEnvelope(r, t, fd1, &el.ch[1].prevEnv, &el.ch[1].prevRes, true) ||
			!parseNoise(r, t, fd1, &el.ch[1].prevNoise, true) {
			return false
		}
	} else {
		if !parseGrid(r, &fd0.grid) || !parseGrid(r, &fd1.grid) {
			return false
		}
		fd0.ampRes = el.hdr.ampRes
		if fd0.grid.frameClass == fixfix && fd0.grid.numEnv == 1 {
			fd0.ampRes = 0
		}
		fd1.ampRes = el.hdr.ampRes
		if fd1.grid.frameClass == fixfix && fd1.grid.numEnv == 1 {
			fd1.ampRes = 0
		}
		parseDtdf(r, fd0)
		parseDtdf(r, fd1)
		for b := 0; b < t.nQ; b++ {
			fd0.invf[b] = int(r.read(2))
		}
		for b := 0; b < t.nQ; b++ {
			fd1.invf[b] = int(r.read(2))
		}
		if !parseEnvelope(r, t, fd0, &el.ch[0].prevEnv, &el.ch[0].prevRes, false) ||
			!parseEnvelope(r, t, fd1, &el.ch[1].prevEnv, &el.ch[1].prevRes, false) ||
			!parseNoise(r, t, fd0, &el.ch[0].prevNoise, false) ||
			!parseNoise(r, t, fd1, &el.ch[1].prevNoise, false) {
			return false
		}
	}
	parseAddHarmonic(r, t, fd0)
	parseAddHarmonic(r, t, fd1)
	if !el.parseSBRExtendedData(r, fillEnd) {
		return false
	}
	return !r.overrun()
}

// parseFill parses one fill-element extension payload of count bytes
// routed to this element, leaving the reader exactly past it. It reports
// whether this frame now has usable SBR data.
func (el *sbrElement) parseFill(r *bitReader, count int) {
	// A zero-length fill is padding, not an extension payload; parsing its
	// nonexistent type field would read the neighbouring element's bits.
	if count < 1 {
		return
	}
	start := r.pos
	end := start + count*8
	el.parsePayload(r, end)
	r.pos = end
}

func (el *sbrElement) parsePayload(r *bitReader, end int) {
	ext := r.read(4)
	switch ext {
	case extSBRDataCRC:
		r.read(10) // bs_sbr_crc; not validated
	case extSBRData:
	default:
		return // some other extension: skipped by length in parseFill
	}
	el.sawSBR = true
	if r.bit() != 0 {
		hdr := parseSBRHeader(r)
		el.applyHeader(hdr)
	}
	if !el.haveHdr || !el.haveTbl {
		return // no header yet: the element keeps concealing
	}
	// The delta anchors (and the PS parameter state, which the extended
	// block mutates on the way through) update while the payload parses;
	// snapshot them so a rejected payload cannot leave garbage for the next
	// frame's delta-time references to anchor on, or a latched PS start
	// widening the element with grids the frame was rejected over.
	var savedEnv [2][64]int32
	var savedNoise [2][5]int32
	var savedRes [2]int
	for c := range el.ch {
		savedEnv[c] = el.ch[c].prevEnv
		savedNoise[c] = el.ch[c].prevNoise
		savedRes[c] = el.ch[c].prevRes
	}
	var savedPS psData
	if el.ps != nil {
		savedPS = el.ps.data
	}
	if el.parseSBRData(r, end) && r.pos <= end {
		el.dataOK = true
	} else {
		el.dataOK = false
		for c := range el.ch {
			el.ch[c].prevEnv = savedEnv[c]
			el.ch[c].prevNoise = savedNoise[c]
			el.ch[c].prevRes = savedRes[c]
		}
		if el.ps != nil {
			el.ps.data = savedPS
		}
	}
}
