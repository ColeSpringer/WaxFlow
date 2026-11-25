package waxflow_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// shortDuration rewrites a Matroska file's Info Duration to state short
// samples fewer than it does, as a writer that rounds to milliseconds does.
func shortDuration(t *testing.T, file []byte, rate int, short int64) []byte {
	t.Helper()
	out := bytes.Clone(file)
	seg, _ := testutil.EBMLSegment(t, out)
	info, ok := testutil.EBMLFind(t, seg, 0x1549A966)
	if !ok {
		t.Fatal("no Info element")
	}
	dur, ok := testutil.EBMLFind(t, info, 0x4489)
	if !ok || len(dur) != 8 {
		t.Fatalf("no 8-byte Duration element (%d bytes)", len(dur))
	}
	ms := math.Float64frombits(binary.BigEndian.Uint64(dur)) - float64(short)*1000/float64(rate)
	binary.BigEndian.PutUint64(dur, math.Float64bits(ms))
	return out
}

// TestCutOfAnUnwalkedTrackKeepsTheSourcesEnd: a Matroska track nothing
// walked states an estimate for its length, and a cut planned and run from
// it must not take the estimate for the end. A span to the end of such a
// file keeps every sample its packets hold.
func TestCutOfAnUnwalkedTrackKeepsTheSourcesEnd(t *testing.T) {
	adts := unprimedAAC(t)
	whole := srcRawSamples(t, adts)
	const short = 32
	exact, _, err := remuxTo(t, container.BytesSource(adts), waxflow.TranscodeOptions{Format: "aac", Container: "mka"}, true)
	if err != nil {
		t.Fatal(err)
	}
	src := shortDuration(t, exact, 48000, short)
	opened := openedTrack(t, src)
	if !opened.SamplesAdvisory || !opened.TrimsUnread || opened.Samples != whole-short {
		t.Fatalf("the source opens with Samples %d, advisory %v, TrimsUnread %v; want the estimate %d, unwalked",
			opened.Samples, opened.SamplesAdvisory, opened.TrimsUnread, whole-short)
	}
	if got := playedFrames(t, src); got != whole {
		t.Fatalf("the source plays %d frames, want %d", got, whole)
	}
	e := waxflow.New()
	ctx := context.Background()
	for _, cont := range []string{"mka", "", waxflow.ContainerProgressive} {
		for _, seekable := range []bool{true, false} {
			if cont == waxflow.ContainerProgressive && !seekable {
				continue
			}
			name := "to " + cont
			if seekable {
				name += "/seekable"
			}
			t.Run(name, func(t *testing.T) {
				opts := waxflow.TranscodeOptions{Format: "aac", Container: cont}
				spans := []waxflow.Span{{From: 0, To: waxflow.ToEnd}}
				plan, err := e.PlanCut(opened, opts, spans,
					func() (int, error) { return e.PacketGrid(ctx, container.BytesSource(src), "") })
				if err != nil {
					t.Fatal(err)
				}
				if plan == nil {
					t.Skip("the cut declines; nothing copies")
				}
				var out []byte
				var res *waxflow.TranscodeResult
				if seekable {
					ws := &memWS{}
					res, err = e.CutStream(ctx, container.BytesSource(src), "", ws, opts, spans, plan.Grid, plan.Source)
					out = ws.Buf
				} else {
					var buf bytes.Buffer
					res, err = e.CutStream(ctx, container.BytesSource(src), "", &buf, opts, spans, plan.Grid, plan.Source)
					out = buf.Bytes()
				}
				if err != nil {
					t.Fatalf("the cut failed: %v", err)
				}
				if res.Samples != whole {
					t.Errorf("the run reports %d samples, want the source's %d", res.Samples, whole)
				}
				if got := playedFrames(t, out); got != whole {
					t.Errorf("the cut plays %d frames, want the source's %d", got, whole)
				}
				if !slices.EqualFunc(payloads(t, out, ""), payloads(t, src, ""), bytes.Equal) {
					t.Error("the cut's packets are not the source's")
				}
				if tr := openedTrack(t, out); tr.Samples >= 0 && !tr.SamplesAdvisory && tr.Samples != whole {
					t.Errorf("the cut states %d samples, want %d or none", tr.Samples, whole)
				}
			})
		}
	}
}

// TestCutOfAnUnwalkedTrimmedTrackWalksIt: a cut planned from a Matroska
// header, whose end trim rides unread on its last block, runs from a walk of
// the source: the copy ends, the trim is stated, and the trimmed samples do
// not play, to a file as to a pipe.
func TestCutOfAnUnwalkedTrimmedTrackWalksIt(t *testing.T) {
	src := cutToMatroska(t, unprimedAAC(t), waxflow.Span{From: 0, To: endTrimKept}, true)
	opened := openedTrack(t, src)
	if !opened.TrimsUnread || opened.Padding != 0 {
		t.Fatalf("the source opens with TrimsUnread %v, Padding %d; this cell needs the unwalked track", opened.TrimsUnread, opened.Padding)
	}
	measured := walkedTrack(t, src, "mka")
	e := waxflow.New()
	ctx := context.Background()
	grid := func() (int, error) { return e.PacketGrid(ctx, container.BytesSource(src), "") }
	stream := func(t *testing.T, opts waxflow.TranscodeOptions, spans []waxflow.Span, g int, from container.Track, seekable bool) ([]byte, *waxflow.TranscodeResult) {
		t.Helper()
		ws, pipe := &memWS{}, &bytes.Buffer{}
		var dst io.Writer = pipe
		if seekable {
			dst = ws
		}
		res, err := e.CutStream(ctx, container.BytesSource(src), "", dst, opts, spans, g, from)
		if err != nil {
			t.Fatalf("the cut failed after %d bytes: %v", pipe.Len()+len(ws.Buf), err)
		}
		return append(pipe.Bytes(), ws.Buf...), res
	}
	for _, cont := range []string{"", "mka"} {
		for _, from := range []int64{0, 10000} {
			for _, seekable := range []bool{true, false} {
				t.Run(fmt.Sprintf("to %q from %d, seekable %v", cont, from, seekable), func(t *testing.T) {
					opts := waxflow.TranscodeOptions{Format: "aac", Container: cont}
					spans := []waxflow.Span{{From: from, To: waxflow.ToEnd}}
					plan, err := e.PlanCut(opened, opts, spans, grid)
					if err != nil || plan == nil {
						t.Fatalf("PlanCut from the header = %v, %v; want a cut", plan, err)
					}
					out, res := stream(t, opts, spans, plan.Grid, plan.Source, seekable)
					want := endTrimKept - from
					if res.Samples != want {
						t.Errorf("the run reports %d samples, want %d", res.Samples, want)
					}
					if got := playedFrames(t, out); got != want {
						t.Errorf("the cut plays %d frames, want %d", got, want)
					}
					// The run from the header is the run from a measurement.
					if same, _ := stream(t, opts, spans, plan.Grid, measured, seekable); !bytes.Equal(out, same) {
						t.Errorf("the cut differs from the one handed the measured track (%d bytes against %d)", len(out), len(same))
					}
				})
			}
		}
	}
	// The segmented cut takes its source track the same way.
	t.Run("segments", func(t *testing.T) {
		opts := waxflow.TranscodeOptions{Format: "aac"}
		spans := []waxflow.Span{{From: 10000, To: waxflow.ToEnd}}
		g, err := grid()
		if err != nil {
			t.Fatal(err)
		}
		segments := func(from container.Track) ([]byte, waxflow.SegmentedResult) {
			var out []byte
			res, err := e.CutSegments(ctx, container.BytesSource(src), "", opts, spans, g, from,
				waxflow.SegmentedOptions{SegmentSamples: 24 * 1024}, func(s mp4.Segment) error {
					out = append(out, s.Data...)
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			return out, *res
		}
		got, gotRes := segments(opened)
		want, wantRes := segments(measured)
		if len(got) == 0 || !bytes.Equal(got, want) {
			t.Errorf("the segments from the header (%d bytes) differ from those from the measured track (%d)", len(got), len(want))
		}
		if gotRes != wantRes {
			t.Errorf("the run from the header reports %+v, the one from the measured track %+v", gotRes, wantRes)
		}
	})
}

// TestSegmentedCutOfAnUnwalkedInnerTrimRefusesFirst: the segmented cut walks
// an unwalked source as the progressive one does, so a trim inside the run,
// which its segments cannot state, is refused before a segment goes out and
// not at the packet that carries it.
func TestSegmentedCutOfAnUnwalkedInnerTrimRefusesFirst(t *testing.T) {
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
	const at, pad = 60, 100
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
		if i == at {
			out.Padding = pad
		}
		raw += pkt.Dur
		if err := m.WritePacket(out); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.End(codec.Trailer{Samples: raw - pad}); err != nil {
		t.Fatal(err)
	}
	src := ws.Buf
	opened := openedTrack(t, src)
	if !opened.TrimsUnread || opened.MidPadding != 0 {
		t.Fatalf("the source opens with TrimsUnread %v, MidPadding %d; this cell needs the unwalked track", opened.TrimsUnread, opened.MidPadding)
	}
	var segments int
	_, err = waxflow.New().CutSegments(context.Background(), container.BytesSource(src), "", waxflow.TranscodeOptions{Format: "aac"},
		[]waxflow.Span{{From: 0, To: waxflow.ToEnd}}, 1024, opened,
		waxflow.SegmentedOptions{SegmentSamples: 24 * 1024}, func(mp4.Segment) error {
			segments++
			return nil
		})
	if err == nil || segments != 0 {
		t.Errorf("the segmented cut = %v after %d segments; want a refusal before the first", err, segments)
	}
}
