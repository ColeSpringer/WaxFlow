package server_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// unprimedEndTrim writes a Matroska AAC with no CodecDelay whose last block
// carries a DiscardPadding, and returns the samples it plays: the engine's
// own cut of an ADTS stream from sample 0 to an end off the packet grid.
func unprimedEndTrim(t *testing.T, path string) int64 {
	t.Helper()
	const kept = 50000
	ctx := context.Background()
	e := waxflow.New()
	wav := testutil.PCM16WAVBytes(t, 48000, testutil.ToneChans(48000, 96000))
	adts := &testutil.MemWriteSeeker{}
	if _, err := e.Transcode(ctx, container.BytesSource(wav), "wav", adts, waxflow.TranscodeOptions{Format: "aac", Container: "adts"}); err != nil {
		t.Fatal(err)
	}
	_, info, err := format.OpenDemuxer(container.BytesSource(adts.Buf), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := waxflow.TranscodeOptions{Format: "aac", Container: "mka"}
	spans := []waxflow.Span{{From: 0, To: kept}}
	plan, err := e.PlanCut(info.Default(), opts, spans,
		func() (int, error) { return e.PacketGrid(ctx, container.BytesSource(adts.Buf), "") })
	if err != nil || plan == nil {
		t.Fatalf("the cut to mka: %v, %v", plan, err)
	}
	mkaFile := &testutil.MemWriteSeeker{}
	if _, err := e.CutStream(ctx, container.BytesSource(adts.Buf), "", mkaFile, opts, spans, plan.Grid, plan.Source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mkaFile.Buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return kept
}

// TestStreamOfAnUnprimedEndTrimCopiesToFragmentedMP4: format=aac of a
// Matroska AAC with an end trim and no delay is served by the copy rung, the
// trim stated as the fMP4's length, and the entry completes.
func TestStreamOfAnUnprimedEndTrimCopiesToFragmentedMP4(t *testing.T) {
	env := newTestEnv(t, nil)
	want := unprimedEndTrim(t, filepath.Join(env.root, "endtrim.mka"))

	before := env.srv.Metrics().Remuxes.Load()
	resp := env.get(t, "/stream?src=lib/endtrim.mka&format=aac", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mp4" {
		t.Errorf("Content-Type = %q, want audio/mp4", ct)
	}
	if dur := resp.Header.Get("X-Content-Duration"); dur != "1.042" {
		t.Errorf("X-Content-Duration = %q, want 1.042", dur)
	}
	if got := env.srv.Metrics().Remuxes.Load(); got != before+1 {
		t.Errorf("remux_total = %d, want %d: the request took a rung other than 2", got, before+1)
	}
	if got := decodedBodyFrames(t, body, ""); got != want {
		t.Errorf("the body decodes to %d frames, want the trimmed %d", got, want)
	}
	// A failed entry never completes; a second request served from the
	// cache says this one did.
	again := env.get(t, "/stream?src=lib/endtrim.mka&format=aac", nil)
	if cached := readBody(t, again); again.StatusCode != 200 || !bytes.Equal(cached, body) {
		t.Errorf("the second request = %d with %d bytes, want the first's %d", again.StatusCode, len(cached), len(body))
	}
	if got := env.srv.Metrics().Remuxes.Load(); got != before+1 {
		t.Errorf("remux_total = %d after the second request, want %d: the entry was built again", got, before+1)
	}
}

// TestStreamOfAProgressiveEndTrimWithNoDelay: the same trim stated by a
// progressive MP4, as one edit from 0, is read at open, so the stream
// advertises and plays the trimmed length. /stream serves no progressive
// form, so the file is the source and the copy is to fragmented MP4.
func TestStreamOfAProgressiveEndTrimWithNoDelay(t *testing.T) {
	env := newTestEnv(t, nil)
	mkaPath := filepath.Join(env.root, "endtrim.mka")
	want := unprimedEndTrim(t, mkaPath)
	mkaFile, err := os.ReadFile(mkaPath)
	if err != nil {
		t.Fatal(err)
	}
	prog := &testutil.MemWriteSeeker{}
	if _, err := waxflow.New().Remux(context.Background(), container.BytesSource(mkaFile), "", prog,
		waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "endtrim.m4a"), prog.Buf, 0o644); err != nil {
		t.Fatal(err)
	}

	before := env.srv.Metrics().Remuxes.Load()
	resp := env.get(t, "/stream?src=lib/endtrim.m4a&format=aac", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d: %s", resp.StatusCode, body)
	}
	if dur := resp.Header.Get("X-Content-Duration"); dur != "1.042" {
		t.Errorf("X-Content-Duration = %q, want 1.042", dur)
	}
	if got := env.srv.Metrics().Remuxes.Load(); got != before+1 {
		t.Errorf("remux_total = %d, want %d: the request took a rung other than 2", got, before+1)
	}
	if got := decodedBodyFrames(t, body, ""); got != want {
		t.Errorf("the body decodes to %d frames, want the trimmed %d", got, want)
	}
}

// TestStreamCopyWalksNothingItsPlanMeasured: the plan measured the Matroska
// source, and the run takes that track, so the copy walks no file for its
// end trim, cold or with the track memo warm, trimmed or not.
func TestStreamCopyWalksNothingItsPlanMeasured(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	want := unprimedEndTrim(t, filepath.Join(env.root, "endtrim.mka"))
	trimmed, err := os.ReadFile(filepath.Join(env.root, "endtrim.mka"))
	if err != nil {
		t.Fatal(err)
	}
	// The same packets with no trim: what a stream copy into Matroska holds.
	adts := &testutil.MemWriteSeeker{}
	e := waxflow.New()
	if _, err := e.Remux(context.Background(), container.BytesSource(trimmed), "", adts,
		waxflow.TranscodeOptions{Format: "aac", Container: "adts"}); err != nil {
		t.Fatal(err)
	}
	untrimmed := &testutil.MemWriteSeeker{}
	if _, err := e.Remux(context.Background(), container.BytesSource(adts.Buf), "", untrimmed,
		waxflow.TranscodeOptions{Format: "aac", Container: "mka"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "untrimmed.mka"), untrimmed.Buf, 0o644); err != nil {
		t.Fatal(err)
	}
	// started is how each copy began: the engine's own word on its walk.
	started := func() []string {
		var out []string
		for _, r := range rec.Records() {
			if r.Msg == "remux started" {
				out = append(out, r.Attrs)
			}
		}
		return out
	}
	for _, src := range []struct {
		name   string
		frames int64
	}{{"endtrim.mka", want}, {"untrimmed.mka", 49 * 1024}} {
		// The second request names the same muxer another way, so it keys
		// apart and runs its own copy on the track the first one measured.
		for i, query := range []string{"format=aac", "format=aac&container=fragmented"} {
			before := len(started())
			resp := env.get(t, "/stream?src=lib/"+src.name+"&"+query, nil)
			body := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("%s, request %d = %d: %s", src.name, i, resp.StatusCode, body)
			}
			if got := decodedBodyFrames(t, body, ""); got != src.frames {
				t.Errorf("%s, request %d: the body decodes to %d frames, want %d", src.name, i, got, src.frames)
			}
			runs := started()[before:]
			if len(runs) != 1 || !strings.Contains(runs[0], "walked=false") {
				t.Errorf("%s, request %d: the copy started as %q, want one run that walked nothing", src.name, i, runs)
			}
		}
	}
}
