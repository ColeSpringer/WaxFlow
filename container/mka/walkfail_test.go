package mka

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// simpleBlockOffsets lists the data offset of every block in a file this
// package's muxer wrote, SimpleBlock or Block in a group, in stream order, so
// a test can damage a chosen one.
func simpleBlockOffsets(t *testing.T, file []byte) []int64 {
	t.Helper()
	var offs []int64
	var walk func(off, end int64)
	walk = func(off, end int64) {
		for off < end {
			id, idLen, _, ok := parseVint(file[off:end], true)
			if !ok {
				t.Fatalf("no element ID at %d", off)
			}
			size, sizeLen, unknown, ok := parseVint(file[off+int64(idLen):end], false)
			if !ok {
				t.Fatalf("no element size at %d", off)
			}
			dataOff := off + int64(idLen+sizeLen)
			dataEnd := end
			if !unknown {
				dataEnd = dataOff + int64(size)
			}
			switch uint32(id) {
			case idSegment, idCluster, idBlockGroup:
				walk(dataOff, dataEnd)
			case idSimpleBlock, idBlock:
				offs = append(offs, dataOff)
			}
			off = dataEnd
		}
	}
	walk(0, int64(len(file)))
	if len(offs) < 500 {
		t.Fatalf("found %d blocks, want the fixture's 500", len(offs))
	}
	return offs
}

// damageBlocks returns a copy of file with each named block's track number
// made an invalid vint, which parseBlock refuses: tolerated as "damaged
// block, skipped", refused under Strict.
func damageBlocks(t *testing.T, file []byte, blocks ...int) []byte {
	t.Helper()
	offs := simpleBlockOffsets(t, file)
	out := slices.Clone(file)
	for _, b := range blocks {
		out[offs[b]] = 0
	}
	return out
}

// TestAFailedWalkKeepsItsError: a full walk the file's damage stopped is
// reported again by the next Walk, at no cost and without walking again,
// Walked stays false, and a strict seek refuses with it.
func TestAFailedWalkKeepsItsError(t *testing.T) {
	p := BuildMidPadding(t)
	file := damageBlocks(t, p.File, 250)
	src := &testutil.CountingSource{Src: container.BytesSource(file)}
	d, err := NewDemuxer(src, &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	first := d.Walk(t.Context())
	if !errors.Is(first, waxerr.ErrMalformedInput) {
		t.Fatalf("a strict walk over a damaged block returned %v, want malformed", first)
	}
	if d.Walked() {
		t.Error("Walked after a walk that refused")
	}
	reads := src.Reads
	again := d.Walk(t.Context())
	if again == nil || again.Error() != first.Error() {
		t.Errorf("the second Walk returned %v, want the first one's %v again", again, first)
	}
	if d.Walked() {
		t.Error("Walked after the second refused walk")
	}
	if src.Reads != reads {
		t.Errorf("the second Walk read %d times; a failed walk is not walked again", src.Reads-reads)
	}
	if tr := d.Tracks()[0]; tr.SamplesExact {
		t.Errorf("a refused walk settled the length at %d", tr.Samples)
	}
	if _, err := d.SeekSample(context.Background(), 0, p.PTS(100)); err == nil || err.Error() != first.Error() {
		t.Errorf("a strict seek after the refused walk = %v, want the refusal again", err)
	}
}

// TestWarningsAreCappedDedupedAndCounted: the list is capped, the damage
// counted past the cap, and a block passed twice (read, then re-read after a
// seek back) listed once.
func TestWarningsAreCappedDedupedAndCounted(t *testing.T) {
	p := BuildMidPadding(t)

	t.Run("capped and counted", func(t *testing.T) {
		const damaged = 100
		blocks := make([]int, damaged)
		for i := range blocks {
			blocks[i] = 2 + 3*i
		}
		d, err := NewDemuxer(container.BytesSource(damageBlocks(t, p.File, blocks...)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Walk(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n, _ := countWarnings(d.Warnings()); n != container.MaxWarnings {
			t.Errorf("a walk over %d damaged blocks listed %d of them, want the cap of %d", damaged, n, container.MaxWarnings)
		}
		if got := container.DamageOf(d); got != damaged {
			t.Errorf("DamageOf = %d after %d damaged blocks", got, damaged)
		}
	})

	t.Run("a block passed twice is reported once", func(t *testing.T) {
		d, err := NewDemuxer(container.BytesSource(damageBlocks(t, p.File, 5)), nil)
		if err != nil {
			t.Fatal(err)
		}
		var pkt container.Packet
		for range 10 {
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
			t.Fatal(err)
		}
		for range 10 {
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(d.Warnings()); n != 1 {
			t.Errorf("one damaged block read twice was listed %d times, want once: %v", n, d.Warnings())
		}
		// Met twice, by the read and the read again: a seek to 0 walks
		// nothing, Cues or not.
		if got := container.DamageOf(d); got != 2 {
			t.Errorf("DamageOf = %d after meeting the damage twice", got)
		}
	})
}
