package waxflow_test

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/waxerr"
)

type memWS struct {
	b   []byte
	pos int64
}

func (w *memWS) Write(p []byte) (int, error) {
	if need := w.pos + int64(len(p)); need > int64(len(w.b)) {
		grown := make([]byte, need)
		copy(grown, w.b)
		w.b = grown
	}
	copy(w.b[w.pos:], p)
	w.pos += int64(len(p))
	return len(p), nil
}

func (w *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = off
	case io.SeekCurrent:
		w.pos += off
	case io.SeekEnd:
		w.pos = int64(len(w.b)) + off
	}
	return w.pos, nil
}

// synth fills a buffer with deterministic pseudo-random samples spanning
// the format's full range, extremes included.
func synth(b *audio.Buffer, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed))
	for c := 0; c < b.Fmt.Channels; c++ {
		if b.Fmt.Type == audio.Int {
			s := b.ChanI(c)
			lo := int32(-1) << (b.Fmt.BitDepth - 1)
			hi := -(lo + 1)
			// uint64(lo) sign-extends, so hi-lo is computed modulo 2^64;
			// the wrapped difference is exactly the span (2^depth - 1),
			// including at depth 32.
			for i := range s {
				s[i] = lo + int32(rng.Uint64N(uint64(hi)-uint64(lo)+1))
			}
			if len(s) >= 2 {
				s[0], s[1] = lo, hi
			}
		} else {
			s := b.ChanF(c)
			for i := range s {
				s[i] = float32(rng.Float64()*2 - 1)
			}
			if len(s) >= 2 {
				s[0], s[1] = -1, 1
			}
		}
	}
}

// makeWAV builds an in-memory WAV with the given wire config and returns
// the file plus the source-of-truth buffer.
func makeWAV(t *testing.T, cfg pcm.Config, channels, frames int, seed uint64) ([]byte, *audio.Buffer) {
	t.Helper()
	f := cfg.PCMFormat(48000, channels, audio.DefaultLayout(channels))
	src := audio.Get(f, frames)
	src.N = frames
	synth(src, seed)

	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return ws.b, src
}

// readAll decodes a container file back to one buffer via the facade.
func readAll(t *testing.T, e *waxflow.Engine, raw []byte, frames int) *audio.Buffer {
	t.Helper()
	med, err := e.OpenStream(container.BytesSource(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	out := audio.Get(f, frames)
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.N+tmp.N > out.Cap() {
			t.Fatalf("decoded more than the expected %d frames", frames)
		}
		for c := 0; c < f.Channels; c++ {
			if f.Type == audio.Float {
				copy(out.F[c*out.Stride+out.N:c*out.Stride+out.N+tmp.N], tmp.ChanF(c))
			} else {
				copy(out.I[c*out.Stride+out.N:c*out.Stride+out.N+tmp.N], tmp.ChanI(c))
			}
		}
		out.N += tmp.N
	}
	return out
}

func equalPCM(t *testing.T, want, got *audio.Buffer) {
	t.Helper()
	if want.Fmt != got.Fmt {
		t.Fatalf("format %v, want %v", got.Fmt, want.Fmt)
	}
	if want.N != got.N {
		t.Fatalf("frames %d, want %d", got.N, want.N)
	}
	for c := 0; c < want.Fmt.Channels; c++ {
		if want.Fmt.Type == audio.Int {
			w, g := want.ChanI(c), got.ChanI(c)
			for i := range w {
				if w[i] != g[i] {
					t.Fatalf("ch%d[%d] = %d, want %d", c, i, g[i], w[i])
				}
			}
		} else {
			w, g := want.ChanF(c), got.ChanF(c)
			for i := range w {
				if math.Float32bits(w[i]) != math.Float32bits(g[i]) {
					t.Fatalf("ch%d[%d] = %v, want %v", c, i, g[i], w[i])
				}
			}
		}
	}
}

// TestTranscodeBitExactRoundTrips is the M1 exit criterion: WAV to AIFF
// and back, across bit depths, float, EXTENSIBLE valid bits, and
// multichannel, must reproduce the source samples bit for bit.
func TestTranscodeBitExactRoundTrips(t *testing.T) {
	matrix := []struct {
		name     string
		cfg      pcm.Config
		channels int
	}{
		{"u8 mono", pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, 1},
		{"s16 stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2},
		{"s24 stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 2},
		{"s32 stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, 2},
		{"s24in32 extensible", pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24}, 2},
		{"f32 stereo", pcm.Config{Encoding: pcm.Float, Bits: 32}, 2},
		{"s16 5.1", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 6},
	}
	e := waxflow.New()
	const frames = 9111 // not a multiple of the chunk size
	for _, tt := range matrix {
		t.Run(tt.name, func(t *testing.T) {
			wav, src := makeWAV(t, tt.cfg, tt.channels, frames, 11)
			defer audio.Put(src)

			// WAV -> AIFF.
			aiffOut := &memWS{}
			res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", aiffOut, waxflow.TranscodeOptions{Format: "aiff"})
			if err != nil {
				t.Fatalf("wav->aiff: %v", err)
			}
			if res.Samples != frames {
				t.Fatalf("wav->aiff samples = %d, want %d", res.Samples, frames)
			}
			// AIFF -> WAV.
			wavOut := &memWS{}
			res, err = e.Transcode(context.Background(), container.BytesSource(aiffOut.b), "", wavOut, waxflow.TranscodeOptions{Format: "wav"})
			if err != nil {
				t.Fatalf("aiff->wav: %v", err)
			}
			if res.Samples != frames {
				t.Fatalf("aiff->wav samples = %d, want %d", res.Samples, frames)
			}

			got := readAll(t, e, wavOut.b, frames)
			defer audio.Put(got)
			equalPCM(t, src, got)
		})
	}
}

// TestTranscodeRF64RoundTrip covers the RF64 read path through the whole
// engine: a file written past a tiny size limit decodes identically.
func TestTranscodeRF64RoundTrip(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 24}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	const frames = 2000
	src := audio.Get(f, frames)
	defer audio.Put(src)
	src.N = frames
	synth(src, 13)

	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, &riff.MuxerOptions{SizeLimit: 1024})
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: frames, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	if string(ws.b[:4]) != "RF64" {
		t.Fatalf("fixture header = %q, want RF64", ws.b[:4])
	}

	e := waxflow.New()
	info, err := e.Probe(container.BytesSource(ws.b), "", &waxflow.ProbeOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "wav" || info.Default().Samples != frames {
		t.Fatalf("RF64 probe = %+v", info)
	}
	got := readAll(t, e, ws.b, frames)
	defer audio.Put(got)
	equalPCM(t, src, got)
}

func TestTranscodeCancellation(t *testing.T) {
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 100, 17)
	audio.Put(src)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waxflow.New().Transcode(ctx, container.BytesSource(wav), "", &memWS{}, waxflow.TranscodeOptions{Format: "wav"})
	if !errors.Is(err, waxerr.ErrCanceled) {
		t.Errorf("err = %v, want canceled", err)
	}
}

func TestTranscodeRejections(t *testing.T) {
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 100, 19)
	audio.Put(src)
	e := waxflow.New()

	_, err := e.Transcode(context.Background(), container.BytesSource(wav), "", &memWS{}, waxflow.TranscodeOptions{Format: "opus"})
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("opus output err = %v, want unsupported-format (not registered until its milestone)", err)
	}
	_, err = e.Transcode(context.Background(), container.BytesSource(wav), "", &memWS{}, waxflow.TranscodeOptions{})
	if !errors.Is(err, waxerr.ErrInvalidRequest) {
		t.Errorf("empty format err = %v, want invalid-request", err)
	}
	// AIFF needs a seekable destination; a bare writer must fail cleanly.
	var sink onlyWriter
	_, err = e.Transcode(context.Background(), container.BytesSource(wav), "", sink, waxflow.TranscodeOptions{Format: "aiff"})
	if err == nil {
		t.Error("aiff to unseekable writer must fail")
	}
	_, err = e.Transcode(context.Background(), container.BytesSource([]byte("garbage")), "", &memWS{}, waxflow.TranscodeOptions{Format: "wav"})
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("garbage input err = %v, want unsupported-format", err)
	}
}

type onlyWriter struct{}

func (onlyWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestOutputTable pins the writer-side capability table: names, extension
// mapping (both spellings), and its agreement with the read-side exts.
func TestOutputTable(t *testing.T) {
	if got := waxflow.OutputFormats(); len(got) != 2 || got[0] != "wav" || got[1] != "aiff" {
		t.Errorf("OutputFormats() = %v", got)
	}
	tests := []struct{ ext, want string }{
		{"wav", "wav"}, {".WAV", "wav"}, {"rf64", "wav"}, {"bw64", "wav"},
		{"aif", "aiff"}, {".aiff", "aiff"}, {"aifc", "aiff"}, {"afc", "aiff"},
		{"xyz", ""}, {"", ""},
	}
	for _, tt := range tests {
		if got := waxflow.OutputFormatForExt(tt.ext); got != tt.want {
			t.Errorf("OutputFormatForExt(%q) = %q, want %q", tt.ext, got, tt.want)
		}
	}
}
