package vorbis

import "testing"

// TestResidueBookTablesWellFormed guards the generated residue books (books_gen.go):
// each product-lattice table must hold exactly L^dim entries and form a valid
// prefix code, so a stale or hand-edited table fails loudly rather than producing
// an over-subscribed codebook at encode time.
func TestResidueBookTablesWellFormed(t *testing.T) {
	tables := []struct {
		name    string
		lengths []uint8
		entries int
	}{
		{"resNoiseLengths", resNoiseLengths, resNoiseEntries},
		{"resCoarseLengths", resCoarseLengths, resCoarseEntries},
		{"resR1Lengths", resR1Lengths, resR1Entries},
		{"resR2Lengths", resR2Lengths, resR2Entries},
		{"resR3Lengths", resR3Lengths, resR3Entries},
	}
	for _, tb := range tables {
		if len(tb.lengths) != tb.entries {
			t.Fatalf("%s has %d entries, want %d", tb.name, len(tb.lengths), tb.entries)
		}
		if _, ok := assignCodewords(tb.lengths); !ok {
			t.Errorf("%s over-subscribes the code space", tb.name)
		}
		// Every entry must be codeable (nonzero length): the encoder can emit any
		// lattice point for an out-of-model outlier, so no entry may be unassigned.
		for i, l := range tb.lengths {
			if l == 0 {
				t.Fatalf("%s[%d] == 0 (uncodeable entry)", tb.name, i)
			}
		}
	}
}
