package apen_test

import (
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the scan for the file header lands behind junk, and behind
// junk that spells one. Regenerate with `make goldens` and review the diff.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return apen.NewDemuxer(src, nil) }
	for i, file := range []string{
		"testdata/tagged.ape", // one frame, a tag behind it
		"testdata/seek.ape",   // three frames, no tag
	} {
		testutil.GoldenDamagedRead(t, file, open, *update, func(t *testing.T, raw []byte) []testutil.Damage {
			h, err := ape.ParseHeader(raw[:min(len(raw), ape.MaxHeaderLen)])
			if err != nil {
				t.Fatal(err)
			}
			// Junk in front of the header is a tagger's padding, and
			// unreported; so is a header there that does not parse.
			silent := func(d testutil.Damage) testutil.Damage { d.Silent = true; return d }
			damages := []testutil.Damage{
				silent(testutil.JunkAt("junk-head", raw, 0, 'M')),
				silent(testutil.JunkOf("junk-span", raw, 0, testutil.SpanJunkLen, 'M')),
				// The scan's first window ends inside the magic behind the junk.
				silent(testutil.JunkOf("junk-window", raw, 0, srcwin.Chunk-2, 'M')),
				silent(testutil.DecoyAt("decoy-head", raw, 0, 'M')),
				// The whole header, and junk where it puts the seek table.
				testutil.IslandAt("junk-island", raw, 0, int(h.SeekTableOffset), 'M'),
			}
			if i == 0 {
				// A header at the last offset the scan takes, and at the first
				// it does not.
				damages = append(damages,
					silent(testutil.JunkOf("junk-under-bound", raw, 0, apen.MaxJunkForTest-1, 'M')),
					testutil.JunkOf("junk-past-bound", raw, 0, apen.MaxJunkForTest, 'M'))
			}
			return damages
		})
	}
}
