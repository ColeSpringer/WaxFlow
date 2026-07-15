package waxflow_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/hls"
)

// wavFrom renders a buffer as a WAV, so a test can cut one continuous signal
// into several files and put it back together.
func wavFrom(t *testing.T, cfg pcm.Config, buf *audio.Buffer) []byte {
	t.Helper()
	enc, err := pcm.NewEncoder(cfg, buf.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(),
		Fmt: buf.Fmt, Samples: int64(buf.N), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return ws.b
}

// ratedWAV renders frames of synthetic PCM at rate as a WAV, plus the
// source-of-truth buffer.
func ratedWAV(t *testing.T, rate, channels, frames int, seed uint64) ([]byte, *audio.Buffer) {
	t.Helper()
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	buf := audio.Get(f, frames)
	buf.N = frames
	synth(buf, seed)
	return wavFrom(t, cfg, buf), buf
}

// members probes each WAV and wires it as a timeline member, the way a
// caller with a play queue does: plan from the headers, open on demand.
func timelineMembers(t *testing.T, e *waxflow.Engine, raws ...[]byte) []waxflow.ConcatSource {
	t.Helper()
	out := make([]waxflow.ConcatSource, len(raws))
	for i, raw := range raws {
		info, err := e.Probe(container.BytesSource(raw), "wav", nil)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = waxflow.ConcatSource{
			Track: info.Default(),
			Open:  func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), "wav") },
		}
	}
	return out
}

// drain reads a Media to end of stream into one buffer, failing if it
// delivers more than capacity frames.
func drainMedia(t *testing.T, med format.Media, capacity int) *audio.Buffer {
	t.Helper()
	f := med.Info().Default().Fmt
	out := audio.Get(f, capacity)
	out.N = 0
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.N+tmp.N > out.Cap() {
			t.Fatalf("the timeline delivered more than the %d frames it promised", capacity)
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
}

// TestConcatGaplessSeam is the primitive's headline claim, checked the only
// way that means anything: cut one continuous signal in two, hand the pieces
// back as a timeline, and require the result to be the original bit for bit.
// A seam that lost, repeated, or filtered a single sample fails here.
//
// The cut is deliberately not on a chunk boundary, so the seam falls inside
// what would otherwise be one read.
func TestConcatGaplessSeam(t *testing.T) {
	const frames, cut = 30000, 12345
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 91)

	head, tail := audio.Get(f, cut), audio.Get(f, frames-cut)
	defer audio.Put(head)
	defer audio.Put(tail)
	head.N, tail.N = cut, frames-cut
	audio.CopyFrames(head, 0, whole, 0, cut)
	audio.CopyFrames(tail, 0, whole, cut, frames-cut)

	e := waxflow.New()
	med, err := waxflow.Concat(timelineMembers(t, e, wavFrom(t, cfg, head), wavFrom(t, cfg, tail)), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	if got := med.Info().Default().Samples; got != frames {
		t.Fatalf("the timeline promises %d samples, want %d", got, frames)
	}
	got := drainMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, whole, got)
}

// TestConcatNoDiscontAtSeam pins the rule that makes the seam invisible to
// everything downstream. A member's own first chunk may be marked as a
// discontinuity; passing that through would drain the downstream resampler
// to end of stream and re-anchor it, which both puts back the seam this
// primitive exists to remove and changes the timeline's length from
// ceil((nA+nB)*L/M) to ceil(nA*L/M)+ceil(nB*L/M).
func TestConcatNoDiscontAtSeam(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 5)
	b, _ := ratedWAV(t, 48000, 2, 9000, 6)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	buf := audio.Get(med.Info().Default().Fmt, 1024)
	defer audio.Put(buf)
	for n := 0; ; n++ {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if buf.Discont {
			t.Fatalf("chunk %d at position %d is marked as a discontinuity; "+
				"a timeline read from the top is continuous everywhere", n, buf.Pos)
		}
	}
}

// TestConcatPositionsAreContinuous checks the other half of continuity: the
// positions themselves run 0..total with nothing skipped or repeated, which
// is what lets the segmenter treat tfdt as pure arithmetic on the index.
func TestConcatPositionsAreContinuous(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 5)
	b, _ := ratedWAV(t, 48000, 2, 4000, 6)
	c, _ := ratedWAV(t, 48000, 2, 7000, 7)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b, c), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	buf := audio.Get(med.Info().Default().Fmt, 1024)
	defer audio.Put(buf)
	var want int64
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if buf.Pos != want {
			t.Fatalf("chunk starts at %d, want %d", buf.Pos, want)
		}
		want += int64(buf.N)
	}
	if want != 20000 {
		t.Fatalf("the timeline delivered %d samples, want 20000", want)
	}
}

// TestConcatMixedRatesLength pins the arithmetic that the plan and the run
// must agree on for a mixed queue: each member is resampled on its own, so
// the timeline's length is the sum of the members' own exact output counts,
// not the count of the summed input.
func TestConcatMixedRatesLength(t *testing.T) {
	const n44, n48 = 44100, 48000
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, n44, 11)
	b, _ := ratedWAV(t, 48000, 2, n48, 12)
	ms := timelineMembers(t, e, a, b)

	env, err := waxflow.ConcatTrack([]container.Track{ms[0].Track, ms[1].Track})
	if err != nil {
		t.Fatal(err)
	}
	if env.Fmt.Rate != 48000 {
		t.Fatalf("envelope rate %d, want 48000 (the maximum, so no member loses information)", env.Fmt.Rate)
	}
	want := resample.OutputLen(n44, 44100, 48000) + int64(n48)
	if env.Samples != want {
		t.Fatalf("the timeline promises %d samples, want %d", env.Samples, want)
	}

	med, err := waxflow.Concat(ms, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got := drainMedia(t, med, int(want)+1)
	defer audio.Put(got)
	// The run has to deliver exactly what the headers promised: a plan that
	// over-promises is a tail 404, and one that under-promises is an early
	// ENDLIST.
	if int64(got.N) != want {
		t.Fatalf("the timeline delivered %d samples, its own track promised %d", got.N, want)
	}
}

// TestConcatLengthDrift pins the enforcement an advisory length forces. A
// declared total that the file does not deliver is a tolerated oddity for
// one file (format.Media says so) and fatal for a timeline: the drift
// desyncs the prefix sum and every position after it, so the playlist
// promises segments the stream cannot fill. The run fails instead, which is
// why a timeline's mint measures any member whose length is not exact.
func TestConcatLengthDrift(t *testing.T) {
	e := waxflow.New()
	raw, _ := ratedWAV(t, 48000, 2, 9000, 21)

	for _, tc := range []struct {
		name    string
		declare int64
	}{
		{"a member shorter than its headers claim", 9500},
		{"a member longer than its headers claim", 8500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := timelineMembers(t, e, raw, raw)
			ms[0].Track.Samples = tc.declare
			med, err := waxflow.Concat(ms, waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			f := med.Info().Default().Fmt
			buf := audio.Get(f, 1024)
			defer audio.Put(buf)
			for {
				err := med.ReadChunk(buf)
				if err == io.EOF {
					t.Fatal("the timeline ran to the end with a member that lied about its length")
				}
				if err != nil {
					return // the drift was caught, which is the point
				}
			}
		})
	}
}

// TestConcatSeekLandsExact walks a seek target across the seam, which is the
// one landing where the member switch and the seek happen at once.
//
// The grid includes the seam sample itself, not only its neighbours, and
// every landing is checked twice: that it reports the sample it was asked
// for, and that the audio from there is bit-exact against the unsplit
// original. The second is what stops a micro-click passing as "landed
// correctly".
func TestConcatSeekLandsExact(t *testing.T) {
	const frames, cut = 30000, 12345
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 77)

	head, tail := audio.Get(f, cut), audio.Get(f, frames-cut)
	defer audio.Put(head)
	defer audio.Put(tail)
	head.N, tail.N = cut, frames-cut
	audio.CopyFrames(head, 0, whole, 0, cut)
	audio.CopyFrames(tail, 0, whole, cut, frames-cut)
	rawA, rawB := wavFrom(t, cfg, head), wavFrom(t, cfg, tail)

	e := waxflow.New()
	for _, target := range []int64{0, 1, cut - 1, cut, cut + 1, cut + 4096, frames - 1} {
		t.Run("", func(t *testing.T) {
			med, err := waxflow.Concat(timelineMembers(t, e, rawA, rawB), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			landed, err := med.SeekSample(target)
			if err != nil {
				t.Fatal(err)
			}
			if landed != target {
				t.Fatalf("seek to %d landed at %d", target, landed)
			}
			rest := drainMedia(t, med, frames)
			defer audio.Put(rest)
			if int64(rest.N) != frames-target {
				t.Fatalf("after seeking to %d the timeline delivered %d samples, want %d",
					target, rest.N, frames-target)
			}
			want := audio.Get(f, int(frames-target))
			defer audio.Put(want)
			want.N = int(frames - target)
			audio.CopyFrames(want, 0, whole, int(target), want.N)
			equalPCM(t, want, rest)
		})
	}
}

// TestConcatSeekMarksDiscont pins where a discontinuity does belong: a seek
// is one, a seam is not, and the chunk after a seek must say so or the
// downstream chain will filter across a splice that really happened.
func TestConcatSeekMarksDiscont(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 31)
	b, _ := ratedWAV(t, 48000, 2, 9000, 32)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if _, err := med.SeekSample(9000); err != nil { // the seam sample itself
		t.Fatal(err)
	}
	buf := audio.Get(med.Info().Default().Fmt, 1024)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if !buf.Discont {
		t.Fatal("the chunk after a seek is not marked as a discontinuity")
	}
	if buf.Pos != 9000 {
		t.Fatalf("the chunk after seeking to the seam starts at %d, want 9000", buf.Pos)
	}
	if err := med.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if buf.Discont {
		t.Fatal("the discontinuity mark stuck to a second chunk")
	}
}

// TestConcatSeekMixedRatesLandsExact is the same landing guarantee on the
// path that has to work for it: a member behind a resampler, whose source
// timeline runs at another rate, so the seek maps back to it, lands at or
// before the target, and discards the slop.
func TestConcatSeekMixedRatesLandsExact(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, 44100, 41)
	b, _ := ratedWAV(t, 48000, 2, 48000, 42)
	seam := resample.OutputLen(44100, 44100, 48000)

	for _, target := range []int64{0, 5000, seam - 1, seam, seam + 1, seam + 24000} {
		t.Run("", func(t *testing.T) {
			med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			landed, err := med.SeekSample(target)
			if err != nil {
				t.Fatal(err)
			}
			if landed != target {
				t.Fatalf("seek to %d landed at %d", target, landed)
			}
			buf := audio.Get(med.Info().Default().Fmt, 1024)
			defer audio.Put(buf)
			if err := med.ReadChunk(buf); err != nil {
				t.Fatal(err)
			}
			if buf.Pos != target {
				t.Fatalf("after seeking to %d the next chunk starts at %d", target, buf.Pos)
			}
		})
	}
}

// TestConcatSeekPastEnd pins the end-of-stream landing a single Media gives,
// which is what the exact-length walk (measureSamples) rides on.
func TestConcatSeekPastEnd(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 51)
	b, _ := ratedWAV(t, 48000, 2, 4000, 52)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	landed, err := med.SeekSample(1 << 40)
	if err != nil {
		t.Fatal(err)
	}
	if landed != 13000 {
		t.Fatalf("a past-the-end seek landed at %d, want the timeline's length 13000", landed)
	}
	buf := audio.Get(med.Info().Default().Fmt, 128)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err != io.EOF {
		t.Fatalf("reading past the end returned %v, want io.EOF", err)
	}
}

// TestConcatComposite pins the capability gate: a timeline reports its
// members' own tracks, which Info's synthetic envelope cannot.
func TestConcatComposite(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, 4410, 61)
	b, _ := ratedWAV(t, 48000, 2, 4800, 62)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	comp, ok := med.(format.Composite)
	if !ok {
		t.Fatal("a timeline does not satisfy format.Composite")
	}
	got := comp.Members()
	if len(got) != 2 || got[0].Fmt.Rate != 44100 || got[1].Fmt.Rate != 48000 {
		t.Fatalf("Members() = %v, want the two members' own tracks", got)
	}
}

// TestPlanSegmentsTimelineVersions pins the two holes a synthetic track
// opens in the ADR-0004 cache key, both of which are silent.
//
// The synthetic track's codec is PCM, so the plan names pcm's decoder and no
// member's: a FLAC decoder fix would leave every album's cached segments
// stale. And the plan's own chain runs envelope to output, which here is 48
// kHz to 48 kHz and holds no resampler at all, so it names none of the
// resampling the 44.1 kHz member does to reach the envelope: a resampler
// revision would go unnoticed for exactly the members that ran it.
//
// The second half of the test is the point of the first: the naive plan,
// over the same synthetic track, really is missing both, so the prepend is
// load-bearing rather than belt and braces.
func TestPlanSegmentsTimelineVersions(t *testing.T) {
	e := waxflow.New()
	f44 := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	f48 := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	tracks := []container.Track{
		{Codec: codec.FLAC, Fmt: f44, Samples: 44100, Default: true},
		{Codec: codec.FLAC, Fmt: f44, Samples: 44100, Default: true},
		{Codec: codec.Opus, Fmt: f48, Samples: 48000, Default: true},
	}
	opts := waxflow.TranscodeOptions{Format: "opus"}

	plan, err := e.PlanSegmentsTimeline(tracks, opts, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{flac.Version, opus.Version, resample.HQ.Version()} {
		if !slices.Contains(plan.Versions, want) {
			t.Fatalf("the timeline plan's Versions %v do not name %q, so a revision of it "+
				"would not invalidate this timeline's cached segments", plan.Versions, want)
		}
	}
	if n := strings.Count(strings.Join(plan.Versions, ","), flac.Version); n != 1 {
		t.Fatalf("flac's version appears %d times; the member lists are deduplicated so a "+
			"thousand-member queue still keys on a handful of entries", n)
	}

	env, err := waxflow.ConcatTrack(tracks)
	if err != nil {
		t.Fatal(err)
	}
	naive, err := e.PlanSegments(env, opts, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{flac.Version, resample.HQ.Version()} {
		if slices.Contains(naive.Versions, absent) {
			t.Fatalf("planning the synthetic track alone already names %q, so this test is "+
				"no longer showing what PlanSegmentsTimeline is for", absent)
		}
	}
}

// TestHLSReadBackTimeline is the full-stack gapless proof, and the one test
// here that exercises every layer at once: one continuous signal is cut into
// three files, handed back as a timeline, planned, encoded to FLAC, framed
// into CMAF segments, served over HTTP, read back through the HLS client's
// fragmented-MP4 demuxer, and decoded. The result must be the original, bit
// for bit.
//
// Nothing weaker would catch a seam that survives the primitive and dies in
// delivery: TestConcatGaplessSeam proves the sample math and the server suite
// proves the wiring, but only this proves that what a player actually
// receives is what the timeline promised. FLAC is what makes it bit-exact
// rather than merely close.
func TestHLSReadBackTimeline(t *testing.T) {
	const frames = 100000
	cuts := []int{37000, 11000, frames - 48000} // uneven, none on a segment boundary
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 63)

	var raws [][]byte
	at := 0
	for _, n := range cuts {
		part := audio.Get(f, n)
		part.N = n
		audio.CopyFrames(part, 0, whole, at, n)
		raws = append(raws, wavFrom(t, cfg, part))
		audio.Put(part)
		at += n
	}

	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	ms := timelineMembers(t, e, raws...)
	tracks := make([]container.Track, len(ms))
	for i := range ms {
		tracks[i] = ms[i].Track
	}
	plan, err := e.PlanSegmentsTimeline(tracks, opts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Samples != frames {
		t.Fatalf("the timeline plan promises %d samples, want %d", plan.Samples, frames)
	}
	init, err := e.InitSegment(plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	segs := timelineSegments(t, e, raws, opts, plan.SegmentSamples, 0)

	files := map[string][]byte{"/init.mp4": init}
	var media []hls.MediaSegment
	for _, s := range segs {
		name := fmt.Sprintf("seg%d.m4s", s.Index)
		files["/"+name] = s.Data
		media = append(media, hls.MediaSegment{URI: name, Seconds: float64(s.Samples) / 48000})
	}
	files["/v0.m3u8"] = []byte(hls.Media("init.mp4", media))
	files["/master.m3u8"] = []byte(hls.Master([]hls.MasterVariant{
		{URI: "v0.m3u8", Bandwidth: plan.Bandwidth, Codecs: plan.Codecs},
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	med, err := hls.OpenVOD(context.Background(), hls.HTTPFetcher{}, srv.URL+"/master.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got := decodeMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, whole, got)
}

// timelineSegments runs a segmented transcode over a freshly built timeline.
// Every run needs its own Concat: a Media is consumed once, which is the
// same reason a worker restart re-opens its source.
func timelineSegments(t *testing.T, e *waxflow.Engine, raws [][]byte, opts waxflow.TranscodeOptions,
	segSamples int, start int64) []mp4.Segment {
	t.Helper()
	med, err := waxflow.Concat(timelineMembers(t, e, raws...), waxflow.ConcatOptions{Profile: opts.ResampleProfile})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	var segs []mp4.Segment
	_, err = e.TranscodeSegmentsMedia(context.Background(), med, opts,
		waxflow.SegmentedOptions{SegmentSamples: segSamples, StartSegment: start},
		func(s mp4.Segment) error {
			if want := start + int64(len(segs)); s.Index != want {
				t.Fatalf("segment index %d, want %d", s.Index, want)
			}
			segs = append(segs, s)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

// TestSegmentedRestartAcrossConcatSeam is M23's gate, and it is a test
// rather than an argument on purpose.
//
// The restart guarantee rests on the priming window exceeding every stateful
// node's memory, and the argument extends across a seam because a timeline's
// output over any span is a pure function of position: it holds no
// per-member state that a seek resets differently. That is a good argument.
// It is also exactly the kind of argument the limiter's restart bug hid
// behind, so the seam is placed everywhere it could matter instead: inside
// the priming window, exactly at the restart point, one sample before it,
// and at a member too short to fill the window at all.
func TestSegmentedRestartAcrossConcatSeam(t *testing.T) {
	const segSamples = 49152 // 12 FLAC blocks of 4096
	const restartAt = 4
	const p0 = restartAt * segSamples
	// primeSeconds is 0.1, which is 4800 samples at 48 kHz, rounded up to a
	// whole 4096-sample block: the window opens 8192 samples before p0.
	const prime = 8192

	const frames = 300000

	for _, tc := range []struct {
		name string
		// lens are the members' lengths in samples; they sum to frames.
		lens []int
	}{
		{"the seam sits inside the priming window", []int{p0 - prime/2, frames - (p0 - prime/2)}},
		{"the seam sits exactly at the restart point", []int{p0, frames - p0}},
		{"the seam sits one sample before the restart point", []int{p0 - 1, frames - (p0 - 1)}},
		{"the seam sits before the window opens", []int{p0 - 3*prime, frames - (p0 - 3*prime)}},
		// A member shorter than the window lies entirely inside it, so the
		// restarted run opens, drains, and closes a whole member during
		// priming and has to arrive at p0 with the same state anyway. This
		// is the case that catches per-member bookkeeping a seek would reset
		// differently, which two long members cannot reach.
		{"a member shorter than the priming window, entirely inside it",
			[]int{p0 - 8000, 2000, frames - (p0 - 6000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := waxflow.New()
			var raws [][]byte
			total := 0
			for i, n := range tc.lens {
				raw, _ := ratedWAV(t, 48000, 2, n, uint64(71+i))
				raws = append(raws, raw)
				total += n
			}
			if total != frames {
				t.Fatalf("the members sum to %d samples, want %d", total, frames)
			}
			opts := waxflow.TranscodeOptions{Format: "flac"}
			full := timelineSegments(t, e, raws, opts, segSamples, 0)
			tail := timelineSegments(t, e, raws, opts, segSamples, restartAt)
			assertSegmentTailMatches(t, full, tail, restartAt)
		})
	}
}

// TestSegmentedRestartAcrossConcatSeamWithGain runs the same seam with the
// limiter engaged, which is the blind spot TestSegmentedRestartFLAC had: with
// no gain the chain holds no decaying node, so the priming horizon is zero
// and the test above proves nothing about one. Here the chain's horizon is
// the limiter's 2 s, and the restart point sits well past it.
func TestSegmentedRestartAcrossConcatSeamWithGain(t *testing.T) {
	const segSamples = 49152
	const restartAt = 4 // 196608 samples = 4.1 s, past the limiter's 2 s horizon
	const frames = 400000
	const seam = restartAt*segSamples - 6000 // inside the priming window

	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, seam, 81)
	b, _ := ratedWAV(t, 48000, 2, frames-seam, 82)
	opts := waxflow.TranscodeOptions{Format: "flac", GainDB: 6}
	full := timelineSegments(t, e, [][]byte{a, b}, opts, segSamples, 0)
	tail := timelineSegments(t, e, [][]byte{a, b}, opts, segSamples, restartAt)
	assertSegmentTailMatches(t, full, tail, restartAt)
}

func assertSegmentTailMatches(t *testing.T, full, tail []mp4.Segment, startAt int64) {
	t.Helper()
	if int64(len(tail)) != int64(len(full))-startAt {
		t.Fatalf("the restarted run yielded %d segments, the continuous one %d from there",
			len(tail), int64(len(full))-startAt)
	}
	for i, s := range tail {
		if cont := full[int64(i)+startAt]; !bytes.Equal(s.Data, cont.Data) {
			t.Fatalf("restarted segment %d differs from the continuous run (%d bytes vs %d)",
				s.Index, len(s.Data), len(cont.Data))
		}
	}
}
