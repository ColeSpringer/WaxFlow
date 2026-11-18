package ogg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// readRest reads d to its end, or to its first error, and returns the
// packets as text and how the read ended.
func readRest(d *Demuxer) ([]string, error) {
	var packets []string
	var pkt container.Packet
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			return packets, err
		}
		packets = append(packets, fmt.Sprintf("%d+%d %08x", pkt.PTS, pkt.Dur, crc32(0, pkt.Data)))
	}
}

// holed is raw with the byte at off unreadable.
func holed(raw []byte, off int64) *testutil.FailingSource {
	return &testutil.FailingSource{Src: container.BytesSource(raw), From: off, To: off + 1}
}

// subset says whether every warning in ws is one of whole.
func subset(ws, whole []container.Warning) bool {
	for _, w := range ws {
		if !slices.Contains(whole, w) {
			return false
		}
	}
	return true
}

// TestAReadThatFailsInAScanIsNeverAnAnswer: a byte that cannot be read where
// a resync reaches is the error of the read that met it, in its words, in a
// read's resync over junk and in the open's tail walk; never a read that
// ends, a length settled, or damage reported from bytes that did not arrive.
func TestAReadThatFailsInAScanIsNeverAnAnswer(t *testing.T) {
	const (
		header = "ogg: reading page header: source is down"
		body   = "ogg: reading page body: source is down"
		scan   = "ogg: scanning for page: source is down"
	)
	raw, _, _ := buildSyntheticOpus(312, 4, 600, 200)
	starts := pageStarts(t, raw)

	// A read's resync: 100 KiB of capture patterns, then 100 heads that
	// parse, in front of page 100. The heads claim 65 KiB bodies, which
	// reach past the page behind the junk and the page after it.
	at := starts[100]
	junk := append(bytes.Repeat([]byte("OggS"), 25<<10), plantedHeads(100<<10, 100)...)
	file := testutil.Splice(raw, at, junk)
	behind := int64(at + len(junk))
	lacingAt, size := pageAt(t, file, int(behind))
	whole, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := readRest(whole)
	// What every read delivers in front of the damage: four packets an audio
	// page. A strict read refuses the damage, whose bytes it read, before any
	// resync; a tolerant one warns of it first.
	before := 4 * (100 - 2)
	refused := fmt.Sprintf("ogg: damaged page, resyncing (at offset %d)", at)
	damage := []container.Warning{{Offset: int64(at), Kind: container.Damage, Msg: "damaged page, resyncing"}}
	for _, tc := range []struct {
		name   string
		hole   int64
		read   string // the tolerant read's error
		strict string // the strict read's
	}{
		{"the damaged page's header", int64(at), header, header},
		{"the scan's first read", int64(at) + 100, scan, refused},
		{"a candidate's header across the first read's end", int64(at) + 1 + scanStep + 10, header, refused},
		{"a scan read over the planted heads", int64(at) + 100<<10 + 2000, scan, refused},
		{"a head's claimed body: the junk's last byte", behind - 1, body, refused},
		{"a head's claimed body: the header of the page behind", behind + 5, body, refused},
		{"a head's claimed body: its lacing table", behind + headerLen, body, refused},
		{"a head's claimed body: its body", behind + int64(lacingAt) + 10, body, refused},
		{"a head's claimed body: the page after it", behind + int64(size) + 3, body, refused},
	} {
		for _, strict := range []bool{false, true} {
			name := fmt.Sprintf("a read, no read at %s (%d), strict %v", tc.name, tc.hole, strict)
			src := holed(file, tc.hole)
			d, err := NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if err != nil || src.Failed != 0 {
				t.Fatalf("%s: the open returned %v after %d failed reads, want a stream that opens", name, err, src.Failed)
			}
			packets, err := readRest(d)
			want, warned := tc.read, damage
			if strict || tc.hole < int64(at)+headerLen {
				warned = nil // refused, or the damage itself unread
			}
			if strict {
				want = tc.strict
			}
			if err == nil || err.Error() != want || !slices.Equal(packets, all[:before]) || !slices.Equal(d.Warnings(), warned) {
				t.Errorf("%s: %d packets, then %v, warnings %v; want %d, then %q, warnings %v",
					name, len(packets), err, d.Warnings(), before, want, warned)
			}
			// Once the source reads, a tolerant read goes on to what a
			// source that reads gives, and a strict one refuses the damage.
			src.From = int64(len(file))
			rest, err := readRest(d)
			switch {
			case strict && (err == nil || err.Error() != refused || len(rest) != 0):
				t.Errorf("%s, then a source that reads: %d packets, then %v; want %q", name, len(rest), err, refused)
			case !strict && (err != io.EOF || !slices.Equal(append(packets, rest...), all)):
				t.Errorf("%s, then a source that reads: %d packets, then %v; want the %d a source that reads gives",
					name, len(packets)+len(rest), err, len(all))
			}
		}
	}

	// The open's tail walk: junk in front of the second-last page, longer
	// than the tail window, so the walk starts in it and resyncs.
	last := starts[len(starts)-3]
	tail := testutil.Splice(raw, last, junk)
	behind = int64(last + len(junk))
	from := int64(len(tail)) - (maxPageSize + scanStep)
	if from <= int64(last) {
		t.Fatalf("the tail window from %d starts in front of the junk at %d: this holds nothing", from, last)
	}
	for _, tc := range []struct {
		name string
		hole int64
		want string
	}{
		{"the walk's first scan read", from, scan},
		{"a head's claimed body, a step on", from + scanStep + 10, body},
		{"a head's claimed body: the junk's last byte", behind - 1, body},
		{"a head's claimed body: the page behind it", behind + 3, body},
		{"the last scan read: the file's last byte", int64(len(tail)) - 1, scan},
	} {
		for _, strict := range []bool{false, true} {
			src := holed(tail, tc.hole)
			_, err := NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if err == nil || err.Error() != tc.want || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
				t.Errorf("the tail walk, no read at %s (%d), strict %v: the open returned %v, want %q",
					tc.name, tc.hole, strict, err, tc.want)
			}
		}
	}
}

// TestAFailedReadIsTheErrorOfItsCall: a byte that cannot be read is the
// error of the call that needed it, in that call's words, however far the
// window read ahead; the page in front of it is delivered whole; and the
// failure does not stick, so once the source reads, the read goes on to the
// end a source that reads gives.
func TestAFailedReadIsTheErrorOfItsCall(t *testing.T) {
	raw, _, _ := buildSyntheticOpus(312, 4, 400, 200)
	starts := pageStarts(t, raw)
	whole, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := readRest(whole)
	const k = 150 // a page mid-stream, behind the open's reads
	lacingAt, _ := pageAt(t, raw, starts[k])
	for _, tc := range []struct {
		name string
		hole int64
		msg  string
	}{
		{"a page's header", int64(starts[k]) + 3, "ogg: reading page header: source is down"},
		{"its lacing table", int64(starts[k]) + headerLen, "ogg: reading lacing table: source is down"},
		{"its body", int64(starts[k]+lacingAt) + 5, "ogg: reading page body: source is down"},
	} {
		src := holed(raw, tc.hole)
		d, err := NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readRest(d)
		if err == nil || err.Error() != tc.msg || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
			t.Errorf("no read at %s: the read returned %v, want %q", tc.name, err, tc.msg)
		}
		// The audio pages in front of it, four packets each.
		if n := 4 * (k - 2); len(got) != n || !slices.Equal(got, want[:n]) {
			t.Errorf("no read at %s: %d packets before the error, want the %d of the pages in front of it", tc.name, len(got), n)
		}
		src.From = int64(len(raw)) // the source reads again
		rest, err := readRest(d)
		if err != io.EOF || !slices.Equal(append(got, rest...), want) {
			t.Errorf("no read at %s, then a source that reads: %d packets and %v, want the %d a source that reads gives",
				tc.name, len(got)+len(rest), err, len(want))
		}
	}

	// A resync's scan read, in its own words, and the landing once it reads.
	junk := bytes.Repeat([]byte("OggS"), 50<<10)
	file := testutil.Splice(raw, starts[k], junk)
	src := holed(file, int64(starts[k])+scanStep)
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readRest(d)
	if err == nil || err.Error() != "ogg: scanning for page: source is down" {
		t.Errorf("no read in a resync's scan: the read returned %v", err)
	}
	src.From = int64(len(file))
	rest, err := readRest(d)
	if err != io.EOF || !slices.Equal(append(got, rest...), want) {
		t.Errorf("no read in a resync's scan, then a source that reads: %d packets and %v, want %d", len(got)+len(rest), err, len(want))
	}

	// Every read failing past the first k, over an open, reads and seeks:
	// the call that met the failure returns its error, and warns nothing a
	// source that reads does not.
	junked := testutil.Splice(raw, starts[3], bytes.Repeat([]byte("OggS"), 20<<10))
	tailed := testutil.Splice(raw, starts[len(starts)-3], bytes.Repeat([]byte("OggS"), 20<<10))
	for _, tc := range []struct {
		name    string
		raw     []byte
		damaged bool // a read on a source that reads warns
	}{{"a clean stream", raw, false}, {"junk behind the first audio page", junked, true}, {"junk in the tail", tailed, false}} {
		run := func(src container.Source, failed func() int, whole []container.Warning) (warnings []container.Warning, met, wrong string) {
			d, err := NewDemuxer(src, nil)
			if err != nil {
				if failed() == 0 || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					wrong = fmt.Sprintf("the open returned %v after %d failed reads", err, failed())
				}
				return nil, "open", wrong
			}
			if failed() > 0 {
				return nil, "open", fmt.Sprintf("the open returned nil after %d failed reads", failed())
			}
			var pkt container.Packet
			read := func() error { return d.ReadPacket(&pkt) }
			seek := func(target int64) func() error {
				return func() error { _, err := d.SeekSample(context.Background(), 0, target); return err }
			}
			calls := []func() error{read, read, read, read, read, read, seek(150000), read, seek(100), read, read}
			names := []string{"read", "read", "read", "read", "read", "read", "seek", "read", "seek", "read", "read"}
			for i, call := range calls {
				before, held := failed(), len(d.Warnings())
				err := call()
				if failed() == before {
					continue
				}
				met = names[i]
				if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					wrong = fmt.Sprintf("call %d returned %v", i, err)
				}
				if !subset(d.Warnings()[held:], whole) {
					wrong = fmt.Sprintf("call %d returned %v and warned %v, which a source that reads does not", i, err, d.Warnings()[held:])
				}
				break
			}
			return d.Warnings(), met, wrong
		}
		counted := &testutil.CountingSource{Src: container.BytesSource(tc.raw)}
		wholeWarnings, _, _ := run(counted, func() int { return 0 }, nil)
		calls := map[string]int{}
		for k := 1; k < counted.Reads; k++ {
			src := &testutil.FailingSource{Src: container.BytesSource(tc.raw), From: int64(len(tc.raw)), After: k}
			_, met, wrong := run(src, func() int { return src.Failed }, wholeWarnings)
			if wrong != "" {
				t.Errorf("%s, every read behind the first %d failing, the first of them %v: %s", tc.name, k, src.FailedAt, wrong)
			}
			calls[met]++
		}
		if calls["open"] == 0 || calls["read"] == 0 || calls["seek"] == 0 || tc.damaged != (len(wholeWarnings) > 0) {
			t.Errorf("%s: failed reads met by %v and warnings %v on a source that reads: this holds nothing", tc.name, calls, wholeWarnings)
		}
	}
}

// TestAReadClosesInOnAnUnreadableByte: a read toward a byte it cannot read
// fails at most twenty reads and delivers every page in front of it; a retry
// is one read, and once the source reads, reads are a readable source's.
func TestAReadClosesInOnAnUnreadableByte(t *testing.T) {
	most := []int{31, 38, 26} // reads to the error, by stream
	for i, s := range costStreams(t) {
		// What a seek to 0 and a read to the end cost, read whole first.
		cs := &testutil.CountingSource{Src: container.BytesSource(s.raw)}
		readable, err := NewDemuxer(cs, nil)
		if err != nil {
			t.Fatal(err)
		}
		readRest(readable)
		cs.Reset()
		if _, err := readable.SeekSample(context.Background(), 0, 0); err != nil {
			t.Fatal(err)
		}
		readRest(readable)
		again := cs.Reads
		want, _ := readRest(func() *Demuxer {
			d, err := NewDemuxer(container.BytesSource(s.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}())
		starts := pageStarts(t, s.raw)
		at := int64(starts[len(starts)/3])
		// The packets of the audio pages in front of the one at at: one, four
		// or fifty a page.
		perPage := len(want) / (len(starts) - 3)
		before := perPage * (len(starts)/3 - 2)
		lacing := int64(s.raw[at+26])
		for _, tc := range []struct {
			name      string
			from, n   int64
			wantError string
		}{
			{"its header", at + 3, 1, "ogg: reading page header: source is down"},
			{"its lacing table", at + headerLen, 1, "ogg: reading lacing table: source is down"},
			{"its body", at + headerLen + lacing + 5, 1, "ogg: reading page body: source is down"},
			{"4 KiB from its header on", at + 3, 4096, "ogg: reading page header: source is down"},
		} {
			name := fmt.Sprintf("%s, no read at %s (%d)", s.name, tc.name, tc.from)
			fs := &testutil.FailingSource{Src: container.BytesSource(s.raw), From: tc.from, To: tc.from + tc.n}
			src := &testutil.CountingSource{Src: fs}
			d, err := NewDemuxer(src, nil)
			if err != nil || fs.Failed != 0 {
				t.Fatalf("%s: the open returned %v after %d failed reads", name, err, fs.Failed)
			}
			src.Reset()
			got, err := readRest(d)
			if err == nil || err.Error() != tc.wantError || len(got) != before || !slices.Equal(got, want[:before]) {
				t.Errorf("%s: %d packets, then %v; want the %d in front of it, then %q", name, len(got), err, before, tc.wantError)
			}
			if fs.Failed > 20 || src.Reads > most[i] {
				t.Errorf("%s: %d reads to the error, %d of them failed, want at most %d and 20", name, src.Reads, fs.Failed, most[i])
			}
			src.Reset()
			failed := fs.Failed
			if retry, err := readRest(d); len(retry) != 0 || err == nil || err.Error() != tc.wantError ||
				src.Reads != 1 || fs.Failed-failed != 1 {
				t.Errorf("%s, read again: %d packets, then %v, in %d reads, %d failed; want %q in one failed read",
					name, len(retry), err, src.Reads, fs.Failed-failed, tc.wantError)
			}
			fs.From = int64(len(s.raw)) // the source reads again
			src.Reset()
			rest, err := readRest(d)
			left := int64(len(s.raw)) - tc.from
			if err != io.EOF || !slices.Equal(append(got, rest...), want) || int64(src.Reads) > left/scanStep+20 {
				t.Errorf("%s, then a source that reads: %d packets, %v, in %d reads; want the %d a source that reads gives in at most %d",
					name, len(got)+len(rest), err, src.Reads, len(want), left/scanStep+20)
			}
			src.Reset()
			if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
				t.Fatal(err)
			}
			if all, err := readRest(d); err != io.EOF || !slices.Equal(all, want) || src.Reads > again {
				t.Errorf("%s, healed, a seek to 0 and a read: %d packets, %v, in %d reads; want the %d a readable source gives in its %d",
					name, len(all), err, src.Reads, len(want), again)
			}
		}
	}
}

// TestAnOpenClosesInOnAnUnreadableTail: an open whose tail walk meets a byte
// it cannot read in the last KiB returns the read's error after at most
// twenty failed reads, whatever its pages' size.
func TestAnOpenClosesInOnAnUnreadableTail(t *testing.T) {
	for _, s := range costStreams(t) {
		n := int64(len(s.raw))
		for _, back := range []int64{1, 2, 30, 500, 1024} {
			fs := &testutil.FailingSource{Src: container.BytesSource(s.raw), From: n - back, To: n - back + 1}
			_, err := NewDemuxer(fs, nil)
			if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || fs.Failed > 20 {
				t.Errorf("%s, no read %d bytes from the end: the open returned %v after %d failed reads, want the read's error after at most 20",
					s.name, back, err, fs.Failed)
			}
		}
	}
}
