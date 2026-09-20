package waxflow

import (
	"context"
	"io"
	"math"
	"slices"
	"strings"
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
	cut, landed, err := CutTrack(track, TranscodeOptions{}, []Span{{0, ToEnd}}, 960)
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
	cut, landed, err := CutTrack(track, TranscodeOptions{}, []Span{{48000, 72000}}, 960)
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
// Under the requested-length version this fails by exactly however far the
// interior edges moved, and the failure is silent in production: mka would
// write a DiscardPadding that eats that much real audio off the end. The
// edges now move the other way (inward, so the landed length is under the
// request), which changes nothing here: the identity is between Samples and
// the windows the walk delivers, whichever side of the request they fall on.
func TestCutTrackSamplesAreLanded(t *testing.T) {
	track := opusTrack(312, 96000)
	spans := []Span{{0, 20000}, {40000, 60000}}
	cut, landed, err := CutTrack(track, TranscodeOptions{}, spans, 960)
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
	const requested = (20000 - 0) + (60000 - 40000)
	if cut.Samples > requested {
		t.Errorf("Samples = %d is past the %d requested; an interior edge must never deliver audio from outside the request",
			cut.Samples, requested)
	}
	// Two interior edges, each losing under one packet.
	if short := int64(requested) - cut.Samples; short >= 2*960 {
		t.Errorf("Samples = %d is %d short of the %d requested, which is a packet or more per interior edge",
			cut.Samples, short, requested)
	}
	// The trailer the muxer will actually be handed. decoded is what the walk
	// delivers: the sum of the kept windows.
	decoded := cut.Delay + cut.Samples + cut.Padding
	tr := remuxTrailer(cut, copiedRun{samples: decoded})
	if tr.Padding != cut.Padding {
		t.Errorf("remuxTrailer padding = %d, want %d: the slop did not cancel", tr.Padding, cut.Padding)
	}
	if tr.Samples != cut.Samples {
		t.Errorf("remuxTrailer samples = %d, want %d", tr.Samples, cut.Samples)
	}
}

// TestCutTrackReportsWhereItLanded pins the head/tail-exact, interior-snapped
// split, which is the rung's whole promise about position: nothing outside the
// request is ever delivered, and each interior edge lands within one packet
// inside it. See ADR-0011.
func TestCutTrackReportsWhereItLanded(t *testing.T) {
	track := aacTrack(0, 96000)
	spans := []Span{{100, 20000}, {40000, 60000}}
	_, landed, err := CutTrack(track, TranscodeOptions{}, spans, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if landed[0].From != 100 {
		t.Errorf("Landed[0].From = %d, want the requested 100 exactly", landed[0].From)
	}
	if landed[1].To != 60000 {
		t.Errorf("Landed[1].To = %d, want the requested 60000 exactly", landed[1].To)
	}
	// The interior splices snapped inward, and say so.
	if landed[0].To != 19456 { // snapDown(20000, 1024) = 19*1024
		t.Errorf("Landed[0].To = %d, want 19456", landed[0].To)
	}
	if landed[1].From != 40960 { // snapUp(40000, 1024) = 40*1024
		t.Errorf("Landed[1].From = %d, want 40960", landed[1].From)
	}
	for i, s := range landed {
		if s.From < spans[i].From || s.To > spans[i].To {
			t.Errorf("Landed[%d] = %v reaches outside the requested %v", i, s, spans[i])
		}
		if (s.From-spans[i].From)+(spans[i].To-s.To) >= 1024 {
			t.Errorf("Landed[%d] = %v gives up a whole packet of the requested %v", i, s, spans[i])
		}
	}
}

// TestCutTrackRefusesASpanWithNoWholePacket is the cost of snapping inward:
// a request narrower than the grid, or one falling between two boundaries,
// keeps nothing. Snapping outward could not produce this, which is why it is
// a new refusal rather than an old one. Rung 3 re-encodes such a span exactly.
func TestCutTrackRefusesASpanWithNoWholePacket(t *testing.T) {
	track := aacTrack(0, 96000)
	// [20000, 21000) spans no 1024 boundary: snapUp(20000) = 20480 and
	// snapDown(21000) = 20480.
	_, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, 10000}, {20000, 21000}, {40000, 50000}}, 1024)
	if err == nil {
		t.Fatal("a span that keeps no whole packet was accepted")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v (a decline: rung 3 serves this exactly)", got, waxerr.CodeUnsupportedFormat)
	}
	// One packet is enough.
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, 10000}, {20000, 22000}, {40000, 50000}}, 1024); err != nil {
		t.Errorf("a span holding one whole packet was declined: %v", err)
	}
}

// TestCutTrackRefusesUnexpressibleGaps: keep windows that overlap would emit
// the packet between them twice. Declining is right; merging would break
// Landed's one-for-one correspondence.
//
// Only SpliceTrims can reach it. Inward snapping never narrows a gap, so
// under the default policy su[i] <= sd[i+1] holds by construction; with the
// option on, every interior head backs off by the codec's pre-roll again and
// every interior tail snaps out, which is what makes a small gap overlap.
// The pair below is accepted under the default policy and declined with the
// option, which is the difference stated as a test rather than as a comment.
func TestCutTrackRefusesUnexpressibleGaps(t *testing.T) {
	track := opusTrack(312, 96000)
	// A 1000-sample gap against a 3840-sample pre-roll on a 960 grid: the
	// second window's walk would start at 18240, inside the first window's
	// 21120 end.
	spans := []Span{{0, 20000}, {21000, 40000}}
	if _, _, err := CutTrack(track, TranscodeOptions{}, spans, 960); err != nil {
		t.Errorf("the default policy declined a gap it snaps clear of: %v", err)
	}
	_, _, err := CutTrack(track, TranscodeOptions{SpliceTrims: true}, spans, 960)
	if err == nil {
		t.Fatal("a spliced cut whose pre-roll reaches back into the span before it was accepted")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v (a decline, not a bad request: rung 3 serves this exactly)",
			got, waxerr.CodeUnsupportedFormat)
	}
	if !strings.Contains(err.Error(), "closer together") {
		t.Errorf("err = %q, want the overlap refusal rather than another decline", err)
	}
	// A gap wide enough for the pre-roll is fine with the option on.
	if _, _, err := CutTrack(track, TranscodeOptions{SpliceTrims: true},
		[]Span{{0, 20000}, {40000, 60000}}, 960); err != nil {
		t.Errorf("a 20000-sample gap was declined with SpliceTrims: %v", err)
	}
}

// TestCutTrackResolvesATailInsideTheFinalPacket: PacketGrid does not measure
// the final short packet, so a bounded last span landing inside it cannot be
// bounded from the header, and the window runs to EOF instead. The trailer is
// what resolves it: the cut's length is exact, and container.SettleLength
// re-derives the padding from the run the copy counted for an exact length
// whether or not the track declares a delay. This used to decline when the
// track had no delay, back when only a delay triggered that re-derivation.
func TestCutTrackResolvesATailInsideTheFinalPacket(t *testing.T) {
	// 47000 + 0 delay + 0 padding: the decode ends at 47000, which is not a
	// multiple of 1024, so the final packet is short and runs [46080, 47000).
	track := aacTrack(0, 47000)
	cut, landed, err := CutTrack(track, TranscodeOptions{}, []Span{{0, 46500}}, 1024)
	if err != nil {
		t.Fatalf("a span ending inside the final short packet was declined: %v", err)
	}
	if cut.Delay != 0 || cut.Padding != 500 || cut.Samples != 46500 || !cut.SamplesExact {
		t.Errorf("cut = {Delay %d Padding %d Samples %d exact %v}, want {0 500 46500 true}",
			cut.Delay, cut.Padding, cut.Samples, cut.SamplesExact)
	}
	if want := (Span{0, 46500}); landed[0] != want {
		t.Errorf("Landed = %v, want %v", landed[0], want)
	}
	// The run half: the view keeps to EOF and the trailer the muxer is handed
	// says exactly what the plan did, through the same copy and settle
	// RemuxDemuxer runs.
	view, err := Cut(&shortTailDemuxer{n: 46, dur: 1024, last: 920}, track, TranscodeOptions{}, []Span{{0, 46500}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	run, err := copyPackets(context.Background(), view, 0, false, func(container.Packet) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if run.samples != 47000 {
		t.Fatalf("the view delivered %d samples, want the whole 47000 to EOF", run.samples)
	}
	if tr := remuxTrailer(cut, run); tr.Samples != 46500 || tr.Padding != 500 || tr.Delay != 0 {
		t.Errorf("remuxTrailer = %+v, want {Samples 46500 Delay 0 Padding 500}", tr)
	}
	// A span ending on a boundary the header can name, and ToEnd, which never
	// asks the question, are as before.
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, 46080}}, 1024); err != nil {
		t.Errorf("a span ending exactly on the grid was declined: %v", err)
	}
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, ToEnd}}, 1024); err != nil {
		t.Errorf("a ToEnd span was declined: %v", err)
	}
}

// shortTailDemuxer yields n packets of dur, the last one last samples long:
// an encoder's tail flush.
type shortTailDemuxer struct {
	n, i      int
	dur, last int64
}

func (d *shortTailDemuxer) Tracks() []container.Track { return nil }

func (d *shortTailDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.i == d.n {
		return io.EOF
	}
	dur := d.dur
	if d.i == d.n-1 {
		dur = d.last
	}
	*pkt = container.Packet{Track: 0, Packet: codec.Packet{Data: []byte{byte(d.i)}, Dur: dur, Sync: true}}
	d.i++
	return nil
}

// TestCutTrackRefusesAZeroLengthSpan: SpanTrack permits To == From because a
// zero-sample Media is coherent. A zero-sample packet span is not, and it fails
// in the surprising direction: snapping only widens, so the caller who asked to
// keep nothing would get a whole packet of audio.
func TestCutTrackRefusesAZeroLengthSpan(t *testing.T) {
	track := aacTrack(0, 96000)
	_, _, err := CutTrack(track, TranscodeOptions{}, []Span{{100, 100}}, 1024)
	if err == nil {
		t.Fatal("a zero-length span was accepted; it would have landed as a whole packet of audio")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("code = %v, want %v (rung 3 would refuse this identically)", got, waxerr.CodeInvalidRequest)
	}
	// The opposite worry, raised and dismissed: snapping cannot reduce a short
	// span to nothing. A span entirely inside one packet lands as that packet.
	_, landed, err := CutTrack(track, TranscodeOptions{}, []Span{{100, 200}}, 1024)
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
		cut, landed, err := CutTrack(track, TranscodeOptions{}, []Span{s}, 960)
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
	if _, _, err := CutTrack(aacTrack(1024, -1), TranscodeOptions{}, []Span{{96000, ToEnd}}, 1024); err != nil {
		t.Errorf("a ToEnd span on a lengthless source was refused: %v", err)
	}
	// And a real ToEnd span from inside the track still works.
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{48000, ToEnd}}, 960); err != nil {
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
	want, _, err := CutTrack(track, TranscodeOptions{}, []Span{{48000, 72000}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Cut(&gridDemuxer{n: 200, dur: 960}, track, TranscodeOptions{}, []Span{{48000, 72000}}, 960)
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
		{"a span with no whole packet", aacTrack(0, 96000), []Span{{0, 1000}, {1200, 20000}}, 1024, waxerr.CodeUnsupportedFormat},
		{"trims the walk did not place", func() container.Track {
			t := aacTrack(0, 96000)
			t.MidPadding = 480
			return t
		}(), []Span{{0, 20000}}, 1024, waxerr.CodeUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CutTrack(tc.track, TranscodeOptions{}, tc.spans, tc.grid)
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
		cut, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, to}}, 1024)
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
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{math.MaxInt64 - 1, ToEnd}}, 1024); err == nil {
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
		grid  int64
	}{
		{"a delay past the ceiling", math.MaxInt64 - 1000, 1024},
		{"a delay at the ceiling", maxCutSample, 1024},
		{"a negative delay", -1, 1024},
		{"a grid past the ceiling", 1024, maxCutSample},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.grid > math.MaxInt {
				// A grid is an int, so on a 32-bit build nothing can reach the
				// ceiling and the type is the guard.
				t.Skip("no int on this build can hold a grid past maxCutSample")
			}
			cut, _, err := CutTrack(aacTrack(tc.delay, -1), TranscodeOptions{}, []Span{{0, 500}}, int(tc.grid))
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
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, ToEnd}}, 1024); err != nil {
		t.Errorf("a ToEnd cut of a lengthless source was refused: %v", err)
	}
	if _, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, 48000}}, 1024); err != nil {
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
		t.Errorf("PlanCut(a span with no whole packet) = (%v, %v), want (nil, nil): a decline is not an error", plan, err)
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
	cut, landed, err := CutTrack(track, TranscodeOptions{}, []Span{{20000, ToEnd}}, 1024)
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
	tr := remuxTrailer(cut, copiedRun{samples: delivered})
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
			cut, _, err := CutTrack(tc.track, TranscodeOptions{}, tc.spans, 1024)
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
	rows := []struct {
		id   codec.ID
		in   bool
		why  string
		grid int
	}{
		{codec.Opus, true, "position-independent packets, and the pre-skip is rewritable", 960},
		{codec.AACLC, true, "position-independent frames; priming rides the container's edit list", 1024},
		{codec.HEAAC, true, "the same frame independence as AAC-LC, but head-bound: only spans " +
			"keeping AU zero are served, because the SBR header rides the leading fills and a " +
			"mid-stream slice would decode with a muted high band " +
			"(TestCutOfHEAACMatchesFullDecode measures the served case)", 2048},
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
	}
	for _, tc := range rows {
		t.Run(string(tc.id), func(t *testing.T) {
			_, ok := cutCodecs[tc.id]
			if ok != tc.in {
				t.Fatalf("cutCodecs[%v] present = %v, want %v (%s)", tc.id, ok, tc.in, tc.why)
			}
			track := container.Track{Codec: tc.id, Samples: 96000}
			_, _, err := CutTrack(track, TranscodeOptions{}, []Span{{0, 20000}}, tc.grid)
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

	// The reverse direction: every cutCodecs entry must have a row above, so
	// the next codec cannot land in the map and ride through unpinned.
	listed := map[codec.ID]bool{}
	for _, tc := range rows {
		listed[tc.id] = true
	}
	for id := range cutCodecs {
		if !listed[id] {
			t.Errorf("cutCodecs[%v] has no allowlist row in this test; add one naming why it is in", id)
		}
	}
}

// TestCutFormatsIsHonest is the source of truth for the /caps
// delivery.cutFormats advertisement: it pins the exact list and proves every
// name on it is reachable on both surfaces the cut serves. The server- and
// client-side wire tests defer here, so the advertisement cannot drift from
// what the rung actually does.
func TestCutFormatsIsHonest(t *testing.T) {
	got := CutFormats()
	if want := []string{"opus", "aac"}; !slices.Equal(got, want) {
		t.Fatalf("CutFormats() = %v, want %v", got, want)
	}

	// Index the output table by name so a returned name can be checked against
	// its own row's codec and surfaces.
	byName := map[string]output{}
	for _, o := range outputs {
		byName[o.name] = o
	}
	segmented := map[string]bool{}
	for _, name := range SegmentedFormats() {
		segmented[name] = true
	}
	for _, name := range got {
		o, ok := byName[name]
		if !ok {
			t.Fatalf("CutFormats() returned %q, which is not an output format", name)
		}
		if !Cuttable(container.Track{Codec: o.codecID}) {
			t.Errorf("%q is advertised for cut but its codec %v is not Cuttable", name, o.codecID)
		}
		if !o.live {
			t.Errorf("%q is advertised for cut but has no live progressive form", name)
		}
		if !segmented[name] {
			t.Errorf("%q is advertised for cut but has no segmented (HLS) form", name)
		}
	}

	// Divergence guard: the set of cuttable output formats that are live must
	// equal the set that are segmented. Today both are {opus, aac}. The day a
	// cuttable codec lands that is live-but-not-segmented (or the reverse), this
	// fires, forcing a conscious choice (split cutFormats per surface, or accept
	// the conservative single-surface exclusion CutFormats already makes) rather
	// than silently over- or under-advertising one surface.
	var cuttableLive, cuttableSegmented []string
	for _, o := range outputs {
		if _, ok := cutCodecs[o.codecID]; !ok {
			continue
		}
		if o.live {
			cuttableLive = append(cuttableLive, o.name)
		}
		if o.hls != nil {
			cuttableSegmented = append(cuttableSegmented, o.name)
		}
	}
	if !slices.Equal(cuttableLive, cuttableSegmented) {
		t.Errorf("cuttable formats diverge across surfaces: live=%v segmented=%v; "+
			"CutFormats() advertises only the intersection, so one surface is now under- or over-served",
			cuttableLive, cuttableSegmented)
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
	demux, err := Cut(&gridDemuxer{n: 94, dur: 1024}, track, TranscodeOptions{}, spans, 1024)
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
	// Windows: [0, 20480) is source packets 0..19, and [40960, 61440) is
	// 40..59. Both spans are on the grid already, so the interior edges snap
	// to themselves and no pre-roll is prepended: the second window starts at
	// the packet the caller asked for, not one before it.
	if len(got) != 20+20 {
		t.Fatalf("kept %d packets, want %d", len(got), 20+20)
	}
	for i := range 20 {
		if got[i] != int64(i) {
			t.Errorf("kept[%d] = source packet %d, want %d", i, got[i], i)
		}
	}
	for i := range 20 {
		if want := int64(40 + i); got[20+i] != want {
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
	demux, err := Cut(&gridDemuxer{n: 96, dur: 1000}, track, TranscodeOptions{}, []Span{{2048, 20480}}, 1024)
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

// warnerDemuxer is a Demuxer that records warnings, for the forwarding pin.
type warnerDemuxer struct {
	container.Demuxer
	ws []container.Warning
}

func (w *warnerDemuxer) Warnings() []container.Warning { return w.ws }

// TestCutDemuxerForwardsWarnings pins that the cut view exposes its source's
// container.Warner: embedding the Demuxer interface promotes nothing outside
// it, and the copy rung's input-damage list is read through that assertion.
func TestCutDemuxerForwardsWarnings(t *testing.T) {
	ws := []container.Warning{{Offset: 7, Msg: "16 unparsable bytes skipped", Kind: container.Damage}}
	c := &cutDemuxer{Demuxer: &warnerDemuxer{ws: ws}}
	var w container.Warner = c
	if got := w.Warnings(); len(got) != 1 || got[0] != ws[0] {
		t.Errorf("Warnings = %v, want the source's %v", got, ws)
	}
	if got := (&cutDemuxer{Demuxer: &warnerDemuxer{}}).Warnings(); len(got) != 0 {
		t.Errorf("Warnings = %v on a source with none", got)
	}
}

// TestSpliceTrimsWalksThePrerollAndDiscardsIt pins the opt-in's arithmetic:
// every interior span gains the codec's pre-roll back, and every sample of it
// is accounted for as a trim, so nothing the caller cut away is heard.
func TestSpliceTrimsWalksThePrerollAndDiscardsIt(t *testing.T) {
	track := opusTrack(312, 96000)
	spans := []Span{{0, 20000}, {40000, 60000}}
	opts := TranscodeOptions{SpliceTrims: true}
	res, err := computeCut(track, opts, spans, 960)
	if err != nil {
		t.Fatal(err)
	}
	cut := res.track
	// Span 1's splice point is snapUp(40312) = 40320, and the walk starts a
	// pre-roll (3840, four packets) before it.
	if got, want := res.windows[1].from, int64(40320-3840); got != want {
		t.Errorf("window 1 starts at %d, want %d (the splice less the pre-roll)", got, want)
	}
	// Four whole-packet discards at the head of window 1, then the exact tail
	// slop on window 0's last kept packet.
	if len(cut.MidTrims) != 5 {
		t.Fatalf("MidTrims = %v, want four pre-roll discards and one tail slop", cut.MidTrims)
	}
	if !cut.MidTrimsComplete() {
		t.Error("the cut's trims do not add up to its MidPadding")
	}
	// Window 0 runs [0, snapUp(20312)) = [0, 21120); the slop past the
	// requested end is 21120-20312 = 808.
	if got := cut.MidTrims[0]; got != (container.PacketTrim{Pos: 21120 - 808, Samples: 808}) {
		t.Errorf("first trim = %v, want the 808-sample tail slop of window 0", got)
	}
	for i := 1; i < 5; i++ {
		want := container.PacketTrim{Pos: 21120 + int64(i-1)*960, Samples: 960}
		if cut.MidTrims[i] != want {
			t.Errorf("trim %d = %v, want %v (a whole pre-roll packet)", i, cut.MidTrims[i], want)
		}
	}
	// The interior tail is exact now, and the head still snaps inward.
	if res.landed[0].To != 20000 {
		t.Errorf("Landed[0].To = %d, want the requested 20000 exactly", res.landed[0].To)
	}
	if res.landed[1].From != 40320-312 {
		t.Errorf("Landed[1].From = %d, want %d (the splice on the track timeline)", res.landed[1].From, 40320-312)
	}
	// The keystone survives: remuxTrailer still yields exactly the padding.
	var delivered int64
	for _, w := range res.windows {
		delivered += w.to - w.from
	}
	tr := remuxTrailer(cut, copiedRun{samples: delivered - cut.MidPadding})
	if tr.Padding != cut.Padding {
		t.Errorf("remuxTrailer padding = %d, want %d", tr.Padding, cut.Padding)
	}
	if tr.Samples != cut.Samples {
		t.Errorf("remuxTrailer samples = %d, want %d", tr.Samples, cut.Samples)
	}
}

// TestSpliceTrimsIsIgnoredForOneSpan: a single-span cut has no interior edge,
// so the option must not narrow it to Matroska for nothing.
func TestSpliceTrimsIsIgnoredForOneSpan(t *testing.T) {
	track := opusTrack(312, 96000)
	plain, _, err := CutTrack(track, TranscodeOptions{}, []Span{{48000, 72000}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	spliced, _, err := CutTrack(track, TranscodeOptions{SpliceTrims: true}, []Span{{48000, 72000}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	if spliced.MidPadding != 0 {
		t.Errorf("MidPadding = %d on a single-span cut; the option has nothing to do there", spliced.MidPadding)
	}
	if spliced.Samples != plain.Samples || spliced.Delay != plain.Delay || spliced.Padding != plain.Padding {
		t.Errorf("spliced = {%d %d %d}, plain = {%d %d %d}",
			spliced.Delay, spliced.Padding, spliced.Samples, plain.Delay, plain.Padding, plain.Samples)
	}
}

// TestCutTrackJoinsAdjacentSpansWithoutAHole: spans that touch name one
// range in two pieces, so there is nothing between them to remove. An
// off-grid boundary must not cost the packet it sits in, which is what two
// independent inward snaps would do -- and which the rung used to avoid only
// by declining the whole request.
func TestCutTrackJoinsAdjacentSpansWithoutAHole(t *testing.T) {
	track := aacTrack(0, 96000)
	for _, tc := range []struct {
		name  string
		spans []Span
		want  int64 // the union's length, which the cut must deliver whole
	}{
		{"off the grid", []Span{{0, 20000}, {20000, 40000}}, 40000},
		{"on the grid", []Span{{0, 20480}, {20480, 40960}}, 40960},
		{"a piece under one packet", []Span{{0, 1000}, {1000, 20000}}, 20000},
		{"three pieces", []Span{{0, 20000}, {20000, 40000}, {40000, 60000}}, 60000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cut, landed, err := CutTrack(track, TranscodeOptions{}, tc.spans, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if cut.Samples != tc.want {
				t.Errorf("Samples = %d, want the whole %d: a touching pair has no gap to snap into",
					cut.Samples, tc.want)
			}
			// The seams are contiguous and the ends exact, so the pieces
			// still add up to the request one for one.
			if landed[0].From != tc.spans[0].From {
				t.Errorf("Landed[0].From = %d, want %d", landed[0].From, tc.spans[0].From)
			}
			last := len(landed) - 1
			if landed[last].To != tc.spans[last].To {
				t.Errorf("Landed[%d].To = %d, want %d", last, landed[last].To, tc.spans[last].To)
			}
			for i := 1; i <= last; i++ {
				if landed[i].From != landed[i-1].To {
					t.Errorf("Landed[%d].From = %d but Landed[%d].To = %d: the seam has a hole",
						i, landed[i].From, i-1, landed[i-1].To)
				}
			}
		})
	}
}
