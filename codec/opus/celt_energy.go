package opus

// CELT band-energy dequantization (RFC 6716 section 4.3.2). Ported from libopus
// quant_bands.c (unquant_coarse_energy / unquant_fine_energy /
// unquant_energy_finalise). Energies are log2 values
// (celt_glog): coarse energy is Laplace-coded with inter/intra prediction, then
// fine energy and the leftover-bit pass refine each band.
//
// oldE is the per-channel log-energy array laid out [band + channel·nbEBands],
// matching the reference. Bit budgets are in whole bits (ec_tell units).

const maxFineBits = 8 // MAX_FINE_BITS

// unquantCoarseEnergy decodes the coarse (integer-step) band energies with
// inter- or intra-frame prediction.
func unquantCoarseEnergy(oldE []float32, nbEBands, start, end int, intra bool, d *rangeDecoder, C, LM int) {
	var pm [42]byte
	var coef, beta float32
	if intra {
		pm = celtEProbModel[LM][1]
		coef, beta = 0, celtBetaIntra
	} else {
		pm = celtEProbModel[LM][0]
		coef, beta = celtPredCoef[LM], celtBetaCoef[LM]
	}
	budget := d.storage * 8
	var prev [2]float32
	for i := start; i < end; i++ {
		for c := 0; c < C; c++ {
			var qi int
			tell := d.tell()
			switch {
			case budget-tell >= 15:
				pi := 2 * min(i, 20)
				qi = d.laplaceDecode(uint32(pm[pi])<<7, int(pm[pi+1])<<6)
			case budget-tell >= 2:
				q := d.decodeICDF(celtSmallEnergyICDF, 2)
				qi = (q >> 1) ^ -(q & 1)
			case budget-tell >= 1:
				qi = -d.decodeBitLogp(1)
			default:
				qi = -1
			}
			// SHL32(EXTEND32(qi), DB_SHIFT) is a no-op in the float build.
			q := float32(qi)
			idx := i + c*nbEBands
			if oldE[idx] < -9 {
				oldE[idx] = -9
			}
			tmp := coef*oldE[idx] + prev[c] + q
			oldE[idx] = tmp
			prev[c] = prev[c] + q - beta*q
		}
	}
}

// unquantFineEnergy decodes the fine-energy refinement bits assigned by the
// allocator (prev_quant is nil on the main pass, so the reference's prev==0).
func unquantFineEnergy(oldE []float32, nbEBands, start, end int, fineQuant []int, d *rangeDecoder, C int) {
	for i := start; i < end; i++ {
		extra := fineQuant[i]
		if extra <= 0 {
			continue
		}
		if d.tell()+C*extra > d.storage*8 {
			continue
		}
		for c := 0; c < C; c++ {
			q2 := int(d.decodeRawBits(uint(extra)))
			offset := (float32(q2)+0.5)*float32(int(1)<<(14-extra))*(1.0/16384) - 0.5
			oldE[i+c*nbEBands] += offset
		}
	}
}

// unquantEnergyFinalise spends the leftover bits on a final ±half-step of energy
// resolution, in two priority passes.
func unquantEnergyFinalise(oldE []float32, nbEBands, start, end int, fineQuant, finePriority []int, bitsLeft int, d *rangeDecoder, C int) {
	for prio := 0; prio < 2; prio++ {
		for i := start; i < end && bitsLeft >= C; i++ {
			if fineQuant[i] >= maxFineBits || finePriority[i] != prio {
				continue
			}
			for c := 0; c < C; c++ {
				q2 := int(d.decodeRawBits(1))
				offset := (float32(q2) - 0.5) * float32(int(1)<<(14-fineQuant[i]-1)) * (1.0 / 16384)
				oldE[i+c*nbEBands] += offset
				bitsLeft--
			}
		}
	}
}
