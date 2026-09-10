package oracletest

// The rate a waxlabel consumer reads back from a WaxFlow output. The MP4
// sample entry's 16.16 samplerate cannot hold a rate past 65535 Hz, so a
// hi-res file's entry never carries the true rate: WaxFlow writes 65535 for
// ALAC (the cookie carries 96000) and, as the FLAC-in-ISOBMFF spec requires,
// the greatest whole halving for FLAC (48000 for 96 kHz), a value the same
// spec says a reader MUST override from the STREAMINFO in the dfLa box.
// waxlabel v1.7.0 reads the codec config box instead of the entry, so both
// cells report the true rate; the entry is a container detail again. The
// AAC cells pin the same property from the other side: an HE-AAC stream's
// played rate comes from its ASC (and, for ADTS, from the frame header),
// not from a core sample rate that is half of it.

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

func TestWaxlabelReadsTheTrueRateFromTheCodecConfig(t *testing.T) {
	t.Run("alac-96k", func(t *testing.T) {
		raw := transcodeToBytes(t, audio.Format{Rate: 96000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}, 9600,
			waxflow.TranscodeOptions{Format: "alac", Container: "fragmented"})
		if got := waxlabelTrack(t, raw).SampleRate; got != 96000 {
			t.Errorf("waxlabel SampleRate = %d for a 96 kHz ALAC, want 96000 from the cookie (the entry says 65535)", got)
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
		if got := waxlabelTrack(t, file).SampleRate; got != 96000 {
			t.Errorf("waxlabel SampleRate = %d for a 96 kHz FLAC-in-MP4, want 96000 from STREAMINFO (the entry says 48000 by spec)", got)
		}
	})

	// HE-AAC v1 in a fragmented MP4: the core codes 22.05 kHz and SBR doubles
	// it, so the played rate lives in the ASC's extension, not in the core
	// sample-rate index.
	t.Run("he-aac-v1-fmp4-44k", func(t *testing.T) {
		raw := transcodeToBytes(t, audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, 44100,
			waxflow.TranscodeOptions{Format: "he-aac", Container: "fragmented"})
		if got := waxlabelTrack(t, raw).SampleRate; got != 44100 {
			t.Errorf("waxlabel SampleRate = %d for a 44.1 kHz HE-AAC v1 fragmented MP4, want 44100 from the ASC", got)
		}
	})

	// HE-AAC v2 in ADTS: there is no ASC in the file at all. The frame header
	// carries the core rate and one channel, and parametric stereo makes the
	// played stream 44.1 kHz stereo.
	t.Run("he-aac-v2-adts-44k", func(t *testing.T) {
		raw := transcodeToBytes(t, audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, 44100,
			waxflow.TranscodeOptions{Format: "he-aac", Container: "adts", HEAACv2: true})
		tr := waxlabelTrack(t, raw)
		if tr.SampleRate != 44100 || tr.Channels != 2 {
			t.Errorf("waxlabel read %d Hz / %d channels for a 44.1 kHz HE-AAC v2 ADTS stream, want 44100 / 2", tr.SampleRate, tr.Channels)
		}
	})
}

// transcodeToBytes runs one Transcode of the synthesized test signal and
// returns the output file's bytes. A real file, not a buffer: the fragmented
// muxer back-patches, so the destination has to be seekable.
func transcodeToBytes(t *testing.T, f audio.Format, frames int, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	wav, _ := synthWAV(t, f, frames)
	path := filepath.Join(t.TempDir(), "out")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", file, opts); err != nil {
		file.Close()
		t.Fatalf("Transcode(%s/%s): %v", opts.Format, opts.Container, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// waxlabelTrack parses raw with waxlabel and returns its one track.
func waxlabelTrack(t *testing.T, raw []byte) waxlabel.AudioTrack {
	t.Helper()
	doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
	if err != nil {
		t.Fatalf("waxlabel.Parse: %v", err)
	}
	tracks := doc.Properties().Tracks
	if len(tracks) != 1 {
		t.Fatalf("waxlabel found %d tracks, want 1", len(tracks))
	}
	return tracks[0]
}
