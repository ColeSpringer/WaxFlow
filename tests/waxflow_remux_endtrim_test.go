package waxflow_test

// An end trim with no delay beside it, copied to the fragmented MP4 muxer.
// That muxer states a trim in its edit list, which Begin writes from a delay
// or a length, so the copy has to bring the length to Begin.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// endTrimKept and endTrimPad are the unprimed fixture's shape: a cut of an
// ADTS stream from sample 0 to an end off the 1024-sample packet grid.
const (
	endTrimKept = 50000
	endTrimPad  = 49*1024 - endTrimKept
)

// unprimedAAC is an AAC stream with no delay and no trim: ADTS states neither.
func unprimedAAC(t *testing.T) []byte {
	t.Helper()
	return remuxFixture(t, waxflow.TranscodeOptions{Format: "aac", Container: "adts"}, 96000)
}

// cutToMatroska cuts adts to spans as Matroska, where the cut's tail slop is
// the last block's DiscardPadding. declareLength writes through a seekable
// destination, so the file carries an Info Duration.
func cutToMatroska(t *testing.T, adts []byte, span waxflow.Span, declareLength bool) []byte {
	t.Helper()
	opts := waxflow.TranscodeOptions{Format: "aac", Container: "mka"}
	if !declareLength {
		out, plan := runCut(t, adts, "", opts, []waxflow.Span{span})
		if plan == nil {
			t.Fatal("the cut to mka declined")
		}
		return out
	}
	e := waxflow.New()
	ctx := context.Background()
	_, info, err := format.OpenDemuxer(container.BytesSource(adts), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanCut(info.Default(), opts, []waxflow.Span{span},
		func() (int, error) { return e.PacketGrid(ctx, container.BytesSource(adts), "") })
	if err != nil || plan == nil {
		t.Fatalf("the cut to mka: %v, %v", plan, err)
	}
	ws := &memWS{}
	if _, err := e.CutStream(ctx, container.BytesSource(adts), "", ws, opts, []waxflow.Span{span}, plan.Grid, plan.Source); err != nil {
		t.Fatal(err)
	}
	return ws.Buf
}

// remuxTo copies src to opts, through a seekable destination or a pipe.
func remuxTo(t *testing.T, src container.Source, opts waxflow.TranscodeOptions, seekable bool) ([]byte, *waxflow.TranscodeResult, error) {
	t.Helper()
	e := waxflow.New()
	if seekable {
		ws := &memWS{}
		res, err := e.Remux(context.Background(), src, "", ws, opts)
		return ws.Buf, res, err
	}
	var buf bytes.Buffer
	res, err := e.Remux(context.Background(), src, "", &buf, opts)
	return buf.Bytes(), res, err
}

// openedTrack is raw's default track as an open leaves it.
func openedTrack(t *testing.T, raw []byte) container.Track {
	t.Helper()
	_, info, err := format.OpenDemuxer(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return info.Default()
}

// playedFrames is how many frames a decode of raw delivers.
func playedFrames(t *testing.T, raw []byte) int64 {
	t.Helper()
	buf, err := decodeAllDynamic(t, container.BytesSource(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(buf)
	return int64(buf.N)
}

// TestRemuxOfAnUnprimedEndTrimToFragmentedMP4: a Matroska AAC with no
// CodecDelay whose last block carries a DiscardPadding copies to the
// fragmented muxer, which states the trim as its edit list's length.
func TestRemuxOfAnUnprimedEndTrimToFragmentedMP4(t *testing.T) {
	adts := unprimedAAC(t)
	for _, declared := range []bool{true, false} {
		src := cutToMatroska(t, adts, waxflow.Span{From: 0, To: endTrimKept}, declared)
		opened := openedTrack(t, src)
		if opened.Delay != 0 || opened.Padding != 0 || !opened.TrimsUnread {
			t.Fatalf("the source opens with Delay %d, Padding %d, TrimsUnread %v; this cell needs an unprimed track nothing walked",
				opened.Delay, opened.Padding, opened.TrimsUnread)
		}
		measured := walkedTrack(t, src, "mka")
		if measured.Padding != endTrimPad || measured.Samples != endTrimKept || measured.TrimsUnread {
			t.Fatalf("the walk found Padding %d, Samples %d, TrimsUnread %v; want %d, %d, false",
				measured.Padding, measured.Samples, measured.TrimsUnread, endTrimPad, endTrimKept)
		}
		for _, cont := range []string{"", waxflow.ContainerFragmented} {
			for _, seekable := range []bool{true, false} {
				name := "duration-less"
				if declared {
					name = "with a Duration"
				}
				name += "/to " + cont
				if seekable {
					name += "/seekable"
				}
				t.Run(name, func(t *testing.T) {
					opts := waxflow.TranscodeOptions{Format: "aac", Container: cont}
					e := waxflow.New()
					for _, track := range []container.Track{opened, measured} {
						if plan, err := e.PlanRemux(track, opts); err != nil || plan == nil {
							t.Fatalf("PlanRemux (TrimsUnread %v) = %v, %v; want a copy", track.TrimsUnread, plan, err)
						}
					}
					out, res, err := remuxTo(t, container.BytesSource(src), opts, seekable)
					if err != nil {
						t.Fatalf("the copy failed after %d bytes: %v", len(out), err)
					}
					if res.Samples != endTrimKept {
						t.Errorf("the run reports %d samples, want %d", res.Samples, endTrimKept)
					}
					if got := openedTrack(t, out); got.Delay != 0 || got.Samples != endTrimKept {
						t.Errorf("the copy reopens with Delay %d, Samples %d; want 0, %d", got.Delay, got.Samples, endTrimKept)
					}
					if got := playedFrames(t, out); got != endTrimKept {
						t.Errorf("the copy plays %d frames, want %d: the end trim is not stated", got, endTrimKept)
					}
					if !slices.EqualFunc(payloads(t, out, ""), payloads(t, src, ""), bytes.Equal) {
						t.Error("the copy's packets are not the source's")
					}
					// The copy is the shape again, as fragmented MP4: an edit
					// from 0 for less than its fragments hold.
					for _, again := range []bool{true, false} {
						next, res, err := remuxTo(t, container.BytesSource(out), opts, again)
						if err != nil {
							t.Fatalf("a copy of the copy (seekable %v) failed: %v", again, err)
						}
						if got := playedFrames(t, next); got != endTrimKept || res.Samples != endTrimKept {
							t.Errorf("a copy of the copy (seekable %v) plays %d frames and reports %d, want %d", again, got, res.Samples, endTrimKept)
						}
					}
				})
			}
		}
	}
}

// copyWalks copies a Matroska src to opts and reports whether the run walked
// it, which a read to the end alone does not do.
func copyWalks(t *testing.T, src []byte, opts waxflow.TranscodeOptions) bool {
	t.Helper()
	demux, info, err := format.OpenDemuxer(container.BytesSource(src), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waxflow.New().RemuxDemuxer(context.Background(), demux, info.Default(), &memWS{}, opts); err != nil {
		t.Fatal(err)
	}
	return demux.(container.Walker).Walked()
}

// TestRemuxNearAnUnprimedEndTrimKeepsCopying: the shapes beside the one above
// copy as they did, and only a source that may hide an end trim from the
// fragmented muxer is walked.
func TestRemuxNearAnUnprimedEndTrimKeepsCopying(t *testing.T) {
	adts := unprimedAAC(t)
	untrimmed := cutToMatroska(t, adts, waxflow.Span{From: 0, To: srcRawSamples(t, adts)}, true)
	primed := cutToMatroska(t, adts, waxflow.Span{From: 20000, To: endTrimKept}, true)
	trimmed := cutToMatroska(t, adts, waxflow.Span{From: 0, To: endTrimKept}, true)
	frag := waxflow.TranscodeOptions{Format: "aac"}
	prog := waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}

	t.Run("Matroska with no delay and no trim", func(t *testing.T) {
		if tr := walkedTrack(t, untrimmed, "mka"); tr.Delay != 0 || tr.Padding != 0 {
			t.Fatalf("the fixture walks to Delay %d, Padding %d; want neither", tr.Delay, tr.Padding)
		}
		for _, seekable := range []bool{true, false} {
			out, _, err := remuxTo(t, container.BytesSource(untrimmed), frag, seekable)
			if err != nil {
				t.Fatal(err)
			}
			// The same packets under the same header as the ADTS source's
			// copy: the walk found no trim and changed nothing.
			want, _, err := remuxTo(t, container.BytesSource(adts), frag, seekable)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, want) {
				t.Errorf("seekable %v: the copy differs from the ADTS source's (%d bytes against %d)", seekable, len(out), len(want))
			}
		}
		if !copyWalks(t, untrimmed, frag) {
			t.Error("the copy to the fragmented form did not walk the source: nothing looked for the trim")
		}
	})
	t.Run("Matroska with a delay and a trim", func(t *testing.T) {
		out, res, err := remuxTo(t, container.BytesSource(primed), frag, true)
		if err != nil {
			t.Fatal(err)
		}
		want := walkedTrack(t, primed, "mka")
		if want.Delay == 0 || want.Padding == 0 {
			t.Fatalf("the fixture walks to Delay %d, Padding %d; want both", want.Delay, want.Padding)
		}
		if got := openedTrack(t, out); got.Delay != want.Delay || got.Samples != want.Samples || res.Samples != want.Samples {
			t.Errorf("the copy reopens with Delay %d, Samples %d and reports %d; want %d, %d", got.Delay, got.Samples, res.Samples, want.Delay, want.Samples)
		}
		if copyWalks(t, primed, frag) {
			t.Error("the copy walked a source that declares a delay")
		}
	})
	// A source that states no trim per packet reads the same bytes for either
	// MP4 form: nothing walks it.
	copyOf := func(src []byte, opts waxflow.TranscodeOptions) []byte {
		out, _, err := remuxTo(t, container.BytesSource(src), opts, true)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for name, src := range map[string][]byte{
		"ADTS":                              adts,
		"progressive MP4":                   remuxFixture(t, prog, 96000),
		"fragmented MP4":                    remuxFixture(t, frag, 96000),
		"progressive MP4 with no delay":     copyOf(adts, prog),
		"fragmented MP4 with no delay":      copyOf(adts, frag),
		"fragmented MP4 with the end trim":  copyOf(trimmed, frag),
		"progressive MP4 with the end trim": copyOf(trimmed, prog),
	} {
		t.Run(name, func(t *testing.T) {
			if openedTrack(t, src).TrimsUnread {
				t.Fatal("the track opens with its trims unread; only a container that states them per packet does")
			}
			var read [2]int64
			for i, opts := range []waxflow.TranscodeOptions{frag, prog} {
				counted := &testutil.CountingSource{Src: container.BytesSource(src)}
				if _, _, err := remuxTo(t, counted, opts, true); err != nil {
					t.Fatal(err)
				}
				read[i] = counted.Bytes
			}
			if read[0] != read[1] {
				t.Errorf("the copy to the fragmented form read %d bytes and the progressive %d: the source was walked", read[0], read[1])
			}
		})
	}
	// The unprimed end trim to the forms that state it another way, or not
	// at all.
	for _, cont := range []string{waxflow.ContainerProgressive, "mka", "adts"} {
		t.Run("the end trim to "+cont, func(t *testing.T) {
			if copyWalks(t, trimmed, waxflow.TranscodeOptions{Format: "aac", Container: cont}) {
				t.Error("the copy walked the source, which only the fragmented form needs")
			}
		})
	}
}

// TestRemuxOfInnerAndEndTrimsStillRefusesMidCopy: a source the walk finds
// trimmed inside its run too is left to the copy's own refusal.
func TestRemuxOfInnerAndEndTrimsStillRefusesMidCopy(t *testing.T) {
	adts := unprimedAAC(t)
	demux, info, err := format.OpenDemuxer(container.BytesSource(adts), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	track.ID, track.Default = 0, true
	ws := &memWS{}
	m := mka.NewMuxer(ws, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var raw int64
	for i := 0; ; i++ {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out := container.Packet{Packet: pkt.Packet}
		if i == 5 {
			out.Padding = 100
		}
		raw += pkt.Dur
		if err := m.WritePacket(out); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.End(codec.Trailer{Samples: raw - 100 - endTrimPad, Padding: endTrimPad}); err != nil {
		t.Fatal(err)
	}
	if tr := walkedTrack(t, ws.Buf, "mka"); tr.MidPadding != 100 || tr.Padding != endTrimPad || tr.Delay != 0 {
		t.Fatalf("the fixture walks to MidPadding %d, Padding %d, Delay %d; want 100, %d, 0", tr.MidPadding, tr.Padding, tr.Delay, endTrimPad)
	}
	_, _, err = remuxTo(t, container.BytesSource(ws.Buf), waxflow.TranscodeOptions{Format: "aac"}, true)
	if err == nil || waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat || !strings.Contains(err.Error(), "trims 100 samples in the middle") {
		t.Fatalf("err = %v; want the copy's refusal of the inner trim", err)
	}
}

// TestPlanRemuxDeclinesAnEndTrimWithNoDelayOrLength: the fragmented muxer has
// nowhere to state such a trim, so the plan declines where the run failed at
// End, and the forms that state one another way still plan.
func TestPlanRemuxDeclinesAnEndTrimWithNoDelayOrLength(t *testing.T) {
	track := walkedTrack(t, cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true), "mka")
	e := waxflow.New()
	for _, tc := range []struct {
		name    string
		samples int64
		cont    string
		want    bool
	}{
		{"a length, fragmented", endTrimKept, "", true},
		{"no length, fragmented", -1, "", false},
		{"no length, by name", -1, waxflow.ContainerFragmented, false},
		{"an empty track, fragmented", 0, "", false},
		{"no length, progressive", -1, waxflow.ContainerProgressive, true},
		{"no length, mka", -1, "mka", true},
		{"no length, adts", -1, "adts", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := track
			tr.Samples, tr.SamplesExact = tc.samples, tc.samples >= 0
			plan, err := e.PlanRemux(tr, waxflow.TranscodeOptions{Format: "aac", Container: tc.cont})
			if err != nil {
				t.Fatal(err)
			}
			if (plan != nil) != tc.want {
				t.Errorf("PlanRemux returned planned=%v, want %v", plan != nil, tc.want)
			}
			if err := mp4.TrackFits(tr); (err == nil) != (tc.samples > 0) {
				t.Errorf("mp4.TrackFits = %v for a track of %d samples", err, tc.samples)
			}
		})
	}
}

// TestRemuxSegmentsStateAnUnprimedEndTrim: the segmented copy's init header
// states the same trim the same way, from the plan's length.
func TestRemuxSegmentsStateAnUnprimedEndTrim(t *testing.T) {
	src := cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "aac"}
	plan, err := e.PlanRemuxSegments(walkedTrack(t, src, "mka"), opts, 0.5,
		func() (int, error) { return e.PacketGrid(context.Background(), container.BytesSource(src), "") })
	if err != nil || plan == nil {
		t.Fatalf("PlanRemuxSegments = %v, %v; want a copy", plan, err)
	}
	stream, err := e.RemuxInitSegment(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, seg := range collectRemuxSegments(t, e, src, "", opts, plan.SegmentSamples, 0) {
		stream = append(stream, seg.Data...)
	}
	if got := openedTrack(t, stream); got.Delay != 0 || got.Samples != endTrimKept {
		t.Errorf("the segments reopen with Delay %d, Samples %d; want 0, %d", got.Delay, got.Samples, endTrimKept)
	}
	if got := playedFrames(t, stream); got != endTrimKept {
		t.Errorf("the segments play %d frames, want %d", got, endTrimKept)
	}
	// The segments are the shape as fragmented MP4, with no Matroska in
	// sight: the muxer's rule alone copies them.
	for _, seekable := range []bool{true, false} {
		out, res, err := remuxTo(t, container.BytesSource(stream), opts, seekable)
		if err != nil {
			t.Fatalf("seekable %v: a copy of the segments failed: %v", seekable, err)
		}
		if got := playedFrames(t, out); got != endTrimKept || res.Samples != endTrimKept {
			t.Errorf("seekable %v: a copy of the segments plays %d frames and reports %d, want %d", seekable, got, res.Samples, endTrimKept)
		}
	}
}

// TestProgressiveMP4EndTrimWithNoDelayReadsBack: the progressive muxer states
// an end trim with no delay as one edit from 0, and the reader takes it, so
// the copy reopens at the length it plays.
func TestProgressiveMP4EndTrimWithNoDelayReadsBack(t *testing.T) {
	src := cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true)
	out, res, err := remuxTo(t, container.BytesSource(src),
		waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != endTrimKept {
		t.Fatalf("the run reports %d samples, want %d", res.Samples, endTrimKept)
	}
	if got := openedTrack(t, out); got.Delay != 0 || got.Padding != endTrimPad || got.Samples != endTrimKept {
		t.Errorf("the copy reopens with Delay %d, Padding %d, Samples %d; want 0, %d, %d",
			got.Delay, got.Padding, got.Samples, endTrimPad, endTrimKept)
	}
	if got := playedFrames(t, out); got != endTrimKept {
		t.Errorf("the copy plays %d frames, want %d", got, endTrimKept)
	}
	// The trim is now the track's own, so a cut from sample 0 to the end
	// as aac declines it: a cut's end trim with no delay goes to the
	// transcode. With the trim unread, the cut copied the tail.
	e := waxflow.New()
	plan, err := e.PlanCut(openedTrack(t, out), waxflow.TranscodeOptions{Format: "aac"}, []waxflow.Span{{From: 0, To: waxflow.ToEnd}},
		func() (int, error) { return e.PacketGrid(context.Background(), container.BytesSource(out), "") })
	if err != nil || plan != nil {
		t.Errorf("PlanCut of the whole file as aac = %v, %v; want a decline", plan, err)
	}
	// And on from there: a copy of the progressive file states the trim
	// too, on a pipe as on a file, though nothing confirms a sample table.
	for _, seekable := range []bool{true, false} {
		again, res, err := remuxTo(t, container.BytesSource(out), waxflow.TranscodeOptions{Format: "aac"}, seekable)
		if err != nil {
			t.Fatalf("seekable %v: a fragmented copy of it failed: %v", seekable, err)
		}
		if got := playedFrames(t, again); got != endTrimKept || res.Samples != endTrimKept {
			t.Errorf("seekable %v: a fragmented copy of it plays %d frames and reports %d, want %d", seekable, got, res.Samples, endTrimKept)
		}
	}
}

// TestRemuxOfAShortDeclaredEndTrimWritesOn: a progressive MP4 that states an
// end trim with no delay and then ends short of the length it stated copies
// to the fragmented form on a pipe, where the length was written unconfirmed.
func TestRemuxOfAShortDeclaredEndTrimWritesOn(t *testing.T) {
	src := cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true)
	prog, _, err := remuxTo(t, container.BytesSource(src),
		waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}, true)
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(prog), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	if track.Delay != 0 || track.Padding != endTrimPad || track.SamplesExact {
		t.Fatalf("the source opens with Delay %d, Padding %d, exact %v; want 0, %d, false", track.Delay, track.Padding, track.SamplesExact, endTrimPad)
	}
	const packets = 10
	var out bytes.Buffer
	res, err := waxflow.New().RemuxDemuxer(context.Background(), &shortDemuxer{Demuxer: demux, left: packets}, track, &out,
		waxflow.TranscodeOptions{Format: "aac"})
	if err != nil {
		t.Fatalf("the copy of the short run failed: %v", err)
	}
	if res.Samples != packets*1024 {
		t.Errorf("the run reports %d samples, want the %d it copied", res.Samples, packets*1024)
	}
	if got := playedFrames(t, out.Bytes()); got != packets*1024 {
		t.Errorf("the copy plays %d frames, want the %d it holds", got, packets*1024)
	}
}

// shortDemuxer ends its demuxer's stream after left packets.
type shortDemuxer struct {
	container.Demuxer
	left int
}

func (s *shortDemuxer) ReadPacket(pkt *container.Packet) error {
	if s.left == 0 {
		return io.EOF
	}
	s.left--
	return s.Demuxer.ReadPacket(pkt)
}

// TestFFmpegMP4EditRoundingLosesNoAudio: ffmpeg states an MP4 edit's length
// in milliseconds, rounded either way, beside a sample table that holds the
// real one. The reader plays every sample ffmpeg encoded, and no more than
// the table holds past them.
func TestFFmpegMP4EditRoundingLosesNoAudio(t *testing.T) {
	testutil.FFmpeg(t)
	const seconds = 1.0417
	for _, tc := range []struct {
		name, acodec, ext string
		block             int64 // the most a table can hold past the audio
		extra             []string
	}{
		{"aac", "aac", "m4a", 1024, nil},
		{"aac in samples", "aac", "m4a", 1024, []string{"-movie_timescale", "44100"}},
		{"alac", "alac", "m4a", 0, nil},
		{"flac", "flac", "mp4", 0, []string{"-strict", "-2"}},
		{"pcm", "pcm_s16le", "mov", 0, nil},
	} {
		for _, rate := range []int{44100, 48000} {
			if len(tc.extra) == 2 && rate != 44100 {
				continue
			}
			t.Run(fmt.Sprintf("%s at %d", tc.name, rate), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "ff."+tc.ext)
				testutil.FFmpegGenerateDuration(t, path, seconds, rate, 2, tc.acodec, tc.extra...)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want := int64(math.Round(seconds * float64(rate)))
				got := playedFrames(t, raw)
				if got < want || got > want+tc.block {
					t.Errorf("the file plays %d frames of the %d ffmpeg encoded", got, want)
				}
				if tr := openedTrack(t, raw); tr.Samples != got {
					t.Errorf("the track states %d samples and plays %d", tr.Samples, got)
				}
			})
		}
	}
}

// TestRemuxTakesTheCallersMeasurement: a caller that measured the source
// hands the run its track, and the run walks nothing for the end trim. A
// track nothing measured is no measurement, and the run walks.
func TestRemuxTakesTheCallersMeasurement(t *testing.T) {
	adts := unprimedAAC(t)
	frag := waxflow.TranscodeOptions{Format: "aac"}
	for name, src := range map[string][]byte{
		"an end trim": cutToMatroska(t, adts, waxflow.Span{From: 0, To: endTrimKept}, true),
		"no trim":     cutToMatroska(t, adts, waxflow.Span{From: 0, To: srcRawSamples(t, adts)}, true),
	} {
		t.Run(name, func(t *testing.T) {
			run := func(seekable bool, opt ...waxflow.RunOption) ([]byte, bool) {
				demux, info, err := format.OpenDemuxer(container.BytesSource(src), "mka", nil)
				if err != nil {
					t.Fatal(err)
				}
				ws, pipe := &memWS{}, &bytes.Buffer{}
				var dst io.Writer = pipe
				if seekable {
					dst = ws
				}
				if _, err := waxflow.New().RemuxDemuxer(context.Background(), demux, info.Default(), dst, frag, opt...); err != nil {
					t.Fatal(err)
				}
				return append(pipe.Bytes(), ws.Buf...), demux.(container.Walker).Walked()
			}
			measured, unread := walkedTrack(t, src, "mka"), openedTrack(t, src)
			for _, seekable := range []bool{true, false} {
				want, walked := run(seekable)
				if !walked {
					t.Fatal("the run with no measurement did not walk the source")
				}
				got, walked := run(seekable, waxflow.WithMeasured(measured))
				if walked {
					t.Errorf("seekable %v: the run walked a source its caller had measured", seekable)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("seekable %v: the measured run wrote %d bytes that differ from the walked run's %d", seekable, len(got), len(want))
				}
				got, walked = run(seekable, waxflow.WithMeasured(unread))
				if !walked {
					t.Errorf("seekable %v: the run took an unwalked track for a measurement", seekable)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("seekable %v: the run handed an unwalked track wrote other bytes", seekable)
				}
			}
		})
	}
}

// TestRemuxCountGoesOnlyWithADelaylessEndTrim: the unconfirmed count rides
// to the fragmented muxer only beside an end trim with no delay. A primed
// source's pipe copy keeps its header, the delay alone, and an estimate is
// no count to state a trim by.
func TestRemuxCountGoesOnlyWithADelaylessEndTrim(t *testing.T) {
	frag := waxflow.TranscodeOptions{Format: "aac"}
	prog := waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}

	primed := remuxFixture(t, prog, 96000)
	if tr := openedTrack(t, primed); tr.Delay == 0 || tr.Padding == 0 || tr.SamplesExact {
		t.Fatalf("the primed source opens with Delay %d, Padding %d, exact %v; want both trims and an unconfirmed count", tr.Delay, tr.Padding, tr.SamplesExact)
	}
	out, _, err := remuxTo(t, container.BytesSource(primed), frag, false)
	if err != nil {
		t.Fatal(err)
	}
	if tr := openedTrack(t, out); tr.Delay != openedTrack(t, primed).Delay || tr.Samples != -1 {
		t.Errorf("the primed pipe copy reopens with Delay %d, Samples %d; want its delay and no length, as it wrote before", tr.Delay, tr.Samples)
	}

	src := cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true)
	trimmed, _, err := remuxTo(t, container.BytesSource(src), prog, true)
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(trimmed), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	estimate := info.Default()
	if estimate.Delay != 0 || estimate.Padding != endTrimPad {
		t.Fatalf("the source opens with Delay %d, Padding %d; want 0, %d", estimate.Delay, estimate.Padding, endTrimPad)
	}
	estimate.SamplesAdvisory = true
	var pipe bytes.Buffer
	_, err = waxflow.New().RemuxDemuxer(context.Background(), demux, estimate, &pipe, frag)
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat || pipe.Len() != 0 {
		t.Errorf("a copy of an estimated count with an end trim = %v after %d bytes; want the muxer's refusal before a byte", err, pipe.Len())
	}
}

// coarseEdit restates a fragmented MP4's edit list as ticks of a movie
// timescale of 1000, as a writer with a millisecond movie header does.
func coarseEdit(t *testing.T, file []byte, ticks uint64) []byte {
	t.Helper()
	out := bytes.Clone(file)
	mvhd, elst := bytes.Index(out, []byte("mvhd")), bytes.Index(out, []byte("elst"))
	if mvhd < 0 || elst < 0 || out[mvhd+4] != 0 || out[elst+4] != 1 {
		t.Fatal("no version 0 mvhd and version 1 elst to patch")
	}
	binary.BigEndian.PutUint32(out[mvhd+16:], 1000)
	binary.BigEndian.PutUint64(out[elst+12:], ticks)
	return out
}

// TestFragmentedMP4CoarseEditPlaysWhatItHolds: a fragmented MP4 whose edit
// states its length in milliseconds is whole when its fragments hold any
// length those milliseconds round, and one that trims plays to the longest
// such length, never short of the audio.
func TestFragmentedMP4CoarseEditPlaysWhatItHolds(t *testing.T) {
	frag := waxflow.TranscodeOptions{Format: "aac"}
	e := waxflow.New()
	ctx := context.Background()
	strict := func(t *testing.T, file []byte) container.Track {
		t.Helper()
		info, err := e.Probe(ctx, container.BytesSource(file), "", &waxflow.ProbeOptions{Strict: true})
		if err != nil {
			t.Fatalf("a strict probe refuses the file: %v", err)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("a strict probe warns: %v", info.Warnings)
		}
		return info.Default()
	}
	t.Run("restates", func(t *testing.T) {
		adts := unprimedAAC(t)
		held := srcRawSamples(t, adts)
		shape, _, err := remuxTo(t, container.BytesSource(cutToMatroska(t, adts, waxflow.Span{From: 0, To: held - 100}, true)), frag, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, ticks := range []uint64{uint64(held / 48), uint64(held/48) + 1} {
			file := coarseEdit(t, shape, ticks)
			if tr := openedTrack(t, file); tr.SamplesExact || tr.SamplesAdvisory || tr.Samples < held {
				t.Fatalf("%d ms: opens with Samples %d, exact %v, advisory %v; want a declared bound of at least %d", ticks, tr.Samples, tr.SamplesExact, tr.SamplesAdvisory, held)
			}
			if got := playedFrames(t, file); got != held {
				t.Errorf("%d ms: plays %d frames, want the %d it holds", ticks, got, held)
			}
			if tr := strict(t, file); tr.Samples != held || !tr.SamplesExact {
				t.Errorf("%d ms: a strict probe reports %d samples, exact %v; want %d, true", ticks, tr.Samples, tr.SamplesExact, held)
			}
			out, res, err := remuxTo(t, container.BytesSource(file), frag, false)
			if err != nil {
				t.Fatal(err)
			}
			if tr := openedTrack(t, out); tr.Samples != held || res.Samples != held {
				t.Errorf("%d ms: a pipe copy states %d samples and reports %d, want %d", ticks, tr.Samples, res.Samples, held)
			}
		}
	})
	t.Run("trims", func(t *testing.T) {
		primed := remuxFixture(t, frag, 96000)
		if tr := openedTrack(t, primed); tr.Delay == 0 || tr.Samples != 96000 {
			t.Fatalf("the source opens with Delay %d, Samples %d; want a delay and 96000", tr.Delay, tr.Samples)
		}
		// 2000 ms is 96000 samples, and as much as 96047 round to it.
		file := coarseEdit(t, primed, 2000)
		if got := playedFrames(t, file); got != 96047 {
			t.Errorf("plays %d frames, want the 96047 that 2000 ms can mean", got)
		}
		if tr := strict(t, file); tr.Samples != 96047 || tr.Padding == 0 || !tr.SamplesExact {
			t.Errorf("a strict probe reports %d samples, Padding %d, exact %v; want 96047, the rest, true", tr.Samples, tr.Padding, tr.SamplesExact)
		}
	})
}

// TestFragmentedMP4CoarseTrimHoldsOnEveryDestination: a fragmented MP4 whose
// millisecond edit trims its end, with no delay, states only a bound until
// its fragments are counted. Every run counts them first, so a copy or a
// transcode ends at the bound on a file as on a pipe. A decode nothing
// walked has no bound to stop at and plays what the fragments hold.
func TestFragmentedMP4CoarseTrimHoldsOnEveryDestination(t *testing.T) {
	adts := unprimedAAC(t)
	shape, _, err := remuxTo(t, container.BytesSource(cutToMatroska(t, adts, waxflow.Span{From: 0, To: endTrimKept}, true)),
		waxflow.TranscodeOptions{Format: "aac"}, true)
	if err != nil {
		t.Fatal(err)
	}
	// 1040 ms at 48 kHz: as much as 49967 samples round to it, which ends
	// inside the last packet, where every container's end trim sits.
	const bound = 49967
	file := coarseEdit(t, shape, 1040)
	held := srcRawSamples(t, file)
	if tr := openedTrack(t, file); tr.Delay != 0 || tr.Samples != bound || tr.SamplesExact || !tr.TrimsUnread || held <= bound {
		t.Fatalf("opens with Delay %d, Samples %d, exact %v, TrimsUnread %v over %d held; want 0, the bound %d, false, true",
			tr.Delay, tr.Samples, tr.SamplesExact, tr.TrimsUnread, held, bound)
	}
	if got := playedFrames(t, file); got != held {
		t.Errorf("a decode nothing walked plays %d frames, want the %d the fragments hold", got, held)
	}
	e := waxflow.New()
	// The segmented transcode counts the fragments first too.
	t.Run("segments", func(t *testing.T) {
		opts := waxflow.TranscodeOptions{Format: "flac"}
		plan, err := e.PlanSegments(openedTrack(t, file), opts, 0.5)
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.TranscodeSegments(context.Background(), container.BytesSource(file), "", opts,
			waxflow.SegmentedOptions{SegmentSamples: plan.SegmentSamples}, func(mp4.Segment) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if plan.Samples != bound || res.Samples != bound {
			t.Errorf("the plan states %d samples and the run reports %d, want the bound %d for both", plan.Samples, res.Samples, bound)
		}
	})
	for _, tc := range []struct {
		name string
		opts waxflow.TranscodeOptions
		pipe bool // the form streams
		copy bool
	}{
		{"a copy to fragmented MP4", waxflow.TranscodeOptions{Format: "aac"}, true, true},
		{"a copy to progressive MP4", waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}, false, true},
		{"a copy to Matroska", waxflow.TranscodeOptions{Format: "aac", Container: "mka"}, true, true},
		{"a transcode to FLAC", waxflow.TranscodeOptions{Format: "flac"}, true, false},
		{"a transcode to WAV", waxflow.TranscodeOptions{Format: "wav"}, true, false},
	} {
		for _, seekable := range []bool{true, false} {
			if !seekable && !tc.pipe {
				continue
			}
			t.Run(fmt.Sprintf("%s, seekable %v", tc.name, seekable), func(t *testing.T) {
				ws, pipe := &memWS{}, &bytes.Buffer{}
				var dst io.Writer = pipe
				if seekable {
					dst = ws
				}
				var res *waxflow.TranscodeResult
				var err error
				if tc.copy {
					res, err = e.Remux(context.Background(), container.BytesSource(file), "", dst, tc.opts)
				} else {
					res, err = e.Transcode(context.Background(), container.BytesSource(file), "", dst, tc.opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				out := append(pipe.Bytes(), ws.Buf...)
				if got := playedFrames(t, out); got != bound || res.Samples != bound {
					t.Errorf("plays %d frames and reports %d, want the bound %d", got, res.Samples, bound)
				}
			})
		}
	}
}

// TestCoarseFragmentedRunWalksOnlyWithNoDelay: a run confirms a coarse
// fragmented edit's bound before a seekable Begin only where nothing else
// holds the decode to it. Beside a delay the bound caps already, and the
// run writes the same file unwalked.
func TestCoarseFragmentedRunWalksOnlyWithNoDelay(t *testing.T) {
	frag := waxflow.TranscodeOptions{Format: "aac"}
	shape, _, err := remuxTo(t, container.BytesSource(cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true)), frag, true)
	if err != nil {
		t.Fatal(err)
	}
	e := waxflow.New()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		file  []byte
		plays int64
		walks bool
	}{
		{"with a delay", coarseEdit(t, remuxFixture(t, frag, 96000), 2000), 96047, false},
		{"with no delay", coarseEdit(t, shape, 1040), 49967, true},
	} {
		t.Run(tc.name+", copied", func(t *testing.T) {
			demux, info, err := format.OpenDemuxer(container.BytesSource(tc.file), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := info.Default(); !tr.TrimsUnread || (tr.Delay > 0) == tc.walks {
				t.Fatalf("opens with TrimsUnread %v, Delay %d; this cell needs a bound %s", tr.TrimsUnread, tr.Delay, tc.name)
			}
			ws := &memWS{}
			res, err := e.RemuxDemuxer(ctx, demux, info.Default(), ws, waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive})
			if err != nil {
				t.Fatal(err)
			}
			if got := demux.(container.Walker).Walked(); got != tc.walks {
				t.Errorf("the copy walked the source: %v, want %v", got, tc.walks)
			}
			if got := playedFrames(t, ws.Buf); got != tc.plays || res.Samples != tc.plays {
				t.Errorf("the copy plays %d frames and reports %d, want %d", got, res.Samples, tc.plays)
			}
		})
		t.Run(tc.name+", transcoded", func(t *testing.T) {
			med, err := e.OpenStream(container.BytesSource(tc.file), "")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			ws := &memWS{}
			res, err := e.TranscodeMedia(ctx, med, ws, waxflow.TranscodeOptions{Format: "flac"})
			if err != nil {
				t.Fatal(err)
			}
			if got := format.MediaWalked(med); got != tc.walks {
				t.Errorf("the transcode walked the source: %v, want %v", got, tc.walks)
			}
			if got := playedFrames(t, ws.Buf); got != tc.plays || res.Samples != tc.plays {
				t.Errorf("the transcode plays %d frames and reports %d, want %d", got, res.Samples, tc.plays)
			}
		})
	}
}
