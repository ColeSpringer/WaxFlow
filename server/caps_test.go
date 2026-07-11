package server

import (
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
)

// TestDeliveryProfilesAreHonest pins the /caps profile contract: the
// four named profiles exist, every format they advertise is actually
// served by this build (live output for progressive, segmented for
// hls), the recommendation names a real surface, and every profile
// states its evidence basis. The per-cell facts live in
// docs/client-matrix.md; this keeps the wire shape mechanically inside
// the build's capabilities.
func TestDeliveryProfilesAreHonest(t *testing.T) {
	caps := buildCaps(true, true, true)

	want := []string{"android-exoplayer", "apple-native", "desktop-mpv", "hls-js"}
	var got []string
	for name := range caps.Profiles {
		got = append(got, name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("profile names = %v, want %v", got, want)
	}

	live := map[string]bool{}
	for _, o := range waxflow.Outputs() {
		if o.Live {
			live[o.Name] = true
		}
	}
	segmented := map[string]bool{}
	for _, name := range caps.Delivery.HLSFormats {
		segmented[name] = true
	}

	for name, p := range caps.Profiles {
		if p.Delivery != "hls" && p.Delivery != "progressive" {
			t.Errorf("%s: delivery = %q, want hls or progressive", name, p.Delivery)
		}
		if p.Basis == "" {
			t.Errorf("%s: empty basis; profiles must state their evidence", name)
		}
		if len(p.Progressive) == 0 || len(p.HLS) == 0 {
			t.Errorf("%s: empty capability list (progressive %d, hls %d)", name, len(p.Progressive), len(p.HLS))
		}
		for _, f := range p.Progressive {
			if !live[f] {
				t.Errorf("%s: progressive advertises %q, which has no live output row", name, f)
			}
		}
		for _, f := range p.HLS {
			if !segmented[f] {
				t.Errorf("%s: hls advertises %q, which has no segmented form", name, f)
			}
		}
	}

	// The Apple recommendation steers to HLS until the progressive
	// live-transcode checklist cell is verified (the recorded M19
	// decision); a change here must be deliberate.
	if caps.Profiles["apple-native"].Delivery != "hls" {
		t.Error("apple-native must recommend hls (progressive live transcodes are a manual checklist cell)")
	}
}
