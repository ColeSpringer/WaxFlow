package server

import (
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/gain"
)

// TestHEv2InCacheKeys pins the stage-3b key contract: hev2 is a shaping
// parameter, so it must land in the shared canonical core and so in
// BOTH the progressive and the segmented cache keys. v1 and v2 at one
// explicit bitrate agree on every other plan-visible value.
//
// Today the keys would split even without this term, because the plan's
// Versions carry HEV2EncoderVersion against HEEncoderVersion and
// cache.NewKey hashes versions too. That is deliberate defense in
// depth, not redundancy to prune: versions exist to track algorithm
// revisions (ADR-0004), and nothing stops a future revision from
// merging the two strings; the canonical core is where request-shape
// identity is contracted to live, which is exactly what this test pins.
func TestHEv2InCacheKeys(t *testing.T) {
	plan := &waxflow.TranscodePlan{
		Format: audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2),
			Type: audio.Float, BitDepth: 32},
		Container: "progressive",
		BitRate:   64000,
	}
	p1 := canonicalParams(plan, 0, gain.PresetOff, span{}, false, 0)
	p2 := canonicalParams(plan, 0, gain.PresetOff, span{}, true, 0)
	if p1 == p2 {
		t.Fatalf("progressive keys collide across hev2: %s", p1)
	}
	if !strings.Contains(p1, "hev2=false") || !strings.Contains(p2, "hev2=true") {
		t.Errorf("progressive keys do not spell the hev2 term:\n%s\n%s", p1, p2)
	}

	seg := &waxflow.SegmentPlan{TranscodePlan: *plan, SegmentSamples: 96 * 2048}
	h1 := canonicalHLSParams(seg, 0, gain.PresetOff, span{}, false)
	h2 := canonicalHLSParams(seg, 0, gain.PresetOff, span{}, true)
	if h1 == h2 {
		t.Fatalf("segmented keys collide across hev2: %s", h1)
	}
	if !strings.Contains(h1, "hev2=false") || !strings.Contains(h2, "hev2=true") {
		t.Errorf("segmented keys do not spell the hev2 term:\n%s\n%s", h1, h2)
	}
}
