package mp4

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// fragmentRun is nfrag one-packet fragments of 960 samples each, with a tfdt.
func fragmentRun(nfrag int) []byte {
	bases := make([]uint64, nfrag)
	for i := range bases {
		bases[i] = uint64(i) * 960
	}
	return fragmentFile(bases, []uint32{960}, []uint32{100}, 100)
}

// TestFragmentedWalkStopsWithItsContext is container.Walker's cancel contract
// on the moof scan (testutil.WalkPauses), plus the index it leaves: every
// fragment once.
func TestFragmentedWalkStopsWithItsContext(t *testing.T) {
	const nfrag = 64
	file := fragmentRun(nfrag)
	var d *Demuxer
	testutil.WalkPauses(t, file, 5, func(src container.Source) (testutil.Walkable, error) {
		d = craftFragmentsOn(false, src)
		return d, nil
	})
	if n := len(d.fragIndex); n != nfrag {
		t.Errorf("the index holds %d fragments after the resume, want each of the %d once", n, nfrag)
	}
	if got := d.Tracks()[0].Samples; got != nfrag*960 {
		t.Errorf("settled %d samples, want %d", got, nfrag*960)
	}
}

// TestFragmentedWalkPausedBeforeTheFirstMoofReadsNothing: a context that has
// ended before the scan costs no read.
func TestFragmentedWalkPausedBeforeTheFirstMoofReadsNothing(t *testing.T) {
	src := &testutil.CountingSource{Src: container.BytesSource(fragmentRun(8))}
	d := craftFragmentsOn(false, src)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.Walk(ctx); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Walk = %v, want canceled", err)
	}
	if src.Reads != 0 {
		t.Errorf("a walk whose context had ended read %d times", src.Reads)
	}
}

// TestAToleratedStopEndsTheScan: a scan that ended on damage the open
// tolerated is over, so the Walk after a seek built the index reads nothing,
// warns nothing more and has nothing left for an ended context to stop.
func TestAToleratedStopEndsTheScan(t *testing.T) {
	file := corruptSecondMoofSize(fragmentRun(8))
	src := &testutil.CountingSource{Src: container.BytesSource(file)}
	d := craftFragmentsOn(false, src)
	if err := d.ensureFragmentIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(d.Warnings()); n != 1 {
		t.Fatalf("the seek's scan left %d warnings, want the one stop: %v", n, d.Warnings())
	}
	reads := src.Reads
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.Walk(ctx); err != nil {
		t.Fatalf("Walk after the seek's scan = %v, want nil: the scan was over", err)
	}
	if !d.Walked() {
		t.Error("not Walked after the walk settled")
	}
	if n := len(d.Warnings()); n != 1 {
		t.Errorf("the Walk after the scan left %d warnings, want the one stop still: %v", n, d.Warnings())
	}
	if src.Reads != reads {
		t.Errorf("the Walk after the scan read %d times; the scan had ended", src.Reads-reads)
	}
}

// TestFragmentedWalkResumesWithoutTfdt: fragments that carry no tfdt are
// timed by the running tick sum, and a paused scan resumes with that sum and
// the raw run intact, at a media timescale the samples are rescaled from.
func TestFragmentedWalkResumesWithoutTfdt(t *testing.T) {
	const nfrag, ticks = 48, 1800 // 960 samples at 48 kHz, in 90 kHz ticks
	var file []byte
	for i := range nfrag {
		frag := fragmentFile([]uint64{uint64(i) * ticks}, []uint32{ticks}, []uint32{100}, 100)
		file = append(file, stripTfdt(frag)...)
	}
	var d *Demuxer
	testutil.WalkPauses(t, file, 5, func(src container.Source) (testutil.Walkable, error) {
		d = craftFragmentsAt(false, src, 90000)
		return d, nil
	})
	if got := d.Tracks()[0].Samples; got != nfrag*960 {
		t.Errorf("settled %d samples, want %d: the ticks were not rescaled", got, nfrag*960)
	}
}

// TestAFragmentedSeekStopsWithItsContext: the moof scan a seek runs ends with
// the seek's context, and a seek with a live one then lands where a fresh
// demuxer lands.
func TestAFragmentedSeekStopsWithItsContext(t *testing.T) {
	file := fragmentRun(64)
	const target = 30 * 960
	want, err := craftFragmentsFrom(false, file).SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	d := craftFragmentsFrom(false, file)
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 3), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose context ended returned %v, want canceled", err)
	}
	if d.fragIndexed {
		t.Error("the scan ran to its end after its context ended")
	}
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the cancel landed at %d (%v), a fresh demuxer at %d", got, err, want)
	}
}

// TestAScanPastTheIndexCapStopsWithItsContext: a seek into the fragments the
// index could not record scans forward moof by moof, and that scan ends with
// the seek's context too.
func TestAScanPastTheIndexCapStopsWithItsContext(t *testing.T) {
	saved := maxFragments
	maxFragments = 4
	defer func() { maxFragments = saved }()
	const nfrag = 64
	file := fragmentRun(nfrag)
	d := craftFragmentsFrom(false, file)
	if err := d.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(d.fragIndex) != 4 {
		t.Fatalf("the index holds %d fragments, want the cap of 4", len(d.fragIndex))
	}
	const target = 60 * 960
	want, err := d.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 2), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose context ended returned %v, want canceled", err)
	}
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the cancel landed at %d (%v), want %d", got, err, want)
	}
}
