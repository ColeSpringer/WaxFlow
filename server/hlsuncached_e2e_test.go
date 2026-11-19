package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

const servedUncached = server.HLSServedUncached

// hlsStream is one HLS presentation as a listener fetched it.
type hlsStream struct {
	init []byte
	segs [][]byte
}

func (h hlsStream) whole() []byte {
	return append(bytes.Clone(h.init), bytes.Join(h.segs, nil)...)
}

// rampSegments asks for the library's four-second ramp as FLAC in
// half-second segments: eight of them, which is every index these tests use.
var rampSegments = map[string]string{"src": "lib/ramp.wav", "format": "flac", "segDur": "0.5"}

// hlsURIs reads a minted master down to its first variant's init and segment
// URIs. No playlist touches the cache.
func hlsURIs(t *testing.T, env *testEnv, params map[string]string) (initURI string, segURIs []string) {
	t.Helper()
	master := readBody(t, keyless(t, env, mintHLS(t, env, params)))
	resp := keyless(t, env, "/hls/"+variantURIs(t, master)[0])
	media := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("media = %d: %s", resp.StatusCode, media)
	}
	initURI, segURIs, _ = mediaURIs(t, media)
	if initURI == "" || len(segURIs) < 3 {
		t.Fatalf("init %q and %d segments, want a stream of several:\n%s", initURI, len(segURIs), media)
	}
	return initURI, segURIs
}

// playHLS fetches the init header and every segment in order, each a 200.
func playHLS(t *testing.T, env *testEnv, initURI string, segURIs []string) hlsStream {
	t.Helper()
	fetch := func(uri string) []byte {
		resp := keyless(t, env, "/hls/"+uri)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || len(body) == 0 {
			name, _, _ := strings.Cut(uri, "?")
			t.Fatalf("%s = %d: %.300s", name, resp.StatusCode, body)
		}
		return body
	}
	out := hlsStream{init: fetch(initURI)}
	for _, uri := range segURIs {
		out.segs = append(out.segs, fetch(uri))
	}
	return out
}

// cacheFaults are the ways a cache refuses an HLS variant, each staged with
// a file where a directory belongs or a directory where a file belongs,
// which holds under any user and system.
var cacheFaults = []struct {
	name string
	// workers is how many workers a stream played through starts, each to
	// have its write refused.
	workers int
	inject  func(t *testing.T, env *testEnv, initURI string, segments int)
}{
	{"no variant directory can be made", 0, func(t *testing.T, env *testEnv, _ string, _ int) {
		fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	}},
	{"the variant refuses new files", 0, func(t *testing.T, env *testEnv, initURI string, _ int) {
		// Gone from under its index entry: the init header's write is
		// refused, which marks the variant before any worker starts.
		fileInPlaceOf(t, bornVariant(t, env, initURI))
	}},
	{"the variant refuses its segments", 1, func(t *testing.T, env *testEnv, initURI string, segments int) {
		// A directory that is not empty stands where each segment goes, so
		// a worker writes its temp file whole and the rename is refused.
		variant := bornVariant(t, env, initURI)
		for n := range segments {
			if err := os.MkdirAll(filepath.Join(variant, fmt.Sprintf("seg-%d.m4s", n), "kept"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}},
}

// bornVariant asks for the init header, which makes the variant's directory
// and caches the header in it, and returns the directory.
func bornVariant(t *testing.T, env *testEnv, initURI string) string {
	t.Helper()
	readBody(t, keyless(t, env, "/hls/"+initURI))
	variants, err := filepath.Glob(filepath.Join(env.cache, "v1", "*", "*"))
	if err != nil || len(variants) != 1 {
		t.Fatalf("the cache holds the variants %v (%v), want one", variants, err)
	}
	return variants[0]
}

// A listener hears the whole stream while the cache cannot be written: every
// request answers 200, the audio is the audio a healthy daemon serves, and
// each segment is counted and logged, at Warn once a variant. One cell per
// rung.
func TestHLSPlaysThroughACacheThatCannotBeWritten(t *testing.T) {
	type rung struct {
		name   string
		params func(t *testing.T, env *testEnv) map[string]string
		// check compares what the broken daemon served with what a healthy
		// one serves for the same request.
		check func(t *testing.T, env *testEnv, got, healthy hlsStream)
	}
	sameBytes := func(t *testing.T, got, healthy hlsStream) {
		t.Helper()
		if !bytes.Equal(got.init, healthy.init) {
			t.Error("the init header differs from a healthy daemon's")
		}
		for i := range healthy.segs {
			if !bytes.Equal(got.segs[i], healthy.segs[i]) {
				t.Errorf("segment %d differs from a healthy daemon's", i)
			}
		}
	}
	sourcePackets := func(t *testing.T, env *testEnv, got hlsStream, name string, all bool) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(env.root, name))
		if err != nil {
			t.Fatal(err)
		}
		want, have := hlsPayloads(t, container.BytesSource(raw), "opus"), hlsPayloads(t, container.BytesSource(got.whole()), "mp4")
		if len(have) == 0 || all && len(have) != len(want) || !all && len(have) >= len(want) {
			t.Fatalf("the stream holds %d packets of the source's %d", len(have), len(want))
		}
		at := 0
		for i, p := range have {
			for at < len(want) && !bytes.Equal(want[at], p) {
				at++
			}
			if at == len(want) {
				t.Fatalf("packet %d is not the source's own, in order", i)
			}
			at++
		}
	}
	rungs := []rung{
		{"a lossless transcode", func(t *testing.T, env *testEnv) map[string]string {
			return map[string]string{"src": "lib/ramp.wav", "format": "flac", "segDur": "1"}
		}, func(t *testing.T, env *testEnv, got, healthy hlsStream) {
			sameBytes(t, got, healthy)
			src, err := os.ReadFile(filepath.Join(env.root, "ramp.wav"))
			if err != nil {
				t.Fatal(err)
			}
			want, have := decodePCM(t, src), decodePCM(t, got.whole())
			if have.N != want.N {
				t.Fatalf("the stream decodes to %d frames, the source holds %d", have.N, want.N)
			}
			for c := range want.Fmt.Channels {
				if !equalSamples(want.ChanI(c)[:want.N], have.ChanI(c)[:have.N]) {
					t.Fatalf("channel %d does not decode to the source's samples", c)
				}
			}
		}},
		// A lossy encoder restarted at each segment writes other bytes than
		// one that ran on: the same frames at the same positions, and no
		// more error behind a boundary than a healthy stream shows anywhere.
		{"an Opus transcode", lossy("tones.wav", toneMix, "opus", "96"), sameAudioAtEveryBoundary(toneMix)},
		{"an Opus transcode at a low rate", lossy("tones.wav", toneMix, "opus", "32"), sameAudioAtEveryBoundary(toneMix)},
		{"an Opus transcode of plucked notes at a low rate", lossy("notes.wav", pluckedNotes, "opus", "32"), sameAudioAtEveryBoundary(pluckedNotes)},
		{"an AAC transcode", lossy("tones.wav", toneMix, "aac", "64"), sameAudioAtEveryBoundary(toneMix)},
		{"a remux", func(t *testing.T, env *testEnv) map[string]string {
			return map[string]string{"src": writeRampOpus(t, env), "format": "opus", "segDur": "1"}
		}, func(t *testing.T, env *testEnv, got, healthy hlsStream) {
			sameBytes(t, got, healthy)
			sourcePackets(t, env, got, "ramp.opus", true)
			if env.srv.Metrics().Remuxes.Load() == 0 {
				t.Error("the stream did not take the remux rung")
			}
		}},
		{"a timeline", func(t *testing.T, env *testEnv) map[string]string {
			refs := make([]string, 3)
			for i, frames := range []int{48000, 24000, 72000} {
				name := fmt.Sprintf("tl-%d.wav", i)
				if err := os.WriteFile(filepath.Join(env.root, name), rampWAV(t, 48000, 2, frames), 0o644); err != nil {
					t.Fatal(err)
				}
				refs[i] = "lib/" + name
			}
			return map[string]string{"tl": mintTimeline(t, env, refs).Tl, "format": "flac", "gain": "off", "segDur": "1"}
		}, func(t *testing.T, env *testEnv, got, healthy hlsStream) {
			sameBytes(t, got, healthy)
			if have := decodePCM(t, got.whole()); have.N != 48000+24000+72000 {
				t.Errorf("the stream decodes to %d frames, the members hold %d", have.N, 48000+24000+72000)
			}
		}},
		{"a cut", func(t *testing.T, env *testEnv) map[string]string {
			return map[string]string{"src": writeRampOpus(t, env), "format": "opus",
				"from": "24000", "to": "168000", "segDur": "1"}
		}, func(t *testing.T, env *testEnv, got, healthy hlsStream) {
			sameBytes(t, got, healthy)
			sourcePackets(t, env, got, "ramp.opus", false)
			if env.srv.Metrics().Cuts.Load() == 0 {
				t.Error("the stream did not take the cut rung")
			}
		}},
	}
	// One healthy daemon plays every rung once, before the cells run.
	timelines := func(cfg *server.Config) { cfg.TimelineDir = filepath.Join(t.TempDir(), "timelines") }
	twin := newTestEnv(t, timelines)
	healthy := make([]hlsStream, len(rungs))
	for i, rg := range rungs {
		initURI, segURIs := hlsURIs(t, twin, rg.params(t, twin))
		healthy[i] = playHLS(t, twin, initURI, segURIs)
	}
	for fi, fault := range cacheFaults {
		for ri, rg := range rungs {
			// What a restart does to a lossy encoder is the same behind
			// either fault, so its cells run behind the first.
			if fi > 0 && strings.Contains(rg.name, "transcode") && !strings.Contains(rg.name, "lossless") {
				continue
			}
			t.Run(fault.name+", "+rg.name, func(t *testing.T) {
				// Each cell has a daemon and a live pool of its own.
				t.Parallel()
				env, rec := recordingEnv(t, timelines)
				newTestClock(env) // never moved: no probe comes due
				initURI, segURIs := hlsURIs(t, env, rg.params(t, env))
				if len(segURIs) != len(healthy[ri].segs) {
					t.Fatalf("a healthy daemon lists %d segments, this one %d", len(healthy[ri].segs), len(segURIs))
				}
				fault.inject(t, env, initURI, len(segURIs))
				met := env.srv.Metrics()
				served, degraded := met.HLSSegments.Load(), met.Degradations.Load()
				got := playHLS(t, env, initURI, segURIs)
				if a, b := met.HLSSegments.Load()-served, met.Degradations.Load()-degraded; a != uint64(len(segURIs)) || b != a {
					t.Errorf("%d segments counted as %d served and %d degraded", len(segURIs), a, b)
				}
				rg.check(t, env, got, healthy[ri])

				waitForIdle(t, env)
				// One line a segment, the first at Warn and the rest at
				// Debug, and one for each worker whose write was refused:
				// none starts for a variant known to refuse them.
				lines := map[string][]server.LogRecord{}
				for _, r := range rec.Records() {
					lines[r.Msg] = append(lines[r.Msg], r)
				}
				wantLines := map[string]int{servedUncached: len(segURIs), "hls worker failed": fault.workers}
				for msg, want := range wantLines {
					if len(lines[msg]) != want {
						t.Fatalf("%d segments left %d %q lines, want %d: %v", len(segURIs), len(lines[msg]), msg, want, lines[msg])
					}
					for i, l := range lines[msg] {
						if wantLevel := map[bool]slog.Level{true: slog.LevelWarn, false: slog.LevelDebug}[i == 0]; l.Level != wantLevel {
							t.Errorf("%q line %d is at %s, want %s", msg, i, l.Level, wantLevel)
						}
					}
				}
				for _, l := range lines[servedUncached] {
					for _, want := range []string{"key=", "segment=", "rung=", "members=", "err="} {
						if !strings.Contains(l.Attrs, want) {
							t.Errorf("the line %v does not say %s", l, want)
						}
					}
				}
				// Nothing of a segment stays behind: no file, and no temp of
				// a write that was refused.
				held, _ := filepath.Glob(filepath.Join(env.cache, "v1", "*", "*", "*"))
				held = slices.DeleteFunc(held, func(p string) bool {
					fi, err := os.Stat(p)
					name := filepath.Base(p)
					return err == nil && (fi.IsDir() || name == "meta.json" || name == "init.mp4")
				})
				if len(held) != 0 {
					t.Errorf("the broken cache holds %v", held)
				}
			})
		}
	}
}

// toneMix is seven seconds of three steady tones, one of them slowly
// swelling: material on which an encoder's fault at a restart stands out.
// In one-second segments it has six boundaries, each judged in 90 windows,
// and a fault of the encoder's restart shows behind every one of them.
var toneMix = sync.OnceValue(func() [][]float32 {
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, 7*48000)
		for i := range chans[c] {
			x := float64(i) / 48000
			chans[c][i] = float32(0.25*math.Sin(2*math.Pi*220*x) + 0.2*math.Sin(2*math.Pi*1337*x+float64(c)) +
				0.1*math.Sin(2*math.Pi*5201*x)*(0.5+0.5*math.Sin(2*math.Pi*0.7*x)))
		}
	}
	return chans
})

// rampOpus is the library's ramp as Opus, encoded once for every daemon.
var rampOpus struct {
	once sync.Once
	raw  []byte
	err  error
}

func writeRampOpus(t *testing.T, env *testEnv) string {
	t.Helper()
	rampOpus.once.Do(func() {
		var src []byte
		if src, rampOpus.err = os.ReadFile(filepath.Join(env.root, "ramp.wav")); rampOpus.err != nil {
			return
		}
		var out bytes.Buffer
		_, rampOpus.err = waxflow.New().Transcode(context.Background(), container.BytesSource(src), "wav", &out,
			waxflow.TranscodeOptions{Format: "opus"})
		rampOpus.raw = out.Bytes()
	})
	if rampOpus.err != nil {
		t.Fatal(rampOpus.err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "ramp.opus"), rampOpus.raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/ramp.opus"
}

// pluckedNotes is seven seconds of decaying harmonic notes, a new one every
// 370 ms: material on which a restarted encoder that loses level stands out.
var pluckedNotes = sync.OnceValue(func() [][]float32 {
	pitches := []float64{130.81, 164.81, 196.0, 261.63, 329.63, 392.0, 220.0, 293.66}
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, 7*48000)
		for i := range chans[c] {
			x := float64(i) / 48000
			note := int(x / 0.37)
			f := pitches[(note*3+c)%len(pitches)]
			var v float64
			for h := 1; h <= 6; h++ {
				v += math.Exp(-4*(x-float64(note)*0.37)) * 0.22 / float64(h) * math.Sin(2*math.Pi*f*float64(h)*x)
			}
			chans[c][i] = float32(v)
		}
	}
	return chans
})

// lossy asks for signal, written to the library as name, in one-second
// segments of a lossy format.
func lossy(name string, signal func() [][]float32, format, kbps string) func(*testing.T, *testEnv) map[string]string {
	return func(t *testing.T, env *testEnv) map[string]string {
		if err := os.WriteFile(filepath.Join(env.root, name), testutil.PCM16WAVBytes(t, 48000, signal()), 0o644); err != nil {
			t.Fatal(err)
		}
		return map[string]string{"src": "lib/" + name, "format": format, "bitrates": kbps, "segDur": "1"}
	}
}

// sameAudioAtEveryBoundary holds a lossy stream of signal whose segments
// were each encoded alone to the healthy stream's frames, and to its audio
// where a restart would show: behind every segment boundary.
func sameAudioAtEveryBoundary(signal func() [][]float32) func(*testing.T, *testEnv, hlsStream, hlsStream) {
	return func(t *testing.T, _ *testEnv, got, healthy hlsStream) {
		t.Helper()
		if !bytes.Equal(got.init, healthy.init) {
			t.Error("the init header differs from a healthy daemon's")
		}
		for i := range healthy.segs {
			if a, b := segmentDecodeSamples(t, got.segs[i]), segmentDecodeSamples(t, healthy.segs[i]); a != b {
				t.Errorf("segment %d holds %d samples, a healthy daemon's holds %d", i, a, b)
			}
		}
		have, want := decodePCM(t, got.whole()), decodePCM(t, healthy.whole())
		if have.N != want.N || have.Fmt.Rate != 48000 {
			t.Fatalf("the stream decodes to %d frames at %d Hz, a healthy daemon's to %d", have.N, have.Fmt.Rate, want.N)
		}
		seg := int(segmentDecodeSamples(t, healthy.segs[1]))
		for _, fault := range boundaryFaults(have.ChanF(0)[:have.N], want.ChanF(0)[:want.N], signal()[0], seg) {
			t.Error(fault)
		}
	}
}

// boundaryFaults judges have, a decoded stream cut into segments of seg
// samples, in the 400 ms behind every segment boundary, 5 ms window by
// window, against healthy and their common source src. A window may hold at
// most twice the error against the source of healthy's worst window, which
// a burst exceeds, and its level may differ from healthy's in the same
// window by at most twice healthy's worst departure from the source's
// level, which a dropout exceeds. The stream's first second is left out.
func boundaryFaults(have, healthy, src []float32, seg int) []string {
	const (
		window = 240   // 5 ms
		behind = 19200 // 400 ms
		ahead  = 2400  // the encoder's delay puts a boundary this much early at most
	)
	level := func(pcm []float32, at int) float64 {
		var sum float64
		for _, v := range pcm[at : at+window] {
			sum += float64(v) * float64(v)
		}
		return math.Sqrt(sum / window)
	}
	errorIn := func(pcm []float32, at int) float64 {
		var sum float64
		for i := at; i < at+window; i++ {
			d := float64(pcm[i]) - float64(src[i])
			sum += d * d
		}
		return math.Sqrt(sum / window)
	}
	var worstError, worstLevel float64 // of the healthy stream, anywhere
	var judged []int
	for at := 48000; at+window <= min(len(have), len(healthy), len(src)); at += window {
		worstError = max(worstError, errorIn(healthy, at))
		worstLevel = max(worstLevel, math.Abs(level(healthy, at)-level(src, at)))
		if (at+ahead)%seg < ahead+behind {
			judged = append(judged, at)
		}
	}
	var loud, off []int
	var loudest, furthest float64
	for _, at := range judged {
		e, l := errorIn(have, at), math.Abs(level(have, at)-level(healthy, at))
		loudest, furthest = max(loudest, e), max(furthest, l)
		if e > 2*worstError {
			loud = append(loud, at)
		}
		if l > 2*worstLevel {
			off = append(off, at)
		}
	}
	var faults []string
	if len(loud) != 0 {
		faults = append(faults, fmt.Sprintf("%d of the %d windows behind a segment boundary hold over twice the error of the healthy stream's worst window (%.4f rms): the worst holds %.4f, %.1f dB over it, and the first is at sample %d",
			len(loud), len(judged), worstError, loudest, 20*math.Log10(loudest/worstError), loud[0]))
	}
	if len(off) != 0 {
		faults = append(faults, fmt.Sprintf("%d of the %d windows behind a segment boundary differ in level from the healthy stream's by over twice its worst departure from the source's (%.4f rms): the furthest by %.4f, %.1f times it, and the first is at sample %d",
			len(off), len(judged), worstLevel, furthest, furthest/worstLevel, off[0]))
	}
	return faults
}

// The gate that judges a restarted encoder can fail both ways: on a healthy
// low-rate stream it finds nothing, on the same stream with 20 ms of silence
// behind every boundary it finds the level, and with those 20 ms at full
// scale it finds the error.
func TestTheBoundaryGateSeesADropoutAndABurst(t *testing.T) {
	env := newTestEnv(t, nil)
	initURI, segURIs := hlsURIs(t, env, lossy("tones.wav", toneMix, "opus", "32")(t, env))
	healthy := playHLS(t, env, initURI, segURIs)
	pcm := decodePCM(t, healthy.whole())
	sound, src, seg := pcm.ChanF(0)[:pcm.N], toneMix()[0], int(segmentDecodeSamples(t, healthy.segs[1]))
	if faults := boundaryFaults(sound, sound, src, seg); len(faults) != 0 {
		t.Errorf("a healthy stream against itself: %v", faults)
	}
	behindEachBoundary := func(v float32) []float32 {
		out := slices.Clone(sound)
		for b := seg; b+960 < len(out); b += seg {
			for i := b; i < b+960; i++ {
				out[i] = v
			}
		}
		return out
	}
	if faults := boundaryFaults(behindEachBoundary(0), sound, src, seg); len(faults) != 1 || !strings.Contains(faults[0], "differ in level") {
		t.Errorf("20 ms of silence behind every boundary: %v, want the level alone", faults)
	}
	if faults := boundaryFaults(behindEachBoundary(1), sound, src, seg); len(faults) != 2 {
		t.Errorf("20 ms at full scale behind every boundary: %v, want the error and the level", faults)
	}
}

func equalSamples(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A cache that takes writes serves every segment from the worker's files and
// never encodes one for a request.
func TestHLSOnAWritableCacheTakesTheWorker(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	playHLS(t, env, initURI, segURIs)
	waitForIdle(t, env)
	for _, r := range rec.Records() {
		if r.Msg == servedUncached {
			t.Errorf("a healthy cache logged %v", r)
		}
	}
	if n := env.srv.Metrics().SessionsHLS.Load(); n != 1 {
		t.Errorf("%d workers ran, want the one", n)
	}
	if n := env.srv.Metrics().Degradations.Load(); n != 0 {
		t.Errorf("a healthy cache counted %d degradations", n)
	}
	onDisk, err := filepath.Glob(filepath.Join(env.cache, "v1", "*", "*", "seg-*.m4s"))
	if err != nil || len(onDisk) != len(segURIs) {
		t.Errorf("the cache holds %d segments (%v), want the worker's %d", len(onDisk), err, len(segURIs))
	}
}

// A segment encoded for its request takes a live slot, and with none free the
// listener is told to come back.
func TestHLSUncachedSegmentWaitsItsTurnForALiveSlot(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) { cfg.LiveSlots = 1 })
	_, segURIs := hlsURIs(t, env, rampSegments)
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	release, ok := env.srv.HoldLiveSlot()
	if !ok {
		t.Fatal("the one live slot is taken before any stream")
	}
	resp := keyless(t, env, "/hls/"+segURIs[1])
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a full pool answers with no Retry-After")
	}
	wantEnvelope(t, resp, http.StatusServiceUnavailable, waxerr.CodeOverloaded)
	release()
	fetchSegment(t, env, segURIs[1])
	if !liveSlotFree(env) {
		t.Error("the served segment kept its live slot")
	}
}

// A listener who leaves while its segment is being encoded gets nothing, and
// the encode gives back its live slot and its file.
func TestHLSUncachedSegmentStopsWhenItsListenerLeaves(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) { cfg.LiveSlots = 1 })
	_, segURIs := hlsURIs(t, env, rampSegments)
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	// The encode is held with its segment made until its listener is gone.
	reached := make(chan struct{})
	env.srv.GateHLSPublish(func(ctx context.Context, _ int64) {
		close(reached)
		<-ctx.Done()
	})
	req := httptest.NewRequest(http.MethodGet, "/hls/"+segURIs[2], nil)
	if w := leaveOnceGated(t, env, req, reached); w.Body.Len() != 0 {
		t.Errorf("a listener who left got %d: %s", w.Code, w.Body)
	}
	if !liveSlotFree(env) {
		t.Error("the abandoned encode kept its live slot")
	}
	if open := openUnder(t, env.root); len(open) != 0 {
		t.Errorf("the abandoned encode left %v open", open)
	}
}

// openUnder lists this process's open files under dir. It skips the test
// where the system does not show them.
func openUnder(t *testing.T, dir string) []string {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd here, so the open files of this process cannot be listed: %v", err)
	}
	var out []string
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && strings.HasPrefix(target, dir+string(filepath.Separator)) {
			out = append(out, target)
		}
	}
	return out
}

// A run that panics gives its live slot back, so the next request is served.
func TestHLSUncachedRunThatPanicsGivesItsSlotBack(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) { cfg.LiveSlots = 1 })
	_, segURIs := hlsURIs(t, env, rampSegments)
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	var once atomic.Bool
	env.srv.GateHLSPublish(func(context.Context, int64) {
		if once.CompareAndSwap(false, true) {
			panic("a codec fault")
		}
	})
	// Served without net/http around it, whose recovery would log the panic.
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		env.srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/hls/"+segURIs[1], nil))
	}()
	if recovered == nil {
		t.Fatal("the run did not panic")
	}
	if !liveSlotFree(env) {
		t.Error("the run that panicked kept the one live slot")
	}
	// The turn is back too, or these would wait for it.
	for _, uri := range segURIs[1:3] {
		if a := <-answered(t, env, uri, 10*time.Second); a.status != http.StatusOK {
			t.Fatalf("a request after the panic = %d: %.200s", a.status, a.body)
		}
	}
}

// One variant makes its uncached segments one at a time, however many ask:
// the rest of the live pool stays free for other listeners, every request is
// still answered, and requests for one segment share the one run.
func TestHLSUncachedRunsTakeOneSlotAVariant(t *testing.T) {
	twin := newTestEnv(t, nil)
	_, twinSegs := hlsURIs(t, twin, rampSegments)
	healthy := make([][]byte, 8)
	for n := range healthy {
		healthy[n] = readBody(t, keyless(t, twin, "/hls/"+twinSegs[n]))
	}
	for _, tc := range []struct {
		name     string
		segment  func(i int) int
		wantRuns int32
	}{
		{"eight requests for one segment", func(int) int { return 5 }, 1},
		{"eight requests for eight segments", func(i int) int { return i }, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, func(cfg *server.Config) { cfg.LiveSlots = 4 })
			_, segURIs := hlsURIs(t, env, rampSegments)
			fileInPlaceOf(t, filepath.Join(env.cache, "v1"))

			// Each run is held with its segment made until open closes.
			open := make(chan struct{})
			var runs, held, most atomic.Int32
			env.srv.GateHLSPublish(func(ctx context.Context, _ int64) {
				runs.Add(1)
				most.Store(max(most.Load(), held.Add(1)))
				defer held.Add(-1)
				select {
				case <-open:
				case <-ctx.Done():
				}
			})
			type answer struct {
				status int
				body   []byte
			}
			answers := make([]answer, 8)
			var wg sync.WaitGroup
			for i := range answers {
				wg.Go(func() {
					resp := keyless(t, env, "/hls/"+segURIs[tc.segment(i)])
					answers[i] = answer{resp.StatusCode, readBody(t, resp)}
				})
			}
			waitFor(t, "a run to reach the gate", func() bool { return held.Load() > 0 })
			// Every request is in: one holds the variant's turn, seven wait.
			waitFor(t, "all eight requests to arrive", func() bool {
				_, requests := env.srv.HLSUncachedTurns()
				return requests == 8
			})
			var free []func()
			for {
				release, ok := env.srv.HoldLiveSlot()
				if !ok {
					break
				}
				free = append(free, release)
			}
			for _, release := range free {
				release()
			}
			if len(free) != 3 {
				t.Errorf("%d of the 4 live slots are free while one variant is served, want 3", len(free))
			}
			other := env.get(t, "/stream?src=lib/ramp.wav&format=flac", nil)
			if body := readBody(t, other); other.StatusCode != http.StatusOK {
				t.Errorf("another listener's stream meanwhile = %d: %.200s", other.StatusCode, body)
			}
			close(open)
			wg.Wait()
			for i, a := range answers {
				n := tc.segment(i)
				if a.status != http.StatusOK {
					t.Errorf("request %d for segment %d = %d: %.200s", i, n, a.status, a.body)
					continue
				}
				if !bytes.Equal(a.body, healthy[n]) {
					t.Errorf("request %d got %d bytes that are not a healthy daemon's segment %d", i, len(a.body), n)
				}
			}
			if got := runs.Load(); got != tc.wantRuns || most.Load() != 1 {
				t.Errorf("%d runs with %d at once, want %d runs one at a time", got, most.Load(), tc.wantRuns)
			}
		})
	}
}

// An uncached segment goes out whole, with nothing a client could store or
// revalidate, and a range request for it gets the whole segment once.
func TestHLSUncachedSegmentIsWholeAndNotToBeStored(t *testing.T) {
	env := newTestEnv(t, nil)
	_, segURIs := hlsURIs(t, env, rampSegments)
	twin := newTestEnv(t, nil)
	_, twinSegs := hlsURIs(t, twin, rampSegments)
	cached := keyless(t, twin, "/hls/"+twinSegs[3])
	want := readBody(t, cached)
	if cached.Header.Get("ETag") == "" || cached.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("a cached segment's headers are %v, want an ETag and byte ranges", cached.Header)
	}

	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	for _, tc := range []struct{ name, method, ranged string }{
		{"a plain request", http.MethodGet, ""},
		{"a range request", http.MethodGet, "bytes=0-9"},
		{"a HEAD", http.MethodHead, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.req(t, tc.method, "/hls/"+segURIs[3], map[string]string{"X-API-Key": "", "Range": tc.ranged})
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %.200s", resp.StatusCode, body)
			}
			if tc.method == http.MethodGet && !bytes.Equal(body, want) {
				t.Errorf("the body is %d bytes, want the whole segment's %d", len(body), len(want))
			}
			for name, wantValue := range map[string]string{
				"ETag": "", "Last-Modified": "", "Content-Range": "", "Accept-Ranges": "none", "Cache-Control": "no-store",
				"Content-Type": "audio/mp4", "Content-Length": fmt.Sprint(len(want)),
			} {
				if got := resp.Header.Get(name); got != wantValue {
					t.Errorf("%s = %q, want %q", name, got, wantValue)
				}
			}
		})
	}
}

// A variant warns once an outage: while its cache stays broken the later
// segments log at Debug, and once a worker has written to it again the
// next refusal is a Warn again.
func TestHLSWarnsAgainWhenACacheBreaksASecondTime(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	clock := newTestClock(env)
	_, segURIs := hlsURIs(t, env, rampSegments)
	warned := func(msg string) int {
		n := 0
		for _, r := range rec.Warnings() {
			if r.Msg == msg {
				n++
			}
		}
		return n
	}
	want := func(when string, uncached, worker int) {
		t.Helper()
		if a, b := warned(servedUncached), warned("hls worker failed"); a != uncached || b != worker {
			t.Fatalf("%s: %d %q and %d %q warnings, want %d and %d: %v", when, a, servedUncached, b, "hls worker failed", uncached, worker, rec.Warnings())
		}
	}
	version := filepath.Join(env.cache, "v1")
	fileInPlaceOf(t, version)
	fetchSegment(t, env, segURIs[0])
	fetchSegment(t, env, segURIs[1])
	want("the first outage", 1, 0)

	// Healed: at the variant's next probe a worker fills it, and the
	// segment comes from disk.
	if err := os.Remove(version); err != nil {
		t.Fatal(err)
	}
	clock.advance(server.HLSProbeEvery)
	fetchSegment(t, env, segURIs[2])
	waitForIdle(t, env)
	variants, err := filepath.Glob(filepath.Join(version, "*", "*"))
	if err != nil || len(variants) != 1 {
		t.Fatalf("the healed cache holds the variants %v (%v), want one", variants, err)
	}
	// The worker started at the segment asked for and ran to the end.
	if cached, _ := filepath.Glob(filepath.Join(variants[0], "seg-*.m4s")); len(cached) != len(segURIs)-2 {
		t.Fatalf("the healed cache holds %d segments, want the %d from the third on", len(cached), len(segURIs)-2)
	}
	want("healed", 1, 0)

	// Broken again, now under a variant the cache knows.
	fileInPlaceOf(t, variants[0])
	fetchSegment(t, env, segURIs[3])
	fetchSegment(t, env, segURIs[4])
	waitForIdle(t, env)
	want("the second outage", 2, 1)

	// And a third time, so the worker's own line is seen to warn again too.
	if err := os.Remove(variants[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(variants[0], 0o755); err != nil {
		t.Fatal(err)
	}
	clock.advance(server.HLSProbeEvery)
	fetchSegment(t, env, segURIs[5])
	waitForIdle(t, env)
	fileInPlaceOf(t, variants[0])
	fetchSegment(t, env, segURIs[6])
	fetchSegment(t, env, segURIs[7])
	waitForIdle(t, env)
	want("the third outage", 3, 2)
}

// holdUncachedRuns holds every uncached run with its segment made until the
// returned open runs, and reports each arrival on reached.
func holdUncachedRuns(t *testing.T, env *testEnv) (open func(), reached <-chan struct{}) {
	t.Helper()
	gate, arrived := make(chan struct{}), make(chan struct{}, 64)
	open = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open)
	env.srv.GateHLSPublish(func(ctx context.Context, _ int64) {
		arrived <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
		}
	})
	return open, arrived
}

// answer is what a request got: status 0 when its handler wrote nothing.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// answered serves uri in the background for a client that waits no longer
// than bound, and delivers what it got.
func answered(t *testing.T, env *testEnv, uri string, bound time.Duration) <-chan answer {
	t.Helper()
	out := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		w := httptest.NewRecorder()
		env.srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hls/"+uri, nil).WithContext(ctx))
		a := answer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
		if len(a.body) == 0 {
			a.status = 0
		}
		out <- a
	}()
	return out
}

// Behind a variant known to refuse writes a request waits its turn before
// anything else, so one live slot serves two listeners of the variant.
func TestHLSRequestBehindAKnownBrokenVariantWaitsItsTurn(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) { cfg.LiveSlots = 1 })
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	cacheFaults[1].inject(t, env, initURI, len(segURIs))
	fetchSegment(t, env, segURIs[0]) // the variant is now known to refuse writes
	open, reached := holdUncachedRuns(t, env)
	first := answered(t, env, segURIs[1], 10*time.Second)
	within(t, reached, "the first run to hold the one slot")
	second := answered(t, env, segURIs[2], 10*time.Second)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, requests := env.srv.HLSUncachedTurns(); requests == 2 {
			break
		}
		select {
		case a := <-second:
			t.Fatalf("the second request did not wait its turn: %d %.200s", a.status, a.body)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the second request neither waited nor answered")
		}
	}
	open()
	for name, a := range map[string]answer{"first": <-first, "second": <-second} {
		if a.status != http.StatusOK {
			t.Errorf("the %s request = %d: %.200s", name, a.status, a.body)
		}
	}
}

// A request waits for its turn no longer than it would wait for a worker,
// and is then told to come back.
func TestHLSUncachedTurnIsWaitedForNoLongerThanAWorker(t *testing.T) {
	env := newTestEnv(t, nil)
	env.srv.SetHLSWaitTimeout(300 * time.Millisecond)
	_, segURIs := hlsURIs(t, env, rampSegments)
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	open, reached := holdUncachedRuns(t, env)
	first := answered(t, env, segURIs[1], 10*time.Second)
	within(t, reached, "the first run to hold the turn")
	late := <-answered(t, env, segURIs[2], 10*time.Second)
	var refused server.ErrorBody
	if err := json.Unmarshal(late.body, &refused); err != nil || late.status != http.StatusServiceUnavailable ||
		refused.Code != waxerr.CodeOverloaded || late.header.Get("Retry-After") == "" {
		t.Errorf("a turn that never came = %d %.200s with Retry-After %q, want 503 %s and when to come back",
			late.status, late.body, late.header.Get("Retry-After"), waxerr.CodeOverloaded)
	}
	if variants, requests := env.srv.HLSUncachedTurns(); variants != 1 || requests != 1 {
		t.Errorf("after the wait %d variants hold a turn for %d requests, want the one run", variants, requests)
	}
	open()
	if a := <-first; a.status != http.StatusOK {
		t.Errorf("the run that held the turn = %d: %.200s", a.status, a.body)
	}
}

// A request that leaves while it waits its turn gives its place back: the
// run in flight is answered, the variant's turn is forgotten once nobody
// holds or waits for it, and the next request runs.
func TestHLSUncachedWaiterThatLeavesGivesItsPlaceBack(t *testing.T) {
	env := newTestEnv(t, nil)
	_, segURIs := hlsURIs(t, env, rampSegments)
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	open, reached := holdUncachedRuns(t, env)
	first := answered(t, env, segURIs[1], 10*time.Second)
	within(t, reached, "the first run to hold the turn")
	turns := func(variants, requests int) func() bool {
		return func() bool {
			v, r := env.srv.HLSUncachedTurns()
			return v == variants && r == requests
		}
	}
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	leaving := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		env.srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hls/"+segURIs[2], nil).WithContext(ctx))
		leaving <- w
	}()
	waitFor(t, "the second request to wait its turn", turns(1, 2))
	leave()
	select {
	case w := <-leaving:
		if w.Body.Len() != 0 {
			t.Errorf("a request that left got %d: %.200s", w.Code, w.Body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a request whose client left is still waiting its turn")
	}
	waitFor(t, "the request that left to give its place back", turns(1, 1))
	open()
	if a := <-first; a.status != http.StatusOK {
		t.Errorf("the run that held the turn = %d: %.200s", a.status, a.body)
	}
	waitFor(t, "the variant's turn to be forgotten", turns(0, 0))
	if a := <-answered(t, env, segURIs[2], 10*time.Second); a.status != http.StatusOK {
		t.Errorf("the next request = %d: %.200s", a.status, a.body)
	}
	waitFor(t, "the variant's turn to be forgotten again", turns(0, 0))
}

// testClock is a clock a test moves, for a daemon that reads it from other
// goroutines.
type testClock struct{ nanos atomic.Int64 }

func newTestClock(env *testEnv) *testClock {
	c := &testClock{}
	c.nanos.Store(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC).UnixNano())
	env.srv.SetHLSClock(func() time.Time { return time.Unix(0, c.nanos.Load()) })
	return c
}

func (c *testClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// uncachedLines counts the segments rec saw served uncached.
func uncachedLines(rec *server.LogRecorder) int {
	n := 0
	for _, r := range rec.Records() {
		if r.Msg == servedUncached {
			n++
		}
	}
	return n
}

// A variant known to refuse writes holds one live slot at a time, so one
// slot serves any number of its listeners at once, and of two slots one is
// always free while it works.
func TestHLSKnownBrokenVariantServesEveryRequestOnOneSlot(t *testing.T) {
	for _, fault := range cacheFaults[1:] {
		for _, slots := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, %d live slots", fault.name, slots), func(t *testing.T) {
				env := newTestEnv(t, func(cfg *server.Config) { cfg.LiveSlots = slots })
				newTestClock(env) // never moved: no probe comes due
				initURI, segURIs := hlsURIs(t, env, rampSegments)
				fault.inject(t, env, initURI, len(segURIs))
				fetchSegment(t, env, segURIs[0]) // a worker fails: the variant is known
				// Whoever publishes holds a slot. With two, the other must
				// be free: nothing else of this variant may hold it.
				var both atomic.Int32
				if slots == 2 {
					env.srv.GateHLSPublish(func(context.Context, int64) {
						release, free := env.srv.HoldLiveSlot()
						if !free {
							both.Add(1)
							return
						}
						time.Sleep(time.Millisecond)
						release()
					})
				}
				for round := range 2 {
					answers := make([]<-chan answer, 0, 7)
					for _, uri := range segURIs[1:8] {
						answers = append(answers, answered(t, env, uri, 20*time.Second))
					}
					for i, a := range answers {
						if a := <-a; a.status != http.StatusOK {
							t.Errorf("round %d, segment %d = %d: %.200s", round, i+1, a.status, a.body)
						}
					}
				}
				if n := both.Load(); n != 0 {
					t.Errorf("the variant held both live slots at %d of its publishes", n)
				}
			})
		}
	}
}

// Between two probes a variant known to refuse writes starts no worker:
// its requests are served uncached without one. The probe's worker, refused
// again, says so at Debug.
func TestHLSKnownBrokenVariantStartsNoWorkerBetweenProbes(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	clock := newTestClock(env)
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	cacheFaults[1].inject(t, env, initURI, len(segURIs))
	for _, uri := range segURIs {
		fetchSegment(t, env, uri)
	}
	waitForIdle(t, env)
	if n := env.srv.Metrics().SessionsHLS.Load(); n != 1 {
		t.Errorf("%d workers started for %d segments, want the one that found the fault", n, len(segURIs))
	}
	if n := uncachedLines(rec); n != len(segURIs) {
		t.Errorf("%d of %d segments were served uncached", n, len(segURIs))
	}

	clock.advance(server.HLSProbeEvery)
	fetchSegment(t, env, segURIs[3])
	fetchSegment(t, env, segURIs[4])
	waitForIdle(t, env)
	if n := env.srv.Metrics().SessionsHLS.Load(); n != 2 {
		t.Errorf("%d workers started, want the first and one probe", n)
	}
	var failed []slog.Level
	for _, r := range rec.Records() {
		if r.Msg == "hls worker failed" {
			failed = append(failed, r.Level)
		}
	}
	if !slices.Equal(failed, []slog.Level{slog.LevelWarn, slog.LevelDebug}) {
		t.Errorf("the workers' failures logged at %v, want a Warn and then a Debug", failed)
	}
}

// Requests in flight when a variant's first write is refused do not each
// start a worker of their own to fail the same way.
func TestHLSFirstRefusedWriteIsNotRetriedByTheRequestsWaitingOnIt(t *testing.T) {
	env := newTestEnv(t, nil)
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	cacheFaults[2].inject(t, env, initURI, len(segURIs))
	// The first worker is held with its first segment made, not yet refused.
	held, open := make(chan struct{}), make(chan struct{})
	var first sync.Once
	env.srv.GateHLSPublish(func(ctx context.Context, _ int64) {
		first.Do(func() {
			close(held)
			select {
			case <-open:
			case <-ctx.Done():
			}
		})
	})
	release := sync.OnceFunc(func() { close(open) })
	t.Cleanup(release)
	a := answered(t, env, segURIs[1], 20*time.Second)
	within(t, held, "the first worker to make a segment")
	b := answered(t, env, segURIs[2], 20*time.Second)
	waitFor(t, "the second request to wait on that worker", func() bool { return env.srv.HLSWaiters() == 2 })
	release()
	for name, got := range map[string]answer{"first": <-a, "second": <-b} {
		if got.status != http.StatusOK {
			t.Errorf("the %s request = %d: %.200s", name, got.status, got.body)
		}
	}
	waitForIdle(t, env)
	if n := env.srv.Metrics().SessionsHLS.Load(); n != 1 {
		t.Errorf("%d workers started, want the one whose write was refused", n)
	}
}

// A cache that heals is noticed at the variant's next probe: until then its
// requests are served uncached with no worker, and from the probe on they
// are served from disk.
func TestHLSNoticesAHealedCacheAtItsNextProbe(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	clock := newTestClock(env)
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	variant := bornVariant(t, env, initURI)
	fileInPlaceOf(t, variant)
	workers := func() uint64 { return env.srv.Metrics().SessionsHLS.Load() }
	want := func(when string, wantWorkers uint64, wantUncached int) {
		t.Helper()
		waitForIdle(t, env)
		if a, b := workers(), uncachedLines(rec); a != wantWorkers || b != wantUncached {
			t.Fatalf("%s: %d workers and %d segments served uncached, want %d and %d", when, a, b, wantWorkers, wantUncached)
		}
	}
	fetchSegment(t, env, segURIs[0])
	fetchSegment(t, env, segURIs[1])
	want("broken", 1, 2)

	// Healed, and not yet asked: the interval has not passed.
	if err := os.Remove(variant); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(variant, 0o755); err != nil {
		t.Fatal(err)
	}
	clock.advance(server.HLSProbeEvery - time.Second)
	fetchSegment(t, env, segURIs[2])
	want("healed, within the interval", 1, 3)

	// From the probe on, what a healthy daemon serves, from disk.
	twin := newTestEnv(t, nil)
	_, twinSegs := hlsURIs(t, twin, rampSegments)
	clock.advance(time.Second)
	for n := 3; n < len(segURIs); n++ {
		got, healthy := keyless(t, env, "/hls/"+segURIs[n]), keyless(t, twin, "/hls/"+twinSegs[n])
		body, wantBody := readBody(t, got), readBody(t, healthy)
		if got.StatusCode != http.StatusOK || got.Header.Get("Accept-Ranges") != "bytes" || !bytes.Equal(body, wantBody) {
			t.Fatalf("segment %d = %d, %d bytes, Accept-Ranges %q; want a healthy daemon's %d bytes from disk",
				n, got.StatusCode, len(body), got.Header.Get("Accept-Ranges"), len(wantBody))
		}
		if n == 3 {
			want("the probe", 2, 3)
		}
	}
	want("after the probe", 2, 3)
}

// probeDeadlines stands in for the timer behind a probe's bound: it keeps
// the bound each probe asked for and passes it only when a test says so.
type probeDeadlines struct {
	mu    sync.Mutex
	asked []time.Duration
	pass  []context.CancelCauseFunc
}

// passedDeadline reads as a context whose deadline passed once it is fired.
type passedDeadline struct{ context.Context }

func (c passedDeadline) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func newProbeDeadlines(env *testEnv) *probeDeadlines {
	d := &probeDeadlines{}
	env.srv.SetHLSProbeDeadline(func(parent context.Context, bound time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancelCause(parent)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.asked, d.pass = append(d.asked, bound), append(d.pass, cancel)
		return passedDeadline{ctx}, func() { cancel(nil) }
	})
	return d
}

// probes is the bound each probe so far asked for.
func (d *probeDeadlines) probes() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.asked)
}

// fire passes the latest probe's bound.
func (d *probeDeadlines) fire(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pass) == 0 {
		t.Fatal("no probe has asked for a bound")
	}
	d.pass[len(d.pass)-1](context.DeadlineExceeded)
}

// probed is a daemon whose one variant is known to refuse its segments and
// is due a probe. No probe's bound passes unless the test fires it.
type probed struct {
	env       *testEnv
	rec       *server.LogRecorder
	clock     *testClock
	deadlines *probeDeadlines
	variant   string // the variant's directory
	segs      []string
	// hold makes the next worker or run stop at its publish, before its
	// write, until its context ends or, if it ignores that, until open.
	// The channel it returns closes when that one is held.
	hold func() <-chan struct{}
	open func()
}

// probedVariant makes one. A stubborn hold ignores its worker's cancel, as
// a write stalled in the system does.
func probedVariant(t *testing.T, params func(*testing.T, *testEnv) map[string]string, mutate func(*server.Config), stubborn bool) *probed {
	t.Helper()
	env, rec := recordingEnv(t, mutate)
	p := &probed{env: env, rec: rec, clock: newTestClock(env), deadlines: newProbeDeadlines(env)}
	var next atomic.Pointer[chan struct{}]
	open := make(chan struct{})
	env.srv.GateHLSPublish(func(ctx context.Context, _ int64) {
		held := next.Swap(nil)
		if held == nil {
			return
		}
		close(*held)
		done := ctx.Done()
		if stubborn {
			done = nil
		}
		select {
		case <-done:
		case <-open:
		}
	})
	p.hold = func() <-chan struct{} {
		held := make(chan struct{})
		next.Store(&held)
		return held
	}
	p.open = sync.OnceFunc(func() { close(open) })
	t.Cleanup(p.open)
	var initURI string
	initURI, p.segs = hlsURIs(t, env, params(t, env))
	p.variant = bornVariant(t, env, initURI)
	cacheFaults[2].inject(t, env, initURI, len(p.segs))
	fetchSegment(t, env, p.segs[0]) // a worker fails: the variant is known
	waitForIdle(t, env)
	p.clock.advance(server.HLSProbeEvery)
	return p
}

// heal takes the fault away: the variant's segments can be written again.
func (p *probed) heal(t *testing.T) {
	t.Helper()
	for n := range p.segs {
		if err := os.RemoveAll(filepath.Join(p.variant, fmt.Sprintf("seg-%d.m4s", n))); err != nil {
			t.Fatal(err)
		}
	}
}

func rampFLAC(*testing.T, *testEnv) map[string]string { return rampSegments }

// linesAt counts the lines rec holds with msg, by level.
func linesAt(rec *server.LogRecorder, msg string) (warn, debug int) {
	for _, r := range rec.Records() {
		switch {
		case r.Msg != msg:
		case r.Level == slog.LevelWarn:
			warn++
		case r.Level == slog.LevelDebug:
			debug++
		}
	}
	return warn, debug
}

// healthySegments is what a healthy daemon serves for params, segment by
// segment.
func healthySegments(t *testing.T, params map[string]string) [][]byte {
	t.Helper()
	twin := newTestEnv(t, nil)
	initURI, segURIs := hlsURIs(t, twin, params)
	return playHLS(t, twin, initURI, segURIs).segs
}

// A probe whose worker hangs costs its listener the probe's bound and no
// more: the segment is then made for the request, as between probes. The
// first probe given up in an outage says so at Warn, with how long it
// waited, and the next at Debug.
func TestHLSProbeThatHangsLeavesItsListenerTheUncachedSegment(t *testing.T) {
	p := probedVariant(t, rampFLAC, nil, false)
	env, segURIs := p.env, p.segs
	healthy := healthySegments(t, rampSegments)
	for n := 1; n <= 2; n++ {
		held := p.hold()
		probing := answered(t, env, segURIs[n], 10*time.Second)
		within(t, held, "the probe's worker to hang at its write")
		p.deadlines.fire(t)
		a := <-probing
		if a.status != http.StatusOK || !bytes.Equal(a.body, healthy[n]) || a.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("the probing listener of segment %d got %d, %d bytes, %v; want a healthy daemon's %d, uncached",
				n, a.status, len(a.body), a.header, len(healthy[n]))
		}
		waitForIdle(t, env)
		if !liveSlotFree(env) {
			t.Error("the probe that was given up kept a live slot")
		}
		p.clock.advance(server.HLSProbeEvery)
	}
	if warn, debug := linesAt(p.rec, server.HLSProbeGivenUp); warn != 1 || debug != 1 {
		t.Errorf("two probes given up logged %d warnings and %d debug lines, want one of each", warn, debug)
	}
	for _, r := range p.rec.Records() {
		// The floor: these segments last half a second.
		if r.Msg == server.HLSProbeGivenUp && !strings.Contains(r.Attrs, "wait=2s") {
			t.Errorf("the line %v does not say how long the probe was given", r)
		}
	}
}

// A probe given up at its bound gives its live slot back at once, while
// its worker is still stalled in the write, so the listener's own segment
// can be made on a pool of one.
func TestHLSProbeGivenUpFreesItsSlotWhileItsWorkerHangs(t *testing.T) {
	p := probedVariant(t, rampFLAC, func(cfg *server.Config) { cfg.LiveSlots = 1 }, true)
	held := p.hold()
	probing := answered(t, p.env, p.segs[1], 10*time.Second)
	within(t, held, "the probe's worker to stall at its write")
	p.deadlines.fire(t)
	if a := <-probing; a.status != http.StatusOK {
		t.Errorf("the probing listener = %d: %.200s", a.status, a.body)
	}
	if n := p.env.srv.Metrics().SessionsActive.Load(); n != 1 {
		t.Fatalf("%d workers are running, want the stalled one still held", n)
	}
	if !liveSlotFree(p.env) {
		t.Error("the stalled worker of a probe given up still holds the live slot")
	}
	p.open()
	waitForIdle(t, p.env)
}

// A probe's bound is as long as one of its variant's segments lasts where
// that is longer than the floor, so a worker slower than the floor still
// heals its variant. No timer runs here: the bound passes only if fired,
// and the test reads what the probe asked for.
func TestHLSProbeWaitsAsLongAsASegmentLasts(t *testing.T) {
	params := map[string]string{"src": "lib/ramp.wav", "format": "flac", "segDur": "1"}
	p := probedVariant(t, func(*testing.T, *testEnv) map[string]string { return params }, nil, true)
	const floor = 50 * time.Millisecond
	p.env.srv.SetHLSProbeWait(floor)
	healthy := healthySegments(t, params)
	p.heal(t)
	held := p.hold()
	probing := answered(t, p.env, p.segs[1], 10*time.Second)
	within(t, held, "the probe's worker to reach its write")
	// One second, snapped to whole FLAC frames.
	if asked := p.deadlines.probes(); len(asked) != 1 || asked[0] < time.Second || asked[0] > 1100*time.Millisecond {
		t.Fatalf("the probe asked for %v, want one bound of a segment's second and not the floor's %v", asked, floor)
	}
	p.open()
	if a := <-probing; a.status != http.StatusOK || !bytes.Equal(a.body, healthy[1]) || a.header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("the probing listener = %d, %d bytes, %v; want the worker's segment from disk", a.status, len(a.body), a.header)
	}
	if n := uncachedLines(p.rec); n != 1 {
		t.Errorf("%d segments were served uncached, want the one before the probe", n)
	}
}

// A probe ends with its listener: the worker it started is stopped before
// its first write, so the variant's one live slot is free for the next.
func TestHLSProbeEndsWithItsListener(t *testing.T) {
	p := probedVariant(t, rampFLAC, func(cfg *server.Config) { cfg.LiveSlots = 1 }, false)
	env, segURIs := p.env, p.segs
	leaveDuringTheProbe(t, p, segURIs[1])
	if a := <-answered(t, env, segURIs[2], 10*time.Second); a.status != http.StatusOK {
		t.Errorf("the next listener of the variant = %d: %.200s", a.status, a.body)
	}
	waitForIdle(t, env)
	if !liveSlotFree(env) {
		t.Error("the variant kept the live slot")
	}
}

// leaveDuringTheProbe asks for uri as a listener who leaves once the
// probe's worker is held, and returns when the handler has.
func leaveDuringTheProbe(t *testing.T, p *probed, uri string) {
	t.Helper()
	held := p.hold()
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	left := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.env.srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hls/"+uri, nil).WithContext(ctx))
		left <- w
	}()
	within(t, held, "the probe's worker to reach its write")
	leave()
	select {
	case w := <-left:
		if w.Body.Len() != 0 {
			t.Errorf("a listener who left got %d: %.200s", w.Code, w.Body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not see its listener leave")
	}
}

// A listener who leaves during a probe starts no run of its own: nothing
// is made for it and no line says a segment was served.
func TestHLSListenerLeavingDuringAProbeStartsNoRun(t *testing.T) {
	p := probedVariant(t, func(t *testing.T, env *testEnv) map[string]string {
		return map[string]string{"src": writeRampOpus(t, env), "format": "opus", "segDur": "1"}
	}, nil, false)
	met := p.env.srv.Metrics()
	// The remux rung counts each pipeline it starts, a run's as a worker's.
	before, served := met.Remuxes.Load(), uncachedLines(p.rec)
	leaveDuringTheProbe(t, p, p.segs[1])
	waitForIdle(t, p.env)
	if n := met.Remuxes.Load() - before; n != 1 {
		t.Errorf("%d pipelines started for a listener who left during its probe, want the probe's worker alone", n)
	}
	if n := uncachedLines(p.rec) - served; n != 0 {
		t.Errorf("%d segments were logged as served to a listener who left", n)
	}
}

// A probe whose worker wrote its segment as the bound passed was not given
// up: the cache healed. The variant is no longer known to refuse writes,
// nothing says a probe hung, the probing request and those after it read a
// healthy daemon's bytes from disk, and a request already waiting on that
// worker gets its segment, never the probe's deadline.
func TestHLSProbeWhoseWorkerWroteAsItsBoundPassedHealedTheVariant(t *testing.T) {
	p := probedVariant(t, rampFLAC, nil, false)
	env, segURIs := p.env, p.segs
	healthy := healthySegments(t, rampSegments)
	p.heal(t)
	// The probe's worker is held with its first segment written and not
	// yet announced.
	written, announce := make(chan struct{}), make(chan struct{})
	var first sync.Once
	env.srv.GateHLSPublished(func(ctx context.Context, _ int64) {
		first.Do(func() {
			close(written)
			select {
			case <-announce:
			case <-ctx.Done():
			}
		})
	})
	release := sync.OnceFunc(func() { close(announce) })
	t.Cleanup(release)
	fromDisk := func(what string, n int, a answer) {
		t.Helper()
		if a.status != http.StatusOK || !bytes.Equal(a.body, healthy[n]) || a.header.Get("Accept-Ranges") != "bytes" {
			t.Errorf("%s = %d, %d bytes, Accept-Ranges %q: %.200s; want a healthy daemon's %d bytes from disk",
				what, a.status, len(a.body), a.header.Get("Accept-Ranges"), a.body, len(healthy[n]))
		}
	}
	probing := answered(t, env, segURIs[1], 10*time.Second)
	within(t, written, "the probe's worker to write its segment")
	// The write made the variant an ordinary one, so this request asks the
	// manager and waits on the same worker.
	between := answered(t, env, segURIs[2], 10*time.Second)
	waitFor(t, "the second request to wait on the worker", func() bool { return env.srv.HLSWaiters() == 2 })
	p.deadlines.fire(t)
	fromDisk("the probing request", 1, <-probing)
	release()
	fromDisk("the request waiting on the worker", 2, <-between)
	waitForIdle(t, env)
	for n := 3; n < len(segURIs); n++ {
		fromDisk(fmt.Sprintf("segment %d after", n), n, <-answered(t, env, segURIs[n], 10*time.Second))
	}
	if warn, debug := linesAt(p.rec, server.HLSProbeGivenUp); warn+debug != 0 {
		t.Errorf("a probe that healed its variant logged %d given-up lines", warn+debug)
	}
	if n := env.srv.HLSBrokenVariants(); n != 0 {
		t.Errorf("%d variants are known to refuse writes after the heal", n)
	}
	if n := uncachedLines(p.rec); n != 1 {
		t.Errorf("%d segments were served uncached, want the one before the probe", n)
	}
}

// The init header follows the segments' rules in an outage: a refusal met
// there marks the variant and is one Warn, later requests are answered from
// memory with no write tried, the variant's segments go uncached with no
// worker started to fail, and once a probe's worker writes, the header is
// cached again.
func TestHLSInitInAnOutageWarnsOnceAndTriesTheWriteOnce(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	clock := newTestClock(env)
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	want := readBody(t, keyless(t, env, "/hls/"+initURI))
	variant := bornVariant(t, env, initURI)
	// A directory that is not empty where the header goes: it is no cached
	// header, and the rename onto it is refused.
	header := filepath.Join(variant, "init.mp4")
	if err := os.Remove(header); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(header, "kept"), 0o755); err != nil {
		t.Fatal(err)
	}
	init := func() {
		t.Helper()
		resp := keyless(t, env, "/hls/"+initURI)
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
			t.Fatalf("the init header = %d and %d bytes, want the %d a healthy cache serves", resp.StatusCode, len(body), len(want))
		}
	}
	init()
	// With what refused the write gone, a second try would land the header.
	if err := os.RemoveAll(header); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		init()
	}
	if warn, debug := linesAt(rec, server.HLSInitUncached); warn != 1 || debug != 5 {
		t.Errorf("six init requests in an outage logged %d warnings and %d debug lines, want 1 and 5", warn, debug)
	}
	if _, err := os.Stat(header); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the header's write was tried again for a variant known to refuse it: stat %v", err)
	}

	fetchSegment(t, env, segURIs[0])
	fetchSegment(t, env, segURIs[1])
	waitForIdle(t, env)
	if workers, uncached := env.srv.Metrics().SessionsHLS.Load(), uncachedLines(rec); workers != 0 || uncached != 2 {
		t.Errorf("after the init path's refusal %d workers started and %d segments were served uncached, want 0 and 2", workers, uncached)
	}

	// The next probe's worker writes the header with its segments.
	clock.advance(server.HLSProbeEvery)
	fetchSegment(t, env, segURIs[2])
	waitForIdle(t, env)
	init()
	if fi, err := os.Stat(header); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the healed variant holds no cached init header: %v", err)
	}
	if warn, debug := linesAt(rec, server.HLSInitUncached); warn != 1 || debug != 5 {
		t.Errorf("after the heal the init path has logged %d warnings and %d debug lines, want still 1 and 5", warn, debug)
	}
}

// With no variant directory to be made the init header is still one Warn
// an outage.
func TestHLSInitWithNoVariantDirectoryWarnsOnce(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	newTestClock(env)
	initURI, _ := hlsURIs(t, env, rampSegments)
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	for range 5 {
		if resp := keyless(t, env, "/hls/"+initURI); resp.StatusCode != http.StatusOK {
			t.Fatalf("the init header = %d: %.200s", resp.StatusCode, readBody(t, resp))
		} else {
			resp.Body.Close()
		}
	}
	if warn, debug := linesAt(rec, server.HLSInitUncached); warn != 1 || debug != 4 {
		t.Errorf("five init requests logged %d warnings and %d debug lines, want 1 and 4", warn, debug)
	}
}

// An outage across more variants than are remembered costs no worker and
// no Warn a request: past the bound the outage is taken to be the store's.
func TestHLSOutageWiderThanIsRememberedCostsNoWorkerOrWarnARequest(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	env.srv.SetHLSBrokenMax(2)
	clock := newTestClock(env)
	var segs [][]string
	for _, dur := range []string{"0.5", "1", "2"} {
		initURI, segURIs := hlsURIs(t, env, map[string]string{"src": "lib/ramp.wav", "format": "flac", "segDur": dur})
		// Born, so that a worker would start for it.
		readBody(t, keyless(t, env, "/hls/"+initURI))
		segs = append(segs, segURIs)
	}
	fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
	met := env.srv.Metrics()
	warnings := func() int { return len(rec.Warnings()) }
	round := func(n int) {
		t.Helper()
		for v, segURIs := range segs {
			if a := <-answered(t, env, segURIs[n%len(segURIs)], 10*time.Second); a.status != http.StatusOK {
				t.Fatalf("variant %d, round %d = %d: %.200s", v, n, a.status, a.body)
			}
		}
		waitForIdle(t, env)
	}
	round(0)
	workers, warned := met.SessionsHLS.Load(), warnings()
	if workers != 3 {
		t.Fatalf("%d workers started for three variants' first refusals, want 3", workers)
	}
	// A refused write and a segment served uncached, said by each of the
	// two variants remembered. The third dropped the first, and what that
	// one had said the outage has said.
	if warned != 4 {
		t.Fatalf("three variants' first refusals logged %d warnings, want the 4 of the two remembered: %v", warned, rec.Warnings())
	}
	for n := 1; n <= 4; n++ {
		round(n)
	}
	if w, l := met.SessionsHLS.Load(), warnings(); w != workers || l != warned {
		t.Errorf("twelve more requests started %d workers and logged %d warnings, want none of either: %v", w-workers, l-warned, rec.Warnings())
	}
	// A probe interval on, the remembered variants and the rest between
	// them try once each, however many requests come.
	clock.advance(server.HLSProbeEvery)
	for n := 5; n <= 7; n++ {
		round(n)
	}
	if w, l := met.SessionsHLS.Load(), warnings(); w-workers > 3 || l != warned {
		t.Errorf("nine requests past a probe interval started %d workers and logged %d warnings, want three workers at most and no warning", w-workers, l-warned)
	}
}

// A write that lands ends an outage wider than is remembered: a variant
// never seen is then an ordinary one, served from disk by a worker.
func TestAWriteEndsAnOutageWiderThanIsRemembered(t *testing.T) {
	env, _ := recordingEnv(t, nil)
	env.srv.SetHLSBrokenMax(2)
	clock := newTestClock(env)
	variant := func(dur string) map[string]string {
		return map[string]string{"src": "lib/ramp.wav", "format": "flac", "segDur": dur}
	}
	var segs [][]string
	for _, dur := range []string{"0.5", "1", "2"} {
		initURI, segURIs := hlsURIs(t, env, variant(dur))
		readBody(t, keyless(t, env, "/hls/"+initURI))
		segs = append(segs, segURIs)
	}
	version := filepath.Join(env.cache, "v1")
	dirs, err := filepath.Glob(filepath.Join(version, "*", "*"))
	if err != nil || len(dirs) != 3 {
		t.Fatalf("the cache holds the variants %v (%v), want three", dirs, err)
	}
	uncached := func(what, uri string) {
		t.Helper()
		if a := <-answered(t, env, uri, 10*time.Second); a.status != http.StatusOK || a.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s = %d with %v, want it served uncached", what, a.status, a.header)
		}
		waitForIdle(t, env)
	}
	fileInPlaceOf(t, version)
	for v, segURIs := range segs {
		uncached(fmt.Sprintf("variant %d in the outage", v), segURIs[0])
	}
	// The first variant was dropped for the third. Its try, a probe
	// interval on, is refused, and stands for every dropped variant's.
	clock.advance(server.HLSProbeEvery)
	uncached("the dropped variant's try", segs[0][1])

	// Healed. Of the other two, the one still remembered is due a try of
	// its own, and its worker's write lands.
	if err := os.Remove(version); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, segURIs := range segs[1:] {
		fetchSegment(t, env, segURIs[1])
	}
	waitForIdle(t, env)
	written, _ := filepath.Glob(filepath.Join(version, "*", "*", "seg-*.m4s"))
	if len(written) == 0 {
		t.Fatal("no worker's write landed after the cache healed")
	}

	// Never seen, and asked for inside the dropped variants' interval.
	initURI, segURIs := hlsURIs(t, env, variant("0.25"))
	for what, uri := range map[string]string{"init header": initURI, "segment": segURIs[0]} {
		resp := keyless(t, env, "/hls/"+uri)
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") == "no-store" || resp.Header.Get("ETag") == "" {
			t.Errorf("a new variant's %s = %d with %v, want it from the cache: %.100s", what, resp.StatusCode, resp.Header, body)
		}
	}
	waitForIdle(t, env)
	if all, _ := filepath.Glob(filepath.Join(version, "*", "*")); len(all) != 4 {
		t.Errorf("the cache holds %d variants, want the new one beside the three", len(all))
	}
}

// A variant the cache cannot make is asked of it once a probe interval,
// counted from the last refusal: between tries no directory is made even
// where one could be.
func TestHLSVariantTheCacheCannotMakeIsTriedOnceAnInterval(t *testing.T) {
	env, _ := recordingEnv(t, nil)
	clock := newTestClock(env)
	initURI, segURIs := hlsURIs(t, env, rampSegments)
	version := filepath.Join(env.cache, "v1")
	ask := func(n int) *http.Response {
		t.Helper()
		readBody(t, keyless(t, env, "/hls/"+initURI))
		resp := keyless(t, env, "/hls/"+segURIs[n])
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("segment %d = %d: %.200s", n, resp.StatusCode, body)
		}
		waitForIdle(t, env)
		return resp
	}
	made := func() bool {
		_, err := os.Stat(version)
		return err == nil
	}
	fileInPlaceOf(t, version)
	ask(0)
	// Healed inside the interval: the cache is not asked.
	if err := os.Remove(version); err != nil {
		t.Fatal(err)
	}
	if ask(1); made() {
		t.Fatal("the cache was asked for the variant again before its next try was due")
	}
	// A try that is refused moves the next one.
	fileInPlaceOf(t, version)
	clock.advance(server.HLSProbeEvery)
	ask(2)
	if err := os.Remove(version); err != nil {
		t.Fatal(err)
	}
	clock.advance(server.HLSProbeEvery - time.Second)
	if ask(3); made() {
		t.Fatal("the cache was asked again less than an interval after it last refused")
	}
	clock.advance(time.Second)
	if resp := ask(4); !made() || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("an interval after the last refusal the variant is made: %v, and the segment came with %v; want it made and read from disk", made(), resp.Header)
	}
}
