//go:build !wmavoicetablesgen

package wmavoice

import (
	"math"
	"slices"
)

// Line spectral frequency coding (note 5). Everything on this path is float64:
// the reference is double throughout it and the values are small differences of
// transcendentals, so single precision is not enough.

// maxLSPs is the larger of the two orders, so the fixed-size arrays below hold
// either.
const maxLSPs = lsp16

// dqStage is one stage of the multi-stage dequantiser: how many vectors the
// stage holds, and the scale and offset its entries carry.
type dqStage struct {
	size int
	mul  float64
	base float64
}

// The dequantiser scales and offsets, notes 5.2 to 5.4. Thirty constants, and
// the only thing about them worth saying twice is that base is added once per
// coefficient PER STAGE, not once per coefficient.
var (
	dqStages10i = [4]dqStage{
		{256, 5.2187144800e-3, math.Pi * -2.15522e-1},
		{64, 1.4626986422e-3, math.Pi * -6.1646e-2},
		{32, 9.6179549166e-4, math.Pi * -3.3486e-2},
		{32, 1.1325736225e-3, math.Pi * -5.7408e-2},
	}
	dqStages16i = [5]dqStage{
		{256, 3.3439586280e-3, math.Pi * -1.27576e-1},
		{64, 6.9908173703e-4, math.Pi * -2.4292e-2},
		{128, 3.3216608306e-3, math.Pi * -1.28094e-1},
		{64, 1.0334960326e-3, math.Pi * -3.2128e-2},
		{128, 3.1899104283e-3, math.Pi * -1.29816e-1},
	}
	dqStages10r = [3]dqStage{
		{128, 2.5807601174e-3, math.Pi * -1.07448e-1},
		{64, 1.2354460219e-3, math.Pi * -5.2706e-2},
		{64, 1.1763821673e-3, math.Pi * -5.1634e-2},
	}
	dqStages16r = [3]dqStage{
		{128, 1.2232979501e-3, math.Pi * -5.5830e-2},
		{128, 1.4062241527e-3, math.Pi * -5.2908e-2},
		{128, 1.6114744851e-3, math.Pi * -5.4776e-2},
	}
)

// dequant is the multi-stage dequantiser every LSP codebook is read through
// (note 5.1). table is a flat run of unsigned 8-bit entries, stage s occupying
// size[s] vectors of num coefficients each, laid end to end.
//
// Every index comes from a bitstream field exactly wide enough for its stage's
// size, so no index can address past its stage; TestEveryCodebookIsExactlyIts
// StagesLong pins the table lengths that make that hold.
func dequant(out []float64, table []uint8, num int, idx []int, stages []dqStage) {
	clear(out[:num])
	offset := 0
	for s, st := range stages {
		row := offset + idx[s]*num
		for m := range num {
			out[m] += st.base + st.mul*float64(table[row+m])
		}
		offset += st.size * num
	}
}

// independentLSFs reads one frame's own LSF set: four or five codebook indices
// and nothing else (notes 5.2 and 5.3). The mean vector is NOT added here;
// callers that need an absolute set add it, because the residual path of 5.4
// wants the difference.
func (d *Decoder) independentLSFs(r *bitReader, out []float64) {
	if d.lsps == lsp10 {
		idx := [4]int{int(r.bits(8)), int(r.bits(6)), int(r.bits(5)), int(r.bits(5))}
		dequant(out, dqLSP10i[:], lsp10, idx[:], dqStages10i[:])
		return
	}
	idx := [5]int{int(r.bits(8)), int(r.bits(6)), int(r.bits(7)), int(r.bits(6)), int(r.bits(7))}
	dequant(out[0:], dqLSP16i1[:], 5, idx[0:2], dqStages16i[0:2])
	dequant(out[5:], dqLSP16i2[:], 5, idx[2:4], dqStages16i[2:4])
	dequant(out[10:], dqLSP16i3[:], 6, idx[4:5], dqStages16i[4:5])
}

// residualLSFs reads the superframe's one LSP block and fills all three frames'
// LSF sets from it (note 5.4). This is what every real file does; the
// independent path below it is reachable in the format and written by nothing.
func (d *Decoder) residualLSFs(r *bitReader, out *[framesPerSuperframe][maxLSPs]float64) {
	l := d.lsps
	mean := d.meanLSF()

	// prev is the previous superframe's third-frame set, as a difference from
	// the mean, which is the same domain the independent read below lands in.
	var prev [maxLSPs]float64
	for n := range l {
		prev[n] = d.prevLSF[n] - mean[n]
	}

	var indep [maxLSPs]float64
	d.independentLSFs(r, indep[:l])

	interp := int(r.bits(5))
	d.paths.LSPInterp[interp] = true
	var idx [3]int
	if l == lsp10 {
		idx = [3]int{int(r.bits(7)), int(r.bits(6)), int(r.bits(6))}
	} else {
		idx = [3]int{int(r.bits(7)), int(r.bits(7)), int(r.bits(7))}
	}

	// The interpolated pair: the first two frames start as weighted blends of
	// the previous superframe's set and this one's, and the correction vector
	// below moves each off that line.
	var a1 [2 * maxLSPs]float64
	t := d.lspInterp(interp)
	for n := range l {
		delta := prev[n] - indep[n]
		a1[n] = float64(t[0][n])*delta + indep[n]
		a1[l+n] = float64(t[1][n])*delta + indep[n]
	}

	var a2 [2 * maxLSPs]float64
	if l == lsp10 {
		dequant(a2[:], dqLSP10r[:], 2*lsp10, idx[:], dqStages10r[:])
	} else {
		dequant(a2[0:], dqLSP16r1[:], 10, idx[0:1], dqStages16r[0:1])
		dequant(a2[10:], dqLSP16r2[:], 10, idx[1:2], dqStages16r[1:2])
		dequant(a2[20:], dqLSP16r3[:], 12, idx[2:3], dqStages16r[2:3])
	}

	// a2 is read as pairs: the even entry corrects the first interpolated
	// frame and the odd entry the second.
	for n := range l {
		out[0][n] = mean[n] + (a1[n] - a2[2*n])
		out[1][n] = mean[n] + (a1[l+n] - a2[2*n+1])
		out[2][n] = mean[n] + indep[n]
	}
}

// independentFrameLSFs is the other arm of note 5.4, taken when the packet
// header's residual flag is clear: each frame carries its own set immediately
// before its data.
//
// DEFICIT: no file in this tree reaches this path. Every packet of every cell
// sets the residual flag, so two codebook readers and a whole alternative
// frame layout ship implemented from the note and exercised by nothing. It is
// implemented rather than refused because the description is complete and
// refusing would turn a valid file into an unplayable one.
func (d *Decoder) independentFrameLSFs(r *bitReader, out []float64) {
	mean := d.meanLSF()
	d.independentLSFs(r, out[:d.lsps])
	for n := range d.lsps {
		out[n] += mean[n]
	}
}

// meanLSF is the mean vector the LSP default mode flag selects.
func (d *Decoder) meanLSF() []float64 {
	if d.lsps == lsp10 {
		return meanLSF10[d.cfg.meanIndex()][:]
	}
	return meanLSF16[d.cfg.meanIndex()][:]
}

// lspInterp is one row of the inter-frame interpolation table the LSP
// quantiser mode flag selects. The entries are float32 in the table and are
// widened for the multiply.
func (d *Decoder) lspInterp(idx int) [2][]float32 {
	if d.lsps == lsp10 {
		if d.cfg.lspQuantiserB() {
			return [2][]float32{lsp10InterpB[idx][0][:], lsp10InterpB[idx][1][:]}
		}
		return [2][]float32{lsp10InterpA[idx][0][:], lsp10InterpA[idx][1][:]}
	}
	if d.cfg.lspQuantiserB() {
		return [2][]float32{lsp16InterpB[idx][0][:], lsp16InterpB[idx][1][:]}
	}
	return [2][]float32{lsp16InterpA[idx][0][:], lsp16InterpA[idx][1][:]}
}

// Stabilisation bounds (note 5.6), as fractions of pi.
const (
	lsfFloor   = 0.0015
	lsfSpacing = 0.0125
	lsfCeiling = 0.9985
)

// stabilise makes an LSF set monotone and bounded away from both band edges,
// which is what keeps the LPC filter the cosines produce stable.
//
// The three flags say which of the clamps fired, which is what the coverage
// matrix pins.
//
// DEFICIT: on the whole corpus the first clamp never fires, the last clamp
// never fires, and the sort never runs. It runs anyway because it costs
// nothing and the encoder is free to write a set that needs it.
func stabilise(lsf []float64) (floored, capped, sorted bool) {
	if lsf[0] < lsfFloor*math.Pi {
		lsf[0], floored = lsfFloor*math.Pi, true
	}
	for n := 1; n < len(lsf); n++ {
		lsf[n] = max(lsf[n], lsf[n-1]+lsfSpacing*math.Pi)
	}
	last := len(lsf) - 1
	if lsf[last] > lsfCeiling*math.Pi {
		lsf[last], capped = lsfCeiling*math.Pi, true
	}
	// Only the ceiling above can break the order it just established, by
	// pushing the last value back below the one before it.
	for n := 1; n < len(lsf); n++ {
		if lsf[n] < lsf[n-1] {
			slices.Sort(lsf)
			return floored, capped, true
		}
	}
	return floored, capped, false
}

// lspToLPC expands the cosines of an LSF set into the direct-form denominator
// of the synthesis filter (note 5.7): A(z) = 1 + lpc[0] z^-1 + ... There is no
// leading 1 in the array and no sign flip.
//
// pa and qa are scratch of at least L/2+1 entries each, held by the caller so
// the hot path does not allocate.
func lspToLPC(lpc []float32, lsp []float64, pa, qa []float64) {
	h := len(lsp) / 2
	lspPoly(pa[:h+1], lsp, h)
	lspPoly(qa[:h+1], lsp[1:], h)
	for i := h - 1; i >= 0; i-- {
		p := pa[i+1] + pa[i]
		q := qa[i+1] - qa[i]
		lpc[i] = float32(0.5 * (p + q))
		lpc[2*h-1-i] = float32(0.5 * (p - q))
	}
}

// lspPoly is the product-of-second-order-sections expansion. The index 2*i-2
// is what makes the caller's second call read the odd-indexed cosines.
func lspPoly(f, v []float64, h int) {
	f[0] = 1
	f[1] = -2 * v[0]
	for i := 2; i <= h; i++ {
		a := -2 * v[2*i-2]
		f[i] = a*f[i-1] + 2*f[i-2]
		for j := i - 1; j >= 2; j-- {
			f[j] += f[j-1]*a + f[j-2]
		}
		f[1] += a
	}
}

// lsfToLPC takes the cosine of a weighted LSF set and expands it. Every place
// that needs LPCs does exactly this: interpolate in the LSF domain, then take
// the cosine (note 5.7).
func (d *Decoder) lsfToLPC(lpc []float32, from, to []float64, weight float64) {
	for n := range d.lsps {
		d.lspScratch[n] = math.Cos(from[n] + weight*(to[n]-from[n]))
	}
	lspToLPC(lpc, d.lspScratch[:d.lsps], d.paScratch[:], d.qaScratch[:])
}
