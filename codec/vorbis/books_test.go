package vorbis

import "testing"

// TestResidueBookTablesWellFormed guards the generated residue books (books_gen.go):
// each product-lattice table must hold exactly L^dim entries and form a valid
// prefix code, so a stale or hand-edited table fails loudly rather than producing
// an over-subscribed codebook at encode time.
func TestResidueBookTablesWellFormed(t *testing.T) {
	if len(resCoarseLengths) != resCoarseEntries {
		t.Fatalf("resCoarseLengths has %d entries, want %d", len(resCoarseLengths), resCoarseEntries)
	}
	if len(resFineLengths) != resFineEntries {
		t.Fatalf("resFineLengths has %d entries, want %d", len(resFineLengths), resFineEntries)
	}
	if _, ok := assignCodewords(resCoarseLengths); !ok {
		t.Error("resCoarseLengths over-subscribes the code space")
	}
	if _, ok := assignCodewords(resFineLengths); !ok {
		t.Error("resFineLengths over-subscribes the code space")
	}
	// Every entry must be codeable (nonzero length): the encoder can emit any
	// lattice point for an out-of-model outlier, so no entry may be unassigned.
	for i, l := range resCoarseLengths {
		if l == 0 {
			t.Fatalf("resCoarseLengths[%d] == 0 (uncodeable entry)", i)
		}
	}
	for i, l := range resFineLengths {
		if l == 0 {
			t.Fatalf("resFineLengths[%d] == 0 (uncodeable entry)", i)
		}
	}
}
