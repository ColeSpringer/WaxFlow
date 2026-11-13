package waxflow

import (
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// TestFloatBuildVersionNamesThisBuild: the term is empty for amd64 below
// GOAMD64=v3, the one target where the compiler fuses no multiply-add, and
// names the architecture, and the level on amd64, everywhere else.
func TestFloatBuildVersionNamesThisBuild(t *testing.T) {
	want := "fp-" + runtime.GOARCH
	if runtime.GOARCH == "amd64" {
		level := ""
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "GOAMD64" {
					level = s.Value
				}
			}
		}
		switch level {
		case "v1", "v2":
			want = ""
		case "v3", "v4":
			want = "fp-amd64" + level
		default:
			t.Skipf("this binary does not say its GOAMD64 level (%q)", level)
		}
	}
	if FloatBuildVersion != want {
		t.Errorf("FloatBuildVersion = %q on this build, want %q", FloatBuildVersion, want)
	}
}

// TestFloatBuildVersionRidesTranscodePlans: a plan that runs a chain keys on
// how this build rounds floats, once, behind the decoder's term; a rung that
// copies packets computes nothing and names no build. On amd64 at its
// default level the term is empty and no plan carries one.
func TestFloatBuildVersionRidesTranscodePlans(t *testing.T) {
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
	builds := func(vs []string) int {
		return len(slices.DeleteFunc(slices.Clone(vs), func(v string) bool { return !strings.HasPrefix(v, "fp-") }))
	}
	want := 0
	if FloatBuildVersion != "" {
		want = 1
	}

	transcodes := map[string]func() ([]string, error){
		"PlanTranscode": func() ([]string, error) {
			p, err := e.PlanTranscode(pcm, TranscodeOptions{Format: "flac"})
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanTranscode of a lossy source": func() ([]string, error) {
			p, err := e.PlanTranscode(opus, TranscodeOptions{Format: "flac"})
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanSegments": func() ([]string, error) {
			p, err := e.PlanSegments(pcm, toOpus, 2)
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanTranscodeTimeline": func() ([]string, error) {
			p, err := e.PlanTranscodeTimeline([]container.Track{pcm, pcm}, ConcatOptions{}, toOpus)
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanSegmentsTimeline": func() ([]string, error) {
			p, err := e.PlanSegmentsTimeline([]container.Track{pcm, pcm}, ConcatOptions{}, toOpus, 2)
			if err != nil {
				return nil, err
			}
			return p.Versions, nil
		},
	}
	for name, plan := range transcodes {
		vs, err := plan()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := builds(vs); got != want {
			t.Errorf("%s: %d build terms in %q, want %d (this build's is %q)", name, got, vs, want, FloatBuildVersion)
		}
		if want == 1 && !slices.Contains(vs, FloatBuildVersion) {
			t.Errorf("%s: versions %q do not name this build, %q", name, vs, FloatBuildVersion)
		}
	}

	copies := map[string]func() ([]string, error){
		"PlanRemux": func() ([]string, error) {
			p, err := e.PlanRemux(opus, toOpus)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanRemuxSegments": func() ([]string, error) {
			p, err := e.PlanRemuxSegments(opus, toOpus, 2, 960)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanCut": func() ([]string, error) {
			p, err := e.PlanCut(opus, toOpus, span, 960)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		},
		"PlanCutSegments": func() ([]string, error) {
			p, err := e.PlanCutSegments(opus, toOpus, span, 960, 2)
			if err != nil || p == nil {
				return nil, err
			}
			return p.Versions, nil
		},
	}
	for name, plan := range copies {
		vs, err := plan()
		if err != nil || len(vs) == 0 {
			t.Fatalf("%s: no plan (%v); the row proves nothing", name, err)
		}
		if builds(vs) != 0 {
			t.Errorf("%s: a copy rung names a build in %q", name, vs)
		}
	}
}
