package sign

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

func newSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := New([]Key{{ID: "1", Secret: []byte("topsecret")}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testQuery() url.Values {
	return url.Values{
		"src":    {"lib/Some Album/01 - Track.flac"},
		"format": {"wav"},
		"id":     {"12345-987654321"},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newSigner(t)
	q := s.Sign("GET", "/stream", testQuery(), time.Now().Add(time.Hour))
	if err := s.Verify("GET", "/stream", q); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// HEAD normalizes to GET in both directions.
	if err := s.Verify("HEAD", "/stream", q); err != nil {
		t.Fatalf("HEAD verify: %v", err)
	}
	qh := s.Sign("HEAD", "/stream", testQuery(), time.Now().Add(time.Hour))
	if err := s.Verify("GET", "/stream", qh); err != nil {
		t.Fatalf("GET verify of HEAD-signed: %v", err)
	}
	// The signed values survive a URL encode/parse cycle.
	parsed, err := url.ParseQuery(q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify("GET", "/stream", parsed); err != nil {
		t.Fatalf("verify after encode/parse: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	s := newSigner(t)
	base := s.Sign("GET", "/stream", testQuery(), time.Now().Add(time.Hour))

	tamper := []func(q url.Values){
		func(q url.Values) { q.Set("src", "lib/other.flac") },
		func(q url.Values) { q.Set("format", "flac") },
		func(q url.Values) { q.Set("id", "12345-1") },
		func(q url.Values) { q.Set("exp", "99999999999") },
		func(q url.Values) { q.Set("kid", "2") },
		func(q url.Values) { q.Add("t", "30") },
		func(q url.Values) { q.Del("exp") },
		func(q url.Values) { q.Set("sig", "AAAA") },
		func(q url.Values) { q.Set("sig", "!!not-base64!!") },
	}
	for i, f := range tamper {
		q, _ := url.ParseQuery(base.Encode())
		f(q)
		err := s.Verify("GET", "/stream", q)
		if waxerr.CodeOf(err) != waxerr.CodeSignatureInvalid {
			t.Errorf("tamper %d: code = %s (%v), want signature-invalid", i, waxerr.CodeOf(err), err)
		}
	}

	// Different method or path invalidates too.
	if err := s.Verify("DELETE", "/stream", base); err == nil {
		t.Error("method change verified")
	}
	if err := s.Verify("GET", "/hls/media.m3u8", base); err == nil {
		t.Error("path change verified")
	}
}

func TestVerifyExpiry(t *testing.T) {
	s := newSigner(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }

	q := s.Sign("GET", "/stream", testQuery(), now.Add(time.Hour))
	if err := s.Verify("GET", "/stream", q); err != nil {
		t.Fatalf("fresh URL: %v", err)
	}
	// Within leeway of expiry still verifies; past it, expired.
	s.now = func() time.Time { return now.Add(time.Hour + Leeway - time.Second) }
	if err := s.Verify("GET", "/stream", q); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
	s.now = func() time.Time { return now.Add(time.Hour + Leeway + time.Second) }
	if got := waxerr.CodeOf(s.Verify("GET", "/stream", q)); got != waxerr.CodeSignatureExpired {
		t.Fatalf("past leeway: code = %s, want signature-expired", got)
	}
	// A tampered expired URL reports invalid, not expired: exp is only
	// trustworthy once the signature checks out.
	q.Set("src", "lib/other.flac")
	if got := waxerr.CodeOf(s.Verify("GET", "/stream", q)); got != waxerr.CodeSignatureInvalid {
		t.Fatalf("tampered+expired: code = %s, want signature-invalid", got)
	}
}

func TestKeyRotation(t *testing.T) {
	old := Key{ID: "1", Secret: []byte("old")}
	fresh := Key{ID: "2", Secret: []byte("new")}
	sOld, _ := New([]Key{old})
	sBoth, _ := New([]Key{fresh, old})

	q := sOld.Sign("GET", "/stream", testQuery(), time.Now().Add(time.Hour))
	if err := sBoth.Verify("GET", "/stream", q); err != nil {
		t.Fatalf("old-key URL after rotation: %v", err)
	}
	minted := sBoth.Sign("GET", "/stream", testQuery(), time.Now().Add(time.Hour))
	if minted.Get("kid") != "2" {
		t.Fatalf("minting kid = %q, want the first key", minted.Get("kid"))
	}
}

func TestCanonicalQuery(t *testing.T) {
	q := url.Values{
		"b":   {"2", "1"},
		"a":   {"x y", "x/z"},
		"sig": {"excluded"},
		"u":   {"~-._"},
	}
	got := CanonicalQuery(q)
	want := "a=x%20y&a=x%2Fz&b=1&b=2&u=~-._"
	if got != want {
		t.Fatalf("canonical query\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "sig") {
		t.Fatal("sig leaked into the canonical string")
	}
}

func TestParseKeys(t *testing.T) {
	keys, err := ParseKeys("plainsecret")
	if err != nil || len(keys) != 1 || keys[0].ID != "1" || string(keys[0].Secret) != "plainsecret" {
		t.Fatalf("plain secret: %+v, %v", keys, err)
	}
	keys, err = ParseKeys("a:00ff, b:0a0b")
	if err != nil || len(keys) != 2 || keys[0].ID != "a" || keys[1].ID != "b" {
		t.Fatalf("rotation list: %+v, %v", keys, err)
	}
	for _, bad := range []string{"", "a:xyz", "a:00ff,plain", "a:"} {
		if _, err := ParseKeys(bad); err == nil {
			t.Errorf("ParseKeys(%q) accepted", bad)
		}
	}
}

func TestLoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signing-secret")
	k1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1.Secret) != 32 || k1.ID != "1" {
		t.Fatalf("generated key: %+v", k1)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %v, want 0600", fi.Mode().Perm())
	}
	k2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(k2.Secret) != string(k1.Secret) {
		t.Fatal("second load did not return the persisted secret")
	}
	// The temp file used for crash-safe publication must not linger.
	leftovers, _ := filepath.Glob(path + ".tmp-*")
	if len(leftovers) != 0 {
		t.Fatalf("temp debris: %v", leftovers)
	}
}

func TestLoadOrCreateConcurrent(t *testing.T) {
	// Concurrent first-run creators (daemon plus `waxflow sign`) must
	// converge on one secret; the no-replace publication guarantees it.
	path := filepath.Join(t.TempDir(), "signing-secret")
	const racers = 8
	keys := make([]Key, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = LoadOrCreate(path)
		}()
	}
	wg.Wait()
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if string(keys[i].Secret) != string(keys[0].Secret) {
			t.Fatal("racers hold different secrets")
		}
	}
	leftovers, _ := filepath.Glob(path + ".tmp-*")
	if len(leftovers) != 0 {
		t.Fatalf("temp debris: %v", leftovers)
	}
}
