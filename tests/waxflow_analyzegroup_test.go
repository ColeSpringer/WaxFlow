package waxflow_test

// The album measurement. Each member is metered at its width and the gates
// run over the union of their blocks, which is what makes one gain right
// for every member (ADR-0010).

import (
	"context"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
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

// closeCountingMedia reports its Close to the test that opened it, so a
// test can count how many members the engine holds open at once, and
// whether the engine closed a member it was handed open. It embeds the
// interface alone, the shape a caller's own file-closing wrapper has, so a
// member is measured through exactly what a caller hands the engine.
type closeCountingMedia struct {
	format.Media
	onClose func()
}

func (m *closeCountingMedia) Close() error {
	m.onClose()
	return m.Media.Close()
}

// lazyMembers builds on-demand members over raw WAV bytes and returns the
// counters the test reads: the open sequence, the number open right now,
// and the most that were open at any moment.
type lazyLedger struct {
	opened []int
	open   int
	peak   int
}

func (l *lazyLedger) member(t *testing.T, i int, raw []byte, channels int) waxflow.GroupMember {
	t.Helper()
	return waxflow.GroupMember{Channels: channels, Open: func() (format.Media, error) {
		med, err := format.Open(container.BytesSource(raw), "wav", nil)
		if err != nil {
			return nil, err
		}
		l.opened = append(l.opened, i)
		l.open++
		l.peak = max(l.peak, l.open)
		return &closeCountingMedia{Media: med, onClose: func() { l.open-- }}, nil
	}}
}

// TestAnalyzeGroupOpensMembersOnDemand is the descriptor promise: a member
// handed an Open function is opened when the run reaches it and closed
// before the next one opens, so an album of any length holds one file at a
// time, and the numbers are the ones the same members measure when handed
// open.
func TestAnalyzeGroupOpensMembersOnDemand(t *testing.T) {
	e := waxflow.New()
	stereo := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2, 0.2}, []float64{440, 523}))
	mono := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.25}, []float64{330}))
	// The handed-open members are the caller's to close: the engine must
	// not close one, however it closes the ones it opened itself.
	var closed int
	handed := func(raw []byte) format.Media {
		return &closeCountingMedia{Media: openMember(t, raw), onClose: func() { closed++ }}
	}
	handedOpen, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		{Media: handed(stereo)},
		{Media: handed(mono), Channels: 2},
		{Media: handed(mono)},
	}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Errorf("the engine closed %d members it was handed open", closed)
	}

	var l lazyLedger
	onDemand, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		l.member(t, 0, stereo, 0), l.member(t, 1, mono, 2), l.member(t, 2, mono, 0),
	}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.opened, []int{0, 1, 2}) {
		t.Errorf("members opened in order %v, want 0, 1, 2", l.opened)
	}
	if l.peak != 1 {
		t.Errorf("%d members were open at once, want one at a time", l.peak)
	}
	if l.open != 0 {
		t.Errorf("%d members still open after the call", l.open)
	}
	if !reflect.DeepEqual(onDemand.Group, handedOpen.Group) {
		t.Errorf("the group measures %+v on demand, %+v handed open", onDemand.Group, handedOpen.Group)
	}
	for i := range handedOpen.Members {
		if !reflect.DeepEqual(onDemand.Members[i], handedOpen.Members[i]) {
			t.Errorf("member %d measures %+v on demand, %+v handed open", i, onDemand.Members[i], handedOpen.Members[i])
		}
	}
}

// TestAnalyzeGroupRefusesAnAmbiguousMember: a member names its source one
// way. Neither field is nothing to measure, both is two owners for one
// Close, and an Open that returns no media is a broken contract rather
// than a nil dereference deep in the decode.
func TestAnalyzeGroupRefusesAnAmbiguousMember(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2}, []float64{440}))
	open := func() (format.Media, error) { return format.Open(container.BytesSource(src), "wav", nil) }
	for _, tc := range []struct {
		name   string
		member waxflow.GroupMember
	}{
		{"neither", waxflow.GroupMember{}},
		{"both", waxflow.GroupMember{Media: openMember(t, src), Open: open}},
		{"open returns nothing", waxflow.GroupMember{Open: func() (format.Media, error) { return nil, nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.AnalyzeGroup(context.Background(),
				[]waxflow.GroupMember{{Media: openMember(t, src)}, tc.member}, waxflow.AnalyzeOptions{})
			if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Fatalf("err = %v, want an invalid-request refusal", err)
			}
			if !strings.Contains(err.Error(), "member 1") {
				t.Errorf("err = %v, want the member's index", err)
			}
		})
	}
}

// TestAnalyzeGroupAnnotatesAFailedOpen: an Open that fails names the member
// and keeps its code, the way a member that fails mid-read does, and the
// run stops there rather than opening the members after it.
func TestAnalyzeGroupAnnotatesAFailedOpen(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2}, []float64{440}))
	var l lazyLedger
	_, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		l.member(t, 0, src, 0),
		{Open: func() (format.Media, error) {
			return nil, waxerr.New(waxerr.CodeSourceUnreadable, "disk gone")
		}},
		l.member(t, 2, src, 0),
	}, waxflow.AnalyzeOptions{})
	if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("err = %v, want the open's own code", err)
	}
	if !strings.Contains(err.Error(), "group member 1") || !strings.Contains(err.Error(), "disk gone") {
		t.Errorf("err = %v, want the member index and the cause", err)
	}
	if !slices.Equal(l.opened, []int{0}) {
		t.Errorf("members opened: %v, want only the one before the failure", l.opened)
	}
	if l.open != 0 {
		t.Errorf("%d members still open after the failure", l.open)
	}
}

// TestAnalyzeGroupCarriesMemberWarnings: a member opened on demand is closed
// by the time the call returns, so the damage its read found rides its own
// result. A frame-walked payload finds its damage where the read reaches
// it, which is why the list is the read's and not the probe's.
func TestAnalyzeGroupCarriesMemberWarnings(t *testing.T) {
	wav, err := os.ReadFile(repoPath("testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	mp3, err := os.ReadFile(repoPath("testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := testutil.ZeroMiddle(mp3, 2048)
	e := waxflow.New()
	got, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		{Open: func() (format.Media, error) { return e.OpenStream(container.BytesSource(wav), "wav") }},
		{Open: func() (format.Media, error) { return e.OpenStream(container.BytesSource(damaged), "mp3") }},
	}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ws := got.Members[0].InputWarnings; ws != nil {
		t.Errorf("the clean member reports %v, want nil", ws)
	}
	if ws := got.Members[1].InputWarnings; !slices.ContainsFunc(ws, func(s string) bool {
		return strings.Contains(s, "unparsable bytes skipped")
	}) {
		t.Errorf("the damaged member reports %v, want the skipped bytes", ws)
	}
	if ws := got.Group.InputWarnings; ws != nil {
		t.Errorf("the group reports %v, want nil: each member's damage is on its own result", ws)
	}
}

// TestAnalyzeGroupProgressWithOnDemandMembers: a member opened on demand
// declares its length when the run reaches it, so the total is unknown
// until the last member is open and the members' sum from then on.
func TestAnalyzeGroupProgressWithOnDemandMembers(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2}, []float64{440}))
	var l lazyLedger
	var last, total int64
	var back bool
	var totals [2][]int64 // the totals reported while each member was being measured
	_, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		l.member(t, 0, src, 0), l.member(t, 1, src, 0),
	}, waxflow.AnalyzeOptions{Progress: func(done, t int64) {
		if done < last {
			back = true
		}
		last, total = done, t
		member := len(l.opened) - 1 // the member open right now
		totals[member] = append(totals[member], t)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if back {
		t.Error("progress went backwards between members")
	}
	if len(totals[0]) == 0 || len(totals[1]) == 0 {
		t.Fatalf("progress calls per member: %d and %d, want both measured", len(totals[0]), len(totals[1]))
	}
	if slices.ContainsFunc(totals[0], func(n int64) bool { return n != -1 }) {
		t.Errorf("totals while the first member was measured: %v, want -1 throughout (the second is not open yet)", totals[0])
	}
	if want := int64(2 * analyzeChFrames); slices.ContainsFunc(totals[1], func(n int64) bool { return n != want }) {
		t.Errorf("totals while the last member was measured: %v, want the members' sum %d", totals[1], want)
	}
	if last != total {
		t.Errorf("progress finished at %d of %d", last, total)
	}
}

// TestAnalyzeGroupRefusesANegativeWidthUpfront: a member's width is checked
// with the rest of the group before anything is opened, not when the run
// reaches the member after decoding every one before it.
func TestAnalyzeGroupRefusesANegativeWidthUpfront(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2}, []float64{440}))
	var l lazyLedger
	_, err := e.AnalyzeGroup(context.Background(), []waxflow.GroupMember{
		l.member(t, 0, src, 0), l.member(t, 1, src, -1),
	}, waxflow.AnalyzeOptions{})
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !strings.Contains(err.Error(), "member 1") {
		t.Fatalf("err = %v, want an invalid-request refusal naming member 1", err)
	}
	if len(l.opened) != 0 {
		t.Errorf("members opened before the refusal: %v, want none", l.opened)
	}
}
