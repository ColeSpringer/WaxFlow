package flacn_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// part is bytes of a stream laid out by hand, and the frame position a read
// delivers them at: -1 for bytes no read delivers.
type part struct {
	b  []byte
	at int
}

// laid builds streams from a whole one's frames: out of their numbers'
// order, numbered off, damaged, and with junk between them.
type laid struct {
	t      *testing.T
	whole  []byte
	frames []testutil.FLACFrame
}

func layOut(t *testing.T, whole []byte) laid {
	return laid{t, whole, testutil.FLACFrames(t, whole)}
}

func (l laid) bytesOf(k int) []byte { return l.whole[l.frames[k].Off:l.frames[k].End] }

// f is frame k as it is, read at its own position.
func (l laid) f(k int) part { return part{l.bytesOf(k), k} }

// fs is frames [from, to) as they are.
func (l laid) fs(from, to int) []part {
	var ps []part
	for k := from; k < to; k++ {
		ps = append(ps, l.f(k))
	}
	return ps
}

// as is frame k numbered coded, read at position at.
func (l laid) as(k, coded, at int) part {
	return part{testutil.SetFLACFrameNumber(l.t, l.bytesOf(k), false, uint64(coded)), at}
}

// renumbered is frames [from, to) numbered delta on, each read where its
// number puts it.
func (l laid) renumbered(from, to, delta int) []part {
	var ps []part
	for k := from; k < to; k++ {
		ps = append(ps, l.as(k, k+delta, k+delta))
	}
	return ps
}

// gone is frame k as it is, which no read delivers.
func (l laid) gone(k int) part { return part{l.bytesOf(k), -1} }

// damaged is frame k with its middle inverted, its header whole.
func (l laid) damaged(k int) part {
	f := l.frames[k]
	return part{testutil.DamageFLACFrames(l.t, l.whole, l.frames, k)[f.Off:f.End], -1}
}

// headless is frame k with its header cleared.
func (l laid) headless(k int) part {
	b := slices.Clone(l.bytesOf(k))
	clear(b[:8])
	return part{b, -1}
}

// junk is n bytes no header scan allows.
func junk(n int) part { return part{testutil.SyncJunk(n, 0xFF), -1} }

// decoy is junk holding the head of frame k, with nothing of it behind.
func (l laid) decoy(k int) part {
	j := testutil.SyncJunk(8<<10, 0xFF)
	copy(j[len(j)-2048:], l.bytesOf(k)[:32])
	return part{j, -1}
}

// stream is the metadata and the parts in the order given; reads is what a
// read delivers of them, in the order of their positions.
func (l laid) stream(parts ...[]part) (raw []byte, reads []part) {
	raw = slices.Clone(l.whole[:l.frames[0].Off])
	for _, p := range slices.Concat(parts...) {
		raw = append(raw, p.b...)
		if p.at >= 0 {
			reads = append(reads, p)
		}
	}
	slices.SortStableFunc(reads, func(a, b part) int { return a.at - b.at })
	return raw, reads
}

// one is a single part as a list, for stream.
func one(ps ...part) []part { return ps }

// holdsReads fails unless a read of d from where it stands delivers reads,
// each frame's bytes alone at its position, with lost samples owed in all.
func holdsReads(t *testing.T, how string, d *flacn.Demuxer, reads []part, lost int64) (got []frame) {
	t.Helper()
	got, err := readFrames(t, d)
	if err != nil {
		t.Errorf("%s: the read ended %v", how, err)
		return nil
	}
	var at, want []int
	var owed int64
	for _, f := range got {
		at = append(at, int(f.pts/4096))
		owed += f.lost
	}
	for _, p := range reads {
		want = append(want, p.at)
	}
	if !slices.Equal(at, want) {
		t.Errorf("%s: read frames %v, want %v", how, at, want)
		return nil
	}
	for i, f := range got {
		if !bytes.Equal(f.data, reads[i].b) {
			t.Errorf("%s: the packet at frame %d is not the frame laid out for it (%d bytes, want %d)", how, at[i], len(f.data), len(reads[i].b))
		}
	}
	if lost >= 0 && owed != lost {
		t.Errorf("%s: %d samples owed, want %d", how, owed, lost)
	}
	return got
}

// TestWholeFramesBehindASkipAreReadWhateverFollowsThem: a frame that checks
// where the numbers skip, and the whole frames numbered on from it, are read
// whatever lies behind them: a frame without its header, junk, a decoy, a
// damaged frame, a second skip. A swapped pair and frames numbered off their
// gap are read in place, and frames numbered past the stream with no gap
// that holds them are joined on at their own numbers, as they always were.
func TestWholeFramesBehindASkipAreReadWhateverFollowsThem(t *testing.T) {
	l := layOut(t, unseekableTwin(t, 4))
	n := len(l.frames)
	id3v1 := append([]byte("TAG"), make([]byte, 125)...)
	for _, tc := range []struct {
		name    string
		parts   [][]part
		lost    int64
		finding string // one of the read's, in part
	}{
		// Frames numbered past the stream, in front of a frame numbered in the gap.
		{"two frames numbered 200 on, the frame behind them headless",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 207, 7), l.headless(8)), l.fs(9, n)}, 4096,
			"bytes hold frames numbered past the stream, read at the head of the gap behind them"},
		{"two frames numbered 200 on, junk behind them",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 207, 7), junk(8<<10)), l.fs(8, n)}, 0,
			"8192 bytes are not a valid frame, dropped: 0 samples lost"},
		{"two frames numbered 200 on, bytes of no frame behind them",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 207, 7), part{bytes.Repeat([]byte{0x11}, 1021), -1}), l.fs(8, n)}, 0,
			"bytes hold frames numbered off the gap they fill, read there"},
		{"two frames numbered 200 on, a decoy behind them",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 207, 7), l.decoy(8)), l.fs(8, n)}, 0, ""},
		{"two frames numbered 200 on, the frame behind them damaged",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 207, 7), l.damaged(8)), l.fs(9, n)}, 4096, ""},
		{"a frame numbered 200 on, the frame behind it headless",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.headless(7)), l.fs(8, n)}, 4096, ""},
		{"two frames numbered 200 on, the second damaged",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), part{l.damagedAs(7, 207), -1}), l.fs(8, n)}, 4096, ""},
		{"two frames numbered 200 on short of their gap, junk behind them",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 207, 7), junk(8<<10)), l.fs(9, n)}, 4096, ""},
		{"a frame numbered 200 on, one numbered further on, a frame gone",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 300, 7)), l.fs(9, n)}, 4096, ""},
		{"three frames numbered 200 on in a gap of two, junk behind them",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 206), l.as(7, 207, 207), l.as(8, 208, 208), junk(8<<10)), goneAll(l.fs(8, n))}, 0,
			"past what the stream holds; joined"},
		// A run is the frames that lie one behind the other: one that checks
		// across a frame between them is no part of it, and positions rise.
		{"three frames numbered on by 200, 500 and 199",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.as(7, 507, 7), l.as(8, 207, 8)), l.fs(9, n)}, 0, ""},
		{"two frames numbered 200 on, an earlier frame between them",
			[][]part{l.fs(0, 6), one(l.as(6, 206, 6), l.gone(3), l.as(7, 207, 7)), l.fs(8, n)}, 0, ""},
		{"junk, then three frames numbered on by 200, 500 and 199",
			[][]part{l.fs(0, 6), one(junk(8<<10), l.as(6, 206, 6), l.as(7, 507, 7), l.as(8, 207, 8)), l.fs(9, n)}, 0, ""},
		// Behind frames met again the read always waited for the successor,
		// so only the successor, or the scan's end without one, ends the wait.
		{"a frame again, two frames numbered far on, then the stream numbered on from before them",
			[][]part{l.fs(0, 4), one(l.as(4, 1, -1), l.as(5, 202, -1), l.as(6, 203, -1)), l.renumbered(7, n, -3)}, 0, ""},
		{"an earlier frame, a frame numbered 200 on, then the successor",
			[][]part{l.fs(0, 6), one(l.gone(3), l.as(6, 206, -1)), l.fs(6, n)}, 0, ""},
		{"an earlier frame, a frame ahead, then the frames it skipped",
			[][]part{l.fs(0, 6), one(l.gone(3), l.f(9)), l.fs(6, 9), l.fs(10, n)}, 0, ""},
		{"an earlier frame, a frame ahead, a later frame, then the successor",
			[][]part{l.fs(0, 6), one(l.gone(3), l.f(9), l.gone(7), l.f(6), l.f(8)), l.fs(10, n)}, 4096, ""},
		{"an earlier frame, two frames numbered 200 on, then no successor",
			[][]part{l.fs(0, 6), one(l.gone(3), l.as(6, 206, 6), l.as(7, 207, 7)), l.fs(8, n)}, 0, ""},
		// Behind a held run the frames read in front of it were such frames to
		// a read that took the run first, and it waited for the successor too.
		{"a swapped pair, two frames ahead out of run, then the frames they skipped",
			[][]part{l.fs(0, 6), one(l.f(7), l.f(6), l.gone(10), l.gone(12), l.f(8), l.f(9), l.f(11)), l.fs(13, n)}, 2 * 4096, ""},
		// Frames numbered ahead inside the stream: held until they are due.
		{"two frames numbered 20 on, the frame behind them headless",
			[][]part{l.fs(0, 6), one(l.as(6, 26, 26), l.as(7, 27, 27), l.headless(8)), l.fs(9, 26), one(l.gone(26), l.gone(27)), l.fs(28, n)}, 3 * 4096,
			"bytes hold frames numbered ahead of the one behind them, read in order"},
		{"a swapped pair, junk behind it",
			[][]part{l.fs(0, 6), one(l.f(7), l.f(6), junk(8<<10)), l.fs(8, n)}, 0,
			"8192 bytes are not a valid frame, dropped: 0 samples lost"},
		{"a swapped pair, the frame behind it headless",
			[][]part{l.fs(0, 6), one(l.f(7), l.f(6), l.headless(8)), l.fs(9, n)}, 4096, ""},
		{"a swapped pair, a decoy behind it",
			[][]part{l.fs(0, 6), one(l.f(7), l.f(6), l.decoy(8)), l.fs(8, n)}, 0, ""},
		{"a swapped pair, the frame behind it damaged",
			[][]part{l.fs(0, 6), one(l.f(7), l.f(6), l.damaged(8)), l.fs(9, n)}, 4096, ""},
		{"a swapped pair, its second frame damaged",
			[][]part{l.fs(0, 6), one(l.f(7), l.damaged(6)), l.fs(8, n)}, 4096, ""},
		// The earlier frame is confirmed by the frames behind it, whole to the stream's end.
		{"a swapped pair, junk between its frames",
			[][]part{l.fs(0, 6), one(l.f(7), junk(8<<10), l.f(6)), l.fs(8, n)}, 0,
			"8192 bytes are not a valid frame, dropped"},
		{"a swapped pair, an earlier frame between its frames",
			[][]part{l.fs(0, 6), one(l.f(7), l.gone(3), l.f(6)), l.fs(8, n)}, 0,
			"bytes hold frames out of sequence, dropped"},
		{"the last two swapped, a tag behind them",
			[][]part{l.fs(0, n-2), one(l.f(n-1), l.f(n-2), part{id3v1, -1})}, 0, ""},
		{"a frame ahead, the frames in front of its number damaged and gone",
			[][]part{l.fs(0, 6), one(l.f(9), l.f(6), l.damaged(7)), l.fs(10, n)}, 2 * 4096, ""},
		// One run is held at a time: a second is read as it always was.
		{"a frame ahead, a pair swapped behind it",
			[][]part{l.fs(0, 6), one(l.f(9), l.f(6), l.f(8), l.gone(7)), l.fs(10, n)}, 4096, ""},
		// A frame numbered past the held one lies in front of the frame it skipped:
		// holding would drop it, so the read goes on as it always did.
		{"a frame ahead, junk and one numbered past it in front of the frame it skipped",
			[][]part{l.fs(0, 9), one(l.f(10), junk(8<<10), l.f(12), l.gone(9), l.gone(11)), l.fs(13, n)}, 2 * 4096, ""},
		// A skip the stream could have, then damage: the frame it lands on is read.
		{"a frame gone, junk behind the next",
			[][]part{l.fs(0, 6), one(l.f(7), junk(8<<10)), l.fs(8, n)}, 4096, ""},
		{"a frame gone, the second frame on headless",
			[][]part{l.fs(0, 6), one(l.f(7), l.headless(8)), l.fs(9, n)}, 2 * 4096, ""},
		{"a frame gone, the second frame on damaged",
			[][]part{l.fs(0, 6), one(l.f(7), l.damaged(8)), l.fs(9, n)}, 2 * 4096, ""},
	} {
		raw, reads := l.stream(tc.parts...)
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := holdsReads(t, tc.name+" from the open", d, reads, tc.lost)
		// The frame read in front of a held one follows the frame before it,
		// unless bytes of no frame were stepped over on the way.
		switch {
		case got == nil:
		case tc.name == "a swapped pair, junk behind it" && got[6].discont:
			t.Errorf("%s: frame 6 is marked discontinuous", tc.name)
		case tc.name == "a swapped pair, junk between its frames" && !got[6].discont:
			t.Errorf("%s: frame 6, behind the junk, is not marked discontinuous", tc.name)
		}
		msgs := strings.Join(damageOf(d), "; ")
		if !strings.Contains(msgs, tc.finding) {
			t.Errorf("%s: findings %q, want %q", tc.name, msgs, tc.finding)
		}
		// Frames are reported out of sequence only where the case lays some out.
		if strings.Contains(msgs, "out of sequence") != (strings.Contains(tc.name, "earlier frame") || strings.Contains(tc.name, "a frame again") || strings.Contains(tc.name, "out of run") ||
			strings.Contains(tc.name, "numbered 20 on") || strings.Contains(tc.name, "a pair swapped behind it") || strings.Contains(tc.name, "numbered past it")) {
			t.Errorf("%s: findings %q", tc.name, msgs)
		}
		if strings.Contains(tc.name, "a tag behind") {
			if ws := d.Warnings(); !slices.ContainsFunc(ws, func(w container.Warning) bool {
				return w.Kind == container.Note && strings.Contains(w.Msg, "128 trailing tag or padding bytes ignored")
			}) || strings.Contains(msgs, "trailing bytes") {
				t.Errorf("%s: warnings %v, want the tag noted and nothing dropped", tc.name, ws)
			}
		}
		// Behind a seek the read delivers what a read from the open does.
		for _, k := range []int{5, 6, 7, 8, 9, 10, 26, 27, n - 2, n - 1} {
			landed, err := d.SeekSample(context.Background(), 0, int64(k)*4096+5)
			if err != nil {
				t.Fatalf("%s: a seek into frame %d: %v", tc.name, k, err)
			}
			from := int(landed / 4096)
			// A frame the read delivers is the one a seek into it lands on.
			at := slices.ContainsFunc(reads, func(p part) bool { return p.at == k })
			if landed%4096 != 0 || from > k || at && from != k {
				t.Errorf("%s: a seek into frame %d landed at %d", tc.name, k, landed)
				continue
			}
			i := slices.IndexFunc(reads, func(p part) bool { return p.at >= from })
			if i < 0 {
				i = len(reads)
			}
			holdsReads(t, fmt.Sprintf("%s behind a seek into frame %d", tc.name, k), d, reads[i:], -1)
		}
	}
}

// damagedAs is frame k numbered coded, with its middle inverted.
func (l laid) damagedAs(k, coded int) []byte {
	b := testutil.SetFLACFrameNumber(l.t, l.bytesOf(k), false, uint64(coded))
	for i := len(b)/2 - 8; i < len(b)/2+8; i++ {
		b[i] ^= 0xFF
	}
	return b
}

// goneAll is ps with none of them read.
func goneAll(ps []part) []part {
	out := slices.Clone(ps)
	for i := range out {
		out[i].at = -1
	}
	return out
}

// TestAHeldRunPastTheWindowIsReadWhenItIsDue: a frame numbered far ahead
// waits for its number however far the read goes first. The window lets go
// of it past a resync's reach and it is read again when it is due, so the
// window stays bounded and no frame is lost.
func TestAHeldRunPastTheWindowIsReadWhenItIsDue(t *testing.T) {
	const frames, ahead = 200, 150
	l := layOut(t, noiseStream(t, frames*4096))
	if len(l.frames) != frames {
		t.Fatalf("%d frames, want %d", len(l.frames), frames)
	}
	if between := l.frames[ahead].Off - l.frames[6].Off; between < 2*flacn.MaxResyncForTest {
		t.Fatalf("%d bytes between the frame and its place, want over two resyncs' reach", between)
	}
	raw, reads := l.stream(l.fs(0, 6), one(l.f(ahead)), l.fs(6, ahead), l.fs(ahead+1, frames))
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	resident := 0
	for i, p := range reads {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if pkt.PTS != int64(p.at)*4096 || pkt.Lost != 0 || !bytes.Equal(pkt.Data, p.b) {
			t.Fatalf("packet %d is at %d owing %d, want frame %d alone", i, pkt.PTS, pkt.Lost, p.at)
		}
		resident = max(resident, d.ResidentForTest())
	}
	if limit := flacn.MaxResyncForTest + 3*srcwin.Chunk; resident > limit {
		t.Errorf("the window held %d bytes, want at most %d", resident, limit)
	}
	if msgs := damageOf(d); len(msgs) != 1 || !strings.Contains(msgs[0], "numbered ahead of the one behind them, read in order") {
		t.Errorf("findings %v, want the run read in order alone", msgs)
	}
}

// TestAFrameAskedAgainBehindARunEndsWhereItDid: a seek's walk finds the end
// of the frame it lands on and the read finds it again. Behind a held run,
// whose last frame goes on in bytes read before it, the two agree, and the
// second finds nothing the first did not.
func TestAFrameAskedAgainBehindARunEndsWhereItDid(t *testing.T) {
	l := layOut(t, unseekableTwin(t, 4))
	n := len(l.frames)
	// Two swapped pairs back to back: the second is found from the first's
	// last frame, in the bytes behind the frame that pair was read behind.
	raw, reads := l.stream(l.fs(0, 6), one(l.f(7), l.f(6), l.f(9), l.f(8)), l.fs(10, n))
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	holdsReads(t, "from the open", d, reads, 0)
	open := damageOf(d)
	if len(open) != 2 || !strings.HasSuffix(open[0], "numbered ahead of the one behind them, read in order") || !strings.HasSuffix(open[1], "numbered ahead of the one behind them, read in order") {
		t.Errorf("a read from the open found %q, want the two pairs read in order alone", open)
	}
	for k := 5; k < 11; k++ {
		if landed, err := d.SeekSample(context.Background(), 0, int64(k)*4096+5); err != nil || landed != int64(k)*4096 {
			t.Fatalf("a seek into frame %d landed at %d (%v)", k, landed, err)
		}
		holdsReads(t, fmt.Sprintf("behind a seek into frame %d", k), d, reads[k:], 0)
	}
	if msgs := damageOf(d); !slices.Equal(msgs, open) {
		t.Errorf("the seeks and their reads found %q, a read from the open %q", msgs, open)
	}
}

// TestAWaitOutOfReachReadsTheFrameBehindTheSkip: the wait behind a skip in
// the numbers ends a resync's reach on, with frames numbered on from it or
// with bytes of no frame behind it, and the frame the skip lands on is read.
func TestAWaitOutOfReachReadsTheFrameBehindTheSkip(t *testing.T) {
	long := layOut(t, noiseStream(t, 200*4096))
	short := layOut(t, unseekableTwin(t, 4))
	if n := long.frames[len(long.frames)-1].Off - long.frames[7].Off; n < 2*flacn.MaxResyncForTest {
		t.Fatalf("%d bytes of frames behind the skip, want over two resyncs' reach", n)
	}
	far := part{bytes.Repeat([]byte{0x11}, flacn.MaxResyncForTest+srcwin.Chunk), -1}
	for name, tc := range map[string]struct {
		l     laid
		parts [][]part
	}{
		"frames numbered on": {long, [][]part{long.fs(0, 6), long.fs(7, len(long.frames))}},
		"bytes of no frame":  {short, [][]part{short.fs(0, 6), one(short.f(7), far), short.fs(8, len(short.frames))}},
	} {
		raw, reads := tc.l.stream(tc.parts...)
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		holdsReads(t, name, d, reads, 4096)
	}
}

// TestASeekReachesFramesNoReadFromTheOpenDoes: frames joined on at numbers
// past the stream end a read, and the frames behind them were always reached
// by a seek. A walk that meets a skip behind its first frame starts further
// back, and where that walk ends in front of the frame it began on, the seek
// lands by the first walk, as it always did.
func TestASeekReachesFramesNoReadFromTheOpenDoes(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	raw, reads := l.stream(l.fs(0, 6), one(l.as(6, 100006, 100006), l.as(7, 100007, 100007), l.gone(7), l.headless(8)), goneAll(l.fs(9, n)))
	again := len(raw) - (len(l.whole) - l.frames[8].Off) - (l.frames[8].Off - l.frames[7].Off)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	holdsReads(t, "from the open", d, reads, 0)
	if off, ok := d.BisectForTest(10*4096 + 5); !ok || off != int64(again) {
		t.Fatalf("the bisection lands at %d (%v), want frame 7 again at %d: this pins nothing", off, ok, again)
	}
	for _, k := range []int{9, 10, n - 1} {
		landed, err := d.SeekSample(context.Background(), 0, int64(k)*4096+5)
		if err != nil || landed != int64(k)*4096 {
			t.Fatalf("a seek into frame %d landed at %d (%v), want the frame", k, landed, err)
		}
		holdsReads(t, fmt.Sprintf("behind a seek into frame %d", k), d, l.fs(k, n), 0)
	}
	// Into the frame met again the seek lands on it, the hole behind it owed.
	if landed, err := d.SeekSample(context.Background(), 0, 7*4096+5); err != nil || landed != 7*4096 {
		t.Fatalf("a seek into frame 7 landed at %d (%v), want the frame met again", landed, err)
	}
	holdsReads(t, "behind a seek into frame 7", d, slices.Concat(one(l.f(7)), l.fs(9, n)), 4096)
	// Past the end the walk from further back passes the frames joined on
	// and ends there, and the seek lands on the last frame the first reaches.
	if landed, err := d.SeekSample(context.Background(), 0, 1<<40); err != nil || landed != int64(n-1)*4096 {
		t.Errorf("a seek past the end landed at %d (%v), want the last frame at %d", landed, err, (n-1)*4096)
	}
	// With the last frame cut the open measures by a walk, which begins on
	// the frame met again and ends where the whole frames do, as it did.
	d, err = flacn.NewDemuxer(container.BytesSource(testutil.CutTail(raw, 100)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if off, ok := d.BisectForTest(1 << 40); !ok || off != int64(again) {
		t.Fatalf("the open's walk begins at %d (%v), want frame 7 again at %d: this pins nothing", off, ok, again)
	}
	if tr := d.Tracks()[0]; tr.Samples != int64(n-1)*4096 || tr.SamplesExact {
		t.Errorf("the open says %d samples, exact %v; want the %d the whole frames run to, not exact", tr.Samples, tr.SamplesExact, (n-1)*4096)
	}
}

// TestASeekIntoAHoleLandsInFrontOfItWhereverItsWalkBegan: a read steps
// from the frame in front of junk to frames numbered far on, the frame
// between them in the hole. A seek into the hole whose walk begins on that
// frame, where the far frames fill a gap, starts further back and lands
// where the read places the hole, and so does the walk the open measures by.
func TestASeekIntoAHoleLandsInFrontOfItWhereverItsWalkBegan(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	raw, reads := l.stream(l.fs(0, 5), one(junk(200<<10), l.gone(5), l.as(6, 206, 206), l.as(7, 207, 207), l.gone(8), l.gone(9), junk(120<<10)))
	lead, _ := l.stream(l.fs(0, 5), one(junk(200<<10)))
	between := len(lead)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The open's own walk begins on that frame too, and states the length
	// the read comes to.
	if tr := d.Tracks()[0]; tr.Samples != 208*4096 || tr.SamplesExact {
		t.Errorf("the open says %d samples, exact %v; want the read's %d, not exact", tr.Samples, tr.SamplesExact, 208*4096)
	}
	holdsReads(t, "from the open", d, reads, 201*4096)
	const target = 100*4096 + 5
	if off, ok := d.BisectForTest(target); !ok || off != int64(between) {
		t.Fatalf("the bisection lands at %d (%v), want the frame in the hole at %d: this pins nothing", off, ok, between)
	}
	landed, err := d.SeekSample(context.Background(), 0, target)
	if err != nil || landed != 4*4096 {
		t.Fatalf("a seek into the hole landed at %d (%v), want the frame in front of it at %d", landed, err, 4*4096)
	}
	holdsReads(t, "behind the seek", d, reads[4:], 201*4096)
}

// TestASeekLandsOnTheFrameItsWalkBeganOnWhereNoReadComesToIt: a frame
// numbered where frames in front of it already stand is out of sequence to
// a read, which drops it. A seek whose walk begins on it, the target in it,
// always landed on it; the walk from further back comes to another frame at
// that position, so the seek lands by the first.
func TestASeekLandsOnTheFrameItsWalkBeganOnWhereNoReadComesToIt(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	// Frames 6 and 7 numbered one on, then frame 8 as it is and frame 9
	// without its header.
	raw, reads := l.stream(l.fs(0, 6), one(l.as(6, 7, 7), l.as(7, 8, 8), l.gone(8), l.headless(9)), l.fs(10, n))
	lead, _ := l.stream(l.fs(0, 6), one(l.as(6, 7, 7), l.as(7, 8, 8)))
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	holdsReads(t, "from the open", d, reads, 2*4096)
	const target = 8*4096 + 5
	if off, ok := d.BisectForTest(target); !ok || off != int64(len(lead)) {
		t.Fatalf("the bisection lands at %d (%v), want frame 8 at %d: this pins nothing", off, ok, len(lead))
	}
	if landed, err := d.SeekSample(context.Background(), 0, target); err != nil || landed != 8*4096 {
		t.Fatalf("a seek into frame 8 landed at %d (%v), want %d", landed, err, 8*4096)
	}
	holdsReads(t, "behind the seek", d, slices.Concat(one(l.f(8)), l.fs(10, n)), 4096)
}

// TestASeekPastTheEndLandsWhereTheFramesRunFurthest: with the first two
// frames out of order under block size bounds that do not agree, frame
// numbers read as sample numbers from the first frame's, and the frames lie
// over one another. A read delivers the first alone; a seek past the end
// always landed on the frame its walk began on, which ends later. The walk
// from further back ends on the first frame, so the seek lands by its first
// walk, and the measure stays where a seek reaches.
func TestASeekPastTheEndLandsWhereTheFramesRunFurthest(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	laid, _ := l.stream(one(l.f(1), l.f(0)), l.fs(2, len(l.frames)))
	// The last frame is cut, so the open calls no length exact and the
	// measure is a seek past the end.
	raw := testutil.CutTail(withBounds(laid, 4608, 256), 5)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.SamplesExact {
		t.Fatalf("the open calls %d samples exact: this pins nothing", tr.Samples)
	}
	got, err := readFrames(t, d)
	if err != nil || len(got) != 1 || got[0].pts != 0 {
		t.Fatalf("a read delivered %d packets (%v), want the first frame alone: this pins nothing", len(got), err)
	}
	off, ok := d.BisectForTest(1 << 40)
	k := slices.IndexFunc(l.frames, func(f testutil.FLACFrame) bool { return int64(f.Off) == off })
	if !ok || k < 2 {
		t.Fatalf("a seek past the end walks from %d (%v), want a frame behind the first two: this pins nothing", off, ok)
	}
	// Frame k is numbered k, and positions run from the first frame's 1.
	landed, err := d.SeekSample(context.Background(), 0, 1<<40)
	if err != nil || landed != int64(k-1) {
		t.Fatalf("a seek past the end landed at %d (%v), want frame %d at %d", landed, err, k, k-1)
	}
	if got, err = readFrames(t, d); err != nil || len(got) != 1 || got[0].pts != int64(k-1) || !bytes.Equal(got[0].data, l.bytesOf(k)) {
		t.Errorf("behind the seek: %d packets (%v), want frame %d alone", len(got), err, k)
	}
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if tr, err := waxflow.MeasureLength(context.Background(), med); err != nil || tr.Samples != int64(k-1)+4096 {
		t.Errorf("MeasureLength says %d (%v), want the %d a seek reaches", tr.Samples, err, int64(k-1)+4096)
	}
}

// TestASeekGoesOnAsTheReadDoesBehindFramesOutOfSequence: where frame
// numbers read as sample numbers the frames lie over one another, so a
// frame numbered past a run can still lie before where the run ends, and
// reading the run first never comes to it. A read behind frames out of
// sequence always took the hole to such a frame, whole to the stream's end,
// and so does the read behind a seek that lands among them.
func TestASeekGoesOnAsTheReadDoesBehindFramesOutOfSequence(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	decoy := part{testutil.SyncJunk(300<<10, 0xFF), -1}
	copy(decoy.b[len(decoy.b)-2048:], l.bytesOf(1)[:32])
	// A decoy, frames 1 to 6, frames 7 on numbered 5000 on, and junk in
	// front of the last: under these bounds the numbers are samples.
	laid, _ := l.stream(one(l.f(0), decoy), l.fs(1, 7), l.renumbered(7, n-1, 5000), one(junk(200<<10), l.as(n-1, n-1+5000, -1)))
	raw := withBounds(laid, 2048, 4096)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	last := int64(n-1) + 5000
	want := func(got []frame, from int) {
		t.Helper()
		if len(got) != 2 || got[0].pts != int64(from) || !bytes.Equal(got[0].data, l.bytesOf(from)) ||
			got[1].pts != last || got[1].lost != last-int64(from)-4096 {
			var at []int64
			for _, f := range got {
				at = append(at, f.pts)
			}
			t.Fatalf("read packets at %v, want frame %d and the last at %d, the hole between them owed", at, from, last)
		}
	}
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	want(got, 0)
	// A seek into the first frame's samples lands on a later frame that
	// holds them, as it always did, and the read goes on to the last frame.
	const target = 2556
	off, ok := d.BisectForTest(target)
	k := slices.IndexFunc(l.frames, func(f testutil.FLACFrame) bool { return int64(f.Off+len(decoy.b)) == off })
	if !ok || k < 2 || k > 5 {
		t.Fatalf("the bisection lands at %d (%v), frame %d: want one of frames 2 to 5, with a frame out of sequence behind it: this pins nothing", off, ok, k)
	}
	if landed, err := d.SeekSample(context.Background(), 0, target); err != nil || landed != int64(k) {
		t.Fatalf("a seek to %d landed at %d (%v), want frame %d", target, landed, err, k)
	}
	if got, err = readFrames(t, d); err != nil {
		t.Fatal(err)
	}
	want(got, k)
}

// TestAClosingFrameThatEndsBeforeTheFirstDoesIsNoExactLength: a whole frame
// at the end of the data whose number puts its end before the first frame's
// is out of sequence to every read, which ends behind the first frame. The
// open states where it ends and does not call that exact, so a decode is not
// cut there.
func TestAClosingFrameThatEndsBeforeTheFirstDoesIsNoExactLength(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	// Sample numbers: the first frame at 0, bytes of no frame, and the short
	// last frame at 3.
	first := testutil.SetFLACFrameNumber(t, l.bytesOf(0), true, 0)
	closing := testutil.SetFLACFrameNumber(t, l.bytesOf(n-1), true, 3)
	raw := slices.Concat(l.whole[:l.frames[0].Off], first, bytes.Repeat([]byte{0x11}, 1021), closing)
	ends := 3 + l.frames[n-1].Dur
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != ends || tr.SamplesExact {
		t.Errorf("the open says %d samples, exact %v; want %d and not exact", tr.Samples, tr.SamplesExact, ends)
	}
	if ws := d.Warnings(); !slices.ContainsFunc(ws, func(w container.Warning) bool {
		return w.Kind == container.Note && strings.Contains(w.Msg, "before the frame in front of it does")
	}) {
		t.Errorf("warnings %v, want the closing frame noted", ws)
	}
	got, err := readFrames(t, d)
	if err != nil || len(got) != 1 || got[0].pts != 0 || got[0].dur != 4096 {
		t.Fatalf("read %d packets (%v), want the first frame alone", len(got), err)
	}
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if c := decodeRest(t, med); c != 4096 {
		t.Errorf("a decode delivered %d samples, want the first frame's 4096", c)
	}
	// Behind a hole at the head that the first frame read does not fill, the
	// same: the closing frame ends inside that frame.
	holed := slices.Concat(l.whole[:l.frames[0].Off], bytes.Repeat([]byte{0x11}, 1021),
		testutil.SetFLACFrameNumber(t, l.bytesOf(1), true, 3000), bytes.Repeat([]byte{0x11}, 1021),
		testutil.SetFLACFrameNumber(t, l.bytesOf(n-1), true, 3003))
	tr, got, _ := reads(t, holed)
	if tr.Samples != 3003+l.frames[n-1].Dur || tr.SamplesExact || len(got) != 1 || got[0].pts != 3000 || got[0].lost != 3000 {
		t.Errorf("behind a hole at the head: %d samples, exact %v, %d packets; want %d, not exact, and the frame at 3000 owing the hole",
			tr.Samples, tr.SamplesExact, len(got), 3003+l.frames[n-1].Dur)
	}
	// A stream of one frame closes with its first frame, exactly.
	single, err := flacn.NewDemuxer(container.BytesSource(raw[:l.frames[0].Off+len(first)]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := single.Tracks()[0]; tr.Samples != 4096 || !tr.SamplesExact {
		t.Errorf("one frame: the open says %d samples, exact %v; want 4096 exact", tr.Samples, tr.SamplesExact)
	}
	// Behind a damaged head the open still takes such a closing frame's end
	// to tell that the first frame read is numbered past it: the stream
	// starts at that frame, with no hole from 0.
	whole := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, whole)
	lastf := frames[len(frames)-1]
	late := slices.Concat(headless(strayBehindAHole(t, whole, 0), frames[0].Off)[:frames[3].Off],
		testutil.SetFLACFrameNumber(t, whole[lastf.Off:lastf.End], false, 0))
	tr, got, d = reads(t, late)
	if tr.SamplesExact || len(got) != 2 || got[0].pts != 0 || got[0].lost != 0 {
		t.Errorf("behind a damaged head: exact %v, %d packets, the first owing %d; want the two frames from 0, nothing owed, no exact length", tr.SamplesExact, len(got), got[0].lost)
	}
	if msgs := strings.Join(damageOf(d), "; "); strings.Contains(msgs, "samples lost") {
		t.Errorf("behind a damaged head: findings %q, want no hole", msgs)
	}
}

// TestAClosingFrameIsExactOnlyWhereTheFrameInFrontCarriesOnToIt: the open
// takes the stream's end from the whole frame at the end of the data. One
// numbered before the successor of the whole frame in front of it lies
// inside the frames a read delivers and is out of sequence to it, so the
// open states where it ends and does not call that exact. One numbered on,
// or past a gap the read counts lost, closes the stream as it did.
func TestAClosingFrameIsExactOnlyWhereTheFrameInFrontCarriesOnToIt(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	short := l.frames[n-1].Dur
	for _, tc := range []struct {
		name   string
		coded  int // the short last frame's number, behind frames 0 to 9
		exact  bool
		packet int // the packets a read delivers
	}{
		{"numbered inside frame 7", 7, false, 10},
		{"numbered inside the last whole frame", 9, false, 10},
		{"numbered on", 10, true, 11},
		{"numbered past a gap", 12, true, 11},
		// The frames in front are the ones out of place: a read comes to
		// the closing frame past them, from the frames further back.
		{"numbered on from before two frames numbered far ahead", 11, true, 11},
	} {
		front := l.fs(0, 10)
		if strings.Contains(tc.name, "far ahead") {
			front = slices.Concat(l.fs(0, 8), one(l.as(8, 208, -1), l.as(9, 209, -1)))
		}
		raw, _ := l.stream(front, one(l.as(n-1, tc.coded, -1)))
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		ends := int64(tc.coded)*4096 + short
		if tr := d.Tracks()[0]; tr.Samples != ends || tr.SamplesExact != tc.exact {
			t.Errorf("%s: the open says %d samples, exact %v; want %d, exact %v", tc.name, tr.Samples, tr.SamplesExact, ends, tc.exact)
		}
		noted := slices.ContainsFunc(d.Warnings(), func(w container.Warning) bool {
			return w.Kind == container.Note && strings.Contains(w.Msg, "the length is not exact")
		})
		if noted == tc.exact {
			t.Errorf("%s: warnings %v, noted %v", tc.name, d.Warnings(), noted)
		}
		got, err := readFrames(t, d)
		if err != nil || len(got) != tc.packet {
			t.Fatalf("%s: read %d packets (%v), want %d", tc.name, len(got), err, tc.packet)
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
			t.Errorf("%s: a decode delivered %d samples, want the ten frames' %d", tc.name, c, 10*4096)
		}
		med.Close()
	}
}
