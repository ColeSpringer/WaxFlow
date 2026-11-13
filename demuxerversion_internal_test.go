package waxflow

import (
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// TestDemuxerVersionRidesEveryPlan: a track that names its demuxer's revision
// puts it at the head of every plan built from it, whichever rung, so a
// demuxer that changes what it delivers retires the cached bytes of its own
// sources. A track that names none keys exactly as it did before the term
// existed.
func TestDemuxerVersionRidesEveryPlan(t *testing.T) {
	const term = "x-demux-9"
	e := New()
	pcm := container.Track{
		Codec:   codec.PCM,
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16},
		Samples: 480000, Default: true, SamplesExact: true,
	}
	opus := container.Track{
		Codec: codec.Opus, CodecConfig: testOpusHead(312),
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: 480000, Delay: 312, Default: true, SamplesExact: true,
	}
	toOpus := TranscodeOptions{Format: "opus"}
	span := []Span{{96000, 288000}}
	for _, p := range []struct {
		name     string
		track    container.Track
		versions func(container.Track) ([]string, error)
	}{
		{"PlanTranscode", pcm, func(tr container.Track) ([]string, error) {
			p, err := e.PlanTranscode(tr, TranscodeOptions{Format: "flac"})
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		}},
		{"PlanSegments", pcm, func(tr container.Track) ([]string, error) {
			p, err := e.PlanSegments(tr, toOpus, 2)
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		}},
		{"PlanRemux", opus, func(tr container.Track) ([]string, error) {
			p, err := e.PlanRemux(tr, toOpus)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		}},
		{"PlanRemuxSegments", opus, func(tr container.Track) ([]string, error) {
			p, err := e.PlanRemuxSegments(tr, toOpus, 2, 960)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		}},
		{"PlanCut", opus, func(tr container.Track) ([]string, error) {
			p, err := e.PlanCut(tr, toOpus, span, 960)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		}},
		{"PlanCutSegments", opus, func(tr container.Track) ([]string, error) {
			p, err := e.PlanCutSegments(tr, toOpus, span, 960, 2)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		}},
	} {
		plain, err := p.versions(p.track)
		if err != nil || len(plain) == 0 {
			t.Fatalf("%s: no plan for the control track (%v); the row proves nothing", p.name, err)
		}
		if slices.Contains(plain, "") || slices.Contains(plain, term) {
			t.Errorf("%s: a track naming no demuxer revision keys on %q", p.name, plain)
		}
		named := p.track
		named.DemuxerVersion = term
		got, err := p.versions(named)
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if want := append([]string{term}, plain...); !slices.Equal(got, want) {
			t.Errorf("%s: versions = %q, want the demuxer's revision and then %q", p.name, got, plain)
		}
	}

	// A span is still its source's track, so it keeps the term.
	named := pcm
	named.DemuxerVersion = term
	if spanned, err := SpanTrack(named, 100, 200); err != nil || spanned.DemuxerVersion != term {
		t.Errorf("SpanTrack: demuxer revision %q, err %v; want it kept", spanned.DemuxerVersion, err)
	}

	// A timeline's synthetic track is no member's, so the plan lists each
	// member's term itself, once.
	other := pcm
	other.DemuxerVersion = "y-demux-2"
	for _, tl := range []struct {
		name     string
		versions func(tracks []container.Track) ([]string, error)
	}{
		{"PlanTranscodeTimeline", func(tracks []container.Track) ([]string, error) {
			p, err := e.PlanTranscodeTimeline(tracks, ConcatOptions{}, TranscodeOptions{Format: "flac"})
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		}},
		{"PlanSegmentsTimeline", func(tracks []container.Track) ([]string, error) {
			p, err := e.PlanSegmentsTimeline(tracks, ConcatOptions{}, toOpus, 2)
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		}},
	} {
		plain, err := tl.versions([]container.Track{pcm, pcm, pcm})
		if err != nil {
			t.Fatalf("%s: %v", tl.name, err)
		}
		got, err := tl.versions([]container.Track{pcm, named, other, named})
		if err != nil {
			t.Fatalf("%s: %v", tl.name, err)
		}
		for _, want := range []string{term, "y-demux-2"} {
			if n := len(slices.DeleteFunc(slices.Clone(got), func(v string) bool { return v != want })); n != 1 {
				t.Errorf("%s: %q appears %d times in %q, want once", tl.name, want, n, got)
			}
		}
		if rest := slices.DeleteFunc(slices.Clone(got), func(v string) bool { return v == term || v == "y-demux-2" }); !slices.Equal(rest, plain) {
			t.Errorf("%s: beside the demuxer revisions the versions are %q, want %q", tl.name, rest, plain)
		}
	}
}
