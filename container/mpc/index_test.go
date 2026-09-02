package mpc_test

import (
	"os"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
)

// readVector loads a pinned vector, or reports why it cannot.
func readVector(t testing.TB, name string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(testutil.VectorPath(t, name))
}

// TestIndexRoundTrip pins the Indexer: a snapshot taken after a seek restores
// into a fresh demuxer, the restored demuxer seeks without rescanning, and a
// foreign or stale blob is rejected.
func TestIndexRoundTrip(t *testing.T) {
	defer mpc.SetIdxMinStatesForTest(2)()
	for _, name := range []string{"seek-sv7.mpc", "seek-pns.mpc"} {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)
			d := open(t, raw, true)
			track := d.Tracks()[0]
			end := track.Delay + track.Samples
			if _, err := d.SeekSample(0, end-1); err != nil {
				t.Fatal(err)
			}
			scanned := d.ScannedForTest()
			if scanned == 0 {
				t.Fatal("a seek to the end scanned nothing")
			}
			blob := d.IndexSnapshot()
			if blob == nil {
				t.Fatal("no snapshot after a scan")
			}
			if again := d.IndexSnapshot(); again == nil {
				t.Error("a second snapshot of an unchanged index is nil; the first should have been kept")
			}

			fresh := open(t, raw, true)
			if !fresh.RestoreIndex(blob) {
				t.Fatal("the fresh demuxer rejected its own stream's snapshot")
			}
			if fresh.IndexSnapshot() != nil {
				t.Error("a restored index snapshots again without growing")
			}
			if fresh.ScannedForTest() < scanned-mpc.CheckpointFramesForTest {
				t.Errorf("restored scan reaches %d, the original %d", fresh.ScannedForTest(), scanned)
			}
			// The restored demuxer's seek decodes exactly like the original's.
			target := end / 2
			a := seekAndDecode(t, open(t, raw, true), target)
			b := seekAndDecode(t, fresh, target)
			if len(a) != len(b) {
				t.Fatalf("%d vs %d samples", len(a), len(b))
			}
			for i := range a {
				if a[i] != b[i] {
					t.Fatalf("sample %d differs after restoring the index", i)
				}
			}

			// A blob for another stream, a stale one, and garbage are refused.
			other := open(t, fixture(t, "seek.mpc"), true)
			if other.RestoreIndex(blob) {
				t.Error("a snapshot of another stream was accepted")
			}
			stale := append([]byte(nil), blob...)
			stale[len(stale)-1] ^= 0xFF
			if open(t, raw, true).RestoreIndex(stale) {
				t.Error("a corrupted snapshot was accepted")
			}
			if open(t, raw, true).RestoreIndex([]byte("WXMPCIDX1\x00garbage")) {
				t.Error("garbage was accepted")
			}
			// Restoring over a scan already begun is refused.
			progressed := open(t, raw, true)
			if _, err := progressed.SeekSample(0, end/2); err != nil {
				t.Fatal(err)
			}
			if progressed.ScannedForTest() > 0 && progressed.RestoreIndex(blob) {
				t.Error("a snapshot was accepted over a scan in progress")
			}
		})
	}
}

// TestNoIndexWithoutNoiseSubstitution pins what an SV8 stream declaring PNS
// off has to index: nothing, since the generator is never drawn from.
func TestNoIndexWithoutNoiseSubstitution(t *testing.T) {
	defer mpc.SetIdxMinStatesForTest(1)()
	d := open(t, fixture(t, "seek.mpc"), true)
	track := d.Tracks()[0]
	if _, err := d.SeekSample(0, track.Delay+track.Samples-1); err != nil {
		t.Fatal(err)
	}
	if d.ScannedForTest() != 0 {
		t.Errorf("a stream with noise substitution off scanned %d blocks", d.ScannedForTest())
	}
	if d.IndexSnapshot() != nil {
		t.Error("a stream with nothing to scan produced a snapshot")
	}
}

func seekAndDecode(t testing.TB, d *mpc.Demuxer, target int64) []float32 {
	t.Helper()
	if _, err := d.SeekSample(0, target); err != nil {
		t.Fatal(err)
	}
	return decodeAll(t, d)
}

var _ container.Indexer = (*mpc.Demuxer)(nil)
