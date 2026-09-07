package oracletest

// A named deficit with teeth: waxlabel v1.6.2 reports the MP4 sample entry's
// 16.16 samplerate verbatim, and that field cannot hold a rate past 65535 Hz.
// WaxFlow writes 65535 for a hi-res ALAC (the cookie carries the true rate)
// and, as the FLAC-in-ISOBMFF spec requires, the greatest whole halving for a
// hi-res FLAC (48000 for 96 kHz), which the same spec says a reader MUST
// override from STREAMINFO. docs/upstream-requests.md carries the request.
// This cell pins the present behaviour so the day waxlabel reads the config
// boxes instead, it fails here, which is the cue to retire the request and
// flip these expectations to the true rate.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
)

func TestWaxlabelReadsTheSampleEntryRateVerbatim(t *testing.T) {
	const retire = "waxlabel now reads the codec config box for the rate: retire the entry in docs/upstream-requests.md and pin the true rate here"

	t.Run("alac-96k", func(t *testing.T) {
		f := audio.Format{Rate: 96000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}
		wav, _ := synthWAV(t, f, 9600)
		path := filepath.Join(t.TempDir(), "out.m4a")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", file,
			waxflow.TranscodeOptions{Format: "alac", Container: "fragmented"}); err != nil {
			t.Fatalf("Transcode: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := waxlabelRate(t, raw); got != 65535 {
			t.Errorf("waxlabel SampleRate = %d for a 96 kHz ALAC whose entry says 65535; %s", got, retire)
		}
	})

	t.Run("flac-96k-fmp4", func(t *testing.T) {
		f := audio.Format{Rate: 96000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}
		enc, err := flac.NewEncoder(f, nil)
		if err != nil {
			t.Fatal(err)
		}
		bs := flac.EncoderBlockSize(flac.DefaultEncoderLevel)
		src := audio.Get(f, bs)
		defer audio.Put(src)
		src.N = bs
		var pkts []codec.Packet
		emit := func(p codec.Packet) error {
			p.Data = append([]byte(nil), p.Data...)
			pkts = append(pkts, p)
			return nil
		}
		if err := enc.Encode(src, emit); err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Finish(emit); err != nil {
			t.Fatal(err)
		}
		track := container.Track{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(bs)}
		init, err := mp4.InitSegment(track)
		if err != nil {
			t.Fatal(err)
		}
		seg, err := mp4.NewSegmenter(track, &mp4.SegmenterOptions{SegmentSamples: bs})
		if err != nil {
			t.Fatal(err)
		}
		file := append([]byte(nil), init...)
		collect := func(s mp4.Segment) error { file = append(file, s.Data...); return nil }
		for _, p := range pkts {
			if err := seg.WritePacket(p, collect); err != nil {
				t.Fatal(err)
			}
		}
		if err := seg.End(collect); err != nil {
			t.Fatal(err)
		}
		if got := waxlabelRate(t, file); got != 48000 {
			t.Errorf("waxlabel SampleRate = %d for a 96 kHz FLAC-in-MP4 whose entry says 48000 by spec; %s", got, retire)
		}
	})
}

// waxlabelRate parses raw with waxlabel and returns its one track's SampleRate.
func waxlabelRate(t *testing.T, raw []byte) int {
	t.Helper()
	doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
	if err != nil {
		t.Fatalf("waxlabel.Parse: %v", err)
	}
	tracks := doc.Properties().Tracks
	if len(tracks) != 1 {
		t.Fatalf("waxlabel found %d tracks, want 1", len(tracks))
	}
	return tracks[0].SampleRate
}
