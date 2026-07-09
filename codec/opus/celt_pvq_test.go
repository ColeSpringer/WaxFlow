package opus

import (
	"fmt"
	"testing"
)

// TestCWRSBijection validates the PVQ codeword enumeration independently of any
// bitstream: for each (N, K) the V(N,K) indices must map to V distinct pulse
// vectors, every one with exactly K pulses (Σ|y|=K). That bijection is the
// defining property of the CWRS coding, so it exercises ncwrsURow, cwrsi, and
// the unext/uprev recurrences without needing an encoder.
func TestCWRSBijection(t *testing.T) {
	cases := []struct{ n, k int }{
		{2, 1}, {2, 5}, {3, 1}, {3, 2}, {3, 4}, {4, 3}, {5, 2}, {6, 4}, {8, 3}, {10, 6},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("N%d_K%d", tc.n, tc.k), func(t *testing.T) {
			u := make([]uint32, tc.k+2)
			v := ncwrsURow(tc.n, tc.k, u)
			seen := make(map[string]bool, v)
			y := make([]int, tc.n)
			for i := uint32(0); i < v; i++ {
				// ncwrsURow refills u destructively each call (cwrsi mutates it).
				ncwrsURow(tc.n, tc.k, u)
				cwrsi(tc.n, tc.k, i, y, u)
				sum := 0
				for _, val := range y {
					if val < 0 {
						sum -= val
					} else {
						sum += val
					}
				}
				if sum != tc.k {
					t.Fatalf("index %d: Σ|y|=%d, want K=%d (y=%v)", i, sum, tc.k, y)
				}
				key := fmt.Sprint(y)
				if seen[key] {
					t.Fatalf("index %d produced duplicate vector %v", i, y)
				}
				seen[key] = true
			}
			if uint32(len(seen)) != v {
				t.Fatalf("decoded %d distinct vectors, want V=%d", len(seen), v)
			}
		})
	}
}
