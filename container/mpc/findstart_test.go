package mpc_test

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/container/mpc"
)

// TestFindStartConfirmsTheStream: every fixture's own start is found behind
// stray bytes, in both framings, and the magics in text or in bytes that go
// on to no stream are not: FindStart is trusted over a sniff of the bytes in
// front of it, so a start it confirms has to be one the demuxer opens.
func TestFindStartConfirmsTheStream(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc", "tagged.mpc", "gapless-sv7.mpc", "chapters.mpc", "seek-pns.mpc"} {
		raw := append(bytes.Repeat([]byte{0xAA}, 300), fixture(t, name)...)
		if at, ok := mpc.FindStart(raw); !ok || at != 300 {
			t.Errorf("%s: FindStart = %d, %v; want 300", name, at, ok)
		}
	}
	sv7 := fixture(t, "seek-sv7.mpc")
	cut := func(n int) []byte { return append(bytes.Clone(sv7[:n]), make([]byte, 64)...) }
	for name, window := range map[string][]byte{
		"text naming SV7":       []byte("Comment: transcoded from MP+7 (SV7) MPC v1.15 by Frank in 2004. " + string(make([]byte, 64))),
		"text naming SV8":       []byte("MPCKSV8 is the packet framing; MPCKAB3 and MPCKSH! are text too." + string(make([]byte, 64))),
		"an SV7 magic and NULs": append([]byte("MP+\x07"), make([]byte, 128)...),
		"an SV7 header alone":   cut(28),
		"zeros":                 make([]byte, 4096),
		"ones":                  bytes.Repeat([]byte{0xFF}, 4096),
	} {
		if at, ok := mpc.FindStart(window); ok {
			t.Errorf("%s: FindStart = %d", name, at)
		}
	}
}
