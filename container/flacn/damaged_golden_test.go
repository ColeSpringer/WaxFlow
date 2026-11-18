package flacn_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the scan for the first frame lands, and where a bisection
// probe does. Regenerate with `make goldens` and review the diff.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return flacn.NewDemuxer(src, nil) }
	for i, file := range []string{
		"testdata/golden-sine-s16-l5.flac", // this tree's muxer: a SEEKTABLE, two frames
		"../../testdata/sine-s24.flac",     // ffmpeg's: a comment and padding, four frames
	} {
		testutil.GoldenDamagedRead(t, file, open, *update, func(t *testing.T, raw []byte) []testutil.Damage {
			spans := frameSpans(t, raw)
			first, mid := spans[0][0], spans[len(spans)/2][0]
			// Long enough to bisect: the open's measurement does when the
			// tail is cut, a seek always, and both probe from inside the junk.
			const long = 300 << 10
			damages := []testutil.Damage{
				testutil.JunkAt("junk-head", raw, first, 0xFF),
				testutil.JunkOf("junk-span", raw, first, testutil.SpanJunkLen, 0xFF),
				testutil.DecoyAt("decoy-head", raw, first, 0xFF),
				testutil.JunkAt("junk-mid", raw, mid, 0xFF),
				testutil.JunkAt("junk-tail", raw, len(raw), 0xFF),
				testutil.JunkOf("junk-probe", raw, mid, long, 0xFF).Cut(100),
				// The decoy is the first header a seek's probe meets.
				testutil.DecoyOf("decoy-probe", raw, mid, long, long-2048, 0xFF),
			}
			if i == 0 {
				// A frame at the last offset the scan takes, and at the first
				// it does not.
				damages = append(damages,
					testutil.JunkOf("junk-under-bound", raw, first, flacn.MaxResyncForTest-1, 0xFF),
					testutil.JunkOf("junk-past-bound", raw, first, flacn.MaxResyncForTest, 0xFF))
			}
			return damages
		})
	}
}
