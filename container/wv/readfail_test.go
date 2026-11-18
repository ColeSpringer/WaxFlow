package wv_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAReadThatFailsInAScanIsNeverAnAnswer: a byte that cannot be read inside
// a scan's reach is the read's error, in a read's resync, in the open's tail
// scan, and in a seek's or a measuring open's bisection; never a block found,
// a length settled, or damage reported from bytes that did not arrive. Each
// row is held to the read its hole fails in.
func TestAReadThatFailsInAScanIsNeverAnAnswer(t *testing.T) {
	hole := func(raw []byte, at int64) *testutil.FailingSource {
		return &testutil.FailingSource{Src: container.BytesSource(raw), From: at, To: at + 1}
	}
	unreadable := func(err error) bool { return waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable }

	// A read's resync: junk behind the first block, dense with magics. The
	// first block is read with a window, so the scan's first read is a Chunk
	// in. A span holding the block behind the junk starts no later than its
	// view; the confirm reads from the window's end, past that view.
	s, at := resyncScan(t)
	// Where the scan's first read ends, from a source that reads.
	firstEnd := int64(0)
	{
		src := &testutil.CountingSource{Src: container.BytesSource(s.File(s.Junk(200<<10, 100))), Log: true}
		d, err := wv.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		for _, r := range src.Spans {
			if r[0] == srcwin.Chunk {
				firstEnd = r[1]
			}
		}
		if firstEnd == 0 {
			t.Fatalf("the resync read %v, with none from %d: the scan's first read moved", src.Spans, srcwin.Chunk)
		}
	}
	for _, tc := range []struct {
		name    string
		junk    int
		cut     func(c, block int64) int64 // c is where the block behind the junk starts
		confirm bool
	}{
		{"the scan's first read", 200 << 10, func(int64, int64) int64 { return srcwin.Chunk }, false},
		{"a byte into the scan's first read", 200 << 10, func(int64, int64) int64 { return srcwin.Chunk + 1 }, false},
		{"the scan's first read's last byte", 200 << 10, func(int64, int64) int64 { return firstEnd - 1 }, false},
		{"the scan's second read's first byte", 200 << 10, func(int64, int64) int64 { return firstEnd }, false},
		{"the junk's last byte", 200 << 10, func(c, _ int64) int64 { return c - 1 }, false},
		{"the block behind it, in the span that offers it", 200 << 10, func(c, _ int64) int64 { return c }, false},
		{"its header's last byte", 200 << 10, func(c, _ int64) int64 { return c + 31 }, false},
		{"the header behind it, in the same span", 200 << 10, func(c, block int64) int64 { return c + block + 31 }, false},
		{"the header behind it, past the span", 250 << 10, func(c, block int64) int64 { return c + block + 31 }, true},
	} {
		file := s.File(s.Junk(tc.junk, 100))
		c := int64(at + tc.junk)
		block := int64(blockStarts(t, file[c:])[1])
		cut := tc.cut(c, block)
		for _, strict := range []bool{false, true} {
			name := fmt.Sprintf("the resync, %s, no read at %d, strict %v", tc.name, cut, strict)
			src := hole(file, cut)
			d, err := wv.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if err != nil || src.Failed != 0 {
				t.Fatalf("%s: the open returned %v after %d failed reads, want a stream that opens", name, err, src.Failed)
			}
			var pkt container.Packet
			err = d.ReadPacket(&pkt)
			if src.Failed == 0 {
				t.Fatalf("%s: the read read nothing of the byte that cannot be read: this holds nothing", name)
			}
			if inConfirm := src.FailedAt[0] >= c+wavpack.BlockHeaderLen; inConfirm != tc.confirm {
				t.Errorf("%s: the first failed read is %v, want it the confirm's %v", name, src.FailedAt, tc.confirm)
				continue
			}
			if !unreadable(err) || len(d.Warnings()) != 0 {
				t.Errorf("%s: the read returned %v with warnings %v, want the read's error and none", name, err, d.Warnings())
			}
		}
	}

	// The open's tail scan, on a stream whose length the header never learned
	// and whose tail is junk, so the scan steps back over the whole stream, a
	// Chunk at a time from the end.
	raw := noiseFile(t, muxFormat, 1<<19)
	tail := append(append([]byte(nil), raw...), bytes.Repeat([]byte{0x11}, 300<<10)...)
	binary.LittleEndian.PutUint32(tail[12:], 0xffffffff)
	end := int64(len(tail))
	for _, cut := range []int64{end - srcwin.Chunk - 1, end - 2*srcwin.Chunk - 100, end - 9*srcwin.Chunk, int64(blockStarts(t, raw)[1])} {
		step := max(end-((end-cut-1)/srcwin.Chunk+1)*srcwin.Chunk, 0)
		for _, strict := range []bool{false, true} {
			src := hole(tail, cut)
			_, err := wv.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if src.Failed == 0 || src.FailedAt[0] != step || !unreadable(err) {
				t.Errorf("the tail scan, no read at %d, strict %v: the open returned %v after %d failed reads, the first %v, want the read's error from the step at %d",
					cut, strict, err, src.Failed, src.FailedAt, step)
			}
		}
	}

	// A seek's bisection, to a target in the first block, which a probe that
	// failed would land on: a hole in its first probe's window, and on a stream
	// with junk around that probe, a hole its scan reads behind the window.
	const hollowJunk = 3 * srcwin.Chunk
	hollowMid := int64(len(raw)+hollowJunk) / 2
	starts := blockStarts(t, raw)
	k := len(starts) - 1
	for int64(starts[k]) > hollowMid-1000 {
		k--
	}
	hollow := spliced(raw, starts[k], bytes.Repeat([]byte{0x11}, hollowJunk))
	if int64(starts[k]+hollowJunk) < hollowMid+srcwin.Chunk+200 {
		t.Fatalf("the junk at %d ends short of the first probe's window at %d: this holds nothing", starts[k], hollowMid)
	}
	for _, tc := range []struct {
		name      string
		raw       []byte
		cut, from int64 // from is where the first failed read starts
	}{
		{"the first probe's window", raw, int64(len(raw)) / 2, int64(len(raw)) / 2},
		{"the first probe's scan behind its window", hollow, hollowMid + srcwin.Chunk + 100, hollowMid + srcwin.Chunk},
	} {
		for _, target := range []int64{100, 1 << 18} {
			name := fmt.Sprintf("a seek to %d, %s, no read at %d", target, tc.name, tc.cut)
			src := hole(tc.raw, tc.cut)
			d, err := wv.NewDemuxer(src, nil)
			if err != nil || src.Failed != 0 {
				t.Fatalf("%s: the open returned %v after %d failed reads", name, err, src.Failed)
			}
			landed, err := d.SeekSample(context.Background(), 0, target)
			if src.Failed == 0 {
				t.Fatalf("%s: the seek read nothing of the byte that cannot be read: this holds nothing", name)
			}
			if src.FailedAt[0] != tc.from {
				t.Errorf("%s: the first failed read is %v, want the one from %d", name, src.FailedAt, tc.from)
				continue
			}
			if !unreadable(err) || len(d.Warnings()) != 0 {
				t.Errorf("%s: landed at %d with %v and warnings %v, want the read's error and none", name, landed, err, d.Warnings())
			}
		}
	}

	// An open that measures: a stream longer than the tail scan's range with
	// its tail cut inside the last block, so the scan finds no tiling and the
	// open bisects, its first probe's window where the byte is gone, under
	// the tail scan's floor.
	long := noiseFile(t, muxFormat, 1<<21)
	cut := long[:len(long)-100]
	probe := int64(len(cut)) / 2
	if probe >= int64(len(cut))-wv.MaxTailScanForTest {
		t.Fatalf("the first probe at %d is inside the tail scan's range: this holds nothing", probe)
	}
	for _, at := range []int64{probe, probe + 10} {
		for _, strict := range []bool{false, true} {
			src := hole(cut, at)
			_, err := wv.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if src.Failed == 0 || src.FailedAt[0] != probe || !unreadable(err) {
				t.Errorf("a measuring open, no read at %d, strict %v: the open returned %v after %d failed reads, the first %v, want the read's error from the probe's window at %d",
					at, strict, err, src.Failed, src.FailedAt, probe)
			}
		}
	}
}

// TestAFailedReadIsTheErrorOfItsCall: the open, read or seek that met a
// failed read returns its error, with no warning a source that reads does not
// give. A read that only asks whether an index holds is no answer instead.
func TestAFailedReadIsTheErrorOfItsCall(t *testing.T) {
	raw := noiseFile(t, muxFormat, 1<<18)
	starts := blockStarts(t, raw)
	s, _ := resyncScan(t)
	bare := append([]byte(nil), raw...)
	if err := wavpack.SetTotalSamples(bare[:starts[1]], -1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		raw     []byte
		damaged bool
		asks    bool // a source that reads is asked whether an index holds
	}{
		{"a clean stream", raw, false, false},
		{"junk behind the first block", spliced(raw, starts[1], s.Junk(50<<10, 100)), true, false},
		{"headers behind the first block and a tail cut short", spliced(raw, starts[1], plantedHeaders(raw, 20<<10, 2000))[:len(raw)+20<<10-100], true, false},
		{"no total, so the open checks the indexes at its ends", bare, false, true},
		{"a block gone behind the second", without(t, raw, 2), true, true},
	} {
		// run opens src, reads two packets, and seeks twice with a read behind
		// each, the second into the first block, where a bisection that failed
		// lands. It stops at the call that returned a failed read's error,
		// names its kind, and says what any call got wrong. asked counts the
		// calls that went on past a failed read.
		run := func(src container.Source, failed func() int, whole []container.Warning) (warnings []container.Warning, met, wrong string, asked int) {
			d, err := wv.NewDemuxer(src, nil)
			if err != nil {
				if failed() == 0 || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					wrong = fmt.Sprintf("the open returned %v after %d failed reads", err, failed())
				}
				return nil, "open", wrong, 0
			}
			if failed() > 0 {
				// Only a check of the length fails without failing the open.
				if d.Tracks()[0].SamplesExact || len(d.Warnings()) > 0 {
					return nil, "open", fmt.Sprintf("the open returned nil after %d failed reads, with %+v and warnings %v",
						failed(), d.Tracks()[0], d.Warnings()), 0
				}
				asked++
			}
			var pkt container.Packet
			read := func() error { return d.ReadPacket(&pkt) }
			seek := func(target int64) func() error {
				return func() error { _, err := d.SeekSample(context.Background(), 0, target); return err }
			}
			for i, call := range []func() error{read, read, seek(150000), read, seek(100), read} {
				before, held := failed(), len(d.Warnings())
				err := call()
				if failed() == before {
					continue
				}
				kind := []string{"read", "read", "seek", "read", "seek", "read"}[i]
				added := d.Warnings()[held:]
				if err == nil {
					// Only a walk ahead of a skip fails without failing its
					// call, and the skip is then read on in order.
					if len(added) != 1 || !strings.Contains(added[0].Msg, "which could not be checked; read on in order") || pkt.Lost != 0 {
						return d.Warnings(), kind, fmt.Sprintf("call %d returned nil after a failed read, warned %v and read a packet owing %d", i, added, pkt.Lost), asked
					}
					asked++
					continue
				}
				met = kind
				if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					wrong = fmt.Sprintf("call %d returned %v", i, err)
				}
				for _, w := range added {
					if !slices.Contains(whole, w) {
						wrong = fmt.Sprintf("call %d returned %v and warned %v, which a source that reads does not", i, err, w)
					}
				}
				break
			}
			return d.Warnings(), met, wrong, asked
		}
		counted := &testutil.CountingSource{Src: container.BytesSource(tc.raw)}
		whole, _, _, _ := run(counted, func() int { return 0 }, nil)
		calls, asked := map[string]int{}, 0
		for k := 1; k < counted.Reads; k++ {
			src := &testutil.FailingSource{Src: container.BytesSource(tc.raw), From: int64(len(tc.raw)), After: k}
			_, met, wrong, n := run(src, func() int { return src.Failed }, whole)
			if wrong != "" {
				t.Errorf("%s, every read behind the first %d failing, the first of them %v: %s", tc.name, k, src.FailedAt, wrong)
			}
			calls[met]++
			asked += n
		}
		// The sweep holds something only where each kind of call met a failed
		// read, where the damaged streams warn on a source that reads, and
		// where a read that only asks failed in the streams that are asked.
		if calls["open"] == 0 || calls["read"] == 0 || calls["seek"] == 0 || tc.damaged != (len(whole) > 0) || tc.asks != (asked > 0) {
			t.Errorf("%s: failed reads met by %v, %d calls that went on past one, and warnings %v on a source that reads: this holds nothing",
				tc.name, calls, asked, whole)
		}
	}
}
