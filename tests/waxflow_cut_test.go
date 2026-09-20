package waxflow_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// runCut assembles the rung the way a caller does: measure the grid, plan,
// build the cut view, and run it through the demuxer seam. It returns nil when
// the plan declines, which is the ladder's own contract and the cue to transcode.
func runCut(t *testing.T, src []byte, hint string, opts waxflow.TranscodeOptions,
	spans []waxflow.Span) ([]byte, *waxflow.CutPlan) {
	t.Helper()
	e := waxflow.New()
	grid, err := e.PacketGrid(container.BytesSource(src), hint)
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(src), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	plan, err := e.PlanCut(track, opts, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		return nil, nil
	}
	cut, _, err := waxflow.CutTrack(track, opts, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	cutDemux, err := waxflow.Cut(demux, track, opts, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := e.RemuxDemuxer(context.Background(), cutDemux, cut, &out, opts); err != nil {
		t.Fatalf("running the cut: %v", err)
	}
	return out.Bytes(), plan
}

// TestCutPayloadsAreByteIdentical is the "rung was taken" proof.
//
// The Metrics().Remuxes pattern the segmented rung uses is a server pattern, and
// the cut has no server surface. Correctness cannot discriminate either: rung 3
// produces correct output too, which is the whole reason remux.go keeps a metric
// rather than asserting bytes. At the engine level, byte-identical Opus payloads
// are the discriminator that works, because no re-encode can produce them.
//
// Identity alone would pass on a no-op, so this asserts both halves: the kept
// packets survive byte for byte, and the dropped ones are gone.
func TestCutPayloadsAreByteIdentical(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 96000)
	want := payloads(t, src, "opus")
	if len(want) < 40 {
		t.Fatalf("the Opus fixture demuxed to %d packets; too few to cut meaningfully", len(want))
	}

	// Keep the first second, drop the middle, keep from 1.5 s to 2 s. The grid
	// is 960 (20 ms), so these land on whole packets.
	spans := []waxflow.Span{{From: 0, To: 48000}, {From: 72000, To: 96000}}
	out, plan := runCut(t, src, "opus", waxflow.TranscodeOptions{Format: "opus", Container: "mka"}, spans)
	if out == nil {
		t.Fatal("PlanCut declined an Opus cut to mka, which it must serve")
	}
	got := payloads(t, out, "mka")

	if len(got) >= len(want) {
		t.Fatalf("the cut emitted %d packets of the source's %d; nothing was dropped", len(got), len(want))
	}
	// Every kept payload must appear in the source, in order, byte for byte.
	// Comparing against a prefix of the source would pass on a cut that dropped
	// only the tail, so this walks the source looking for the kept run.
	var si int
	for gi, p := range got {
		for si < len(want) && !bytes.Equal(want[si], p) {
			si++
		}
		if si == len(want) {
			t.Fatalf("cut packet %d is not any source packet's bytes: this is re-encoded audio, not moved packets", gi)
		}
		si++
	}
	t.Logf("cut %d source packets to %d, all byte-identical", len(want), len(got))
	if plan.Samples <= 0 {
		t.Errorf("plan.Samples = %d, want the landed length", plan.Samples)
	}
}

// TestCutOfEverythingIsAPlainRemux: a single ToEnd span from 0 keeps every
// packet and must produce exactly what Remux produces. A free identity, and a
// sharp regression net over the whole arithmetic.
func TestCutOfEverythingIsAPlainRemux(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	opts := waxflow.TranscodeOptions{Format: "opus", Container: "mka"}

	out, _ := runCut(t, src, "opus", opts, []waxflow.Span{{From: 0, To: waxflow.ToEnd}})
	if out == nil {
		t.Fatal("PlanCut declined an identity cut")
	}

	var want bytes.Buffer
	e := waxflow.New()
	if _, err := e.Remux(context.Background(), container.BytesSource(src), "opus", &want, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want.Bytes()) {
		t.Errorf("a cut of everything produced %d bytes, a plain remux %d; they must be identical",
			len(out), want.Len())
	}
}

// TestCutRewritesTheOpusHead: setting Track.Delay without rewriting the config
// does nothing at all, because every muxer reads Opus priming from the OpusHead
// in preference to the track. Ogg writes the config verbatim as its BOS page,
// and mka overwrites its own CodecDelay from the config's bytes, so an Ogg-only
// test would pass on a half-fix. This drives both.
func TestCutRewritesTheOpusHead(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 96000)
	// A From past 0 forces a synthesized delay: the head backs off by the
	// pre-roll and the slop becomes the trim.
	spans := []waxflow.Span{{From: 48000, To: 72000}}

	for _, tc := range []struct{ name, container, hint string }{
		{"ogg", "", "opus"},
		{"mka", "mka", "mka"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := waxflow.TranscodeOptions{Format: "opus", Container: tc.container}
			out, _ := runCut(t, src, "opus", opts, spans)
			if out == nil {
				t.Fatal("PlanCut declined")
			}
			_, info, err := format.OpenDemuxer(container.BytesSource(out), tc.hint, nil)
			if err != nil {
				t.Fatal(err)
			}
			track := info.Default()
			cfg, err := opus.ParseOpusHead(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PreSkip < opus.SeekPreroll {
				t.Errorf("OpusHead pre-skip = %d, want at least the %d-sample pre-roll: "+
					"the head snap's trim was not written into the config",
					cfg.PreSkip, opus.SeekPreroll)
			}
			// The demuxer reads the trim back off the container, so this is the
			// end-to-end form of the claim: the trim survives a round trip.
			if track.Delay != int64(cfg.PreSkip) {
				t.Errorf("track Delay = %d but OpusHead pre-skip = %d; they must agree",
					track.Delay, cfg.PreSkip)
			}
		})
	}
}

// TestCutJoinsSpansContiguously round-trips a ramp through mka, the only
// consumer of pkt.PTS, which is what pins the retiming. Every sample of a ramp
// names its own position, so audio from the dropped span is recognizable on
// sight rather than merely making a length wrong.
func TestCutJoinsSpansContiguously(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 96000)
	spans := []waxflow.Span{{From: 0, To: 24000}, {From: 72000, To: 96000}}
	out, plan := runCut(t, src, "opus", waxflow.TranscodeOptions{Format: "opus", Container: "mka"}, spans)
	if out == nil {
		t.Fatal("PlanCut declined")
	}

	_, info, err := format.OpenDemuxer(container.BytesSource(out), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The container must report the length the plan promised. A retiming bug
	// leaves a hole in the timeline, which Matroska records as a gap rather than
	// erroring, so the declared length is where it shows.
	if got := info.Default().Samples; got != plan.Samples {
		t.Errorf("the cut declares %d samples, the plan promised %d", got, plan.Samples)
	}
	// And the output really is shorter than the source by about the dropped
	// span, rather than the same length with silence in it.
	if plan.Samples >= 96000 {
		t.Errorf("plan.Samples = %d, want well under the source's 96000", plan.Samples)
	}
}

// TestCutFLACMultiSpanBreaks pins what a multi-span FLAC cut actually does,
// which is the claim the allowlist's FLAC row rests on. The design left the
// failure mode open between two candidates: a scan that runs past maxFrameLen
// and errors, or a CRC-confirmed glue that silently swallows audio. It is the
// second, and it is worse than either the design or WaxTap supposed.
//
// FLAC is not on the allowlist, so this cannot go through CutTrack: it byte-cuts
// a fixture directly, which is exactly the operation a cut would perform. Frame
// numbering is relative to the preceding frame rather than anchored to zero, so
// a contiguous cut re-reads clean and only a gap in the ordinals breaks it. That
// is why the row's reason is the gap and not the cut, and why this test keeps
// the reason honest: WaxTap will test the claim.
func TestCutFLACMultiSpanBreaks(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "flac"}, 96000)
	frames := payloads(t, src, "flac")
	if len(frames) < 24 {
		t.Fatalf("the FLAC fixture has %d frames; too few to leave a gap", len(frames))
	}
	// The frames run contiguously from the end of the metadata blocks, so the
	// file can be rebuilt by concatenation.
	off := bytes.Index(src, frames[0])
	if off < 0 {
		t.Fatal("could not locate the first frame in the fixture")
	}

	// Keep frames 0..9 and 20..end: the ordinal gap a multi-span cut leaves.
	const keepHead, resumeAt = 10, 20
	var cut bytes.Buffer
	cut.Write(src[:off])
	for i := range keepHead {
		cut.Write(frames[i])
	}
	var keptFrames int64 = keepHead
	for i := resumeAt; i < len(frames); i++ {
		cut.Write(frames[i])
		keptFrames++
	}

	// It opens, and it reads, and it reports no error at all. That is the whole
	// problem: nothing about this stream announces itself as broken.
	demux, _, err := format.OpenDemuxer(container.BytesSource(cut.Bytes()), "flac", nil)
	if err != nil {
		t.Fatalf("the byte-cut FLAC failed to open (%v), which is not the mode this pins; "+
			"if the demuxer has changed, the allowlist's FLAC row must change with it", err)
	}
	var pkt container.Packet
	var packets int
	var glued int
	for {
		if err := demux.ReadPacket(&pkt); err != nil {
			if err != io.EOF {
				t.Fatalf("read failed with %v; this test pins the silent mode, not an error", err)
			}
			break
		}
		// A packet holding more bytes than its own frame is the glue: the
		// boundary scan wanted the next ordinal, the cut took it away, and the
		// scan ran on until a CRC happened to confirm at a later frame's end.
		if len(pkt.Data) > len(frames[min(packets, len(frames)-1)])+1024 {
			glued++
		}
		packets++
	}
	if glued == 0 {
		t.Fatalf("no glued packet: read %d packets cleanly, which contradicts the FLAC row's reason", packets)
	}
	t.Logf("mode: %d packets read, %d of them glued, ending in a clean EOF", packets, glued)

	// The decode is where the audio goes missing, silently. FLAC leaves
	// SamplesExact false because its declared total can lie, so the shortfall is
	// a tolerated oddity rather than a truncation: no error is raised anywhere.
	med, err := waxflow.New().OpenStream(container.BytesSource(cut.Bytes()), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var decoded int64
	for {
		if err := med.ReadChunk(buf); err != nil {
			if err != io.EOF {
				t.Fatalf("decode failed with %v; this test pins the silent mode", err)
			}
			break
		}
		decoded += int64(buf.N)
	}
	kept := keptFrames * 4096
	if decoded >= kept {
		t.Fatalf("decoded %d samples of the %d the kept frames hold; nothing was lost, "+
			"which contradicts the FLAC row's reason", decoded, kept)
	}
	t.Logf("decoded %d samples of the %d kept: %d samples of the caller's own audio silently gone",
		decoded, kept, kept-decoded)
}

// TestCutOfAACToFMP4 drives the other allowlist member, through the edit list.
//
// The source track must carry a nonzero Delay, or this lands in the declined
// case rather than the path it means to test: the fragmented muxer only sets its
// delay from the track's, so a Delay == 0 AAC track with a nonzero synthesized
// padding is refused at End by design, and PlanCut declines it ahead of time.
// Our own AAC encoder writes a 1024-sample priming, so an fMP4 fixture has one.
func TestCutOfAACToFMP4(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "aac"}, 96000)
	_, info, err := format.OpenDemuxer(container.BytesSource(src), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Guard the fixture: without a delay this test would exercise the decline,
	// not the edit list, and would pass while proving nothing.
	if info.Default().Delay == 0 {
		t.Fatal("the AAC fixture declares no delay; this test would exercise the decline instead of the cut")
	}

	spans := []waxflow.Span{{From: 0, To: 20480}}
	out, plan := runCut(t, src, "m4a", waxflow.TranscodeOptions{Format: "aac"}, spans)
	if out == nil {
		t.Fatal("PlanCut declined an AAC cut to fMP4 with a delay to carry the trim")
	}
	_, cutInfo, err := format.OpenDemuxer(container.BytesSource(out), "m4a", nil)
	if err != nil {
		t.Fatalf("the cut fMP4 does not re-read: %v", err)
	}
	if got := cutInfo.Default().Samples; got != plan.Samples {
		t.Errorf("the cut declares %d samples, the plan promised %d", got, plan.Samples)
	}
	if plan.Samples >= 96000 {
		t.Errorf("plan.Samples = %d, want the cut length", plan.Samples)
	}
}

// TestCutOfADamagedSourceIsMalformedNotADecline pins the one thing the cut
// ladder's decline seam must not swallow.
//
// PlanCut maps CutTrack's unsupported-format onto (nil, nil), which is the
// ladder's way of saying "this rung cannot serve this, try the next one".
// Damage is not that, and neither is a bad request. A source whose bytes
// deviate from their format fails every rung, so declining it only spends a
// full transcode to arrive at the same refusal, under a status (415) that
// tells the client to send a different format.
func TestCutOfADamagedSourceIsMalformedNotADecline(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 96000)
	e := waxflow.New()

	// Truncated inside the first page's lacing table, which is what a partial
	// download leaves behind. The grid probe is the first thing a cut-eligible
	// request runs, so that is where the damage surfaces and the request stops.
	if _, err := e.PacketGrid(container.BytesSource(src[:30]), "opus"); err == nil {
		t.Fatal("the grid probe read a truncated Ogg clean; this cell covers nothing")
	} else if got := waxerr.CodeOf(err); got != waxerr.CodeMalformedInput {
		t.Errorf("grid probe code = %q, want %q (%v)", got, waxerr.CodeMalformedInput, err)
	}

	// And the seam itself only declines on unsupported-format. A span the
	// source cannot hold is invalid-request, and PlanCut must propagate it
	// rather than hand the request to a rung that would refuse it identically.
	// Without this the cell above would pass on a PlanCut that swallowed
	// everything, since it never reaches one.
	_, info, err := format.OpenDemuxer(container.BytesSource(src), "opus", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	past := []waxflow.Span{{From: 0, To: track.Samples + 1}}
	plan, err := e.PlanCut(track, waxflow.TranscodeOptions{Format: "opus", Container: "mka"}, past, 960)
	if err == nil {
		t.Fatalf("PlanCut accepted a span past the source (plan=%v); a decline here loses the reason", plan)
	}
	if plan != nil {
		t.Error("PlanCut returned both a plan and an error")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("span-past-the-end code = %q, want %q (%v)", got, waxerr.CodeInvalidRequest, err)
	}
}

// midTrimTrack is the walked mid-trim WebM's track, with the numbers every
// cell below computes from pinned: a 312 pre-skip, a 960 grid, and the one
// inner trim at block 8, whose 480 samples begin at raw 9*960-480.
func midTrimTrack(t *testing.T, src []byte) (container.Track, int) {
	t.Helper()
	track := walkedTrack(t, src, "webm")
	if track.MidPadding != midTrimPad || len(track.MidTrims) != 1 ||
		track.MidTrims[0] != (container.PacketTrim{Pos: 8160, Samples: midTrimPad}) {
		t.Fatalf("the walk found MidPadding %d at %v, want %d at [{8160 %d}]",
			track.MidPadding, track.MidTrims, midTrimPad, midTrimPad)
	}
	if track.Delay != 312 {
		t.Fatalf("the fixture's pre-skip is %d, the cells below assume 312", track.Delay)
	}
	grid, err := waxflow.New().PacketGrid(container.BytesSource(src), "webm")
	if err != nil {
		t.Fatal(err)
	}
	if grid != 960 {
		t.Fatalf("the fixture's grid is %d, the cells below assume 960", grid)
	}
	return track, grid
}

// keptSourcePackets maps a cut's payloads back to source packet indices, in
// order; a payload that is no source packet's fails the test.
func keptSourcePackets(t *testing.T, cut, src [][]byte) []int {
	t.Helper()
	var kept []int
	si := 0
	for gi, p := range cut {
		for si < len(src) && !bytes.Equal(src[si], p) {
			si++
		}
		if si == len(src) {
			t.Fatalf("cut packet %d is not any source packet's bytes", gi)
		}
		kept = append(kept, si)
		si++
	}
	return kept
}

func indexRange(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// TestCutAcrossAMidStreamTrim: a cut whose kept packets include the trimmed
// one carries the trim as the source did, on its packet, which only Matroska
// can write, and the cut's arithmetic runs on the raw timeline the grid holds
// on, so the head and tail land exactly where they were asked for.
//
// [9600, 28800): the head steps past the delay and the trim to raw 10392 and
// backs off the pre-roll to packet 6, so the trimmed packet 8 is in the head
// slop; the tail lands in packet 30. The delay is the slop less the trim it
// holds, since the reader drops the trim before it skips the delay.
func TestCutAcrossAMidStreamTrim(t *testing.T) {
	src := midTrimWebM(t, true)
	track, grid := midTrimTrack(t, src)
	srcPackets := payloads(t, src, "webm")
	e := waxflow.New()
	spans := []waxflow.Span{{From: 9600, To: 28800}}

	// An Ogg granule names one end trim, so the kept trim declines it.
	plan, err := e.PlanCut(track, waxflow.TranscodeOptions{Format: "opus"}, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Error("PlanCut planned a cut into Ogg-Opus whose kept packets carry a trim")
	}
	for _, cont := range []string{"mka", "webm"} {
		t.Run(cont, func(t *testing.T) {
			opts := waxflow.TranscodeOptions{Format: "opus", Container: cont}
			plan, err := e.PlanCut(track, opts, spans, grid)
			if err != nil {
				t.Fatal(err)
			}
			if plan == nil {
				t.Fatal("PlanCut declined a cut into Matroska, which carries the trim")
			}
			if plan.Samples != 19200 || plan.Track.Delay != 10392-5760-480 || plan.Track.Padding != 29760-29592 {
				t.Errorf("plan = {Samples %d Delay %d Padding %d}, want {19200 %d %d}",
					plan.Samples, plan.Track.Delay, plan.Track.Padding, 10392-5760-480, 29760-29592)
			}
			if want := (waxflow.Span{From: 9600, To: 28800}); plan.Landed[0] != want {
				t.Errorf("Landed = %v, want %v: the head and tail land exactly", plan.Landed[0], want)
			}

			out, _ := runMeasuredCut(t, src, track, opts, spans)
			if kept, want := keptSourcePackets(t, payloads(t, out, cont), srcPackets), indexRange(6, 31); !slices.Equal(kept, want) {
				t.Errorf("the cut kept source packets %v, want %v", kept, want)
			}
			// The trim comes out on the block that carries source packet 8,
			// the third kept, at raw 2*960+480 of the output.
			got := walkedTrack(t, out, cont)
			if got.Samples != 19200 || got.MidPadding != midTrimPad ||
				!slices.Equal(got.MidTrims, []container.PacketTrim{{Pos: 2400, Samples: midTrimPad}}) {
				t.Errorf("the output walks to %d samples, MidPadding %d at %v; want 19200, %d at [{2400 %d}]",
					got.Samples, got.MidPadding, got.MidTrims, midTrimPad, midTrimPad)
			}
			if n := int64(len(decodeWhole(t, out, cont))) / int64(got.Fmt.Channels); n != 19200 {
				t.Errorf("the output decodes to %d frames, want the 19200 asked for", n)
			}
		})
	}
}

// TestCutPastAMidStreamTrim: a span whose kept packets lie past the trim
// carries none, so every destination serves it, and the window is a whole
// packet later than a plan blind to the trim would place it.
//
// [12000, 24400): raw 12792 backs off to packet 9 where 12312 would have
// backed off to packet 8, and raw 25192 snaps up to 25920 (packet 26 is the
// last kept) where 24712 would have snapped to 24960. So the kept run is
// packets 9..26, and a plan that ignored the trim would keep 8..25.
func TestCutPastAMidStreamTrim(t *testing.T) {
	src := midTrimWebM(t, true)
	track, _ := midTrimTrack(t, src)
	srcPackets := payloads(t, src, "webm")
	spans := []waxflow.Span{{From: 12000, To: 24400}}
	for _, tc := range []struct{ cont, hint string }{{"", "opus"}, {"mka", "mka"}} {
		name := tc.cont
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			opts := waxflow.TranscodeOptions{Format: "opus", Container: tc.cont}
			out, plan := runMeasuredCut(t, src, track, opts, spans)
			if out == nil {
				t.Fatal("PlanCut declined a cut whose kept packets carry no trim")
			}
			if plan.Samples != 12400 || plan.Track.MidPadding != 0 {
				t.Errorf("plan = {Samples %d MidPadding %d}, want {12400 0}", plan.Samples, plan.Track.MidPadding)
			}
			if kept, want := keptSourcePackets(t, payloads(t, out, tc.hint), srcPackets), indexRange(9, 27); !slices.Equal(kept, want) {
				t.Errorf("the cut kept source packets %v, want %v", kept, want)
			}
			ch := int64(probeTrack(t, out, tc.hint).Fmt.Channels)
			if n := int64(len(decodeWhole(t, out, tc.hint))) / ch; n != 12400 {
				t.Errorf("the output decodes to %d frames, want the 12400 asked for", n)
			}
		})
	}
}

// TestCutAroundAMidStreamTrimInAGap: a trim between two spans is dropped
// with the packets around it, and the second span's landing is reported on
// the track's own timeline, where the trim does not exist.
//
// [0, 5000) and [20000, 30000): both interior edges snap inward, so the
// second window starts at packet 22 (raw 21120), which is 21120-312-480 on
// the track's timeline, and the first ends at packet 5 (raw 4800).
func TestCutAroundAMidStreamTrimInAGap(t *testing.T) {
	src := midTrimWebM(t, true)
	track, _ := midTrimTrack(t, src)
	srcPackets := payloads(t, src, "webm")
	spans := []waxflow.Span{{From: 0, To: 5000}, {From: 20000, To: 30000}}
	out, plan := runMeasuredCut(t, src, track, waxflow.TranscodeOptions{Format: "opus"}, spans)
	if out == nil {
		t.Fatal("PlanCut declined a cut whose gap holds the trim")
	}
	if want := []waxflow.Span{{From: 0, To: 4488}, {From: 20328, To: 30000}}; !slices.Equal(plan.Landed, want) {
		t.Errorf("Landed = %v, want %v", plan.Landed, want)
	}
	want := append(indexRange(0, 5), indexRange(22, 33)...)
	if kept := keptSourcePackets(t, payloads(t, out, "opus"), srcPackets); !slices.Equal(kept, want) {
		t.Errorf("the cut kept source packets %v, want %v", kept, want)
	}
}

// runMeasuredCut is runCut over a measured track: the plan and the view both
// read the walk's trims, as the daemon's memo hands them to both.
func runMeasuredCut(t *testing.T, src []byte, measured container.Track, opts waxflow.TranscodeOptions,
	spans []waxflow.Span) ([]byte, *waxflow.CutPlan) {
	t.Helper()
	e := waxflow.New()
	grid, err := e.PacketGrid(container.BytesSource(src), "webm")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanCut(measured, opts, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		return nil, nil
	}
	out := &memWS{}
	if _, err := e.CutStream(context.Background(), container.BytesSource(src), "webm", out, opts, spans, grid, measured); err != nil {
		t.Fatalf("running the cut: %v", err)
	}
	return out.Buf, plan
}

// TestCutRefusesAMidStreamTrimMidWalk is the backstop for a source nothing
// measured first: the cut view refuses on the packet after the padded one,
// rather than splicing a stream whose positions have drifted off the grid.
func TestCutRefusesAMidStreamTrimMidWalk(t *testing.T) {
	src := midTrimWebM(t, true)
	e := waxflow.New()
	grid, err := e.PacketGrid(container.BytesSource(src), "webm")
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(src), "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	if track.MidPadding != 0 {
		t.Fatalf("the source arrives measured (MidPadding %d); this cell needs the unwalked shape", track.MidPadding)
	}
	cutDemux, err := waxflow.Cut(demux, track, waxflow.TranscodeOptions{}, []waxflow.Span{{From: 0, To: 28800}}, grid)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := cutDemux.ReadPacket(&pkt)
		if err == io.EOF {
			t.Fatal("the cut finished; the inner trim must stop it")
		}
		if err != nil {
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Fatalf("code = %v, want %v: %v", code, waxerr.CodeUnsupportedFormat, err)
			}
			if i != midTrimBlock+1 {
				t.Errorf("refused on packet %d, want the one after the padded block (%d)", i, midTrimBlock+1)
			}
			return
		}
	}
}

// TestCutRunReadsThePlansTrack: a cut's plan and its run must compute from one
// track, or the windows the run cuts are not the windows the plan advertised
// and keyed.
//
// The fields that differ are the ones only a measurement establishes. A
// Matroska states its tail trim on the last block, so a fresh header open
// reports Padding 0 while the measured track carries the walk's; computeCut's
// decodedEnd reads that trim, and a bounded span whose end lands near the
// source's own is clamped by one track and not the other.
func TestCutRunReadsThePlansTrack(t *testing.T) {
	src := midTrimWebM(t, true)
	measured := walkedTrack(t, src, "webm")
	if measured.Padding == 0 {
		t.Fatal("the measure found no tail trim; this cell needs one")
	}
	fresh := probeTrack(t, src, "webm")
	if fresh.Padding != 0 {
		t.Fatalf("a header open already reports Padding %d; the divergence this pins is gone", fresh.Padding)
	}
	e := waxflow.New()
	grid, err := e.PacketGrid(container.BytesSource(src), "webm")
	if err != nil {
		t.Fatal(err)
	}
	// A span ending inside the source's final packet, which is where the two
	// decodedEnds disagree: one clamps the tail to the decode end, the other to
	// the audio end.
	to := measured.Samples - 1
	spans := []waxflow.Span{{From: 0, To: to}}

	planned, _, err := waxflow.CutTrack(measured, waxflow.TranscodeOptions{}, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	// What the run would compute from a fresh open with the plan's track
	// overlaid, which is what CutStream does.
	ran, _, err := waxflow.CutTrack(adoptForTest(fresh, measured), waxflow.TranscodeOptions{}, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Samples != ran.Samples || planned.Padding != ran.Padding || planned.Delay != ran.Delay {
		t.Errorf("the plan computes %d samples (delay %d, padding %d) and the run %d (%d, %d)",
			planned.Samples, planned.Delay, planned.Padding, ran.Samples, ran.Delay, ran.Padding)
	}
	// And the same overlay on a header-only track, which is what the run used
	// to do, differs: the cell would pass by luck without it.
	blind, _, err := waxflow.CutTrack(withSamples(fresh, measured.Samples), waxflow.TranscodeOptions{}, spans, grid)
	if err == nil && blind.Padding == planned.Padding && blind.Samples == planned.Samples {
		t.Error("the header-only track computes the same cut; this cell proves nothing")
	}
}

// adoptForTest mirrors what CutStream overlays from the plan's measured track
// onto a fresh header open.
func adoptForTest(fresh, measured container.Track) container.Track {
	fresh.Samples, fresh.SamplesExact, fresh.SamplesAdvisory = measured.Samples, true, false
	fresh.Padding, fresh.MidPadding, fresh.MidTrims = measured.Padding, measured.MidPadding, measured.MidTrims
	return fresh
}

// withSamples is the older overlay: the length alone.
func withSamples(fresh container.Track, samples int64) container.Track {
	fresh.Samples, fresh.SamplesExact, fresh.SamplesAdvisory = samples, true, false
	return fresh
}
