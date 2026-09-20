package waxflow_test

// The album measurement. Each member is metered at its width and the gates
// run over the union of their blocks, which is what makes one gain right
// for every member (ADR-0010).

import (
	"context"
	"math"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// openMember opens raw WAV bytes as a Media for AnalyzeGroup.
func openMember(t *testing.T, raw []byte) format.Media {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "wav", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { med.Close() })
	return med
}

// TestAnalyzeGroupOfOneEqualsTheMember: a group of identical members must
// measure what one of them does. Integrated loudness is a power mean over
// gated blocks, so repeating the same blocks cannot move it, and this is
// what says the accumulator is folding rather than averaging.
func TestAnalyzeGroupOfOneEqualsTheMember(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2, 0.2}, []float64{440, 523}))
	single, err := e.Analyze(context.Background(), container.BytesSource(src), "wav", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 3} {
		members := make([]waxflow.GroupMember, n)
		for i := range members {
			members[i] = waxflow.GroupMember{Media: openMember(t, src)}
		}
		got, err := e.AnalyzeGroup(context.Background(), members, waxflow.AnalyzeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Members) != n {
			t.Fatalf("%d member results, want %d", len(got.Members), n)
		}
		if d := math.Abs(got.Group.IntegratedLUFS - single.IntegratedLUFS); d > 1e-9 {
			t.Errorf("%d identical members measure %.6f LUFS, one measures %.6f", n, got.Group.IntegratedLUFS, single.IntegratedLUFS)
		}
		if got.Group.TruePeakDB != single.TruePeakDB || got.Group.SamplePeakDB != single.SamplePeakDB {
			t.Errorf("peaks moved: %.6f/%.6f against %.6f/%.6f",
				got.Group.TruePeakDB, got.Group.SamplePeakDB, single.TruePeakDB, single.SamplePeakDB)
		}
		if want := single.Samples * int64(n); got.Group.Samples != want {
			t.Errorf("Samples = %d, want %d", got.Group.Samples, want)
		}
		for i, m := range got.Members {
			if m.IntegratedLUFS != single.IntegratedLUFS {
				t.Errorf("member %d measures %.6f, want the single %.6f", i, m.IntegratedLUFS, single.IntegratedLUFS)
			}
		}
	}
}

// TestAnalyzeGroupMeasuresEachMemberAtItsOwnWidth is the finding this API
// exists for. A mono member measured at its own width is the number a
// listener hears; measured inside a stereo envelope it reads 3.01 dB hot,
// because the envelope duplicates it, and an album gain computed from that
// pushes the member down by the same 3 dB.
func TestAnalyzeGroupMeasuresEachMemberAtItsOwnWidth(t *testing.T) {
	e := waxflow.New()
	stereo := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2, 0.2}, []float64{440, 523}))
	mono := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.25}, []float64{330}))

	own, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		{Media: openMember(t, stereo)},
		{Media: openMember(t, mono)},
	}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	widened, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		{Media: openMember(t, stereo), Channels: 2},
		{Media: openMember(t, mono), Channels: 2},
	}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The mono member alone, measured both ways, is the 3.01 dB.
	solo, err := e.Analyze(context.Background(), container.BytesSource(mono), "wav", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := own.Members[1].IntegratedLUFS - solo.IntegratedLUFS; math.Abs(d) > 1e-9 {
		t.Errorf("the mono member at its own width measures %.6f, alone it measures %.6f",
			own.Members[1].IntegratedLUFS, solo.IntegratedLUFS)
	}
	const dup = 3.0102999566398120 // 10*log10(2)
	if d := widened.Members[1].IntegratedLUFS - own.Members[1].IntegratedLUFS; math.Abs(d-dup) > 1e-3 {
		t.Errorf("widening the mono member shifted it by %.6f LU, want %.6f", d, dup)
	}
	// And the group's own number moves with it, which is the album gain.
	if widened.Group.IntegratedLUFS <= own.Group.IntegratedLUFS {
		t.Errorf("the widened group measures %.6f, not above the own-width %.6f",
			widened.Group.IntegratedLUFS, own.Group.IntegratedLUFS)
	}
	// The stereo member is unaffected either way: Channels 2 on a stereo
	// source is a no-op.
	if own.Members[0].IntegratedLUFS != widened.Members[0].IntegratedLUFS {
		t.Errorf("the stereo member moved: %.6f against %.6f",
			own.Members[0].IntegratedLUFS, widened.Members[0].IntegratedLUFS)
	}
}

// TestAnalyzeGroupRefusesPerSourceOptions: a silence map and a tap are
// properties of one source's timeline, and a group has none.
func TestAnalyzeGroupRefusesPerSourceOptions(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2}, []float64{440}))
	for _, tc := range []struct {
		name string
		opts waxflow.AnalyzeOptions
	}{
		{"silence", waxflow.AnalyzeOptions{Silence: &waxflow.SilenceOptions{}}},
		{"tap", waxflow.AnalyzeOptions{Tap: func([][]float32) error { return nil }}},
		{"channels", waxflow.AnalyzeOptions{Channels: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.AnalyzeGroup(context.Background(),
				[]waxflow.GroupMember{{Media: openMember(t, src)}}, tc.opts)
			if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Errorf("err = %v, want an invalid-request refusal", err)
			}
		})
	}
	if _, err := e.AnalyzeGroup(context.Background(), nil, waxflow.AnalyzeOptions{}); err == nil {
		t.Error("an empty group was accepted")
	}
}

// TestAnalyzeGroupProgressSpansTheMembers: the callback counts across the
// whole group against the sum of the members' totals, so a caller's bar
// does not restart at every track.
func TestAnalyzeGroupProgressSpansTheMembers(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2}, []float64{440}))
	var last, total int64
	var back bool
	_, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		{Media: openMember(t, src)}, {Media: openMember(t, src)},
	}, waxflow.AnalyzeOptions{Progress: func(done, t int64) {
		if done < last {
			back = true
		}
		last, total = done, t
	}})
	if err != nil {
		t.Fatal(err)
	}
	if back {
		t.Error("progress went backwards between members")
	}
	if want := int64(2 * analyzeChFrames); total != want {
		t.Errorf("progress total %d, want the members' sum %d", total, want)
	}
	if last != total {
		t.Errorf("progress finished at %d of %d", last, total)
	}
}
