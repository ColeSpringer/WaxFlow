package flacn_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// notExactNoted reports whether d notes a closing frame no read comes to.
func notExactNoted(d *flacn.Demuxer) bool {
	return slices.ContainsFunc(d.Warnings(), func(w container.Warning) bool {
		return w.Kind == container.Note && strings.Contains(w.Msg, "the length is not exact")
	})
}

// TestAClosingFrameBehindARunNumberedAheadIsExactHoweverLongTheRun: frames
// numbered far ahead in front of a closing frame numbered on from the
// frames before them are the ones out of place. A read comes to the closing
// frame past them, so its end is exact, whatever the run's length and
// wherever in front of the tail window the frame it follows lies.
func TestAClosingFrameBehindARunNumberedAheadIsExactHoweverLongTheRun(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	total := int64(n-1)*4096 + l.frames[n-1].Dur
	for _, run := range []int{2, 8, 9, n - 3} {
		// Frames in order, then run of them numbered 200 on, then the last.
		at := n - 1 - run
		raw, _ := l.stream(l.fs(0, at), l.renumbered(at, n-1, 200), one(l.f(n-1)))
		if tail := len(raw) - l.frames[at].Off; (tail > srcwin.Chunk) != (run >= 8) {
			t.Fatalf("a run of %d lies %d bytes from the end: this pins nothing of the tail window", run, tail)
		}
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("a run of %d: %v", run, err)
		}
		if tr := d.Tracks()[0]; tr.Samples != total || !tr.SamplesExact || notExactNoted(d) {
			t.Errorf("a run of %d: the open says %d samples, exact %v, findings %v; want %d exact", run, tr.Samples, tr.SamplesExact, d.Warnings(), total)
		}
		got, err := readFrames(t, d)
		if err != nil || len(got) != n {
			t.Fatalf("a run of %d: read %d packets (%v), want all %d", run, len(got), err, n)
		}
		var count int64
		for _, f := range got {
			count += f.dur + f.lost
		}
		if count != total {
			t.Errorf("a run of %d: the read counted %d samples, want the length's %d", run, count, total)
		}
		// And a closing frame numbered inside the frames in front stays out
		// of sequence, however far back the frame it would follow lies.
		raw, _ = l.stream(l.fs(0, n-1), one(l.as(n-1, 1, -1)))
		d, err = flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tr.SamplesExact || !notExactNoted(d) {
			t.Errorf("a closing frame numbered 1 behind %d frames: exact %v, findings %v; want the length not exact", n-1, tr.SamplesExact, d.Warnings())
		}
	}
}

// wideStream is n samples of noise in eight channels of 32 bits, whose
// frames are each larger than the open's tail window.
func wideStream(t *testing.T, n int) []byte {
	t.Helper()
	src := audio.Get(muxFmt(48000, 8, 32), n)
	defer audio.Put(src)
	src.N = n
	r := rand.New(rand.NewPCG(uint64(n), 11))
	for c := range src.Fmt.Channels {
		s := src.ChanI(c)
		for j := range n {
			s[j] = int32(r.Uint32())
		}
	}
	var buf bytes.Buffer
	encodeStream(t, src, 5, &buf, int64(n))
	return buf.Bytes()
}

// TestAClosingFrameWithNoWholeFrameInTheTailWindowIsChecked: where the tail
// window holds no whole frame in front of the closing frame, the open reads
// the frame in front: one read of the largest frame's bytes on a healthy
// stream, which keeps its exact length, and as far as a resync reaches in
// front of damage. A closing frame numbered inside the frames in front is
// then out of sequence as anywhere else.
func TestAClosingFrameWithNoWholeFrameInTheTailWindowIsChecked(t *testing.T) {
	// Frames larger than the window.
	wide := layOut(t, wideStream(t, 3*4096+100))
	n := len(wide.frames)
	if n != 4 || wide.frames[n-2].End-wide.frames[n-2].Off <= srcwin.Chunk {
		t.Fatalf("%d frames, the one in front of the last %d bytes: want it larger than the tail window", n, wide.frames[n-2].End-wide.frames[n-2].Off)
	}
	total := int64(3*4096 + 100)
	src := &testutil.CountingSource{Src: container.BytesSource(wide.whole), Log: true}
	d, err := flacn.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != total || !tr.SamplesExact || len(d.Warnings()) != 0 {
		t.Errorf("a healthy stream of large frames: %d samples, exact %v, findings %v; want %d exact and none", tr.Samples, tr.SamplesExact, d.Warnings(), total)
	}
	// The head, the tail window, and the frame in front of the closing one:
	// from where the largest frame could start, which this stream does not
	// state, so a header, a footer and 4096 samples of 33 bits a channel.
	tailAt := int64(len(wide.whole)) - srcwin.Chunk
	near := int64(wide.frames[n-1].Off) - (16 + 2 + 8*(1+(33*4096+7)/8))
	if last := src.Spans[len(src.Spans)-1]; last != [2]int64{near, tailAt} || !slices.Contains(src.Spans, [2]int64{tailAt, int64(len(wide.whole))}) {
		t.Errorf("the open read %v, want the tail window and then the one read %v", src.Spans, [2]int64{near, tailAt})
	}
	for _, s := range src.Spans[:len(src.Spans)-1] {
		if s[0] > 8192 && s[0] != tailAt {
			t.Errorf("the open read %v: a read at %d is neither the head's nor the tail window's", src.Spans, s[0])
		}
	}
	raw, _ := wide.stream(wide.fs(0, n-1), one(wide.as(n-1, 1, -1)))
	if d, err = flacn.NewDemuxer(container.BytesSource(raw), nil); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.SamplesExact || !notExactNoted(d) {
		t.Errorf("large frames, the closing one numbered 1: exact %v, findings %v; want the length not exact", tr.SamplesExact, d.Warnings())
	}

	// Junk a window long in front of the closing frame.
	l := layOut(t, noiseStream(t, 60000))
	n = len(l.frames)
	short := l.frames[n-1].Dur
	for _, tc := range []struct {
		coded int // the closing frame's number, behind frames 0 to 9 and the junk
		exact bool
	}{{5, false}, {10, true}, {12, true}} {
		name := fmt.Sprintf("junk in front of a closing frame numbered %d", tc.coded)
		raw, _ := l.stream(l.fs(0, 10), one(junk(srcwin.Chunk+4096)), one(l.as(n-1, tc.coded, -1)))
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ends := int64(tc.coded)*4096 + short
		if tr := d.Tracks()[0]; tr.Samples != ends || tr.SamplesExact != tc.exact || notExactNoted(d) == tc.exact {
			t.Errorf("%s: the open says %d samples, exact %v, findings %v; want %d, exact %v", name, tr.Samples, tr.SamplesExact, d.Warnings(), ends, tc.exact)
		}
		if tc.exact {
			continue
		}
		// A decode is not cut where the closing frame ends.
		med, err := format.Open(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c := decodeRest(t, med); c != 10*4096 {
			t.Errorf("%s: a decode delivered %d samples, want the ten frames' %d", name, c, 10*4096)
		}
		med.Close()
	}

	// Junk further than a resync reaches in front of the closing frame: no
	// read comes to it from the frames in front, whatever its number.
	raw, _ = l.stream(l.fs(0, 10), one(junk(flacn.MaxResyncForTest+srcwin.Chunk)), one(l.as(n-1, 10, -1)))
	if d, err = flacn.NewDemuxer(container.BytesSource(raw), nil); err != nil {
		t.Fatal(err)
	}
	far := slices.ContainsFunc(d.Warnings(), func(w container.Warning) bool {
		return w.Kind == container.Note && strings.Contains(w.Msg, "as far as a resync reaches; the length is not exact")
	})
	if tr := d.Tracks()[0]; tr.Samples != 10*4096+short || tr.SamplesExact || !far {
		t.Errorf("junk past a resync's reach in front of the closing frame: %d samples, exact %v, findings %v; want %d, not exact, and the reason noted",
			tr.Samples, tr.SamplesExact, d.Warnings(), 10*4096+short)
	}
}

// TestASwappedFirstPairWithNoDamageIsReadInOrder: a stream whose first two
// frames lie the other way round, nothing damaged in front, starts at the
// earlier one. Both are read, in order, with nothing lost, from the open
// and from a seek, and the length is every frame's.
func TestASwappedFirstPairWithNoDamageIsReadInOrder(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	raw, reads := l.stream(one(l.f(1), l.f(0)), l.fs(2, n))
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != 60000 || !tr.SamplesExact {
		t.Errorf("the open says %d samples, exact %v; want all 60000 exact", tr.Samples, tr.SamplesExact)
	}
	holdsReads(t, "from the open", d, reads, 0)
	for _, target := range []int64{0, 100, 4096 + 5, 3 * 4096} {
		landed, err := d.SeekSample(t.Context(), 0, target)
		if err != nil || landed != target/4096*4096 {
			t.Fatalf("a seek to %d landed at %d (%v), want the frame holding it", target, landed, err)
		}
		holdsReads(t, fmt.Sprintf("behind a seek to %d", target), d, reads[landed/4096:], 0)
	}
	msgs := strings.Join(damageOf(d), "; ")
	if !strings.Contains(msgs, "read in order") || strings.Contains(msgs, "dropped") || strings.Contains(msgs, "lost") {
		t.Errorf("findings %q, want the pair read in order and nothing dropped or lost", msgs)
	}
	// Strict refuses the pair at the read, as it does one further in.
	s, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a Strict open returned %v, want the read to refuse", err)
	}
	var pkt container.Packet
	if err := s.ReadPacket(&pkt); err == nil {
		t.Error("a Strict read delivered a packet, want the pair refused")
	}
}

// TestAClosingFrameBehindManyHeadersWithNoFrameIsStillChecked: headers
// that parse with no frame behind them, however many lie between the
// closing frame and the whole frame in front of it, do not leave it
// unchecked: the parses are bounded by the bytes they read, not counted.
func TestAClosingFrameBehindManyHeadersWithNoFrameIsStillChecked(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	for _, tc := range []struct {
		name  string
		head  int // the bytes of frame 3 each header keeps
		exact bool
	}{
		// The header alone: the parse behind it ends in a few bytes.
		{"200 bare headers", 6, false},
		// With the start of its subframes each parse runs on through the
		// rest, past twice the bytes searched in all: the search stops and
		// the length is called as it was.
		{"200 headers whose parses run long", 32, true},
	} {
		var heads []byte
		for range 200 {
			heads = append(heads, l.bytesOf(3)[:tc.head]...)
			heads = append(heads, testutil.SyncJunk(64-tc.head, 0xFF)...)
		}
		raw, _ := l.stream(l.fs(0, 10), one(part{heads, -1}), one(l.as(n-1, 5, -1)))
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tr.SamplesExact != tc.exact || notExactNoted(d) == tc.exact {
			t.Errorf("a closing frame numbered 5 behind ten frames and %s: exact %v, findings %v; want exact %v", tc.name, tr.SamplesExact, d.Warnings(), tc.exact)
		}
	}
}

// TestThePairLookIsAskedOnlyWhereItCanAnswer: the look behind the first
// frame for the frame due in front of it reads a window there. A stream
// numbered from past 0 pays it; one with bytes skipped in front of its
// first frame, or numbered from 0, reads nothing for it.
func TestThePairLookIsAskedOnlyWhereItCanAnswer(t *testing.T) {
	whole := noiseStream(t, 200000)
	first := frameSpans(t, whole)[0][0]
	late := testutil.RenumberFLACFrames(t, whole, 1000)
	junked := slices.Concat(late[:first], testutil.SyncJunk(100, 0xFF), late[first:])
	cost := func(raw []byte) int64 {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := flacn.NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		return src.Bytes
	}
	plain, look, skipped := cost(whole), cost(late), cost(junked)
	if look-plain < srcwin.Chunk {
		t.Fatalf("a stream numbered from 1000 opened in %d bytes and one from 0 in %d: want a window more for the pair look, or this pins nothing", look, plain)
	}
	if skipped-plain >= srcwin.Chunk {
		t.Errorf("a stream numbered from 1000 behind 100 skipped bytes opened in %d bytes, one from 0 in %d: the pair look was read for", skipped, plain)
	}
}

// TestASeekCanceledOnTheWayLeavesTheReadWhereItStood: a seek reads its
// context at every piece a probe scans and every view of a frame's scan.
// One whose context ends on the way returns the walk's cancel and leaves
// the read, its findings and the length as they stood, whichever scan the
// cancel fell in. A seek that returns no error landed where a live one
// does and lists what a live one lists, at every count of checks.
func TestASeekCanceledOnTheWayLeavesTheReadWhereItStood(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	laid := func(parts ...[]part) []byte { raw, _ := l.stream(parts...); return raw }
	twin := unseekableTwin(t, 2)
	for name, tc := range map[string]struct {
		raw     []byte
		targets []int64
	}{
		"a healthy stream": {l.whole, []int64{11*4096 + 5}},
		// The walk from the frame behind a swap starts again further back.
		"a swapped pair": {laid(l.fs(0, 8), one(l.f(9), l.f(8)), l.fs(10, n)), []int64{8*4096 + 5, 11*4096 + 5}},
		// A scan through junk between two frames.
		"junk between frames": {laid(l.fs(0, 9), one(junk(300<<10)), l.fs(9, n)), []int64{11*4096 + 5}},
		// A run held in front of the frames it is numbered past: the scan
		// that asks whether it is due is one a cancel can fall in.
		"a held run": {laid(l.fs(0, 8), one(l.f(12), l.f(13)), l.fs(8, 12), l.fs(14, n)), []int64{8*4096 + 5, 9*4096 + 5, 10*4096 + 5, 12*4096 + 5}},
		// A head hole in front of frames joined on: the landing is not 0.
		"a head hole before a join": {headless(farBehindAHole(t, twin, 1, 5000), testutil.FLACFrames(t, twin)[0].Off), []int64{0, 100}},
	} {
		for _, target := range tc.targets {
			open := func() *flacn.Demuxer {
				d, err := flacn.NewDemuxer(container.BytesSource(tc.raw), nil)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				var pkt container.Packet
				for range 2 {
					if err := d.ReadPacket(&pkt); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
				}
				return d
			}
			live := open()
			want, err := live.SeekSample(t.Context(), 0, target)
			if err != nil {
				t.Fatalf("%s: a seek to %d: %v", name, target, err)
			}
			listed := len(live.Warnings())
			behind, _ := readFrames(t, live)
			rest, _ := readFrames(t, open())
			canceled := 0
			for checks := 1; ; checks++ {
				d := open()
				findings, track := len(d.Warnings()), d.Tracks()[0]
				ctx := testutil.CancelAfterChecks(t, checks)
				landed, err := d.SeekSample(ctx, 0, target)
				if err == nil {
					// Whether or not the context outlived it, the seek is a
					// live one's.
					got, _ := readFrames(t, d)
					if landed != want || len(d.Warnings()) < listed || !slices.EqualFunc(got, behind, sameFrame) {
						t.Errorf("%s: a seek to %d whose context ends on check %d returned no error, landed at %d with %d findings; want %d, %d and the same frames behind it",
							name, target, checks, landed, len(d.Warnings()), want, listed)
					}
					d = open()
					if _, err := d.SeekSample(testutil.CancelAfterChecks(t, checks), 0, target); err == nil && len(d.Warnings()) != listed {
						t.Errorf("%s: a seek to %d whose context ends on check %d returned no error and lists %d findings, a live one %d", name, target, checks, len(d.Warnings()), listed)
					}
					if ctx.Err() == nil {
						break // the seek asked fewer times than that
					}
					continue
				}
				canceled++
				if waxerr.CodeOf(err) != waxerr.CodeCanceled || landed != 0 {
					t.Fatalf("%s: a seek to %d canceled on check %d returned %d, %v; want the cancel", name, target, checks, landed, err)
				}
				if len(d.Warnings()) != findings || !reflect.DeepEqual(d.Tracks()[0], track) {
					t.Errorf("%s: a seek to %d canceled on check %d left findings %v and track %+v", name, target, checks, d.Warnings(), d.Tracks()[0])
				}
				if got, _ := readFrames(t, d); !slices.EqualFunc(got, rest, sameFrame) {
					t.Errorf("%s: behind a seek to %d canceled on check %d the read delivered %d frames, want the %d a read with no seek does", name, target, checks, len(got), len(rest))
				}
				// And the seek again, with a live context.
				d = open()
				d.SeekSample(testutil.CancelAfterChecks(t, checks), 0, target)
				if landed, err := d.SeekSample(t.Context(), 0, target); err != nil || landed != want {
					t.Errorf("%s: the seek to %d again after a cancel on check %d landed at %d (%v), want %d", name, target, checks, landed, err, want)
				}
			}
			if canceled < 1 {
				t.Errorf("%s: no seek to %d was canceled: this pins nothing", name, target)
			}
		}
	}
}

// sameFrame reports whether two frames a read delivered are one.
func sameFrame(a, b frame) bool {
	return a.pts == b.pts && a.lost == b.lost && a.discont == b.discont && bytes.Equal(a.data, b.data)
}

// TestASeekCanceledBeforeItsFirstProbeReadsNothingForIt: a seek whose
// context has ended scans no probe and no frame. On a stream long enough
// to bisect it reads the first frame's header at most.
func TestASeekCanceledBeforeItsFirstProbeReadsNothingForIt(t *testing.T) {
	raw := noiseStream(t, 200000)
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := flacn.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 1), 0, 150000); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek with an ended context returned %v, want the cancel", err)
	}
	if src.Bytes > flac.MaxFrameHeaderLen {
		t.Errorf("a seek with an ended context read %d bytes in %d reads, want the first frame's header at most", src.Bytes, src.Reads)
	}
	src.Reset()
	if landed, err := d.SeekSample(t.Context(), 0, 150000); err != nil || landed != 150000/4096*4096 || src.Bytes < 4096 {
		t.Fatalf("the seek again landed at %d (%v) reading %d bytes, want the frame holding it and its probes read: this pins nothing", landed, err, src.Bytes)
	}
}

// failsOnce is a source whose read number fail (from 1) fails, once.
type failsOnce struct {
	container.Source
	fail, reads int
	spans       [][2]int64
}

func (s *failsOnce) ReadAt(p []byte, off int64) (int, error) {
	s.reads++
	s.spans = append(s.spans, [2]int64{off, off + int64(len(p))})
	if s.reads == s.fail {
		return 0, testutil.ErrSourceDown
	}
	return s.Source.ReadAt(p, off)
}

// TestAFailedLookBehindTheFirstFrameIsNoAnswer: the looks behind the first
// frame, for the frame due in front of it and for what settles the
// numbering, only strengthen a claim. A read that fails in one is no answer
// and not the open's error, on a stream under a window as on a longer one:
// the open goes on as if the look had found nothing, and the failure does
// not stick to the reads behind it.
func TestAFailedLookBehindTheFirstFrameIsNoAnswer(t *testing.T) {
	for name, whole := range map[string][]byte{
		"the pair look, a stream numbered from 1000":  testutil.RenumberFLACFrames(t, noiseStream(t, 20000), 1000),
		"the numbering look, bounds of no block size": oldFormat(t, noiseStream(t, 20000)),
	} {
		first := int64(frameSpans(t, whole)[0][0])
		if int64(len(whole))-first > srcwin.Chunk {
			t.Fatalf("%s: %d bytes of frames, want a stream under a window", name, int64(len(whole))-first)
		}
		clean := &failsOnce{Source: container.BytesSource(whole)}
		d, err := flacn.NewDemuxer(clean, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, _ := readFrames(t, d)
		// The look is the read behind the first frame's header.
		look := slices.Index(clean.spans, [2]int64{first, first + flac.MaxFrameHeaderLen}) + 2
		if look < 2 || look > len(clean.spans) || clean.spans[look-1][0] != first+flac.MaxFrameHeaderLen {
			t.Fatalf("%s: the open read %v, want the first frame's header and then the look from behind it: this pins nothing", name, clean.spans)
		}
		src := &failsOnce{Source: container.BytesSource(whole), fail: look}
		d, err = flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatalf("%s: the open returned %v with the look's read failing, want it to go on", name, err)
		}
		if got, err := readFrames(t, d); err != nil || len(got) != len(want) {
			t.Errorf("%s: behind a failed look the read delivered %d frames (%v), want all %d", name, len(got), err, len(want))
		}
	}
}

// TestThePairLookOnAStreamUnderAWindowReadsNoByteTwice: on a stream under a
// window the look behind the first frame keeps what it read, so an open and
// a read of a stream numbered from past 0 cost what they cost one numbered
// from 0.
func TestThePairLookOnAStreamUnderAWindowReadsNoByteTwice(t *testing.T) {
	whole := noiseStream(t, 20000)
	cost := func(raw []byte) int64 {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := readFrames(t, d); err != nil || len(got) != 5 {
			t.Fatalf("read %d frames (%v), want 5", len(got), err)
		}
		return src.Bytes
	}
	late := testutil.RenumberFLACFrames(t, whole, 1000)
	if plain, look := cost(whole), cost(late); look > plain+int64(len(late)-len(whole)) {
		t.Errorf("a stream numbered from 1000 cost %d bytes to open and read, one from 0 %d: the look's bytes were read again", look, plain)
	}
}
