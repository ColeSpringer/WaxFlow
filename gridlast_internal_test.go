package waxflow

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// knownGrid is a grid already measured, for a planner that asks for one.
func knownGrid(g int) func() (int, error) {
	return func() (int, error) { return g, nil }
}

// countedGrid answers g, or err when set, and counts the asks.
type countedGrid struct {
	g     int
	err   error
	asked int
}

func (c *countedGrid) get() (int, error) {
	c.asked++
	return c.g, c.err
}

// gridLastOpus is an Opus track every copy planner serves on a 960 grid.
func gridLastOpus() container.Track {
	return container.Track{
		Codec: codec.Opus, CodecConfig: testOpusHead(312),
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: 480000, Delay: 312, Default: true, SamplesExact: true,
	}
}

// gridLastHEAAC is an HE-AAC track (explicit SBR, 24 kHz core) every copy
// planner serves at format=aac on a 2048 grid, from the stream head.
func gridLastHEAAC() container.Track {
	return container.Track{
		Codec: codec.HEAAC, CodecConfig: []byte{0x2B, 0x11, 0x88, 0x00},
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: 480000, Delay: 2048, Default: true, SamplesExact: true,
	}
}

// gridLastAAC is an AAC-LC track (44.1 kHz stereo) on a 1024 grid whose
// container declares no delay.
func gridLastAAC() container.Track {
	return container.Track{
		Codec: codec.AACLC, CodecConfig: []byte{0x12, 0x10},
		Fmt:     audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: 441000, Default: true, SamplesExact: true,
	}
}

// family1Head is a stereo OpusHead in channel mapping family 1, one coupled
// stream in the identity order: Ogg carries it, an fMP4 sample entry cannot.
func family1Head(preSkip int) []byte {
	h := append(testOpusHead(preSkip), 1, 1, 0, 1)
	h[18] = 1
	return h
}

// gridLastResult is a planner's answer: whether it planned the copy, the
// grid the plan carries, and the source track a cut plan carries.
type gridLastResult struct {
	served bool
	grid   int
	source *container.Track
}

// gridLastPlanner is one copy planner over a track, options and spans, with
// the grid it is handed.
type gridLastPlanner func(track container.Track, opts TranscodeOptions, spans []Span, grid func() (int, error)) (gridLastResult, error)

// checkGridLast runs the shared half of the grid-last tests on a source whose
// grid is g: the grid is asked once for a request the planner serves and the
// plan carries it and the track, its error comes back as it is, and a grid of
// 0 declines.
func checkGridLast(t *testing.T, plan gridLastPlanner, track container.Track, opts TranscodeOptions, spans []Span, g int) {
	t.Helper()
	cg := &countedGrid{g: g}
	res, err := plan(track, opts, spans, cg.get)
	switch {
	case err != nil || !res.served || cg.asked != 1:
		t.Errorf("a request the copy serves: served %v, err %v, grid asked %d times; want served, asked once", res.served, err, cg.asked)
	case res.grid != g:
		t.Errorf("the plan carries grid %d, want %d", res.grid, g)
	case res.source != nil && !reflect.DeepEqual(*res.source, track):
		t.Errorf("the plan carries source %+v, want %+v", *res.source, track)
	}
	walkErr := errors.New("the walk failed")
	cg = &countedGrid{err: walkErr}
	if res, err := plan(track, opts, spans, cg.get); err != walkErr || res.served {
		t.Errorf("a grid that fails: served %v, err %v; want the grid's error unchanged", res.served, err)
	}
	cg = &countedGrid{}
	if res, err := plan(track, opts, spans, cg.get); err != nil || res.served || cg.asked != 1 {
		t.Errorf("a grid of 0: served %v, err %v, asked %d; want a decline after one ask", res.served, err, cg.asked)
	}
}

// neverAsked is a grid that fails the test when asked.
func neverAsked(t *testing.T, what string) func() (int, error) {
	return func() (int, error) {
		t.Errorf("%s: the grid was asked for a request its headers decline", what)
		return 960, nil
	}
}

// TestPlanRemuxSegmentsAsksForTheGridLast: the segmented remux measures the
// packet grid only once every header question has passed.
func TestPlanRemuxSegmentsAsksForTheGridLast(t *testing.T) {
	e := New()
	opus := gridLastOpus()
	flac20 := container.Track{
		Codec:   codec.FLAC,
		Fmt:     audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 20},
		Samples: 480000, Default: true,
	}
	trimmed := opus
	trimmed.MidPadding = 960
	trimmed.MidTrims = []container.PacketTrim{{Pos: 96000, Samples: 960}}
	// An OpusHead whose pre-skip disagrees with the track's delay: PlanRemux
	// copies it, and the init segment cannot declare it.
	unheaded := opus
	unheaded.Delay = 0
	for _, tc := range []struct {
		name  string
		track container.Track
		opts  TranscodeOptions
	}{
		{"a codec mismatch", opus, TranscodeOptions{Format: "aac"}},
		{"a bitrate", opus, TranscodeOptions{Format: "opus", Bitrate: 64000}},
		{"a 20-bit FLAC to flac", flac20, TranscodeOptions{Format: "flac"}},
		{"a trim inside the run", trimmed, TranscodeOptions{Format: "opus"}},
		{"a track the init header cannot express", unheaded, TranscodeOptions{Format: "opus"}},
	} {
		if p, err := e.PlanRemuxSegments(tc.track, tc.opts, 2, neverAsked(t, tc.name)); p != nil || err != nil {
			t.Errorf("%s: %v, %v; want a decline", tc.name, p, err)
		}
	}
	plan := func(track container.Track, opts TranscodeOptions, _ []Span, grid func() (int, error)) (gridLastResult, error) {
		p, err := e.PlanRemuxSegments(track, opts, 2, grid)
		if p == nil {
			return gridLastResult{}, err
		}
		return gridLastResult{served: true, grid: p.FrameSize}, err
	}
	checkGridLast(t, plan, opus, TranscodeOptions{Format: "opus"}, nil, 960)
	for _, seconds := range []float64{math.NaN(), 120} {
		what := fmt.Sprintf("a %g s segment", seconds)
		if p, err := e.PlanRemuxSegments(opus, TranscodeOptions{Format: "opus"}, seconds, neverAsked(t, what)); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: %v, %v; want an invalid-request refusal", what, p, err)
		}
	}
	// format=aac reaches an HE-AAC source through the he-aac row.
	checkGridLast(t, plan, gridLastHEAAC(), TranscodeOptions{Format: "aac"}, nil, 2048)
}

// gridLastDeclines are the requests a cut planner declines before the grid:
// an Opus track to other, a format of another codec, and each kind of
// transform.
func gridLastDeclines(other string) []struct {
	name string
	opts TranscodeOptions
} {
	return []struct {
		name string
		opts TranscodeOptions
	}{
		{"an Opus track to " + other, TranscodeOptions{Format: other}},
		{"a bitrate", TranscodeOptions{Format: "opus", Bitrate: 64000}},
		{"a gain", TranscodeOptions{Format: "opus", GainDB: -6}},
		{"a seek", TranscodeOptions{Format: "opus", FromSample: 4800}},
	}
}

// cutRow is a request a cut planner serves on one ask of a grid of g, or,
// when g is 0, declines without asking it.
type cutRow struct {
	name  string
	track container.Track
	opts  TranscodeOptions
	spans []Span
	g     int
}

// checkCutRows asks plan each row.
func checkCutRows(t *testing.T, plan gridLastPlanner, rows []cutRow) {
	t.Helper()
	for _, r := range rows {
		if r.g == 0 {
			if res, err := plan(r.track, r.opts, r.spans, neverAsked(t, r.name)); res.served || err != nil {
				t.Errorf("%s: served %v, err %v; want a decline", r.name, res.served, err)
			}
			continue
		}
		cg := &countedGrid{g: r.g}
		if res, err := plan(r.track, r.opts, r.spans, cg.get); !res.served || err != nil || cg.asked != 1 {
			t.Errorf("%s: served %v, err %v, grid asked %d times; want served on one ask", r.name, res.served, err, cg.asked)
		}
	}
}

// withTrims is track with trims inside its run, listed whole.
func withTrims(track container.Track, trims ...container.PacketTrim) container.Track {
	track.MidTrims, track.MidPadding = trims, 0
	for _, tr := range trims {
		track.MidPadding += tr.Samples
	}
	return track
}

// cutTrimRows are the trims every cut planner answers alike: each that rules
// out a cut whatever the grid, beside a near miss the cut serves.
func cutTrimRows() []cutRow {
	opus := gridLastOpus()
	spans := []Span{{From: 96000, To: 288000}}
	two := []Span{{From: 96000, To: 192000}, {From: 240000, To: 288000}}
	short, inside, past := opus, opus, opus
	short.MidPadding = 960
	inside.MidPadding, inside.MidTrims = 100, []container.PacketTrim{{Pos: 191900, Samples: 100}}
	past.MidPadding, past.MidTrims = 100, []container.PacketTrim{{Pos: 383900, Samples: 100}}
	// The first span's pre-roll point is raw 96000+312-3840 plus the trim
	// before it: 92972 for a 500-sample trim, 93120 for a 648-sample one.
	across := withTrims(opus, container.PacketTrim{Pos: 92620, Samples: 500})
	atPoint := withTrims(opus, container.PacketTrim{Pos: 92472, Samples: 648})
	// Past raw 480412, where the declared run ends.
	beyond := withTrims(opus, container.PacketTrim{Pos: 480860, Samples: 100})
	// Ends 150 and 1077 share no divisor as large as 77.
	noGrid := withTrims(opus, container.PacketTrim{Pos: 100, Samples: 50}, container.PacketTrim{Pos: 1000, Samples: 77})
	// Ends 192000 and 288960 have a gcd of 960, and no other divisor of it
	// holds a 481-sample trim.
	oneGrid := withTrims(opus, container.PacketTrim{Pos: 191519, Samples: 481}, container.PacketTrim{Pos: 288479, Samples: 481})
	splice := TranscodeOptions{Format: "opus", SpliceTrims: true}
	aac := gridLastAAC()
	padded := aac
	padded.Padding = 500
	primed := padded
	primed.Delay = 2112
	whole := []Span{{From: 0, To: ToEnd}}
	opusOpts, aacOpts := TranscodeOptions{Format: "opus"}, TranscodeOptions{Format: "aac"}
	return []cutRow{
		{"a trim list the walk left short", short, opusOpts, spans, 0},
		{"a source trim inside the first span", inside, opusOpts, spans, 0},
		{"a source trim past the span", past, opusOpts, spans, 960},
		{"a source trim beginning at the span's end", inside, opusOpts, []Span{{From: 96000, To: 191588}}, 960},
		{"a source trim across the first span's pre-roll point", across, opusOpts, spans, 0},
		{"a source trim ending at the first span's pre-roll point", atPoint, opusOpts, spans, 960},
		{"a source trim past the declared end", beyond, opusOpts, []Span{{From: 96000, To: ToEnd}}, 960},
		{"trims no grid places", noGrid, opusOpts, spans, 0},
		{"a span the one grid its trims fit declines", oneGrid, opusOpts, []Span{{From: 0, To: 96000}, {From: 96100, To: 96600}}, 0},
		{"a span the one grid its trims fit serves", oneGrid, opusOpts, []Span{{From: 0, To: 96000}, {From: 96100, To: 98000}}, 960},
		{"SpliceTrims across two spans", opus, splice, two, 0},
		{"SpliceTrims on one span", opus, splice, spans, 960},
		{"two spans", opus, opusOpts, two, 960},
		{"an end trim kept alone", padded, aacOpts, whole, 0},
		{"a whole unprimed source", aac, aacOpts, whole, 1024},
		{"a padded unprimed source to a packet edge", padded, aacOpts, []Span{{From: 0, To: 10240}}, 1024},
		{"a padded unprimed source from past sample 0 to its end", padded, aacOpts, []Span{{From: 10240, To: ToEnd}}, 1024},
		{"a padded primed source whole", primed, aacOpts, whole, 1024},
	}
}

// checkCutGridLast is the cut planners' test over plan, other naming a
// format of another codec the planner has a form for, and rows the
// planner's own.
func checkCutGridLast(t *testing.T, plan gridLastPlanner, other string, rows []cutRow) {
	t.Helper()
	opus := gridLastOpus()
	spans := []Span{{From: 96000, To: 288000}}
	checkCutRows(t, plan, append(cutTrimRows(), rows...))
	for _, tc := range gridLastDeclines(other) {
		if res, err := plan(opus, tc.opts, spans, neverAsked(t, tc.name)); res.served || err != nil {
			t.Errorf("%s: served %v, err %v; want a decline", tc.name, res.served, err)
		}
	}
	for _, delay := range []int64{-1, 1 << 61} {
		what := fmt.Sprintf("a %d-sample delay", delay)
		track := opus
		track.Delay = delay
		if res, err := plan(track, TranscodeOptions{Format: "opus"}, spans, neverAsked(t, what)); res.served || err != nil {
			t.Errorf("%s: served %v, err %v; want a decline", what, res.served, err)
		}
	}
	he := gridLastHEAAC()
	if res, err := plan(he, TranscodeOptions{Format: "aac"}, spans, neverAsked(t, "an HE-AAC span off the head")); res.served || err != nil {
		t.Errorf("an HE-AAC span off the head: served %v, err %v; want a decline", res.served, err)
	}
	empty := []Span{{From: 100, To: 100}}
	_, _, want := CutTrack(opus, TranscodeOptions{Format: "opus"}, empty, 960)
	if _, err := plan(opus, TranscodeOptions{Format: other}, empty, neverAsked(t, "an empty span")); err == nil || want == nil || err.Error() != want.Error() {
		t.Errorf("an empty span: %v, want %v", err, want)
	}
	checkGridLast(t, plan, opus, TranscodeOptions{Format: "opus"}, spans, 960)
	checkGridLast(t, plan, he, TranscodeOptions{Format: "aac"}, []Span{{From: 0, To: 288000}}, 2048)
	// The plan on the one grid a source's trims fit errors for a head the
	// rewrite refuses, which is no decline: the grid is asked, and the error
	// comes back.
	bad := withTrims(opus, container.PacketTrim{Pos: 191519, Samples: 481}, container.PacketTrim{Pos: 288479, Samples: 481})
	bad.CodecConfig = []byte("nope")
	cg := &countedGrid{g: 960}
	res, err := plan(bad, TranscodeOptions{Format: "opus"}, []Span{{From: 0, To: 96000}, {From: 96100, To: 98000}}, cg.get)
	if res.served || waxerr.CodeOf(err) != waxerr.CodeMalformedInput || cg.asked != 1 {
		t.Errorf("a head the rewrite refuses, on the one grid its trims fit: served %v, err %v, grid asked %d times; want CodeMalformedInput on one ask",
			res.served, err, cg.asked)
	}
}

// TestPlanCutAsksForTheGridLast: the progressive cut measures the packet grid
// only once the spans, the codec and the format and options allow a copy.
func TestPlanCutAsksForTheGridLast(t *testing.T) {
	e := New()
	aac, primed, pce, unpadded := gridLastAAC(), gridLastAAC(), gridLastAAC(), gridLastAAC()
	primed.Delay = 1024
	pce.CodecConfig = []byte{0x12, 0x00} // channel configuration 0
	unpadded.Padding = -1
	adts := TranscodeOptions{Format: "aac", Container: "adts"}
	head, mid := []Span{{From: 0, To: 10240}}, []Span{{From: 10240, To: 20480}}
	dual := gridLastHEAAC()
	dual.Delay = 0
	single := dual
	single.CodecConfig = []byte{0x29, 0x91, 0x88, 0x00} // SBR at the 48 kHz core rate
	implicit := dual
	implicit.CodecConfig = []byte{0x13, 0x10} // AAC-LC at 24 kHz, the SBR in its payload
	implicit.Fmt.Rate = 24000
	opus, inside, fam1, first := gridLastOpus(), gridLastOpus(), gridLastOpus(), gridLastOpus()
	inside.MidPadding, inside.MidTrims = 100, []container.PacketTrim{{Pos: 191900, Samples: 100}}
	fam1.CodecConfig = family1Head(312)
	first.MidPadding, first.MidTrims = 960, []container.PacketTrim{{Pos: 0, Samples: 960}}
	noGrid := withTrims(opus, container.PacketTrim{Pos: 100, Samples: 50}, container.PacketTrim{Pos: 1000, Samples: 77})
	// Ends 192000 and 288960: 960 is the one grid whole-packet trims fit,
	// and half-packet ones fit 480 as well.
	wholes := withTrims(opus, container.PacketTrim{Pos: 191040, Samples: 960}, container.PacketTrim{Pos: 288000, Samples: 960})
	halves := withTrims(opus, container.PacketTrim{Pos: 191520, Samples: 480}, container.PacketTrim{Pos: 288480, Samples: 480})
	// A whole packet trimmed at raw 10240, where the spans join.
	joined := withTrims(aac, container.PacketTrim{Pos: 10240, Samples: 1024})
	spans := []Span{{From: 96000, To: 288000}}
	two := []Span{{From: 96000, To: 192000}, {From: 240000, To: 288000}}
	mka := TranscodeOptions{Format: "opus", Container: "mka"}
	spliceMKA := mka
	spliceMKA.SpliceTrims = true
	spliceAACMKA := TranscodeOptions{Format: "aac", Container: "mka", SpliceTrims: true}
	checkCutGridLast(t, func(track container.Track, opts TranscodeOptions, spans []Span, grid func() (int, error)) (gridLastResult, error) {
		p, err := e.PlanCut(track, opts, spans, grid)
		if p == nil {
			return gridLastResult{}, err
		}
		return gridLastResult{served: true, grid: p.Grid, source: &p.Source}, err
	}, "mp3", []cutRow{
		{"a primed source to adts", primed, adts, head, 0},
		{"a span past sample 0 to adts", aac, adts, mid, 0},
		{"an unprimed span from sample 0 to adts", aac, adts, head, 1024},
		{"a span past sample 0 to aac", aac, TranscodeOptions{Format: "aac"}, mid, 1024},
		{"a span past sample 0 to fragmented", aac, TranscodeOptions{Format: "aac", Container: ContainerFragmented}, mid, 1024},
		{"a PCE to fMP4", pce, TranscodeOptions{Format: "aac"}, mid, 0},
		{"a PCE to mka", pce, TranscodeOptions{Format: "aac", Container: "mka"}, mid, 1024},
		{"a negative padding kept to the end", unpadded, TranscodeOptions{Format: "aac"}, []Span{{From: 10240, To: ToEnd}}, 0},
		{"a negative padding cut off before the end", unpadded, TranscodeOptions{Format: "aac"}, mid, 1024},
		{"single-rate SBR to adts", single, adts, []Span{{From: 0, To: 20480}}, 0},
		{"dual-rate SBR to adts", dual, adts, []Span{{From: 0, To: 20480}}, 2048},
		{"implicit SBR to adts", implicit, adts, []Span{{From: 0, To: 20480}}, 2048},
		{"a source trim inside the first span to mka", inside, mka, spans, 960},
		{"SpliceTrims across two spans to mka", opus, spliceMKA, two, 960},
		{"SpliceTrims over spans that touch to mka", opus, spliceMKA, []Span{{From: 96000, To: 192000}, {From: 192000, To: 288000}}, 0},
		{"SpliceTrims over a later pair of spans that touch to mka", opus, spliceMKA,
			[]Span{{From: 0, To: 96000}, {From: 144000, To: 192000}, {From: 192000, To: 288000}}, 0},
		{"SpliceTrims over spans a sample apart to mka", aac, spliceAACMKA, []Span{{From: 0, To: 10240}, {From: 10241, To: ToEnd}}, 1024},
		{"SpliceTrims over spans a trim joins to mka", joined, spliceAACMKA, []Span{{From: 0, To: 10240}, {From: 10240, To: ToEnd}}, 1024},
		// Raw 3840 and 1024 are the Opus and AAC-LC pre-rolls.
		{"SpliceTrims to a head at the pre-roll to mka", opus, spliceMKA, []Span{{From: 0, To: 960}, {From: 3528, To: 200000}}, 0},
		{"SpliceTrims to a head a sample past the pre-roll to mka", opus, spliceMKA, []Span{{From: 0, To: 960}, {From: 3529, To: 200000}}, 1920},
		{"SpliceTrims to an AAC head at the pre-roll to mka", aac, spliceAACMKA, []Span{{From: 0, To: 500}, {From: 1024, To: 200000}}, 0},
		{"SpliceTrims to an AAC head a sample past the pre-roll to mka", aac, spliceAACMKA, []Span{{From: 0, To: 500}, {From: 1025, To: 200000}}, 1024},
		{"a family-1 OpusHead to Ogg", fam1, TranscodeOptions{Format: "opus"}, spans, 960},
		{"a trim filling the first packet", first, TranscodeOptions{Format: "opus"}, spans, 960},
		{"trims no grid places to mka", noGrid, mka, spans, 0},
		{"whole-packet trims one grid places", wholes, TranscodeOptions{Format: "opus"}, []Span{{From: 0, To: 96000}}, 960},
		{"half-packet trims two grids place", halves, TranscodeOptions{Format: "opus"},
			[]Span{{From: 0, To: 96000}, {From: 96100, To: 96600}}, 480},
	})
}

// TestPreGridRulesOnAPreRollAskForOne: the spliced-span and front-trim rules
// hold because a head backs off by the codec's pre-roll, so each guards on
// one, and every allowlisted codec has one.
func TestPreGridRulesOnAPreRollAskForOne(t *testing.T) {
	for id, cc := range cutCodecs {
		if cc.preroll <= 0 {
			t.Errorf("%v has no pre-roll, so the spliced-span and front-trim rules never decline its cuts before the grid", id)
		}
	}
	// A codec off the allowlist has none, as one added without would.
	none := container.Track{Codec: codec.FLAC, Samples: 480000, Delay: 312}
	splice := TranscodeOptions{SpliceTrims: true}
	touching := []Span{{From: 96000, To: 192000}, {From: 192000, To: 288000}}
	if reason := splicedOverlap(none, splice, touching); reason != "" {
		t.Errorf("spans that touch, with no pre-roll: ruled out as %q", reason)
	}
	for _, opts := range []TranscodeOptions{splice, {}} {
		if reason := cutTrimsRuledOut(none, opts, touching, "adts", false); reason != "" {
			t.Errorf("SpliceTrims %v, with no pre-roll: ruled out as %q", opts.SpliceTrims, reason)
		}
	}
}

// TestPlanCutSegmentsAsksForTheGridLast: the segmented cut asks the grid as
// the progressive one does, and its init header's questions before it.
func TestPlanCutSegmentsAsksForTheGridLast(t *testing.T) {
	e := New()
	fam1, skewed, asc, first, pce := gridLastOpus(), gridLastOpus(), gridLastAAC(), gridLastOpus(), gridLastAAC()
	pce.CodecConfig = []byte{0x12, 0x00} // channel configuration 0
	unpadded := gridLastAAC()
	unpadded.Padding = -1 // the init header states no end trim
	fam1.CodecConfig = family1Head(312)
	skewed.Delay = 0
	asc.Fmt.Rate = 48000
	first.MidPadding, first.MidTrims = 960, []container.PacketTrim{{Pos: 0, Samples: 960}}
	// Trims that empty their packets on the one grid they fit.
	wholes := withTrims(gridLastOpus(), container.PacketTrim{Pos: 191040, Samples: 960}, container.PacketTrim{Pos: 288000, Samples: 960})
	opus := TranscodeOptions{Format: "opus"}
	checkCutGridLast(t, func(track container.Track, opts TranscodeOptions, spans []Span, grid func() (int, error)) (gridLastResult, error) {
		p, err := e.PlanCutSegments(track, opts, spans, grid, 2)
		if p == nil {
			return gridLastResult{}, err
		}
		return gridLastResult{served: true, grid: p.Grid, source: &p.Source}, err
	}, "aac", []cutRow{
		{"a family-1 OpusHead", fam1, opus, []Span{{From: 96000, To: 288000}}, 0},
		{"an ASC that disagrees with the track's format", asc, TranscodeOptions{Format: "aac"}, []Span{{From: 10240, To: 20480}}, 0},
		{"an Opus delay that disagrees with its pre-skip", skewed, opus, []Span{{From: 96000, To: 288000}}, 960},
		{"a trim filling the first packet", first, opus, []Span{{From: 96000, To: 288000}}, 0},
		{"whole-packet trims one grid places", wholes, opus, []Span{{From: 0, To: 96000}}, 0},
		{"a PCE", pce, TranscodeOptions{Format: "aac"}, []Span{{From: 10240, To: 20480}}, 0},
		{"a negative padding kept to the end", unpadded, TranscodeOptions{Format: "aac"}, []Span{{From: 10240, To: ToEnd}}, 1024},
	})
	spans := []Span{{From: 96000, To: 288000}}
	for _, seconds := range []float64{math.NaN(), 120} {
		what := fmt.Sprintf("a %g s segment", seconds)
		if p, err := e.PlanCutSegments(gridLastOpus(), TranscodeOptions{Format: "opus"}, spans, neverAsked(t, what), seconds); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: %v, %v; want an invalid-request refusal", what, p, err)
		}
	}
}
