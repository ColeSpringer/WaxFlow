package adts_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/internal/testutil"
)

// encodeADTS drives the AAC encoder into the ADTS muxer.
func encodeADTS(t *testing.T, f audio.Format, src [][]float32) ([]byte, codec.Trailer) {
	t.Helper()
	enc, err := aac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := adts.NewMuxer(&out)
	track := container.Track{ID: 0, Codec: codec.AACLC, CodecConfig: enc.CodecConfig(),
		Fmt: f, Samples: int64(len(src[0]))}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	write := func(p codec.Packet) error {
		return mux.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	n := len(src[0])
	for off := 0; off < n; off += 1024 {
		end := min(off+1024, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off
		for c := 0; c < f.Channels; c++ {
			copy(buf.ChanF(c), src[c][off:end])
		}
		if err := enc.Encode(buf, write); err != nil {
			t.Fatal(err)
		}
		audio.Put(buf)
	}
	tr, err := enc.Finish(write)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(tr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), tr
}

func museSrc(n, ch, rate int) [][]float32 {
	src := make([][]float32, ch)
	for c := range src {
		src[c] = make([]float32, n)
		state := uint32(7 + c)
		for i := range src[c] {
			state = state*1664525 + 1013904223
			ti := float64(i) / float64(rate)
			v := 0.3*math.Sin(2*math.Pi*440*ti) +
				0.15*math.Sin(2*math.Pi*2093*ti+float64(c)) +
				0.05*float64(int32(state))/(1<<31)
			src[c][i] = float32(v)
		}
	}
	return src
}

// TestMuxRoundTrip re-demuxes our own ADTS output and checks stream
// parameters and total sample coverage (ADTS has no trims, so the raw
// decode includes delay and padding).
func TestMuxRoundTrip(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	const n = 44100
	stream, tr := encodeADTS(t, f, museSrc(n, 2, 44100))

	d, err := adts.NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracks := d.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("%d tracks", len(tracks))
	}
	if tracks[0].Fmt.Rate != 44100 || tracks[0].Fmt.Channels != 2 {
		t.Fatalf("track format %v", tracks[0].Fmt)
	}
	// ADTS has no length signaling, so the demuxer honestly reports -1;
	// the packet walk must still cover every encoded frame.
	if tracks[0].Samples != -1 {
		t.Fatalf("demuxed Samples %d, want -1 (ADTS has no length signaling)", tracks[0].Samples)
	}
	var pkt container.Packet
	frames := 0
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		frames++
	}
	total := tr.Delay + tr.Samples + tr.Padding
	if int64(frames)*1024 != total {
		t.Fatalf("demuxed %d frames (%d samples), encoder wrote %d", frames, frames*1024, total)
	}
}

// TestMuxFFmpegDifferential is the external oracle: ffmpeg must decode
// our ADTS stream to the source within the encoder's quality band.
func TestMuxFFmpegDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	const n = 44100
	src := museSrc(n, 2, 44100)
	stream, tr := encodeADTS(t, f, src)

	path := filepath.Join(t.TempDir(), "enc.aac")
	if err := os.WriteFile(path, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	// ffmpeg output is interleaved with no trims (ADTS carries none).
	want := int((tr.Delay + tr.Samples + tr.Padding)) * 2
	if len(got) != want {
		t.Fatalf("ffmpeg decoded %d samples, want %d", len(got), want)
	}
	var sig, errE float64
	off := int(tr.Delay)
	for i := 0; i < n; i++ {
		for c := 0; c < 2; c++ {
			s := float64(src[c][i])
			e := s - float64(got[(off+i)*2+c])
			sig += s * s
			errE += e * e
		}
	}
	snr := 10 * math.Log10(sig/errE)
	t.Logf("ffmpeg differential SNR %.1f dB", snr)
	if snr < 15 {
		t.Fatalf("SNR %.1f dB below 15", snr)
	}
}

// TestMuxFFmpegTNSDifferential runs a strong-temporal-envelope signal
// (TNS engages on nearly every frame) through ffmpeg: the reference
// decoder must agree with the encoder's TNS side data and filtering.
func TestMuxFFmpegTNSDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	f := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1),
		Type: audio.Float, BitDepth: 32}
	const n = 32768
	src := [][]float32{make([]float32, n)}
	for i := range src[0] {
		src[0][i] = float32(0.6 * math.Exp(-float64(i%441)/40))
	}
	stream, tr := encodeADTS(t, f, src)
	path := filepath.Join(t.TempDir(), "tns.aac")
	if err := os.WriteFile(path, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	var sig, errE float64
	off := int(tr.Delay)
	for i := 0; i < n; i++ {
		s := float64(src[0][i])
		e := s - float64(got[off+i])
		sig += s * s
		errE += e * e
	}
	snr := 10 * math.Log10(sig/errE)
	t.Logf("TNS ffmpeg differential SNR %.1f dB", snr)
	if snr < 15 {
		t.Fatalf("SNR %.1f dB below 15", snr)
	}
}
