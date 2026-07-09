package mp3

import "math"

// Band and window tables for Layer III. The scalefactor band width tables
// (one row per sample rate family, 11.025 and 12 kHz sharing a row; see
// Header.rateRow) and the scalefactor partition table follow the ISO band
// tables as arranged by minimp3. The synthesis window is the ISO
// Table B.3 coefficient set as carried by PDMP3 via go-mp3.
// Everything derivable is computed at init from the spec formulas.

// sfbLong, sfbShort, and sfbMixed hold scalefactor band widths in
// spectral lines, zero-terminated. Long rows have 22 bands, short rows 39
// window entries (13 bands times 3 windows), mixed rows a long prefix
// followed by window entries. Widths per row sum to 576.
var sfbLong = [8][23]uint8{
	{6, 6, 6, 6, 6, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 38, 46, 52, 60, 68, 58, 54, 0},
	{12, 12, 12, 12, 12, 12, 16, 20, 24, 28, 32, 40, 48, 56, 64, 76, 90, 2, 2, 2, 2, 2, 0},
	{6, 6, 6, 6, 6, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 38, 46, 52, 60, 68, 58, 54, 0},
	{6, 6, 6, 6, 6, 6, 8, 10, 12, 14, 16, 18, 22, 26, 32, 38, 46, 54, 62, 70, 76, 36, 0},
	{6, 6, 6, 6, 6, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 38, 46, 52, 60, 68, 58, 54, 0},
	{4, 4, 4, 4, 4, 4, 6, 6, 8, 8, 10, 12, 16, 20, 24, 28, 34, 42, 50, 54, 76, 158, 0},
	{4, 4, 4, 4, 4, 4, 6, 6, 6, 8, 10, 12, 16, 18, 22, 28, 34, 40, 46, 54, 54, 192, 0},
	{4, 4, 4, 4, 4, 4, 6, 6, 8, 10, 12, 16, 20, 24, 30, 38, 46, 56, 68, 84, 102, 26, 0},
}

var sfbShort = [8][40]uint8{
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 24, 24, 24, 30, 30, 30, 40, 40, 40, 18, 18, 18, 0},
	{8, 8, 8, 8, 8, 8, 8, 8, 8, 12, 12, 12, 16, 16, 16, 20, 20, 20, 24, 24, 24, 28, 28, 28, 36, 36, 36, 2, 2, 2, 2, 2, 2, 2, 2, 2, 26, 26, 26, 0},
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 6, 6, 6, 8, 8, 8, 10, 10, 10, 14, 14, 14, 18, 18, 18, 26, 26, 26, 32, 32, 32, 42, 42, 42, 18, 18, 18, 0},
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 24, 24, 24, 32, 32, 32, 44, 44, 44, 12, 12, 12, 0},
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 24, 24, 24, 30, 30, 30, 40, 40, 40, 18, 18, 18, 0},
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 22, 22, 22, 30, 30, 30, 56, 56, 56, 0},
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 6, 6, 6, 10, 10, 10, 12, 12, 12, 14, 14, 14, 16, 16, 16, 20, 20, 20, 26, 26, 26, 66, 66, 66, 0},
	{4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 8, 8, 8, 12, 12, 12, 16, 16, 16, 20, 20, 20, 26, 26, 26, 34, 34, 34, 42, 42, 42, 12, 12, 12, 0},
}

var sfbMixed = [8][40]uint8{
	{6, 6, 6, 6, 6, 6, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 24, 24, 24, 30, 30, 30, 40, 40, 40, 18, 18, 18, 0, 0, 0, 0},
	{12, 12, 12, 4, 4, 4, 8, 8, 8, 12, 12, 12, 16, 16, 16, 20, 20, 20, 24, 24, 24, 28, 28, 28, 36, 36, 36, 2, 2, 2, 2, 2, 2, 2, 2, 2, 26, 26, 26, 0},
	{6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 8, 8, 8, 10, 10, 10, 14, 14, 14, 18, 18, 18, 26, 26, 26, 32, 32, 32, 42, 42, 42, 18, 18, 18, 0, 0, 0, 0},
	{6, 6, 6, 6, 6, 6, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 24, 24, 24, 32, 32, 32, 44, 44, 44, 12, 12, 12, 0, 0, 0, 0},
	{6, 6, 6, 6, 6, 6, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 24, 24, 24, 30, 30, 30, 40, 40, 40, 18, 18, 18, 0, 0, 0, 0},
	{4, 4, 4, 4, 4, 4, 6, 6, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 18, 18, 18, 22, 22, 22, 30, 30, 30, 56, 56, 56, 0, 0},
	{4, 4, 4, 4, 4, 4, 6, 6, 4, 4, 4, 6, 6, 6, 6, 6, 6, 10, 10, 10, 12, 12, 12, 14, 14, 14, 16, 16, 16, 20, 20, 20, 26, 26, 26, 66, 66, 66, 0, 0},
	{4, 4, 4, 4, 4, 4, 6, 6, 4, 4, 4, 6, 6, 6, 8, 8, 8, 12, 12, 12, 16, 16, 16, 20, 20, 20, 26, 26, 26, 34, 34, 34, 42, 42, 42, 12, 12, 12, 0, 0},
}

var scfPartitions = [3][28]uint8{
	{6, 5, 5, 5, 6, 5, 5, 5, 6, 5, 7, 3, 11, 10, 0, 0, 7, 7, 7, 0, 6, 6, 6, 3, 8, 8, 5, 0},
	{8, 9, 6, 12, 6, 9, 9, 9, 6, 9, 12, 6, 15, 18, 0, 0, 6, 15, 12, 0, 6, 12, 9, 6, 6, 18, 9, 0},
	{9, 9, 6, 12, 9, 9, 9, 9, 9, 9, 12, 6, 18, 18, 0, 0, 12, 12, 12, 0, 12, 9, 9, 6, 15, 12, 9, 0},
}

// scfcDecode packs the MPEG-1 scalefactor size pair (slen1 in the high
// two bits, slen2 in the low two) per scalefac_compress value.
var scfcDecode = [16]uint8{0, 1, 2, 3, 12, 5, 6, 7, 9, 10, 11, 13, 14, 15, 18, 19}

// scfMod holds the mixed-radix moduli that decode the 9-bit MPEG-2/2.5
// scalefac_compress into four size fields: three ranges for the normal
// mode, three more for the intensity-stereo variant (ISO 13818-3
// section 2.4.3.2).
var scfMod = [24]uint8{
	5, 5, 4, 4, 5, 5, 4, 1, 4, 3, 1, 1,
	5, 6, 6, 1, 4, 4, 4, 1, 4, 3, 1, 1,
}

// preamp is the pretab amplification added to long scalefactor bands 11
// to 20 when preflag is set.
var preamp = [10]uint8{1, 1, 1, 1, 2, 2, 3, 3, 3, 2}

// pow43 is v^(4/3) for the requantizer; 8206 is the largest decodable
// value (15 plus 13 linbits).
var pow43 [8207]float64

// pow2qBias offsets quarter-step exponents into pow2q. The most negative
// exponent is global gain 0 minus 210, minus the widest scaled
// scalefactor term, minus the mid/side fold.
const pow2qBias = 452

// pow2q is 2^(q/4) for quarter-step exponents q in [-pow2qBias, 47].
var pow2q [pow2qBias + 48]float64

// csTab and caTab are the alias reduction butterfly coefficients derived
// from the eight ci constants (ISO 11172-3 Table B.9).
var csTab, caTab [8]float64

// panPos is the MPEG-1 intensity stereo pan table: kl, kr pairs per
// is_pos 0..6, from the tan(is_pos*pi/12) ratios.
var panPos [7][2]float32

// cosN12f and cosN36f are the short and long IMDCT cosine kernels, and
// imdctWinF the four hybrid window shapes over 36 points: normal, start,
// short, and stop (ISO 11172-3 section 2.4.3.4.10.3). Computed in
// float64, stored float32 for the hot loops. They are filled by this
// file's init, in one place, because a sibling file's init would run
// first (file order) and snapshot zeros.
var cosN12f [6][12]float32
var cosN36f [18][36]float32
var imdctWinF [4][36]float32

// synthNWin is the synthesis filterbank matrixing kernel
// (ISO 11172-3 section 2.4.3.4.10.4).
var synthNWin [64][32]float32

func init() {
	for i := range pow43 {
		pow43[i] = math.Pow(float64(i), 4.0/3.0)
	}
	for i := range pow2q {
		pow2q[i] = math.Exp2(float64(i-pow2qBias) / 4)
	}
	ci := [8]float64{-0.6, -0.535, -0.33, -0.185, -0.095, -0.041, -0.0142, -0.0037}
	for i, c := range ci {
		csTab[i] = 1 / math.Sqrt(1+c*c)
		caTab[i] = c / math.Sqrt(1+c*c)
	}
	for i := 0; i < 7; i++ {
		if i == 6 {
			panPos[i] = [2]float32{1, 0} // tan(pi/2): all left
			continue
		}
		r := math.Tan(float64(i) * math.Pi / 12)
		panPos[i] = [2]float32{float32(r / (1 + r)), float32(1 / (1 + r))}
	}

	var imdctWin [4][36]float64
	for i := 0; i < 36; i++ {
		imdctWin[0][i] = math.Sin(math.Pi / 36 * (float64(i) + 0.5))
	}
	for i := 0; i < 18; i++ {
		imdctWin[1][i] = math.Sin(math.Pi / 36 * (float64(i) + 0.5))
	}
	for i := 18; i < 24; i++ {
		imdctWin[1][i] = 1
	}
	for i := 24; i < 30; i++ {
		imdctWin[1][i] = math.Sin(math.Pi / 12 * (float64(i) + 0.5 - 18))
	}
	for i := 0; i < 12; i++ {
		imdctWin[2][i] = math.Sin(math.Pi / 12 * (float64(i) + 0.5))
	}
	for i := 6; i < 12; i++ {
		imdctWin[3][i] = math.Sin(math.Pi / 12 * (float64(i) + 0.5 - 6))
	}
	for i := 12; i < 18; i++ {
		imdctWin[3][i] = 1
	}
	for i := 18; i < 36; i++ {
		imdctWin[3][i] = math.Sin(math.Pi / 36 * (float64(i) + 0.5))
	}
	for w := range imdctWin {
		for i := range imdctWin[w] {
			imdctWinF[w][i] = float32(imdctWin[w][i])
		}
	}
	for i := 0; i < 6; i++ {
		for j := 0; j < 12; j++ {
			cosN12f[i][j] = float32(math.Cos(math.Pi / 24 * (2*float64(j) + 1 + 6) * (2*float64(i) + 1)))
		}
	}
	for i := 0; i < 18; i++ {
		for j := 0; j < 36; j++ {
			cosN36f[i][j] = float32(math.Cos(math.Pi / 72 * (2*float64(j) + 1 + 18) * (2*float64(i) + 1)))
		}
	}
	for i := 0; i < 64; i++ {
		for j := 0; j < 32; j++ {
			synthNWin[i][j] = float32(math.Cos(float64((16+i)*(2*j+1)) * (math.Pi / 64)))
		}
	}
}

// bands describes the granule's scalefactor band shape: the width table
// to walk, split into long band entries and short window entries, plus
// the hybrid filterbank's long/short subband split for mixed blocks.
type bands struct {
	widths []uint8 // zero-terminated width entries, long then short
	nLong  int     // long entries at the head
	nShort int     // short window entries after them
	// longSubbands is how many of the 32 filterbank subbands a mixed
	// block treats as long; the reorder base and the scalefactor walk
	// must agree with it (nLong entries spanning longSubbands*18 lines).
	longSubbands int
}

// bandsFor resolves the band shape for a granule.
func bandsFor(h Header, g *grInfo) bands {
	row := h.rateRow()
	switch {
	case g.blockType != blockShort:
		return bands{widths: sfbLong[row][:], nLong: 22}
	case g.mixed:
		n, sb := 8, 2
		if h.Version != MPEG1 {
			n, sb = 6, 2
			if row == 1 {
				// The 8 kHz row's mixed prefix is coarser: nine width
				// entries spanning four subbands (72 lines), the split
				// minimp3 applies. Everything downstream keys off these
				// two numbers, so they must move together.
				n, sb = 9, 4
			}
		}
		return bands{widths: sfbMixed[row][:], nLong: n, nShort: 30, longSubbands: sb}
	default:
		return bands{widths: sfbShort[row][:], nShort: 39}
	}
}
