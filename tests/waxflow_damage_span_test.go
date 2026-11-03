package waxflow_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpenSpanOfADamagedSourceEndsWithItsWarning: an Ogg Opus or Vorbis that
// loses a page to damage reads short of its exact length, and says so. An open
// span of it ends there, as a plain read does, rather than failing as a silent
// shortfall.
func TestOpenSpanOfADamagedSourceEndsWithItsWarning(t *testing.T) {
	for _, codec := range []string{"opus", "vorbis"} {
		t.Run(codec, func(t *testing.T) {
			raw := transcodeBytes(t, loopedWAV(t, 20), waxflow.TranscodeOptions{Format: codec})
			pages := testutil.OggPages(t, raw)
			bad := testutil.DamageOggPage(t, raw, len(pages)/2)
			med, err := format.Open(container.BytesSource(bad), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := med.Info().Default(); !tr.SamplesExact {
				med.Close()
				t.Fatalf("the stream's length is not exact (%d); nothing to hold a span to", tr.Samples)
			}
			sl, err := waxflow.Slice(med, 1000, waxflow.ToEnd)
			if err != nil {
				med.Close()
				t.Fatal(err)
			}
			defer sl.Close()
			buf := audio.Get(sl.Info().Default().Fmt, audio.StandardChunk)
			defer audio.Put(buf)
			var n int64
			for {
				err := sl.ReadChunk(buf)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("a span over a source that reported its damage failed: %v", err)
				}
				n += int64(buf.N)
			}
			if len(sl.Info().Warnings) == 0 {
				t.Fatal("the damage left no warning")
			}
			if want := sl.Info().Default().Samples; n >= want {
				t.Fatalf("delivered %d of %d samples: the damage took nothing", n, want)
			}
		})
	}
}

// TestCopyAcrossAHoleKeepsTheTimeline: a FLAC copy carries the frames damage
// took as silent frames, so it decodes to what a decode of the source
// delivers, silence included, and an unseekable destination's header holds.
func TestCopyAcrossAHoleKeepsTheTimeline(t *testing.T) {
	clean := transcodeBytes(t, loopedWAV(t, 10), waxflow.TranscodeOptions{Format: "flac"})
	frames := payloads(t, clean, "flac")
	k := len(frames) / 2
	at := bytes.Index(clean, frames[k])
	bad := append([]byte(nil), clean...)
	mid := at + len(frames[k])/2
	for i := mid - 8; i < mid+8; i++ {
		bad[i] ^= 0xFF
	}
	want, err := decodeAllDynamic(t, container.BytesSource(bad), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(want)
	for _, format := range []string{"flac", "ogg"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer // unseekable: the header commits its length up front
			opts := waxflow.TranscodeOptions{Format: "flac"}
			if format == "ogg" {
				opts.Container = "ogg"
			}
			if _, err := waxflow.New().Remux(t.Context(), container.BytesSource(bad), "flac", &out, opts); err != nil {
				t.Fatalf("copy: %v", err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(out.Bytes()), format)
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(got)
			if got.N != want.N {
				t.Fatalf("the copy decodes to %d samples, the source to %d", got.N, want.N)
			}
			if j := sameFrames(got, 0, want, 0, want.N); j >= 0 {
				t.Fatalf("the copy differs from the source's decode at sample %d", j)
			}
		})
	}
}

// TestSegmentedCopyAcrossAHoleRestartsIdentically: a segmented FLAC copy of a
// damaged source carries the hole as silent frames, so a worker restarted at
// any segment writes the continuous run's bytes and the plan's count holds.
func TestSegmentedCopyAcrossAHoleRestartsIdentically(t *testing.T) {
	clean := transcodeBytes(t, loopedWAV(t, 10), waxflow.TranscodeOptions{Format: "flac"})
	frames := payloads(t, clean, "flac")
	at := bytes.Index(clean, frames[len(frames)/2])
	bad := append([]byte(nil), clean...)
	for i := at + 40; i < at+56; i++ {
		bad[i] ^= 0xFF
	}
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	track := probeTrack(t, bad, "flac")
	grid, err := e.PacketGrid(context.Background(), container.BytesSource(bad), "flac")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanRemuxSegments(track, opts, 1.0, grid)
	if err != nil || plan == nil {
		t.Fatalf("plan: %v (%v)", plan, err)
	}
	full := collectRemuxSegments(t, e, bad, "flac", opts, plan.SegmentSamples, 0)
	if int64(len(full)) != plan.Segments {
		t.Fatalf("the run wrote %d segments, the plan promised %d", len(full), plan.Segments)
	}
	for start := 1; start < len(full); start++ {
		tail := collectRemuxSegments(t, e, bad, "flac", opts, plan.SegmentSamples, int64(start))
		for i, s := range tail {
			if !bytes.Equal(s.Data, full[start+i].Data) {
				t.Fatalf("restarted at %d, segment %d differs from the continuous run's", start, s.Index)
			}
		}
	}
}

// TestOpenSpanOverAFullWarningListEndsWithItsDamage: whether a read met damage
// is a count, so a span over a media whose warning list is already full still
// sees the damage its own read meets, and ends with it.
func TestOpenSpanOverAFullWarningListEndsWithItsDamage(t *testing.T) {
	raw := transcodeBytes(t, loopedWAV(t, 60), waxflow.TranscodeOptions{Format: "opus"})
	pages := testutil.OggPages(t, raw)
	if len(pages) < 160 {
		t.Fatalf("%d pages, too few to damage 70 apart", len(pages))
	}
	bad := raw
	for k := 10; k < 150; k += 2 {
		bad = testutil.DamageOggPage(t, bad, k)
	}
	med, err := format.Open(container.BytesSource(bad), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := readCount(med); err != nil || len(med.Info().Warnings) < 64 {
		med.Close()
		t.Fatalf("read %d (%v) with %d warnings; this cell needs a full list", n, err, len(med.Info().Warnings))
	}
	if _, err := med.SeekSample(context.Background(), 0); err != nil {
		med.Close()
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(med, 1000, waxflow.ToEnd)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()
	if n, err := readCount(sl); err != nil {
		t.Fatalf("a span over a source that reported its damage failed after %d samples: %v", n, err)
	}
}
