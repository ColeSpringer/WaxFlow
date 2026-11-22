package waxflow

import (
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"
)

// TestDecodeVersionCoversEveryDecoder is the stale-cache ratchet ADR-0004
// needs. decodeVersion rides in the cached plan's version tuple, and a codec
// whose registry row states no version falls to a default keyed only on the
// codec ID. That ID is stable across decoder revisions, and would therefore
// serve audio decoded by the old one after a fix. The default is deliberate
// for UNREGISTERED codecs (they cannot decode, so no cached bytes exist), but
// a registered decoder reaching it is a bug that shows up as wrong audio a
// release later.
func TestDecodeVersionCoversEveryDecoder(t *testing.T) {
	seen := map[string]codec.ID{}
	for _, id := range format.Decoders() {
		v := decodeVersion(id)
		if strings.HasPrefix(v, "dec:") {
			t.Errorf("codec %q decodes but decodeVersion falls back to %q; add its Version constant", id, v)
			continue
		}
		if prev, dup := seen[v]; dup && prev != id {
			// Two codecs may deliberately share a package version (the AAC
			// pair does not: it has HEVersion), but an accidental collision
			// would let a revision to one invalidate the other's cache and
			// not its own.
			t.Errorf("codecs %q and %q both report version %q", prev, id, v)
		}
		seen[v] = id
	}
}
