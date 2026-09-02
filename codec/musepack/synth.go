package musepack

// The 32-band polyphase synthesis filterbank, ported from libmpcdec's
// synth_filter.c: the MPEG-1 Layer II synthesis (ISO 11172-3 figure C.3) with
// Lee's fast DCT for the 64 new V values per subband sample and the Di_opt
// window over a 1024-sample V history. Arithmetic order follows the reference
// exactly, and every product is rounded to float32 before it is summed so the
// compiler cannot fuse it into a differently rounded multiply-add.

// vMem is the V history the filter reads behind the newest block, and vLen the
// whole buffer: the 36 new blocks of a frame plus the 960 carried over.
const (
	vMem = 2304
	vLen = vMem + 960
)

// synthesis is one channel's filter state.
type synthesis struct {
	v [vLen]float32
}

// diOpt is the synthesis window, Di_opt[32][16] in the reference, stored
// there as integers over 65536; the division is exact in float32.
var diOpt = func() (w [32][16]float32) {
	for i := range diOptInt {
		for j := range diOptInt[i] {
			w[i][j] = float32(float64(diOptInt[i][j]) / 65536)
		}
	}
	return w
}()

var diOptInt = [32][16]int32{
	{0, -29, 213, -459, 2037, -5153, 6574, -37489, 75038, 37489, 6574, 5153, 2037, 459, 213, 29},
	{-1, -31, 218, -519, 2000, -5517, 5959, -39336, 74992, 35640, 7134, 4788, 2063, 401, 208, 26},
	{-1, -35, 222, -581, 1952, -5879, 5288, -41176, 74856, 33791, 7640, 4425, 2080, 347, 202, 24},
	{-1, -38, 225, -645, 1893, -6237, 4561, -43006, 74630, 31947, 8092, 4063, 2087, 294, 196, 21},
	{-1, -41, 227, -711, 1822, -6589, 3776, -44821, 74313, 30112, 8492, 3705, 2085, 244, 190, 19},
	{-1, -45, 228, -779, 1739, -6935, 2935, -46617, 73908, 28289, 8840, 3351, 2075, 197, 183, 17},
	{-1, -49, 228, -848, 1644, -7271, 2037, -48390, 73415, 26482, 9139, 3004, 2057, 153, 176, 16},
	{-2, -53, 227, -919, 1535, -7597, 1082, -50137, 72835, 24694, 9389, 2663, 2032, 111, 169, 14},
	{-2, -58, 224, -991, 1414, -7910, 70, -51853, 72169, 22929, 9592, 2330, 2001, 72, 161, 13},
	{-2, -63, 221, -1064, 1280, -8209, -998, -53534, 71420, 21189, 9750, 2006, 1962, 36, 154, 11},
	{-2, -68, 215, -1137, 1131, -8491, -2122, -55178, 70590, 19478, 9863, 1692, 1919, 2, 147, 10},
	{-3, -73, 208, -1210, 970, -8755, -3300, -56778, 69679, 17799, 9935, 1388, 1870, -29, 139, 9},
	{-3, -79, 200, -1283, 794, -8998, -4533, -58333, 68692, 16155, 9966, 1095, 1817, -57, 132, 8},
	{-4, -85, 189, -1356, 605, -9219, -5818, -59838, 67629, 14548, 9959, 814, 1759, -83, 125, 7},
	{-4, -91, 177, -1428, 402, -9416, -7154, -61289, 66494, 12980, 9916, 545, 1698, -106, 117, 7},
	{-5, -97, 163, -1498, 185, -9585, -8540, -62684, 65290, 11455, 9838, 288, 1634, -127, 111, 6},
	{-5, -104, 146, -1567, -45, -9727, -9975, -64019, 64019, 9975, 9727, 45, 1567, -146, 104, 5},
	{-6, -111, 127, -1634, -288, -9838, -11455, -65290, 62684, 8540, 9585, -185, 1498, -163, 97, 5},
	{-7, -117, 106, -1698, -545, -9916, -12980, -66494, 61289, 7154, 9416, -402, 1428, -177, 91, 4},
	{-7, -125, 83, -1759, -814, -9959, -14548, -67629, 59838, 5818, 9219, -605, 1356, -189, 85, 4},
	{-8, -132, 57, -1817, -1095, -9966, -16155, -68692, 58333, 4533, 8998, -794, 1283, -200, 79, 3},
	{-9, -139, 29, -1870, -1388, -9935, -17799, -69679, 56778, 3300, 8755, -970, 1210, -208, 73, 3},
	{-10, -147, -2, -1919, -1692, -9863, -19478, -70590, 55178, 2122, 8491, -1131, 1137, -215, 68, 2},
	{-11, -154, -36, -1962, -2006, -9750, -21189, -71420, 53534, 998, 8209, -1280, 1064, -221, 63, 2},
	{-13, -161, -72, -2001, -2330, -9592, -22929, -72169, 51853, -70, 7910, -1414, 991, -224, 58, 2},
	{-14, -169, -111, -2032, -2663, -9389, -24694, -72835, 50137, -1082, 7597, -1535, 919, -227, 53, 2},
	{-16, -176, -153, -2057, -3004, -9139, -26482, -73415, 48390, -2037, 7271, -1644, 848, -228, 49, 1},
	{-17, -183, -197, -2075, -3351, -8840, -28289, -73908, 46617, -2935, 6935, -1739, 779, -228, 45, 1},
	{-19, -190, -244, -2085, -3705, -8492, -30112, -74313, 44821, -3776, 6589, -1822, 711, -227, 41, 1},
	{-21, -196, -294, -2087, -4063, -8092, -31947, -74630, 43006, -4561, 6237, -1893, 645, -225, 38, 1},
	{-24, -202, -347, -2080, -4425, -7640, -33791, -74856, 41176, -5288, 5879, -1952, 581, -222, 35, 1},
	{-26, -208, -401, -2063, -4788, -7134, -35640, -74992, 39336, -5959, 5517, -2000, 519, -218, 31, 1},
}

// vTaps is where the window's sixteen taps read in the V history, relative to
// the newest block. The filter loop spells them as constants over a slice cut
// to the last tap, so the compiler proves every index in range once per
// sample; the array is the same list for the tests.
var vTaps = [16]int{0, 96, 128, 224, 256, 352, 384, 480, 512, 608, 640, 736, 768, 864, 896, 992}

// vSpan is one past the last tap: the slice a sample's taps are read from.
const vSpan = 993

// The DCT's rotation constants, float32 as the reference spells them.
const (
	c0 = float32(0.5024192929)
	c1 = float32(0.5224986076)
	c2 = float32(0.5669440627)
	c3 = float32(0.6468217969)
	c4 = float32(0.7881546021)
	c5 = float32(1.0606776476)
	c6 = float32(1.7224471569)
	c7 = float32(5.1011486053)

	d0 = float32(0.5097956061)
	d1 = float32(0.6013448834)
	d2 = float32(0.8999761939)
	d3 = float32(2.5629155636)

	e0 = float32(0.5411961079)
	e1 = float32(1.3065630198)
	f0 = float32(0.7071067691)

	g0  = float32(0.5006030202)
	g1  = float32(0.5054709315)
	g2  = float32(0.5154473186)
	g3  = float32(0.5310425758)
	g4  = float32(0.5531039238)
	g5  = float32(0.5829349756)
	g6  = float32(0.6225041151)
	g7  = float32(0.6748083234)
	g8  = float32(0.7445362806)
	g9  = float32(0.8393496275)
	g10 = float32(0.9725682139)
	g11 = float32(1.1694399118)
	g12 = float32(1.4841645956)
	g13 = float32(2.0577809811)
	g14 = float32(3.4076085091)
	g15 = float32(10.1900081635)
)

// mul rounds a product to float32 so it cannot be fused into a following
// addition.
func mul(a, b float32) float32 { return float32(a * b) }

// computeNewV computes the 64 new V values for one subband sample
// (mpc_compute_new_V), s being the 32 subband values.
func computeNewV(s *[32]float32, pV []float32) {
	var tmp float32

	A00 := s[0] + s[31]
	A01 := s[1] + s[30]
	A02 := s[2] + s[29]
	A03 := s[3] + s[28]
	A04 := s[4] + s[27]
	A05 := s[5] + s[26]
	A06 := s[6] + s[25]
	A07 := s[7] + s[24]
	A08 := s[8] + s[23]
	A09 := s[9] + s[22]
	A10 := s[10] + s[21]
	A11 := s[11] + s[20]
	A12 := s[12] + s[19]
	A13 := s[13] + s[18]
	A14 := s[14] + s[17]
	A15 := s[15] + s[16]

	B00 := A00 + A15
	B01 := A01 + A14
	B02 := A02 + A13
	B03 := A03 + A12
	B04 := A04 + A11
	B05 := A05 + A10
	B06 := A06 + A09
	B07 := A07 + A08
	B08 := mul(A00-A15, c0)
	B09 := mul(A01-A14, c1)
	B10 := mul(A02-A13, c2)
	B11 := mul(A03-A12, c3)
	B12 := mul(A04-A11, c4)
	B13 := mul(A05-A10, c5)
	B14 := mul(A06-A09, c6)
	B15 := mul(A07-A08, c7)

	A00 = B00 + B07
	A01 = B01 + B06
	A02 = B02 + B05
	A03 = B03 + B04
	A04 = mul(B00-B07, d0)
	A05 = mul(B01-B06, d1)
	A06 = mul(B02-B05, d2)
	A07 = mul(B03-B04, d3)
	A08 = B08 + B15
	A09 = B09 + B14
	A10 = B10 + B13
	A11 = B11 + B12
	A12 = mul(B08-B15, d0)
	A13 = mul(B09-B14, d1)
	A14 = mul(B10-B13, d2)
	A15 = mul(B11-B12, d3)

	B00 = A00 + A03
	B01 = A01 + A02
	B02 = mul(A00-A03, e0)
	B03 = mul(A01-A02, e1)
	B04 = A04 + A07
	B05 = A05 + A06
	B06 = mul(A04-A07, e0)
	B07 = mul(A05-A06, e1)
	B08 = A08 + A11
	B09 = A09 + A10
	B10 = mul(A08-A11, e0)
	B11 = mul(A09-A10, e1)
	B12 = A12 + A15
	B13 = A13 + A14
	B14 = mul(A12-A15, e0)
	B15 = mul(A13-A14, e1)

	A00 = B00 + B01
	A01 = mul(B00-B01, f0)
	A02 = B02 + B03
	A03 = mul(B02-B03, f0)
	A04 = B04 + B05
	A05 = mul(B04-B05, f0)
	A06 = B06 + B07
	A07 = mul(B06-B07, f0)
	A08 = B08 + B09
	A09 = mul(B08-B09, f0)
	A10 = B10 + B11
	A11 = mul(B10-B11, f0)
	A12 = B12 + B13
	A13 = mul(B12-B13, f0)
	A14 = B14 + B15
	A15 = mul(B14-B15, f0)

	pV[48] = -A00
	pV[0] = A01
	pV[8] = A03
	pV[40] = -A02 - pV[8]
	pV[12] = A07
	pV[4] = A05 + pV[12]
	pV[36] = -(pV[4] + A06)
	pV[44] = -A04 - A06 - A07
	pV[14] = A15
	pV[10] = A11 + pV[14]
	pV[6] = pV[10] + A13
	pV[2] = A09 + A13 + A15
	pV[34] = -pV[2] - A14
	pV[38] = pV[34] + A09 - A10 - A11
	tmp = -(A12 + A14 + A15)
	pV[46] = tmp - A08
	pV[42] = tmp - A10 - A11

	A00 = mul(s[0]-s[31], g0)
	A01 = mul(s[1]-s[30], g1)
	A02 = mul(s[2]-s[29], g2)
	A03 = mul(s[3]-s[28], g3)
	A04 = mul(s[4]-s[27], g4)
	A05 = mul(s[5]-s[26], g5)
	A06 = mul(s[6]-s[25], g6)
	A07 = mul(s[7]-s[24], g7)
	A08 = mul(s[8]-s[23], g8)
	A09 = mul(s[9]-s[22], g9)
	A10 = mul(s[10]-s[21], g10)
	A11 = mul(s[11]-s[20], g11)
	A12 = mul(s[12]-s[19], g12)
	A13 = mul(s[13]-s[18], g13)
	A14 = mul(s[14]-s[17], g14)
	A15 = mul(s[15]-s[16], g15)

	B00 = A00 + A15
	B01 = A01 + A14
	B02 = A02 + A13
	B03 = A03 + A12
	B04 = A04 + A11
	B05 = A05 + A10
	B06 = A06 + A09
	B07 = A07 + A08
	B08 = mul(A00-A15, c0)
	B09 = mul(A01-A14, c1)
	B10 = mul(A02-A13, c2)
	B11 = mul(A03-A12, c3)
	B12 = mul(A04-A11, c4)
	B13 = mul(A05-A10, c5)
	B14 = mul(A06-A09, c6)
	B15 = mul(A07-A08, c7)

	A00 = B00 + B07
	A01 = B01 + B06
	A02 = B02 + B05
	A03 = B03 + B04
	A04 = mul(B00-B07, d0)
	A05 = mul(B01-B06, d1)
	A06 = mul(B02-B05, d2)
	A07 = mul(B03-B04, d3)
	A08 = B08 + B15
	A09 = B09 + B14
	A10 = B10 + B13
	A11 = B11 + B12
	A12 = mul(B08-B15, d0)
	A13 = mul(B09-B14, d1)
	A14 = mul(B10-B13, d2)
	A15 = mul(B11-B12, d3)

	B00 = A00 + A03
	B01 = A01 + A02
	B02 = mul(A00-A03, e0)
	B03 = mul(A01-A02, e1)
	B04 = A04 + A07
	B05 = A05 + A06
	B06 = mul(A04-A07, e0)
	B07 = mul(A05-A06, e1)
	B08 = A08 + A11
	B09 = A09 + A10
	B10 = mul(A08-A11, e0)
	B11 = mul(A09-A10, e1)
	B12 = A12 + A15
	B13 = A13 + A14
	B14 = mul(A12-A15, e0)
	B15 = mul(A13-A14, e1)

	A00 = B00 + B01
	A01 = mul(B00-B01, f0)
	A02 = B02 + B03
	A03 = mul(B02-B03, f0)
	A04 = B04 + B05
	A05 = mul(B04-B05, f0)
	A06 = B06 + B07
	A07 = mul(B06-B07, f0)
	A08 = B08 + B09
	A09 = mul(B08-B09, f0)
	A10 = B10 + B11
	A11 = mul(B10-B11, f0)
	A12 = B12 + B13
	A13 = mul(B12-B13, f0)
	A14 = B14 + B15
	A15 = mul(B14-B15, f0)

	pV[15] = A15
	pV[13] = A07 + pV[15]
	pV[11] = pV[13] + A11
	pV[5] = pV[11] + A05 + A13
	pV[9] = A03 + A11 + A15
	pV[7] = pV[9] + A13
	pV[1] = A01 + A09 + A13 + A15
	pV[33] = -pV[1] - A14
	pV[3] = A05 + A07 + A09 + A13 + A15
	pV[35] = -pV[3] - A06 - A14
	tmp = -(A10 + A11 + A13 + A14 + A15)
	pV[37] = tmp - A05 - A06 - A07
	pV[39] = tmp - A02 - A03
	tmp += A13 - A12
	pV[41] = tmp - A02 - A03
	pV[43] = tmp - A04 - A06 - A07
	tmp = -(A08 + A12 + A14 + A15)
	pV[47] = tmp - A00
	pV[45] = tmp - A04 - A06 - A07

	pV[32] = -pV[0]
	pV[31] = -pV[1]
	pV[30] = -pV[2]
	pV[29] = -pV[3]
	pV[28] = -pV[4]
	pV[27] = -pV[5]
	pV[26] = -pV[6]
	pV[25] = -pV[7]
	pV[24] = -pV[8]
	pV[23] = -pV[9]
	pV[22] = -pV[10]
	pV[21] = -pV[11]
	pV[20] = -pV[12]
	pV[19] = -pV[13]
	pV[18] = -pV[14]
	pV[17] = -pV[15]

	pV[63] = pV[33]
	pV[62] = pV[34]
	pV[61] = pV[35]
	pV[60] = pV[36]
	pV[59] = pV[37]
	pV[58] = pV[38]
	pV[57] = pV[39]
	pV[56] = pV[40]
	pV[55] = pV[41]
	pV[54] = pV[42]
	pV[53] = pV[43]
	pV[52] = pV[44]
	pV[51] = pV[45]
	pV[50] = pV[46]
	pV[49] = pV[47]
}

// frame synthesizes one frame: y holds 36 subband samples of 32 bands, out
// receives 1152 PCM samples (mpc_decoder_synthese_filter_float for one
// channel, writing planar rather than interleaved).
func (s *synthesis) frame(y *[36][32]float32, out []float32) {
	// The newest 960 V values move behind the space the frame's 36 blocks
	// will fill, so the taps read a continuous history.
	copy(s.v[vMem:], s.v[:960])
	at := vMem
	for n := 0; n < 36; n++ {
		at -= 64
		pV := s.v[at : at+64 : at+64]
		computeNewV(&y[n], pV)
		o := out[n*32 : n*32+32]
		for k := 0; k < 32; k++ {
			v := s.v[at+k : at+k+vSpan]
			d := &diOpt[k]
			o[k] = mul(v[0], d[0]) + mul(v[96], d[1]) + mul(v[128], d[2]) + mul(v[224], d[3]) +
				mul(v[256], d[4]) + mul(v[352], d[5]) + mul(v[384], d[6]) + mul(v[480], d[7]) +
				mul(v[512], d[8]) + mul(v[608], d[9]) + mul(v[640], d[10]) + mul(v[736], d[11]) +
				mul(v[768], d[12]) + mul(v[864], d[13]) + mul(v[896], d[14]) + mul(v[992], d[15])
		}
	}
}

// reset clears the V history, which is what a decoder does at setup and what
// a seek landing has to live with: the filter is fully warm 512 output
// samples after the landing.
func (s *synthesis) reset() {
	clear(s.v[:])
}
