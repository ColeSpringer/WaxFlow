//go:build !wmaprotablesgen

package wmapro

import (
	"math"
	"testing"
)

// TestBooksAreCanonicalAndComplete pins the property the trie builder relies
// on: every book closes on exactly one under the in-table-order assignment,
// so no bit pattern falls outside it and every codeword fits the reader's
// window. A book that did not close would leave a symbol undecodable, which
// on damaged input reads as a refusal and on a regenerated table as a build
// that silently lost a codeword.
func TestBooksAreCanonicalAndComplete(t *testing.T) {
	for _, b := range []struct {
		name string
		lens []uint8
		syms []uint16
	}{
		{"scaleDelta", scaleDeltaLens[:], scaleDeltaSyms[:]},
		{"scaleRunLevel", scaleRunLevelLens[:], scaleRunLevelSyms[:]},
		{"coef0", coefLens[0], coefSyms[0]},
		{"coef1", coefLens[1], coefSyms[1]},
		{"vec4", vec4Lens[:], vec4Syms[:]},
		{"vec2", vec2Lens[:], vec2Syms[:]},
		{"vec1", vec1Lens[:], vec1Syms[:]},
	} {
		kraft := 0.0
		longest := 0
		for _, l := range b.lens {
			if l == 0 {
				continue
			}
			kraft += math.Ldexp(1, -int(l))
			longest = max(longest, int(l))
		}
		if kraft != 1 {
			t.Errorf("%s: Kraft sum %v, want exactly 1", b.name, kraft)
		}
		if longest > maxBits {
			t.Errorf("%s: a %d-bit codeword is wider than the reader's %d-bit window", b.name, longest, maxBits)
		}
		// newBook panics on a collision or a prefix, so building is the check.
		if v := newBook(b.lens, b.syms); v.maxLen != longest {
			t.Errorf("%s: trie depth %d, want %d", b.name, v.maxLen, longest)
		}
	}
}
