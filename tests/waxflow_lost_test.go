package waxflow_test

// The samples a read delivers as silence where damage took the audio are
// counted where they are delivered (format.Info.Lost), through every Media
// that wraps the read and onto the engine's results.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// holedFLAC is a native FLAC of the given length with the named frames
// damaged, and the samples each one held: the holes a read fills.
func holedFLAC(t *testing.T, seconds int, pick func(n int) []int) (raw []byte, holes [][2]int64) {
	t.Helper()
	raw = transcodeBytes(t, loopedWAV(t, seconds), waxflow.TranscodeOptions{Format: "flac"})
	frames := testutil.FLACFrames(t, raw)
	ks := pick(len(frames))
	for _, k := range ks {
		if k >= len(frames)-1 {
			t.Fatalf("frame %d of %d: a damaged last frame is a dropped tail, not a hole", k, len(frames))
		}
		holes = append(holes, [2]int64{frames[k].Pos, frames[k].Pos + frames[k].Dur})
	}
	return testutil.DamageFLACFrames(t, raw, frames, ks...), holes
}

// middleFrame names the frame in the middle of n.
func middleFrame(n int) []int { return []int{n / 2} }

// drain reads med to its end and returns the samples it delivered.
func drain(t *testing.T, med format.Media) int64 {
	t.Helper()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n += int64(buf.N)
	}
}

// TestSliceCountsTheFillInItsWindow: a span's Lost is the silence inside its
// own window, whichever edge of it cuts the hole, and none for a hole the
// window leaves out.
func TestSliceCountsTheFillInItsWindow(t *testing.T) {
	raw, holes := holedFLAC(t, 10, middleFrame)
	lo, hi := holes[0][0], holes[0][1]
	for _, c := range []struct {
		name     string
		from, to int64
		want     int64
	}{
		{"inside the hole", lo + 100, hi - 200, hi - lo - 300},
		{"ends in the hole", lo - 5000, lo + 1000, 1000},
		{"starts in the hole", lo + 1000, waxflow.ToEnd, hi - lo - 1000},
		{"around the hole", lo - 5000, hi + 5000, hi - lo},
		{"before the hole", 0, lo, 0},
		{"after the hole", hi, waxflow.ToEnd, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			med, err := format.Open(container.BytesSource(raw), "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			sl, err := waxflow.Slice(med, c.from, c.to)
			if err != nil {
				med.Close()
				t.Fatal(err)
			}
			defer sl.Close()
			drain(t, sl)
			if got := sl.Info().Lost; got != c.want {
				t.Fatalf("Lost is %d over [%d, %d) with the hole at [%d, %d), want %d", got, c.from, c.to, lo, hi, c.want)
			}
		})
	}
}

// TestSliceHeadroomIsNotItsWindow: a hole a consumer reads through on its way
// to the window's sample 0, priming from the headroom, is not in the window.
func TestSliceHeadroomIsNotItsWindow(t *testing.T) {
	raw, holes := holedFLAC(t, 10, middleFrame)
	lo, hi := holes[0][0], holes[0][1]
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	const into = 700 // the window opens this far before the hole ends
	sl, err := waxflow.Slice(med, hi-into, waxflow.ToEnd)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()
	if _, err := sl.SeekSample(context.Background(), -(hi - into - lo + 300)); err != nil {
		t.Fatal(err)
	}
	drain(t, sl)
	if got := sl.Info().Lost; got != into {
		t.Fatalf("Lost is %d, want the %d samples of the hole inside the window", got, into)
	}
}

// flacMember wires raw, a native FLAC, as a timeline member.
func flacMember(t *testing.T, e *waxflow.Engine, raw []byte) waxflow.ConcatSource {
	t.Helper()
	info, err := e.Probe(context.Background(), container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	return waxflow.ConcatSource{
		Track: info.Default(),
		Open:  func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), "flac") },
	}
}

// TestMeasuredMediaReportsItsSourcesLost: a Media that only restates its
// source's length delivers its source's reads, fill included.
func TestMeasuredMediaReportsItsSourcesLost(t *testing.T) {
	raw, holes := holedFLAC(t, 10, middleFrame)
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	measured := med.Info().Default()
	measured.SamplesCounted = true // a different claim, so the wrapper is built
	wrapped := waxflow.MeasuredMedia(med, measured)
	defer wrapped.Close()
	if wrapped == med {
		t.Fatal("MeasuredMedia returned its source; this cell needs the wrapper")
	}
	drain(t, wrapped)
	if got, want := wrapped.Info().Lost, holes[0][1]-holes[0][0]; got != want {
		t.Fatalf("Lost is %d through the wrapper, want the hole's %d", got, want)
	}
}

// TestConcatSumsItsMembersLost: a timeline's Lost is its members', the
// finished ones' and the open one's as its read goes.
func TestConcatSumsItsMembersLost(t *testing.T) {
	e := waxflow.New()
	a, ha := holedFLAC(t, 10, middleFrame)
	b, hb := holedFLAC(t, 10, func(n int) []int { return []int{n / 4, 3 * n / 4} })
	ms := []waxflow.ConcatSource{flacMember(t, e, a), flacMember(t, e, b)}
	med, err := waxflow.Concat(ms, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var pos int64
	reported := false
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pos += int64(buf.N)
		// Past the first member's hole and still inside that member.
		if !reported && pos >= ha[0][1] && pos < ms[0].Track.Samples {
			reported = true
			if got, want := med.Info().Lost, ha[0][1]-ha[0][0]; got != want {
				t.Fatalf("Lost is %d with the first member open past its hole, want %d", got, want)
			}
		}
	}
	if !reported {
		t.Fatal("no read ended between the first member's hole and its end")
	}
	want := ha[0][1] - ha[0][0] + hb[0][1] - hb[0][0] + hb[1][1] - hb[1][0]
	if got := med.Info().Lost; got != want {
		t.Fatalf("Lost is %d at the end, want the three holes' %d", got, want)
	}
}

// TestConcatScalesLostToItsRate: a member the timeline resamples lost its
// samples at its own rate, and the timeline counts them at the envelope's.
func TestConcatScalesLostToItsRate(t *testing.T) {
	e := waxflow.New()
	a, ha := holedFLAC(t, 10, middleFrame)
	am := flacMember(t, e, a)
	rate := am.Track.Fmt.Rate
	b, _ := ratedWAV(t, 2*rate+rate/3, am.Track.Fmt.Channels, 5000, 9)
	ms := append([]waxflow.ConcatSource{am}, timelineMembers(t, e, b)...)
	med, err := waxflow.Concat(ms, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	env := med.Info().Default().Fmt.Rate
	if env == rate {
		t.Fatalf("the envelope runs at the FLAC member's own %d Hz; this cell needs it resampled", rate)
	}
	drain(t, med)
	lost := ha[0][1] - ha[0][0]
	want := (lost*int64(env) + int64(rate)/2) / int64(rate)
	if got := med.Info().Lost; got != want {
		t.Fatalf("Lost is %d, want %d: the member's %d samples at %d Hz, on the envelope's %d Hz", got, want, lost, rate, env)
	}
}

// TestAnalyzeReportsLostSamples: Analyze closes the source before it returns,
// so the fill its read delivered comes back on the result, beside Samples and
// at the same rate.
func TestAnalyzeReportsLostSamples(t *testing.T) {
	raw, holes := holedFLAC(t, 10, func(n int) []int { return []int{n / 3, 2 * n / 3} })
	want := holes[0][1] - holes[0][0] + holes[1][1] - holes[1][0]
	e := waxflow.New()
	res, err := e.Analyze(context.Background(), container.BytesSource(raw), "flac", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.LostSamples != want {
		t.Fatalf("LostSamples is %d of %d measured, want the two holes' %d", res.LostSamples, res.Samples, want)
	}
	clean, _ := holedFLAC(t, 10, func(int) []int { return nil })
	res, err = e.Analyze(context.Background(), container.BytesSource(clean), "flac", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.LostSamples != 0 {
		t.Fatalf("LostSamples is %d for a file with no hole", res.LostSamples)
	}
}

// TestAnalyzeMediaReportsOnlyItsOwnRead: a Media handed in already read has
// delivered fill this measurement did not, and the result counts its own.
func TestAnalyzeMediaReportsOnlyItsOwnRead(t *testing.T) {
	raw, holes := holedFLAC(t, 10, middleFrame)
	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	drain(t, med)
	if _, err := med.SeekSample(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	res, err := e.AnalyzeMedia(context.Background(), med, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := holes[0][1] - holes[0][0]; res.LostSamples != want {
		t.Fatalf("LostSamples is %d, want this read's %d (the media's Info says %d)", res.LostSamples, want, med.Info().Lost)
	}
}

// TestAnalyzeGroupReportsLostSamples: each member's result carries its own
// fill and the group's the total, as its Samples is.
func TestAnalyzeGroupReportsLostSamples(t *testing.T) {
	a, ha := holedFLAC(t, 10, middleFrame)
	b, hb := holedFLAC(t, 10, func(n int) []int { return []int{n / 4, 3 * n / 4} })
	e := waxflow.New()
	open := func(raw []byte) func() (format.Media, error) {
		return func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), "flac") }
	}
	res, err := e.AnalyzeGroup(context.Background(),
		[]waxflow.GroupMember{{Open: open(a)}, {Open: open(b)}}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wa := ha[0][1] - ha[0][0]
	wb := hb[0][1] - hb[0][0] + hb[1][1] - hb[1][0]
	if got := res.Members[0].LostSamples; got != wa {
		t.Errorf("member 0 LostSamples is %d, want %d", got, wa)
	}
	if got := res.Members[1].LostSamples; got != wb {
		t.Errorf("member 1 LostSamples is %d, want %d", got, wb)
	}
	if got := res.Group.LostSamples; got != wa+wb {
		t.Errorf("the group's LostSamples is %d, want the members' %d", got, wa+wb)
	}
}

// TestTranscodeReportsLostSamples: a transcode's result counts the fill at
// the rate of the output it describes, so one that resamples scales it.
func TestTranscodeReportsLostSamples(t *testing.T) {
	raw, holes := holedFLAC(t, 10, middleFrame)
	lost := holes[0][1] - holes[0][0]
	e := waxflow.New()
	rate := probeTrack(t, raw, "flac").Fmt.Rate
	for _, c := range []struct {
		name string
		rate int
	}{{"at the source rate", 0}, {"resampled", 48000}, {"resampled down", 22050}} {
		t.Run(c.name, func(t *testing.T) {
			if c.rate == rate {
				t.Fatalf("the source already runs at %d Hz", rate)
			}
			var out bytes.Buffer
			res, err := e.Transcode(context.Background(), container.BytesSource(raw), "flac", &out,
				waxflow.TranscodeOptions{Format: "wav", Rate: c.rate})
			if err != nil {
				t.Fatal(err)
			}
			want := lost
			if c.rate != 0 {
				want = (lost*int64(c.rate) + int64(rate)/2) / int64(rate)
			}
			if res.LostSamples != want {
				t.Fatalf("LostSamples is %d of %d at %d Hz, want %d", res.LostSamples, res.Samples, res.Format.Rate, want)
			}
		})
	}
}

// TestCopyReportsTheSilenceItWrote: the copy rung carries a hole as silent
// frames, which are fill in its output as a decode's silence is.
func TestCopyReportsTheSilenceItWrote(t *testing.T) {
	raw, holes := holedFLAC(t, 10, func(n int) []int { return []int{n / 3, 2 * n / 3} })
	want := holes[0][1] - holes[0][0] + holes[1][1] - holes[1][0]
	for _, into := range []string{"", "ogg"} {
		t.Run("into "+into, func(t *testing.T) {
			var out bytes.Buffer
			res, err := waxflow.New().Remux(context.Background(), container.BytesSource(raw), "flac", &out,
				waxflow.TranscodeOptions{Format: "flac", Container: into})
			if err != nil {
				t.Fatal(err)
			}
			if res.LostSamples != want {
				t.Fatalf("LostSamples is %d of %d copied, want the two holes' %d", res.LostSamples, res.Samples, want)
			}
		})
	}
}

// TestOggFLACHolesAreCounted: an Ogg FLAC's holes are counted as a native
// one's are, the two shapes included whose warnings state no "samples lost"
// for a caller to read: a gap behind a damaged page, where the warning is the
// page's, and a first page lost to damage, a hole from sample 0.
func TestOggFLACHolesAreCounted(t *testing.T) {
	clean := transcodeBytes(t, loopedWAV(t, 20), waxflow.TranscodeOptions{Format: "flac", Container: "ogg"})
	pages := testutil.OggPages(t, clean)
	first := testutil.OggFirstAudioPage(t, clean, 2)
	whole := probeTrack(t, clean, "oga").Samples
	for _, c := range []struct {
		name string
		page int
	}{{"a page in the middle", (first + len(pages)) / 2}, {"the first audio page", first}} {
		t.Run(c.name, func(t *testing.T) {
			bad := testutil.DamageOggPage(t, clean, c.page)
			// What the packets say the holes took is the oracle.
			demux, _, err := format.OpenDemuxer(container.BytesSource(bad), "oga", nil)
			if err != nil {
				t.Fatal(err)
			}
			var pkt container.Packet
			var want int64
			for demux.ReadPacket(&pkt) == nil {
				want += pkt.Lost
			}
			if want == 0 {
				t.Fatal("the damaged page cost no samples; this cell needs a hole")
			}
			med, err := format.Open(container.BytesSource(bad), "oga", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			if n := drain(t, med); n != whole {
				t.Fatalf("read %d samples, want the whole stream's %d with the hole filled", n, whole)
			}
			info := med.Info()
			if info.Lost != want {
				t.Fatalf("Lost is %d, want the %d samples the packets say the hole took", info.Lost, want)
			}
			for _, w := range info.Warnings {
				if strings.Contains(w, "samples lost") {
					t.Fatalf("warning %q states the count; this cell is the shape whose wording does not", w)
				}
			}
		})
	}
}

// TestSliceLostCountsDeliveriesAcrossItsOwnSeeks: a span's count is of what
// it delivered, as a file's is, so a hole its window read twice is counted
// twice and one a seek carried it past is not counted again.
func TestSliceLostCountsDeliveriesAcrossItsOwnSeeks(t *testing.T) {
	raw, holes := holedFLAC(t, 10, middleFrame)
	lo, hi := holes[0][0], holes[0][1]
	const from = 20000
	open := func() format.Media {
		med, err := format.Open(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		sl, err := waxflow.Slice(med, from, hi+20000)
		if err != nil {
			med.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { sl.Close() })
		return sl
	}
	seek := func(sl format.Media, to int64) {
		t.Helper()
		if _, err := sl.SeekSample(context.Background(), to); err != nil {
			t.Fatal(err)
		}
	}

	twice := open()
	drain(t, twice)
	seek(twice, 0)
	drain(t, twice)
	if got, want := twice.Info().Lost, 2*(hi-lo); got != want {
		t.Errorf("Lost is %d after the window read its hole twice, want %d", got, want)
	}

	// Into the hole, then back to the window's start: the part of the hole
	// the first read delivered, and then all of it.
	back := open()
	seek(back, lo-from+1000)
	drain(t, back)
	seek(back, 0)
	drain(t, back)
	if got, want := back.Info().Lost, (hi-lo-1000)+(hi-lo); got != want {
		t.Errorf("Lost is %d after a read from inside the hole and one of the whole window, want %d", got, want)
	}

	// Past the hole from the start: the window never delivered it.
	past := open()
	seek(past, hi-from+10)
	drain(t, past)
	if got := past.Info().Lost; got != 0 {
		t.Errorf("Lost is %d for a window that only read past its hole", got)
	}
}

// TestSeekCountsNothing: a seek delivers nothing, so no Media's Lost moves in
// one, whatever it read to position itself. A timeline positions a member it
// resamples by reading a little through that member's chain, and a caller
// that takes the count before a seek and after it must see one number.
func TestSeekCountsNothing(t *testing.T) {
	e := waxflow.New()
	a, ha := holedFLAC(t, 10, middleFrame)
	am := flacMember(t, e, a)
	rate := am.Track.Fmt.Rate
	fast, _ := ratedWAV(t, 2*rate+rate/3, am.Track.Fmt.Channels, 5000, 9)
	open := map[string]func() format.Media{
		"a file": func() format.Media {
			med, err := format.Open(container.BytesSource(a), "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			return med
		},
		"a span": func() format.Media {
			med, err := format.Open(container.BytesSource(a), "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			sl, err := waxflow.Slice(med, 1000, waxflow.ToEnd)
			if err != nil {
				t.Fatal(err)
			}
			return sl
		},
		"a timeline": func() format.Media {
			med, err := waxflow.Concat([]waxflow.ConcatSource{am, flacMember(t, e, a)}, waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return med
		},
		"a timeline that resamples": func() format.Media {
			med, err := waxflow.Concat(append([]waxflow.ConcatSource{am}, timelineMembers(t, e, fast)...), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return med
		},
		"a span of a timeline that resamples": func() format.Media {
			med, err := waxflow.Concat(append([]waxflow.ConcatSource{am}, timelineMembers(t, e, fast)...), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sl, err := waxflow.Slice(med, 1000, waxflow.ToEnd)
			if err != nil {
				t.Fatal(err)
			}
			return sl
		},
	}
	for name, mk := range open {
		t.Run(name, func(t *testing.T) {
			med := mk()
			defer med.Close()
			scale := int64(med.Info().Default().Fmt.Rate)
			buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
			defer audio.Put(buf)
			// Every landing near the hole, on both sides of each edge, twice
			// over so a seek away from an open, half-read member is covered.
			for range 2 {
				for _, near := range []int64{ha[0][0], ha[0][1]} {
					for _, by := range []int64{-3000, -60, -20, -5, -1, 0, 1, 5, 20, 60, 3000} {
						before := med.Info().Lost
						if _, err := med.SeekSample(context.Background(), (near+by)*scale/int64(rate)); err != nil {
							t.Fatal(err)
						}
						if after := med.Info().Lost; after != before {
							t.Fatalf("a seek to %d samples from the hole's edge at %d moved Lost from %d to %d", by, near, before, after)
						}
						if err := med.ReadChunk(buf); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		})
	}
}

// failOnceDemuxer fails one ReadPacket and reads on after it, as a source
// read that fails once does.
type failOnceDemuxer struct {
	container.Demuxer
	reads, failAt int
}

func (d *failOnceDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.reads++; d.reads == d.failAt {
		return waxerr.New(waxerr.CodeSourceUnreadable, "a read that fails once")
	}
	return d.Demuxer.ReadPacket(pkt)
}

// TestFailedReadInsideTheFrontTrimKeepsItsPlace: a read that fails while the
// front trim is still being decoded and dropped has dropped part of it, and
// the read after it owes only the rest. Starting the trim over would drop
// that many samples of audio and deliver everything after them early.
func TestFailedReadInsideTheFrontTrimKeepsItsPlace(t *testing.T) {
	// HE-AAC primes with more than one access unit, which is what leaves a
	// read part way through the trim to fail in.
	raw := transcodeBytes(t, loopedWAV(t, 2), waxflow.TranscodeOptions{Format: "he-aac"})
	want, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(want)
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	var first container.Packet
	probe, _, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.ReadPacket(&first); err != nil {
		t.Fatal(err)
	}
	if tr := info.Default(); tr.Delay <= first.Dur {
		t.Fatalf("the priming is %d samples and a packet %d; this cell needs it to span more than one", tr.Delay, first.Dur)
	}
	// The second packet's read fails, with the first already decoded and
	// dropped as priming.
	med, err := format.FromDemuxer("mp4", &failOnceDemuxer{Demuxer: demux, failAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("the first read returned %v, want the failure", err)
	}
	got := drainMedia(t, med, want.N+1)
	defer audio.Put(got)
	if got.N != want.N {
		t.Fatalf("read %d samples after the failure, want the clean decode's %d", got.N, want.N)
	}
	if i := sameFrames(got, 0, want, 0, want.N); i >= 0 {
		t.Fatalf("the read after the failure differs from the clean decode at sample %d: the front trim started over", i)
	}
}
