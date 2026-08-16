package aac

import "math"

// Parametric stereo tables (ISO/IEC 14496-3 8.6.4 and its Annex 8.A
// tables): the parameter codebooks, dequantization values, hybrid filter
// prototypes, and decorrelator constants, plus the mixing and phase tables
// derived from them at init. All of it is normative spec data, recorded in
// the same black-box parameter pass as the SBR tables with the FFmpeg
// restatement as a cross-check; no decoder logic was taken from it.
// Derived tables are computed in float64 and rounded once, so they are a
// pure function of the listed spec data on every platform.

// A PS codebook is listed as {value, bits} pairs in ascending canonical
// code order (the spec's tables list codewords; sorting them by codeword
// yields this form, which pins the whole book with no explicit codes).
// buildPSTree turns a listing into the walker's binary pair tree. The
// deltas here are the true signed values: unlike the SBR envelope books
// nothing is offset-coded, so what the walker returns is what accumulates.

// Inter-channel intensity difference deltas, fine resolution (-30..30).
var psHuffIIDFineDF = [61][2]int8{
	{-2, 4}, {2, 4}, {-1, 3}, {1, 3}, {-3, 5},
	{3, 5}, {-4, 6}, {4, 6}, {-5, 7}, {5, 7},
	{-6, 8}, {6, 8}, {7, 9}, {10, 11}, {-11, 12},
	{11, 12}, {-8, 10}, {8, 10}, {-21, 17}, {21, 17},
	{-19, 17}, {19, 17}, {-17, 16}, {17, 16}, {-14, 14},
	{-12, 13}, {12, 13}, {14, 14}, {-18, 17}, {18, 17},
	{-26, 18}, {-25, 18}, {-28, 18}, {-27, 18}, {-15, 15},
	{-9, 11}, {9, 11}, {15, 15}, {-22, 18}, {22, 18},
	{-24, 18}, {-23, 18}, {25, 18}, {26, 18}, {23, 18},
	{24, 18}, {-13, 14}, {13, 14}, {29, 18}, {30, 18},
	{27, 18}, {28, 18}, {-30, 18}, {-29, 18}, {-20, 18},
	{20, 18}, {-16, 16}, {16, 16}, {-10, 12}, {-7, 10},
	{0, 1},
}

var psHuffIIDFineDT = [61][2]int8{
	{1, 2}, {-4, 7}, {4, 7}, {-3, 6}, {3, 6},
	{5, 8}, {-6, 9}, {6, 9}, {9, 11}, {11, 12},
	{-21, 15}, {-20, 15}, {18, 15}, {19, 15}, {-13, 13},
	{-7, 10}, {7, 10}, {13, 13}, {-19, 15}, {-18, 15},
	{-26, 16}, {26, 16}, {-28, 16}, {-27, 16}, {29, 16},
	{30, 16}, {27, 16}, {28, 16}, {-30, 16}, {-29, 16},
	{-25, 16}, {25, 16}, {-24, 16}, {24, 16}, {-17, 15},
	{-15, 14}, {-10, 12}, {10, 12}, {-8, 11}, {8, 11},
	{15, 14}, {17, 15}, {-23, 16}, {23, 16}, {-12, 13},
	{12, 13}, {-14, 14}, {14, 14}, {-22, 16}, {22, 16},
	{-16, 15}, {16, 15}, {20, 16}, {21, 16}, {-11, 13},
	{-9, 12}, {-5, 9}, {-2, 5}, {2, 5}, {-1, 3},
	{0, 1},
}

// Inter-channel intensity difference deltas, default resolution (-14..14).
var psHuffIIDDF = [29][2]int8{
	{0, 1}, {1, 3}, {-1, 3}, {2, 4}, {-2, 4},
	{3, 5}, {-3, 5}, {-4, 6}, {4, 6}, {5, 6},
	{-5, 7}, {6, 8}, {-6, 9}, {-7, 10}, {7, 11},
	{8, 13}, {-8, 13}, {9, 14}, {10, 14}, {-9, 15},
	{11, 15}, {-10, 16}, {-11, 17}, {-14, 17}, {-13, 17},
	{-12, 17}, {12, 17}, {13, 18}, {14, 18},
}

var psHuffIIDDT = [29][2]int8{
	{0, 1}, {-1, 2}, {1, 3}, {-2, 4}, {2, 5},
	{-3, 6}, {3, 7}, {-4, 8}, {4, 9}, {-5, 10},
	{5, 11}, {-6, 12}, {6, 13}, {7, 14}, {-7, 15},
	{8, 17}, {-8, 17}, {9, 19}, {-14, 19}, {-13, 19},
	{-12, 19}, {-11, 20}, {-10, 20}, {-9, 20}, {10, 20},
	{11, 20}, {12, 20}, {13, 20}, {14, 20},
}

// Inter-channel coherence deltas (-7..7).
var psHuffICCDF = [15][2]int8{
	{0, 1}, {1, 2}, {-1, 3}, {2, 4}, {-2, 5},
	{3, 6}, {-3, 7}, {4, 8}, {5, 9}, {-4, 10},
	{6, 11}, {-5, 12}, {7, 13}, {-6, 14}, {-7, 14},
}

var psHuffICCDT = [15][2]int8{
	{0, 1}, {1, 2}, {-1, 3}, {2, 4}, {-2, 5},
	{3, 6}, {-3, 7}, {4, 8}, {-4, 9}, {5, 10},
	{-5, 11}, {6, 12}, {-6, 13}, {-7, 14}, {7, 14},
}

// Phase difference deltas (mod-8 wrapped, 0..7).
var psHuffIPDDF = [8][2]int8{
	{1, 3}, {4, 4}, {5, 4}, {3, 4}, {6, 4},
	{2, 4}, {7, 4}, {0, 1},
}

var psHuffIPDDT = [8][2]int8{
	{5, 4}, {4, 5}, {3, 5}, {2, 4}, {6, 4},
	{1, 3}, {7, 3}, {0, 1},
}

var psHuffOPDDF = [8][2]int8{
	{7, 3}, {1, 3}, {3, 4}, {6, 4}, {2, 4},
	{5, 5}, {4, 5}, {0, 1},
}

var psHuffOPDDT = [8][2]int8{
	{5, 4}, {2, 4}, {6, 4}, {4, 5}, {3, 5},
	{1, 3}, {7, 3}, {0, 1},
}

// psLeafBias marks tree leaves: a node entry below it decodes to entry
// plus the bias. Internal node indexes stay small non-negative ints, and
// deltas span -30..30, so the ranges cannot collide.
const psLeafBias = -1000

// buildPSTree builds the binary pair tree for a codebook listing. The
// canonical construction assigns each entry the next codeword of its
// length in listing order; a listing that does not tile the code space
// exactly (a transcription slip) returns nil, which the tables test turns
// into a failure.
func buildPSTree(entries [][2]int8) [][2]int16 {
	const unset = int16(-1)
	tree := [][2]int16{{unset, unset}}
	code := uint32(0) // codeword left-justified in 32 bits
	for i, e := range entries {
		val, bits := int(e[0]), uint(e[1])
		node := 0
		for b := uint(0); b < bits; b++ {
			bit := (code >> (31 - b)) & 1
			next := tree[node][bit]
			if b == bits-1 {
				if next != unset {
					return nil
				}
				tree[node][bit] = int16(val + psLeafBias)
				break
			}
			if next == unset {
				tree = append(tree, [2]int16{unset, unset})
				next = int16(len(tree) - 1)
				tree[node][bit] = next
			} else if next < 0 {
				return nil // descends through a leaf
			}
			node = int(next)
		}
		code += 1 << (32 - bits)
		if code == 0 && i != len(entries)-1 {
			return nil // code space exhausted early
		}
	}
	if code != 0 {
		return nil // Kraft inequality strict: incomplete tree
	}
	return tree
}

// psHuffDecode walks a PS pair tree; the deepest spec code is 20 bits.
func psHuffDecode(r *bitReader, tree [][2]int16) int {
	idx := 0
	for range 24 {
		v := tree[idx][r.bit()]
		if v < 0 {
			return int(v) - psLeafBias
		}
		idx = int(v)
	}
	return 0
}

var (
	psTreeIIDFineDF = buildPSTree(psHuffIIDFineDF[:])
	psTreeIIDFineDT = buildPSTree(psHuffIIDFineDT[:])
	psTreeIIDDF     = buildPSTree(psHuffIIDDF[:])
	psTreeIIDDT     = buildPSTree(psHuffIIDDT[:])
	psTreeICCDF     = buildPSTree(psHuffICCDF[:])
	psTreeICCDT     = buildPSTree(psHuffICCDT[:])
	psTreeIPDDF     = buildPSTree(psHuffIPDDF[:])
	psTreeIPDDT     = buildPSTree(psHuffIPDDT[:])
	psTreeOPDDF     = buildPSTree(psHuffOPDDF[:])
	psTreeOPDDT     = buildPSTree(psHuffOPDDT[:])
)

// Parameter band geometry, indexed by the 34-band flag (0: the 10/20-band
// modes processed at 20 bands, 1: 34 bands).
var (
	psNrParBands    = [2]int{20, 34} // stereo parameter bands b
	psNrBands       = [2]int{71, 91} // hybrid sub-subband channels k
	psNrAllpass     = [2]int{30, 50} // channels decorrelated by the allpass cascade
	psDecayCutoff   = [2]int{10, 32} // first channel the decay slope attenuates
	psShortDelay    = [2]int{42, 62} // first channel using the one-slot delay
	psNrIPDOPDBands = [2]int{11, 17} // phase parameter bands
)

// psKToI20/34 map each hybrid channel k to its stereo parameter band
// (Tables 8.48 and 8.49).
var psKToI20 = [71]int8{
	1, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 14, 15,
	15, 15, 16, 16, 16, 16, 17, 17, 17, 17, 17, 18, 18, 18, 18, 18, 18, 18, 18,
	18, 18, 18, 18, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19,
	19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19, 19,
}

var psKToI34 = [91]int8{
	0, 1, 2, 3, 4, 5, 6, 6, 7, 2, 1, 0, 10, 10, 4, 5, 6, 7, 8,
	9, 10, 11, 12, 9, 14, 11, 12, 13, 14, 15, 16, 13, 16, 17, 18, 19, 20, 21,
	22, 22, 23, 23, 24, 24, 25, 25, 26, 26, 27, 27, 27, 28, 28, 28, 29, 29, 29,
	30, 30, 30, 31, 31, 31, 31, 32, 32, 32, 32, 33, 33, 33, 33, 33, 33, 33, 33,
	33, 33, 33, 33, 33, 33, 33, 33, 33, 33, 33, 33, 33, 33, 33,
}

// Hybrid filter prototypes (Tables 8.32-8.35): 13-tap symmetric lowpass
// halves, g[6] the centre tap. The full filters are modulated from these
// at init.
var (
	psProtoQ8 = [7]float64{ // band 0, 8-way split (10/20-band modes)
		0.00746082949812, 0.02270420949825, 0.04546865930473, 0.07266113929591,
		0.09885108575264, 0.11793710567217, 0.125,
	}
	psProtoQ12 = [7]float64{ // band 0, 12-way split (34-band mode)
		0.04081179924692, 0.03812810994926, 0.05144908135699, 0.06399831151592,
		0.07428313801106, 0.08100347892914, 0.08333333333333,
	}
	psProtoQ8Alt = [7]float64{ // band 1, 8-way split (34-band mode)
		0.01565675600122, 0.03752716391991, 0.05417891378782, 0.08417044116767,
		0.10307344158036, 0.12222452249753, 0.125,
	}
	psProtoQ4 = [7]float64{ // bands 2-4, 4-way split (34-band mode)
		-0.05908211155639, -0.04871498374946, 0.0, 0.07778723915851,
		0.16486303567403, 0.23279856662996, 0.25,
	}
	// psProtoQ2 is the real 2-way filter for bands 1 and 2 in the
	// 10/20-band modes; its even off-centre taps are zero by design.
	psProtoQ2 = [7]float32{
		0, 0.01899487526049, 0, -0.07293139167538, 0, 0.30596630545168, 0.5,
	}
)

// Modulated hybrid filters, [sub-filter][tap][re/im]. The full 13 taps are
// expanded at init: proto[12-n] = proto[n], and the modulation
// e^(-j*2*pi*(q+1/2)*(n-6)/N) conjugates across the centre.
var (
	psFilter20x8  [8][13][2]float32
	psFilter34x12 [12][13][2]float32
	psFilter34x8  [8][13][2]float32
	psFilter34x4  [4][13][2]float32
)

func psMakeFilters(out [][13][2]float32, proto *[7]float64) {
	bands := len(out)
	for q := range out {
		for n := 0; n < 13; n++ {
			p := proto[min(n, 12-n)]
			theta := 2 * math.Pi * (float64(q) + 0.5) * float64(n-6) / float64(bands)
			s, c := math.Sincos(theta)
			out[q][n][0] = float32(p * c)
			out[q][n][1] = float32(p * -s)
		}
	}
}

// Dequantized parameter values. The IID quantizers are listed in dB
// (Tables 8.24, 8.25); the linear ratios come out at init. The ICC values
// are the correlation list of Table 8.27.
var (
	psIIDStepsDB     = [8]float64{0, 2, 4, 7, 10, 14, 18, 25}
	psIIDFineStepsDB = [16]float64{0, 2, 4, 6, 8, 10, 13, 16, 19, 22, 25, 30, 35, 40, 45, 50}
	psICCRho         = [8]float64{1, 0.937, 0.84118, 0.60092, 0.36764, 0, -0.589, -1}
)

// psMixLUT holds the mixing matrices for every quantized (iid, icc) pair:
// [0] the sum/difference matrices of mixing procedure A (8.6.4.6.2.1),
// [1] the covariance-driven matrices of procedure B (8.6.4.6.2.2), used
// when the ICC mode selects it. Rows 0..14 are the default IID quantizer
// at index iid+7, rows 15..45 the fine quantizer at iid+30 (one shared
// iid+7+23*quant expression covers both).
var psMixLUT [2][46][8][4]float32

// psPhaseSmooth is the smoothed phase unit vector for a (previous two,
// current) triple of quantized pi/4 phase indexes, weighted 1/4, 1/2, 1
// (8.6.4.6.3's phase parameter smoother); indexed pd0*64+pd1*8+pd2.
var psPhaseSmooth [512][2]float32

// Decorrelator fractional-delay phases per hybrid channel: the allpass
// links' q(m) delays and the direct path's 0.39 (8.6.4.6.4). fCenter is
// each channel's centre frequency in QMF band units.
var (
	psPhiFract [2][50][2]float32
	psQFract   [2][50][3][2]float32
)

// psFCenter20 lists the sub-subband centre frequencies of the 10/20-band
// hybrid in eighths of a QMF band; channels past the hybrid sit at
// k - 6.5 whole bands. psFCenter34 is the 34-band equivalent in
// twenty-fourths, with k - 26.5 past the hybrid.
var (
	psFCenter20 = [10]int8{-3, -1, 1, 3, 5, 7, 10, 14, 18, 22}
	psFCenter34 = [32]int8{
		2, 6, 10, 14, 18, 22, 26, 30,
		34, -10, -6, -2, 51, 57, 15, 21,
		27, 33, 39, 45, 54, 66, 78, 42,
		102, 66, 78, 90, 102, 114, 126, 90,
	}
)

// Decorrelator constants (8.6.4.6.4).
const (
	psPeakDecayFactor  = 0.76592833836465
	psTransientImpact  = 1.5
	psASmooth          = 0.25
	psDecaySlope       = 0.05
	psFractDelayDirect = 0.39
)

// psAllpassA is the allpass links' base coefficients; their delays of 3,
// 4, and 5 slots are baked into the cascade's buffer indexing.
var (
	psAllpassA    = [3]float64{0.65143905753106, 0.56471812200776, 0.48954165955695}
	psFractDelays = [3]float64{0.43, 0.75, 0.347}
)

func init() {
	psMakeFilters(psFilter20x8[:], &psProtoQ8)
	psMakeFilters(psFilter34x12[:], &psProtoQ12)
	psMakeFilters(psFilter34x8[:], &psProtoQ8Alt)
	psMakeFilters(psFilter34x4[:], &psProtoQ4)

	// The 46-row linear IID list: default quantizer descending negative to
	// ascending positive, then the fine quantizer likewise.
	linear := func(steps []float64, idx int) float64 {
		if idx < 0 {
			return math.Pow(10, -steps[-idx]/20)
		}
		return math.Pow(10, steps[idx]/20)
	}
	var iidLin [46]float64
	for i := range 15 {
		iidLin[i] = linear(psIIDStepsDB[:], i-7)
	}
	for i := range 31 {
		iidLin[15+i] = linear(psIIDFineStepsDB[:], i-15)
	}
	for i, c := range iidLin {
		c1 := math.Sqrt2 / math.Sqrt(1+c*c)
		c2 := c * c1
		for icc := range 8 {
			rho := psICCRho[icc]
			// Procedure A: rotation of the sum/difference signals.
			alpha := 0.5 * math.Acos(rho)
			beta := alpha * (c1 - c2) / math.Sqrt2
			psMixLUT[0][i][icc][0] = float32(c2 * math.Cos(beta+alpha))
			psMixLUT[0][i][icc][1] = float32(c1 * math.Cos(beta-alpha))
			psMixLUT[0][i][icc][2] = float32(c2 * math.Sin(beta+alpha))
			psMixLUT[0][i][icc][3] = float32(c1 * math.Sin(beta-alpha))
			// Procedure B: the covariance synthesis form.
			rho = math.Max(rho, 0.05)
			alpha = 0.5 * math.Atan2(2*c*rho, c*c-1)
			if alpha < 0 {
				alpha += math.Pi / 2
			}
			mu := c + 1/c
			mu = math.Sqrt(1 + (4*rho*rho-4)/(mu*mu))
			gamma := math.Atan(math.Sqrt((1 - mu) / (1 + mu)))
			psMixLUT[1][i][icc][0] = float32(math.Sqrt2 * math.Cos(alpha) * math.Cos(gamma))
			psMixLUT[1][i][icc][1] = float32(math.Sqrt2 * math.Sin(alpha) * math.Cos(gamma))
			psMixLUT[1][i][icc][2] = float32(-math.Sqrt2 * math.Sin(alpha) * math.Sin(gamma))
			psMixLUT[1][i][icc][3] = float32(math.Sqrt2 * math.Cos(alpha) * math.Sin(gamma))
		}
	}

	for pd0 := range 8 {
		for pd1 := range 8 {
			for pd2 := range 8 {
				re := 0.25*psPhaseCos(pd0) + 0.5*psPhaseCos(pd1) + psPhaseCos(pd2)
				im := 0.25*psPhaseSin(pd0) + 0.5*psPhaseSin(pd1) + psPhaseSin(pd2)
				mag := 1 / math.Hypot(re, im)
				psPhaseSmooth[pd0*64+pd1*8+pd2][0] = float32(re * mag)
				psPhaseSmooth[pd0*64+pd1*8+pd2][1] = float32(im * mag)
			}
		}
	}

	for is34 := range 2 {
		for k := 0; k < psNrAllpass[is34]; k++ {
			var f float64
			if is34 == 0 {
				if k < len(psFCenter20) {
					f = float64(psFCenter20[k]) * 0.125
				} else {
					f = float64(k) - 6.5
				}
			} else {
				if k < len(psFCenter34) {
					f = float64(psFCenter34[k]) / 24
				} else {
					f = float64(k) - 26.5
				}
			}
			for m := range 3 {
				s, c := math.Sincos(-math.Pi * psFractDelays[m] * f)
				psQFract[is34][k][m][0] = float32(c)
				psQFract[is34][k][m][1] = float32(s)
			}
			s, c := math.Sincos(-math.Pi * psFractDelayDirect * f)
			psPhiFract[is34][k][0] = float32(c)
			psPhiFract[is34][k][1] = float32(s)
		}
	}
}

// psPhaseCos/Sin evaluate the quantized pi/4-step phase angles exactly.
func psPhaseCos(i int) float64 {
	switch i & 7 {
	case 0:
		return 1
	case 1, 7:
		return math.Sqrt2 / 2
	case 2, 6:
		return 0
	case 3, 5:
		return -math.Sqrt2 / 2
	default:
		return -1
	}
}

func psPhaseSin(i int) float64 {
	return psPhaseCos(i - 2)
}
