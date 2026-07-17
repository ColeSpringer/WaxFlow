package server_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// writeADTSFixture transcodes a WAV in the env's root into a raw ADTS AAC-LC
// file beside it and returns its library ref. ADTS declares no stream length,
// which is the one cuttable case where the header track's Samples is -1: the
// corpus has no such fixture, and the cut's length-threading needs one.
func writeADTSFixture(t *testing.T, env *testEnv, from, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.root, from))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: "aac", Container: "adts"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, name), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/" + name
}

// TestCutServesTheProgressiveRung drives A15's new ladder rung end to end: a
// from/to span on an Opus source served by moving the source's own packets
// rather than re-encoding them.
//
// The proof it took the cut rung and not rung 3 is the metric, and it has to be:
// rung 3 produces a correct, playable span too, which is exactly why remux.go
// asserts a counter rather than bytes. The byte-identity check is the second
// half of the claim: the kept payloads are the source's own Opus packets, which
// no re-encode could reproduce.
func TestCutServesTheProgressiveRung(t *testing.T) {
	env := newTestEnv(t, nil)
	// ramp.wav is 4 s at 48 kHz; the Opus fixture inherits that length, and the
	// grid is 960 (20 ms), so cut points that are whole multiples of 960 land on
	// packet boundaries with no snap slop to hide.
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")
	before := env.srv.Metrics().Cuts.Load()

	// Keep [48000, 144000): drop the first and last seconds. format=opus is
	// load-bearing, not decorative: the cut is reached only with a format the
	// source codec matches (format=auto resolves to wav here and would transcode).
	resp := env.get(t, "/stream?src="+ref+"&from=48000&to=144000&format=opus", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("cut request = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/ogg" {
		t.Fatalf("Content-Type = %q, want audio/ogg", ct)
	}
	// A live cut is a fresh stream, not the file: no byte ranges.
	if resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("live cut Accept-Ranges = %q, want none", resp.Header.Get("Accept-Ranges"))
	}
	// The body is a well-formed Ogg stream.
	if len(body) < 4 || string(body[:4]) != "OggS" {
		t.Fatal("cut output does not begin with an Ogg capture pattern")
	}
	if got := env.srv.Metrics().Cuts.Load(); got != before+1 {
		t.Fatalf("cut_total = %d, want %d: the request took a rung other than the cut", got, before+1)
	}

	// The kept payloads must be the source's own packets, in order, byte for
	// byte. The head backs off by the codec pre-roll, so the emitted run starts a
	// few packets before sample 48000; those are still source packets (the Delay
	// trims them on playback), so a walk that finds each emitted packet somewhere
	// ahead in the source, in order, is the right check. Comparing against a
	// prefix would pass on a cut that only dropped the tail.
	srcRaw, err := os.ReadFile(filepath.Join(env.root, "ramp.opus"))
	if err != nil {
		t.Fatal(err)
	}
	want := hlsPayloads(t, container.BytesSource(srcRaw), "opus")
	got := hlsPayloads(t, container.BytesSource(body), "opus")
	if len(got) == 0 {
		t.Fatal("the cut emitted no packets")
	}
	if len(got) >= len(want) {
		t.Fatalf("the cut emitted %d of the source's %d packets; nothing was dropped", len(got), len(want))
	}
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
}

// TestCutDeclinedFallsThrough pins the other side of the rung: a span the cut
// cannot serve must fall through to a transcode with no cut counted, exactly as
// remux does. Two declines, each a distinct reason:
//
//   - a FLAC span: FLAC is lossless and off the cut allowlist, so the rung never
//     applies however cleanly the packets would move.
//   - an Opus span under format=auto: auto resolves to the first live output
//     (wav), whose codec the source is not, so PlanRemux inside PlanCut declines.
//     This is the reachability fact the API doc surfaces: the cut needs a
//     source-matching format, not auto.
func TestCutDeclinedFallsThrough(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")

	for _, tc := range []struct{ name, query string }{
		{"flac-not-cuttable", "/stream?src=lib/album/track.flac&format=flac&to=10000"},
		{"opus-format-auto", "/stream?src=" + ref + "&from=48000&to=144000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := env.srv.Metrics().Cuts.Load()
			resp := env.get(t, tc.query, nil)
			if body := readBody(t, resp); resp.StatusCode != 200 {
				t.Fatalf("request = %d, want 200 from rung 3: %s", resp.StatusCode, body)
			}
			if got := env.srv.Metrics().Cuts.Load(); got != before {
				t.Fatalf("cut_total moved to %d: the cut rung took a request it must decline", got)
			}
		})
	}
}

// TestCutSwallowsStricterRefusal pins the cut rung's critical swallow: a span
// that SpanTrack accepts but the cut's own stricter validateCutSpans refuses
// must NOT surface as a 400. The reachable case is a from at exactly the source
// length: SpanTrack reads it as a zero-sample virtual track (a valid empty
// stream), while validateCutSpans rejects a span that keeps no samples, so
// PlanCut returns an error rather than a clean decline. cutPlanFor swallows that
// error and falls through, so the request is served as the empty 200 rung 3
// gives it today. Propagating instead would regress it to a new 400.
func TestCutSwallowsStricterRefusal(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")
	// Derive the exact gapless length rather than assume it: the from must land
	// exactly on the end for SpanTrack to admit it, and one sample either way
	// takes a different path (a live cut, or a 400 from SpanTrack itself).
	srcRaw, err := os.ReadFile(filepath.Join(env.root, "ramp.opus"))
	if err != nil {
		t.Fatal(err)
	}
	total := decodePCM(t, srcRaw).N
	before := env.srv.Metrics().Cuts.Load()

	resp := env.get(t, "/stream?src="+ref+"&from="+strconv.Itoa(total)+"&format=opus", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("from==length span = %d, want 200 (an empty stream from rung 3, not a 400): %s",
			resp.StatusCode, body)
	}
	if got := env.srv.Metrics().Cuts.Load(); got != before {
		t.Fatalf("cut_total moved to %d: the cut rung served a span its own validation refuses", got)
	}
}

// TestCutThreadsMeasuredLengthForUndeclaredSource pins the length-threading: an
// ADTS AAC-LC source declares no length (its header track's Samples is -1), so
// the cut PLAN measures it while the RUN opens the source fresh and would see
// -1. If the run used the header's -1, its fMP4 init segment would declare an
// unknown duration, contradicting the plan's advertised X-Content-Duration and
// breaking a player's seek bar. cutPlanFor threads the measured length to the
// run through req.cutSamples so the served stream declares the length the plan
// promised.
//
// A ToEnd span is the discriminating case: only there does the header's -1 flow
// into the run's synthesized track length (a bounded cut computes its own finite
// length regardless). from>0 keeps the cut from declining on an unanswerable
// tail.
func TestCutThreadsMeasuredLengthForUndeclaredSource(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeADTSFixture(t, env, "ramp.wav", "ramp.aac")
	before := env.srv.Metrics().Cuts.Load()

	// [24576, ToEnd): 24576 is 24 grids of 1024, so it lands on a packet.
	resp := env.get(t, "/stream?src="+ref+"&from=24576&format=aac", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("adts cut = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mp4" {
		t.Fatalf("Content-Type = %q, want audio/mp4", ct)
	}
	if got := env.srv.Metrics().Cuts.Load(); got != before+1 {
		t.Fatalf("cut_total = %d, want %d: the ADTS span did not take the cut rung", got, before+1)
	}

	// The served fMP4 must declare its own length, not the header's -1. Demuxing
	// it back and reading the track length is the whole check: -1 here is the bug
	// this test exists for.
	_, info, err := format.OpenDemuxer(container.BytesSource(body), "mp4", nil)
	if err != nil {
		t.Fatalf("served body is not a demuxable mp4: %v", err)
	}
	got := info.Default()
	if got.Samples <= 0 {
		t.Fatalf("served fMP4 declares Samples=%d: the run used the header's -1 instead of the plan's measured length", got.Samples)
	}
	// And it must agree with the advertised duration, so the fMP4's own clock and
	// X-Content-Duration cannot tell a player two different stories.
	if dur := resp.Header.Get("X-Content-Duration"); dur != "" {
		wantSecs, err := strconv.ParseFloat(dur, 64)
		if err != nil {
			t.Fatalf("X-Content-Duration %q: %v", dur, err)
		}
		gotSecs := float64(got.Samples) / float64(got.Fmt.Rate)
		if diff := gotSecs - wantSecs; diff > 0.05 || diff < -0.05 {
			t.Fatalf("fMP4 length %.3fs disagrees with X-Content-Duration %.3fs", gotSecs, wantSecs)
		}
	}
}
