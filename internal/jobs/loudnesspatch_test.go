package jobs

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/source"
)

// A loudness job to either MP4 form reads back with the values its Analysis
// reports patched over the placeholders the muxer wrote at Begin.
func TestLoudnessJobPatchesMP4(t *testing.T) {
	for _, form := range []string{waxflow.ContainerProgressive, waxflow.ContainerFragmented} {
		t.Run(form, func(t *testing.T) {
			root := t.TempDir()
			ref := writeLongWAV(t, root)
			res := openRoots(t, root)
			r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})
			j, err := r.Create(Request{
				Type: TypeTranscode, Src: source.Ref(ref), SourceID: pinID(t, res, ref),
				Params: outshape.Params{Format: "alac", Container: form}, Loudness: "analyze",
			})
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			a := done.Analysis
			if a == nil || a.ReplayGainTrackGain == "" || a.ReplayGainTrackPeak == "" {
				t.Fatalf("analysis: %+v", a)
			}
			// The gain lands near zero on a normalized output, so the peak is
			// what tells a patched file from its placeholders.
			if a.ReplayGainTrackPeak == meta.FormatPeak(0) {
				t.Fatalf("the analysis reports the placeholder peak %q", a.ReplayGainTrackPeak)
			}
			f, err := os.Open(r.OutputPath(done, 0))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			src, err := container.FileSource(f)
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(context.Background(), src, "m4a", nil)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"REPLAYGAIN_TRACK_GAIN": a.ReplayGainTrackGain,
				"REPLAYGAIN_TRACK_PEAK": a.ReplayGainTrackPeak,
			} {
				if got := info.Tags[key]; !slices.Equal(got, []string{want}) {
					t.Errorf("%s reads back %q, want [%q]", key, got, want)
				}
			}
		})
	}
}
