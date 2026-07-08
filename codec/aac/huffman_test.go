package aac

import "testing"

// TestHuffmanKnownCodewords verifies the decode against known table entries.
func TestHuffmanKnownCodewords(t *testing.T) {
	// codes1[0]=0x7f8 length 11 -> index 0; 0xFF00 supplies 11111111000.
	r := newBitReader([]byte{0xFF, 0x00})
	if idx, ok := spectralBooks[0].decode(r); !ok || idx != 0 {
		t.Fatalf("book1 decode = %d ok=%v, want index 0", idx, ok)
	}
	// codes1[40]=0x000 length 1 -> index 40; a leading 0 bit.
	r = newBitReader([]byte{0x00})
	if idx, ok := spectralBooks[0].decode(r); !ok || idx != 40 {
		t.Fatalf("book1 short code = %d ok=%v, want index 40", idx, ok)
	}
	// Sanity: sizes match the artifact.
	if len(spectralCodes[10]) != 289 {
		t.Fatalf("codebook 11 size = %d, want 289", len(spectralCodes[10]))
	}
}
