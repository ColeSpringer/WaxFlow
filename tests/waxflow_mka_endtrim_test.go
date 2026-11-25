package waxflow_test

// An end trim longer than the source's last packet, copied to Matroska. A
// DiscardPadding trims the block it rides on, so the copy spreads the trim
// over the final blocks, and a reader counts them back as one end trim.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// requireCleanMatroska holds a Matroska file to what a spread end trim buys:
// a strict probe passes with no damage, a walk finds trim as the end trim and
// nothing inside the run, and a decode that walks nothing plays samples.
func requireCleanMatroska(t *testing.T, raw []byte, samples, trim int64) {
	t.Helper()
	info, err := waxflow.New().Probe(context.Background(), container.BytesSource(raw), "mka", &waxflow.ProbeOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict probe refuses the copy: %v", err)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("the copy reopens with damage: %v", info.Warnings)
	}
	if tr := walkedTrack(t, raw, "mka"); tr.Padding != trim || tr.MidPadding != 0 || tr.Samples != samples {
		t.Errorf("a walk finds Padding %d, MidPadding %d, Samples %d; want %d, 0, %d",
			tr.Padding, tr.MidPadding, tr.Samples, trim, samples)
	}
	if got := playedFrames(t, raw); got != samples {
		t.Errorf("a plain decode plays %d frames, want %d", got, samples)
	}
}

// blockTrims is the trim each block of a Matroska file states, in order.
func blockTrims(t *testing.T, raw []byte) []int64 {
	t.Helper()
	d, err := mka.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pads []int64
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return pads
		}
		if err != nil {
			t.Fatal(err)
		}
		pads = append(pads, pkt.Padding)
	}
}

// trimmedBlocks counts the blocks stating a trim.
func trimmedBlocks(t *testing.T, raw []byte) int {
	t.Helper()
	n := 0
	for _, p := range blockTrims(t, raw) {
		if p > 0 {
			n++
		}
	}
	return n
}

// remuxWithEndTrim copies src to opts as if its track ended in trim samples
// of padding: the track's own, its length moved to match.
func remuxWithEndTrim(t *testing.T, src []byte, trim int64, opts waxflow.TranscodeOptions, seekable bool) ([]byte, container.Track, error) {
	t.Helper()
	demux, info, err := format.OpenDemuxer(container.BytesSource(src), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	track.Samples, track.Padding = track.Samples+track.Padding-trim, trim
	if seekable {
		ws := &memWS{}
		_, err = waxflow.New().RemuxDemuxer(context.Background(), demux, track, ws, opts)
		return ws.Buf, track, err
	}
	var buf bytes.Buffer
	_, err = waxflow.New().RemuxDemuxer(context.Background(), demux, track, &buf, opts)
	return buf.Bytes(), track, err
}

// TestRemuxToMatroskaSpreadsTheEndTrimOfAnHEAACv2Source: fdk's HE-AAC v2
// states 2494 samples of padding on access units of 2048. The copy reopens
// clean, plays its 144000 samples on a plain decode, and keeps the packets.
func TestRemuxToMatroskaSpreadsTheEndTrimOfAnHEAACv2Source(t *testing.T) {
	src := heV2Fixture(t)
	for _, seekable := range []bool{true, false} {
		t.Run(fmt.Sprintf("seekable=%v", seekable), func(t *testing.T) {
			out, res, err := remuxTo(t, container.BytesSource(src), waxflow.TranscodeOptions{Format: "aac", Container: "mka"}, seekable)
			if err != nil {
				t.Fatal(err)
			}
			if res.Samples != heV2FixtureSamples {
				t.Errorf("the run reports %d samples, want %d", res.Samples, heV2FixtureSamples)
			}
			requireCleanMatroska(t, out, heV2FixtureSamples, 2494)
			if got := trimmedBlocks(t, out); got != 2 {
				t.Errorf("%d blocks state a trim, want the last 2", got)
			}
			if !slices.EqualFunc(payloads(t, out, "mka"), payloads(t, src, "m4a"), bytes.Equal) {
				t.Error("the copy's packets are not the source's")
			}
			// The same audio, sample for sample: the spread drops what the
			// source's own trim drops and nothing else.
			if !slices.Equal(decodeWhole(t, out, "mka"), decodeWhole(t, src, "m4a")) {
				t.Error("a plain decode of the copy is not the source's decode")
			}
		})
	}
}

// TestRemuxToMatroskaSpreadsAnEndTrimOverItsBlocks: trims of one sample over
// a block and more, on AAC (1024-sample packets) and HE-AAC (2048), are one
// DiscardPadding per block they reach, and the copy copies on to MP4 with the
// trim whole. A trim of one block or one sample under is one DiscardPadding.
func TestRemuxToMatroskaSpreadsAnEndTrimOverItsBlocks(t *testing.T) {
	heV1, err := os.ReadFile(filepath.Join("..", "codec", "aac", "testdata", "fdk_he_v1.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	prog := waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}
	for _, src := range []struct {
		name   string
		raw    []byte
		packet int64
	}{
		{"aac", remuxFixture(t, prog, 96000), 1024},
		{"he-aac v1", heV1, 2048},
		{"he-aac v2", heV2Fixture(t), 2048},
	} {
		want := payloads(t, src.raw, "m4a")
		for _, trim := range []int64{src.packet - 1, src.packet, 1025, 2129, 2494} {
			for _, seekable := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%d/seekable=%v", src.name, trim, seekable), func(t *testing.T) {
					out, track, err := remuxWithEndTrim(t, src.raw, trim, waxflow.TranscodeOptions{Format: "aac", Container: "mka"}, seekable)
					if err != nil {
						t.Fatal(err)
					}
					requireCleanMatroska(t, out, track.Samples, trim)
					if got, blocks := trimmedBlocks(t, out), int((trim+src.packet-1)/src.packet); got != blocks {
						t.Errorf("%d blocks state a trim, want %d", got, blocks)
					}
					// The round trip: Matroska to MP4 is a copy again, the
					// trim one statement and the packets the source's.
					back := &memWS{}
					res, err := waxflow.New().Remux(context.Background(), container.BytesSource(out), "mka", back, prog)
					if err != nil {
						t.Fatalf("mka to m4a: %v", err)
					}
					if tr := openedTrack(t, back.Buf); res.Samples != track.Samples || tr.Samples != track.Samples ||
						tr.Delay != track.Delay || tr.Padding != trim {
						t.Errorf("the round trip reports %d samples and reopens with Samples %d, Delay %d, Padding %d; want %d, %d, %d",
							res.Samples, tr.Samples, tr.Delay, tr.Padding, track.Samples, track.Delay, trim)
					}
					if !slices.EqualFunc(payloads(t, back.Buf, "m4a"), want, bytes.Equal) {
						t.Error("the round trip's packets are not the source's")
					}
				})
			}
		}
	}
}

// longOggTrim is an Ogg Opus stream whose final granule trims 2000 samples
// more than the encoder's own end trim: more than its last 960-sample packet.
func longOggTrim(t *testing.T) (raw []byte, samples int64) {
	t.Helper()
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 96000)
	last := len(testutil.OggPages(t, src)) - 1
	src = testutil.SetOggGranule(t, src, last, testutil.OggGranule(t, src, last)-2000)
	return src, openedTrack(t, src).Samples
}

// TestRemuxToMatroskaSpreadsAnOggFinalGranuleTrim: Ogg states its end trim as
// a final granule, so the copy learns it from the length. Matroska spreads
// it; WebM states it on the last block, as it did, and so does a WebM copy
// of the Matroska copy, whose packets state the trim between them.
func TestRemuxToMatroskaSpreadsAnOggFinalGranuleTrim(t *testing.T) {
	src, samples := longOggTrim(t)
	var spread []byte
	for _, seekable := range []bool{true, false} {
		out, res, err := remuxTo(t, container.BytesSource(src), waxflow.TranscodeOptions{Format: "opus", Container: "mka"}, seekable)
		if err != nil {
			t.Fatal(err)
		}
		if res.Samples != samples {
			t.Errorf("the run reports %d samples, want %d", res.Samples, samples)
		}
		trim := walkedTrack(t, out, "mka").Padding
		if trim <= 2000 {
			t.Fatalf("the copy's end trim is %d samples, want more than the 2000 the granule took", trim)
		}
		requireCleanMatroska(t, out, samples, trim)
		if got := trimmedBlocks(t, out); got < 2 {
			t.Fatalf("%d blocks of the copy state a trim, want the trim spread", got)
		}
		spread = out
	}
	webm := waxflow.TranscodeOptions{Format: "opus", Container: "webm"}
	first, _, err := remuxTo(t, container.BytesSource(src), webm, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := trimmedBlocks(t, first); got != 1 {
		t.Errorf("the WebM copy states a trim on %d blocks, want the last alone", got)
	}
	if tr := walkedTrack(t, first, "webm"); tr.Samples != samples {
		t.Errorf("a walk of the WebM copy finds %d samples, want %d", tr.Samples, samples)
	}
	if plan, err := waxflow.New().PlanRemux(walkedTrack(t, spread, "mka"), webm); err != nil || plan == nil {
		t.Fatalf("PlanRemux of the Matroska copy to webm = %v, %v; want a copy", plan, err)
	}
	second, _, err := remuxTo(t, container.BytesSource(spread), webm, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, first) {
		t.Errorf("a WebM copy of the Matroska copy states a trim on %d blocks; want the file the WebM copy of the source is",
			trimmedBlocks(t, second))
	}
}

// TestRemuxToWebMDeclinesAnEndTrimNoPacketHolds: WebM states an end trim on
// its last block alone (Firefox rejects a second DiscardPadding), so a copy
// whose stated end trim is longer than any packet of its codec declines to
// the transcode, and Matroska copies it.
func TestRemuxToWebMDeclinesAnEndTrimNoPacketHolds(t *testing.T) {
	e := waxflow.New()
	opus := openedTrack(t, remuxFixture(t, waxflow.TranscodeOptions{Format: "opus", Container: "mka"}, 96000))
	vorb := openedTrack(t, remuxFixture(t, waxflow.TranscodeOptions{Format: "vorbis", Container: "mka"}, 96000))
	cfg, err := vorbis.ParseConfig(vorb.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		format  string
		track   container.Track
		longest int64
	}{
		{"opus", opus, 48000 * 120 / 1000},
		{"vorbis", vorb, int64(cfg.LongBlock()) / 2},
	} {
		t.Run(tc.format, func(t *testing.T) {
			track := tc.track
			track.TrimsUnread = false
			webm := waxflow.TranscodeOptions{Format: tc.format, Container: "webm"}
			track.Padding = tc.longest
			if plan, err := e.PlanRemux(track, webm); err != nil || plan == nil {
				t.Errorf("a trim one packet can hold: PlanRemux = %v, %v; want a copy", plan, err)
			}
			track.Padding = tc.longest + 1
			if plan, err := e.PlanRemux(track, webm); err != nil || plan != nil {
				t.Errorf("PlanRemux to webm = %v, %v; want a decline", plan, err)
			}
			if plan, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: tc.format, Container: "mka"}); err != nil || plan == nil {
				t.Errorf("PlanRemux to mka = %v, %v; want a copy", plan, err)
			}
		})
	}

	// The run refuses what the plan declines, in the muxer's words.
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus", Container: "mka"}, 96000)
	_, _, err = remuxWithEndTrim(t, src, 48000*120/1000+1, waxflow.TranscodeOptions{Format: "opus", Container: "webm"}, true)
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Errorf("the run to webm = %v, want an unsupported-format refusal", err)
	}
}

// TestCutOfASpreadEndTrimCopies: a cut to the end of a Matroska source whose
// end trim spans three blocks carries it as its own end trim, so it copies
// to MP4, which states one.
func TestCutOfASpreadEndTrimCopies(t *testing.T) {
	prog := waxflow.TranscodeOptions{Format: "aac", Container: waxflow.ContainerProgressive}
	src, _, err := remuxWithEndTrim(t, remuxFixture(t, prog, 96000), 2129, waxflow.TranscodeOptions{Format: "aac", Container: "mka"}, true)
	if err != nil {
		t.Fatal(err)
	}
	measured := walkedTrack(t, src, "mka")
	if measured.Padding != 2129 || measured.MidPadding != 0 {
		t.Fatalf("the source walks to Padding %d, MidPadding %d; want 2129, 0", measured.Padding, measured.MidPadding)
	}
	const from = 20000
	spans := []waxflow.Span{{From: from, To: waxflow.ToEnd}}
	e := waxflow.New()
	ctx := context.Background()
	plan, err := e.PlanCut(measured, prog, spans, func() (int, error) { return e.PacketGrid(ctx, container.BytesSource(src), "mka") })
	if err != nil || plan == nil {
		t.Fatalf("PlanCut = %v, %v; want a copy", plan, err)
	}
	out := &memWS{}
	res, err := e.CutStream(ctx, container.BytesSource(src), "mka", out, prog, spans, plan.Grid, plan.Source)
	if err != nil {
		t.Fatal(err)
	}
	want := measured.Samples - from
	if got := playedFrames(t, out.Buf); res.Samples != want || got != want {
		t.Errorf("the cut reports %d samples and plays %d, want %d", res.Samples, got, want)
	}
}
