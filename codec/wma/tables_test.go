//go:build !wmatablesgen

package wma

// The tables are extracted mechanically (tablesgen_test.go), which rules out a
// typo but not a mis-parse: a dropped brace or a table read under the wrong
// upstream name would produce something that still compiles. These are the
// structural invariants the upstream data satisfies, so a damaged extraction
// fails here rather than in the decoder, and they need none of the pinned
// files present.
//
// The build tag is the recovery path: it keeps this file out of the build the
// generator runs under, so a generated file that is missing or damaged can be
// deleted and regenerated instead of wedging the package.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// kraft reports the Kraft sum of a length list. A complete prefix code sums to
// exactly 1; anything above 1 cannot be decoded uniquely.
func kraft(bits []uint8) float64 {
	var sum float64
	for _, n := range bits {
		sum += 1 / float64(uint64(1)<<n)
	}
	return sum
}

// prefixFree reports whether the explicit (code, length) listing is a prefix
// code: no codeword is a prefix of another, checked by walking every codeword
// into a shared binary tree.
func prefixFree(codes []uint32, bits []uint8) error {
	type link struct{ kid [2]int }
	tree := []link{{}}
	for i := range codes {
		n := int(bits[i])
		if n == 0 || n > 32 {
			return fmt.Errorf("entry %d: length %d out of range", i, n)
		}
		if n < 32 && codes[i] >= 1<<n {
			return fmt.Errorf("entry %d: code %#x does not fit %d bits", i, codes[i], n)
		}
		at := 0
		for b := n - 1; b >= 0; b-- {
			if at < 0 {
				return fmt.Errorf("entry %d: code %#x/%d extends a leaf", i, codes[i], n)
			}
			d := (codes[i] >> b) & 1
			next := tree[at].kid[d]
			if next == 0 {
				if b == 0 {
					tree[at].kid[d] = -1 - i // leaf
					at = 0
					break
				}
				tree = append(tree, link{})
				next = len(tree) - 1
				tree[at].kid[d] = next
			} else if b == 0 {
				return fmt.Errorf("entry %d: code %#x/%d collides", i, codes[i], n)
			}
			at = next
		}
	}
	return nil
}

func TestCoefBooksAreDecodable(t *testing.T) {
	for i := range coefCodes {
		if len(coefCodes[i]) != len(coefBits[i]) {
			t.Fatalf("book %d: %d codes, %d lengths", i, len(coefCodes[i]), len(coefBits[i]))
		}
		if err := prefixFree(coefCodes[i], coefBits[i]); err != nil {
			t.Errorf("book %d: %v", i, err)
		}
		if k := kraft(coefBits[i]); k != 1 {
			t.Errorf("book %d: Kraft sum %v, want 1", i, k)
		}
	}
}

// The run/level ladder starts at index 2, past the escape and the
// end-of-block, and every remaining index belongs to exactly one level. A
// ladder that ran short would leave indices unexpandable; one that ran long
// would claim indices the book does not have.
func TestCoefLevelLadderCoversTheBook(t *testing.T) {
	for i := range coefLevels {
		total := 0
		for _, n := range coefLevels[i] {
			if n == 0 {
				t.Errorf("book %d: zero-width level", i)
			}
			total += int(n)
		}
		if want := len(coefBits[i]) - 2; total != want {
			t.Errorf("book %d: ladder covers %d indices, book has %d past the escape and EOB",
				i, total, want)
		}
	}
}

// The books are three pairs of two distinct books, the second of each pair
// being the smaller mid/side book. Equal sizes would mean the extractor
// picked the same upstream array twice, which is the one mis-parse the
// structural checks above would not catch.
func TestCoefBooksArePaired(t *testing.T) {
	for p := range 3 {
		a, b := 2*p, 2*p+1
		if len(coefCodes[a]) <= len(coefCodes[b]) {
			t.Errorf("pair %d: books are %d and %d entries; the second should be the smaller",
				p, len(coefCodes[a]), len(coefCodes[b]))
		}
	}
}

func TestExponentScalefactorBook(t *testing.T) {
	if err := prefixFree(expScaleCodes[:], expScaleBits[:]); err != nil {
		t.Fatal(err)
	}
	if k := kraft(expScaleBits[:]); k != 1 {
		t.Errorf("Kraft sum %v, want 1", k)
	}
	// Index 60 is the zero delta and so must be the shortest codeword: the
	// book is biased by -60 and a run of equal exponents is the common case.
	if expScaleBits[60] != 1 {
		t.Errorf("zero delta is %d bits, want 1", expScaleBits[60])
	}
}

// THIRD-PARTY-NOTICES states that the scalefactor book here is the same one
// codec/aac already carries, extracted independently from the same upstream
// file in a different pass. That is a claim about two artifacts in this tree,
// so it is checked rather than asserted. Neither package can see the other's
// unexported tables, so the check reads aac's source: unusual, and cheaper
// than exporting a table for a test's benefit.
func TestScalefactorBookMatchesTheAACCopy(t *testing.T) {
	const path = "../aac/tables_hcb.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	want := map[string][]uint64{"scalefactorCodes": nil, "scalefactorBits": nil}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
			return true
		}
		vals, seek := want[spec.Names[0].Name]
		if !seek || vals != nil {
			return true
		}
		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		out := make([]uint64, 0, len(lit.Elts))
		for _, e := range lit.Elts {
			b, ok := e.(*ast.BasicLit)
			if !ok {
				t.Fatalf("%s: unexpected element %T", spec.Names[0].Name, e)
			}
			v, err := strconv.ParseUint(b.Value, 0, 64)
			if err != nil {
				t.Fatalf("%s: %v", spec.Names[0].Name, err)
			}
			out = append(out, v)
		}
		want[spec.Names[0].Name] = out
		return true
	})

	for name, mine := range map[string][]uint64{
		"scalefactorCodes": toU64(expScaleCodes[:]),
		"scalefactorBits":  toU64(expScaleBits[:]),
	} {
		theirs := want[name]
		if theirs == nil {
			t.Fatalf("%s: not found in %s", name, path)
		}
		if len(theirs) != len(mine) {
			t.Errorf("%s: aac has %d entries, wma has %d", name, len(theirs), len(mine))
			continue
		}
		for i := range mine {
			if mine[i] != theirs[i] {
				t.Errorf("%s[%d]: wma %d, aac %d", name, i, mine[i], theirs[i])
				break
			}
		}
	}
}

func toU64[T ~uint32 | ~uint8](in []T) []uint64 {
	out := make([]uint64, len(in))
	for i, v := range in {
		out[i] = uint64(v)
	}
	return out
}

// The noise-gain book carries no codewords. They come from walking the listed
// order and advancing an accumulator by 1<<(maxLen-len) at each entry, so the
// listed order is the only thing that assigns them: a book checked for symbol
// uniqueness and Kraft alone would pass reversed and decode every gain wrong.
// So the walk itself is what gets checked, and one pinned codeword keeps a
// reader from "correcting" it into a length-sorted canonical build, which
// would hand the four 3-bit entries 000..011 instead of 100..111.
func TestHgainBook(t *testing.T) {
	seen := make([]bool, len(hgainHuff))
	bits := make([]uint8, len(hgainHuff))
	codes := make([]uint32, len(hgainHuff))
	maxLen := uint8(0)
	for _, e := range hgainHuff {
		maxLen = max(maxLen, e[1])
	}
	var acc uint64
	for i, e := range hgainHuff {
		if int(e[0]) >= len(seen) {
			t.Fatalf("entry %d: symbol %d out of range", i, e[0])
		}
		if seen[e[0]] {
			t.Errorf("symbol %d listed twice", e[0])
		}
		seen[e[0]] = true
		bits[i] = e[1]
		codes[i] = uint32(acc >> (maxLen - e[1]))
		acc += 1 << (maxLen - e[1])
	}
	if want := uint64(1) << maxLen; acc != want {
		t.Errorf("the listed-order walk closes on %d, want %d", acc, want)
	}
	if err := prefixFree(codes, bits); err != nil {
		t.Errorf("codes from the listed-order walk: %v", err)
	}
	if last := len(hgainHuff) - 1; codes[last] != 0b111 || bits[last] != 3 {
		t.Errorf("last entry codes as %b/%d bits, want 111/3", codes[last], bits[last])
	}
}

func TestCriticalFreqsAscend(t *testing.T) {
	for i := 1; i < len(criticalFreqs); i++ {
		if criticalFreqs[i] <= criticalFreqs[i-1] {
			t.Errorf("band %d: %d does not exceed %d", i, criticalFreqs[i], criticalFreqs[i-1])
		}
	}
}

var expBandTables = map[int]*[3][]uint8{
	22050: &expBands22050,
	32000: &expBands32000,
	44100: &expBands44100,
}

// Each tabulated row partitions one block exactly, and row r is the block of
// 128<<r coefficients. Every band is a multiple of four, which is what lets a
// decoder fill exponents four at a time.
func TestExponentBandRowsPartitionTheirBlock(t *testing.T) {
	for rate, table := range expBandTables {
		for r, row := range table {
			total := 0
			for i, w := range row {
				if w == 0 || w%4 != 0 {
					t.Errorf("%d row %d band %d: width %d is not a positive multiple of 4", rate, r, i, w)
				}
				total += int(w)
			}
			if want := 128 << r; total != want {
				t.Errorf("%d row %d: bands sum to %d, block is %d", rate, r, total, want)
			}
		}
	}
}

// computedBands is the layout the v2 formula produces for a block at a rate,
// which is what the tabulated rows coarsen. It is here to check the tables,
// not to decode: the decoder carries its own.
func computedBands(blockLen, rate int) []int {
	var out []int
	lpos := 0
	for _, edge := range criticalFreqs {
		pos := min(((blockLen*2*int(edge)+2*rate)/(4*rate))<<2, blockLen)
		if pos > lpos {
			out = append(out, pos)
		}
		if pos >= blockLen {
			break
		}
		lpos = pos
	}
	return out
}

func bandEdges(row []uint8) []int {
	out, pos := make([]int, 0, len(row)), 0
	for _, w := range row {
		pos += int(w)
		out = append(out, pos)
	}
	return out
}

// worstEdgeGap is how far the row's furthest band edge sits from the nearest
// edge the formula would place at that rate.
func worstEdgeGap(row []uint8, blockLen, rate int) int {
	computed, worst := computedBands(blockLen, rate), 0
	for _, e := range bandEdges(row) {
		best := blockLen
		for _, c := range computed {
			best = min(best, abs(e-c))
		}
		worst = max(worst, best)
	}
	return worst
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// The partition check above cannot tell the three tables apart: every row of
// every table sums to its block by construction, so a table extracted under
// the wrong upstream name would ship silently. What does tell them apart is
// that each is a coarsening of the critical-band layout *at its own rate*:
// every tabulated edge sits on, or within one quarter-step of, an edge the
// formula places there, and lands further out at the other two rates. That
// margin is the grid the v2 formula rounds to, not a tuned constant.
func TestExponentBandTablesMatchTheirOwnRate(t *testing.T) {
	const step = 4
	for rate, table := range expBandTables {
		for r, row := range table {
			blockLen := 128 << r
			if gap := worstEdgeGap(row, blockLen, rate); gap > step {
				t.Errorf("%d row %d: an edge sits %d coefficients off the layout for %d Hz, want <= %d",
					rate, r, gap, rate, step)
			}
			for other := range expBandTables {
				if other == rate {
					continue
				}
				// The 128-coefficient block is coarse enough that all three
				// grids coincide there; the distinct band counts below carry
				// that row instead.
				if blockLen == 128 {
					continue
				}
				if gap := worstEdgeGap(row, blockLen, other); gap <= step {
					t.Errorf("%d row %d also fits the layout for %d Hz (gap %d); the table is not rate-specific",
						rate, r, other, gap)
				}
			}
		}
	}
	counts := map[int]int{}
	for rate, table := range expBandTables {
		if prev, dup := counts[len(table[0])]; dup {
			t.Errorf("%d and %d both give %d bands for the 128-coefficient block", rate, prev, len(table[0]))
		}
		counts[len(table[0])] = rate
	}
}

// Rows read with three bits use their first eight entries and leave the rest
// zero; rows read with four bits use all sixteen. Getting this wrong would
// index a zero as a coefficient and flatten the exponent curve.
func TestLSPCodebookRowWidths(t *testing.T) {
	narrow := map[int]bool{0: true, 8: true, 9: true}
	for r, row := range lspCodebook {
		used := 16
		if narrow[r] {
			used = 8
		}
		for i, v := range row {
			if i < used && v == 0 {
				t.Errorf("row %d entry %d: zero inside the used range", r, i)
			}
			if i >= used && v != 0 {
				t.Errorf("row %d entry %d: %v past the used range", r, i, v)
			}
		}
	}
}

// Every row falls with its index, at exactly two places excepted. Row 7
// restarts a second descending run at entry 8, and row 5 entry 10 is a lone
// spike that looks like a digit slipped when the codebook was recovered from
// the binary decoder (-1.40 where -0.14 would sit in order). Both are what
// deployed files are coded against, so both are pinned here: a re-extraction
// that smooths either away fails, and so does a reader who "corrects" one.
func TestLSPCodebookFallsExceptWhereItDoesNot(t *testing.T) {
	// The spike is a lone outlier, so the entry after it belongs in order with
	// the entry *before* it; the restart begins a fresh run, so the entries
	// after it belong in order with the restart value itself. Comparing the
	// after-spike entry against the spike would compare against nothing, which
	// is how a corrupted neighbour slips through: with the spike skipped and
	// the entry after it skipped too, nothing bounds that entry from above.
	const spike, restart = 5, 7
	breaks := map[[2]int]float32{{spike, 10}: -1.40037388, {restart, 8}: -1.10158808}
	for r, row := range lspCodebook {
		used := 16
		if r == 0 || r == 8 || r == 9 {
			used = 8
		}
		for i := 1; i < used; i++ {
			if _, isBreak := breaks[[2]int{r, i}]; isBreak {
				continue // the break's own value is pinned below
			}
			above := row[i-1]
			if _, afterSpike := breaks[[2]int{spike, i - 1}]; afterSpike && r == spike {
				above = row[i-2]
			}
			if row[i] >= above {
				t.Errorf("row %d entry %d: %v does not fall below %v", r, i, row[i], above)
			}
		}
	}
	for at, want := range breaks {
		if got := lspCodebook[at[0]][at[1]]; got != want {
			t.Errorf("row %d entry %d is %v, want the out-of-order %v", at[0], at[1], got, want)
		}
	}
}
