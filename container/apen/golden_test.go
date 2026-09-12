package apen_test

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// goldenFrames is two whole frames plus a short one, so the golden pins the
// seek table with more than one entry, the final frame's stated length and the
// remainder arithmetic all at once.
const goldenFrames = 2*ape.BlocksPerFrame + 4321

// goldenTags exercise the two things the tag builder decides: a multi-valued
// key becomes one NUL-separated item, and a reserved item name is dropped.
var goldenTags = []container.Tag{
	{Key: "TITLE", Value: "Golden"},
	{Key: "ARTIST", Value: "First"},
	{Key: "ARTIST", Value: "Second"},
	{Key: "ID3", Value: "dropped"},
}

// TestGoldenMuxOutputs pins the muxer's byte-exact output, which is what the
// ADR-0004 MuxerVersion stands for: the descriptor, the seek table that is the
// format's only index, the MD5 over the lot and the APEv2 block behind it.
// Regenerate with `make goldens` and review the diff. The encoder is
// integer-only, so the bytes are the same on every architecture.
//
// The second case is the same stream written by a caller who would not say how
// long it is. That layout reserves every entry the header's 32-bit block count
// could ever need, which at this frame length is 233 KB of zeros: too big to
// commit, so the golden is its SHA-256. It is still a file rather than a
// constant in this test, so `make goldens` regenerates it like any other.
func TestGoldenMuxOutputs(t *testing.T) {
	// One encode for both cases: only the Track's projected length differs,
	// and the frames do not depend on it.
	track, pkts := muxFramesAt(t, goldenFrames, goldenFrames, ape.LevelFast)
	mux := func(projected int64) []byte {
		track := track
		track.Samples = projected
		return writeAll(t, track, pkts, &apen.MuxerOptions{Tags: goldenTags})
	}

	t.Run("golden-known.ape", func(t *testing.T) {
		testutil.Golden(t, filepath.Join("testdata", "golden-known.ape"), mux(goldenFrames), *update)
	})
	t.Run("golden-unknown-length.sha256", func(t *testing.T) {
		sum := sha256.Sum256(mux(-1))
		testutil.Golden(t, filepath.Join("testdata", "golden-unknown-length.sha256"),
			[]byte(hex.EncodeToString(sum[:])+"\n"), *update)
	})
}
