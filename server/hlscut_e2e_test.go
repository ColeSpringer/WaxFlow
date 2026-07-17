package server_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// fetchHLSPresentation drives a minted HLS URL to completion: master, media, then
// the init header and every segment in order, returning the concatenated bytes.
func fetchHLSPresentation(t *testing.T, env *testEnv, masterURL string) []byte {
	t.Helper()
	master := readBody(t, keyless(t, env, masterURL))
	var mediaRef string
	for _, l := range playlistLines(master) {
		if !strings.HasPrefix(l, "#") {
			mediaRef = l
		}
	}
	if mediaRef == "" {
		t.Fatalf("no variant in master:\n%s", master)
	}
	resp := keyless(t, env, "/hls/"+mediaRef)
	media := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media = %d: %s", resp.StatusCode, media)
	}
	initURI, segURIs, _ := mediaURIs(t, media)
	if initURI == "" || len(segURIs) == 0 {
		t.Fatalf("init %q, %d segments:\n%s", initURI, len(segURIs), media)
	}
	whole := readBody(t, keyless(t, env, "/hls/"+initURI))
	for _, u := range segURIs {
		r := keyless(t, env, "/hls/"+u)
		b := readBody(t, r)
		if r.StatusCode != 200 {
			t.Fatalf("segment %s = %d", u, r.StatusCode)
		}
		whole = append(whole, b...)
	}
	return whole
}

// TestHLSCutServesTheCutRung is A15's HLS half end to end: a from/to span on an
// Opus source served by moving the source's own packets into fMP4 segments rather
// than decoding and re-encoding them.
//
// The proof it took the cut rung and not rung 3 is the metric, and it has to be:
// rung 3 produces correct, playable segments too, which is exactly why the
// segmented rungs assert a counter rather than bytes. The byte-identity check is
// the second half of the claim: the concatenated presentation demuxes back to the
// source's own Opus packets, which no re-encode could reproduce.
func TestHLSCutServesTheCutRung(t *testing.T) {
	env := newTestEnv(t, nil)
	// ramp.wav is 4 s at 48 kHz; the Opus fixture inherits it, and the grid is 960
	// (20 ms), so cut points that are whole multiples of 960 land on packet
	// boundaries with no snap slop to hide.
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")
	before := env.srv.Metrics().Cuts.Load()

	// Keep [48000, 144000): drop the first and last second. format=opus is
	// load-bearing, not decorative: the cut is reached only with a format the
	// source codec matches.
	masterURL := mintHLS(t, env, map[string]string{
		"src": ref, "format": "opus", "from": "48000", "to": "144000",
	})
	whole := fetchHLSPresentation(t, env, masterURL)

	if got := env.srv.Metrics().Cuts.Load(); got != before+1 {
		t.Fatalf("cut_total = %d, want %d: the HLS span took a rung other than the cut", got, before+1)
	}

	// The kept payloads must be the source's own packets, in order, byte for byte.
	// The head backs off by the codec pre-roll, so the emitted run starts a few
	// packets before sample 48000; those are still source packets (the edit list
	// trims them on playback), so a walk that finds each emitted packet ahead in the
	// source, in order, is the check. A prefix comparison would pass on a cut that
	// dropped only the tail.
	srcRaw, err := os.ReadFile(filepath.Join(env.root, "ramp.opus"))
	if err != nil {
		t.Fatal(err)
	}
	want := hlsPayloads(t, container.BytesSource(srcRaw), "opus")
	got := hlsPayloads(t, container.BytesSource(whole), "mp4")
	if len(got) == 0 {
		t.Fatal("the cut presentation holds no packets")
	}
	if len(got) >= len(want) {
		t.Fatalf("the cut holds %d of the source's %d packets; nothing was dropped", len(got), len(want))
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
	t.Logf("cut %d source packets to %d across HLS segments, all byte-identical", len(want), len(got))
}

// TestHLSCutDeclinedFallsThrough pins the other side of the rung: a span the cut
// cannot serve must fall through to a transcode with no cut counted, exactly as
// the remux rung declines. Two distinct declines:
//
//   - a FLAC span: FLAC is lossless and off the cut allowlist, so the rung never
//     applies however cleanly the packets would move.
//   - an Opus span requested as FLAC: the cut track is Opus and the output FLAC,
//     so PlanRemux inside PlanCutSegments declines the codec mismatch. This is the
//     reachability fact the API doc surfaces: the cut needs a source-matching
//     format, not one the source is not.
func TestHLSCutDeclinedFallsThrough(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")

	for _, tc := range []struct {
		name   string
		params map[string]string
	}{
		{"flac-not-cuttable", map[string]string{"src": "lib/album/track.flac", "format": "flac", "to": "10000"}},
		{"opus-format-mismatch", map[string]string{"src": ref, "format": "flac", "from": "48000", "to": "144000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := env.srv.Metrics().Cuts.Load()
			whole := fetchHLSPresentation(t, env, mintHLS(t, env, tc.params))
			if len(whole) == 0 {
				t.Fatal("the fallback transcode produced no bytes")
			}
			if got := env.srv.Metrics().Cuts.Load(); got != before {
				t.Fatalf("cut_total moved to %d: the cut rung took a span it must decline", got)
			}
		})
	}
}
