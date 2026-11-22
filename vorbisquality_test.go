package waxflow_test

import (
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/psy"
)

// TestVorbisQualityIsInThePlanVersions: two Vorbis qualities never share a
// cache key, and the default quality, unset or named, keeps the key it had
// before the quality joined the version term.
func TestVorbisQualityIsInThePlanVersions(t *testing.T) {
	e := waxflow.New()
	track := container.Track{
		Codec:   codec.PCM,
		Fmt:     audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16},
		Samples: 441000, Default: true, SamplesExact: true,
	}
	versions := func(q float64) []string {
		t.Helper()
		p, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "vorbis", VorbisQuality: q})
		if err != nil {
			t.Fatalf("quality %g: %v", q, err)
		}
		return p.Versions
	}
	if v2, v6 := versions(2), versions(6); slices.Equal(v2, v6) {
		t.Errorf("qualities 2 and 6 share the versions %q", v2)
	}
	v0, v3 := versions(0), versions(3)
	if !slices.Equal(v0, v3) {
		t.Errorf("quality 0 gives %q, the default 3 gives %q", v0, v3)
	}
	if today := "vorbis-enc-11+" + psy.Version; !slices.Contains(v0, today) {
		t.Errorf("the default quality's versions %q lost the term %q", v0, today)
	}
}
