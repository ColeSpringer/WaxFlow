package server_test

// HE-AAC v2 over the server surface: the hev2 parameter on /stream and
// the HLS descriptor. The cache-key unit pin lives beside canonicalCore;
// here the same contract is proven end to end by serving v2 before v1
// from one warm cache: if the key missed the flag, the second request
// would answer with the first's bytes.

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// streamPS fetches one /stream URL and reports whether the served fMP4
// carries an explicit PS (AOT-29) config.
func streamPS(t *testing.T, env *testEnv, url string) bool {
	t.Helper()
	resp := env.get(t, url, nil)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%s = %d: %s", url, resp.StatusCode, body)
	}
	_, info, err := format.OpenDemuxer(container.BytesSource(body), "mp4", nil)
	if err != nil {
		t.Fatalf("opening the served stream: %v", err)
	}
	tr := info.Default()
	if tr.Codec != codec.HEAAC || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
		t.Fatalf("served track = codec %q %v, want stereo he-aac at 48000", tr.Codec, tr.Fmt)
	}
	cfg, err := aac.ParseASC(tr.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.PS
}

// TestStreamHEv2 covers the /stream parameter: hev2=1 serves an AOT-29
// stream, the flagless request stays v1 (the two requests differ in
// both the canonical params and the plan's version tuple, so a
// collision would take losing both; serving them back to back against
// one warm cache keeps that pinned end to end), and the flag on a
// non-he-aac format is refused by name even where the source could
// direct-play (directPlayable's hev2 decline: without it a matching
// source served its own bytes with the flag silently ignored).
func TestStreamHEv2(t *testing.T) {
	env := newTestEnv(t, nil)
	if !streamPS(t, env, "/stream?src=lib/ramp.wav&format=he-aac&hev2=1") {
		t.Fatal("hev2=1 served a stream without an explicit PS config")
	}
	if streamPS(t, env, "/stream?src=lib/ramp.wav&format=he-aac") {
		t.Fatal("the flagless request served the v2 entry: hev2 fell out of the cache key")
	}

	// The refusal must fire on a direct-play-eligible request: a FLAC
	// source under format=flac would serve its original bytes, and hev2
	// must decline that rung rather than ride along ignored.
	for _, url := range []string{
		"/stream?src=lib/album/track.flac&format=flac&hev2=1",
		"/stream?src=lib/ramp.wav&hev2=1", // format=auto resolves off he-aac too
	} {
		resp := env.get(t, url, nil)
		body := readBody(t, resp)
		if resp.StatusCode != 415 {
			t.Fatalf("%s = %d (%s), want 415", url, resp.StatusCode, body)
		}
		if !bytes.Contains(body, []byte("hev2")) {
			t.Fatalf("the refusal does not name the parameter: %s", body)
		}
	}
}

// TestHLSMintEmptyHEv2 pins the mint against /stream's empty-value
// rule: a raw master mint with hev2 present but empty (what a client
// templating hev2=${flag} produces) must refuse, not silently mint and
// sign a downgraded v1 ladder.
func TestHLSMintEmptyHEv2(t *testing.T) {
	env := newTestEnv(t, nil)
	resp := env.get(t, "/hls/master.m3u8?src=lib/ramp.wav&format=he-aac&hev2=", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("empty hev2 minted with %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("hev2 is present but empty")) {
		t.Fatalf("the refusal does not state the empty-value rule: %s", body)
	}
}

// TestHLSAACBitrateLadder pins planHLSVariant's AACBitrate wiring: an
// aac master with a bitrate ladder must produce rungs that actually
// encode at their advertised rates. Before the fix the field was never
// set from the descriptor, so every rung encoded at the row default and
// collapsed onto one cache entry: three BANDWIDTH lines serving
// byte-identical media.
func TestHLSAACBitrateLadder(t *testing.T) {
	env := newTestEnv(t, nil)
	masterURL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "aac", "bitrates": "32,192"})
	resp := keyless(t, env, masterURL)
	master := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("master = %d: %s", resp.StatusCode, master)
	}
	var sizes []int
	for _, l := range playlistLines(master) {
		if len(l) == 0 || l[0] == '#' {
			continue
		}
		resp := keyless(t, env, "/hls/"+l)
		media := readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("media %s = %d: %s", l, resp.StatusCode, media)
		}
		_, segURIs, _ := mediaURIs(t, media)
		if len(segURIs) == 0 {
			t.Fatalf("media %s lists no segments", l)
		}
		resp = keyless(t, env, "/hls/"+segURIs[0])
		seg := readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("segment = %d", resp.StatusCode)
		}
		sizes = append(sizes, len(seg))
	}
	if len(sizes) != 2 {
		t.Fatalf("master lists %d variants, want 2:\n%s", len(sizes), master)
	}
	lo, hi := min(sizes[0], sizes[1]), max(sizes[0], sizes[1])
	if hi < lo*2 {
		t.Fatalf("ladder rungs measure %d and %d bytes; the 192k rung is not clearly larger than the 32k one, so the descriptor bitrate is not reaching the AAC encoder", sizes[0], sizes[1])
	}
}

// TestHLSMasterAdvertisesHEv2 pins the encode-side CODECS resolver
// through the whole HLS path: a v2 variant's master says mp4a.40.29 and
// its media playlist serves, while the same source without the flag
// stays mp4a.40.5.
func TestHLSMasterAdvertisesHEv2(t *testing.T) {
	env := newTestEnv(t, nil)

	masterURL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "he-aac", "hev2": "1"})
	resp := keyless(t, env, masterURL)
	master := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("master = %d: %s", resp.StatusCode, master)
	}
	if !bytes.Contains(master, []byte(`CODECS="mp4a.40.29"`)) {
		t.Fatalf("master does not advertise the v2 variant as mp4a.40.29: %s", master)
	}

	v1URL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "he-aac"})
	resp = keyless(t, env, v1URL)
	v1 := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("v1 master = %d: %s", resp.StatusCode, v1)
	}
	if !bytes.Contains(v1, []byte(`CODECS="mp4a.40.5"`)) {
		t.Fatalf("the flagless master lost its v1 CODECS: %s", v1)
	}

	// A v2 media playlist and its init segment serve (the descriptor
	// carries the flag through the variant path).
	var mediaRef string
	for _, l := range playlistLines(master) {
		if len(l) > 0 && l[0] != '#' {
			mediaRef = l
			break
		}
	}
	if mediaRef == "" {
		t.Fatalf("master lists no media playlist: %s", master)
	}
	resp = keyless(t, env, "/hls/"+mediaRef)
	media := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media = %d: %s", resp.StatusCode, media)
	}
	initRef, _, _ := mediaURIs(t, media)
	resp = keyless(t, env, "/hls/"+initRef)
	init := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("init = %d", resp.StatusCode)
	}
	_, info, err := format.OpenDemuxer(container.BytesSource(init), "mp4", nil)
	if err != nil {
		t.Fatalf("opening the init segment: %v", err)
	}
	cfg, err := aac.ParseASC(info.Default().CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PS {
		t.Fatal("the v2 variant's init segment carries no PS config")
	}
}
