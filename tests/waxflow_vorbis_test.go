package waxflow_test

// Ogg-Vorbis and Vorbis-in-Matroska coverage (phase 5): the Vorbis encoder is
// wired into the engine through the vorbis output row, muxed into Ogg by the
// vorbisMuxMapping and into Matroska by the mka muxer's A_VORBIS reverse map. A
// float source must survive WAV -> Ogg-Vorbis -> decode within a lossy bound and
// gapless-exact, ffmpeg's reference libvorbis decoder must accept the output and
// agree on its length (the granulepos convention), and the same must hold for
// Vorbis in .mka/.webm.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// vorbisTestWAV synthesizes a stereo float WAV of a mixed tonal signal, distinct
// per channel so a channel swap or a coupling bug shows up.
func vorbisTestWAV(t *testing.T, frames int) (wav []byte, f audio.Format, interleaved []float32) {
	t.Helper()
	const rate, ch = 48000, 2
	f = audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
	interleaved = make([]float32, frames*ch)
	for i := 0; i < frames; i++ {
		l := 0.30*math.Sin(2*math.Pi*440*float64(i)/rate) + 0.15*math.Sin(2*math.Pi*880*float64(i)/rate)
		r := 0.25 * math.Sin(2*math.Pi*660*float64(i)/rate)
		interleaved[i*ch] = float32(l)
		interleaved[i*ch+1] = float32(r)
	}
	return synthWAVFromSamples(t, f, interleaved), f, interleaved
}

// nrmse returns the normalized RMS error between the decoded buffer and the
// interleaved source over their common length: sqrt(mean((a-b)^2))/rms(source).
func nrmse(dec *audio.Buffer, src []float32, channels int) float64 {
	frames := dec.N
	if sf := len(src) / channels; sf < frames {
		frames = sf
	}
	var errSq, sigSq float64
	for c := 0; c < channels; c++ {
		d := dec.ChanF(c)
		for i := 0; i < frames; i++ {
			s := float64(src[i*channels+c])
			e := float64(d[i]) - s
			errSq += e * e
			sigSq += s * s
		}
	}
	if sigSq == 0 {
		return 0
	}
	return math.Sqrt(errSq / sigSq)
}

// TestTranscodeOggVorbisRoundTrip pins the vorbis output row: WAV -> Ogg-Vorbis
// reports the true length gapless-exact, and decoding it back through our own
// pipeline reconstructs the signal within a lossy bound (not silence, not
// garbage, correctly aligned).
func TestTranscodeOggVorbisRoundTrip(t *testing.T) {
	e := waxflow.New()
	const frames = 30000
	wav, f, interleaved := vorbisTestWAV(t, frames)

	out := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: "vorbis"})
	if err != nil {
		t.Fatalf("transcode vorbis: %v", err)
	}
	// The default container for the vorbis row is native Ogg; the reported
	// container name is the row name (as the opus row reports "opus"), and the
	// bytes are an Ogg stream.
	if res.Container != "vorbis" {
		t.Errorf("Container = %q, want vorbis", res.Container)
	}
	if res.Samples != frames {
		t.Fatalf("Samples = %d, want %d (gapless exact)", res.Samples, frames)
	}
	if len(out.b) < 4 || string(out.b[:4]) != "OggS" {
		t.Fatalf("output is not an Ogg stream")
	}

	got := readAll(t, e, out.b, frames)
	defer audio.Put(got)
	if got.N != frames {
		t.Errorf("decoded %d frames, want %d", got.N, frames)
	}
	if e := nrmse(got, interleaved, f.Channels); e > 0.25 {
		t.Errorf("Ogg-Vorbis decode NRMSE %.3f exceeds lossy bound 0.25", e)
	}
}

// TestOggVorbisDifferential proves our Ogg-Vorbis output is a real file that
// ffmpeg's reference libvorbis decoder accepts and agrees with on length. The
// length agreement is the granulepos-convention gate: the final page granule
// carries the firstBlock/2 priming shift, so libvorbis end-trims to exactly the
// source length (a wrong shift would leave the decode short or long by ~1024).
func TestOggVorbisDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const frames = 24000
	wav, f, interleaved := vorbisTestWAV(t, frames)

	path := filepath.Join(t.TempDir(), "out.ogg")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: "vorbis"})
	if err != nil {
		out.Close()
		t.Fatalf("transcode: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if res.Samples != frames {
		t.Fatalf("Samples = %d, want %d", res.Samples, frames)
	}

	ref := testutil.FFprobeFile(t, path)
	if ref.CodecName != "vorbis" || ref.SampleRate != 48000 || ref.Channels != 2 {
		t.Errorf("ffprobe = %+v, want vorbis/48000/2", ref)
	}
	// Decode with libvorbis (not ffmpeg's experimental native Vorbis decoder).
	dec := testutil.FFmpegDecodeF32Codec(t, path, "libvorbis")
	decFrames := len(dec) / f.Channels
	// libvorbis reports the trimmed length exactly; a small tolerance absorbs
	// any single-block edge rounding without letting a lost/extra block through.
	if diff := decFrames - frames; diff < -64 || diff > 64 {
		t.Errorf("libvorbis decoded %d frames, want ~%d (granulepos end-trim off by %d)", decFrames, frames, diff)
	}
	// The libvorbis-decoded audio must match the source within a lossy bound,
	// confirming it is the same signal (aligned, not garbage).
	common := min(decFrames, frames)
	lv := audio.Get(f, common)
	defer audio.Put(lv)
	lv.N = common
	for c := 0; c < f.Channels; c++ {
		ch := lv.ChanF(c)
		for i := 0; i < common; i++ {
			ch[i] = dec[i*f.Channels+c]
		}
	}
	if e := nrmse(lv, interleaved, f.Channels); e > 0.25 {
		t.Errorf("libvorbis decode NRMSE %.3f exceeds 0.25 (not the same signal)", e)
	}
}

// TestTranscodeVorbisMatroska pins Vorbis in Matroska and WebM: the encoder
// reaches the mka muxer through the container override, the A_VORBIS CodecPrivate
// is the PackHeaders blob unchanged, and a decode round trip is gapless-exact and
// within a lossy bound.
func TestTranscodeVorbisMatroska(t *testing.T) {
	e := waxflow.New()
	const frames = 20000
	wav, f, interleaved := vorbisTestWAV(t, frames)

	for _, container_ := range []string{"mka", "webm"} {
		t.Run(container_, func(t *testing.T) {
			out := &memWS{}
			res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
				waxflow.TranscodeOptions{Format: "vorbis", Container: container_})
			if err != nil {
				t.Fatalf("transcode vorbis/%s: %v", container_, err)
			}
			if res.Samples != frames {
				t.Fatalf("Samples = %d, want %d (gapless exact)", res.Samples, frames)
			}
			got := readAll(t, e, out.b, frames)
			defer audio.Put(got)
			if got.N != frames {
				t.Errorf("decoded %d frames, want %d", got.N, frames)
			}
			if e := nrmse(got, interleaved, f.Channels); e > 0.25 {
				t.Errorf("vorbis-in-%s decode NRMSE %.3f exceeds 0.25", container_, e)
			}
		})
	}
}
