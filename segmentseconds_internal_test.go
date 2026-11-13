package waxflow

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestSegmentSecondsMustBeFinite: every segmented plan refuses a segment
// duration that is no number. A NaN compares false against both bounds, so
// the range check passed it and the frame count came from converting it to
// an int, whose value the language leaves to the platform.
func TestSegmentSecondsMustBeFinite(t *testing.T) {
	e := New()
	pcm := container.Track{
		Codec:   codec.PCM,
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16},
		Samples: 480000, Default: true,
	}
	opus := container.Track{
		Codec: codec.Opus, CodecConfig: testOpusHead(312),
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: 480000, Delay: 312, Default: true, SamplesExact: true,
	}
	opts := TranscodeOptions{Format: "opus"}
	plans := []struct {
		name string
		plan func(seconds float64) (planned bool, err error)
	}{
		{"PlanSegments", func(s float64) (bool, error) {
			p, err := e.PlanSegments(pcm, opts, s)
			return p != nil, err
		}},
		{"PlanRemuxSegments", func(s float64) (bool, error) {
			p, err := e.PlanRemuxSegments(opus, opts, s, 960)
			return p != nil, err
		}},
		{"PlanCutSegments", func(s float64) (bool, error) {
			p, err := e.PlanCutSegments(opus, opts, []Span{{96000, 288000}}, 960, s)
			return p != nil, err
		}},
	}
	for _, p := range plans {
		// The control: these inputs plan, so a refusal below is the
		// duration's and not a decline for some other reason.
		if planned, err := p.plan(2); err != nil || !planned {
			t.Fatalf("%s at 2 s: planned %v, err %v; the rows below prove nothing", p.name, planned, err)
		}
		for _, seconds := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
			planned, err := p.plan(seconds)
			if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Errorf("%s at %g s: planned %v, err %v; want an invalid-request refusal", p.name, seconds, planned, err)
			}
		}
	}
}
