package cache

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestIdxStoreRoundTrip(t *testing.T) {
	s, err := OpenIdx(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Load("missing"); got != nil {
		t.Errorf("Load(missing) = %d bytes, want nil", len(got))
	}
	blob := []byte("WXMPAIDX1\x00 pretend index payload")
	s.Save("lib/a.mp3|123-456", blob)
	if got := s.Load("lib/a.mp3|123-456"); !bytes.Equal(got, blob) {
		t.Errorf("round trip mismatch: %q", got)
	}
	// A different identity misses.
	if got := s.Load("lib/a.mp3|123-457"); got != nil {
		t.Error("changed identity still hits")
	}
	// Empty and oversized blobs are refused.
	s.Save("empty", nil)
	if s.Load("empty") != nil {
		t.Error("empty blob was stored")
	}
	// Drop removes a blob for good (the rejected-blob path).
	s.Drop("lib/a.mp3|123-456")
	if s.Load("lib/a.mp3|123-456") != nil {
		t.Error("dropped blob still loads")
	}
	s.Drop("never-existed") // must not panic or disturb the total
}

func TestIdxStoreTrim(t *testing.T) {
	// Cap small enough that only some blobs survive; the survivors must
	// be the most recently touched ones.
	s, err := OpenIdx(t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 1024)
	for i := range 8 {
		s.Save(fmt.Sprint("id-", i), blob)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	if total > 4096 {
		t.Errorf("directory holds %d bytes past the %d cap", total, 4096)
	}
	// The last write always survives its own trim.
	if s.Load("id-7") == nil {
		t.Error("most recent blob was evicted")
	}
}

// TestIdxStoreConcurrentSaves exercises the write path from many
// goroutines, same identity included: the race detector proves the
// serialization and Load must always see a whole blob.
func TestIdxStoreConcurrentSaves(t *testing.T) {
	s, err := OpenIdx(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte("idx"), 4096)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				s.Save("shared", blob)
				s.Save(fmt.Sprint("own-", w, "-", i), blob)
				if got := s.Load("shared"); got != nil && !bytes.Equal(got, blob) {
					t.Errorf("torn blob: %d bytes", len(got))
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := s.Load("shared"); !bytes.Equal(got, blob) {
		t.Error("final blob does not round-trip")
	}
}
