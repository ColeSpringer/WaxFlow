package waxflow_test

// Engine-level Matroska/WebM coverage: transcode a known signal to each codec
// MKA carries through the whole engine, then re-read the file with the
// production mka demuxer (unlike the fMP4 case, the mka demuxer reads exactly
// what the muxer writes). Lossless paths (PCM, FLAC) must reconstruct bit for
// bit; lossy paths (Opus, AAC) must reproduce the exact gapless sample count,
// which proves the CodecDelay/DiscardPadding round-trip.

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

// transcodeMKA runs a WAV source to the given format+container and returns the
// output bytes and the reported result.
func transcodeMKA(t *testing.T, e *waxflow.Engine, wav []byte, format, cont string) ([]byte, *waxflow.TranscodeResult) {
	t.Helper()
	out := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: format, Container: cont})
	if err != nil {
		t.Fatalf("transcode %s/%s: %v", format, cont, err)
	}
	if res.Container != cont {
		t.Errorf("Container = %q, want %q", res.Container, cont)
	}
	return out.b, res
}

// TestTranscodeMKALossless pins the lossless MKA paths: an integer WAV survives
// to PCM-in-Matroska and FLAC-in-Matroska and back bit for bit.
func TestTranscodeMKALossless(t *testing.T) {
	e := waxflow.New()
	const frames = 9111
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}

	for _, format := range []string{"wav", "flac"} {
		t.Run(format, func(t *testing.T) {
			wav, src := makeWAV(t, cfg, 2, frames, 23)
			defer audio.Put(src)
			mkaBytes, res := transcodeMKA(t, e, wav, format, "mka")
			if res.Samples != frames {
				t.Fatalf("Samples = %d, want %d", res.Samples, frames)
			}
			got := readAll(t, e, mkaBytes, frames)
			defer audio.Put(got)
			equalPCM(t, src, got)
		})
	}
}

// TestTranscodeMKALossy pins the lossy MKA/WebM paths: Opus-in-WebM and
// AAC-in-Matroska decode back to exactly the source sample count, proving
// gapless (CodecDelay front trim, DiscardPadding tail trim) round-trips.
func TestTranscodeMKALossy(t *testing.T) {
	e := waxflow.New()
	const frames = 9111
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}

	cases := []struct{ format, cont string }{
		{"opus", "webm"},
		{"opus", "mka"},
		{"aac", "mka"},
	}
	for _, tc := range cases {
		t.Run(tc.format+"/"+tc.cont, func(t *testing.T) {
			wav, src := makeWAV(t, cfg, 2, frames, 29)
			defer audio.Put(src)
			mkaBytes, res := transcodeMKA(t, e, wav, tc.format, tc.cont)
			if res.Samples != frames {
				t.Fatalf("Samples = %d, want %d", res.Samples, frames)
			}
			// readAll fatals if the decoded stream overruns the expected
			// frame count, so an exact count here proves the gapless trims.
			got := readAll(t, e, mkaBytes, frames)
			defer audio.Put(got)
			if got.N != frames {
				t.Fatalf("decoded %d frames, want %d (gapless trim failed)", got.N, frames)
			}
		})
	}
}

// TestTranscodeWebMRejectsLossless checks the webm codec-subset guard end to
// end: a webm request for FLAC (not a webm audio codec) fails the transcode.
func TestTranscodeWebMRejectsLossless(t *testing.T) {
	e := waxflow.New()
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 1000, 31)
	defer audio.Put(src)
	out := &memWS{}
	_, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: "flac", Container: "webm"})
	if err == nil {
		t.Error("flac/webm transcode accepted; want rejection")
	}
}
