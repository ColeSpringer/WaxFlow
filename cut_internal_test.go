package waxflow

import (
	"io"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// opusHead builds a minimal OpusHead carrying preSkip, the config an Opus
// track's CodecConfig holds.
func opusHead(preSkip int, channels int) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1 // version
	head[9] = byte(channels)
	head[10] = byte(preSkip)
	head[11] = byte(preSkip >> 8)
	// Input sample rate, gain, and family all stay zero, which is a legal
	// mono/stereo head.
	return head
}

func opusTrack(preSkip int, samples int64) container.Track {
	return container.Track{
		Codec:       codec.Opus,
		CodecConfig: opusHead(preSkip, 2),
		Fmt:         audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples:     samples,
		Delay:       int64(preSkip),
	}
}

func aacTrack(delay, samples int64) container.Track {
	return container.Track{
		Codec: codec.AACLC,
		Fmt:   audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		// CodecConfig stays nil: nothing on the cut path parses an ASC, since
		// AAC's priming lives in the container's edit list rather than in the
		// config. PlanRemux's muxer-side checks are where a real ASC is needed,
		// and the tests that need one drive a real file.
		Samples: samples,
		Delay:   delay,
	}
}

// TestCutTrackKeepsThePrimingItWasGiven is the coverage for the head snap's
// back-off, and it is the whole reason that correction exists.
//
// opusenc writes a pre-skip of 3840, and 3840 is a whole multiple of the 960
// grid. A head snap that did not back off by the codec's pre-roll would land
// exactly on it, drop all four priming packets, and declare Delay 0: a cold
// decoder at output sample 0, with no error anywhere.
//
// No fixture in this tree can catch it. Our own encoder's pre-skip is 312, which
// is not a grid multiple, so it escapes the bug by luck.
func TestCutTrackKeepsThePrimingItWasGiven(t *testing.T) {
	track := opusTrack(3840, 48000)
	cut, landed, err := CutTrack(track, []Span{{0, ToEnd}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Delay != 3840 {
		t.Errorf("Delay = %d, want 3840: the source's priming was cut away", cut.Delay)
	}
	if cut.Samples != 48000 {
		t.Errorf("Samples = %d, want 48000", cut.Samples)
	}
	// The rewrite is what makes the Delay real: mka reads the pre-skip from the
	// config in preference to Track.Delay, so a Delay set without it does
	// nothing at all.
	cfg, err := opus.ParseOpusHead(cut.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreSkip != 3840 {
		t.Errorf("OpusHead pre-skip = %d, want 3840", cfg.PreSkip)
	}
	if want := (Span{0, 48000}); landed[0] != want {
		t.Errorf("Landed = %v, want %v", landed[0], want)
	}
	// The source's own config must not have been patched in place: a Track's
	// CodecConfig is shared with the demuxer that produced it.
	if orig, _ := opus.ParseOpusHead(track.CodecConfig); orig.PreSkip != 3840 {
		t.Errorf("source OpusHead pre-skip = %d, want it untouched at 3840", orig.PreSkip)
	}
}

// TestCutTrackBacksTheHeadOffAtAPositiveFrom pins the other half of the same
// correction: a From past 0 buys a converged decoder, which is what makes an
// exact head mean exact audio rather than an exact index.
func TestCutTrackBacksTheHeadOffAtAPositiveFrom(t *testing.T) {
	track := opusTrack(312, 96000)
	cut, landed, err := CutTrack(track, []Span{{48000, 72000}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	// df = 48000+312 = 48312; sd = snapDown(48312-3840) = snapDown(44472) =
	// 46*960 = 44160. So the walk starts 4152 samples early and trims them.
	if want := int64(48312 - 44160); cut.Delay != want {
		t.Errorf("Delay = %d, want %d", cut.Delay, want)
	}
	if cut.Delay < opus.SeekPreroll {
		t.Errorf("Delay = %d is under the %d-sample pre-roll: the decoder would not have converged",
			cut.Delay, opus.SeekPreroll)
	}
	// The head still lands exactly where it was asked for, because the slop is
	// expressed as the trim rather than delivered.
	if want := (Span{48000, 72000}); landed[0] != want {
		t.Errorf("Landed = %v, want %v", landed[0], want)
	}
	if cut.Samples != 24000 {
		t.Errorf("Samples = %d, want 24000", cut.Samples)
	}
}

// TestCutTrackSamplesAreLanded is the keystone. Samples must be the landed
// length rather than the requested one, and the proof is that remuxTrailer then
// yields exactly the padding the cut computed: the interior slop cancels.
//
// Under the requested-length version this fails by exactly the interior slop,
// and the failure is silent in production: mka would write a DiscardPadding that
// eats that much real audio off the end.
func TestCutTrackSamplesAreLanded(t *testing.T) {
	track := opusTrack(312, 96000)
	spans := []Span{{0, 20000}, {40000, 60000}}
	cut, landed, err := CutTrack(track, spans, 960)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, s := range landed {
		sum += s.To - s.From
	}
	if cut.Samples != sum {
		t.Errorf("Samples = %d, want %d (the sum of the landed spans)", cut.Samples, sum)
	}
	if cut.Samples <= (20000-0)+(60000-40000) {
		t.Errorf("Samples = %d is not longer than the %d requested; the interior slop is delivered audio and must count",
			cut.Samples, 40000)
	}
	// The trailer the muxer will actually be handed. decoded is what the walk
	// delivers: the sum of the kept windows.
	decoded := cut.Delay + cut.Samples + cut.Padding
	tr := remuxTrailer(cut, decoded)
	if tr.Padding != cut.Padding {
		t.Errorf("remuxTrailer padding = %d, want %d: the slop did not cancel", tr.Padding, cut.Padding)
	}
	if tr.Samples != cut.Samples {
		t.Errorf("remuxTrailer samples = %d, want %d", tr.Samples, cut.Samples)
	}
}

// TestCutTrackReportsWhereItLanded pins the head/tail-exact, interior-snapped
// split, which is the rung's whole promise about position.
func TestCutTrackReportsWhereItLanded(t *testing.T) {
	track := aacTrack(0, 96000)
	spans := []Span{{100, 20000}, {40000, 60000}}
	_, landed, err := CutTrack(track, spans, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if landed[0].From != 100 {
		t.Errorf("Landed[0].From = %d, want the requested 100 exactly", landed[0].From)
	}
	if landed[1].To != 60000 {
		t.Errorf("Landed[1].To = %d, want the requested 60000 exactly", landed[1].To)
	}
	// The interior splices snapped outward, and say so.
	if landed[0].To != 20480 { // snapUp(20000, 1024)
		t.Errorf("Landed[0].To = %d, want 20480", landed[0].To)
	}
	if landed[1].From != 38912 { // snapDown(40000-1024, 1024) = 38*1024
		t.Errorf("Landed[1].From = %d, want 38912", landed[1].From)
	}
	for i, s := range landed {
		if s.To-s.From < int64(spans[i].To-spans[i].From) {
			t.Errorf("Landed[%d] = %v is shorter than the requested %v; snapping only ever widens", i, s, spans[i])
		}
	}
}

// TestCutTrackRefusesUnexpressibleGaps: a gap smaller than the grid can express
// makes the keep windows overlap, which would emit a packet twice. Declining is
// right; merging would break Landed's one-for-one correspondence.
func TestCutTrackRefusesUnexpressibleGaps(t *testing.T) {
	track := aacTrack(0, 96000)
	_, _, err := CutTrack(track, []Span{{0, 1000}, {1200, 20000}}, 1024)
	if err == nil {
		t.Fatal("a sub-grid gap was accepted; the windows overlap and packet 1 is emitted twice")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v (a decline, not a bad request: rung 3 serves this exactly)",
			got, waxerr.CodeUnsupportedFormat)
	}
	// A gap wide enough to express is fine.
	if _, _, err := CutTrack(track, []Span{{0, 1000}, {20000, 40000}}, 1024); err != nil {
		t.Errorf("a gap of 19000 samples was declined: %v", err)
	}
}

// TestCutTrackDeclinesAnUnanswerableTail: PacketGrid does not measure the final
// short packet, so a bounded last span landing inside it cannot be resolved from
// the header.
func TestCutTrackDeclinesAnUnanswerableTail(t *testing.T) {
	// 47000 + 0 delay + 0 padding: the decode ends at 47000, which is not a
	// multiple of 1024, so the final packet is short and runs [46080, 47000).
	track := aacTrack(0, 47000)
	_, _, err := CutTrack(track, []Span{{0, 46500}}, 1024)
	if err == nil {
		t.Fatal("a span ending inside the final short packet was accepted")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v", got, waxerr.CodeUnsupportedFormat)
	}
	// A span ending on a boundary the header can name is fine.
	if _, _, err := CutTrack(track, []Span{{0, 46080}}, 1024); err != nil {
		t.Errorf("a span ending exactly on the grid was declined: %v", err)
	}
	// And ToEnd never asks the question: it runs to EOF.
	if _, _, err := CutTrack(track, []Span{{0, ToEnd}}, 1024); err != nil {
		t.Errorf("a ToEnd span was declined: %v", err)
	}
}

// TestCutTrackRefusesAZeroLengthSpan: SpanTrack permits To == From because a
// zero-sample Media is coherent. A zero-sample packet span is not, and it fails
// in the surprising direction: snapping only widens, so the caller who asked to
// keep nothing would get a whole packet of audio.
func TestCutTrackRefusesAZeroLengthSpan(t *testing.T) {
	track := aacTrack(0, 96000)
	_, _, err := CutTrack(track, []Span{{100, 100}}, 1024)
	if err == nil {
		t.Fatal("a zero-length span was accepted; it would have landed as a whole packet of audio")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("code = %v, want %v (rung 3 would refuse this identically)", got, waxerr.CodeInvalidRequest)
	}
	// The opposite worry, raised and dismissed: snapping cannot reduce a short
	// span to nothing. A span entirely inside one packet lands as that packet.
	_, landed, err := CutTrack(track, []Span{{100, 200}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if landed[0].To-landed[0].From < 100 {
		t.Errorf("Landed = %v is shorter than the 100 samples requested", landed[0])
	}
}

// TestCutTrackRefusesAnEmptySpanInEitherSpelling: {From, From} and
// {Samples, ToEnd} are the same request, "keep nothing", and refusing one while
// accepting the other is the worst of both.
//
// The ToEnd spelling did not fail usefully either. It returned Samples 0 with a
// Delay covering a pre-roll of delivered audio, and a zero-length Landed span,
// which the rung's own promise that a landed span is never shorter than a grid
// says cannot happen.
func TestCutTrackRefusesAnEmptySpanInEitherSpelling(t *testing.T) {
	track := opusTrack(312, 96000)
	for _, s := range []Span{{96000, 96000}, {96000, ToEnd}} {
		cut, landed, err := CutTrack(track, []Span{s}, 960)
		if err == nil {
			t.Errorf("CutTrack accepted the empty span %v: Samples=%d Delay=%d landed=%v",
				s, cut.Samples, cut.Delay, landed)
			continue
		}
		if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
			t.Errorf("span %v: code = %v, want %v", s, got, waxerr.CodeInvalidRequest)
		}
	}
	// A From at the end of a source that declares no length cannot be known to
	// be empty, so it is not refused: a bound that cannot be checked is not
	// checked, which is SpanTrack's own call.
	if _, _, err := CutTrack(aacTrack(1024, -1), []Span{{96000, ToEnd}}, 1024); err != nil {
		t.Errorf("a ToEnd span on a lengthless source was refused: %v", err)
	}
	// And a real ToEnd span from inside the track still works.
	if _, _, err := CutTrack(track, []Span{{48000, ToEnd}}, 960); err != nil {
		t.Errorf("an ordinary ToEnd span was refused: %v", err)
	}
}

// TestCutViewReportsTheCutTrack: the view's packets are the cut's, so its
// Tracks() must be too. The embedded Demuxer would otherwise promote the
// source's answer, and every field that matters would be a lie.
//
// Nothing in the intended flow reads it, which is exactly why it would have gone
// unnoticed: format.FromDemuxer builds Info.Tracks straight off this call, so a
// caller assembling a Media around a cut view would get the source's headers
// over the cut's packets.
func TestCutViewReportsTheCutTrack(t *testing.T) {
	track := opusTrack(312, 96000)
	want, _, err := CutTrack(track, []Span{{48000, 72000}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Cut(&gridDemuxer{n: 200, dur: 960}, track, []Span{{48000, 72000}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	got := view.Tracks()
	if len(got) != 1 {
		t.Fatalf("Tracks() = %d tracks, want exactly the cut's one", len(got))
	}
	if got[0].Samples != want.Samples || got[0].Delay != want.Delay || got[0].Padding != want.Padding {
		t.Errorf("Tracks()[0] = {Samples:%d Delay:%d Padding:%d}, want the cut's {Samples:%d Delay:%d Padding:%d}",
			got[0].Samples, got[0].Delay, got[0].Padding, want.Samples, want.Delay, want.Padding)
	}
	if got[0].Samples == track.Samples {
		t.Errorf("Tracks() reports the source's uncut length %d", track.Samples)
	}
	cfg, err := opus.ParseOpusHead(got[0].CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreSkip == 312 {
		t.Error("Tracks() reports the source's un-rewritten OpusHead pre-skip")
	}
	// The packet walk still filters on the source's own track ID, which the cut
	// track carries across: reporting the cut's headers must not renumber it.
	if got[0].ID != track.ID {
		t.Errorf("Tracks()[0].ID = %d, want the source's %d", got[0].ID, track.ID)
	}
}

// TestCutTrackErrorsVsDeclines pins the rung's load-bearing mechanism: CutTrack
// returns codes throughout because its signature has nowhere to put a decline,
// and PlanCut is what maps them onto the ladder's (nil, nil) contract.
//
// The split is the seam between "this rung cannot serve this" (a decline: rung 3
// serves it) and "no rung can" (an error: rung 3 fails identically).
func TestCutTrackErrorsVsDeclines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		track container.Track
		spans []Span
		grid  int
		want  waxerr.Code
	}{
		// Errors: spans that do not describe this file.
		{"empty span list", aacTrack(0, 96000), nil, 1024, waxerr.CodeInvalidRequest},
		{"negative start", aacTrack(0, 96000), []Span{{-1, 100}}, 1024, waxerr.CodeInvalidRequest},
		{"ends before it starts", aacTrack(0, 96000), []Span{{500, 100}}, 1024, waxerr.CodeInvalidRequest},
		{"past the end", aacTrack(0, 96000), []Span{{0, 96001}}, 1024, waxerr.CodeInvalidRequest},
		{"out of order", aacTrack(0, 96000), []Span{{40000, 60000}, {0, 20000}}, 1024, waxerr.CodeInvalidRequest},
		{"ToEnd other than last", aacTrack(0, 96000), []Span{{0, ToEnd}, {40000, 60000}}, 1024, waxerr.CodeInvalidRequest},
		{"zero length", aacTrack(0, 96000), []Span{{100, 100}}, 1024, waxerr.CodeInvalidRequest},
		// Declines: this rung cannot, another can.
		{"codec off the allowlist", container.Track{Codec: codec.MP3, Samples: 96000}, []Span{{0, 20000}}, 1152, waxerr.CodeUnsupportedFormat},
		{"no grid", aacTrack(0, 96000), []Span{{0, 20000}}, 0, waxerr.CodeUnsupportedFormat},
		{"sub-grid gap", aacTrack(0, 96000), []Span{{0, 1000}, {1200, 20000}}, 1024, waxerr.CodeUnsupportedFormat},
		{"unanswerable tail", aacTrack(0, 47000), []Span{{0, 46500}}, 1024, waxerr.CodeUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CutTrack(tc.track, tc.spans, tc.grid)
			if err == nil {
				t.Fatalf("CutTrack accepted %v", tc.spans)
			}
			if got := waxerr.CodeOf(err); got != tc.want {
				t.Errorf("code = %v, want %v (%v)", got, tc.want, err)
			}
		})
	}
}

// TestCutTrackRefusesSpansThatOverflowTheTimeline guards the one hole the
// length check cannot cover.
//
// A span is bounded against the source's length only when there is one to bound
// against, which is SpanTrack's own call and the right one. But an ADTS source
// declares no length and AAC-LC is on the allowlist, so a caller's To reaches
// the grid arithmetic unchecked, and that arithmetic overflows rather than
// saturating: To + Delay wraps, and the snap's x + g - 1 wraps after it.
//
// The reason this is a refusal and not a comment is that an overflowed span does
// not fail, it lands. Before the guard, a MaxInt64 To was accepted and
// synthesized a Padding of 9223372036854774785 with no error anywhere.
func TestCutTrackRefusesSpansThatOverflowTheTimeline(t *testing.T) {
	track := aacTrack(1024, -1) // the ADTS shape: no length to bound against
	for _, to := range []int64{math.MaxInt64, math.MaxInt64 - 1024, 1 << 62} {
		cut, _, err := CutTrack(track, []Span{{0, to}}, 1024)
		if err == nil {
			t.Errorf("CutTrack accepted To=%d and synthesized Samples=%d Padding=%d",
				to, cut.Samples, cut.Padding)
			continue
		}
		if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
			t.Errorf("To=%d: code = %v, want %v", to, got, waxerr.CodeInvalidRequest)
		}
	}
	// A From past the ceiling is refused on the same terms.
	if _, _, err := CutTrack(track, []Span{{math.MaxInt64 - 1, ToEnd}}, 1024); err == nil {
		t.Error("CutTrack accepted a From at the top of the timeline")
	}

	// The span is only one of the three addends, and bounding it alone leaves
	// the sum free: the positions are span + Delay and the snap adds grid - 1 on
	// top. Nothing in the tree bounds a container's Delay above, so a hostile
	// one overflowed the snap just as well and produced a Padding of 1525 where
	// 501 was the answer. These decline rather than error: neither number is the
	// caller's, so rung 3 gets the request.
	for _, tc := range []struct {
		name  string
		delay int64
		grid  int
	}{
		{"a delay past the ceiling", math.MaxInt64 - 1000, 1024},
		{"a delay at the ceiling", 1 << 61, 1024},
		{"a negative delay", -1, 1024},
		{"a grid past the ceiling", 1024, 1 << 61},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cut, _, err := CutTrack(aacTrack(tc.delay, -1), []Span{{0, 500}}, tc.grid)
			if err == nil {
				t.Fatalf("CutTrack accepted delay=%d grid=%d and synthesized Samples=%d Padding=%d",
					tc.delay, tc.grid, cut.Samples, cut.Padding)
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
				t.Errorf("code = %v, want a decline: the source's own numbers are not the caller's fault", got)
			}
		})
	}
	// The ceiling refuses nothing real: 2^62 samples is about three million
	// years at 48 kHz, and an ordinary unbounded cut of a lengthless source is
	// still served.
	if _, _, err := CutTrack(track, []Span{{0, ToEnd}}, 1024); err != nil {
		t.Errorf("a ToEnd cut of a lengthless source was refused: %v", err)
	}
	if _, _, err := CutTrack(track, []Span{{0, 48000}}, 1024); err != nil {
		t.Errorf("an ordinary bounded cut of a lengthless source was refused: %v", err)
	}
}

// TestPlanCutMapsCodesOntoTheLadder is the other half of the seam: the same
// inputs, through PlanCut, must become a decline or an error per the ladder's
// published contract.
func TestPlanCutMapsCodesOntoTheLadder(t *testing.T) {
	e := New()
	opts := TranscodeOptions{Format: "aac", Container: "mka"}

	// A decline is (nil, nil): the caller falls through to a transcode.
	plan, err := e.PlanCut(aacTrack(0, 96000), opts, []Span{{0, 1000}, {1200, 20000}}, 1024)
	if err != nil || plan != nil {
		t.Errorf("PlanCut(sub-grid gap) = (%v, %v), want (nil, nil): a decline is not an error", plan, err)
	}
	// An error is an error: no rung serves it.
	if _, err := e.PlanCut(aacTrack(0, 96000), opts, []Span{{100, 100}}, 1024); err == nil {
		t.Error("PlanCut(zero-length span) returned no error; rung 3 would refuse it identically")
	}
	// And a servable cut plans.
	plan, err = e.PlanCut(aacTrack(1024, 96000), opts, []Span{{0, 20480}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanCut declined a cut it can serve")
	}
	if plan.Samples != plan.Track.Samples {
		t.Errorf("plan.Samples = %d but plan.Track.Samples = %d", plan.Samples, plan.Track.Samples)
	}
	if len(plan.Landed) != 1 {
		t.Fatalf("Landed = %v, want one span for one", plan.Landed)
	}
	var haveCut bool
	for _, v := range plan.Versions {
		if v == CutVersion {
			haveCut = true
		}
	}
	if !haveCut {
		t.Errorf("Versions = %v, want %s among them: the cut's arithmetic shapes the bytes",
			plan.Versions, CutVersion)
	}
}

// TestCutTrackResolvesAnUnknownLength: an unknown source length inverts the
// arithmetic rather than defeating it, which is remuxTrailer's own precedent.
// Reachable for real rather than hypothetical: ADTS declares no length and
// AAC-LC is on the allowlist.
func TestCutTrackResolvesAnUnknownLength(t *testing.T) {
	track := aacTrack(1024, -1) // the ADTS shape
	cut, landed, err := CutTrack(track, []Span{{20000, ToEnd}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Samples != -1 {
		t.Errorf("Samples = %d, want -1 propagated: there is nothing to resolve the end against", cut.Samples)
	}
	if landed[0].To != ToEnd {
		t.Errorf("Landed[0].To = %d, want ToEnd", landed[0].To)
	}
	if landed[0].From != 20000 {
		t.Errorf("Landed[0].From = %d, want the requested 20000 exactly", landed[0].From)
	}
	// remuxTrailer resolves the landed length from the walk, and it is the same
	// number a bounded cut would have computed from the header.
	// df = 21024; sd = snapDown(21024-1024) = 20000 -> 19*1024 = 19456.
	// Delay' = 21024-19456 = 1568. Say the walk delivers to decoded 100000:
	// the kept window is [19456, 100000), 80544 samples.
	const delivered = 80544
	tr := remuxTrailer(cut, delivered)
	if want := int64(delivered) - cut.Delay - cut.Padding; tr.Samples != want {
		t.Errorf("remuxTrailer samples = %d, want %d", tr.Samples, want)
	}
	if tr.Samples <= 0 {
		t.Errorf("remuxTrailer samples = %d, want a real length resolved from the walk", tr.Samples)
	}
}

// TestCutTrackSamplesExact: a bounded last span makes the length computed rather
// than declared, so it is exact by construction. A ToEnd one inherits whatever
// the source's own total was worth.
func TestCutTrackSamplesExact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		track container.Track
		spans []Span
		want  bool
	}{
		{"bounded is exact", aacTrack(1024, 96000), []Span{{0, 20480}}, true},
		{"ToEnd inherits false", aacTrack(1024, 96000), []Span{{0, ToEnd}}, false},
		{"ToEnd inherits true", func() container.Track {
			t := aacTrack(1024, 96000)
			t.SamplesExact = true
			return t
		}(), []Span{{0, ToEnd}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cut, _, err := CutTrack(tc.track, tc.spans, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if cut.SamplesExact != tc.want {
				t.Errorf("SamplesExact = %v, want %v", cut.SamplesExact, tc.want)
			}
		})
	}
}

// TestCutCodecsIsAnAllowlist: a codec landing later must opt in rather than
// silently ride a rule that was never checked against it. Each exclusion below
// names what actually breaks, because the reasons are not interchangeable and
// one of them was wrong in an earlier draft of this work.
func TestCutCodecsIsAnAllowlist(t *testing.T) {
	for _, tc := range []struct {
		id   codec.ID
		in   bool
		why  string
		grid int
	}{
		{codec.Opus, true, "position-independent packets, and the pre-skip is rewritable", 960},
		{codec.AACLC, true, "position-independent frames; priming rides the container's edit list", 1024},
		{codec.MP3, false, "the bit reservoir: a frame's main data may begin inside earlier frames, " +
			"and an unsatisfied reference decodes to silence rather than erroring", 1152},
		{codec.FLAC, false, "a multi-span cut leaves a gap in the frame ordinals, and the boundary " +
			"scan then glues the post-gap frames into one packet that decodes to its first frame " +
			"alone, ending in a clean EOF with the rest of the audio silently gone " +
			"(TestCutFLACMultiSpanBreaks measures it); the STREAMINFO MD5 goes stale with no way to " +
			"say unknown; and the PTS rides the ordinal rather than the container. A contiguous cut " +
			"does re-read clean, so the reason is the gap and not the cut. Moot regardless: FLAC is " +
			"lossless, so rung 3 costs no generation and EncoderOptions.FirstFrame already numbers a " +
			"mid-stream slice correctly", 4096},
		{codec.ALAC, false, "nothing, technically: its frames are position-independent. It is out on " +
			"codecSurvives's own argument, that this rung avoids generation loss and lossless has " +
			"none, and mp4 refuses its trims besides", 4096},
		{codec.Vorbis, false, "overlap-adds with its predecessor, so the first packet to arrive is " +
			"priming and emits nothing. packetGrid's prev <= 0 clause already excludes it; named " +
			"anyway, because a rule holding by accident of another rule is one waiting to break", 1024},
		{codec.PCM, false, "already out at codecSurvives: its packet is raw samples whose wire " +
			"layout is the container's choice", 1},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			_, ok := cutCodecs[tc.id]
			if ok != tc.in {
				t.Fatalf("cutCodecs[%v] present = %v, want %v (%s)", tc.id, ok, tc.in, tc.why)
			}
			track := container.Track{Codec: tc.id, Samples: 96000}
			_, _, err := CutTrack(track, []Span{{0, 20000}}, tc.grid)
			if tc.in {
				return // the allowlisted codecs are exercised for real elsewhere
			}
			if err == nil {
				t.Fatalf("CutTrack accepted %v, which is out because %s", tc.id, tc.why)
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
				t.Errorf("code = %v, want a decline: rung 3 serves %v correctly", got, tc.id)
			}
		})
	}
}

// TestCutDeclinesTrimsTheDestinationCannotWrite is the decline nothing else in
// the ladder covers. PlanRemux checks the source's trims; a cut's are new, and
// the allowlist screens codecs rather than destinations.
func TestCutDeclinesTrimsTheDestinationCannotWrite(t *testing.T) {
	e := New()

	// An AAC track with Delay 0 (an MP4 muxed without iTunSMPB, which is
	// common), cut so the padding is not a whole frame. To fMP4 this must
	// decline rather than die at End after the whole file is written.
	track := aacTrack(0, 96000)
	plan, err := e.PlanCut(track, TranscodeOptions{Format: "aac"}, []Span{{0, 20000}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Errorf("PlanCut to fMP4 accepted Delay=%d Padding=%d; the muxer rejects that at End, "+
			"part way through a response", plan.Track.Delay, plan.Track.Padding)
	}

	// The same cut to ADTS must decline rather than silently play the audio the
	// caller removed: ADTS can signal no trim at all, and this rung's Delay
	// covers real source audio from before the cut point.
	plan, err = e.PlanCut(aacTrack(1024, 96000), TranscodeOptions{Format: "aac", Container: "adts"},
		[]Span{{20000, 40000}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Errorf("PlanCut to ADTS accepted Delay=%d; that is the wrong audio, not unsignalled priming",
			plan.Track.Delay)
	}

	// A From == 0 cut to ADTS that synthesizes no trims at all stays legal,
	// which is the useful case and the early-out that keeps this narrow.
	plan, err = e.PlanCut(aacTrack(0, -1), TranscodeOptions{Format: "aac", Container: "adts"},
		[]Span{{0, ToEnd}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Error("PlanCut declined a From=0 ToEnd cut to ADTS, which synthesizes no trims to signal")
	}

	// Matroska carries both trims outright, so the same cut the fMP4 case
	// declined plans fine there. Without this row the test would pass on a
	// PlanCut that declined everything.
	plan, err = e.PlanCut(track, TranscodeOptions{Format: "aac", Container: "mka"}, []Span{{0, 20000}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Error("PlanCut to mka declined; Matroska signals CodecDelay and DiscardPadding outright")
	}
}

// TestCutRetimesPacketsContiguously drives the wrapper over a synthetic demuxer:
// the kept packets must come out with no holes in their PTS, since the cut
// track's Delay trims the head exactly as a plain remux's does.
func TestCutRetimesPacketsContiguously(t *testing.T) {
	track := aacTrack(0, 96000)
	spans := []Span{{0, 20480}, {40960, 61440}} // all on the 1024 grid
	demux, err := Cut(&gridDemuxer{n: 94, dur: 1024}, track, spans, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var got []int64
	var want int64
	for {
		err := demux.ReadPacket(&pkt)
		if err != nil {
			break
		}
		if pkt.PTS != want {
			t.Fatalf("packet PTS = %d, want %d: the output timeline has a hole", pkt.PTS, want)
		}
		want += pkt.Dur
		got = append(got, int64(pkt.Data[0])) // the source packet's ordinal
	}
	// Windows: [0, 20480) is source packets 0..19, and [39936, 61440) is 39..59
	// (the head backs off by one frame of pre-roll).
	if len(got) != 20+21 {
		t.Fatalf("kept %d packets, want %d", len(got), 20+21)
	}
	for i := range 20 {
		if got[i] != int64(i) {
			t.Errorf("kept[%d] = source packet %d, want %d", i, got[i], i)
		}
	}
	for i := range 21 {
		if want := int64(39 + i); got[20+i] != want {
			t.Errorf("kept[%d] = source packet %d, want %d", 20+i, got[20+i], want)
		}
	}
}

// TestCutStraddleErrorsLoudly: the boundaries are computed from the header, so a
// plan computed against a different source would splice mid-packet. It is the
// one failure of this rung that no other error would surface, and the re-check
// is free because the walk is happening anyway.
func TestCutStraddleErrorsLoudly(t *testing.T) {
	track := aacTrack(0, 96000)
	// The spans are computed on a 1024 grid, but the source delivers 1000-sample
	// packets: the stand-in for a stale plan or a file replaced under its URL.
	demux, err := Cut(&gridDemuxer{n: 96, dur: 1000}, track, []Span{{2048, 20480}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == nil {
			continue
		}
		if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
			t.Fatalf("error = %v (code %v), want a loud straddle refusal", err, got)
		}
		return
	}
}

// gridDemuxer emits n packets of dur samples each, whose payload is the packet's
// own ordinal so a test can say which ones survived a cut.
type gridDemuxer struct {
	n, i int
	dur  int64
	pos  int64
}

func (d *gridDemuxer) Tracks() []container.Track { return nil }

func (d *gridDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.i == d.n {
		return io.EOF
	}
	*pkt = container.Packet{Track: 0, Packet: codec.Packet{
		Data: []byte{byte(d.i)},
		PTS:  d.pos,
		Dur:  d.dur,
		Sync: true,
	}}
	d.i++
	d.pos += d.dur
	return nil
}
