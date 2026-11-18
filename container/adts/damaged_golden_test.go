package adts_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the frame scan lands in front of the first frame, between
// frames and at the end. Regenerate with `make goldens` and review the diff.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return adts.NewDemuxer(src, nil) }
	for i, file := range []string{
		"testdata/mono.aac",                      // AAC-LC
		"testdata/golden-heaac-implicit.aac",     // HE-AAC v1, implicit SBR
		"../../codec/aac/testdata/fdk_he_v2.aac", // HE-AAC v2, implicit SBR and PS
	} {
		testutil.GoldenDamagedRead(t, file, open, *update, func(t *testing.T, raw []byte) []testutil.Damage {
			// Frame starts by each header's own length, then the end.
			var frames []int
			for off := 0; off < len(raw); {
				if off+7 > len(raw) || raw[off] != 0xFF || raw[off+1]&0xF6 != 0xF0 {
					t.Fatalf("no frame header at %d", off)
				}
				n := int(raw[off+3]&3)<<11 | int(raw[off+4])<<3 | int(raw[off+5])>>5
				if n < 7 {
					t.Fatalf("the header at %d states a frame of %d bytes", off, n)
				}
				frames = append(frames, off)
				off += n
			}
			mid, last := len(frames)/2, len(frames)-1
			frames = append(frames, len(raw))
			damages := []testutil.Damage{
				testutil.JunkAt("junk-head", raw, 0, 0xFF),
				testutil.JunkOf("junk-span", raw, 0, testutil.SpanJunkLen, 0xFF),
				testutil.JunkAt("junk-mid", raw, frames[mid], 0xFF),
				testutil.DecoyAt("decoy-mid", raw, frames[mid], 0xFF),
				// A lone frame has no header behind it to confirm it, one with
				// the next frame's head does, and the last frame needs none.
				testutil.IslandAt("junk-island", raw, frames[mid], frames[mid+1], 0xFF),
				testutil.IslandAt("junk-island-header", raw, frames[mid],
					frames[mid+1]+min(32, frames[mid+2]-frames[mid+1]-1), 0xFF),
				testutil.JunkAt("junk-last", raw, frames[last], 0xFF),
				testutil.JunkAt("junk-tail", raw, len(raw), 0xFF),
			}
			if i == 0 {
				// A frame at the last offset the scan takes, and at the first
				// it does not.
				damages = append(damages,
					testutil.JunkOf("junk-under-bound", raw, 0, adts.MaxResyncForTest-1, 0xFF),
					testutil.JunkOf("junk-past-bound", raw, 0, adts.MaxResyncForTest, 0xFF))
			}
			return damages
		})
	}
}
