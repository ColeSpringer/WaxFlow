package wv_test

import (
	"testing"

	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the block scan and a bisection probe land, and what the tail
// scan settles. Regenerate with `make goldens` and review the diff.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return wv.NewDemuxer(src, nil) }
	for i, file := range []string{
		"testdata/golden-known.wv", // this tree's muxer: a declared total, a tag
		"testdata/golden-pipe.wv",  // no declared total
		"testdata/seek-mono.wv",    // the reference encoder's: twenty blocks and a closing one without samples
	} {
		testutil.GoldenDamagedRead(t, file, open, *update, func(t *testing.T, raw []byte) []testutil.Damage {
			// Block starts by each header's own length, then where the blocks
			// end and a tag begins.
			var blocks []int
			end := 0
			for end+wavpack.BlockHeaderLen <= len(raw) && wavpack.SyncOK(raw[end:]) {
				h, err := wavpack.ParseBlockHeader(raw[end:])
				if err != nil {
					t.Fatal(err)
				}
				blocks = append(blocks, end)
				end += int(h.Size)
			}
			mid := len(blocks) / 2
			blocks = append(blocks, end)
			damages := []testutil.Damage{
				testutil.JunkAt("junk-mid", raw, blocks[mid], 'w'),
				testutil.JunkOf("junk-span", raw, blocks[mid], testutil.SpanJunkLen, 'w'),
				// The scan's first window ends inside the magic behind the junk.
				testutil.JunkOf("junk-window", raw, blocks[mid], srcwin.Chunk-2, 'w'),
				testutil.DecoyAt("decoy-mid", raw, blocks[mid], 'w'),
				// A lone block has no block behind it to confirm it.
				testutil.IslandAt("junk-island", raw, blocks[mid], blocks[mid+1], 'w'),
				testutil.JunkAt("junk-tail", raw, end, 'w'),
			}
			if len(blocks) > 4 {
				damages = append(damages,
					// One with the next block's header behind it does.
					testutil.IslandAt("junk-island-header", raw, blocks[mid], blocks[mid+1]+wavpack.BlockHeaderLen, 'w'),
					// Past what a resync crosses, with blocks left behind the
					// junk after the cut: only a probe that lands there
					// measures them.
					testutil.JunkOf("junk-probe", raw[:end], blocks[mid], wv.MaxResyncForTest+testutil.JunkLen, 'w').Cut(100))
			}
			if i == 0 {
				// A block at the last offset the scan takes, and at the first
				// it does not.
				damages = append(damages,
					testutil.JunkOf("junk-under-bound", raw, blocks[mid], wv.MaxResyncForTest-1, 'w'),
					testutil.JunkOf("junk-past-bound", raw, blocks[mid], wv.MaxResyncForTest, 'w'))
			}
			return damages
		})
	}
}
