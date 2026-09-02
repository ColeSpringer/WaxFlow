package musepack_test

import (
	"math"
	"math/bits"
	"sort"
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
)

// TestBooksAreComplete pins every Huffman book structurally: the codewords are
// prefix-free (the builder panics otherwise) and Kraft-complete, so a
// transcription error that dropped, duplicated or mislengthened a row fails
// here rather than as a wrong sample somewhere in a fixture.
func TestBooksAreComplete(t *testing.T) {
	for name, words := range musepack.Books() {
		t.Run(name, func(t *testing.T) {
			if len(words) == 0 {
				t.Fatal("empty book")
			}
			var kraft float64
			seen := map[uint64]bool{}
			for _, w := range words {
				if w.Length < 1 || w.Length > 16 {
					t.Errorf("codeword %#x has length %d", w.Code, w.Length)
				}
				if w.Code>>uint(w.Length) != 0 {
					t.Errorf("codeword %#x does not fit %d bits", w.Code, w.Length)
				}
				key := uint64(w.Length)<<32 | uint64(w.Code)
				if seen[key] {
					t.Errorf("codeword %#x/%d listed twice", w.Code, w.Length)
				}
				seen[key] = true
				kraft += math.Ldexp(1, -w.Length)
			}
			if kraft != 1 {
				t.Errorf("Kraft sum %v, want exactly 1", kraft)
			}
		})
	}
}

// TestSV7RowsAreSingleCodewords pins what the SV7 tables mean: each row is one
// codeword carrying its value, unlike the canonical SV8 rows that span a run.
func TestSV7RowsAreSingleCodewords(t *testing.T) {
	for name, counts := range musepack.SV7RowCounts() {
		for i, n := range counts {
			if n != 1 {
				t.Errorf("%s row %d expands to %d codewords, want 1", name, i, n)
			}
		}
	}
}

// TestBookSymbolsCoverTheirRange pins the symbol tables: each SV8 sample book
// must produce every value its quantiser can take, exactly once, and the
// tuple books every tuple index.
func TestBookSymbolsCoverTheirRange(t *testing.T) {
	want := map[string][2]int{
		"sv8/SCFI_1": {0, 3}, "sv8/SCFI_2": {0, 15},
		"sv8/DSCF_1": {0, 63}, "sv8/DSCF_2": {0, 64},
		"sv8/Bands": {0, 32}, "sv8/Res_1": {0, 16}, "sv8/Res_2": {0, 16},
		"sv8/Q1": {0, 18}, "sv8/Q2_1": {0, 124}, "sv8/Q2_2": {0, 124},
		"sv8/Q5_1": {-7, 7}, "sv8/Q5_2": {-7, 7}, "sv8/Q6_1": {-15, 15}, "sv8/Q6_2": {-15, 15},
		"sv8/Q7_1": {-31, 31}, "sv8/Q7_2": {-31, 31}, "sv8/Q8_1": {-63, 63}, "sv8/Q8_2": {-63, 63},
		"sv8/Q9up":     {-128, 127},
		"sv7/HuffSCFI": {0, 3}, "sv7/HuffQ1[0]": {0, 26}, "sv7/HuffQ1[1]": {0, 26},
		"sv7/HuffQ2[0]": {0, 24}, "sv7/HuffQ2[1]": {0, 24},
		"sv7/HuffQ3[0]": {-3, 3}, "sv7/HuffQ3[1]": {-3, 3}, "sv7/HuffQ4[0]": {-4, 4}, "sv7/HuffQ4[1]": {-4, 4},
		"sv7/HuffQ5[0]": {-7, 7}, "sv7/HuffQ5[1]": {-7, 7}, "sv7/HuffQ6[0]": {-15, 15}, "sv7/HuffQ6[1]": {-15, 15},
		"sv7/HuffQ7[0]": {-31, 31}, "sv7/HuffQ7[1]": {-31, 31},
	}
	books := musepack.Books()
	for name, rng := range want {
		words, ok := books[name]
		if !ok {
			t.Errorf("no book %s", name)
			continue
		}
		syms := make([]int, 0, len(words))
		for _, w := range words {
			syms = append(syms, w.Symbol)
		}
		sort.Ints(syms)
		if len(syms) != rng[1]-rng[0]+1 {
			t.Errorf("%s has %d symbols for the range %d..%d", name, len(syms), rng[0], rng[1])
			continue
		}
		for i, s := range syms {
			if s != rng[0]+i {
				t.Errorf("%s symbols are not %d..%d each once: %v", name, rng[0], rng[1], syms)
				break
			}
		}
	}
	// The nibble-pair books code two samples per symbol, so their symbols are
	// checked as pairs: every pair in the quantiser's range appears once.
	for _, c := range []struct {
		name  string
		bound int
	}{{"sv8/Q3", 3}, {"sv8/Q4", 4}} {
		seen := map[[2]int]int{}
		for _, w := range books[c.name] {
			lo := int(int8(uint8(w.Symbol)<<4) >> 4)
			hi := int(int8(w.Symbol) >> 4)
			seen[[2]int{lo, hi}]++
		}
		n := 2*c.bound + 1
		if len(seen) != n*n {
			t.Errorf("%s codes %d sample pairs, want %d", c.name, len(seen), n*n)
		}
		for lo := -c.bound; lo <= c.bound; lo++ {
			for hi := -c.bound; hi <= c.bound; hi++ {
				if seen[[2]int{lo, hi}] != 1 {
					t.Errorf("%s codes the pair (%d, %d) %d times", c.name, lo, hi, seen[[2]int{lo, hi}])
				}
			}
		}
	}
	// HuffHdr and HuffDSCF carry an escape beside their deltas.
	for name, want := range map[string][]int{
		"sv7/HuffHdr":  {-5, -4, -3, -2, -1, 0, 1, 2, 3, 4},
		"sv7/HuffDSCF": {-7, -6, -5, -4, -3, -2, -1, 0, 1, 2, 3, 4, 5, 6, 7, 8},
	} {
		var syms []int
		for _, w := range books[name] {
			syms = append(syms, w.Symbol)
		}
		sort.Ints(syms)
		if len(syms) != len(want) {
			t.Errorf("%s symbols %v, want %v", name, syms, want)
			continue
		}
		for i := range want {
			if syms[i] != want[i] {
				t.Errorf("%s symbols %v, want %v", name, syms, want)
				break
			}
		}
	}
}

// TestDerivedTablesMatchTheReference pins the tables computed at init against
// the reference's spelled-out versions: the 3- and 5-step tuple decodes, the
// 5-step switching weights, and the coefficient identity Cc = 65536/steps.
func TestDerivedTablesMatchTheReference(t *testing.T) {
	// mpc_decoder.c's idx30/idx31/idx32 and idx50/idx51.
	idx30 := []int8{-1, 0, 1, -1, 0, 1, -1, 0, 1, -1, 0, 1, -1, 0, 1, -1, 0, 1, -1, 0, 1, -1, 0, 1, -1, 0, 1}
	idx31 := []int8{-1, -1, -1, 0, 0, 0, 1, 1, 1, -1, -1, -1, 0, 0, 0, 1, 1, 1, -1, -1, -1, 0, 0, 0, 1, 1, 1}
	idx32 := []int8{-1, -1, -1, -1, -1, -1, -1, -1, -1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	for i := range idx30 {
		if musepack.IdxTables[0][i] != idx30[i] || musepack.IdxTables[1][i] != idx31[i] || musepack.IdxTables[2][i] != idx32[i] {
			t.Errorf("3-step tuple %d decodes to (%d,%d,%d), reference (%d,%d,%d)", i,
				musepack.IdxTables[0][i], musepack.IdxTables[1][i], musepack.IdxTables[2][i], idx30[i], idx31[i], idx32[i])
		}
	}
	// The SV8 5-step tuples: sample j of tuple i is (i / 5^j) % 5 - 2, which is
	// also the SV7 idx50/idx51 read for the first two.
	for i := range 125 {
		want := [3]int8{int8(i%5) - 2, int8(i/5%5) - 2, int8(i/25) - 2}
		got := [3]int8{musepack.Idx8Tables[0][i], musepack.Idx8Tables[1][i], musepack.Idx8Tables[2][i]}
		if got != want {
			t.Errorf("5-step tuple %d decodes to %v, want %v", i, got, want)
		}
		if i < 25 && (musepack.IdxTables[3][i] != want[0] || musepack.IdxTables[4][i] != want[1]) {
			t.Errorf("SV7 5-step tuple %d decodes to (%d,%d), want (%d,%d)", i, musepack.IdxTables[3][i], musepack.IdxTables[4][i], want[0], want[1])
		}
	}
	// mpc_decoder.c's HuffQ2_var, the switching weight per tuple.
	huffQ2Var := []uint32{6, 5, 4, 5, 6, 5, 4, 3, 4, 5, 4, 3, 2, 3, 4, 5, 4, 3, 4, 5, 6, 5, 4, 5, 6, 5, 4, 3, 4, 5, 4, 3, 2, 3, 4, 3, 2, 1, 2, 3, 4, 3, 2, 3, 4, 5, 4, 3, 4, 5, 4, 3, 2, 3, 4, 3, 2, 1, 2, 3, 2, 1, 0, 1, 2, 3, 2, 1, 2, 3, 4, 3, 2, 3, 4, 5, 4, 3, 4, 5, 4, 3, 2, 3, 4, 3, 2, 1, 2, 3, 4, 3, 2, 3, 4, 5, 4, 3, 4, 5, 6, 5, 4, 5, 6, 5, 4, 3, 4, 5, 4, 3, 2, 3, 4, 5, 4, 3, 4, 5, 6, 5, 4, 5, 6}
	for i, want := range huffQ2Var {
		if musepack.HuffQ2Var[i] != want {
			t.Errorf("HuffQ2_var[%d] = %d, reference %d", i, musepack.HuffQ2Var[i], want)
		}
	}
	// Cc[res] = 65536 / (2*Dc[res] + 1) for every real resolution, and the
	// noise entry is 32768/2/255*sqrt(3).
	for res := 1; res <= 17; res++ {
		steps := 2*float64(musepack.Dc[res+1]) + 1
		if want := float32(65536 / steps); math.Abs(float64(musepack.Cc[res+1]-want)) > 1e-3 {
			t.Errorf("Cc[%d] = %v, want 65536/%v = %v", res, musepack.Cc[res+1], steps, want)
		}
	}
	if want := float32(32768.0 / 2 / 255 * math.Sqrt(3)); math.Abs(float64(musepack.Cc[0]-want)) > 1e-3 {
		t.Errorf("noise Cc = %v, want %v", musepack.Cc[0], want)
	}
}

// TestSCFTableIsGeometric pins the scalefactor table: index 1 is 1/32768,
// the entries above it shrink by the ratio per step up to 128, and the
// entries below it (wrapping through 255) grow by the same ratio down to 129,
// where the two series meet and the reference's own setup order lets the
// growing one win.
func TestSCFTableIsGeometric(t *testing.T) {
	scf := musepack.SCFTable
	if scf[1] != 1.0/32768 {
		t.Fatalf("SCF[1] = %v, want 1/32768", scf[1])
	}
	for i := 2; i <= 128; i++ {
		if r := float64(scf[i]) / float64(scf[i-1]); math.Abs(r-musepack.SCFRatio) > 1e-6 {
			t.Errorf("SCF[%d]/SCF[%d] = %v, want %v", i, i-1, r, musepack.SCFRatio)
		}
	}
	for n := 1; n <= 128; n++ {
		i, prev := uint8(1-n), uint8(2-n)
		if r := float64(scf[i]) / float64(scf[prev]); math.Abs(r-1/musepack.SCFRatio) > 1e-6 {
			t.Errorf("SCF[%d]/SCF[%d] = %v, want %v", i, prev, r, 1/musepack.SCFRatio)
		}
	}
}

// TestWindowSymmetry pins the synthesis window's transcription: the MPEG-1 D
// window is antisymmetric about its middle row and mirrored between rows i
// and 32-i with alternating signs, which is what Di_opt lays out.
func TestWindowSymmetry(t *testing.T) {
	d := musepack.DiOptInt
	for i := 1; i <= 15; i++ {
		for j := range 16 {
			if d[32-i][j] != -d[i][15-j] {
				t.Errorf("Di_opt[%d][%d] = %d, want -Di_opt[%d][%d] = %d", 32-i, j, d[32-i][j], i, 15-j, -d[i][15-j])
			}
		}
	}
	for j := range 16 {
		if d[16][j] != -d[16][15-j] {
			t.Errorf("Di_opt[16][%d] = %d, want -Di_opt[16][%d]", j, d[16][j], 15-j)
		}
	}
	for j := 1; j <= 7; j++ {
		want := d[0][16-j]
		if j%2 == 1 {
			want = -want
		}
		if d[0][j] != want {
			t.Errorf("Di_opt[0][%d] = %d, want %d", j, d[0][j], want)
		}
	}
	// The window scale: the reference stores integers over 65536, and the
	// centre tap is the MPEG-1 D[256] of 1.144989.
	if math.Abs(float64(musepack.DiOpt[0][8])-1.144989014) > 1e-6 {
		t.Errorf("centre tap %v, want 1.144989014", musepack.DiOpt[0][8])
	}
}

// TestBinomialTableIsPascal pins the enumeration tables' source: the binomial
// coefficients, computed rather than transcribed, agree with the reference's
// Cnk rows spot-checked here.
func TestBinomialTableIsPascal(t *testing.T) {
	c := musepack.Binomial
	for n := 0; n <= 32; n++ {
		for k := 0; k <= 16 && k <= n; k++ {
			want := 1.0
			for i := 1; i <= k; i++ {
				want = want * float64(n-k+i) / float64(i)
			}
			if float64(c[n][k]) != math.Round(want) {
				t.Errorf("C(%d,%d) = %d, want %v", n, k, c[n][k], want)
			}
		}
	}
	// mpc_bits_reader.c: Cnk[8][31] (k=9, n=31) and Cnk[15][31] (k=16, n=31).
	if c[31][9] != 20160075 || c[31][16] != 300540195 {
		t.Errorf("C(31,9) = %d, C(31,16) = %d; the reference's Cnk rows say 20160075 and 300540195", c[31][9], c[31][16])
	}
}

// TestTruncatedBinaryMatchesTheReferenceTables pins the derived code lengths
// against the reference's log2/log2_lost and Cnk_len/Cnk_lost tables, rows
// transcribed from mpc_bits_reader.c: V values take ceil(log2 V) bits with
// 2^L - V of them one bit shorter.
func TestTruncatedBinaryMatchesTheReferenceTables(t *testing.T) {
	log2 := []int{1, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 6}
	log2Lost := []int{0, 1, 0, 3, 2, 1, 0, 7, 6, 5, 4, 3, 2, 1, 0, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 31}
	for max := 1; max <= 32; max++ {
		v := uint32(max + 1)
		l := bits.Len32(v - 1)
		if l != log2[max-1] {
			t.Errorf("log2[%d]: derived %d, reference %d", max-1, l, log2[max-1])
		}
		if lost := int(uint32(1)<<l - v); lost != log2Lost[max-1] {
			t.Errorf("log2_lost[%d]: derived %d, reference %d", max-1, lost, log2Lost[max-1])
		}
	}
	// Cnk_len[k-1][n-1] and Cnk_lost[k-1][n-1] for k = 1, 2, 9 and 16.
	cnkLen := map[int][]int{
		1:  {0, 1, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5},
		2:  {0, 0, 2, 3, 4, 4, 5, 5, 6, 6, 6, 7, 7, 7, 7, 7, 8, 8, 8, 8, 8, 8, 8, 9, 9, 9, 9, 9, 9, 9, 9, 9},
		9:  {0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 6, 8, 10, 11, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 21, 22, 23, 23, 24, 24, 25, 25},
		16: {0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 8, 10, 13, 15, 17, 18, 20, 21, 23, 24, 25, 27, 28, 29, 30},
	}
	cnkLost := map[int][]int{
		1:  {0, 0, 1, 0, 3, 2, 1, 0, 7, 6, 5, 4, 3, 2, 1, 0, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		2:  {0, 0, 1, 2, 6, 1, 11, 4, 28, 19, 9, 62, 50, 37, 23, 8, 120, 103, 85, 66, 46, 25, 3, 236, 212, 187, 161, 134, 106, 77, 47, 16},
		9:  {0, 0, 0, 0, 0, 0, 0, 0, 0, 6, 9, 36, 309, 46, 3187, 4944, 8458, 16916, 38694, 94184, 230358, 26868, 231386, 789648, 54177, 1069754, 3701783, 1481708, 6762211, 2470066, 13394357, 5505632},
		16: {0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 15, 103, 55, 3347, 12419, 56459, 16987, 313105, 54177, 3076873, 3739321, 3132677, 66353813, 123012781, 236330717},
	}
	for k, lens := range cnkLen {
		for n := 1; n <= 32; n++ {
			c := musepack.Binomial[n][k]
			if n < k {
				continue // the reference leaves these zero; nothing reads them
			}
			l := 0
			if c > 1 {
				l = bits.Len32(c - 1)
			}
			if l != lens[n-1] {
				t.Errorf("Cnk_len[%d][%d]: derived %d, reference %d", k-1, n-1, l, lens[n-1])
			}
			if n-1 < len(cnkLost[k]) {
				if lost := int(uint32(1)<<l - c); lost != cnkLost[k][n-1] {
					t.Errorf("Cnk_lost[%d][%d]: derived %d, reference %d", k-1, n-1, lost, cnkLost[k][n-1])
				}
			}
		}
	}
}

// TestEnumDecodeIsABijection round-trips every k-of-n set through an encoder
// written here from the same combinatorial number system, for the shapes the
// format uses: 18 positions for the res-1 quantiser and up to 32 for the
// mid/side flags.
func TestEnumDecodeIsABijection(t *testing.T) {
	encode := func(set uint32, k, n int) []byte {
		// The index of the set in the combinatorial number system, then
		// truncated binary over C(n, k) values, MSB first.
		var code uint32
		kk := k
		for pos := n - 1; pos >= 0 && kk > 0; pos-- {
			if set&(1<<pos) != 0 {
				code += musepack.Binomial[pos][kk]
				kk--
			}
		}
		total := musepack.Binomial[n][k]
		l := bits.Len32(total - 1)
		lost := uint32(1)<<l - total
		var w bitWriter
		if code < lost {
			w.write(code, l-1)
		} else {
			w.write(code+lost, l)
		}
		return w.bytes()
	}
	for _, c := range []struct{ k, n int }{{1, 18}, {4, 18}, {9, 18}, {1, 2}, {3, 7}, {5, 11}, {16, 32}, {7, 32}} {
		count := 0
		for set := uint32(0); set < 1<<c.n; set++ {
			if bits.OnesCount32(set) != c.k {
				continue
			}
			count++
			if count > 20000 {
				break
			}
			b := encode(set, c.k, c.n)
			got, _ := musepack.ReadEnum(b, c.k, c.n)
			if got != set {
				t.Fatalf("k=%d n=%d: set %#x decoded as %#x", c.k, c.n, set, got)
			}
		}
	}
}

// TestLogCodeIsTruncatedBinary pins the band-count code: max+1 values, the
// first 2^L-(max+1) of them one bit short.
func TestLogCodeIsTruncatedBinary(t *testing.T) {
	for max := 0; max <= 32; max++ {
		v := uint32(max + 1)
		l := bits.Len32(v - 1)
		lost := uint32(1)<<l - v
		for value := 0; value <= max; value++ {
			var w bitWriter
			if max > 0 {
				if uint32(value) < lost {
					w.write(uint32(value), l-1)
				} else {
					w.write(uint32(value)+lost, l)
				}
			}
			w.write(0x55, 8) // trailing bits that must not be consumed
			got, used := musepack.ReadLog(w.bytes(), max)
			wantBits := 0
			if max > 0 {
				wantBits = l
				if uint32(value) < lost {
					wantBits = l - 1
				}
			}
			if got != value || used != wantBits {
				t.Errorf("max %d value %d: decoded %d in %d bits, want %d in %d", max, value, got, used, value, wantBits)
			}
		}
	}
}

// TestGolombCode pins the seek table's code: a unary quotient, a stop bit,
// then k remainder bits.
func TestGolombCode(t *testing.T) {
	for _, c := range []struct {
		v int32
		k int
	}{{0, 12}, {1, 12}, {4095, 12}, {4096, 12}, {12345, 12}, {5, 0}, {77, 3}} {
		var w bitWriter
		q := c.v >> c.k
		for range q {
			w.write(0, 1)
		}
		w.write(1, 1)
		w.write(uint32(c.v)&(1<<c.k-1), c.k)
		w.write(0xA5, 8)
		got, ok, used := musepack.ReadGolomb(w.bytes(), c.k)
		if !ok || got != c.v || used != int(q)+1+c.k {
			t.Errorf("golomb(%d, k=%d): got %d ok=%v in %d bits", c.v, c.k, got, ok, used)
		}
	}
	if _, ok, _ := musepack.ReadGolomb([]byte{0, 0, 0, 0, 0, 0}, 12); ok {
		t.Error("a run of zeros with no stop bit decoded as a Golomb code")
	}
}

// TestPRNGMatchesTheReferenceSetup pins the noise generator: the reference's
// setup words and its first draws, computed here from its definition with the
// parity table written out.
func TestPRNGMatchesTheReferenceSetup(t *testing.T) {
	parity := func(x uint32) uint32 { return uint32(bits.OnesCount32(x) & 1) }
	r1, r2 := uint32(1), uint32(1)
	for i := 0; i < 1000; i++ {
		want1 := r1>>1 | parity(r1&0xF5)<<31
		want2 := r2<<1 | parity(r2>>25&0x63)
		v, n1, n2 := musepack.PRNGNext(r1, r2)
		if n1 != want1 || n2 != want2 || v != want1^want2 {
			t.Fatalf("draw %d from (%#x,%#x): got %#x (%#x,%#x), want %#x (%#x,%#x)", i, r1, r2, v, n1, n2, want1^want2, want1, want2)
		}
		r1, r2 = n1, n2
	}
	// The first draw from the setup state, by hand: r1 = 1 has parity 1 at
	// bit 0 of 0xF5, so r1 becomes 0x80000000; r2 = 1 shifts to 2 with no
	// parity bit; the draw is their XOR.
	if v, n1, n2 := musepack.PRNGNext(1, 1); v != 0x80000002 || n1 != 0x80000000 || n2 != 2 {
		t.Errorf("first draw = %#x (%#x, %#x), want 0x80000002 (0x80000000, 2)", v, n1, n2)
	}
}

// bitWriter packs MSB-first fields for the code tests.
type bitWriter struct {
	buf  []byte
	bits int
}

func (w *bitWriter) write(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.bits&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.buf[w.bits>>3] |= 1 << (7 - uint(w.bits&7))
		}
		w.bits++
	}
}

func (w *bitWriter) bytes() []byte {
	return append(w.buf, 0, 0, 0, 0)
}
