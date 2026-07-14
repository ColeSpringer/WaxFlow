package vorbis

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The Ogg-Vorbis test packer lives in internal/testutil (OggVorbisFile) so the
// tests/ quality gate shares it; it frames raw Vorbis packets so ffmpeg can
// decode our stream before the production Ogg-Vorbis muxer exists.

// TestEncodeFFmpegDecode is the other half of the validity gate: libvorbis
// (via ffmpeg) decodes our encoded stream, and the reconstruction tracks the
// source under the same lossy bound the in-house round-trip uses. It skips
// without ffmpeg.
func TestEncodeFFmpegDecode(t *testing.T) {
	testutil.FFmpeg(t)
	const rate = 44100
	for _, ch := range []int{1, 2} {
		f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		n := rate // 1 second
		src := make([][]float32, ch)
		for c := range src {
			src[c] = make([]float32, n)
			for i := range src[c] {
				src[c][i] = float32(0.4*math.Sin(2*math.Pi*float64(i)*(440+float64(c)*110)/rate) +
					0.15*math.Sin(2*math.Pi*float64(i)*2000/rate))
			}
		}

		e, err := NewEncoder(f, nil)
		if err != nil {
			t.Fatal(err)
		}
		packets, granules, tr := encodeSignal(t, e, src)
		id, comment, setup, err := splitHeaders(e.CodecConfig())
		if err != nil {
			t.Fatal(err)
		}
		ogg := testutil.OggVorbisFile(id, comment, setup, packets, granules, tr.Samples)

		path := filepath.Join(t.TempDir(), "ours.ogg")
		if err := os.WriteFile(path, ogg, 0o644); err != nil {
			t.Fatal(err)
		}
		// Decode with libvorbis (the reference), not ffmpeg's experimental native
		// Vorbis decoder, which mis-decodes some legal coupled streams.
		dec := testutil.FFmpegDecodeF32Codec(t, path, "libvorbis")
		if len(dec) == 0 {
			t.Fatalf("ch=%d: libvorbis decoded no samples from our stream", ch)
		}

		// Interleave the source and score shape error after a correlation
		// alignment (ffmpeg applies its own gapless trim, so lead differs).
		ref := interleave(src)
		off, nrmse := shapeError(dec, ref, ch, e.long)
		t.Logf("ch=%d: ffmpeg decoded %d frames, off=%d NRMSE=%.4f", ch, len(dec)/ch, off, nrmse)
		// A loose validity bound: the validity configuration has no psychoacoustics
		// and a coarse scalar
		// residue book, and ffmpeg applies its own gapless trim against our
		// approximate test-packer granulepos, so shape error runs a little above
		// the exact in-house round-trip. The ODG gate is the real quality bar.
		if nrmse > 0.30 {
			t.Errorf("ch=%d: libvorbis-decoded shape error %.4f exceeds the lossy bound", ch, nrmse)
		}
	}
}

func interleave(planar [][]float32) []float32 {
	ch := len(planar)
	n := len(planar[0])
	out := make([]float32, n*ch)
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			out[i*ch+c] = planar[c][i]
		}
	}
	return out
}

// shapeError finds the frame offset of test against ref (both interleaved)
// minimizing the least-squares residual over ref's length, returning that
// offset and the normalized RMS. It absorbs the codec's priming lead.
func shapeError(test, ref []float32, ch, maxOff int) (int, float64) {
	refFrames := len(ref) / ch
	best, bestOff := math.Inf(1), 0
	for o := 0; o <= maxOff; o++ {
		if (o+refFrames)*ch > len(test) {
			break
		}
		var sqErr, sqSig float64
		for i := 0; i < refFrames*ch; i++ {
			d := float64(test[o*ch+i]) - float64(ref[i])
			sqErr += d * d
			sqSig += float64(ref[i]) * float64(ref[i])
		}
		if e := sqErr / sqSig; e < best {
			best, bestOff = e, o
		}
	}
	return bestOff, math.Sqrt(best)
}
