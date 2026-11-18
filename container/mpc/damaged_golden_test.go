package mpc_test

import (
	"flag"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the scan for the magic lands behind junk, and behind junk
// that spells one. Regenerate with `make goldens` and review the diff.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return mpc.NewDemuxer(src, nil) }
	for i, file := range []string{
		"testdata/seek-pns.mpc",    // SV8
		"testdata/gapless-sv7.mpc", // SV7
	} {
		testutil.GoldenDamagedRead(t, file, open, *update, func(t *testing.T, raw []byte) []testutil.Damage {
			// Junk in front of the magic is a tagger's padding, and unreported.
			silent := func(d testutil.Damage) testutil.Damage { d.Silent = true; return d }
			damages := []testutil.Damage{
				silent(testutil.JunkAt("junk-head", raw, 0, 'M')),
				silent(testutil.JunkOf("junk-span", raw, 0, testutil.SpanJunkLen, 'M')),
				testutil.DecoyAt("decoy-head", raw, 0, 'M'),
			}
			if i == 0 {
				// A magic at the last offset the scan takes, and at the first it
				// does not.
				damages = append(damages,
					silent(testutil.JunkOf("junk-under-bound", raw, 0, mpc.MaxJunkForTest-1, 'M')),
					testutil.JunkOf("junk-past-bound", raw, 0, mpc.MaxJunkForTest, 'M'))
			}
			return damages
		})
	}
}
