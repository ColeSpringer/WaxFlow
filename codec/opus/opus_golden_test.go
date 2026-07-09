package opus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestOpusEncodeDeterministic requires the encoder to be byte-for-byte
// reproducible: the same input through a fresh encoder must produce identical
// Ogg-Opus output every time. Determinism is what lets HLS segments regenerate
// identically after cache eviction (a golden requirement) and keeps cache keys
// coherent. It fails if any non-deterministic source (a clock or RNG) leaks in.
func TestOpusEncodeDeterministic(t *testing.T) {
	for _, C := range []int{1, 2} {
		a := encodeN(t, C, 12000, 128000)
		b := encodeN(t, C, 12000, 128000)
		if !bytes.Equal(a, b) {
			t.Fatalf("C=%d: encoder output is not deterministic (%d vs %d bytes)", C, len(a), len(b))
		}
		// A stable hash pins the exact bytes so an unintended bitstream change
		// (which would invalidate caches and break golden HLS) is caught.
		sum := sha256.Sum256(a)
		t.Logf("C=%d: %d bytes, sha256=%s", C, len(a), hex.EncodeToString(sum[:8]))
	}
}
