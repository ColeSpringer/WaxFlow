package waxflow_test

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// pastAnyEnd is a seek target past the end of any stream.
const pastAnyEnd = int64(1) << 61

// measureRoutes is one fixture per route MeasureLength takes.
var measureRoutes = []struct {
	route, path, hint string
}{
	{"exact", filepath.Join("..", "testdata", "sine-s16.flac"), "flac"},
	{"walk", filepath.Join("..", "testdata", "sine-cbr128.mp3"), "mp3"},
	{"seek", filepath.Join("..", "container", "mp4", "testdata", "alac-stereo.m4a"), "m4a"},
	{"decode", filepath.Join("..", "testdata", "sine-s16.wma"), "wma"},
}

// opener returns a function opening a fresh Media over the file at path.
func opener(t *testing.T, path, hint string) func() format.Media {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return func() format.Media {
		t.Helper()
		med, err := format.Open(container.BytesSource(raw), hint, nil)
		if err != nil {
			t.Fatal(err)
		}
		return med
	}
}

// TestMeasureRouteFixturesTakeTheirRoute: each measureRoutes fixture still
// takes the route its row names, or the cells built on the table test some
// other route without saying so.
func TestMeasureRouteFixturesTakeTheirRoute(t *testing.T) {
	for _, r := range measureRoutes {
		t.Run(r.route, func(t *testing.T) {
			med := opener(t, r.path, r.hint)()
			defer med.Close()
			exact := func() bool { tr := med.Info().Default(); return tr.SamplesExact && tr.Samples >= 0 }
			if got := exact(); got != (r.route == "exact") {
				t.Fatalf("exact at open = %v", got)
			}
			if r.route == "exact" {
				return
			}
			if err := format.WalkMedia(med); err != nil {
				t.Fatal(err)
			}
			if got := exact(); got != (r.route == "walk") {
				t.Fatalf("exact after the walk = %v", got)
			}
			if r.route == "walk" {
				return
			}
			if got := med.Info().Default().SamplesAdvisory; got != (r.route == "decode") {
				t.Fatalf("advisory after the walk = %v", got)
			}
		})
	}
}

// TestMeasureLengthAnswersFromAnywhere: the answer does not depend on where the
// media is, so a caller that read part of it first still gets the whole length.
func TestMeasureLengthAnswersFromAnywhere(t *testing.T) {
	for _, r := range measureRoutes {
		t.Run(r.route, func(t *testing.T) {
			open := opener(t, r.path, r.hint)
			fresh := open()
			want, err := waxflow.MeasureLength(fresh)
			fresh.Close()
			if err != nil {
				t.Fatal(err)
			}
			med := open()
			defer med.Close()
			buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
			defer audio.Put(buf)
			if err := med.ReadChunk(buf); err != nil {
				t.Fatal(err)
			}
			got, err := waxflow.MeasureLength(med)
			if err != nil {
				t.Fatal(err)
			}
			if got.Samples != want.Samples {
				t.Errorf("measured %d after a read, %d from the start", got.Samples, want.Samples)
			}
		})
	}
}

// TestMeasureLengthTakesTheCheapestRoute pins the walk route, which differs
// from a decode only in what it costs.
//
// The MP3 cell is also the correctness half. Its Xing count promises audio the
// truncated file does not hold, and the measure has to come back with the
// audio rather than the promise.
func TestMeasureLengthTakesTheCheapestRoute(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	cut := raw[:len(raw)-100]
	// The walk's own answer, which a tolerant consumer is the only one to get:
	// the file is damaged, so a strict probe of it refuses outright.
	tol, err := format.Open(container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tol.Close()
	if err := tol.(format.Walker).Walk(); err != nil {
		t.Fatal(err)
	}
	want := tol.Info().Default().Samples
	if want >= 22050 {
		t.Fatalf("the walk settled at %d; this cell needs a count short of the declared 22050", want)
	}

	med, err := format.Open(container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got, err := waxflow.MeasureLength(med)
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != want {
		t.Errorf("measured %d, the walk settles at %d", got.Samples, want)
	}

	// A Matroska Opus track takes the walk route: no open pays the cluster
	// walk, so the estimate the headers state has to be replaced by reading,
	// and the walk is the cheapest reading there is (no decode, no bisection).
	webm, err := os.ReadFile(filepath.Join("..", "container", "mka", "testdata", "seed-opus.webm"))
	if err != nil {
		t.Fatal(err)
	}
	cs := &testutil.CountingSource{Src: container.BytesSource(webm)}
	wm, err := format.Open(cs, "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wm.Close()
	opened := wm.Info().Default()
	if !opened.SamplesAdvisory {
		t.Fatalf("this cell needs an open that estimates; got %d (exact %v)", opened.Samples, opened.SamplesExact)
	}
	cs.Reset()
	n, err := waxflow.MeasureLength(wm)
	if err != nil {
		t.Fatal(err)
	}
	strict, err := format.Probe(container.BytesSource(webm), "webm", &format.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := strict.Default().Samples; n.Samples != want {
		t.Errorf("measured %d, a strict probe walks to %d", n.Samples, want)
	}
	if cs.Reads == 0 {
		t.Error("the measure read nothing; the walk it is supposed to take reads the clusters")
	}
}

// TestMeasureLengthTakesAnExactLengthWithoutReading: a length the open
// verified is the answer, and taking it costs nothing.
func TestMeasureLengthTakesAnExactLengthWithoutReading(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.flac"))
	if err != nil {
		t.Fatal(err)
	}
	cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
	med, err := format.Open(cs, "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	cs.Reset()
	got, err := waxflow.MeasureLength(med)
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 15435 || !got.SamplesExact {
		t.Errorf("measured %d (exact %v), want the verified 15435", got.Samples, got.SamplesExact)
	}
	if cs.Reads != 0 {
		t.Errorf("the measure read %d bytes in %d reads after the open, want none", cs.Bytes, cs.Reads)
	}
}

// TestMeasureAgreesWithTheDecodeOnARoundedContainer is the decode route, and
// the reason it exists. ASF names its positions in milliseconds, so a seek past
// the end answers where the landing claims to be plus the frames decoded from
// there, and the rounding up to that landing lands in the total: sine-s16.wma
// comes back 33 samples under a read of the same file.
//
// Those 33 are the kind of wrong this measure exists to prevent. A timeline's
// prefix sum carries them into every member after it, a bounded slice drops
// them, and an HLS playlist promises a tail it will not serve. So a container
// whose own positions are rounded is decoded instead.
func TestMeasureAgreesWithTheDecodeOnARoundedContainer(t *testing.T) {
	open := opener(t, filepath.Join("..", "testdata", "sine-s16.wma"), "wma")

	med := open()
	defer med.Close()
	if tr := med.Info().Default(); !tr.SamplesAdvisory {
		t.Fatalf("the fixture's length is not advisory (%+v); this cell needs a rounded container", tr)
	}
	got, err := waxflow.MeasureLength(med)
	if err != nil {
		t.Fatal(err)
	}

	// The oracle is a read of the same file, which is what every consumer of
	// this number will get.
	lin := open()
	defer lin.Close()
	want := int64(drainMedia(t, lin, 1<<20).N)
	if got.Samples != want {
		t.Errorf("measured %d, a read of the same file delivers %d", got.Samples, want)
	}

	// And the shortcut it declines is still wrong, so the route is doing work
	// rather than arriving at the same place by luck.
	sk := open()
	defer sk.Close()
	if landed, err := sk.SeekSample(pastAnyEnd); err == nil && landed == want {
		t.Errorf("a seek past the end answered %d too; this cell no longer pins anything", landed)
	}
}

// TestMeasureLengthWalksAFragmentedMP4 pins the route a fragmented movie
// takes: the walk reads one moof header per fragment, not every payload.
//
// Zero decoder work is the sharp half: the walk reads moof headers and skips
// every mdat by its own, so the sample data is never touched.
func TestMeasureLengthWalksAFragmentedMP4(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "container", "mp4", "testdata", "ima4-frag.mov"))
	if err != nil {
		t.Fatal(err)
	}
	cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
	med, err := format.Open(cs, "mov", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if format.MediaWalked(med) {
		t.Fatal("this cell needs a source with a deferred walk")
	}
	cs.Reset()
	got, err := waxflow.MeasureLength(med)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SamplesExact || got.Samples <= 0 {
		t.Fatalf("measured %d (exact %v), want a positive exact count", got.Samples, got.SamplesExact)
	}
	if cs.Bytes >= int64(len(raw)) {
		t.Errorf("the measure read %d bytes of a %d-byte file; a moof walk reads headers, not payloads",
			cs.Bytes, len(raw))
	}

	// The oracle: a walked read of the same file delivers the same count. It
	// has to be walked, because an un-walked read of a fragmented movie
	// delivers whole decoded blocks past the trun sum; see
	// TestFragmentedLengthWalkedAndNot, which pins both numbers.
	lin, err := format.Open(container.BytesSource(raw), "mov", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lin.Close()
	if err := format.WalkMedia(lin); err != nil {
		t.Fatal(err)
	}
	if n := int64(drainMedia(t, lin, 1<<20).N); n != got.Samples {
		t.Errorf("measured %d, a walked read delivers %d", got.Samples, n)
	}
}

// TestMeasureLengthLeavesTheMediaAtItsStart: whichever route ran, the media
// then reads exactly what a fresh open does. The seek and decode routes go to
// the end to measure and have to come back.
func TestMeasureLengthLeavesTheMediaAtItsStart(t *testing.T) {
	for _, r := range measureRoutes {
		t.Run(r.route, func(t *testing.T) {
			open := opener(t, r.path, r.hint)
			fresh := open()
			want := drainMedia(t, fresh, 1<<20)
			fresh.Close()
			defer audio.Put(want)

			med := open()
			defer med.Close()
			if _, err := waxflow.MeasureLength(med); err != nil {
				t.Fatal(err)
			}
			got := drainMedia(t, med, 1<<20)
			defer audio.Put(got)
			equalPCM(t, want, got)
		})
	}
}

// TestSliceMeasuredOpensAWindowInTheAdvisoryGap: sine-s16.wma declares 22491
// samples and decodes 22528, so a window ending between the two is one the
// file can serve. A bare Slice bounds it by the declaration and refuses it.
// The window starts at 0 because a WMA seek into this file's tail lands late
// (docs/deferred-work.md).
func TestSliceMeasuredOpensAWindowInTheAdvisoryGap(t *testing.T) {
	const declared, measured, from, to = 22491, 22528, 0, 22510
	open := opener(t, filepath.Join("..", "testdata", "sine-s16.wma"), "wma")
	fresh := open()
	got := fresh.Info().Default().Samples
	whole := drainMedia(t, fresh, 1<<20)
	fresh.Close()
	defer audio.Put(whole)
	if got != declared {
		t.Fatalf("the fixture declares %d samples, want %d", got, declared)
	}
	if whole.N != measured {
		t.Fatalf("the fixture decodes %d samples, want %d", whole.N, measured)
	}

	bare := open()
	if sl, err := waxflow.Slice(bare, from, to); err == nil {
		sl.Close()
		t.Fatal("a bare Slice opened a window past the declaration; the gap this pins is gone")
	}
	bare.Close()

	med := open()
	track, err := waxflow.MeasureLength(med)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	sl, err := waxflow.SliceMeasured(med, track.Samples, from, to)
	if err != nil {
		t.Fatalf("SliceMeasured refused a window the file can serve: %v", err)
	}
	defer sl.Close()
	window := drainMedia(t, sl, 1<<20)
	defer audio.Put(window)
	want := audio.Get(whole.Fmt, to-from)
	defer audio.Put(want)
	audio.CopyFrames(want, 0, whole, from, to-from)
	want.N = to - from
	equalPCM(t, want, window)
}

// TestSliceAfterMeasureMatchesAFreshSlice: a media measured and then sliced
// delivers what a fresh one sliced at the same total does, on every route,
// from the start or from inside.
func TestSliceAfterMeasureMatchesAFreshSlice(t *testing.T) {
	for _, r := range measureRoutes {
		t.Run(r.route, func(t *testing.T) {
			open := opener(t, r.path, r.hint)
			first := open()
			track, err := waxflow.MeasureLength(first)
			first.Close()
			if err != nil {
				t.Fatal(err)
			}
			n := track.Samples
			for _, w := range [][2]int64{{0, waxflow.ToEnd}, {n / 3, 2 * n / 3}} {
				ref, err := waxflow.Slice(waxflow.MeasuredMedia(open(), n), w[0], w[1])
				if err != nil {
					t.Fatal(err)
				}
				want := drainMedia(t, ref, 1<<20)
				ref.Close()
				med := open()
				if _, err := waxflow.MeasureLength(med); err != nil {
					t.Fatal(err)
				}
				sl, err := waxflow.SliceMeasured(med, n, w[0], w[1])
				if err != nil {
					t.Fatal(err)
				}
				got := drainMedia(t, sl, 1<<20)
				sl.Close()
				equalPCM(t, want, got)
				audio.Put(want)
				audio.Put(got)
			}
		})
	}
}

// stubMedia is a Media whose declared total and true length can disagree,
// which no honest in-tree fixture does at a usable size. Only Info and Close
// carry weight: Slice derives its bound from the declaration at open.
type stubMedia struct {
	info   *format.Info
	closed bool
}

func (m *stubMedia) Info() *format.Info                { return m.info }
func (m *stubMedia) ReadChunk(*audio.Buffer) error     { return io.EOF }
func (m *stubMedia) SeekSample(t int64) (int64, error) { return t, nil }
func (m *stubMedia) Close() error                      { m.closed = true; return nil }

func advisoryMedia(declared int64) *stubMedia {
	return &stubMedia{info: &format.Info{
		Container: "test",
		Tracks: []container.Track{{
			Fmt: audio.Format{Rate: 48000, Channels: 2,
				Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16},
			Samples: declared,
			Default: true,
		}},
	}}
}

// TestSliceMeasuredReanchorsAdvisoryDeclarations: a window validated against
// the measured total opens against the same number, not the shorter advisory
// declaration a fresh open reports. Without it a window edge in that gap would
// refuse at every open: a timeline the mint accepted would fail every segment.
func TestSliceMeasuredReanchorsAdvisoryDeclarations(t *testing.T) {
	const declared, measured = 99000, 100000

	// The disease first: a bare Slice can only see the declaration.
	if _, err := waxflow.Slice(advisoryMedia(declared), 98000, 99500); err == nil {
		t.Fatal("a bare Slice accepted a window past the advisory declaration; " +
			"Slice is pure by design, and the re-anchor is MeasuredMedia over a measured total")
	}

	for _, tc := range []struct {
		name        string
		declared, n int64
		from, to    int64
		want        int64 // the slice's declared length; -1: refused
	}{
		{"a bounded window edge inside the gap", declared, measured, 98000, 99500, 1500},
		// measured - from, not declared - from: the open end inherits the
		// measure, which is what a timeline holds delivery to.
		{"an open end starting inside the gap", declared, measured, 99200, waxflow.ToEnd, 800},
		{"a window past the measure", declared, measured, 0, measured + 1, -1},
		{"a matching declaration", measured, measured, 98000, 99500, 1500},
		{"an unknown measure leaves the declaration", declared, -1, 1000, waxflow.ToEnd, declared - 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			med := advisoryMedia(tc.declared)
			sl, err := waxflow.SliceMeasured(med, tc.n, tc.from, tc.to)
			if tc.want < 0 {
				if err == nil || !strings.Contains(err.Error(), "past the source's") {
					t.Fatalf("a window past the measure was not refused: %v", err)
				}
				if !med.closed {
					t.Fatal("a refused window left the media open")
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			defer sl.Close()
			if got := sl.Info().Default().Samples; got != tc.want {
				t.Fatalf("the slice declares %d samples, want %d", got, tc.want)
			}
		})
	}
}

// TestSliceMeasuredForwardsLiveWarnings: Warnings and Notes are live on the
// inner media (see format.Media), and the re-anchored copy must not freeze
// them. A source damaged past its head reports the skipped bytes once the
// read has reached them, through the slice as through the bare media.
func TestSliceMeasuredForwardsLiveWarnings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := testutil.ZeroMiddle(raw, 2048)
	e := waxflow.New()
	bare, err := e.OpenStream(container.BytesSource(damaged), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	measured := int64(drainMedia(t, bare, 1<<20).N)
	bare.Close()

	med, err := e.OpenStream(container.BytesSource(damaged), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.SliceMeasured(med, measured, 0, waxflow.ToEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	if got := sl.Info().Default().Samples; got != measured {
		t.Fatalf("the slice declares %d samples, want the measured %d", got, measured)
	}
	if w := sl.Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings at open = %v, want none: the head is clean", w)
	}
	drainMedia(t, sl, 1<<20)
	if !slices.ContainsFunc(sl.Info().Warnings, func(s string) bool { return strings.Contains(s, "unparsable bytes skipped") }) {
		t.Errorf("warnings after the read = %v, want the skipped bytes forwarded", sl.Info().Warnings)
	}
}

// TestSliceMeasuredClosesWhatItRefuses: SliceMeasured owns med, so a refused
// window closes it.
func TestSliceMeasuredClosesWhatItRefuses(t *testing.T) {
	closed := false
	spy := &closeCountingMedia{
		Media:   opener(t, filepath.Join("..", "testdata", "sine-s16.flac"), "flac")(),
		onClose: func() { closed = true },
	}
	if _, err := waxflow.SliceMeasured(spy, -1, 0, 15436); err == nil {
		t.Error("a window one sample past the length opened")
	}
	if !closed {
		t.Error("a refused window left the media open")
	}
}

// failingWalk is a Media of unknown length whose walk fails.
type failingWalk struct{ format.Media }

func (f *failingWalk) Info() *format.Info {
	info := *f.Media.Info()
	info.Tracks = []container.Track{info.Default()}
	info.Tracks[0].Samples, info.Tracks[0].SamplesExact = -1, false
	return &info
}
func (f *failingWalk) Walk() error  { return waxerr.New(waxerr.CodeMalformedInput, "no frame sync") }
func (f *failingWalk) Walked() bool { return false }

// TestMeasureLengthKeepsTheRouteErrorCode: a failed walk is malformed input,
// and the measure says so rather than re-coding it.
func TestMeasureLengthKeepsTheRouteErrorCode(t *testing.T) {
	fw := &failingWalk{Media: opener(t, filepath.Join("..", "testdata", "sine-s16.flac"), "flac")()}
	defer fw.Close()
	if _, err := waxflow.MeasureLength(fw); waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Errorf("a failed walk returned %v, want its malformed-input", err)
	}
}

// TestMeasureRefusesANilMedia: a nil Media is the caller's mistake, refused as
// one rather than a panic, the way Slice refuses it.
func TestMeasureRefusesANilMedia(t *testing.T) {
	if _, err := waxflow.MeasureLength(nil); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("MeasureLength(nil) = %v, want invalid-request", err)
	}
	if _, err := waxflow.SliceMeasured(nil, -1, 0, waxflow.ToEnd); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("SliceMeasured(nil) = %v, want invalid-request", err)
	}
}
