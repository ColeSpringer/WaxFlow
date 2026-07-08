package vorbis

import (
	"strings"
	"testing"
)

// TestParseCodebookZeroDim guards the divide-by-zero fix: a codebook with zero
// dimensions is rejected at parse (it would otherwise reach partSize/dimensions
// == 0 during residue type-0 decode). The bytes encode the sync pattern, then
// dimensions == 0 and entries == 1.
func TestParseCodebookZeroDim(t *testing.T) {
	r := newBitReader([]byte{0x42, 0x43, 0x56, 0, 0, 1, 0, 0})
	_, err := parseCodebook(r)
	if err == nil || !strings.Contains(err.Error(), "zero dimensions") {
		t.Fatalf("parseCodebook(dim=0) err = %v, want a zero-dimensions rejection", err)
	}
}

// TestParseFloor0Rejected guards that the unverified LSP floor is refused
// rather than silently decoded wrong.
func TestParseFloor0Rejected(t *testing.T) {
	_, err := parseFloor0(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "floor 0") {
		t.Fatalf("parseFloor0 err = %v, want a floor 0 rejection", err)
	}
}
