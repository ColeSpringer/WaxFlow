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
)

// writeOpusFixture transcodes a WAV in the env's root into an Opus file beside
// it and returns its library ref. The corpus carries no Opus fixture, and the
// remux rung needs a source whose codec already is the output's.
func writeOpusFixture(t *testing.T, env *testEnv, from, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.root, from))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := waxflow.New()
	if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, name), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/" + name
}

// TestHLSRemuxServesTheMiddleRung is WaxTap's motivating case end to end:
// format=opus on a source that is already Opus. There is no direct-play rung in
// HLS (every variant is segmented), so this is the request that used to decode
// and re-encode an Opus stream for no reason at all, and now does not.
//
// The proof it took rung 2 is the metric; the proof the result is real is that
// the concatenated init and segments demux back to the source's own packets.
func TestHLSRemuxServesTheMiddleRung(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")
	before := env.srv.Metrics().Remuxes.Load()

	masterURL := mintHLS(t, env, map[string]string{"src": ref, "format": "opus"})
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

	// Fetch the whole presentation: init plus every segment, in order.
	whole := readBody(t, keyless(t, env, "/hls/"+initURI))
	for _, u := range segURIs {
		r := keyless(t, env, "/hls/"+u)
		b := readBody(t, r)
		if r.StatusCode != 200 {
			t.Fatalf("segment %s = %d", u, r.StatusCode)
		}
		whole = append(whole, b...)
	}
	if got := env.srv.Metrics().Remuxes.Load(); got == before {
		t.Fatal("remux_total did not move: an Opus source served as Opus HLS was re-encoded")
	}

	// The payloads must be the source's own, which is the whole claim.
	srcRaw, err := os.ReadFile(filepath.Join(env.root, "ramp.opus"))
	if err != nil {
		t.Fatal(err)
	}
	want := hlsPayloads(t, container.BytesSource(srcRaw), "opus")
	got := hlsPayloads(t, container.BytesSource(whole), "mp4")
	if len(got) != len(want) {
		t.Fatalf("the presentation holds %d packets, the source had %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d changed: rung 2 must move the source's bytes, not remake them", i)
		}
	}
}

// hlsPayloads demuxes a container and returns its packet payloads, copied.
func hlsPayloads(t *testing.T, src container.Source, hint string) [][]byte {
	t.Helper()
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := info.Default().ID
	var out [][]byte
	var pkt container.Packet
	for demux.ReadPacket(&pkt) == nil {
		if pkt.Track == id {
			out = append(out, bytes.Clone(pkt.Data))
		}
	}
	return out
}

// TestHLSRemuxDeclinedForSpanAndBitrate pins the structural declines. A span
// cuts mid-packet, and a bitrate ladder is a per-variant encoder setting, so
// both must take rung 3 however well the codec would otherwise have matched.
func TestHLSRemuxDeclinedForSpanAndBitrate(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")

	for _, tc := range []struct {
		name   string
		params map[string]string
	}{
		{"span", map[string]string{"src": ref, "format": "opus", "to": "48000"}},
		{"bitrate", map[string]string{"src": ref, "format": "opus", "bitrate": "64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := env.srv.Metrics().Remuxes.Load()
			master := readBody(t, keyless(t, env, mintHLS(t, env, tc.params)))
			var mediaRef string
			for _, l := range playlistLines(master) {
				if !strings.HasPrefix(l, "#") {
					mediaRef = l
				}
			}
			media := readBody(t, keyless(t, env, "/hls/"+mediaRef))
			initURI, segURIs, _ := mediaURIs(t, media)
			if initURI == "" || len(segURIs) == 0 {
				t.Fatalf("no segments:\n%s", media)
			}
			// Pull one segment, which is what spawns a worker at all.
			r := keyless(t, env, "/hls/"+segURIs[0])
			readBody(t, r)
			if r.StatusCode != 200 {
				t.Fatalf("segment = %d", r.StatusCode)
			}
			if got := env.srv.Metrics().Remuxes.Load(); got != before {
				t.Fatalf("remux_total moved to %d: rung 2 took a request it cannot serve", got)
			}
		})
	}
}
