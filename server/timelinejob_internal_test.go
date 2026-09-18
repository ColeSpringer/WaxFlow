package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/source"
)

// memIndexCache is a waxflow.IndexCache in memory, keyed by source size,
// which is enough for a root whose files all differ in size.
type memIndexCache struct {
	blobs map[string][]byte
	saves int
}

func (c *memIndexCache) key(src container.Source) string  { return fmt.Sprint(src.Size()) }
func (c *memIndexCache) Load(src container.Source) []byte { return c.blobs[c.key(src)] }
func (c *memIndexCache) Save(src container.Source, blob []byte) {
	c.saves++
	c.blobs[c.key(src)] = blob
}
func (c *memIndexCache) Drop(src container.Source) { delete(c.blobs, c.key(src)) }

// TestTimelineJobGateAsksTheFile pins the job gate's question, whether
// measuring this member costs a full scan, as the member's own answer
// through format.Walker: not exact and not yet walked. Both directions are
// asserted on one container, since a gate that answered the same way for
// both would be no gate; the shapes that fell through the old Indexer mark
// (ADTS, an advisory-length Matroska, and an ASF whose rounded positions make
// its measure a decode) take the job path; and a sidecar restored complete
// mints inline.
func TestTimelineJobGateAsksTheFile(t *testing.T) {
	root := t.TempDir()
	for _, f := range []struct{ src, dst string }{
		{filepath.Join("..", "testdata", "sine-s16.wav"), "pcm.wav"},
		{filepath.Join("..", "container", "riff", "testdata", "mp3.wav"), "mp3.wav"},
		{filepath.Join("..", "testdata", "sine-ima.wav"), "ima-nofact.wav"},
		{filepath.Join("..", "container", "adts", "testdata", "stereo.aac"), "stereo.aac"},
		{filepath.Join("..", "container", "mka", "testdata", "seed-pcm.mka"), "pcm.mka"},
		{filepath.Join("..", "container", "mka", "testdata", "seed-cues.mka"), "cues.mka"},
		{filepath.Join("..", "testdata", "sine-s16.wma"), "sine.wma"},
	} {
		b, err := os.ReadFile(f.src)
		if err != nil {
			t.Fatal(err)
		}
		if f.dst == "ima-nofact.wav" {
			b = testutil.WAVWithoutChunk(t, b, "fact")
		}
		if err := os.WriteFile(filepath.Join(root, f.dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A bare MP3 long enough for its index to be worth a sidecar (the
	// snapshot floor is 4096 frames).
	if err := os.WriteFile(filepath.Join(root, "long.mp3"), testutil.SyntheticMP3Frames(4200), 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	cache := &memIndexCache{blobs: map[string][]byte{}}
	s := &Server{eng: waxflow.New(waxflow.WithIndexCache(cache)), resolver: roots}

	resolve := func(ref string) *source.File {
		t.Helper()
		f, err := s.resolver.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	ask := func(ref string) bool {
		t.Helper()
		f := resolve(ref)
		defer f.Close()
		slow, err := s.timelineNeedsJob([]*source.File{f})
		if err != nil {
			t.Fatal(err)
		}
		return slow
	}

	for _, tc := range []struct {
		ref  string
		slow bool
	}{
		{"lib/pcm.wav", false},        // byte-linear: exact from its byte count
		{"lib/mp3.wav", true},         // a frame run behind a WAV: the walk is the measure
		{"lib/ima-nofact.wav", false}, // a block codec: exact, whatever the fact chunk said
		{"lib/stereo.aac", true},      // ADTS states no length; the index is the measure
		{"lib/pcm.mka", true},         // an advisory Info Duration, and no walk at open
		{"lib/cues.mka", true},        // Opus: the cluster walk is the measure, and no open pays it
		{"lib/long.mp3", true},        // a cold bare MP3
		{"lib/sine.wma", true},        // rounded positions: measuring it is a decode
	} {
		if slow := ask(tc.ref); slow != tc.slow {
			t.Errorf("%s: needs a job = %v, want %v", tc.ref, slow, tc.slow)
		}
	}

	// Measure the long MP3 once, through the same engine: Close saves its
	// complete index, and the file then mints inline.
	f := resolve("lib/long.mp3")
	med, err := s.eng.OpenStream(f, f.Ext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := med.SeekSample(1 << 40); err != nil {
		t.Fatal(err)
	}
	med.Close()
	f.Close()
	if cache.saves != 1 {
		t.Fatalf("saves = %d, want the one complete index", cache.saves)
	}
	if ask("lib/long.mp3") {
		t.Error("an MP3 whose sidecar index is complete still needs a job")
	}
}
