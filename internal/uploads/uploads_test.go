package uploads

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/colespringer/waxflow/internal/ulid"
	"github.com/colespringer/waxflow/waxerr"
)

func openStore(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// dirNames lists the spool directory for residue checks.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// endlessReader yields zeros forever, standing in for a client that
// streams an unbounded body.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	payload := []byte("not actually flac bytes")

	it, err := s.Put(bytes.NewReader(payload), "song.flac")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !ulid.Valid(it.ID) {
		t.Errorf("Put id %q is not a valid ulid", it.ID)
	}
	if it.Name != "song.flac" {
		t.Errorf("Put name = %q, want %q", it.Name, "song.flac")
	}
	if it.Bytes != int64(len(payload)) {
		t.Errorf("Put bytes = %d, want %d", it.Bytes, len(payload))
	}

	got, path, err := s.Get(it.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("Get path %q is not absolute", path)
	}
	if got.ID != it.ID || got.Name != it.Name || got.Bytes != it.Bytes || !got.Created.Equal(it.Created) {
		t.Errorf("Get item = %+v, want %+v", got, it)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading spooled bytes: %v", err)
	}
	if !bytes.Equal(b, payload) {
		t.Errorf("spooled bytes differ: got %d bytes, want %d", len(b), len(payload))
	}
	if n := s.Bytes(); n != int64(len(payload)) {
		t.Errorf("Bytes = %d, want %d", n, len(payload))
	}
	if items := s.Items(); len(items) != 1 || items[0].ID != it.ID {
		t.Errorf("Items = %+v, want the one upload", items)
	}

	if err := s.Delete(it.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := s.Get(it.ID); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Errorf("Get after Delete: code %q, want %q", waxerr.CodeOf(err), waxerr.CodeNotFound)
	}
	if err := s.Delete(it.ID); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Errorf("second Delete: code %q, want %q", waxerr.CodeOf(err), waxerr.CodeNotFound)
	}
	if n := s.Bytes(); n != 0 {
		t.Errorf("Bytes after Delete = %d, want 0", n)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("residue after Delete: %v", names)
	}
}

func TestPerUploadCap(t *testing.T) {
	t.Run("over-cap upload is rejected without residue", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{MaxBytes: 100})
		_, err := s.Put(bytes.NewReader(make([]byte, 101)), "big")
		if waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
			t.Fatalf("Put: code %q, want %q", waxerr.CodeOf(err), waxerr.CodePayloadTooLarge)
		}
		if names := dirNames(t, dir); len(names) != 0 {
			t.Errorf("residue after rejected Put: %v", names)
		}
		if n := s.Bytes(); n != 0 {
			t.Errorf("Bytes = %d, want 0", n)
		}
		if items := s.Items(); len(items) != 0 {
			t.Errorf("Items = %+v, want none", items)
		}
	})
	t.Run("exactly at cap fits", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{MaxBytes: 100})
		it, err := s.Put(bytes.NewReader(make([]byte, 100)), "")
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if it.Bytes != 100 {
			t.Errorf("Put bytes = %d, want 100", it.Bytes)
		}
	})
	t.Run("endless body stops at the cap", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{MaxBytes: 100})
		_, err := s.Put(endlessReader{}, "")
		if waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
			t.Fatalf("Put: code %q, want %q", waxerr.CodeOf(err), waxerr.CodePayloadTooLarge)
		}
		if names := dirNames(t, dir); len(names) != 0 {
			t.Errorf("residue after rejected Put: %v", names)
		}
	})
}

func TestAggregateCap(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{MaxTotalBytes: 100})

	first, err := s.Put(bytes.NewReader(make([]byte, 60)), "a")
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if _, err := s.Put(bytes.NewReader(make([]byte, 60)), "b"); waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
		t.Fatalf("over-aggregate Put: code %q, want %q", waxerr.CodeOf(err), waxerr.CodePayloadTooLarge)
	}
	if n := s.Bytes(); n != 60 {
		t.Errorf("Bytes after rejection = %d, want 60", n)
	}
	if names := dirNames(t, dir); len(names) != 2 {
		t.Errorf("dir after rejection = %v, want the first upload's pair", names)
	}

	// Filling to exactly the cap is allowed.
	if _, err := s.Put(bytes.NewReader(make([]byte, 40)), "c"); err != nil {
		t.Fatalf("Put to exactly the cap: %v", err)
	}
	if _, err := s.Put(bytes.NewReader([]byte{0}), "d"); waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
		t.Fatalf("Put past full spool: code %q, want %q", waxerr.CodeOf(err), waxerr.CodePayloadTooLarge)
	}

	// Deleting frees headroom for a new upload.
	if err := s.Delete(first.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Put(bytes.NewReader(make([]byte, 60)), "e"); err != nil {
		t.Fatalf("Put after Delete: %v", err)
	}
	if n := s.Bytes(); n != 100 {
		t.Errorf("Bytes = %d, want 100", n)
	}
}

func TestRestartAdoption(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Put(strings.NewReader("first payload"), "a.wav")
	if err != nil {
		t.Fatalf("Put a: %v", err)
	}
	b, err := s.Put(strings.NewReader("second"), "")
	if err != nil {
		t.Fatalf("Put b: %v", err)
	}
	s.Close()

	// Plant debris a crash could leave behind: an interrupted spool
	// write, a sidecar whose payload never landed, and the reverse.
	orphanSidecarID, err := ulid.Make(time.UnixMilli(1), bytes.NewReader([]byte("0123456789")))
	if err != nil {
		t.Fatal(err)
	}
	orphanPayloadID, err := ulid.Make(time.UnixMilli(2), bytes.NewReader([]byte("abcdefghij")))
	if err != nil {
		t.Fatal(err)
	}
	for _, stray := range []struct{ name, content string }{
		{"junk.tmp", "interrupted spool write"},
		{orphanSidecarID + ".json", `{"name":"ghost","created":1}`},
		{orphanPayloadID, "payload without metadata"},
	} {
		if err := os.WriteFile(filepath.Join(dir, stray.name), []byte(stray.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s2 := openStore(t, dir, Options{})
	items := s2.Items()
	if len(items) != 2 {
		t.Fatalf("Items after reopen = %+v, want 2", items)
	}
	for _, want := range []*Item{a, b} {
		got, path, err := s2.Get(want.ID)
		if err != nil {
			t.Fatalf("Get %s after reopen: %v", want.ID, err)
		}
		if got.Name != want.Name || got.Bytes != want.Bytes || !got.Created.Equal(want.Created) {
			t.Errorf("adopted item = %+v, want %+v", got, want)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("adopted payload missing: %v", err)
		}
	}
	if n := s2.Bytes(); n != a.Bytes+b.Bytes {
		t.Errorf("Bytes after reopen = %d, want %d", n, a.Bytes+b.Bytes)
	}
	names := dirNames(t, dir)
	if len(names) != 4 {
		t.Errorf("dir after reopen = %v, want only the two adopted pairs", names)
	}
	for _, n := range names {
		if strings.Contains(n, "junk") || strings.Contains(n, orphanSidecarID) || strings.Contains(n, orphanPayloadID) {
			t.Errorf("stray %q survived the rescan", n)
		}
	}
}

func TestTTLExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{TTL: time.Hour})

		it, err := s.Put(strings.NewReader("doomed"), "d.wav")
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		_, path, err := s.Get(it.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		// Halfway through the TTL the upload must survive the janitor.
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		if len(s.Items()) != 1 {
			t.Fatal("upload expired before its TTL")
		}

		// Past the TTL the janitor removes the item and its files.
		time.Sleep(31 * time.Minute)
		synctest.Wait()
		if items := s.Items(); len(items) != 0 {
			t.Fatalf("Items after TTL = %+v, want none", items)
		}
		if n := s.Bytes(); n != 0 {
			t.Errorf("Bytes after TTL = %d, want 0", n)
		}
		if _, _, err := s.Get(it.ID); waxerr.CodeOf(err) != waxerr.CodeNotFound {
			t.Errorf("Get after TTL: code %q, want %q", waxerr.CodeOf(err), waxerr.CodeNotFound)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("payload survived TTL: %v", err)
		}
		if _, err := os.Stat(path + ".json"); !os.IsNotExist(err) {
			t.Errorf("sidecar survived TTL: %v", err)
		}

		// The spool keeps working after an expiry.
		fresh, err := s.Put(strings.NewReader("fresh"), "")
		if err != nil {
			t.Fatalf("Put after expiry: %v", err)
		}
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		if _, _, err := s.Get(fresh.ID); err != nil {
			t.Errorf("fresh upload expired early: %v", err)
		}
	})
}

func TestMalformedIDs(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	unknown, err := ulid.Make(time.UnixMilli(3), bytes.NewReader([]byte("jklmnopqrs")))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"dots", ".."},
		{"traversal", "../x"},
		{"absolute path", "/etc/passwd"},
		{"too short", strings.Repeat("0", 25)},
		{"too long", strings.Repeat("0", 27)},
		{"lowercase", "01aryz6s41vtpvxvr024h36h2n"},
		{"excluded letter", strings.Repeat("U", 26)},
		{"traversal at ulid length", "../../../../../../../etc/x"},
		{"well-formed but unknown", unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.Get(tt.id); waxerr.CodeOf(err) != waxerr.CodeNotFound {
				t.Errorf("Get(%q): code %q, want %q", tt.id, waxerr.CodeOf(err), waxerr.CodeNotFound)
			}
			if err := s.Delete(tt.id); waxerr.CodeOf(err) != waxerr.CodeNotFound {
				t.Errorf("Delete(%q): code %q, want %q", tt.id, waxerr.CodeOf(err), waxerr.CodeNotFound)
			}
		})
	}
}

func TestNameValidation(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	if _, err := s.Put(strings.NewReader("x"), strings.Repeat("n", 256)); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("256-byte name: code %q, want %q", waxerr.CodeOf(err), waxerr.CodeInvalidRequest)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("residue after rejected name: %v", names)
	}
	it, err := s.Put(strings.NewReader("x"), strings.Repeat("n", 255))
	if err != nil {
		t.Fatalf("255-byte name: %v", err)
	}
	if len(it.Name) != 255 {
		t.Errorf("name length = %d, want 255", len(it.Name))
	}
}

func TestConcurrentPuts(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	const workers, each = 8, 10
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, err := s.Put(strings.NewReader("payload"), fmt.Sprintf("w%d-%d", w, i)); err != nil {
					t.Errorf("Put: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if n := len(s.Items()); n != workers*each {
		t.Errorf("Items = %d, want %d", n, workers*each)
	}
	if n := s.Bytes(); n != int64(workers*each*len("payload")) {
		t.Errorf("Bytes = %d, want %d", n, workers*each*len("payload"))
	}
}
