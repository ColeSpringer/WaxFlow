// The progressive timeline suite: a minted tl served on /stream as one live
// encode over every member, gapless at each seam, seekable on the timeline's
// clock, cached and signed like any other /stream.
package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// tlStream is the /stream URL of a minted timeline in format, with gain=off,
// since the fixture daemon's default gain is a tag mode a timeline refuses.
func tlStream(tl, format string) string {
	return "/stream?tl=" + tl + "&format=" + format + "&gain=off"
}

// memberFile is a library reference's path on disk.
func memberFile(env *testEnv, ref string) string {
	return filepath.Join(env.root, strings.TrimPrefix(ref, "lib/"))
}

// timelinePCM decodes each member and lays them end to end: what a gapless
// timeline of them delivers, derived without the timeline code.
func timelinePCM(t *testing.T, env *testEnv, refs []string) *audio.Buffer {
	t.Helper()
	var parts []*audio.Buffer
	total := 0
	for _, ref := range refs {
		raw, err := os.ReadFile(memberFile(env, ref))
		if err != nil {
			t.Fatal(err)
		}
		b := decodePCM(t, raw)
		parts = append(parts, b)
		total += b.N
	}
	out := audio.Get(parts[0].Fmt, total)
	t.Cleanup(func() { audio.Put(out) })
	out.N = 0
	for _, b := range parts {
		audio.CopyFrames(out, out.N, b, 0, b.N)
		out.N += b.N
	}
	return out
}

// wantFramesFrom fails unless got is want from frame from on, sample for sample.
func wantFramesFrom(t *testing.T, got, want *audio.Buffer, from int) {
	t.Helper()
	if got.N != want.N-from {
		t.Fatalf("the stream holds %d frames, want %d", got.N, want.N-from)
	}
	for c := range want.Fmt.Channels {
		w, g := want.ChanI(c)[from:want.N], got.ChanI(c)[:got.N]
		for i := range w {
			if w[i] != g[i] {
				t.Fatalf("ch%d frame %d (timeline frame %d) = %d, want %d", c, i, from+i, g[i], w[i])
			}
		}
	}
}

// TestStreamTimelineSeamIsGapless is the headline claim: one encode over every
// member, decoded, is the members' own decodes laid end to end. FLAC at the
// members' rate makes it bit for bit, so a sample lost, doubled or primed at
// either seam fails.
func TestStreamTimelineSeamIsGapless(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	resp := env.get(t, tlStream(tl.Tl, "flac"), nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("/stream?tl = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Content-Duration"); got != "3.000" {
		t.Errorf("X-Content-Duration = %q, want the timeline's 3.000", got)
	}
	wantFramesFrom(t, decodePCM(t, body), timelinePCM(t, env, refs), 0)
}

// TestStreamTimelineSeekCrossesASeam pins t= on the timeline's clock: 1.25 s is
// sample 60000, which is 12000 samples into the second member, and the stream
// from there is the rest of the timeline, exactly.
func TestStreamTimelineSeekCrossesASeam(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	resp := env.get(t, tlStream(tl.Tl, "flac")+"&t=1.25", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("/stream?tl&t = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Content-Duration"); got != "1.750" {
		t.Errorf("X-Content-Duration = %q, want the 1.750 s left after the seek", got)
	}
	wantFramesFrom(t, decodePCM(t, body), timelinePCM(t, env, refs), 60000)

	// Past the end is a valid empty stream, as on a single source.
	resp = env.get(t, tlStream(tl.Tl, "flac")+"&t=10", nil)
	body = readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Content-Duration") != "0.000" {
		t.Fatalf("a seek past the end = %d, X-Content-Duration %q: %s",
			resp.StatusCode, resp.Header.Get("X-Content-Duration"), body)
	}
	med, err := waxflow.New().OpenStream(container.BytesSource(body), "")
	if err != nil {
		t.Fatalf("the empty stream does not open: %v", err)
	}
	defer med.Close()
	if n := decodedMediaFrames(t, med); n != 0 {
		t.Errorf("a seek past the end decodes to %d frames, want none", n)
	}
}

// TestStreamTimelineSeeksOnTheEnvelopesClock pins which clock t= counts on when
// the output resamples: the timeline's, at the envelope's 44.1 kHz, not the
// 48 kHz Opus delivers, so 0.5 s into 1.5 s leaves exactly 1 s.
func TestStreamTimelineSeeksOnTheEnvelopesClock(t *testing.T) {
	env, _ := timelineEnv(t)
	refs := []string{"lib/a44.wav", "lib/b44.wav"}
	for i, n := range []int{44100, 22050} {
		if err := os.WriteFile(memberFile(env, refs[i]), rampWAV(t, 44100, 2, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tl := mintTimeline(t, env, refs)
	resp := env.get(t, tlStream(tl.Tl, "opus")+"&t=0.5", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("/stream?tl&t = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Content-Duration"); got != "1.000" {
		t.Errorf("X-Content-Duration = %q, want the 1.000 s left after 0.5 s of 1.5", got)
	}
}

// TestStreamTimelineAppliesItsGain: an explicit dB, the one gain a timeline
// takes besides off, reaches the encode. -6.0206 dB halves the amplitude, so
// every sample lands within dither of half the reference's.
func TestStreamTimelineAppliesItsGain(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	resp := env.get(t, "/stream?tl="+tl.Tl+"&format=flac&gain=-6.0206", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("/stream?tl&gain = %d: %s", resp.StatusCode, body)
	}
	got, want := decodePCM(t, body), timelinePCM(t, env, refs)
	if got.N != want.N {
		t.Fatalf("the stream holds %d frames, want %d", got.N, want.N)
	}
	for c := range want.Fmt.Channels {
		w, g := want.ChanI(c), got.ChanI(c)
		for i := range want.N {
			if d := g[i] - w[i]/2; d < -2 || d > 2 {
				t.Fatalf("ch%d frame %d = %d, want about %d (half of %d)", c, i, g[i], w[i]/2, w[i])
			}
		}
	}
}

// TestStreamTimelineCacheHit pins write-through: once the encode finishes, the
// same URL serves the cached entry with a real length, and its ETag is the key
// of an entry the timeline's digest names.
func TestStreamTimelineCacheHit(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	stream := tlStream(tl.Tl, "flac")
	first := readBody(t, env.get(t, stream, nil))
	waitForIdle(t, env)

	resp := env.get(t, stream, nil)
	second := readBody(t, resp)
	if resp.Header.Get("Accept-Ranges") != "bytes" || resp.ContentLength != int64(len(first)) || !bytes.Equal(first, second) {
		t.Fatalf("the second GET: Accept-Ranges %q, Content-Length %d, %d bytes; want the cached %d",
			resp.Header.Get("Accept-Ranges"), resp.ContentLength, len(second), len(first))
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if len(etag) < 2 {
		t.Fatalf("ETag = %q, want the cache key", resp.Header.Get("ETag"))
	}
	raw, err := os.ReadFile(filepath.Join(env.cache, "v1", etag[:2], etag, "meta.json"))
	if err != nil {
		t.Fatalf("no cache entry under the ETag's key: %v", err)
	}
	var meta cache.Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if want := "tl:" + tl.Tl; meta.Identity != want || meta.Ref != want {
		t.Errorf("the entry's ref %q and identity %q, want both %q", meta.Ref, meta.Identity, want)
	}
}

// TestStreamTimelineHeadNeverStartsAnEncode: HEAD answers from the plan, as on
// a single source, so a player probing a book's duration starts no encode.
func TestStreamTimelineHeadNeverStartsAnEncode(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	resp := env.req(t, http.MethodHead, tlStream(tl.Tl, "mp3"), nil)
	readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Content-Duration") != "3.000" {
		t.Fatalf("HEAD = %d, X-Content-Duration %q, want 200 and 3.000",
			resp.StatusCode, resp.Header.Get("X-Content-Duration"))
	}
	if n := env.srv.Metrics().SessionsLive.Load(); n != 0 {
		t.Fatalf("HEAD started %d pipeline(s)", n)
	}
}

// TestStreamTimelineMemberChangedIs410 pins the digest's identity promise on
// /stream: a member replaced since the mint is a 410, as on HLS.
func TestStreamTimelineMemberChangedIs410(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	if err := os.WriteFile(memberFile(env, refs[1]), rampWAV(t, 48000, 2, 96000), 0o644); err != nil {
		t.Fatal(err)
	}
	wantEnvelope(t, env.get(t, tlStream(tl.Tl, "flac"), nil), http.StatusGone, waxerr.CodeSourceChanged)
}

// TestStreamTimelineRefusals pins the answers a tl URL shares with HLS: a digest
// the store does not hold is the 404 a client re-mints on, a daemon without
// timelines refuses by name, and a tag-derived gain is refused because one
// chain has one gain.
func TestStreamTimelineRefusals(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	unknown := strings.Repeat("a", len(tl.Tl))

	wantEnvelope(t, env.get(t, tlStream(unknown, "flac"), nil), http.StatusNotFound, waxerr.CodeNotFound)
	off := newTestEnv(t, nil)
	wantEnvelope(t, off.get(t, tlStream(unknown, "flac"), nil), http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat)
	// Before the query's own refusals too: no gain= is the daemon's tag gain.
	wantEnvelope(t, off.get(t, "/stream?tl="+unknown+"&format=flac", nil), http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat)

	wantRefusal(t, env.get(t, "/stream?tl="+tl.Tl+"&format=flac", nil),
		http.StatusBadRequest, waxerr.CodeInvalidRequest, "this daemon's default gain")
	wantRefusal(t, env.get(t, "/stream?tl="+tl.Tl+"&format=flac&gain=album", nil),
		http.StatusBadRequest, waxerr.CodeInvalidRequest, "gain=album")
	wantRefusal(t, env.get(t, tlStream(tl.Tl, "aiff"), nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, "no streaming form")
	if resp := env.get(t, "/stream?tl="+tl.Tl+"&format=flac&gain=-6", nil); resp.StatusCode != 200 {
		t.Errorf("an explicit dB on a timeline = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	} else {
		readBody(t, resp)
	}
}

// TestStreamTimelineCrossfadeKeysApart pins that a blended render is its own
// stream: shorter by a crossfade per seam, and never served the butt-join's
// cached entry, though both share one digest.
func TestStreamTimelineCrossfadeKeysApart(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	butt := tlStream(tl.Tl, "flac")
	readBody(t, env.get(t, butt, nil))
	waitForIdle(t, env)
	wantCached(t, env, butt)

	resp := env.get(t, butt+"&crossfadeSeconds=0.1", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("the blended render = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Content-Duration"); got != "2.800" {
		t.Errorf("X-Content-Duration = %q, want 2.800 (two seams of 0.1 s)", got)
	}
	if got := decodePCM(t, body); got.N != 3*48000-2*4800 {
		t.Errorf("the blended render decodes to %d frames, want %d", got.N, 3*48000-2*4800)
	}
}

// TestStreamTimelineEveryLiveFormat pins that no live muxer is left out: each
// one is handed the timeline's exact projected total before its first byte, so
// each decodes back to the length X-Content-Duration promised.
func TestStreamTimelineEveryLiveFormat(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	for _, o := range waxflow.Outputs() {
		if !o.Live {
			continue
		}
		t.Run(o.Name, func(t *testing.T) {
			resp := env.get(t, tlStream(tl.Tl, o.Name), nil)
			body := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("/stream?tl&format=%s = %d: %s", o.Name, resp.StatusCode, body)
			}
			dur, err := strconv.ParseFloat(resp.Header.Get("X-Content-Duration"), 64)
			if err != nil {
				t.Fatalf("X-Content-Duration %q: %v", resp.Header.Get("X-Content-Duration"), err)
			}
			med, err := waxflow.New().OpenStream(container.BytesSource(body), "")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			rate := med.Info().Default().Fmt.Rate
			n := decodedMediaFrames(t, med)
			if got := float64(n) / float64(rate); math.Abs(got-dur) > 0.0005 {
				t.Errorf("the %s stream decodes to %d frames (%.4f s at %d Hz), X-Content-Duration says %.3f",
					o.Name, n, got, rate, dur)
			}
		})
	}
}

// decodedMediaFrames reads med to its end and counts the frames.
func decodedMediaFrames(t *testing.T, med format.Media) int64 {
	t.Helper()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n += int64(buf.N)
	}
}

// TestStreamTimelineOfOneEqualsSource: a one-member timeline is the member's
// own stream, byte for byte. The source is tagless because the single-source
// path embeds the source's tags and a timeline carries none.
func TestStreamTimelineOfOneEqualsSource(t *testing.T) {
	env, _ := timelineEnv(t)
	tl := mintTimeline(t, env, []string{"lib/ramp.wav"})
	want := readBody(t, env.get(t, "/stream?src=lib/ramp.wav&format=mp3&gain=off", nil))
	resp := env.get(t, tlStream(tl.Tl, "mp3"), nil)
	got := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Equal(got, want) {
		t.Fatalf("/stream?tl of one = %d, %d bytes; want the source's own %d-byte stream", resp.StatusCode, len(got), len(want))
	}
}

// TestStreamTimelineIsTheEngineRun pins the body to the engine's own run of the
// same timeline into a writer that cannot seek, which is what a live encode
// writes into: headers projected from the exact total, and nothing patched.
func TestStreamTimelineIsTheEngineRun(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	e := waxflow.New()
	for _, f := range []string{"mp3", "flac"} {
		t.Run(f, func(t *testing.T) {
			members := make([]waxflow.ConcatSource, len(refs))
			for i, ref := range refs {
				raw, err := os.ReadFile(memberFile(env, ref))
				if err != nil {
					t.Fatal(err)
				}
				info, err := e.Probe(context.Background(), container.BytesSource(raw), "wav", nil)
				if err != nil {
					t.Fatal(err)
				}
				members[i] = waxflow.ConcatSource{Track: info.Default(),
					Open: func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), "wav") }}
			}
			med, err := waxflow.Concat(members, waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			var want bytes.Buffer
			if _, err := e.TranscodeMedia(t.Context(), med, &want, waxflow.TranscodeOptions{Format: f}); err != nil {
				t.Fatal(err)
			}
			got := readBody(t, env.get(t, tlStream(tl.Tl, f), nil))
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("/stream?tl = %d bytes, the engine's run of the same timeline %d; they differ", len(got), want.Len())
			}
		})
	}
}

// stripID3v2 drops a leading ID3v2 tag, whose size is four syncsafe bytes.
func stripID3v2(b []byte) []byte {
	if len(b) < 10 || string(b[:3]) != "ID3" {
		return b
	}
	n := int(b[6])<<21 | int(b[7])<<14 | int(b[8])<<7 | int(b[9])
	if b[5]&0x10 != 0 {
		n += 10 // footer
	}
	return b[10+n:]
}

// infoFrame splits an MPEG stream into its first frame, the Xing/Info frame,
// and the rest, and reads the frame count and the LAME gapless fields from it.
func infoFrame(t *testing.T, b []byte) (frame, rest []byte, frames, delay, padding int64) {
	t.Helper()
	h, err := mp3.ParseHeader(b)
	if err != nil {
		t.Fatalf("the stream does not open on an MPEG frame: %v", err)
	}
	frame, rest = b[:h.Size()], b[h.Size():]
	p := mp3.HeaderLen + h.SideInfoLen()
	if h.Protected {
		p += 2
	}
	if magic := string(frame[p : p+4]); magic != "Xing" && magic != "Info" {
		t.Fatalf("frame 0 carries %q, want a Xing or Info tag", magic)
	}
	flags := binary.BigEndian.Uint32(frame[p+4:])
	p += 8
	if flags&1 == 0 {
		t.Fatal("the Info frame carries no frame count")
	}
	frames = int64(binary.BigEndian.Uint32(frame[p:]))
	p += 4
	for _, f := range []struct {
		bit  uint32
		size int
	}{{2, 4}, {4, 100}, {8, 4}} {
		if flags&f.bit != 0 {
			p += f.size
		}
	}
	// The LAME extension: a 9-byte encoder string, then the delay and padding
	// packed into three bytes 12 further on.
	g := frame[p+9+12:]
	delay = int64(g[0])<<4 | int64(g[1])>>4
	padding = int64(g[1]&0xF)<<8 | int64(g[2])
	return frame, rest, frames, delay, padding
}

// TestStreamTimelineMatchesTheMergeJob pins /stream?tl against the merge job of
// the same members, the file a client would otherwise download: the same
// encode, so every MPEG frame after the first is identical, and the first, the
// Info frame, agrees on everything a player trims by. The two differ only where
// a seekable file can be patched at End and a live stream cannot: the byte
// counts and CRCs a live Info frame leaves 0.
func TestStreamTimelineMatchesTheMergeJob(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
		cfg.TimelineDir = filepath.Join(t.TempDir(), "timelines")
	})
	refs := []string{"lib/m-0.wav", "lib/m-1.wav"}
	for i, n := range []int{48000, 30000} {
		if err := os.WriteFile(memberFile(env, refs[i]), rampWAV(t, 48000, 2, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tl := mintTimeline(t, env, refs)
	live := stripID3v2(readBody(t, env.get(t, tlStream(tl.Tl, "mp3"), nil)))

	body, err := json.Marshal(map[string]any{"type": "merge", "srcs": refs, "format": "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	id := createJob(t, env, string(body))
	awaitJob(t, env, id)
	merged := stripID3v2(readBody(t, env.get(t, "/jobs/"+id+"/result", nil)))

	lf, lrest, lframes, ldelay, lpad := infoFrame(t, live)
	mf, mrest, mframes, mdelay, mpad := infoFrame(t, merged)
	if !bytes.Equal(lf[:mp3.HeaderLen], mf[:mp3.HeaderLen]) {
		t.Errorf("frame 0 headers differ: live % x, merge % x", lf[:mp3.HeaderLen], mf[:mp3.HeaderLen])
	}
	if lframes != mframes || ldelay != mdelay || lpad != mpad {
		t.Errorf("Info frame: live %d frames, delay %d, padding %d; merge %d, %d, %d",
			lframes, ldelay, lpad, mframes, mdelay, mpad)
	}
	if !bytes.Equal(lrest, mrest) {
		t.Errorf("the MPEG frames after the Info frame differ: live %d bytes, merge %d", len(lrest), len(mrest))
	}
}

// wantRefusal asserts an error envelope whose message contains want, which is
// what tells a named refusal from the generic one it replaced.
func wantRefusal(t *testing.T, resp *http.Response, status int, code waxerr.Code, want string) {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, status, body)
	}
	var env server.ErrorBody
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an envelope: %s", body)
	}
	if env.Code != code {
		t.Fatalf("code = %s, want %s (%s)", env.Code, code, env.Error)
	}
	if !strings.Contains(env.Error, want) {
		t.Fatalf("the refusal %q does not say %q", env.Error, want)
	}
}

// TestStreamTimelineAndSrcAreExclusive pins that a /stream URL names one source
// or one timeline, and that the parameters only a single source can mean are
// refused by name on a timeline rather than ignored: a member's window lives in
// the store, and the digest pins every member's identity.
func TestStreamTimelineAndSrcAreExclusive(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	for _, tc := range []struct{ name, extra string }{
		{"src", "&src=" + refs[0]},
		{"from", "&from=10"},
		{"to", "&to=10"},
		{"id", "&id=1-2"},
		{"track", "&track=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.get(t, "/stream?tl="+tl.Tl+"&format=flac&gain=off"+tc.extra, nil)
			wantRefusal(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest, "tl and "+tc.name)
		})
	}
}

// TestStreamTimelineParameterValidation is the tl half of TestParameterValidation:
// a malformed digest and an unusable crossfade fail before anything resolves,
// and a crossfade on a single source is refused, since it has no seam to blend.
func TestStreamTimelineParameterValidation(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	for _, tc := range []struct{ name, query, want string }{
		{"an empty tl", "tl=&format=flac", "tl is present but empty"},
		{"a tl that is no digest", "tl=nope&format=flac", "not a timeline digest"},
		{"a negative crossfade", "tl=" + tl.Tl + "&format=flac&gain=off&crossfadeSeconds=-1", "non-negative"},
		{"a NaN crossfade", "tl=" + tl.Tl + "&format=flac&gain=off&crossfadeSeconds=NaN", "non-negative and finite"},
		{"an empty crossfade", "tl=" + tl.Tl + "&format=flac&gain=off&crossfadeSeconds=", "crossfadeSeconds is present but empty"},
		{"a crossfade that is no number", "tl=" + tl.Tl + "&format=flac&gain=off&crossfadeSeconds=soft", "want seconds"},
		{"a crossfade on one source", "src=lib/sine.wav&format=flac&crossfadeSeconds=1", "a crossfade blends a timeline's seams"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantRefusal(t, env.get(t, "/stream?"+tc.query, nil), http.StatusBadRequest, waxerr.CodeInvalidRequest, tc.want)
		})
	}
}

// TestTranscodeRefusesTimeline pins /transcode's half of the shared parameter
// surface: one long file of a timeline is what a merge job writes, so the
// one-shot refuses a tl by name rather than as an unknown parameter.
func TestTranscodeRefusesTimeline(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	resp := env.req(t, http.MethodPost, "/transcode?tl="+tl.Tl+"&format=flac&gain=off", nil)
	wantRefusal(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest, "merge job")
}

// storedExpiry is when the timeline store will sweep digest, read off disk.
func storedExpiry(t *testing.T, env *testEnv, digest string) time.Time {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.tlDir, digest+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Expires time.Time `json:"expires"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Expires
}

// TestStreamTimelineReadExtendsTheTimeline pins ADR-0009's lifetime rule on
// /stream: a read extends the stored timeline to the expiry of the URL it came
// in on, so no URL that verifies outlives its timeline. The URL is minted
// offline rather than through /sign, which would extend the timeline itself.
func TestStreamTimelineReadExtendsTheTimeline(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	exp := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	signed, err := client.MintURL("1:"+hexEncode(testSecret), "/stream",
		url.Values{"tl": {tl.Tl}, "format": {"flac"}, "gain": {"off"}}, exp)
	if err != nil {
		t.Fatal(err)
	}
	resp := keyless(t, env, signed)
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("the signed /stream?tl = %d: %s", resp.StatusCode, body)
	}
	if got := storedExpiry(t, env, tl.Tl); got.Before(exp) {
		t.Errorf("the stored timeline expires %v, before the %v the URL read through verifies until", got, exp)
	}
}

// TestStreamTimelineSigned pins /sign for a timeline stream: the URL plays with
// no key and pins nothing beside the digest, which pins every member, and /sign
// refuses what playback would, so a URL that signs is one that plays.
func TestStreamTimelineSigned(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := c.Sign(t.Context(), client.SignRequest{Path: "/stream", TTLSeconds: 30 * 24 * 3600,
		Params: map[string]string{"tl": tl.Tl, "format": "flac", "gain": "off"}})
	if err != nil {
		t.Fatalf("signing a timeline stream: %v", err)
	}
	if got, exp := storedExpiry(t, env, tl.Tl), time.Unix(minted.Exp, 0); got.Before(exp) {
		t.Errorf("the stored timeline expires %v, before the %v the signed URL verifies until", got, exp)
	}
	u, err := url.Parse(minted.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Has("id") {
		t.Errorf("the signed URL %q carries an id; a timeline's digest pins its members", minted.URL)
	}
	resp := keyless(t, env, minted.URL)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("the signed /stream?tl = %d: %s", resp.StatusCode, body)
	}
	wantFramesFrom(t, decodePCM(t, body), timelinePCM(t, env, refs), 0)

	for _, tc := range []struct {
		name   string
		params map[string]string
		code   waxerr.Code
	}{
		{"the daemon's tag gain", map[string]string{"tl": tl.Tl, "format": "flac"}, waxerr.CodeInvalidRequest},
		{"an unknown digest", map[string]string{"tl": strings.Repeat("a", len(tl.Tl)), "format": "flac", "gain": "off"},
			waxerr.CodeNotFound},
		{"a format with no streaming form", map[string]string{"tl": tl.Tl, "format": "aiff", "gain": "off"},
			waxerr.CodeUnsupportedFormat},
		{"a crossfade the shortest member cannot carry",
			map[string]string{"tl": tl.Tl, "format": "flac", "gain": "off", "crossfadeSeconds": "0.6"},
			waxerr.CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Sign(t.Context(), client.SignRequest{Path: "/stream", Params: tc.params})
			if got := waxerr.CodeOf(err); err == nil || got != tc.code {
				t.Fatalf("signing = %v (code %q), want a %s refusal", err, got, tc.code)
			}
		})
	}
}

// TestTimelineKeyAuthedReadsRewriteTheStoreOnce: a key-authed session's reads
// extend the stored timeline without rewriting it on every request, on every
// path that touches it. A read's default expiry is rounded up to the hour, so
// a scrubbing client's later reads ask for nothing past what the first stored.
// Each path gets a timeline of its own: one whose expiry an earlier path
// already rounded up would hide a path that does not round.
func TestTimelineKeyAuthedReadsRewriteTheStoreOnce(t *testing.T) {
	env, refs := timelineEnv(t)
	queues := [][]string{{refs[0], refs[1]}, {refs[1], refs[2]}, {refs[2], refs[0]}, refs}
	paths := []func(tl string) []string{
		func(tl string) []string {
			s := tlStream(tl, "flac")
			return []string{s, s + "&t=0.5", s + "&t=1", s + "&t=2"}
		},
		func(tl string) []string { return []string{"/hls/media.m3u8?v=" + masterV(t, env, tl)} },
		func(tl string) []string { return []string{"/hls/master.m3u8?v=" + masterV(t, env, tl)} },
		func(tl string) []string { return []string{"/hls/master.m3u8?tl=" + tl + "&format=opus&gain=off"} },
	}
	for i, path := range paths {
		tl := mintTimeline(t, env, queues[i]).Tl
		urls := path(tl)
		if len(urls) == 1 {
			urls = append(urls, urls[0])
		}
		// HEAD where /stream would encode; the touch is the same.
		method := http.MethodGet
		if strings.HasPrefix(urls[0], "/stream") {
			method = http.MethodHead
		}
		start := time.Now()
		if !keepsTheStore(t, env, tl, method, urls) {
			t.Errorf("key-authed reads of %s rewrote the stored timeline", urls[0])
		}
		// Rounded up, never down: the reads still cover the documented 6 h default.
		if got, floor := storedExpiry(t, env, tl), start.Add(6*time.Hour); got.Before(floor) {
			t.Errorf("after reads of %s the timeline expires %v, short of a default-TTL URL's %v", urls[0], got, floor)
		}
	}
}

// masterV is the descriptor of a signed master for the timeline tl.
func masterV(t *testing.T, env *testEnv, tl string) string {
	t.Helper()
	return hlsDescriptorFor(t, env, map[string]string{"tl": tl, "format": "opus", "gain": "off"})
}

// keepsTheStore reads every url with the key and reports whether all but the
// first left tl's stored document alone. Reads that straddle an hour boundary
// may rewrite it legitimately (readExpiry rounds to the hour), so such a run
// repeats.
func keepsTheStore(t *testing.T, env *testEnv, tl, method string, urls []string) bool {
	t.Helper()
	modTime := func() time.Time {
		t.Helper()
		fi, err := os.Stat(filepath.Join(env.tlDir, tl+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	read := func(u string) {
		t.Helper()
		resp := env.req(t, method, u, nil)
		if body := readBody(t, resp); resp.StatusCode != 200 {
			t.Fatalf("%s %s = %d: %s", method, u, resp.StatusCode, body)
		}
	}
	for range 3 {
		hour := time.Now().Truncate(time.Hour)
		read(urls[0])
		before := modTime()
		for _, u := range urls[1:] {
			read(u)
		}
		after := modTime()
		if time.Now().Truncate(time.Hour).Equal(hour) {
			return after.Equal(before)
		}
	}
	t.Fatal("three runs in a row straddled an hour boundary")
	return false
}

// TestStreamTimelineSeeksToAMemberStart: a client seeks to a member's start by
// sending its boundary offset over the envelope rate, and lands on that
// member's first sample. 6980 / 44100 in float64, times 44100, is
// 6979.999999999999, so a conversion that truncates opens one sample early, on
// the previous member's last.
func TestStreamTimelineSeeksToAMemberStart(t *testing.T) {
	env, _ := timelineEnv(t)
	refs := []string{"lib/s0.wav", "lib/s1.wav"}
	for i, n := range []int{6980, 44100} {
		if err := os.WriteFile(memberFile(env, refs[i]), rampWAV(t, 44100, 2, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tl := mintTimeline(t, env, refs)
	start := tl.Boundaries[1].OffsetSamples
	if start != 6980 || tl.EnvelopeRate != 44100 {
		t.Fatalf("member 1 starts at %d of a %d Hz envelope, want 6980 of 44100", start, tl.EnvelopeRate)
	}
	seek := strconv.FormatFloat(float64(start)/float64(tl.EnvelopeRate), 'g', -1, 64)
	resp := env.get(t, tlStream(tl.Tl, "flac")+"&t="+seek, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("/stream?tl&t=%s = %d: %s", seek, resp.StatusCode, body)
	}
	wantFramesFrom(t, decodePCM(t, body), timelinePCM(t, env, refs), int(start))
}

// TestStreamTimelineRefusesItsQueryBeforeResolving: a refusal the query alone
// decides answers before any member is resolved, so it costs no member open or
// measure, and a member changed since the mint cannot hide it behind a 410.
func TestStreamTimelineRefusesItsQueryBeforeResolving(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	if err := os.WriteFile(memberFile(env, refs[1]), rampWAV(t, 48000, 2, 96000), 0o644); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, env.get(t, "/stream?tl="+tl.Tl+"&format=flac", nil),
		http.StatusBadRequest, waxerr.CodeInvalidRequest, "this daemon's default gain")
	wantRefusal(t, env.get(t, tlStream(tl.Tl, "flac")+"&bitrate=128", nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, "flac is lossless")
	wantRefusal(t, env.get(t, tlStream(tl.Tl, "aac")+"&hev2=1", nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, "hev2 applies to he-aac")
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sign(t.Context(), client.SignRequest{Path: "/stream",
		Params: map[string]string{"tl": tl.Tl, "format": "flac"}})
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("signing with the daemon's tag gain = %v (code %q), want the gain refusal", err, got)
	}
	// The changed member still answers 410 once the query is fine.
	wantEnvelope(t, env.get(t, tlStream(tl.Tl, "flac"), nil), http.StatusGone, waxerr.CodeSourceChanged)
}

// TestStreamTimelineSeeksIntoABlend pins the seek /stream newly reaches: into a
// crossfade zone whose outgoing member resamples to the envelope. Three 1 s
// members at 44.1, 48 and 44.1 kHz make a 48 kHz envelope of 3 x 48000 samples;
// two 0.1 s blends take 2 x 4800, leaving 134400. Zone 1 is [43200, 48000), so
// t=0.95 (45600) lands inside it and leaves 88800; t=0.5 lands inside the
// resampled first member and leaves 110400. FLAC refuses a header its run
// misses and MP3 writes its frame count up front, so each must also decode to
// exactly what it promised.
func TestStreamTimelineSeeksIntoABlend(t *testing.T) {
	env, _ := timelineEnv(t)
	refs := []string{"lib/x44a.wav", "lib/x48.wav", "lib/x44b.wav"}
	for i, rate := range []int{44100, 48000, 44100} {
		if err := os.WriteFile(memberFile(env, refs[i]), rampWAV(t, rate, 2, rate), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tl := mintTimeline(t, env, refs)
	for _, f := range []string{"flac", "mp3"} {
		for _, tc := range []struct {
			seek string
			want int64
		}{{"0.95", 88800}, {"0.5", 110400}} {
			t.Run(f+" t="+tc.seek, func(t *testing.T) {
				resp := env.get(t, tlStream(tl.Tl, f)+"&crossfadeSeconds=0.1&t="+tc.seek, nil)
				body := readBody(t, resp)
				if resp.StatusCode != 200 {
					t.Fatalf("/stream = %d: %s", resp.StatusCode, body)
				}
				if got, want := resp.Header.Get("X-Content-Duration"), fmt.Sprintf("%.3f", float64(tc.want)/48000); got != want {
					t.Errorf("X-Content-Duration = %q, want %q", got, want)
				}
				med, err := waxflow.New().OpenStream(container.BytesSource(body), "")
				if err != nil {
					t.Fatal(err)
				}
				defer med.Close()
				if n := decodedMediaFrames(t, med); n != tc.want {
					t.Errorf("the stream decodes to %d frames, want %d", n, tc.want)
				}
			})
		}
	}
}

// TestHLSTimelineRefusesItsQueryBeforeResolving: as on /stream, what the
// descriptor alone rules out answers before any member resolves, at the mint,
// at /sign, on a master and on a per-variant URL, so a member changed since
// the mint cannot hide it behind a 410. The descriptors are made by hand,
// because the mint refuses to make them.
func TestHLSTimelineRefusesItsQueryBeforeResolving(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	if err := os.WriteFile(memberFile(env, refs[1]), rampWAV(t, 48000, 2, 96000), 0o644); err != nil {
		t.Fatal(err)
	}
	const gainRefusal = "this daemon's default gain"
	wantRefusal(t, env.get(t, "/hls/master.m3u8?tl="+tl.Tl+"&format=opus", nil),
		http.StatusBadRequest, waxerr.CodeInvalidRequest, gainRefusal)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8",
		Params: map[string]string{"tl": tl.Tl, "format": "opus"}})
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("signing a master with the daemon's tag gain = %v (code %q), want the gain refusal", err, got)
	}
	desc := func(fields string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(`{"ver":1,"tl":"` + tl.Tl + `",` + fields + `}`))
	}
	tagGain := desc(`"format":"opus"`)
	for _, path := range []string{"/hls/master.m3u8?v=" + tagGain, "/hls/media.m3u8?v=" + tagGain} {
		wantRefusal(t, env.get(t, path, nil), http.StatusBadRequest, waxerr.CodeInvalidRequest, gainRefusal)
	}
	wantRefusal(t, env.get(t, "/hls/media.m3u8?v="+desc(`"format":"flac","bitrate":128,"gain":"off"`), nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, "flac is lossless")
	// The changed member still answers 410 once the descriptor is fine.
	wantEnvelope(t, env.get(t, "/hls/media.m3u8?v="+desc(`"format":"opus","gain":"off"`), nil),
		http.StatusGone, waxerr.CodeSourceChanged)
}
