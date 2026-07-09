package opus

import "math"

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

// --- encode side (libopus bands.c compute_band_energies/normalise_bands and
// quant_bands.c amp2Log2/quant_*_energy, float build) ---

// computeBandEnergies computes each band's L2 energy from the MDCT coefficients X
// (per channel, stride N), matching libopus's float celt_inner_prod accumulation.
func computeBandEnergies(X, bandE []float32, end, C, LM int) {
	N := celtShortMDCTSize << LM
	for c := 0; c < C; c++ {
		for i := 0; i < end; i++ {
			lo := int(celtEBands[i]) << LM
			hi := int(celtEBands[i+1]) << LM
			var sum float32 = 1e-27
			for j := lo; j < hi; j++ {
				v := X[c*N+j]
				sum += v * v
			}
			bandE[i+c*celtNBands] = float32(math.Sqrt(float64(sum)))
		}
	}
}

// normaliseBands scales the MDCT coefficients to unit-norm band shapes X, the
// inverse of denormaliseBands (libopus normalise_bands, float build).
func normaliseBands(freq, X, bandE []float32, end, C, M int) {
	N := M * celtShortMDCTSize
	for c := 0; c < C; c++ {
		for i := 0; i < end; i++ {
			g := 1.0 / (1e-27 + bandE[i+c*celtNBands])
			for j := M * int(celtEBands[i]); j < M*int(celtEBands[i+1]); j++ {
				X[j+c*N] = freq[j+c*N] * g
			}
		}
	}
}

// amp2Log2 converts band energies to the log2-minus-mean domain the coarse-energy
// quantizer codes (libopus amp2Log2, float build with exact celt_log2).
func amp2Log2(effEnd, end int, bandE, bandLogE []float32, C int) {
	for c := 0; c < C; c++ {
		for i := 0; i < effEnd; i++ {
			bandLogE[i+c*celtNBands] = float32(math.Log2(float64(bandE[i+c*celtNBands]))) - celtEMeans[i]
		}
		for i := effEnd; i < end; i++ {
			bandLogE[c*celtNBands+i] = -14
		}
	}
}

// lossDistortion measures how far the current band energies moved from the
// previous frame's, used to bias the intra/inter coarse-energy choice.
func lossDistortion(eBands, oldEBands []float32, start, end, C int) float32 {
	var dist float32
	for c := 0; c < C; c++ {
		for i := start; i < end; i++ {
			d := eBands[i+c*celtNBands] - oldEBands[i+c*celtNBands]
			dist += d * d
		}
	}
	return min(200, dist)
}

// quantCoarseEnergyImpl Laplace-codes the coarse (integer-step) band energies
// with inter- or intra-frame prediction, updating oldEBands to the reconstructed
// values and error to the per-band residual for the fine pass. Returns a badness
// score. Clean-room port of libopus quant_coarse_energy_impl (float, lfe=0).
func quantCoarseEnergyImpl(start, end int, eBands, oldEBands []float32, budget, tell int,
	probModel []byte, errorArr []float32, e *rangeEncoder, C, LM, intra int, maxDecay float32) int {

	badness := 0
	var prev [2]float32
	var coef, beta float32
	if tell+3 <= budget {
		e.encodeBitLogp(intra, 3)
	}
	if intra != 0 {
		coef, beta = 0, celtBetaIntra
	} else {
		coef, beta = celtPredCoef[LM], celtBetaCoef[LM]
	}
	for i := start; i < end; i++ {
		for c := 0; c < C; c++ {
			x := eBands[i+c*celtNBands]
			oldE := max(-9.0, oldEBands[i+c*celtNBands])
			f := x - coef*oldE - prev[c]
			// Rounding to nearest is important here.
			qi := int(math.Floor(float64(0.5 + f)))
			decayBound := max(-28.0, oldEBands[i+c*celtNBands]) - maxDecay
			// Prevent the energy from dropping too fast (single-bin bands).
			if qi < 0 && x < decayBound {
				qi += int(decayBound - x)
				if qi > 0 {
					qi = 0
				}
			}
			qi0 := qi
			tell = e.tell()
			bitsLeft := budget - tell - 3*C*(end-i)
			if i != start && bitsLeft < 30 {
				if bitsLeft < 24 {
					qi = min(1, qi)
				}
				if bitsLeft < 16 {
					qi = max(-1, qi)
				}
			}
			switch {
			case budget-tell >= 15:
				pi := 2 * min(i, 20)
				qi = e.laplaceEncode(qi, uint32(probModel[pi])<<7, int(probModel[pi+1])<<6)
			case budget-tell >= 2:
				qi = max(-1, min(qi, 1))
				e.encodeICDF(2*qi^-b2i(qi < 0), celtSmallEnergyICDF, 2)
			case budget-tell >= 1:
				qi = min(0, qi)
				e.encodeBitLogp(-qi, 1)
			default:
				qi = -1
			}
			errorArr[i+c*celtNBands] = f - float32(qi)
			badness += iabs(qi0 - qi)
			q := float32(qi)
			tmp := coef*oldE + prev[c] + q
			oldEBands[i+c*celtNBands] = tmp
			prev[c] = prev[c] + q - beta*q
		}
	}
	return badness
}

// quantCoarseEnergy chooses inter- or intra-frame prediction and Laplace-codes
// the coarse band energies. This is the single-pass form (libopus's complexity<4
// path): the intra decision follows the delayedIntra heuristic. Clean-room port
// of libopus quant_coarse_energy (float, two_pass=0, lfe=0, loss_rate=0).
// quantCoarseEnergy codes the coarse band energies. When twoPass is set
// (complexity >= 4) it encodes the frame both intra (no inter-band/inter-frame
// prediction) and inter, then keeps whichever produced less badness, restoring
// the range coder to the winning attempt. Clean-room port of libopus
// quant_coarse_energy (float build). loss_rate is 0 (no PLC in file decode), so
// the intra_bias tie-break term is 0.
func quantCoarseEnergy(start, end, effEnd int, eBands, oldEBands []float32, budget int,
	errorArr []float32, e *rangeEncoder, C, LM, nbAvailableBytes int, forceIntra bool, delayedIntra *float32, twoPass bool) {

	nb := celtNBands
	intra := 0
	if forceIntra || (!twoPass && *delayedIntra > float32(2*C*(end-start)) && nbAvailableBytes > (end-start)*C) {
		intra = 1
	}
	const intraBias = 0 // = budget*(*delayedIntra)*loss_rate/(C*512); loss_rate == 0
	newDistortion := lossDistortion(eBands, oldEBands, start, effEnd, C)
	tell := e.tell()
	if tell+3 > budget {
		twoPass = false
		intra = 0
	}
	maxDecay := float32(16.0)
	if end-start > 10 {
		maxDecay = min(maxDecay, 0.125*float32(nbAvailableBytes))
	}
	encStart := e.snapshot() // rewind point before coarse energy

	oldEIntra := append([]float32(nil), oldEBands...)
	errorIntra := make([]float32, C*nb)
	badness1 := 0
	if twoPass || intra != 0 {
		badness1 = quantCoarseEnergyImpl(start, end, eBands, oldEIntra, budget, tell,
			celtEProbModel[LM][1][:], errorIntra, e, C, LM, 1, maxDecay)
	}
	if intra == 0 {
		tellIntra := e.tellFrac()
		encIntra := e.snapshot()
		intraBits := e.tailBytes(&encStart, nil)
		e.restore(&encStart) // rewind and encode the inter pass over the same bytes
		badness2 := quantCoarseEnergyImpl(start, end, eBands, oldEBands, budget, tell,
			celtEProbModel[LM][0][:], errorArr, e, C, LM, 0, maxDecay)
		if twoPass && (badness1 < badness2 || (badness1 == badness2 && e.tellFrac()+intraBias > tellIntra)) {
			e.restore(&encIntra)
			e.restoreTail(&encStart, intraBits)
			copy(oldEBands, oldEIntra)
			copy(errorArr, errorIntra)
			intra = 1
		}
	} else {
		copy(oldEBands, oldEIntra)
		copy(errorArr, errorIntra)
	}
	if intra != 0 {
		*delayedIntra = newDistortion
	} else {
		pc := celtPredCoef[LM]
		*delayedIntra = pc*pc*(*delayedIntra) + newDistortion
	}
}

// quantFineEnergy codes the fine-energy refinement bits the allocator assigned,
// updating oldEBands and error (libopus quant_fine_energy, float, prev_quant nil).
func quantFineEnergy(start, end int, oldEBands, errorArr []float32, extraQuant []int, e *rangeEncoder, C int) {
	for i := start; i < end; i++ {
		if extraQuant[i] <= 0 {
			continue
		}
		extra := 1 << extraQuant[i]
		if e.tell()+C*extraQuant[i] > e.storage*8 {
			continue
		}
		for c := 0; c < C; c++ {
			q2 := int(math.Floor(float64((errorArr[i+c*celtNBands] + 0.5) * float32(extra))))
			if q2 > extra-1 {
				q2 = extra - 1
			}
			if q2 < 0 {
				q2 = 0
			}
			e.encodeRawBits(uint32(q2), uint(extraQuant[i]))
			offset := (float32(q2)+0.5)*float32(int(1)<<(14-extraQuant[i]))*(1.0/16384) - 0.5
			oldEBands[i+c*celtNBands] += offset
			errorArr[i+c*celtNBands] -= offset
		}
	}
}

// quantEnergyFinalise spends the leftover bits on a final ±half-step, in two
// priority passes (libopus quant_energy_finalise, float).
func quantEnergyFinalise(start, end int, oldEBands, errorArr []float32, fineQuant, finePriority []int, bitsLeft int, e *rangeEncoder, C int) {
	for prio := 0; prio < 2; prio++ {
		for i := start; i < end && bitsLeft >= C; i++ {
			if fineQuant[i] >= maxFineBits || finePriority[i] != prio {
				continue
			}
			for c := 0; c < C; c++ {
				q2 := 0
				if errorArr[i+c*celtNBands] >= 0 {
					q2 = 1
				}
				e.encodeRawBits(uint32(q2), 1)
				offset := (float32(q2) - 0.5) * float32(int(1)<<(14-fineQuant[i]-1)) * (1.0 / 16384)
				oldEBands[i+c*celtNBands] += offset
				errorArr[i+c*celtNBands] -= offset
				bitsLeft--
			}
		}
	}
}
