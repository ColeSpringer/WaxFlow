package waxflow_test

import (
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestVorbisEncoderQuality is the Vorbis half of the encoder-quality harness. It
// encodes the shared synthesized corpus with our Vorbis encoder and with
// ffmpeg's libvorbis at the same quality point, scores both against the source
// with the ODG-proxy, and enforces the docs/quality-gates.md Vorbis gate: our
// corpus mean is at least libvorbis's minus 0.2, and no track falls more than
// 0.5 below. It self-skips without ffmpeg; the nightly job sets the require and
// report variables.
//
// Both outputs decode through ffmpeg. Ours is framed into Ogg-Vorbis by the
// test packer (the production Ogg-Vorbis muxer is a later phase); the metric's
// alignment search absorbs the priming difference.
func TestVorbisEncoderQuality(t *testing.T) {
	testutil.EncoderQualityGate(t) // not part of the default loop; `make encoder-quality`
	testutil.FFmpeg(t)

	const rate = 44100
	const quality = 4.0 // libvorbis -q4 is ~128 kbps stereo, the gate point
	corpus := qualityCorpus(rate, 3*rate)

	type row struct {
		name             string
		ours, ref, delta float64
	}
	var rows []row
	var sumOurs, sumRef float64
	worst := math.Inf(1)

	for _, item := range corpus {
		f := audio.Format{Rate: rate, Channels: item.ch, Layout: audio.DefaultLayout(item.ch), Type: audio.Float, BitDepth: 32}
		wav := synthWAVFromSamples(t, f, item.samples)
		wavPath := filepath.Join(t.TempDir(), item.name+".wav")
		if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
			t.Fatal(err)
		}
		ref := item.samples

		// Our encoder, framed into Ogg-Vorbis, decoded by ffmpeg.
		ourPath := filepath.Join(t.TempDir(), item.name+".ours.ogg")
		if err := os.WriteFile(ourPath, encodeVorbisOgg(t, f, item.samples, quality), 0o644); err != nil {
			t.Fatal(err)
		}
		// Decode with libvorbis, not ffmpeg's experimental native Vorbis decoder
		// (which mis-decodes some legal coupled streams).
		ourDec := testutil.FFmpegDecodeF32Codec(t, ourPath, "libvorbis")
		ourODG := testutil.ODGProxy(ref, ourDec, rate, item.ch)

		// libvorbis at the matching quality point, decoded by libvorbis.
		refPath := testutil.FFmpegVorbisEncodeFile(t, wavPath, quality)
		refDec := testutil.FFmpegDecodeF32Codec(t, refPath, "libvorbis")
		refODG := testutil.ODGProxy(ref, refDec, rate, item.ch)

		delta := ourODG - refODG
		rows = append(rows, row{item.name, ourODG, refODG, delta})
		sumOurs += ourODG
		sumRef += refODG
		worst = math.Min(worst, delta)
		t.Logf("%-16s ours=%.3f libvorbis=%.3f delta=%+.3f", item.name, ourODG, refODG, delta)
	}

	meanOurs := sumOurs / float64(len(rows))
	meanRef := sumRef / float64(len(rows))
	t.Logf("corpus mean: ours=%.3f libvorbis=%.3f (delta %+.3f); worst per-track delta %+.3f",
		meanOurs, meanRef, meanOurs-meanRef, worst)

	if path := os.Getenv("WAXFLOW_QUALITY_REPORT"); path != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "<h1>Vorbis encoder-quality report</h1>\n")
		fmt.Fprintf(&b, "<p>ODG-proxy at libvorbis -q%.0f. Higher is better (0 imperceptible, -4 very annoying). Gate: mean &ge; libvorbis &minus; 0.2, no track &gt; 0.5 below.</p>\n", quality)
		fmt.Fprintf(&b, "<table border=1 cellpadding=4><tr><th>track</th><th>WaxFlow</th><th>libvorbis</th><th>delta</th></tr>\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%.3f</td><td>%.3f</td><td>%+.3f</td></tr>\n",
				html.EscapeString(r.name), r.ours, r.ref, r.delta)
		}
		fmt.Fprintf(&b, "<tr><th>mean</th><th>%.3f</th><th>%.3f</th><th>%+.3f</th></tr>\n", meanOurs, meanRef, meanOurs-meanRef)
		fmt.Fprintf(&b, "</table>\n")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing quality report: %v", err)
		}
		t.Logf("wrote quality report to %s", path)
	}

	if meanOurs < meanRef-0.2 {
		t.Errorf("corpus mean ODG %.3f below libvorbis mean %.3f - 0.2 (gate)", meanOurs, meanRef)
	}
	if worst < -0.5 {
		t.Errorf("worst per-track ODG delta %.3f exceeds the 0.5 allowance below libvorbis", worst)
	}
}

// encodeVorbisOgg encodes interleaved samples with our Vorbis encoder and frames
// the result into an Ogg-Vorbis byte stream (the test packer; the production
// muxer is a later phase).
func encodeVorbisOgg(t *testing.T, f audio.Format, interleaved []float32, quality float64) []byte {
	t.Helper()
	ch := f.Channels
	n := len(interleaved) / ch
	e, err := vorbis.NewEncoder(f, &vorbis.EncoderOptions{Quality: quality})
	if err != nil {
		t.Fatal(err)
	}
	var packets [][]byte
	var granules []int64
	emit := func(p codec.Packet) error {
		packets = append(packets, append([]byte(nil), p.Data...))
		granules = append(granules, p.PTS+p.Dur)
		return nil
	}
	for off := 0; off < n; off += 1024 {
		end := min(off+1024, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off
		for c := 0; c < ch; c++ {
			dst := buf.ChanF(c)
			for i := off; i < end; i++ {
				dst[i-off] = interleaved[i*ch+c]
			}
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
		audio.Put(buf)
	}
	tr, err := e.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	id, comment, setup, err := vorbis.SplitConfig(e.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	return testutil.OggVorbisFile(id, comment, setup, packets, granules, tr.Samples)
}
