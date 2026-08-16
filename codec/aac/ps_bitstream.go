package aac

// PS bitstream parsing (ISO/IEC 14496-3 8.4): ps_data rides inside the
// SBR extended-data block under extension id 2, carrying the stereo
// parameter grids. Parameters persist across frames: delta-time coding
// references the previous frame's last envelope, and a frame whose data
// cannot be trusted clears them and drops the element back to dual mono
// until a header arrives again (spec-legal legacy behavior, and what the
// reference decoders do).

// extensionIDPS is the SBR extended-data id that carries ps_data
// (Table 8.18).
const extensionIDPS = 2

// psNumEnvTab resolves bs_num_env_idx per frame class (Table 8.20).
var psNumEnvTab = [2][4]int8{
	{0, 1, 2, 4},
	{1, 2, 3, 4},
}

// psNrParTab maps bs_iid_mode / bs_icc_mode to its parameter band count;
// modes 3-5 repeat the counts with the fine quantizer (IID) or mixing
// procedure B (ICC).
var psNrParTab = [6]int8{10, 20, 34, 10, 20, 34}

// psNrIPDOPDParTab is the phase parameter count each IID mode implies.
var psNrIPDOPDParTab = [6]int8{5, 11, 17, 5, 11, 17}

// psData is one element's persistent PS parameter state.
type psData struct {
	// start reports that a ps_header has been seen; until then (and after
	// a damaged frame) the element widens to dual mono only.
	start bool

	enableIID    bool
	iidQuant     int // 1 selects the fine IID quantizer
	nrIIDPar     int
	nrIPDOPDPar  int
	enableICC    bool
	iccMode      int
	nrICCPar     int
	enableExt    bool
	enableIPDOPD bool

	frameClass int
	numEnvOld  int
	numEnv     int
	border     [6]int // slot borders, border[0] = -1, last = 31

	iid [5][34]int8
	icc [5][34]int8
	ipd [5][34]int8
	opd [5][34]int8

	is34, is34Old bool
}

// fail is the damaged-frame path: parameters are cleared so nothing
// delta-anchors on half-parsed values, and start drops so the element
// conceals as dual mono until the next header.
func (ps *psData) fail() (int, bool) {
	ps.iid = [5][34]int8{}
	ps.icc = [5][34]int8{}
	ps.ipd = [5][34]int8{}
	ps.opd = [5][34]int8{}
	ps.start = false
	return 0, false
}

// parse reads one ps_data payload, bounded by the bitsLeft remaining in
// the extended-data block. It returns the bits consumed; the caller owns
// the reader position either way (the extended block is skipped whole).
func (ps *psData) parse(r *bitReader, bitsLeft int) (int, bool) {
	startPos := r.pos
	header := r.bit() != 0
	if header {
		ps.enableIID = r.bit() != 0
		if ps.enableIID {
			mode := int(r.read(3))
			if mode > 5 {
				return ps.fail()
			}
			ps.nrIIDPar = int(psNrParTab[mode])
			ps.iidQuant = 0
			if mode > 2 {
				ps.iidQuant = 1
			}
			ps.nrIPDOPDPar = int(psNrIPDOPDParTab[mode])
		}
		ps.enableICC = r.bit() != 0
		if ps.enableICC {
			ps.iccMode = int(r.read(3))
			if ps.iccMode > 5 {
				return ps.fail()
			}
			ps.nrICCPar = int(psNrParTab[ps.iccMode])
		}
		ps.enableExt = r.bit() != 0
	}

	ps.frameClass = int(r.bit())
	ps.numEnvOld = ps.numEnv
	ps.numEnv = int(psNumEnvTab[ps.frameClass][r.read(2)])

	ps.border[0] = -1
	if ps.frameClass == 1 {
		for e := 1; e <= ps.numEnv; e++ {
			ps.border[e] = int(r.read(5))
			if ps.border[e] < ps.border[e-1] {
				return ps.fail()
			}
		}
	} else {
		// Evenly split borders; numEnv is 0 or a power of two here, so the
		// division is the spec's shift.
		for e := 1; e <= ps.numEnv; e++ {
			ps.border[e] = e*sbrSlots/ps.numEnv - 1
		}
	}

	if ps.enableIID {
		limit := 7 + 8*ps.iidQuant
		dfTree, dtTree := psTreeIIDDF, psTreeIIDDT
		if ps.iidQuant != 0 {
			dfTree, dtTree = psTreeIIDFineDF, psTreeIIDFineDT
		}
		for e := 0; e < ps.numEnv; e++ {
			if !ps.readPar(r, &ps.iid, e, ps.nrIIDPar, dfTree, dtTree, -limit, limit, 0) {
				return ps.fail()
			}
		}
	} else {
		ps.iid = [5][34]int8{}
	}

	if ps.enableICC {
		// The ICC range is one-sided: a negative accumulation is as
		// illegal as one past 7, and it must be rejected here because it
		// would otherwise index the mixing tables.
		for e := 0; e < ps.numEnv; e++ {
			if !ps.readPar(r, &ps.icc, e, ps.nrICCPar, psTreeICCDF, psTreeICCDT, 0, 7, 0) {
				return ps.fail()
			}
		}
	} else {
		ps.icc = [5][34]int8{}
	}

	if ps.enableExt {
		cnt := int(r.read(4))
		if cnt == 15 {
			cnt += int(r.read(8))
		}
		bits := cnt * 8
		for bits > 7 {
			id := int(r.read(2))
			bits -= 2
			bits -= ps.readExtension(r, id)
		}
		if bits < 0 {
			return ps.fail()
		}
		r.skip(bits)
	}

	// A frame whose last border stops short of the frame end (or that
	// carried no envelopes at all) extends its final parameters to slot 31
	// as an extra envelope, sourced from the previous frame when there was
	// nothing this frame. The copied values are re-validated: they may
	// predate a header that changed the quantizer.
	if ps.numEnv == 0 || ps.border[ps.numEnv] < sbrSlots-1 {
		source := ps.numEnv - 1
		if ps.numEnv == 0 {
			source = ps.numEnvOld - 1
		}
		if source >= 0 && source != ps.numEnv {
			ps.iid[ps.numEnv] = ps.iid[source]
			ps.icc[ps.numEnv] = ps.icc[source]
			ps.ipd[ps.numEnv] = ps.ipd[source]
			ps.opd[ps.numEnv] = ps.opd[source]
		}
		if ps.enableIID {
			limit := int8(7 + 8*ps.iidQuant)
			for b := 0; b < ps.nrIIDPar; b++ {
				if v := ps.iid[ps.numEnv][b]; v > limit || v < -limit {
					return ps.fail()
				}
			}
		}
		if ps.enableICC {
			for b := 0; b < ps.nrICCPar; b++ {
				if v := ps.icc[ps.numEnv][b]; v > 7 || v < 0 {
					return ps.fail()
				}
			}
		}
		ps.numEnv++
		ps.border[ps.numEnv] = sbrSlots - 1
	}

	ps.is34Old = ps.is34
	if ps.enableIID || ps.enableICC {
		ps.is34 = (ps.enableIID && ps.nrIIDPar == 34) ||
			(ps.enableICC && ps.nrICCPar == 34)
	}

	if !ps.enableIPDOPD {
		ps.ipd = [5][34]int8{}
		ps.opd = [5][34]int8{}
	}

	consumed := r.pos - startPos
	if consumed > bitsLeft || r.overrun() {
		return ps.fail()
	}
	if header {
		ps.start = true
	}
	return consumed, true
}

// readPar reads one envelope's parameter run, delta-coded in frequency
// (accumulating from zero) or in time (anchored on the previous envelope,
// which for the frame's first is the previous frame's last). mask wraps
// the phase parameters mod 8, which keeps them inside [lo, hi] by
// construction; the delta-accumulated parameters are rejected outside it.
func (ps *psData) readPar(r *bitReader, par *[5][34]int8, e, num int,
	dfTree, dtTree [][2]int16, lo, hi int, mask int8) bool {
	dt := r.bit() != 0
	if dt {
		ePrev := max(ps.numEnvOld-1, 0)
		if e > 0 {
			ePrev = e - 1
		}
		for b := 0; b < num; b++ {
			v := int(par[ePrev][b]) + psHuffDecode(r, dtTree)
			if mask != 0 {
				v &= int(mask)
			}
			par[e][b] = int8(v)
			if v > hi || v < lo {
				return false
			}
		}
		return true
	}
	v := 0
	for b := 0; b < num; b++ {
		v += psHuffDecode(r, dfTree)
		if mask != 0 {
			v &= int(mask)
		}
		par[e][b] = int8(v)
		if v > hi || v < lo {
			return false
		}
	}
	return true
}

// readExtension reads one ps_extension block; id 0 carries the IPD/OPD
// phase parameters, anything else is unknown and left for the caller's
// byte-count skip. Returns the bits consumed.
func (ps *psData) readExtension(r *bitReader, id int) int {
	if id != 0 {
		return 0
	}
	startPos := r.pos
	ps.enableIPDOPD = r.bit() != 0
	if ps.enableIPDOPD {
		for e := 0; e < ps.numEnv; e++ {
			// Phase deltas wrap mod 8, so no accumulation can leave the
			// range and nothing here can fail.
			ps.readPar(r, &ps.ipd, e, ps.nrIPDOPDPar, psTreeIPDDF, psTreeIPDDT, 0, 7, 7)
			ps.readPar(r, &ps.opd, e, ps.nrIPDOPDPar, psTreeOPDDF, psTreeOPDDT, 0, 7, 7)
		}
	}
	r.bit() // reserved
	return r.pos - startPos
}
