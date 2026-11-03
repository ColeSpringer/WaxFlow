package mka

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// faultingSource serves reads until from, then fails every read of at most
// small bytes: the element headers, while the block bodies still arrive.
type faultingSource struct {
	src   container.Source
	from  int64
	small int
}

func (s *faultingSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.from && len(p) <= s.small {
		return 0, errors.New("disk fault")
	}
	return s.src.ReadAt(p, off)
}

func (s *faultingSource) Size() int64 { return s.src.Size() }

// TestAReadFaultIsNotDamage: a source that fails an element header read is
// unreadable, not a damaged file that ends there. The walk fails and settles
// nothing, and a read fails rather than ending early.
func TestAReadFaultIsNotDamage(t *testing.T) {
	p := BuildMidPadding(t)
	src := &faultingSource{src: container.BytesSource(p.File), from: int64(len(p.File)) / 2, small: 12}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(t.Context()); !errors.Is(err, waxerr.ErrSourceUnreadable) {
		t.Fatalf("Walk over a faulting source = %v, want source-unreadable", err)
	}
	if d.Walked() {
		t.Error("a walk stopped by a read fault reports Walked")
	}
	if tr := d.Tracks()[0]; tr.SamplesExact {
		t.Errorf("a walk stopped by a read fault settled %d samples as exact", tr.Samples)
	}

	rd, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for {
		err := rd.ReadPacket(&pkt)
		if err == io.EOF {
			t.Fatal("a read over a faulting source ended with a clean EOF")
		}
		if err != nil {
			if !errors.Is(err, waxerr.ErrSourceUnreadable) {
				t.Fatalf("the read failed with %v, want source-unreadable", err)
			}
			break
		}
	}
}

// otherTrack returns p's file with every block moved to track 5, so the
// selected track has no blocks and a walk skips every element it meets.
func otherTrack(t *testing.T, p MidPadding) []byte {
	t.Helper()
	offs := simpleBlockOffsets(t, p.File)
	file := append([]byte(nil), p.File...)
	for _, off := range offs {
		if file[off] != 0x81 {
			t.Fatalf("block at %d carries track vint %#x, want 0x81", off, file[off])
		}
		file[off] = 0x85
	}
	return file
}

// TestWalkStopsBetweenOtherTracksBlocks: the context is read per element, not
// per selected-track block, so a walk over a stream whose blocks all belong to
// another track stops within an element of the cancel.
func TestWalkStopsBetweenOtherTracksBlocks(t *testing.T) {
	file := otherTrack(t, BuildMidPadding(t))
	src := &testutil.CountingSource{Src: container.BytesSource(file)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := src.Reads
	if err := d.Walk(testutil.CancelAfterChecks(t, 3)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Walk = %v, want canceled", err)
	}
	if n := src.Reads - opened; n > 6 {
		t.Errorf("a walk canceled on its third check read %d times, want two elements' worth at most", n)
	}
	if err := d.Walk(t.Context()); err != nil || !d.Walked() {
		t.Fatalf("Walk after the cancel = %v, Walked %v", err, d.Walked())
	}
}

// TestSeeksAgreeAfterAFailedFullWalk: a full walk the file's own damage
// stopped is kept. Under Strict every seek refuses with it, the one that ran
// the walk included; tolerant, every seek lands on what the walk indexed. Two
// identical seeks answer alike either way, and Walk reports the failure.
func TestSeeksAgreeAfterAFailedFullWalk(t *testing.T) {
	p := BuildMidPadding(t)
	file := damageBlocks(t, p.File, 250)
	target := p.PTS(400)

	strict, err := NewDemuxer(container.BytesSource(file), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err1 := strict.SeekSample(t.Context(), 0, target)
	_, err2 := strict.SeekSample(t.Context(), 0, target)
	if !errors.Is(err1, waxerr.ErrMalformedInput) || err2 == nil || err2.Error() != err1.Error() {
		t.Errorf("two strict seeks to %d over a refused walk gave %v and %v, want the refusal twice", target, err1, err2)
	}
	if err := strict.Walk(t.Context()); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("Walk after the refusing seeks = %v, want the refusal", err)
	}

	src := &testutil.CountingSource{Src: container.BytesSource(file)}
	tolerant, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err1 := tolerant.SeekSample(t.Context(), 0, target)
	reads := src.Reads
	second, err2 := tolerant.SeekSample(t.Context(), 0, target)
	if err1 != nil || err2 != nil || first != second {
		t.Errorf("two tolerant seeks to %d gave (%d, %v) and (%d, %v)", target, first, err1, second, err2)
	}
	if src.Reads-reads > 4 {
		t.Errorf("the second tolerant seek read %d times; the walk finished once", src.Reads-reads)
	}
}

// flakyHeaders fails every element header read while armed, which fails the
// walk as source-unreadable; disarmed, every read serves.
type flakyHeaders struct {
	src   container.Source
	armed bool
}

func (s *flakyHeaders) ReadAt(p []byte, off int64) (int, error) {
	if s.armed && len(p) <= 12 {
		return 0, errors.New("disk fault")
	}
	return s.src.ReadAt(p, off)
}

func (s *flakyHeaders) Size() int64 { return s.src.Size() }

// TestATransientFaultDoesNotLatchTheWalk: a full walk a read fault stopped is
// tried again by the next seek or Walk, which succeed once the source serves;
// only the file's own faults are kept.
func TestATransientFaultDoesNotLatchTheWalk(t *testing.T) {
	p := BuildMidPadding(t)
	fresh, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := p.PTS(400)
	want, err := fresh.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	src := &flakyHeaders{src: container.BytesSource(p.File)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.armed = true
	if _, err := d.SeekSample(t.Context(), 0, target); err != nil {
		t.Fatalf("a tolerant seek over a faulting source failed: %v; it lands on what was indexed", err)
	}
	if d.walked {
		t.Fatal("the walk finished through a faulting source")
	}
	src.armed = false
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the source recovered landed at %d (%v), a fresh demuxer at %d: the walk was not tried again", got, err, want)
	}
	if !d.walked {
		t.Error("the walk after the source recovered did not finish")
	}
}

// TestADeadContextStopsNoSeekThatWalksNothing: a seek to 0, which lands on
// the first cluster without a walk, lands whatever its context says.
func TestADeadContextStopsNoSeekThatWalksNothing(t *testing.T) {
	p := BuildMidPadding(t)
	d, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if landed, err := d.SeekSample(ctx, 0, 0); err != nil || landed != 0 {
		t.Errorf("a seek to 0 under an ended context = %d, %v; want 0 and nil, since nothing walks", landed, err)
	}
}
