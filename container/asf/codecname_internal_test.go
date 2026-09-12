package asf

import (
	"testing"

	"github.com/colespringer/waxflow/container/internal/codecname"
)

// TestASFAndSharedNamesAgree pins the one deliberate divergence in the codec
// vocabulary. This package names the tags it decodes in full ("Windows Media
// Audio 2"), because that is what an ASF refusal has always said and three
// tests pin those words; container/internal/codecname carries the short names
// the sibling tag reader uses ("WMA v2"), and the fall-through in selectStream
// reaches it only for tags this table does not name. The two therefore never
// answer for the same tag at runtime, but they must stay descriptions of the
// same codec, and nothing else would notice if one drifted.
func TestASFAndSharedNamesAgree(t *testing.T) {
	for tag, short := range map[uint16]string{
		0x000A: "WMA Voice",
		0x0160: "WMA v1",
		0x0161: "WMA v2",
		0x0162: "WMA Pro",
		0x0163: "WMA Lossless",
	} {
		_, long := asfCodecID(tag)
		if long == "" {
			t.Errorf("%#04x: this package names nothing; codecname says %q", tag, short)
			continue
		}
		if got := codecname.WaveFormat(tag); got != short {
			t.Errorf("%#04x: codecname says %q, want %q (this package says %q)", tag, got, short, long)
		}
	}
	// The two tags this package names and codecname does not: both are forms
	// no reference decoder reads, so the shared table, which follows the tag
	// reader's vocabulary, has no name to offer for them.
	for _, tag := range []uint16{0x0164, 0x000B} {
		if _, long := asfCodecID(tag); long == "" {
			t.Errorf("%#04x: this package stopped naming it", tag)
		}
		if got := codecname.WaveFormat(tag); got != "" {
			t.Errorf("%#04x: codecname now says %q; fold it into this package's table too", tag, got)
		}
	}
}
