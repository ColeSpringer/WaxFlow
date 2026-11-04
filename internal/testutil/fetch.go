package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/posixfs"
)

// VectorsDir returns the on-disk vector cache, testdata/vectors under the
// repository root (located relative to this source file).
func VectorsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: cannot locate source file for vectors dir")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "vectors")
}

// VectorPath returns the local path of a fetched vector. Tests self-skip
// when it has not been fetched; WAXFLOW_REQUIRE_VECTORS=1 (set by the CI
// jobs that fetch the vectors first) escalates absence to failure.
func VectorPath(t testing.TB, name string) string {
	t.Helper()
	path := filepath.Join(VectorsDir(), filepath.FromSlash(name))
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_VECTORS") == "1" {
			t.Fatalf("vector %s required by WAXFLOW_REQUIRE_VECTORS=1 but not fetched (run `make verify-vectors`)", name)
		}
		t.Skipf("vector %s not fetched (run `make verify-vectors`); skipping", name)
	}
	return path
}

// Fetch downloads vectors into dir, verifying each digest. Files already
// present with a matching digest are kept; mismatches are re-downloaded,
// and a mismatched download is an error. Progress goes to w.
func Fetch(w io.Writer, dir string, vectors []Vector) error {
	for _, v := range vectors {
		path := filepath.Join(dir, filepath.FromSlash(v.Name))
		if sum, err := fileSHA256(path); err == nil {
			if sum == v.SHA256 {
				fmt.Fprintf(w, "ok       %s (cached)\n", v.Name)
				continue
			}
			fmt.Fprintf(w, "refetch  %s (digest changed)\n", v.Name)
		}
		if err := fetchOne(w, path, v); err != nil {
			return fmt.Errorf("fetching %s: %w", v.Name, err)
		}
		fmt.Fprintf(w, "fetched  %s\n", v.Name)
	}
	fmt.Fprintf(w, "%d vector(s) verified in %s\n", len(vectors), dir)
	return nil
}

// Prune removes every regular file under dir that no vector in the list
// names, and any directory that leaves empty. CI restores the vector cache
// from an older pin list when the list changes; without this, every
// superseded archive would ride along into each cache saved after it. Links
// are someone's own arrangement and are left alone, as is whatever they lead
// to, but a linked dir is pruned through. A dir not made yet has nothing to
// prune.
func Prune(w io.Writer, dir string, vectors []Vector) error {
	root, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pinned := make(map[string]bool, len(vectors))
	for _, v := range vectors {
		pinned[v.Name] = true
	}
	var dirs []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if pinned[name] {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed  %s (not pinned)\n", name)
		return nil
	})
	if err != nil {
		return err
	}
	// The walk lists a directory before its contents, so walking the list
	// backwards empties a tree from the leaves up.
	for _, d := range slices.Backward(dirs) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(d); err != nil {
				return err
			}
		}
	}
	return nil
}

// fetchClient bounds a fetch that stalls: a host that never answers the
// connect, or accepts it and then says nothing, must not hang a CI job until
// the runner's own ceiling. The dial, handshake and header timeouts bound
// each phase of an attempt (30 s, 30 s, a minute); without the dial timeout
// a connect waits on the operating system's, about two minutes on Linux.
// The overall deadline rides on the context instead, so that it covers a
// vector rather than a request (fetchBudget).
var fetchClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: fetchDialTimeout}
			return d.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

// fetchAttempts, fetchRetryDelay, fetchRetryCap and fetchBudget bound the
// repeats of one vector. A host that drops connections must not sink the
// whole run, which saves nothing it fetched to the CI cache when it fails.
// The delay doubles up to the cap, so the attempts span about three minutes
// of waiting: fate-suite.ffmpeg.org once refused connections for over a
// minute. A host that names its own Retry-After is heard out up to the same
// cap. The budget is generous because the largest pinned vector is a 183 MB
// archive, and it covers every attempt together: a link slow enough to
// spend it will not do better from byte zero, and fresh tries would outlast
// the CI job the retry is there to keep alive.
var (
	fetchAttempts   = 8
	fetchRetryDelay = 2 * time.Second
	fetchRetryCap   = time.Minute
	fetchBudget     = 30 * time.Minute
)

// fetchDialTimeout bounds one connect, as Go's default transport does.
var fetchDialTimeout = 30 * time.Second

// permanentError marks a failure a second attempt cannot fix: a status the
// host will keep returning, bytes that do not match the pin, or a temp file
// this machine cannot create, close or put in place. A mismatch is the
// supply-chain check doing its job and has to fail loudly rather than be
// retried away: every pinned host frames its responses, so a truncated
// transfer arrives as a read error, not as a short body.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// retryAfterError carries a host's own Retry-After beside a status worth
// another attempt. The backoff cannot know how long a rate limit runs for;
// the host does.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e retryAfterError) Error() string { return e.err.Error() }
func (e retryAfterError) Unwrap() error { return e.err }

// retryAfter reads the header in either spelling, delta-seconds or an
// HTTP-date. Anything else, a date already past included, reads as silence.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// fetchOne downloads one vector, repeating a transient failure until the
// attempts or the budget run out.
func fetchOne(w io.Writer, path string, v Vector) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchBudget)
	defer cancel()
	for attempt := 1; ; attempt++ {
		err := fetchAttempt(ctx, path, v)
		var permanent permanentError
		if err == nil || errors.As(err, &permanent) {
			return err
		}
		if attempt == fetchAttempts || ctx.Err() != nil {
			return err
		}
		fmt.Fprintf(w, "retry    %s (attempt %d of %d failed: %v)\n", v.Name, attempt, fetchAttempts, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryDelay(attempt, err)):
		}
	}
}

// retryDelay is the wait after the given failed attempt: doubling from
// fetchRetryDelay up to fetchRetryCap, and raised to a host's own
// Retry-After up to the same cap.
func retryDelay(attempt int, err error) time.Duration {
	delay := fetchRetryDelay
	for i := 1; i < attempt && delay < fetchRetryCap; i++ {
		delay *= 2
	}
	delay = min(delay, fetchRetryCap)
	if wait, ok := errors.AsType[retryAfterError](err); ok {
		delay = max(delay, min(wait.after, fetchRetryCap))
	}
	return delay
}

func fetchAttempt(ctx context.Context, path string, v Vector) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.URL, nil)
	if err != nil {
		return permanentError{err}
	}
	// Name the fetcher: Go's default headers are enough of a bot signature
	// that at least one of the pinned hosts answers them with a 406.
	req.Header.Set("User-Agent", "waxflow-vectorfetch/1 (+https://github.com/colespringer/waxflow)")
	req.Header.Set("Accept", "*/*")
	resp, err := fetchClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		status := fmt.Errorf("GET %s: %s", v.URL, resp.Status)
		if !retryableStatus(resp.StatusCode) {
			return permanentError{status}
		}
		return retryAfterError{err: status, after: retryAfter(resp.Header)}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return permanentError{err}
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return permanentError{err}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.SHA256 {
		return permanentError{fmt.Errorf("digest mismatch: got %s, pinned %s", got, v.SHA256)}
	}
	if err := posixfs.Replace(tmp.Name(), path); err != nil {
		return permanentError{err}
	}
	return nil
}

// retryableStatus reports whether a status is the host asking for a later
// attempt rather than refusing the request. The rest of the 5xx range is as
// settled as a 404: 501 and 505 answer the request itself, not the moment.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
