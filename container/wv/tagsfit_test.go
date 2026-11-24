package wv_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
)

// TestTagsFitAgreesWithTheWrite: at the APEv2 block's edge, a set one byte
// under is answered fitting and written, and one byte over is answered with
// the refusal Begin gives.
func TestTagsFitAgreesWithTheWrite(t *testing.T) {
	track, _ := muxBlocks(t, 1000, 1000)
	// Header and footer, then the one item: its length, flags, the key with
	// its NUL, and the value.
	room := 48<<10 - (2*32 + 8 + len("COMMENT") + 1)
	for _, tc := range []struct {
		n    int
		fits bool
	}{{room, true}, {room + 1, false}} {
		tags := []container.Tag{{Key: "COMMENT", Value: strings.Repeat("c", tc.n)}}
		fitErr := wv.TagsFit(tags)
		beginErr := wv.NewMuxer(&bytes.Buffer{}, &wv.MuxerOptions{Tags: tags}).Begin([]container.Track{track})
		if (fitErr == nil) != tc.fits || (beginErr == nil) != tc.fits {
			t.Errorf("%d bytes: TagsFit = %v, Begin = %v, want fits %t", tc.n, fitErr, beginErr, tc.fits)
		}
		if !tc.fits && fitErr.Error() != beginErr.Error() {
			t.Errorf("%d bytes: TagsFit says %q, Begin %q", tc.n, fitErr, beginErr)
		}
	}
}
