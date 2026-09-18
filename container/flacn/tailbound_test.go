package flacn

// The bound on the cheap tail confirmation, and what happens when it is hit.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// fixtureInternal loads a committed testdata file; demux_test.go's fixture is
// in the external test package and not reachable from here.
func fixtureInternal(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestTailConfirmationGivesUpAndStillMeasures is why the bound is safe to have.
//
// Every candidate consistent lets through costs a frame checksum over the rest
// of the window, and a file chooses both the headers it plants and the
// STREAMINFO they are checked against, so nothing in the data limits how many
// there are. maxTailCRCs stops that; giving up is only safe because the exact
// walk behind it answers the same question, and this is the cell that says so.
//
// The fixture is an ordinary FLAC with the cap turned down to zero, which is
// the same state a crafted tail puts the scan in, without needing to craft one.
func TestTailConfirmationGivesUpAndStillMeasures(t *testing.T) {
	raw := fixtureInternal(t, "sine-s16.flac")

	confirmed, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := confirmed.Tracks()[0]
	if !want.SamplesExact || want.Samples <= 0 {
		t.Fatalf("the tail confirmation did not settle the fixture: %+v", want)
	}

	defer func(n int) { tailCRCBudget = n }(tailCRCBudget)
	tailCRCBudget = 0
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.tailBacks(); ok {
		t.Fatal("the confirmation answered with no budget to check anything")
	}
	got := d.Tracks()[0]
	if got.Samples != want.Samples || !got.SamplesExact {
		t.Errorf("without the tail confirmation: %d samples (exact %v), want %d exact from the walk",
			got.Samples, got.SamplesExact, want.Samples)
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("falling through to the walk warned about an intact file: %v", d.Warnings())
	}
}
