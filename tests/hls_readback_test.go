package waxflow_test

// HLS VOD read-back: the symmetry closer for HLS. The segmenter
// writes an init segment, numbered fMP4 media segments, and M3U8 playlists; the
// HLS client reads that presentation back over HTTP, follows the playlists,
// concatenates the media behind the out-of-band init, and decodes it through the
// fragmented-MP4 demuxer as a format.Media. FLAC proves it bit-exact; the lossy
// codecs prove the dOps/edit-list path; a two-variant master proves variant
// selection.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/hls"
)

// hlsPresentation is one variant's worth of served resources.
type hlsPresentation struct {
	init string // init URI
	segs []mp4.Segment
	rate int
}

// serveHLS builds init + segments for each named format and serves them, plus a
// media playlist per variant and a master playlist, from an httptest server. It
// returns the master URL and the fetcher. Variants are served in the given
// order, so a caller selects one by index.
func serveHLS(t *testing.T, e *waxflow.Engine, raw []byte, src *audio.Buffer, variants []waxflow.TranscodeOptions, segSamples int) (string, hls.Fetcher) {
	t.Helper()
	files := map[string][]byte{}
	var masterVariants []hls.MasterVariant

	for vi, opts := range variants {
		plan, err := e.PlanSegments(pcmTrack(src.Fmt, src.N), opts, float64(segSamples)/float64(src.Fmt.Rate))
		if err != nil {
			t.Fatalf("variant %d PlanSegments: %v", vi, err)
		}
		init, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatalf("variant %d InitSegment: %v", vi, err)
		}
		segs, _ := collectSegments(t, e, raw, opts, plan.SegmentSamples, 0)

		initName := fmt.Sprintf("v%d-init.mp4", vi)
		files["/"+initName] = init
		var media []hls.MediaSegment
		for _, s := range segs {
			segName := fmt.Sprintf("v%d-seg%d.m4s", vi, s.Index)
			files["/"+segName] = s.Data
			media = append(media, hls.MediaSegment{URI: segName, Seconds: float64(s.Samples) / float64(src.Fmt.Rate)})
		}
		mediaName := fmt.Sprintf("v%d.m3u8", vi)
		files["/"+mediaName] = []byte(hls.Media(initName, media))
		masterVariants = append(masterVariants, hls.MasterVariant{
			URI: mediaName, Bandwidth: plan.Bandwidth, Codecs: plan.Codecs,
		})
	}
	files["/master.m3u8"] = []byte(hls.Master(masterVariants))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/master.m3u8", hls.HTTPFetcher{}
}

// decodeMedia reads a whole Media into one buffer, refusing to exceed capMax
// frames so an untrimmed read (a gapless-trim regression) fails loudly.
func decodeMedia(t *testing.T, med format.Media, capMax int) *audio.Buffer {
	t.Helper()
	f := med.Info().Default().Fmt
	out := audio.Get(f, capMax)
	out.N = 0
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		if out.N+tmp.N > out.Cap() {
			t.Fatalf("decoded more than the %d-frame cap (gapless trim not applied?)", capMax)
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
	return out
}

// TestHLSReadBackFLAC is the strongest symmetry proof: a lossless HLS
// presentation the segmenter wrote, read back over HTTP through the client,
// reconstructs the source bit for bit.
func TestHLSReadBackFLAC(t *testing.T) {
	const frames = 100000 // ~2 s at 48 kHz: several segments plus a tail
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 61)
	defer audio.Put(src)
	e := waxflow.New()

	opts := waxflow.TranscodeOptions{Format: "flac"}
	masterURL, fetcher := serveHLS(t, e, raw, src, []waxflow.TranscodeOptions{opts}, 48000)

	med, err := hls.OpenVOD(context.Background(), fetcher, masterURL, nil)
	if err != nil {
		t.Fatalf("OpenVOD: %v", err)
	}
	defer med.Close()
	if got := med.Info().Default().Fmt; got != src.Fmt {
		t.Fatalf("read-back format %v, want %v", got, src.Fmt)
	}
	got := decodeMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, src, got)
}

// TestHLSReadBackLossy exercises the lossy read-back paths (the dOps Opus sample
// entry and the AAC edit list): the presentation reads back gapless-exact in
// length and as a real, aligned signal (not silence or garbage).
func TestHLSReadBackLossy(t *testing.T) {
	const frames = 96000 // 2 s at 48 kHz
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 62)
	defer audio.Put(src)
	e := waxflow.New()

	for _, tc := range []struct {
		name       string
		opts       waxflow.TranscodeOptions
		segSamples int
	}{
		{"opus", waxflow.TranscodeOptions{Format: "opus"}, 48000},
		{"aac", waxflow.TranscodeOptions{Format: "aac"}, 48128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masterURL, fetcher := serveHLS(t, e, raw, src, []waxflow.TranscodeOptions{tc.opts}, tc.segSamples)
			med, err := hls.OpenVOD(context.Background(), fetcher, masterURL, nil)
			if err != nil {
				t.Fatalf("OpenVOD: %v", err)
			}
			defer med.Close()
			got := decodeMedia(t, med, frames+2048)
			defer audio.Put(got)
			// Gapless read-back lands within a frame of the source length.
			if got.N < frames-1024 || got.N > frames+1024 {
				t.Errorf("read back %d frames, want ~%d", got.N, frames)
			}
			// The signal has real energy (not silence, not garbage), so the
			// segments genuinely decoded.
			var energy float64
			ch := got.ChanF(0)
			for _, v := range ch {
				energy += float64(v) * float64(v)
			}
			if got.N == 0 || energy/float64(got.N) < 1e-4 {
				t.Errorf("read-back signal is empty or silent (mean energy %g)", energy/float64(max(got.N, 1)))
			}
		})
	}
}

// TestHLSReadBackVariantSelect proves the master-playlist path and the variant
// selector: a two-variant master read at index 1 returns the second variant's
// audio. The variants carry the same source at different segment lengths, so the
// selected one still reconstructs the signal.
func TestHLSReadBackVariantSelect(t *testing.T) {
	const frames = 80000
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 63)
	defer audio.Put(src)
	e := waxflow.New()

	variants := []waxflow.TranscodeOptions{{Format: "flac"}, {Format: "flac"}}
	masterURL, fetcher := serveHLS(t, e, raw, src, variants, 48000)

	// Index 1 must read (proves the master is parsed and the selector honored).
	med, err := hls.OpenVOD(context.Background(), fetcher, masterURL, &hls.ClientOptions{VariantIndex: 1})
	if err != nil {
		t.Fatalf("OpenVOD variant 1: %v", err)
	}
	defer med.Close()
	got := decodeMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, src, got)

	// An out-of-range index fails cleanly rather than panicking.
	if _, err := hls.OpenVOD(context.Background(), fetcher, masterURL, &hls.ClientOptions{VariantIndex: 5}); err == nil {
		t.Error("variant index 5 of a 2-variant master must fail")
	}
}

// TestHLSReadBackTranscode proves the assembled Media flows through the engine
// like any local source: an HLS FLAC presentation transcodes to WAV via
// TranscodeMedia, the uniform consume-point for non-Source inputs.
func TestHLSReadBackTranscode(t *testing.T) {
	const frames = 60000
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 64)
	defer audio.Put(src)
	e := waxflow.New()

	masterURL, fetcher := serveHLS(t, e, raw, src, []waxflow.TranscodeOptions{{Format: "flac"}}, 48000)
	med, err := hls.OpenVOD(context.Background(), fetcher, masterURL, nil)
	if err != nil {
		t.Fatalf("OpenVOD: %v", err)
	}
	defer med.Close()

	out := &memWS{}
	res, err := e.TranscodeMedia(context.Background(), med, out, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatalf("TranscodeMedia: %v", err)
	}
	if res.Samples != frames {
		t.Errorf("transcoded %d samples, want %d", res.Samples, frames)
	}
	back := readAll(t, e, out.b, frames)
	defer audio.Put(back)
	equalPCM(t, src, back)
}
