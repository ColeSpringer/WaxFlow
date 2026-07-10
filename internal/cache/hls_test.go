package cache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHLSVariantRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, Options{})
	key := NewKey("id", "hls-params", []string{"v1"})

	v, err := s.HLS(key, Meta{Ref: "lib/a.flac", Ext: "m4s", ContentType: "audio/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Has("seg-0.m4s") {
		t.Fatal("segment reported before write")
	}
	if _, ok := v.Open("seg-0.m4s"); ok {
		t.Fatal("open succeeded before write")
	}
	if err := v.WriteFile("seg-0.m4s", []byte("segment-zero")); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("init.mp4", []byte("init")); err != nil {
		t.Fatal(err)
	}
	if !v.Has("seg-0.m4s") || !v.Has("init.mp4") {
		t.Fatal("written files not reported")
	}
	c, ok := v.Open("seg-0.m4s")
	if !ok {
		t.Fatal("open missed a written segment")
	}
	got, err := io.ReadAll(c.File)
	c.File.Close()
	if err != nil || !bytes.Equal(got, []byte("segment-zero")) {
		t.Fatalf("read %q err %v", got, err)
	}
	if c.Meta.Kind != KindHLS {
		t.Fatalf("meta kind %q, want %q", c.Meta.Kind, KindHLS)
	}
	if st := s.Stats(); st.Bytes != int64(len("segment-zero")+len("init")) {
		t.Fatalf("accounted %d bytes, want %d", st.Bytes, len("segment-zero")+len("init"))
	}

	// Rewriting replaces and adjusts by the difference.
	if err := v.WriteFile("seg-0.m4s", []byte("longer-segment-zero")); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Bytes != int64(len("longer-segment-zero")+len("init")) {
		t.Fatalf("accounted %d bytes after rewrite", st.Bytes)
	}

	// A second HLS on the same key attaches without rewriting meta.
	v2, err := s.HLS(key, Meta{Ref: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := v2.Open("seg-0.m4s"); !ok {
		t.Fatal("second handle cannot open")
	} else {
		c.File.Close()
		if c.Meta.Ref != "lib/a.flac" {
			t.Fatalf("meta ref %q rewritten", c.Meta.Ref)
		}
	}
}

func TestHLSVariantNameValidation(t *testing.T) {
	s := openTestStore(t, t.TempDir(), Options{})
	v, err := s.HLS(NewKey("id", "p", nil), Meta{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "meta.json", "../escape", "a/b", `a\b`} {
		if err := v.WriteFile(name, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) accepted", name)
		}
		if v.Has(name) {
			t.Errorf("Has(%q) true", name)
		}
	}
}

func TestHLSVariantScanRebuild(t *testing.T) {
	dir := t.TempDir()
	key := NewKey("id", "hls-params", []string{"v1"})
	func() {
		s := openTestStore(t, dir, Options{})
		v, err := s.HLS(key, Meta{Ext: "m4s"})
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("seg-0.m4s", make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("seg-1.m4s", make([]byte, 50)); err != nil {
			t.Fatal(err)
		}
		// A crash-interrupted temp write is debris the rescan removes.
		if err := os.WriteFile(filepath.Join(v.dir, "seg-2.m4s.tmp-9"), make([]byte, 7), 0o644); err != nil {
			t.Fatal(err)
		}
	}()

	s := openTestStore(t, dir, Options{})
	v, err := s.HLS(key, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Has("seg-0.m4s") || !v.Has("seg-1.m4s") {
		t.Fatal("segments lost across restart")
	}
	if v.Has("seg-2.m4s.tmp-9") {
		t.Fatal("temp debris survived the rescan")
	}
	st := s.Stats()
	// 150 payload bytes plus the meta.json the size walk includes.
	metaSize, err := os.Stat(filepath.Join(v.dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(150) + metaSize.Size(); st.Bytes != want {
		t.Fatalf("rescanned %d bytes, want %d", st.Bytes, want)
	}
}

func TestHLSVariantEvictionAndPinning(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, Options{MaxBytes: 1}) // any content is over budget
	key := NewKey("id", "hls-params", []string{"v1"})
	v, err := s.HLS(key, Meta{Ext: "m4s"})
	if err != nil {
		t.Fatal(err)
	}
	s.Pin(key)
	if err := v.WriteFile("seg-0.m4s", make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if removed, _ := s.GC(); removed != 0 {
		t.Fatalf("gc evicted %d pinned entries", removed)
	}
	if !v.Has("seg-0.m4s") {
		t.Fatal("pinned variant lost its segment")
	}
	s.Unpin(key)
	if removed, _ := s.GC(); removed != 1 {
		t.Fatalf("gc removed %d, want the unpinned variant", removed)
	}
	if v.Has("seg-0.m4s") {
		t.Fatal("evicted variant still has files")
	}
	if st := s.Stats(); st.Bytes != 0 || st.Entries != 0 {
		t.Fatalf("stats %+v after eviction", st)
	}
}

// TestHLSKeyNeverServesProgressive pins the layout guard: a progressive
// Lookup on an HLS key must miss without dropping the variant.
func TestHLSKeyNeverServesProgressive(t *testing.T) {
	s := openTestStore(t, t.TempDir(), Options{})
	key := NewKey("id", "hls-params", []string{"v1"})
	v, err := s.HLS(key, Meta{Ext: "m4s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("seg-0.m4s", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if c := s.Lookup(key); c != nil {
		c.File.Close()
		t.Fatal("progressive Lookup served an HLS variant")
	}
	if !v.Has("seg-0.m4s") {
		t.Fatal("the miss dropped the variant")
	}
}
