package waxflow_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
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
	cut, _, err := waxflow.CutTrack(track, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	cutDemux, err := waxflow.Cut(demux, track, spans, grid)
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
