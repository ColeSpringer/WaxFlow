package server_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRemuxServesTheMiddleRung drives the ladder's new rung end to end:
// format=flac&container=mka on a FLAC source. Rung 1 declines (the wrapper is
// not the one the file has), rung 2 accepts (FLAC survives into Matroska), and
// nothing decodes.
//
// The proof that it took rung 2 and not rung 3 is the metric, and the proof
// that it is a rewrite rather than the original bytes is the body: a Matroska
// file with the source's own FLAC frames in it.
func TestRemuxServesTheMiddleRung(t *testing.T) {
	env := newTestEnv(t, nil)
	before := env.srv.Metrics().Remuxes.Load()

	resp := env.get(t, "/stream?src=lib/album/track.flac&format=flac&container=mka", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("remux request = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/x-matroska" {
		t.Fatalf("Content-Type = %q, want audio/x-matroska: the container override was ignored", ct)
	}
	// EBML magic: this is Matroska, not the .flac file served verbatim.
	if len(body) < 4 || !bytes.Equal(body[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		t.Fatalf("body is not an EBML stream (first bytes %x)", body[:min(4, len(body))])
	}
	orig, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(body, orig) {
		t.Fatal("the response is the original file: rung 1 served a request whose container it does not satisfy")
	}
	if got := env.srv.Metrics().Remuxes.Load(); got != before+1 {
		t.Fatalf("remux_total = %d, want %d: the request took a rung other than 2", got, before+1)
	}
}

// TestRemuxDeclinedWhenSamplesMustChange pins the other side of the ladder: a
// request that transforms samples must not take rung 2, however well its codec
// and container would have matched.
func TestRemuxDeclinedWhenSamplesMustChange(t *testing.T) {
	env := newTestEnv(t, nil)

	for _, tc := range []struct{ name, query string }{
		// 48000 against a 44.1 kHz source: a rate that genuinely resamples. A
		// rate equal to the source's is a no-op and remuxes, which
		// TestRemuxAcceptsNoOpParameters pins.
		{"rate", "/stream?src=lib/album/track.flac&format=flac&container=mka&rate=48000"},
		{"gain", "/stream?src=lib/album/track.flac&format=flac&container=mka&gain=-6"},
		{"seek", "/stream?src=lib/album/track.flac&format=flac&container=mka&t=0.5"},
		// A span only trims the end, so from stays 0 and every other clause
		// passes: without its own decline this would remux the whole rip for a
		// request that asked for one track of it.
		{"span", "/stream?src=lib/album/track.flac&format=flac&container=mka&to=10000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := env.srv.Metrics().Remuxes.Load()
			resp := env.get(t, tc.query, nil)
			readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("request = %d", resp.StatusCode)
			}
			if got := env.srv.Metrics().Remuxes.Load(); got != before {
				t.Fatalf("remux_total moved to %d: rung 2 accepted a request that transforms samples", got)
			}
		})
	}
}

// TestRemuxAcceptsNoOpParameters pins rung 2 against rung 1's convention: a
// parameter naming what the source already is asks for no transform, so it must
// not push the request into a re-encode.
//
// The asymmetry without this is indefensible. directPlayable compares rate=,
// ch=, and bits= against the track rather than against zero, so
// format=flac&rate=44100 on a 44.1 kHz FLAC serves the original bytes; the same
// request with container=mka would have decoded and re-encoded the file to
// produce samples it already had.
// Each case takes a fresh daemon on purpose. All three resolve to the same
// output, so they share one canonical key and one cache entry: run against one
// env, only the first would spawn a pipeline and the rest would silently pass on
// a cache hit whatever rung had filled it.
func TestRemuxAcceptsNoOpParameters(t *testing.T) {
	// The fixture is 44.1 kHz stereo 16-bit; every value below names exactly
	// what it already is.
	for _, tc := range []struct{ name, query string }{
		{"rate", "/stream?src=lib/album/track.flac&format=flac&container=mka&rate=44100"},
		{"ch", "/stream?src=lib/album/track.flac&format=flac&container=mka&ch=2"},
		{"bits", "/stream?src=lib/album/track.flac&format=flac&container=mka&bits=16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, nil)
			resp := env.get(t, tc.query, nil)
			readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("request = %d", resp.StatusCode)
			}
			if got := env.srv.Metrics().Remuxes.Load(); got != 1 {
				t.Fatalf("remux_total = %d, want 1: a no-op %s forced a generation-losing re-encode",
					got, tc.name)
			}
		})
	}
}

// TestRemuxDeclinedUnderABitrateCap pins the decline that is the server's own
// rather than the engine's: this rung cannot promise a bit rate, because the
// source's real rate lives in its packets and a plan reads headers. A cap must
// therefore fall to the rung that can hold one honestly, rather than being
// refused outright for a request a transcode would serve.
func TestRemuxDeclinedUnderABitrateCap(t *testing.T) {
	env := newTestEnv(t, nil)
	before := env.srv.Metrics().Remuxes.Load()
	resp := env.get(t, "/stream?src=lib/album/track.flac&format=opus&maxBitRate=128", nil)
	readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("capped request = %d, want 200 from rung 3", resp.StatusCode)
	}
	if got := env.srv.Metrics().Remuxes.Load(); got != before {
		t.Fatalf("remux_total moved to %d under a bit rate cap it cannot promise", got)
	}
}
