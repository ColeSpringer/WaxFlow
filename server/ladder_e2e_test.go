package server_test

import (
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// ladderRequest asks the daemon for params on surface ("stream" or "hls"),
// pulling an HLS variant's first segment so a worker runs, and fails the
// test unless every answer is a 200.
func ladderRequest(t *testing.T, env *testEnv, surface string, params map[string]string) {
	t.Helper()
	if surface == "stream" {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		resp := env.get(t, "/stream?"+q.Encode(), nil)
		if body := readBody(t, resp); resp.StatusCode != 200 {
			t.Fatalf("/stream = %d: %s", resp.StatusCode, body)
		}
		return
	}
	master := readBody(t, keyless(t, env, mintHLS(t, env, params)))
	media := readBody(t, keyless(t, env, "/hls/"+variantURIs(t, master)[0]))
	_, segURIs, _ := mediaURIs(t, media)
	if len(segURIs) == 0 {
		t.Fatalf("no segments:\n%s", media)
	}
	resp := keyless(t, env, "/hls/"+segURIs[0])
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("segment = %d: %s", resp.StatusCode, body)
	}
}

// TestHLSRemuxDeclinesOddDepthsBeforeTheWalk: a 20-bit FLAC asked as flac
// over HLS is widened for MSE, which a packet copy cannot do, so the variant
// takes the transcode and no packet grid is measured for it.
func TestHLSRemuxDeclinesOddDepthsBeforeTheWalk(t *testing.T) {
	env := newTestEnv(t, nil)
	raw, err := os.ReadFile(testutil.PkgFile("..", "testdata", "sine-s24.wav"))
	if err != nil {
		t.Fatal(err)
	}
	ws := &testutil.MemWriteSeeker{}
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", ws,
		waxflow.TranscodeOptions{Format: "flac", BitDepth: 20}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "sine-s20.flac"), ws.Buf, 0o644); err != nil {
		t.Fatal(err)
	}
	ladderRequest(t, env, "hls", map[string]string{"src": "lib/sine-s20.flac", "format": "flac"})
	if got := env.srv.Metrics().Remuxes.Load(); got != 0 {
		t.Errorf("remux_total = %d: the copy rung took a depth it cannot widen", got)
	}
	if got := env.srv.PacketGridsMeasured(); got != 0 {
		t.Errorf("%d packet grids measured for a variant whose headers rule out a copy", got)
	}
}

// transcodeFixture writes the library's ramp as name under opts and returns
// its ref.
func transcodeFixture(t *testing.T, env *testEnv, name string, opts waxflow.TranscodeOptions) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.root, "ramp.wav"))
	if err != nil {
		t.Fatal(err)
	}
	ws := &testutil.MemWriteSeeker{}
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", ws, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, name), ws.Buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/" + name
}

// rewriteConfig copies the library file src (sniffed as hint) to name under
// opts, its packets unchanged and its codec config the one edit returns, and
// returns its ref.
func rewriteConfig(t *testing.T, env *testEnv, src, hint, name string, opts waxflow.TranscodeOptions,
	edit func(cfg []byte) []byte) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.root, src))
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	track.CodecConfig = edit(append([]byte(nil), track.CodecConfig...))
	ws := &testutil.MemWriteSeeker{}
	if _, err := waxflow.New().RemuxDemuxer(t.Context(), demux, track, ws, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, name), ws.Buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/" + name
}

// writeFamily1Opus writes the ramp as a stereo Opus whose OpusHead names
// channel mapping family 1 (one coupled stream, identity order), which Ogg
// carries and an fMP4 sample entry cannot, and returns its ref.
func writeFamily1Opus(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	writeOpusFixture(t, env, "ramp.wav", "family0-"+name)
	return rewriteConfig(t, env, "family0-"+name, "opus", name, waxflow.TranscodeOptions{Format: "opus"},
		func(head []byte) []byte {
			head = append(head, 1, 1, 0, 1)
			head[18] = 1
			return head
		})
}

// TestSpanCopyDeclinesBeforeTheWalk: a span whose source, format or options
// rule out a copy takes the transcode with no packet grid measured, on both
// surfaces. The controls are spans the cut serves, which measure it once.
func TestSpanCopyDeclinesBeforeTheWalk(t *testing.T) {
	opus := func(t *testing.T, env *testEnv) string { return writeOpusFixture(t, env, "ramp.wav", "ramp.opus") }
	for _, tc := range []struct {
		name, surface string
		src           func(*testing.T, *testEnv) string
		params        map[string]string
		cut           bool
	}{
		{"stream as mp3", "stream", opus, map[string]string{"format": "mp3"}, false},
		{"stream under a bitrate", "stream", opus, map[string]string{"format": "opus", "bitrate": "64"}, false},
		{"hls as aac", "hls", opus, map[string]string{"format": "aac"}, false},
		{"hls under a bitrate", "hls", opus, map[string]string{"format": "opus", "bitrate": "64"}, false},
		// A cut from past sample 0 has a front trim, which ADTS cannot state.
		{"stream to adts", "stream", func(t *testing.T, env *testEnv) string {
			return writeFixture(t, env, "ramp.wav", "ramp.m4a", "aac")
		}, map[string]string{"format": "aac", "container": "adts"}, false},
		{"hls of a family-1 opus", "hls", func(t *testing.T, env *testEnv) string {
			return writeFamily1Opus(t, env, "family1.opus")
		}, map[string]string{"format": "opus"}, false},
		{"stream the cut serves", "stream", opus, map[string]string{"format": "opus"}, true},
		{"hls the cut serves", "hls", opus, map[string]string{"format": "opus"}, true},
		// From sample 0 of a source with no delay to a packet edge: no trims.
		{"stream to adts from the start", "stream", func(t *testing.T, env *testEnv) string {
			return writeADTSFixture(t, env, "ramp.wav", "ramp.aac")
		}, map[string]string{"format": "aac", "container": "adts", "from": "0", "to": "24576"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, nil)
			params := map[string]string{"src": tc.src(t, env), "from": "48000", "to": "144000"}
			for k, v := range tc.params {
				params[k] = v
			}
			ladderRequest(t, env, tc.surface, params)
			want, wantCuts := 0, uint64(0)
			if tc.cut {
				want, wantCuts = 1, 1
			}
			if got := env.srv.Metrics().Cuts.Load(); got != wantCuts {
				t.Errorf("cut_total = %d, want %d", got, wantCuts)
			}
			if got := env.srv.PacketGridsMeasured(); got != want {
				t.Errorf("%d packet grids measured, want %d", got, want)
			}
		})
	}
}

// TestCopyDeclinesWhatItsMuxerRefuses: a source whose destination muxer
// would refuse the track at Begin takes the transcode, a span of it without
// measuring a packet grid. The controls copy to a muxer that takes it, and
// over HLS, which writes fMP4 whatever the form, plan and run both copy.
func TestCopyDeclinesWhatItsMuxerRefuses(t *testing.T) {
	aacMKA := waxflow.TranscodeOptions{Format: "aac", Container: "mka"}
	oggFLAC := waxflow.TranscodeOptions{Format: "flac", Container: waxflow.ContainerOgg}
	aacWith := func(asc []byte) func(*testing.T, *testEnv) string {
		return func(t *testing.T, env *testEnv) string {
			transcodeFixture(t, env, "aac.mka", aacMKA)
			return rewriteConfig(t, env, "aac.mka", "mka", "edited.mka", aacMKA, func([]byte) []byte { return asc })
		}
	}
	pce := aacWith([]byte{0x11, 0x80}) // channel configuration 0 at 48 kHz
	long := aacWith(append([]byte{0x11, 0x90}, make([]byte, 120)...))
	minBlock := func(t *testing.T, env *testEnv) string {
		transcodeFixture(t, env, "flac.oga", oggFLAC)
		return rewriteConfig(t, env, "flac.oga", "oga", "minblock.oga", oggFLAC, func(si []byte) []byte {
			si[0], si[1] = 0, 8 // a minimum block under 16
			return si
		})
	}
	for _, tc := range []struct {
		name, surface string
		src           func(*testing.T, *testEnv) string
		params        map[string]string
		remux         uint64
		grids         int
	}{
		{"a PCE as aac", "stream", pce, map[string]string{"format": "aac"}, 0, 0},
		{"a PCE span as aac", "stream", pce, map[string]string{"format": "aac", "from": "48000", "to": "96000"}, 0, 0},
		{"a long config as aac", "stream", long, map[string]string{"format": "aac"}, 0, 0},
		{"a long config as adts", "stream", long, map[string]string{"format": "aac", "container": "adts"}, 1, 0},
		{"a minimum block under 16 as flac", "stream", minBlock, map[string]string{"format": "flac"}, 0, 0},
		{"a minimum block under 16 as mka", "stream", minBlock, map[string]string{"format": "flac", "container": "mka"}, 1, 0},
		{"a minimum block under 16 over hls", "hls", minBlock, map[string]string{"format": "flac"}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, nil)
			params := map[string]string{"src": tc.src(t, env)}
			maps.Copy(params, tc.params)
			ladderRequest(t, env, tc.surface, params)
			if got := env.srv.Metrics().Remuxes.Load(); got != tc.remux {
				t.Errorf("remux_total = %d, want %d", got, tc.remux)
			}
			if got := env.srv.Metrics().Cuts.Load(); got != 0 {
				t.Errorf("cut_total = %d, want 0", got)
			}
			if got := env.srv.PacketGridsMeasured(); got != tc.grids {
				t.Errorf("%d packet grids measured, want %d", got, tc.grids)
			}
		})
	}
}
