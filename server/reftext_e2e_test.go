// A source reference is a byte string, and a file name need not be UTF-8.
// These tests pin that such a reference travels every surface: as raw bytes
// in a query string, and in its pct: text form wherever JSON carries it.
package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// refCase is one reference: its own bytes, and a pct: spelling of them.
type refCase struct {
	name string
	raw  string
	text string
}

// refCases returns an escaped spelling of a fixture every platform holds
// and, where the platform can hold one, a file whose name is not UTF-8.
func refCases(t *testing.T, env *testEnv) []refCase {
	t.Helper()
	cases := []refCase{{name: "escaped", raw: "lib/sine.wav", text: "pct:lib%2Fsine%2Ewav"}}
	if c, ok := strayCase(t, env); ok {
		cases = append(cases, c)
	}
	return cases
}

// strayCase writes a copy of sine.wav under a name that is not UTF-8 and
// returns its reference, or false where no file system holds such a name.
func strayCase(t *testing.T, env *testEnv) (refCase, bool) {
	t.Helper()
	name, escaped, ok := testutil.NonUTF8Name()
	if !ok {
		return refCase{}, false
	}
	if err := os.WriteFile(filepath.Join(env.root, name), readFile(t, filepath.Join(env.root, "sine.wav")), 0o644); err != nil {
		t.Fatal(err)
	}
	return refCase{name: "not UTF-8", raw: "lib/" + name, text: "pct:lib/" + escaped}, true
}

// jsonString is s as a JSON string literal.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantText is the text form the daemon writes for a case's reference: the
// reference itself when that is UTF-8, else the pct: spelling.
func (c refCase) wantText() string {
	if utf8.ValidString(c.raw) {
		return c.raw
	}
	return c.text
}

func TestPctRefOnQuerySurfaces(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			resp := env.get(t, "/probe?src="+url.QueryEscape(c.text), nil)
			if body := readBody(t, resp); resp.StatusCode != 200 {
				t.Fatalf("GET /probe = %d: %s", resp.StatusCode, body)
			}
			resp = env.get(t, "/stream?format=flac&src="+url.QueryEscape(c.raw), nil)
			want := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("GET /stream by the raw reference = %d: %s", resp.StatusCode, want)
			}
			waitForIdle(t, env)
			before := cacheStats(t, env)
			resp = env.get(t, "/stream?format=flac&src="+url.QueryEscape(c.text), nil)
			got := readBody(t, resp)
			if resp.StatusCode != 200 || !bytes.Equal(got, want) {
				t.Fatalf("GET /stream by the pct: spelling = %d, %d bytes; want the raw spelling's %d", resp.StatusCode, len(got), len(want))
			}
			// Two spellings of one file are one cache entry.
			waitForIdle(t, env)
			if after := cacheStats(t, env); after.Entries != before.Entries {
				t.Fatalf("the pct: spelling made a second cache entry (%d, then %d)", before.Entries, after.Entries)
			}
		})
	}
}

func TestPctRefInAProbeBody(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			resp := env.postJSON(t, "/probe", `{"src":`+jsonString(t, c.text)+`}`)
			if body := readBody(t, resp); resp.StatusCode != 200 {
				t.Fatalf("POST /probe = %d: %s", resp.StatusCode, body)
			}
		})
	}
}

// TestPctRefSigns: a signed URL is how a player reaches a file, and POST
// /sign takes its parameters as JSON.
func TestPctRefSigns(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			for _, path := range []string{"/stream", "/art"} {
				resp := env.postJSON(t, "/sign", `{"path":"`+path+`","params":{"src":`+jsonString(t, c.text)+`}}`)
				body := readBody(t, resp)
				if resp.StatusCode != 200 {
					t.Fatalf("POST /sign %s = %d: %s", path, resp.StatusCode, body)
				}
				var minted server.SignResponse
				if err := json.Unmarshal(body, &minted); err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(minted.URL)
				if err != nil {
					t.Fatal(err)
				}
				// A query string carries the bytes itself, so the URL needs no marker.
				if got := u.Query().Get("src"); got != c.raw {
					t.Errorf("%s URL names src %q, want the reference's own bytes %q", path, got, c.raw)
				}
				if path != "/stream" {
					continue
				}
				resp = keyless(t, env, minted.URL)
				if got := readBody(t, resp); resp.StatusCode != 200 {
					t.Fatalf("the signed URL = %d: %s", resp.StatusCode, got)
				}
			}
		})
	}
}

// TestPctRefInJobs: a job's request is echoed and stored as JSON, so the
// reference has to survive both.
func TestPctRefInJobs(t *testing.T) {
	jobsDir := filepath.Join(t.TempDir(), "jobs")
	env := newTestEnv(t, func(cfg *server.Config) { cfg.JobsDir = jobsDir })
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			for _, body := range []string{
				`{"type":"transcode","format":"flac","src":` + jsonString(t, c.text) + `}`,
				`{"type":"merge","format":"flac","srcs":[` + jsonString(t, c.text) + `,"lib/sine.wav"]}`,
			} {
				id := createJob(t, env, body)
				j := awaitJob(t, env, id)
				got := string(j.Request.Src)
				if j.Request.Type == jobs.TypeMerge {
					got = string(j.Request.Srcs[0])
				}
				if got != c.raw {
					t.Fatalf("the job names %q, want %q", got, c.raw)
				}
				for what, doc := range map[string][]byte{
					"GET /jobs/{id}": readBody(t, env.get(t, "/jobs/"+id, nil)),
					"job.json":       readFile(t, filepath.Join(jobsDir, id, "job.json")),
				} {
					if !utf8.Valid(doc) || !bytes.Contains(doc, []byte(jsonString(t, c.wantText()))) {
						t.Errorf("%s does not carry the reference as %s:\n%s", what, c.wantText(), doc)
					}
				}
			}
		})
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPctRefInATimeline: a timeline's members are stored as JSON and its
// digest is taken over that JSON.
func TestPctRefInATimeline(t *testing.T) {
	env, refs := timelineEnv(t)
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			mint := func(first string) string {
				resp := env.postJSON(t, "/hls/timeline", `{"srcs":[{"src":`+jsonString(t, first)+`},{"src":"`+refs[0]+`"}]}`)
				body := readBody(t, resp)
				if resp.StatusCode != http.StatusCreated {
					t.Fatalf("POST /hls/timeline = %d: %s", resp.StatusCode, body)
				}
				var v server.TimelineResponse
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatal(err)
				}
				return v.Tl
			}
			tl := mint(c.text)
			// One queue is one digest, however its references were spelled.
			if utf8.ValidString(c.raw) {
				if plain := mint(c.raw); plain != tl {
					t.Errorf("the pct: spelling minted %s, the plain one %s", tl, plain)
				}
			}
			doc := readFile(t, filepath.Join(env.tlDir, tl+".json"))
			if !utf8.Valid(doc) || !bytes.Contains(doc, []byte(jsonString(t, c.wantText()))) {
				t.Errorf("the stored timeline does not carry the reference as %s:\n%s", c.wantText(), doc)
			}
			resp := env.get(t, "/stream?format=flac&gain=off&tl="+tl, nil)
			if body := readBody(t, resp); resp.StatusCode != 200 {
				t.Fatalf("GET /stream of the timeline = %d: %s", resp.StatusCode, body)
			}
		})
	}
}

// TestHLSCarriesAnyReference: the descriptor is JSON inside the URL, and it
// used to refuse a name that is not UTF-8 for that reason.
func TestHLSCarriesAnyReference(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			resp := env.get(t, "/hls/master.m3u8?format=flac&src="+url.QueryEscape(c.raw), nil)
			master := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("master by the raw reference = %d: %s", resp.StatusCode, master)
			}
			// The descriptor is canonical, so both spellings name one variant.
			resp = env.get(t, "/hls/master.m3u8?format=flac&src="+url.QueryEscape(c.text), nil)
			if again := readBody(t, resp); resp.StatusCode != 200 || !sameVariants(t, master, again) {
				t.Fatalf("master by the pct: spelling = %d, naming other variants:\n%s\n%s", resp.StatusCode, master, again)
			}
			resp = keyless(t, env, "/hls/"+variantURIs(t, master)[0])
			media := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("media playlist = %d: %s", resp.StatusCode, media)
			}
			initURI, segURIs, _ := mediaURIs(t, media)
			for _, uri := range []string{initURI, segURIs[0]} {
				resp = keyless(t, env, "/hls/"+uri)
				if body := readBody(t, resp); resp.StatusCode != 200 {
					t.Fatalf("%s = %d: %s", uri, resp.StatusCode, body)
				}
			}
		})
	}
}

// sameVariants reports whether two masters name the same descriptors. The
// URIs differ in their signatures' expiry at most, so the v= values compare.
func sameVariants(t *testing.T, a, b []byte) bool {
	t.Helper()
	descs := func(master []byte) []string {
		var out []string
		for _, uri := range variantURIs(t, master) {
			u, err := url.Parse(uri)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, u.Query().Get("v"))
		}
		return out
	}
	x, y := descs(a), descs(b)
	return len(x) > 0 && strings.Join(x, " ") == strings.Join(y, " ")
}

// TestBodyThatIsNotUTF8IsRefused: a JSON decoder reads a stray byte as
// U+FFFD, so the request would name another file. Here that file exists,
// and it must not be the one answered for. Half a surrogate pair written
// \udXXX is read the same way, and it is ASCII, so the body is valid UTF-8:
// that is how Python's json.dumps writes such a name.
func TestBodyThatIsNotUTF8IsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "timelines")
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
		cfg.TimelineDir = dir
	})
	b := readFile(t, filepath.Join(env.root, "sine.wav"))
	if err := os.WriteFile(filepath.Join(env.root, "caf\ufffd.wav"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"\"lib/caf\xe9.wav\"", `"lib/caf\udce9.wav"`} {
		for path, body := range map[string]string{
			"/probe":        `{"src":` + src + `}`,
			"/sign":         `{"params":{"src":` + src + `}}`,
			"/sign art":     `{"path":"/art","params":{"src":` + src + `}}`,
			"/sign hls":     `{"path":"/hls/master.m3u8","params":{"src":` + src + `}}`,
			"/jobs":         `{"type":"analyze","src":` + src + `}`,
			"/jobs merge":   `{"type":"merge","format":"flac","srcs":["lib/sine.wav",` + src + `]}`,
			"/jobs cue":     `{"type":"split","format":"flac","src":"lib/sine.wav","cue":` + src + `}`,
			"/hls/timeline": `{"srcs":[{"src":` + src + `}]}`,
		} {
			path, _, _ = strings.Cut(path, " ")
			resp := env.postJSON(t, path, body)
			got := readBody(t, resp)
			if resp.StatusCode != 400 {
				t.Errorf("POST %s %s = %d, want 400: %s", path, body, resp.StatusCode, got)
				continue
			}
			var envl server.ErrorBody
			if err := json.Unmarshal(got, &envl); err != nil || envl.Code != waxerr.CodeInvalidRequest {
				t.Errorf("POST %s envelope = %s", path, got)
			}
			if !strings.Contains(envl.Error, "UTF-8") || !strings.Contains(envl.Error, "pct:") {
				t.Errorf("POST %s refusal does not state the rule and the form that satisfies it: %s", path, envl.Error)
			}
		}
	}
}

// TestStrayByteOutsideAReferenceIsNotRefused: only a reference names a file.
// A title holding a Latin-1 byte reads as it always did.
func TestStrayByteOutsideAReferenceIsNotRefused(t *testing.T) {
	env := jobsEnv(t)
	resp := env.postJSON(t, "/jobs", "{\"type\":\"merge\",\"format\":\"alac\",\"srcs\":[\"lib/sine.wav\"],\"titles\":[\"caf\xe9\"]}")
	if body := readBody(t, resp); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs = %d, want 201: %s", resp.StatusCode, body)
	}
	resp = env.postJSON(t, "/sign", "{\"params\":{\"src\":\"lib/sine.wav\",\"format\":\"wav\"},\"ttlSeconds\":60}")
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("POST /sign = %d: %s", resp.StatusCode, body)
	}
}

// pctSchemeResolver serves the reference "pct:abc" itself, as a build that
// adds a scheme of that name would, and records what it was asked for.
type pctSchemeResolver struct {
	next source.Resolver
	mu   sync.Mutex
	seen []string
}

func (r *pctSchemeResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	r.mu.Lock()
	r.seen = append(r.seen, ref)
	r.mu.Unlock()
	if ref != "pct:abc" {
		return r.next.Resolve(ctx, ref)
	}
	f, err := r.next.Resolve(ctx, "lib/sine.wav")
	if err == nil {
		f.Ref = ref
	}
	return f, err
}

// TestMintedURLKeepsAReferenceThatStartsWithTheMarker: every query reader
// decodes pct:, so a URL that carried such a reference bare would play
// another one.
func TestMintedURLKeepsAReferenceThatStartsWithTheMarker(t *testing.T) {
	var res *pctSchemeResolver
	env := newTestEnv(t, func(cfg *server.Config) {
		res = &pctSchemeResolver{next: cfg.Resolver}
		cfg.Resolver = res
	})
	for _, path := range []string{"/stream", "/art"} {
		resp := env.postJSON(t, "/sign", `{"path":"`+path+`","params":{"src":"pct:pct%3Aabc"}}`)
		body := readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("POST /sign %s = %d: %s", path, resp.StatusCode, body)
		}
		var minted server.SignResponse
		if err := json.Unmarshal(body, &minted); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(minted.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := source.ParseRef(u.Query().Get("src")); err != nil || got != "pct:abc" {
			t.Errorf("%s URL's src reads back as %q (%v), want pct:abc", path, got, err)
		}
		if path != "/stream" {
			continue
		}
		res.mu.Lock()
		res.seen = nil
		res.mu.Unlock()
		if resp := keyless(t, env, minted.URL); resp.StatusCode != 200 {
			t.Fatalf("the signed URL = %d: %s", resp.StatusCode, readBody(t, resp))
		} else {
			resp.Body.Close()
		}
		res.mu.Lock()
		if len(res.seen) == 0 || res.seen[0] != "pct:abc" {
			t.Errorf("the signed URL resolved %q, want pct:abc", res.seen)
		}
		res.mu.Unlock()
	}
}

// TestHLSMintRefusesSrcBesideTl: /stream refuses the pair by presence, and
// a master playlist must not drop an empty src and mint the timeline.
func TestHLSMintRefusesSrcBesideTl(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs).Tl
	for _, src := range []string{"", "pct:"} {
		resp := env.get(t, "/hls/master.m3u8?format=flac&gain=off&tl="+tl+"&src="+url.QueryEscape(src), nil)
		wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
		resp = env.get(t, "/stream?format=flac&gain=off&tl="+tl+"&src="+url.QueryEscape(src), nil)
		wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
	}
}

// TestErrorTextNamesAnyFile: an error that quotes a name goes out as JSON,
// where a stray byte would become U+FFFD and the message name another file.
func TestErrorTextNamesAnyFile(t *testing.T) {
	env := newTestEnv(t, nil)
	resp := env.get(t, "/probe?src="+url.QueryEscape("lib/gone\xe9.wav"), nil)
	body := readBody(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("GET /probe = %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`gone\\xe9.wav`)) || bytes.Contains(body, []byte(`\ufffd`)) || bytes.Contains(body, []byte("\ufffd")) {
		t.Errorf("the envelope does not show the name's bytes: %s", body)
	}
}

func TestMalformedPctRefIs400(t *testing.T) {
	env := jobsEnv(t)
	resp := env.get(t, "/probe?src="+url.QueryEscape("pct:lib/100%"), nil)
	wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
	resp = env.get(t, "/stream?src="+url.QueryEscape("pct:lib/%zz.wav"), nil)
	wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
	resp = env.get(t, "/hls/master.m3u8?src="+url.QueryEscape("pct:lib/%4"), nil)
	wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
	resp = env.postJSON(t, "/probe", `{"src":"pct:lib/100%"}`)
	wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
	// A marker with nothing behind it is a mistake, not an absent src.
	resp = env.postJSON(t, "/jobs", `{"type":"merge","format":"flac","src":"pct:","srcs":["lib/sine.wav"]}`)
	wantEnvelope(t, resp, 400, waxerr.CodeInvalidRequest)
}

// TestCacheSidecarNamesAnyReference: the sidecar's ref is there for a
// person reading the cache directory, and U+FFFD would not say which file.
func TestCacheSidecarNamesAnyReference(t *testing.T) {
	env := newTestEnv(t, nil)
	c, ok := strayCase(t, env)
	if !ok {
		t.Skip("this platform's file names are all valid UTF-8")
	}
	resp := env.get(t, "/stream?format=flac&src="+url.QueryEscape(c.raw), nil)
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("GET /stream = %d: %s", resp.StatusCode, body)
	}
	// An HLS variant writes a sidecar of its own.
	resp = env.get(t, "/hls/master.m3u8?format=flac&src="+url.QueryEscape(c.raw), nil)
	master := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("master = %d: %s", resp.StatusCode, master)
	}
	media := readBody(t, keyless(t, env, "/hls/"+variantURIs(t, master)[0]))
	_, segURIs, _ := mediaURIs(t, media)
	if resp = keyless(t, env, "/hls/"+segURIs[0]); resp.StatusCode != 200 {
		t.Fatalf("segment = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	waitForIdle(t, env)
	var metas [][]byte
	err := filepath.WalkDir(env.cache, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "meta.json" {
			metas = append(metas, readFile(t, path))
		}
		return err
	})
	if err != nil || len(metas) < 2 {
		t.Fatalf("%d cache sidecars written, want the stream's and the variant's (%v)", len(metas), err)
	}
	for _, m := range metas {
		if !bytes.Contains(m, []byte(`"ref": "`+c.text+`"`)) || bytes.Contains(m, []byte(`\ufffd`)) {
			t.Errorf("sidecar does not name the reference as %s throughout:\n%s", c.text, m)
		}
	}
}

// syncBuffer is a log sink the pipelines' goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestLogsNameAnyReference: a JSON log line writes U+FFFD for a stray byte,
// and would no longer say which file a pipeline was for.
func TestLogsNameAnyReference(t *testing.T) {
	var logs syncBuffer
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	c, ok := strayCase(t, env)
	if !ok {
		t.Skip("this platform's file names are all valid UTF-8")
	}
	resp := env.get(t, "/stream?format=flac&src="+url.QueryEscape(c.raw), nil)
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("GET /stream = %d: %s", resp.StatusCode, body)
	}
	resp = env.get(t, "/hls/master.m3u8?format=flac&src="+url.QueryEscape(c.raw), nil)
	master := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("master = %d: %s", resp.StatusCode, master)
	}
	media := readBody(t, keyless(t, env, "/hls/"+variantURIs(t, master)[0]))
	_, segURIs, _ := mediaURIs(t, media)
	readBody(t, keyless(t, env, "/hls/"+segURIs[0]))
	waitForIdle(t, env)
	waitFor(t, "the pipelines' closing log lines", func() bool {
		return strings.Count(logs.String(), `"src":"`+c.text+`"`) >= 2
	})
	if got := logs.String(); strings.Contains(got, `\ufffd`) || strings.Contains(got, "\ufffd") {
		t.Errorf("a log line lost the reference's bytes:\n%s", got)
	}
}

// TestClientCarriesAnyReference: the Go client takes a reference's own
// bytes and does the escaping, so a caller never spells pct: itself.
func TestClientCarriesAnyReference(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "timelines")
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
		cfg.TimelineDir = dir
	})
	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range refCases(t, env) {
		t.Run(c.name, func(t *testing.T) {
			if _, err := cl.Probe(t.Context(), c.raw); err != nil {
				t.Fatalf("Probe: %v", err)
			}
			minted, err := cl.Sign(t.Context(), client.SignRequest{Params: map[string]string{"src": c.raw, "format": "flac"}})
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if resp := keyless(t, env, minted.URL); resp.StatusCode != 200 {
				t.Fatalf("the signed URL = %d: %s", resp.StatusCode, readBody(t, resp))
			} else {
				resp.Body.Close()
			}
			j, err := cl.CreateJob(t.Context(), client.JobRequest{Type: "analyze", Src: c.raw})
			if err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if j.Request.Src != c.raw {
				t.Errorf("the job echoes %q, want %q", j.Request.Src, c.raw)
			}
			awaitJob(t, env, j.ID)
			tl, jobID, err := cl.CreateTimeline(t.Context(), client.TimelineRequest{Srcs: []client.TimelineSrc{{Src: c.raw}}})
			if err != nil || tl == nil {
				t.Fatalf("CreateTimeline = %v, job %q, %v", tl, jobID, err)
			}
		})
	}
}
