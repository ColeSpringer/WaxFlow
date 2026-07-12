package oracletest

import (
	"bytes"
	"math"
	"testing"

	"github.com/jfreymuth/oggvorbis"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestVorbisEncodeJfreymuthDecode cross-checks our Vorbis encoder against an
// independent pure-Go decoder (jfreymuth/oggvorbis), the way go-mp3 backstops
// the MP3 path. It proves a third-party implementation accepts our headers and
// packets and reconstructs the signal, independent of ffmpeg/libvorbis: our own
// decoder could share a blind spot with the encoder, a foreign one will not.
func TestVorbisEncodeJfreymuthDecode(t *testing.T) {
	const rate = 44100
	for _, ch := range []int{1, 2} {
		f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		n := rate // 1 second
		src := make([][]float32, ch)
		for c := range src {
			src[c] = make([]float32, n)
			for i := range src[c] {
				src[c][i] = float32(0.4*math.Sin(2*math.Pi*float64(i)*(440+float64(c)*110)/rate) +
					0.2*math.Sin(2*math.Pi*float64(i)*1600/rate))
			}
		}

		e, err := vorbis.NewEncoder(f, nil)
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
			end := off + 1024
			if end > n {
				end = n
			}
			buf := audio.Get(f, end-off)
			buf.N = end - off
			for c := 0; c < ch; c++ {
				copy(buf.ChanF(c), src[c][off:end])
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
		ogg := testutil.OggVorbisFile(id, comment, setup, packets, granules, tr.Samples)

		got, format, err := oggvorbis.ReadAll(bytes.NewReader(ogg))
		if err != nil {
			t.Fatalf("ch=%d: jfreymuth/oggvorbis rejected our stream: %v", ch, err)
		}
		if format.Channels != ch || format.SampleRate != rate {
			t.Fatalf("ch=%d: oracle read %dch %dHz", ch, format.Channels, format.SampleRate)
		}
		frames := len(got) / ch
		if frames < n/2 {
			t.Fatalf("ch=%d: oracle decoded only %d frames from %d input", ch, frames, n)
		}

		// The oracle output is untrimmed; align by correlation and check the
		// reconstruction tracks the source (a foreign decoder producing the
		// right waveform is the real cross-check).
		ref := make([]float32, n*ch)
		for i := 0; i < n; i++ {
			for c := 0; c < ch; c++ {
				ref[i*ch+c] = src[c][i]
			}
		}
		if nrmse := bestShapeNRMSE(got, ref, ch, 4096); nrmse > 0.3 {
			t.Errorf("ch=%d: oracle-decoded shape NRMSE %.3f too high", ch, nrmse)
		}
	}
}

// bestShapeNRMSE returns the least normalized-RMS error of test against ref
// (both interleaved) over frame offsets [0, maxOff], absorbing the codec lead.
func bestShapeNRMSE(test, ref []float32, ch, maxOff int) float64 {
	refFrames := len(ref) / ch
	best := math.Inf(1)
	for o := 0; o <= maxOff; o++ {
		if (o+refFrames)*ch > len(test) {
			break
		}
		var se, ss float64
		for i := 0; i < refFrames*ch; i++ {
			d := float64(test[o*ch+i]) - float64(ref[i])
			se += d * d
			ss += float64(ref[i]) * float64(ref[i])
		}
		if e := se / ss; e < best {
			best = e
		}
	}
	return math.Sqrt(best)
}
