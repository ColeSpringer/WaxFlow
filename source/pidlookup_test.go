package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

const testPID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// pidHost is a fake lookup host: it answers every GET through answer and
// records how many requests it saw and what the last one carried.
type pidHost struct {
	*httptest.Server
	requests atomic.Int64
	conns    atomic.Int64 // connections opened to the host
	answer   atomic.Pointer[func(id string, w http.ResponseWriter, r *http.Request)]

	mu       sync.Mutex
	lastPath string // as requested, escaping kept
	lastHdr  http.Header
}

func newPIDHost(t *testing.T, answer func(id string, w http.ResponseWriter, r *http.Request)) *pidHost {
	t.Helper()
	h := &pidHost{}
	h.setAnswer(answer)
	h.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.mu.Lock()
		h.lastPath = r.URL.EscapedPath()
		h.lastHdr = r.Header.Clone()
		h.mu.Unlock()
		(*h.answer.Load())(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], w, r)
	}))
	h.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			h.conns.Add(1)
		}
	}
	h.Start()
	t.Cleanup(h.Close)
	return h
}

func (h *pidHost) setAnswer(fn func(id string, w http.ResponseWriter, r *http.Request)) {
	h.answer.Store(&fn)
}

func (h *pidHost) last() (string, http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastPath, h.lastHdr
}

// answerRef answers every id with a JSON ref, plus any header pairs given.
func answerRef(ref string, hdr ...string) func(string, http.ResponseWriter, *http.Request) {
	return func(_ string, w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ref":%q}`, ref)
	}
}

// answerStatus answers every id with a bare status.
func answerStatus(code int, hdr ...string) func(string, http.ResponseWriter, *http.Request) {
	return func(_ string, w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.WriteHeader(code)
	}
}

// answerBody answers every id with a 200 and the given body.
func answerBody(body string) func(string, http.ResponseWriter, *http.Request) {
	return func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

// newLookup builds a lookup at host.URL/pid over the standard test roots
// (root lib, file sub/a.wav holding RIFFdata) and returns the roots' dir.
func newLookup(t *testing.T, host *pidHost, opts PIDLookupOptions) (*PIDLookup, string) {
	t.Helper()
	roots, dir := newRoots(t, 0)
	l, err := NewPIDLookup(host.URL+"/pid", roots, opts)
	if err != nil {
		t.Fatal(err)
	}
	return l, dir
}

func wantCode(t *testing.T, err error, want waxerr.Code) {
	t.Helper()
	if got := waxerr.CodeOf(err); got != want {
		t.Fatalf("code = %s, want %s (%v)", got, want, err)
	}
}

func TestPIDLookupResolvesThroughTheRoots(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/sub/a.wav"))
	l, dir := newLookup(t, host, PIDLookupOptions{})

	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Ref != "pid:"+testPID {
		t.Errorf("Ref = %q, want the pid reference", f.Ref)
	}
	if f.Ext != "wav" {
		t.Errorf("Ext = %q, want wav", f.Ext)
	}
	fi, err := os.Stat(filepath.Join(dir, "sub", "a.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != (Identity{Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()}) {
		t.Errorf("ID = %+v, want the file's stat", f.ID)
	}
	if got := readAll(t, f); !strings.HasPrefix(got, "RIFF") {
		t.Errorf("ReadAt = %q, want the file's bytes", got)
	}
	path, hdr := host.last()
	if path != "/pid/"+testPID {
		t.Errorf("request path = %q, want /pid/%s", path, testPID)
	}
	if hdr.Get("Accept") != "application/json" || hdr.Get("User-Agent") != "waxflow" {
		t.Errorf("request headers = %v, want Accept application/json and User-Agent waxflow", hdr)
	}
	if hdr.Get("Authorization") != "" {
		t.Errorf("Authorization = %q sent with no key configured", hdr.Get("Authorization"))
	}

	keyed, _ := newLookup(t, host, PIDLookupOptions{Key: "s3cret"})
	f2, err := keyed.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if _, hdr := host.last(); hdr.Get("Authorization") != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", hdr.Get("Authorization"))
	}

	host.setAnswer(answerBody(`{"ref":"lib/sub/a.wav","title":"x"}`))
	f3, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatalf("an answer with extra fields = %v, want success", err)
	}
	f3.Close()
}

func TestPIDLookupURLShapes(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/sub/a.wav"))
	roots, _ := newRoots(t, 0)
	for _, tc := range []struct{ base, want string }{
		{host.URL + "/pid", "/pid/" + testPID},
		{host.URL + "/pid/", "/pid/" + testPID},
		{host.URL, "/" + testPID},
		{host.URL + "/a/b", "/a/b/" + testPID},
		{host.URL + "/a%2Fb/pid", "/a%2Fb/pid/" + testPID},
	} {
		l, err := NewPIDLookup(tc.base, roots, PIDLookupOptions{})
		if err != nil {
			t.Fatalf("NewPIDLookup(%q) = %v", tc.base, err)
		}
		f, err := l.Resolve(context.Background(), "pid:"+testPID)
		if err != nil {
			t.Fatalf("base %q: %v", tc.base, err)
		}
		f.Close()
		if path, _ := host.last(); path != tc.want {
			t.Errorf("base %q requested %q, want %q", tc.base, path, tc.want)
		}
	}
}

func TestParsePIDLookupURL(t *testing.T) {
	for _, raw := range []string{"http://h", "https://h/", "http://h/a/b/", "http://u:p@h/x", "http://127.0.0.1:4420/engine/pid"} {
		if _, err := ParsePIDLookupURL(raw); err != nil {
			t.Errorf("ParsePIDLookupURL(%q) = %v, want accepted", raw, err)
		}
	}
	for _, raw := range []string{"", "h/x", "ftp://h", "http://", "http:///x", "http://h/?x=1", "http://h/#f", "http://h/x?",
		"http://u:s3cret@h/?x=1", "http://u:s3cret@h/%zz"} {
		_, err := ParsePIDLookupURL(raw)
		if err == nil {
			t.Errorf("ParsePIDLookupURL(%q) accepted", raw)
			continue
		}
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("ParsePIDLookupURL(%q) code = %s, want invalid-request", raw, waxerr.CodeOf(err))
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("ParsePIDLookupURL(%q) = %q leaks the password", raw, err)
		}
	}
}

// TestNewPIDLookupRefusals: construction refuses what Resolve could only
// discover by failing: a nil resolver to delegate to, a key no header can
// carry.
func TestNewPIDLookupRefusals(t *testing.T) {
	roots, _ := newRoots(t, 0)
	if _, err := NewPIDLookup("http://h/pid", nil, PIDLookupOptions{}); err == nil {
		t.Error("a nil next was accepted")
	}
	for _, key := range []string{"bad key", "k\n", "k\x7f", "ключ"} {
		if _, err := NewPIDLookup("http://h/pid", roots, PIDLookupOptions{Key: key}); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("key %q: err = %v, want invalid-request", key, err)
		}
	}
	for _, key := range []string{"abc", "Abc-._~+/=", "0123456789abcdef0123456789abcdef"} {
		if _, err := NewPIDLookup("http://h/pid", roots, PIDLookupOptions{Key: key}); err != nil {
			t.Errorf("key %q refused: %v", key, err)
		}
	}
}

// roundTripperFunc is a RoundTripper that is not an *http.Transport.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestNewPIDLookupWithoutAStandardDefaultTransport: a build that wraps
// http.DefaultTransport (tracing, say) must still construct and resolve.
func TestNewPIDLookupWithoutAStandardDefaultTransport(t *testing.T) {
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(prev.RoundTrip)
	defer func() { http.DefaultTransport = prev }()
	host := newPIDHost(t, answerRef("lib/sub/a.wav"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// TestPIDLookupCloseKeepsResolving: Close releases idle connections and
// nothing else, so it is safe twice and a resolve after it still works.
func TestPIDLookupCloseKeepsResolving(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/sub/a.wav"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	resolveN(t, l, host, 1)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	resolveN(t, l, host, 1)
}

func TestPIDLookupRefusesBadIDs(t *testing.T) {
	host := newPIDHost(t, answerStatus(http.StatusNotFound))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	for _, ref := range []string{"pid:", "pid:..", "pid:a/b", "pid:a b", "pid:%41", "pid:é", "pid:" + strings.Repeat("a", 129)} {
		_, err := l.Resolve(context.Background(), ref)
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("Resolve(%q) = %v, want invalid-request", ref, err)
		}
	}
	if n := host.requests.Load(); n != 0 {
		t.Fatalf("host saw %d requests for malformed ids, want 0", n)
	}
	_, err := l.Resolve(context.Background(), "pid:"+strings.Repeat("a", 128))
	wantCode(t, err, waxerr.CodeNotFound)
	if n := host.requests.Load(); n != 1 {
		t.Fatalf("host saw %d requests for a 128-byte id, want 1", n)
	}
}

func TestPIDLookupStatusCodes(t *testing.T) {
	host := newPIDHost(t, nil)
	l, _ := newLookup(t, host, PIDLookupOptions{})
	for _, tc := range []struct {
		status int
		want   waxerr.Code
	}{
		{http.StatusNotFound, waxerr.CodeNotFound},
		{http.StatusGone, waxerr.CodeNotFound},
		{http.StatusBadRequest, waxerr.CodeInvalidRequest},
		{http.StatusUnprocessableEntity, waxerr.CodeInvalidRequest},
		{http.StatusUnauthorized, waxerr.CodeCatalogUnavailable},
		{http.StatusForbidden, waxerr.CodeCatalogUnavailable},
		{http.StatusRequestTimeout, waxerr.CodeCatalogUnavailable},
		{http.StatusTooManyRequests, waxerr.CodeCatalogUnavailable},
		{http.StatusInternalServerError, waxerr.CodeCatalogUnavailable},
		{http.StatusServiceUnavailable, waxerr.CodeCatalogUnavailable},
		{http.StatusFound, waxerr.CodeInternal},
		{http.StatusMethodNotAllowed, waxerr.CodeInternal},
		{http.StatusNotAcceptable, waxerr.CodeInternal},
	} {
		host.setAnswer(answerStatus(tc.status, "Location", host.URL+"/pid/other"))
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		if waxerr.CodeOf(err) != tc.want {
			t.Errorf("status %d: code = %s, want %s (%v)", tc.status, waxerr.CodeOf(err), tc.want, err)
			continue
		}
		if (tc.want == waxerr.CodeCatalogUnavailable || tc.want == waxerr.CodeInternal) && !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
			t.Errorf("status %d: error %q does not name the status", tc.status, err)
		}
		if strings.Contains(err.Error(), host.URL) {
			t.Errorf("status %d: error %q echoes the host URL", tc.status, err)
		}
	}
	host.Close()
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
	if strings.Contains(err.Error(), host.URL) {
		t.Errorf("closed host: error %q echoes the host URL", err)
	}

	// A name that does not resolve: the common compose outage, whose raw error
	// names the host and the resolver.
	roots, _ := newRoots(t, 0)
	unresolvable, err := NewPIDLookup("http://pid-host.invalid/pid", roots, PIDLookupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = unresolvable.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
	if msg := err.Error(); strings.Contains(msg, "pid-host.invalid") || strings.Contains(msg, ":53") {
		t.Errorf("error %q names the host or the resolver", msg)
	}
}

// TestPIDLookupDoesNotRememberABrokenAnswer: an answer the roots refuse is
// the host's mistake, and a max-age on it must not pin that mistake.
func TestPIDLookupDoesNotRememberABrokenAnswer(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/sub", "Cache-Control", "max-age=60"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeInternal)
	host.setAnswer(answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatalf("resolve after the host was fixed = %v, want success", err)
	}
	f.Close()
	if n := host.requests.Load(); n != 2 {
		t.Fatalf("requests = %d, want 2 (the broken answer was not remembered)", n)
	}
	// A good answer is remembered as before.
	if n := resolveN(t, l, host, 1); n != 2 {
		t.Fatalf("requests = %d, want 2 (the good answer was remembered)", n)
	}
}

// TestPIDLookupKeepsTheHostsPathsToItself: a pid exists to be location-free,
// so what the host answered never reaches the client's error text.
func TestPIDLookupKeepsTheHostsPathsToItself(t *testing.T) {
	host := newPIDHost(t, nil)
	l, _ := newLookup(t, host, PIDLookupOptions{})
	long := strings.Repeat("Artist/Album/", 400) + "01.flac"
	for _, tc := range []struct {
		body   string
		code   waxerr.Code
		secret string
	}{
		{`{"ref":"lib/sub/missing.wav"}`, waxerr.CodeNotFound, "missing.wav"},
		{`{"ref":"nope/a.wav"}`, waxerr.CodeNotFound, "nope"},
		{`{"ref":"lib/../sub/a.wav"}`, waxerr.CodeInternal, ".."},
		{`{"ref":"lib/sub"}`, waxerr.CodeInternal, "sub"},
		{`{"ref":"` + long + `"}`, waxerr.CodeNotFound, "Artist"}, // an unknown root, however long
		{`{"ref":"/lib/sub/a.wav"}`, waxerr.CodeInternal, "a.wav"},
	} {
		host.setAnswer(answerBody(tc.body))
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		wantCode(t, err, tc.code)
		if msg := err.Error(); strings.Contains(msg, tc.secret) || len(msg) > 256 {
			t.Errorf("body %.40q: error text %q carries the host's answer", tc.body, msg)
		}
	}
}

// TestPIDLookupLogsForTheOperator: what the client messages leave out goes to
// the logger, once per change of the host's state, and per broken answer.
func TestPIDLookupLogsForTheOperator(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	host := newPIDHost(t, answerStatus(http.StatusServiceUnavailable))
	l, _ := newLookup(t, host, PIDLookupOptions{Logger: logger})
	for range 2 {
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		wantCode(t, err, waxerr.CodeCatalogUnavailable)
	}
	if got := strings.Count(buf.String(), "unavailable"); got != 1 {
		t.Fatalf("two failures logged %d unavailable lines, want 1:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "503") {
		t.Fatalf("the log does not carry the status:\n%s", buf.String())
	}
	host.setAnswer(answerRef("lib/sub/a.wav"))
	resolveN(t, l, host, 2)
	if got := strings.Count(buf.String(), "answering again"); got != 1 {
		t.Fatalf("recovery logged %d times, want 1:\n%s", got, buf.String())
	}
	host.setAnswer(answerBody(`{"ref":"lib/../sub/a.wav"}`))
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeInternal)
	if !strings.Contains(buf.String(), "lib/../sub/a.wav") {
		t.Fatalf("the log does not carry the host's answer:\n%s", buf.String())
	}
	host.setAnswer(answerBody(`{"ref":"` + strings.Repeat("a", 4096) + `"}`))
	before := buf.Len()
	_, err = l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeInternal)
	if buf.Len()-before > 1024 {
		t.Fatalf("a huge answer was logged whole (%d bytes)", buf.Len()-before)
	}
}

func TestPIDLookupMalformedAnswers(t *testing.T) {
	host := newPIDHost(t, nil)
	l, _ := newLookup(t, host, PIDLookupOptions{})
	for _, body := range []string{
		`not json`, `{}`, `{"ref":""}`, `{"ref":"pid:X"}`, `{"ref":"upload:x"}`,
		`{"ref":"/lib/sub/a.wav"}`, `{"ref":"lib"}`, `{"ref":"lib/"}`, `{"ref":"lib/../sub/a.wav"}`,
		`{"ref":"` + strings.Repeat("a", 65<<10) + `"}`,
	} {
		host.setAnswer(answerBody(body))
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		if waxerr.CodeOf(err) != waxerr.CodeInternal {
			t.Errorf("body %.40q: code = %s, want internal (%v)", body, waxerr.CodeOf(err), err)
		}
	}
	for _, ref := range []string{"nope/a.wav", "lib/missing.wav"} {
		host.setAnswer(answerRef(ref))
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		if waxerr.CodeOf(err) != waxerr.CodeNotFound {
			t.Errorf("ref %q: code = %s, want not-found (%v)", ref, waxerr.CodeOf(err), err)
		}
	}
}

func TestPIDLookupTimeoutAndCancel(t *testing.T) {
	// The host holds every request until the client gives up on it.
	hold := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	host := newPIDHost(t, nil)
	host.setAnswer(func(_ string, w http.ResponseWriter, r *http.Request) { hold(w, r) })
	l, _ := newLookup(t, host, PIDLookupOptions{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
	if time.Since(start) > time.Second {
		t.Fatalf("a held request took %v, want the 50 ms bound", time.Since(start))
	}

	// Headers arrive and the body never does: the bound covers the body too.
	host.setAnswer(func(_ string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		hold(w, r)
	})
	start = time.Now()
	_, err = l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
	if time.Since(start) > time.Second {
		t.Fatalf("a held body took %v, want the 50 ms bound", time.Since(start))
	}

	// A caller that leaves is told so, not that the host is down.
	slow, _ := newLookup(t, host, PIDLookupOptions{})
	host.setAnswer(func(_ string, w http.ResponseWriter, r *http.Request) { hold(w, r) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = slow.Resolve(ctx, "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCanceled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("canceled lookup = %v, want the context's error inside", err)
	}
}

func TestPIDLookupDelegatesOtherReferences(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/sub/a.wav"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	f, err := l.Resolve(context.Background(), "lib/sub/a.wav")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if f.Ref != "lib/sub/a.wav" {
		t.Errorf("Ref = %q, want the root reference untouched", f.Ref)
	}
	for _, ref := range []string{"upload:x", "pidx:1"} {
		_, err := l.Resolve(context.Background(), ref)
		if waxerr.CodeOf(err) != waxerr.CodeUnsupportedSource {
			t.Errorf("Resolve(%q) = %v, want unsupported-source", ref, err)
		}
	}
	if n := host.requests.Load(); n != 0 {
		t.Fatalf("host saw %d requests for non-pid references, want 0", n)
	}
}

// resolveN resolves the pid n times, closing each file, and returns the
// host's request count.
func resolveN(t *testing.T, l *PIDLookup, host *pidHost, n int) int64 {
	t.Helper()
	for range n {
		f, err := l.Resolve(context.Background(), "pid:"+testPID)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	return host.requests.Load()
}

func TestPIDLookupHonorsMaxAge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hdr   []string
		wantN int64
	}{
		{"max-age remembers", []string{"Cache-Control", "max-age=60"}, 1},
		{"no header asks every time", nil, 2},
		{"max-age=0", []string{"Cache-Control", "max-age=0"}, 2},
		{"no-store wins", []string{"Cache-Control", "no-store, max-age=60"}, 2},
		{"no-cache wins", []string{"Cache-Control", "no-cache, max-age=60"}, 2},
		{"unparsable max-age", []string{"Cache-Control", "max-age=abc"}, 2},
		{"public, max-age", []string{"Cache-Control", "public, max-age=60"}, 1},
		{"max-age past int64 seconds is the cap", []string{"Cache-Control", "max-age=10000000000"}, 1},
		{"max-age past int64 is the cap", []string{"Cache-Control", "max-age=99999999999999999999"}, 1},
		{"negative max-age", []string{"Cache-Control", "max-age=-5"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newPIDHost(t, answerRef("lib/sub/a.wav", tc.hdr...))
			l, _ := newLookup(t, host, PIDLookupOptions{})
			if n := resolveN(t, l, host, 2); n != tc.wantN {
				t.Fatalf("two resolves made %d requests, want %d", n, tc.wantN)
			}
		})
	}

	t.Run("a 404 is never remembered", func(t *testing.T) {
		host := newPIDHost(t, answerStatus(http.StatusNotFound, "Cache-Control", "max-age=60"))
		l, _ := newLookup(t, host, PIDLookupOptions{})
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		wantCode(t, err, waxerr.CodeNotFound)
		host.setAnswer(answerRef("lib/sub/a.wav"))
		if n := resolveN(t, l, host, 1); n != 2 {
			t.Fatalf("requests = %d, want 2 (the 404 was not cached)", n)
		}
	})

	t.Run("an entry expires", func(t *testing.T) {
		host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
		l, _ := newLookup(t, host, PIDLookupOptions{})
		base := time.Now()
		var offset time.Duration
		l.now = func() time.Time { return base.Add(offset) }
		resolveN(t, l, host, 1)
		offset = 30 * time.Second
		if n := resolveN(t, l, host, 1); n != 1 {
			t.Fatalf("requests at +30s = %d, want 1", n)
		}
		offset = 61 * time.Second
		if n := resolveN(t, l, host, 1); n != 2 {
			t.Fatalf("requests at +61s = %d, want 2", n)
		}
	})

	t.Run("the TTL is capped at an hour", func(t *testing.T) {
		host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=7200"))
		l, _ := newLookup(t, host, PIDLookupOptions{})
		base := time.Now()
		var offset time.Duration
		l.now = func() time.Time { return base.Add(offset) }
		resolveN(t, l, host, 1)
		offset = 3599 * time.Second
		if n := resolveN(t, l, host, 1); n != 1 {
			t.Fatalf("requests at +3599s = %d, want 1", n)
		}
		offset = 3601 * time.Second
		if n := resolveN(t, l, host, 1); n != 2 {
			t.Fatalf("requests at +3601s = %d, want 2", n)
		}
	})
}

func TestPIDLookupCacheSelfHeals(t *testing.T) {
	t.Run("the host tracks the rename", func(t *testing.T) {
		host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
		l, dir := newLookup(t, host, PIDLookupOptions{})
		resolveN(t, l, host, 1)
		if err := os.Rename(filepath.Join(dir, "sub", "a.wav"), filepath.Join(dir, "sub", "b.wav")); err != nil {
			t.Fatal(err)
		}
		host.setAnswer(answerRef("lib/sub/b.wav", "Cache-Control", "max-age=60"))
		f, err := l.Resolve(context.Background(), "pid:"+testPID)
		if err != nil {
			t.Fatalf("resolve after the rename = %v, want the re-asked path", err)
		}
		f.Close()
		if f.Ref != "pid:"+testPID {
			t.Errorf("Ref = %q, want the pid reference", f.Ref)
		}
		if n := host.requests.Load(); n != 2 {
			t.Fatalf("requests = %d, want 2 (one re-ask)", n)
		}
	})
	t.Run("the host still names the old path", func(t *testing.T) {
		host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
		l, dir := newLookup(t, host, PIDLookupOptions{})
		resolveN(t, l, host, 1)
		if err := os.Rename(filepath.Join(dir, "sub", "a.wav"), filepath.Join(dir, "sub", "b.wav")); err != nil {
			t.Fatal(err)
		}
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		wantCode(t, err, waxerr.CodeNotFound)
		if n := host.requests.Load(); n != 2 {
			t.Fatalf("requests = %d, want 2 (one re-ask, then a real not-found)", n)
		}
	})
}

func TestPIDLookupCacheBound(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	id := func(i int) string { return fmt.Sprintf("pid:%026d", i) }
	for i := range pidCacheEntries + 1 {
		f, err := l.Resolve(context.Background(), id(i))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	before := host.requests.Load()
	// The second entry is still remembered; the first, the oldest, was
	// evicted when the cache filled.
	f, err := l.Resolve(context.Background(), id(1))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n := host.requests.Load(); n != before {
		t.Fatalf("resolving the second id again made a request (%d -> %d)", before, n)
	}
	f, err = l.Resolve(context.Background(), id(0))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n := host.requests.Load(); n != before+1 {
		t.Fatalf("resolving the evicted first id made %d requests, want 1", n-before)
	}
}

func TestPIDLookupConcurrentResolves(t *testing.T) {
	for _, tc := range []struct {
		name string
		hdr  []string
	}{
		{"cold every time", nil},
		{"remembered", []string{"Cache-Control", "max-age=60"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newPIDHost(t, answerRef("lib/sub/a.wav", tc.hdr...))
			l, _ := newLookup(t, host, PIDLookupOptions{})
			var wg sync.WaitGroup
			errs := make(chan error, 10)
			for i := range 10 {
				wg.Go(func() {
					// Odd goroutines take their own pid, so remembering and
					// evicting run beside the shared pid's lookups.
					ref := "pid:" + testPID
					if i%2 == 1 {
						ref = fmt.Sprintf("pid:%026d", i)
					}
					f, err := l.Resolve(context.Background(), ref)
					if err != nil {
						errs <- err
						return
					}
					defer f.Close()
					head := make([]byte, 4)
					if _, err := f.ReadAt(head, 0); err != nil || string(head) != "RIFF" {
						errs <- fmt.Errorf("read %q, %v", head, err)
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// TestPIDLookupHidesEveryPathFromClients: whatever the roots say about the
// path behind a pid, the client hears a fixed sentence per code. The one
// exception is a root that is down, whose error names the root alone.
func TestPIDLookupHidesEveryPathFromClients(t *testing.T) {
	host := newPIDHost(t, answerRef("lib/secret/one.flac"))
	for _, tc := range []struct {
		name string
		err  error
		want waxerr.Code
	}{
		{"out of handles", waxerr.Wrap(waxerr.CodeOverloaded, "source: out of file handles",
			&fs.PathError{Op: "openat", Path: "secret/one.flac", Err: errors.New("too many open files")}), waxerr.CodeOverloaded},
		{"unreadable", waxerr.Wrap(waxerr.CodeSourceUnreadable, "source: permission denied",
			&fs.PathError{Op: "openat", Path: "secret/one.flac", Err: fs.ErrPermission}), waxerr.CodeSourceUnreadable},
		{"gone", waxerr.Wrap(waxerr.CodeNotFound, "source: no such file",
			&fs.PathError{Op: "openat", Path: "secret/one.flac", Err: fs.ErrNotExist}), waxerr.CodeNotFound},
		{"unknown code", waxerr.Wrap(waxerr.CodeMalformedInput, "source: odd",
			&fs.PathError{Op: "openat", Path: "secret/one.flac", Err: errors.New("odd")}), waxerr.CodeMalformedInput},
		{"escapes", waxerr.Wrap(waxerr.CodeInvalidRequest, "source: unresolvable path",
			&fs.PathError{Op: "openat", Path: "secret/one.flac", Err: errors.New("path escapes from parent")}), waxerr.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewPIDLookup(host.URL+"/pid", stubResolver{err: tc.err}, PIDLookupOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = l.Resolve(context.Background(), "pid:"+testPID)
			wantCode(t, err, tc.want)
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q carries the path", err)
			}
		})
	}
	// A root that is down keeps its typed cause for the CLI's hint.
	down := unavailable("lib", &fs.PathError{Op: "open", Path: "/mnt/secret", Err: fs.ErrNotExist})
	l, err := NewPIDLookup(host.URL+"/pid", stubResolver{err: down}, PIDLookupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeRootUnavailable)
	if _, ok := errors.AsType[*RootDownError](err); !ok || strings.Contains(err.Error(), "secret") {
		t.Errorf("root-unavailable through a pid = %v, want the typed cause and no path", err)
	}
}

// stubResolver fails every reference with one error.
type stubResolver struct{ err error }

func (s stubResolver) Resolve(context.Context, string) (*File, error) { return nil, s.err }

// TestPIDLookupLogsOnceWhileDown: a host that answers headers and then
// stalls is still down, not back and down again.
func TestPIDLookupLogsOnceWhileDown(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	host := newPIDHost(t, answerStatus(http.StatusServiceUnavailable))
	l, _ := newLookup(t, host, PIDLookupOptions{Logger: logger, Timeout: 50 * time.Millisecond})
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
	host.setAnswer(func(_ string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	_, err = l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
	if strings.Contains(buf.String(), "answering again") || strings.Count(buf.String(), "unavailable") != 1 {
		t.Fatalf("a stalled answer was logged as a recovery:\n%s", buf.String())
	}
}

// TestPIDLookupCoalescesColdLookups: callers asking for one pid at once
// share one request to the host.
func TestPIDLookupCoalescesColdLookups(t *testing.T) {
	release := make(chan struct{})
	host := newPIDHost(t, func(_ string, w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		answerRef("lib/sub/a.wav")("", w, r)
	})
	l, _ := newLookup(t, host, PIDLookupOptions{})
	const callers = 10
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			f, err := l.Resolve(context.Background(), "pid:"+testPID)
			if err != nil {
				errs <- err
				return
			}
			f.Close()
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for l.flights.Waiting(testPID) < callers {
		if time.Now().After(deadline) {
			t.Fatalf("only %d callers joined the flight", l.flights.Waiting(testPID))
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := host.requests.Load(); n != 1 {
		t.Fatalf("host saw %d requests for one pid asked %d times at once, want 1", n, callers)
	}
}

// TestPIDLookupReusesItsConnection: an answer's body is drained before the
// connection is put back, so a run of 404s with envelopes rides one
// connection instead of opening one per lookup.
func TestPIDLookupReusesItsConnection(t *testing.T) {
	host := newPIDHost(t, func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such item","code":"not-found"}`)
	})
	l, _ := newLookup(t, host, PIDLookupOptions{})
	for range 3 {
		_, err := l.Resolve(context.Background(), "pid:"+testPID)
		wantCode(t, err, waxerr.CodeNotFound)
	}
	if n := host.conns.Load(); n != 1 {
		t.Fatalf("three 404 lookups opened %d connections, want 1", n)
	}
}

// TestPIDLookupCacheEvictsExpiredThenLeastRecentlyUsed pins the eviction
// order at capacity: an expired entry goes first, then the one used longest
// ago, never one that was just hit.
func TestPIDLookupCacheEvictsExpiredThenLeastRecentlyUsed(t *testing.T) {
	id := func(i int) string { return fmt.Sprintf("pid:%026d", i) }
	fill := func(t *testing.T, l *PIDLookup, from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			f, err := l.Resolve(context.Background(), id(i))
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}
	remembered := func(t *testing.T, l *PIDLookup, host *pidHost, ref string) bool {
		t.Helper()
		before := host.requests.Load()
		f, err := l.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		return host.requests.Load() == before
	}

	t.Run("expired first", func(t *testing.T) {
		host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=1"))
		l, _ := newLookup(t, host, PIDLookupOptions{})
		base := time.Now()
		var offset time.Duration
		l.now = func() time.Time { return base.Add(offset) }
		fill(t, l, 0, 1) // id 0 expires in a second
		offset = 2 * time.Second
		host.setAnswer(answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
		fill(t, l, 1, pidCacheEntries+1) // fills the cache, then one more
		if !remembered(t, l, host, id(1)) {
			t.Fatal("the oldest live entry was evicted ahead of the expired one")
		}
	})
	t.Run("then least recently used", func(t *testing.T) {
		host := newPIDHost(t, answerRef("lib/sub/a.wav", "Cache-Control", "max-age=60"))
		l, _ := newLookup(t, host, PIDLookupOptions{})
		fill(t, l, 0, pidCacheEntries)
		if !remembered(t, l, host, id(0)) { // a hit, so id 0 is now the most recently used
			t.Fatal("id 0 was not remembered")
		}
		fill(t, l, pidCacheEntries, pidCacheEntries+1)
		if !remembered(t, l, host, id(0)) {
			t.Fatal("the entry just hit was evicted")
		}
		if remembered(t, l, host, id(1)) {
			t.Fatal("the least recently used entry survived")
		}
	})
}

// TestPIDLookupAnswerInTextForm: the host may write any reference pct:
// escaped, which is how it names a file whose name is not UTF-8.
func TestPIDLookupAnswerInTextForm(t *testing.T) {
	host := newPIDHost(t, answerRef("pct:lib%2Fsub/a%2Ewav", "Cache-Control", "max-age=60"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	for range 2 {
		f, err := l.Resolve(context.Background(), "pid:"+testPID)
		if err != nil {
			t.Fatal(err)
		}
		if f.Ref != "pid:"+testPID || f.Ext != "wav" {
			t.Errorf("Ref = %q, Ext = %q, want the pid reference and wav", f.Ref, f.Ext)
		}
		if got := readAll(t, f); !strings.HasPrefix(got, "RIFF") {
			t.Errorf("ReadAt = %q, want the file's bytes", got)
		}
		f.Close()
	}
	if n := host.requests.Load(); n != 1 {
		t.Errorf("%d requests, want the remembered answer to serve the second", n)
	}
}

// TestPIDLookupNamesANonUTF8File needs a library that holds such a name.
func TestPIDLookupNamesANonUTF8File(t *testing.T) {
	name, escaped, ok := testutil.NonUTF8Name()
	if !ok {
		t.Skip("this platform's file names are all valid UTF-8")
	}
	host := newPIDHost(t, answerRef("pct:lib/sub/"+escaped))
	l, dir := newLookup(t, host, PIDLookupOptions{})
	if err := os.WriteFile(filepath.Join(dir, "sub", name), []byte("RIFFstray"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readAll(t, f); got != "RIFFstray" {
		t.Errorf("ReadAt = %q, want the non-UTF-8 file's bytes", got)
	}
}

// TestPIDLookupRefusesAnAnswerThatIsNotUTF8: a JSON decoder reads a stray
// byte as U+FFFD, so the answer would name another file. Here that file
// exists, and it must not be the one served.
func TestPIDLookupRefusesAnAnswerThatIsNotUTF8(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	host := newPIDHost(t, answerBody("{\"ref\":\"lib/sub/caf\xe9.wav\"}"))
	l, dir := newLookup(t, host, PIDLookupOptions{Logger: logger})
	if err := os.WriteFile(filepath.Join(dir, "sub", "caf�.wav"), []byte("RIFFother"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err == nil {
		defer f.Close()
		t.Fatalf("resolved %q for an answer that is not UTF-8", readAll(t, f))
	}
	wantCode(t, err, waxerr.CodeInternal)
	if !strings.Contains(buf.String(), "not UTF-8") || !strings.Contains(buf.String(), "pct:") {
		t.Errorf("the log does not say the answer is not UTF-8 or name the pct: form:\n%s", buf.String())
	}
}

func TestPIDLookupRefusesAMalformedEscape(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	host := newPIDHost(t, answerRef("pct:lib/sub/100%.wav"))
	l, _ := newLookup(t, host, PIDLookupOptions{Logger: logger})
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeInternal)
	if strings.Contains(err.Error(), "lib/sub") {
		t.Errorf("the client message carries the host's path: %v", err)
	}
	if !strings.Contains(buf.String(), "pct:lib/sub/100%.wav") || !strings.Contains(buf.String(), "two hex digits") {
		t.Errorf("the log does not carry the host's answer and what is wrong with it:\n%s", buf.String())
	}
}

// TestPIDLookupLogsAReferenceInTextForm: a JSON log line would otherwise
// carry U+FFFD where the stray byte was.
func TestPIDLookupLogsAReferenceInTextForm(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	host := newPIDHost(t, answerRef("pct:lib/sub/gone%E9.wav"))
	l, _ := newLookup(t, host, PIDLookupOptions{Logger: logger})
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeNotFound)
	if !strings.Contains(buf.String(), "pct:lib/sub/gone%E9.wav") {
		t.Errorf("the log does not name the reference in text form:\n%s", buf.String())
	}
}

// TestPIDLookupRefusesAnEscapedLoneSurrogate: \udce9 is ASCII, so the answer
// is valid UTF-8, and a decoder still reads it as U+FFFD. Python's json.dumps
// writes a name that is not UTF-8 this way.
func TestPIDLookupRefusesAnEscapedLoneSurrogate(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	host := newPIDHost(t, answerBody(`{"ref":"lib/sub/caf\udce9.wav"}`))
	l, dir := newLookup(t, host, PIDLookupOptions{Logger: logger})
	if err := os.WriteFile(filepath.Join(dir, "sub", "caf\ufffd.wav"), []byte("RIFFother"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err == nil {
		defer f.Close()
		t.Fatalf("resolved %q for an answer that escapes half a surrogate pair", readAll(t, f))
	}
	wantCode(t, err, waxerr.CodeInternal)
	if !strings.Contains(buf.String(), "surrogate") || !strings.Contains(buf.String(), "pct:") {
		t.Errorf("the log does not say what is wrong or name the pct: form:\n%s", buf.String())
	}
}

// TestPIDLookupIgnoresOtherFields: only ref names a file, so a stray byte in
// a field beside it does not cost the answer.
func TestPIDLookupIgnoresOtherFields(t *testing.T) {
	host := newPIDHost(t, answerBody("{\"ref\":\"lib/sub/a.wav\",\"title\":\"caf\xe9 \\udce9\"}"))
	l, _ := newLookup(t, host, PIDLookupOptions{})
	f, err := l.Resolve(context.Background(), "pid:"+testPID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// TestPIDLookupLogsABrokenAnswersBytes: a JSON log line writes U+FFFD for a
// stray byte, and the operator could not see which byte the host sent.
func TestPIDLookupLogsABrokenAnswersBytes(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	host := newPIDHost(t, answerBody("{\"ref\":\"lib/sub/caf\xe9.wav\"}"))
	l, _ := newLookup(t, host, PIDLookupOptions{Logger: logger})
	_, err := l.Resolve(context.Background(), "pid:"+testPID)
	wantCode(t, err, waxerr.CodeInternal)
	if got := buf.String(); !strings.Contains(got, `caf\\xe9.wav`) || strings.Contains(got, `\ufffd`) {
		t.Errorf("the log does not show the byte the host sent:\n%s", got)
	}
}

// TestClipKeepsWhatItShowsWhole: a cut inside a character or a %XX escape
// leaves text that is not the reference's.
func TestClipKeepsWhatItShowsWhole(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("a", 159) + "\u00e9" + strings.Repeat("b", 40),
		strings.Repeat("a", 158) + "%E9" + strings.Repeat("b", 40),
		strings.Repeat("a", 159) + "%E9" + strings.Repeat("b", 40),
	} {
		got := strings.TrimSuffix(clip(s), " (clipped)")
		if got == s || !strings.HasPrefix(s, got) || !utf8.ValidString(got) || len(got) > 160 {
			t.Errorf("clip gave %q", got)
		}
		if i := strings.LastIndexByte(got, '%'); i >= 0 && i > len(got)-3 {
			t.Errorf("clip cut an escape: %q", got[len(got)-8:])
		}
	}
	if got := clip("short"); got != "short" {
		t.Errorf("clip changed a short string: %q", got)
	}
	// An error quotes a name's own bytes. A run of them that looks like the
	// middle of a character must not walk the cut back to nothing, and what
	// is shown keeps them legible.
	got := clip(strings.Repeat("a", 100) + strings.Repeat("\x80", 200))
	if !strings.HasPrefix(got, strings.Repeat("a", 100)+`\x80`) || !utf8.ValidString(got) || len(got) > 180 {
		t.Errorf("clip of a run of stray bytes gave %q", got)
	}
	if got := clip(strings.Repeat("\x80", 400)); len(got) < 150 || !utf8.ValidString(got) {
		t.Errorf("clip of nothing but stray bytes gave %q", got)
	}
}

// TestPIDLookupURLRefusalsQuoteNoQuery: a refused lookup URL is quoted
// without its query, its fragment or its password. A query is where a token
// rides, and the refusal travels: to the daemon's log, and to the body of a
// reload the configuration fails.
func TestPIDLookupURLRefusalsQuoteNoQuery(t *testing.T) {
	for _, raw := range []string{
		"http://deck/pid?token=s3cret",
		"http://deck/pid#s3cret",
		"http://deck/pid?",
		"ftp://deck/pid?token=s3cret",
		"http:///pid?token=s3cret",
		"http://user:s3cret@deck/pid?k=s3cret#s3cret",
	} {
		_, err := ParsePIDLookupURL(raw)
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: %v, want an invalid-request refusal", raw, err)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "token") {
			t.Errorf("%s: the refusal quotes what it should not: %v", raw, err)
		}
		if !strings.Contains(err.Error(), "pid") {
			t.Errorf("%s: the refusal no longer says which URL: %v", raw, err)
		}
	}
}
