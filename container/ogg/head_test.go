package ogg

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// readTimed reads d to its end and returns where each packet lies.
func readTimed(t *testing.T, d *Demuxer) []timed {
	t.Helper()
	var got []timed
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("the read ended %v", err)
		}
		got = append(got, timed{pkt.PTS, pkt.Dur, pkt.Lost})
	}
}

// TestAnOggFLACStrayHeadIsPlacedFromZero: frames joined on at the head are
// placed from sample 0, as a stream starting late is, and the stream's own
// frames follow at their numbers, so no position runs back behind them. A
// seek lands on the packet a read from there delivers first.
func TestAnOggFLACStrayHeadIsPlacedFromZero(t *testing.T) {
	const bs = 4096
	for name, numbers := range map[string][]uint64{
		"numbered past the length":         {0, 51, 52, 3, 4, 5, 6, 7, 8, 9},
		"numbered past the frames behind":  {0, 501, 502, 3, 4, 5, 6, 7, 8, 9},
		"one frame":                        {0, 77, 2, 3, 4, 5, 6, 7, 8, 9},
		"a swapped pair behind the strays": {0, 51, 52, 4, 3, 5, 6, 7, 8, 9},
	} {
		raw := testutil.DamageOggPage(t, oggFLACFrames(t, 10*bs, numbers...), 2)
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		exactAt(t, d, 10*bs)
		got := readTimed(t, d)
		// Nine frames, the strays from 0, and the frame the damage took owed
		// in front of the first frame at its own number.
		strays := 2
		if name == "one frame" {
			strays = 1
		}
		var want []timed
		for i := range 9 {
			at, lost := int64(i+1)*bs, int64(0)
			if i < strays {
				at = int64(i) * bs
			} else if i == strays {
				lost = bs
			}
			want = append(want, timed{at, bs, lost})
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: read %+v, want %+v", name, got, want)
			continue
		}
		exactAt(t, d, 10*bs)
		for _, target := range []int64{0, 100, bs, 2*bs - 1, 2 * bs, 3*bs - 1, 3 * bs, 5*bs + 7, 10*bs - 1, 1 << 40} {
			landed, err := d.SeekSample(context.Background(), 0, target)
			if err != nil || landed > min(target, 10*bs-1) {
				t.Errorf("%s: a seek to %d landed at %d, %v", name, target, landed, err)
				continue
			}
			// The landing is a packet of the read's, the hole in front of
			// it behind the seek unless the seek landed in it.
			behind := readTimed(t, d)
			i := slices.IndexFunc(want, func(w timed) bool { return len(behind) > 0 && w.pts == behind[0].pts })
			if i < 0 || behind[0].pts-behind[0].lost != landed || behind[0].lost != 0 || !slices.Equal(behind[1:], want[i+1:]) {
				t.Errorf("%s: a seek to %d landed at %d and read %+v, want the read's packets from there", name, target, landed, behind)
			}
		}
	}
}

// TestAnOggFLACSwappedFirstPairIsReadInOrder: behind damage at the head,
// the first two frames the other way round are read in order, the hole in
// front of the earlier one, from the open and from a seek into the hole.
func TestAnOggFLACSwappedFirstPairIsReadInOrder(t *testing.T) {
	const bs = 4096
	for name, tc := range map[string]struct {
		numbers []uint64
		want    []timed
		took    string
	}{
		"a frame lost in front": {
			[]uint64{0, 2, 1, 3, 4, 5, 6, 7, 8, 9},
			[]timed{{bs, bs, bs}, {2 * bs, bs, 0}, {3 * bs, bs, 0}},
			"damage before the first frame took 4096 samples",
		},
		"two frames lost in front": {
			[]uint64{0, 3, 2, 4, 5, 6, 7, 8, 9},
			[]timed{{2 * bs, bs, 2 * bs}, {3 * bs, bs, 0}, {4 * bs, bs, 0}},
			"damage before the first frame took 8192 samples",
		},
		"not a pair": {
			[]uint64{0, 3, 1, 4, 5, 6, 7, 8, 9},
			[]timed{{3 * bs, bs, 3 * bs}, {bs, bs, 0}},
			"damage before the first frame took 12288 samples",
		},
	} {
		raw := testutil.DamageOggPage(t, oggFLACFrames(t, 10*bs, tc.numbers...), 2)
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		exactAt(t, d, 10*bs)
		for pass := range 2 {
			got := readTimed(t, d)
			if len(got) != len(tc.numbers)-1 || !slices.Equal(got[:len(tc.want)], tc.want) {
				t.Fatalf("%s: pass %d read %+v, want it to begin %+v", name, pass, got, tc.want)
			}
			if landed, err := d.SeekSample(context.Background(), 0, 100); err != nil || landed != 0 {
				t.Fatalf("%s: a seek to 100 landed at %d (%v), want the hole's start", name, landed, err)
			}
		}
		if msgs := damageList(d); !strings.Contains(msgs, tc.took) || strings.Contains(msgs, "read in order") != (name != "not a pair") {
			t.Errorf("%s: findings %q, want %q and the pair named where there is one", name, msgs, tc.took)
		}
	}
}

// TestAnOggFLACSeekProbeInsideAStrayHeadWalksFromTheFirstFrame: a stray
// head's frames are placed by a walk from the first frame alone. A seek
// whose bisection lands inside a run longer than its window walks from
// there, and lands on the packet a read from the open has at that place.
func TestAnOggFLACSeekProbeInsideAStrayHeadWalksFromTheFirstFrame(t *testing.T) {
	const bs, strays, own = 4096, 9000, 6000
	numbers := []uint64{0}
	for i := range strays {
		numbers = append(numbers, uint64(4000+i))
	}
	for i := range own {
		numbers = append(numbers, uint64(3+i))
	}
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, (own+3)*bs, numbers...), 2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	type read struct {
		pts  int64
		data string
	}
	var open []read
	var pkt container.Packet
	for d.ReadPacket(&pkt) == nil {
		open = append(open, read{pkt.PTS, string(pkt.Data)})
	}
	if len(open) != strays+own || open[strays-1].pts != (strays-1)*bs || open[strays].pts != 3*bs {
		t.Fatalf("the read placed %d packets, the last stray at %d and the frame behind it at %d: this pins nothing", len(open), open[strays-1].pts, open[strays].pts)
	}
	lo, err := d.bisectFrames(t.Context(), 5900*bs)
	if end, _ := d.strayRunEnd(t.Context()); err != nil || lo <= d.firstData || lo > end {
		t.Fatalf("the bisection for frame 5900 found %d (%v), want a page inside the stray run, which ends at %d: this pins nothing", lo, err, end)
	}
	for _, target := range []int64{5900 * bs, 5900*bs + 77, 5999 * bs} {
		landed, err := d.SeekSample(context.Background(), 0, target)
		if err != nil || landed != target/bs*bs {
			t.Fatalf("a seek to %d landed at %d (%v), want the frame holding it", target, landed, err)
		}
		if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != landed || string(pkt.Data) != open[landed/bs].data {
			t.Errorf("behind a seek to %d the read delivered the packet at %d (%v), want the one a read from the open has at %d", target, pkt.PTS, err, landed)
		}
	}
}

// TestAnOggFLACSwappedFirstPairWithNoDamageIsReadInOrder: a stream whose
// first two frames lie the other way round, nothing damaged in front,
// starts at the earlier one. Both are read, in order, with nothing lost,
// from the open and from a seek, wherever the stream's numbers start.
func TestAnOggFLACSwappedFirstPairWithNoDamageIsReadInOrder(t *testing.T) {
	const bs = 4096
	for name, numbers := range map[string][]uint64{
		"from frame 0":   {1, 0, 2, 3, 4, 5, 6, 7, 8, 9},
		"from frame 500": {501, 500, 502, 503, 504, 505},
		"the pair alone": {1, 0},
	} {
		n := int64(len(numbers))
		d, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, n*bs, numbers...)), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var want []timed
		for i := range n {
			want = append(want, timed{i * bs, bs, 0})
		}
		for _, target := range []int64{-1, 0, 100, bs + 5, 3 * bs} {
			from := int64(0)
			if target >= d.stated {
				continue // the length is the last packet's end, which a lone pair's is short of
			}
			if target >= 0 {
				landed, err := d.SeekSample(context.Background(), 0, target)
				if err != nil || landed != target/bs*bs {
					t.Fatalf("%s: a seek to %d landed at %d (%v), want the frame holding it", name, target, landed, err)
				}
				from = landed / bs
			}
			var got []timed
			var pkt container.Packet
			for d.ReadPacket(&pkt) == nil {
				got = append(got, timed{pkt.PTS, pkt.Dur, pkt.Lost})
				if want := buildFrameBytes(t, numbers[0]-1+uint64(pkt.PTS/bs), bs); string(pkt.Data) != string(want) {
					t.Errorf("%s: the packet at %d is not its frame's bytes", name, pkt.PTS)
				}
			}
			if !slices.Equal(got, want[from:]) {
				t.Errorf("%s: behind a seek to %d (-1 the open) read %+v, want %+v", name, target, got, want[from:])
			}
		}
		if msgs := damageList(d); !strings.Contains(msgs, "read in order") || strings.Contains(msgs, "dropped") || strings.Contains(msgs, "lost") {
			t.Errorf("%s: findings %q, want the pair read in order and nothing dropped or lost", name, msgs)
		}
	}
}

// TestAnOggFLACStrayHeadIsFoundHoweverFarItsRunHidesTheFramesBelow: a first
// frame behind damage whose run is longer than the open's tail window, the
// stream's last frames numbered above it, is still a stray: the open looks
// through the run, as far as a resync reaches, for the frame numbered
// before it, and the frames numbered below it are read, not dropped.
func TestAnOggFLACStrayHeadIsFoundHoweverFarItsRunHidesTheFramesBelow(t *testing.T) {
	const bs, strays, own = 4096, 9000, 6000
	numbers := []uint64{0}
	for i := range strays {
		numbers = append(numbers, uint64(500+i))
	}
	for i := range own {
		numbers = append(numbers, uint64(3+i))
	}
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, (own+3)*bs, numbers...), 2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := readTimed(t, d)
	if len(got) != strays+own || got[0].pts != 0 || got[strays-1].pts != (strays-1)*bs || got[strays].pts != 3*bs || got[len(got)-1].pts != (own+2)*bs {
		t.Fatalf("read %d packets, want all %d: the strays from 0 and the stream's frames at their numbers", len(got), strays+own)
	}
	if msgs := damageList(d); strings.Contains(msgs, "is not a valid") {
		t.Errorf("findings %q: frames of the stream were dropped", msgs)
	}
}

// lookedAhead is every field of d as text, the window and the log aside:
// what a look-ahead and a canceled seek leave as it was. A field added to
// the demuxer is in it unless it is named here.
func lookedAhead(d *Demuxer) map[string]string {
	apart := map[string]string{
		"win": "the bytes read stay read",
		"log": "its findings are rolled back by the caller, not by the field",
	}
	out := map[string]string{}
	v := reflect.ValueOf(d).Elem()
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if _, ok := apart[name]; ok {
			continue
		}
		f := v.Field(i)
		out[name] = fmt.Sprintf("%#v", reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface())
	}
	return out
}

// moved names the fields that differ between two lookedAhead answers.
func moved(a, b map[string]string) (names []string) {
	for name := range a {
		if a[name] != b[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// TestAnOggLookAheadLeavesEveryFieldAsItWas: the three look-aheads read on
// from where the read stands and put every field of the demuxer back, the
// ones a read rewrites in place too. The read they run in between moves
// those fields, or this pins nothing.
func TestAnOggLookAheadLeavesEveryFieldAsItWas(t *testing.T) {
	const bs = 4096
	numbers := make([]uint64, 300)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	for name, tc := range map[string]struct {
		raw   []byte
		moves []string // the fields the read inside a look-ahead moves
	}{
		"flac, a packet a page":  {oggFLACFrames(t, 300*bs, numbers...), []string{"off", "page", "pktPage", "lookLimit"}},
		"opus, packets on pages": {fixtureLocal(t, "golden-opus.opus"), []string{"off", "page", "seg", "segOff", "pktPage", "lookLimit"}},
	} {
		open := func() *Demuxer {
			d, err := NewDemuxer(container.BytesSource(tc.raw), nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var pkt container.Packet
			for range 5 {
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			return d
		}
		d := open()
		was := lookedAhead(d)
		var during map[string]string
		looks := map[string]func(){
			"lookAhead": func() {
				d.lookAhead(lookAheadBytes, func() {
					for range 40 {
						d.nextPacket()
					}
					during = lookedAhead(d)
				})
			},
			"nextGranulePage": func() { d.nextGranulePage() },
		}
		if fm, ok := d.mapping.(*flacMapping); ok {
			looks["frameAhead"] = func() { d.frameAhead() }
			looks["lowerAhead"] = func() { d.lowerAhead(fm) }
			looks["dueBefore"] = func() { d.dueBefore(0, 6*bs) }
		}
		for look, run := range looks {
			run()
			if names := moved(was, lookedAhead(d)); len(names) > 0 {
				t.Errorf("%s: %s left %v other than they were", name, look, names)
			}
		}
		for _, field := range tc.moves {
			if during[field] == was[field] {
				t.Errorf("%s: the read inside a look-ahead left %s as it was: this pins nothing of it", name, field)
			}
		}
		// And the read goes on as one that never looked.
		if got, want := readTimed(t, d), readTimed(t, open()); !slices.Equal(got, want) {
			t.Errorf("%s: behind the look-aheads the read delivered %d packets, a read that never looked %d", name, len(got), len(want))
		}
	}
}

// TestAnOggSeekCanceledOnTheWayLeavesTheReadWhereItStood: a seek reads its
// context at every probe and every packet it walks. One whose context ends
// on the way returns the walk's cancel and leaves the read as it stood, the
// mapping's timing with it, and the same seek with a live context lands
// where it always did.
func TestAnOggSeekCanceledOnTheWayLeavesTheReadWhereItStood(t *testing.T) {
	const bs = 4096
	numbers := make([]uint64, 300)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	type read struct {
		pts, dur, lost int64
		data           string
	}
	rest := func(d *Demuxer) (out []read) {
		var pkt container.Packet
		for d.ReadPacket(&pkt) == nil {
			out = append(out, read{pkt.PTS, pkt.Dur, pkt.Lost, string(pkt.Data)})
		}
		return out
	}
	swapped := slices.Clone(numbers)
	swapped[40], swapped[41] = 41, 40
	stray := slices.Concat([]uint64{0, 4000, 4001, 4002}, numbers[3:])
	spanning, _ := repaginate(t, "noise-s24.oga", true)
	for name, raw := range map[string][]byte{
		"flac":                    oggFLACFrames(t, 300*bs, numbers...),
		"flac, a swapped pair":    oggFLACFrames(t, 300*bs, swapped...),
		"flac, a lost page":       testutil.DamageOggPage(t, oggFLACFrames(t, 300*bs, numbers...), 42),
		"flac, a stray head":      testutil.DamageOggPage(t, oggFLACFrames(t, 300*bs, stray...), 2),
		"flac, a swapped head":    oggFLACFrames(t, 300*bs, slices.Concat([]uint64{1, 0}, numbers[2:])...),
		"vorbis":                  fixtureLocal(t, "golden-vorbis.ogg"),
		"opus":                    fixtureLocal(t, "golden-opus.opus"),
		"opus, a lost page":       testutil.DamageOggPage(t, fixtureLocal(t, "golden-opus.opus"), 9),
		"flac, large pages":       fixture(t, "noise-s24.oga"),
		"flac, packets in parts":  spanning,
		"flac, a seek at the top": oggFLACFrames(t, 300*bs, numbers...),
	} {
		open := func() *Demuxer {
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var pkt container.Packet
			for range 3 {
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			return d
		}
		targets := []int64{open().Tracks()[0].Samples * 3 / 4, 40*bs + 5, 41*bs + 5}
		if strings.Contains(name, "top") || strings.Contains(name, "head") {
			targets = []int64{0, 100, bs + 5}
		}
		for _, target := range targets {
			live := open()
			if target >= live.Tracks()[0].Samples {
				continue
			}
			want, err := live.SeekSample(t.Context(), 0, target)
			if err != nil {
				t.Fatalf("%s: a seek to %d: %v", name, target, err)
			}
			listed := len(live.Warnings())
			behind, plain := rest(live), rest(open())
			canceled := 0
			for checks := 1; ; checks++ {
				d := open()
				was, findings := lookedAhead(d), len(d.Warnings())
				ctx := testutil.CancelAfterChecks(t, checks)
				landed, err := d.SeekSample(ctx, 0, target)
				if err == nil {
					// Whether or not the context outlived it, the seek is a
					// live one's.
					if n := len(d.Warnings()); landed != want || n != listed || !slices.Equal(rest(d), behind) {
						t.Errorf("%s: a seek to %d whose context ends on check %d returned no error, landed at %d with %d findings; want %d, %d and the same packets behind it",
							name, target, checks, landed, n, want, listed)
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
				if checks > 3 && checks%7 != 0 {
					continue // every step is canceled; a few are read behind
				}
				if names := moved(was, lookedAhead(d)); len(names) > 0 || len(d.Warnings()) != findings {
					t.Errorf("%s: a seek to %d canceled on check %d left %v other than they were, findings %v", name, target, checks, names, d.Warnings())
				}
				if got := rest(d); !slices.Equal(got, plain) {
					t.Errorf("%s: behind a seek to %d canceled on check %d the read delivered %d packets, want the %d a read with no seek does, each as it was", name, target, checks, len(got), len(plain))
				}
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

// TestAnOggSeekCopiesNothingToBeCanceled: a seek can be canceled and put
// the read back as it stood without copying the page or the packets it
// holds. A healthy seek allocates what its walk does and no more.
func TestAnOggSeekCopiesNothingToBeCanceled(t *testing.T) {
	for name, raw := range map[string][]byte{
		"flac, large pages": fixture(t, "noise-s24.oga"),
		"opus":              fixtureLocal(t, "golden-opus.opus"),
	} {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		var pkt container.Packet
		for range 3 {
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
		}
		held := int64(len(d.page.body) + len(d.page.lacing) + len(d.pending))
		target := d.Tracks()[0].Samples / 2
		spent := func(seek func()) int64 {
			seek() // the buffers a first seek grows
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 50 {
				seek()
			}
			runtime.ReadMemStats(&after)
			return int64(after.TotalAlloc-before.TotalAlloc) / 50
		}
		walk := spent(func() {
			if d.mapping.selfTiming() {
				d.seekSelfTiming(t.Context(), target)
			} else {
				d.seekAccumulating(t.Context(), target)
			}
		})
		seek := spent(func() { d.SeekSample(t.Context(), 0, target) })
		t.Logf("%s: a seek allocates %d bytes, its walk %d; the read held %d", name, seek, walk, held)
		if held < 1024 {
			t.Fatalf("%s: the read holds %d bytes of page and packet: this pins nothing", name, held)
		}
		if seek > walk+held/8 {
			t.Errorf("%s: a seek allocates %d bytes and its walk %d, with %d bytes of page and packet held: the seek copied them", name, seek, walk, held)
		}
	}
}

// TestAnOggSeekCanceledBeforeItsFirstProbeReadsNothing: a seek whose
// context has ended runs no probe: on a stream long enough to bisect it
// reads no byte.
func TestAnOggSeekCanceledBeforeItsFirstProbeReadsNothing(t *testing.T) {
	const bs = 4096
	numbers := make([]uint64, 8000)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	raw := oggFLACFrames(t, 8000*bs, numbers...)
	if len(raw) <= 2*seekWindow {
		t.Fatalf("%d bytes, want a stream long enough to bisect", len(raw))
	}
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 1), 0, 6000*bs); waxerr.CodeOf(err) != waxerr.CodeCanceled || src.Reads != 0 {
		t.Errorf("a seek with an ended context returned %v after %d reads, want the cancel and none", err, src.Reads)
	}
	if landed, err := d.SeekSample(t.Context(), 0, 6000*bs); err != nil || landed != 6000*bs || src.Reads == 0 {
		t.Fatalf("the seek again landed at %d (%v) in %d reads, want the frame and its probes read: this pins nothing", landed, err, src.Reads)
	}
}
