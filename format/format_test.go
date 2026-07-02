package format

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/waxerr"
)

// memWS is an in-memory io.WriteSeeker.
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

// buildFile encodes a deterministic mono int16 ramp (sample i has value
// i%3000-1500) into a WAV or AIFF, so tests can verify positions by value.
func buildFile(t testing.TB, kind string, frames int) []byte {
	t.Helper()
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: kind == "aiff"}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	var m container.Muxer
	if kind == "aiff" {
		m = aiff.NewMuxer(ws)
	} else {
		m = riff.NewMuxer(ws, nil)
	}
	cfgBytes, err := cfg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.PCM, CodecConfig: cfgBytes, Fmt: f, Samples: int64(frames), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	buf := audio.Get(f, frames)
	defer audio.Put(buf)
	buf.N = frames
	s := buf.ChanI(0)
	for i := range s {
		s[i] = sampleAt(int64(i))
	}
	if err := enc.Encode(buf, func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return ws.b
}

func sampleAt(pos int64) int32 { return int32(pos%3000 - 1500) }

func TestProbeIdentifies(t *testing.T) {
	tests := []struct {
		kind  string
		codec codec.ID
	}{
		{"wav", codec.PCM},
		{"aiff", codec.PCM},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			raw := buildFile(t, tt.kind, 500)
			info, err := Probe(container.BytesSource(raw), "", &Options{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if info.Container != tt.kind {
				t.Errorf("container = %q, want %q", info.Container, tt.kind)
			}
			d := info.Default()
			if d.Codec != tt.codec || d.Samples != 500 || d.Fmt.Rate != 48000 {
				t.Errorf("default track = %+v", d)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings = %v, want none", info.Warnings)
			}
		})
	}
}

func TestProbeMagicBeatsHint(t *testing.T) {
	raw := buildFile(t, "wav", 10)
	info, err := Probe(container.BytesSource(raw), "aiff", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "wav" {
		t.Errorf("container = %q; the sniff table outranks the extension hint", info.Container)
	}
}

func TestProbeHintTiebreak(t *testing.T) {
	// Corrupt the magic so only the extension hint can pick the driver;
	// the driver then reports its own structural error.
	raw := buildFile(t, "wav", 10)
	copy(raw, "JUNK")
	if _, err := Probe(container.BytesSource(raw), ".wav", nil); !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("err = %v, want unsupported-format from the hinted driver", err)
	}
	// Without a hint the generic unrecognized error surfaces.
	_, err := Probe(container.BytesSource(raw), "", nil)
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("err = %v, want unsupported-format", err)
	}
}

func TestProbeUnrecognized(t *testing.T) {
	_, err := Probe(container.BytesSource([]byte("definitely not audio data, longer than any magic")), "", nil)
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("err = %v, want unsupported-format", err)
	}
}

func TestProbeSkipsID3v2(t *testing.T) {
	wav := buildFile(t, "wav", 20)
	tag := make([]byte, 10+64) // 64-byte tag body
	copy(tag, "ID3")
	tag[3], tag[4] = 4, 0
	tag[9] = 64
	raw := append(tag, wav...)
	info, err := Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "wav" || info.Default().Samples != 20 {
		t.Errorf("probe through ID3v2 = %+v, %v", info, err)
	}
}

func TestMediaReadChunkStampsPositions(t *testing.T) {
	const frames = 10000
	for _, kind := range []string{"wav", "aiff"} {
		t.Run(kind, func(t *testing.T) {
			raw := buildFile(t, kind, frames)
			med, err := Open(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			f := med.Info().Default().Fmt

			dst := audio.Get(f, 1000)
			defer audio.Put(dst)
			var pos, total int64
			for {
				err := med.ReadChunk(dst)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if dst.Pos != pos {
					t.Fatalf("chunk pos = %d, want %d", dst.Pos, pos)
				}
				if dst.Discont {
					t.Fatal("linear read must not flag a discontinuity")
				}
				s := dst.ChanI(0)
				for i, v := range s {
					if v != sampleAt(dst.Pos+int64(i)) {
						t.Fatalf("sample at %d = %d, want %d", dst.Pos+int64(i), v, sampleAt(dst.Pos+int64(i)))
					}
				}
				pos += int64(dst.N)
				total += int64(dst.N)
			}
			if total != frames {
				t.Errorf("read %d frames, want %d", total, frames)
			}
		})
	}
}

func TestMediaSeekSampleExact(t *testing.T) {
	const frames = 10000
	raw := buildFile(t, "wav", frames)
	med, err := Open(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	dst := audio.Get(f, 512)
	defer audio.Put(dst)

	for _, target := range []int64{1, 4095, 4096, 4097, 7777, 0, 9999} {
		landed, err := med.SeekSample(target)
		if err != nil {
			t.Fatalf("SeekSample(%d): %v", target, err)
		}
		if landed != target {
			t.Fatalf("SeekSample(%d) landed %d; PCM seeks are sample-exact", target, landed)
		}
		if err := med.ReadChunk(dst); err != nil {
			t.Fatalf("ReadChunk after seek to %d: %v", target, err)
		}
		if dst.Pos != target || !dst.Discont {
			t.Fatalf("post-seek chunk pos=%d discont=%v, want pos=%d discont=true", dst.Pos, dst.Discont, target)
		}
		for i, v := range dst.ChanI(0) {
			if v != sampleAt(target+int64(i)) {
				t.Fatalf("post-seek sample at +%d = %d, want %d", i, v, sampleAt(target+int64(i)))
			}
		}
		// The chunk after a seek is back to a linear read.
		if err := med.ReadChunk(dst); err == nil && dst.Discont {
			t.Fatal("second chunk after seek must not flag a discontinuity")
		}
	}

	// Past-the-end seeks land at the end and read EOF.
	landed, err := med.SeekSample(frames + 100)
	if err != nil {
		t.Fatal(err)
	}
	if landed != frames {
		t.Errorf("past-end seek landed %d, want %d", landed, frames)
	}
	if err := med.ReadChunk(dst); !errors.Is(err, io.EOF) {
		t.Errorf("read after past-end seek = %v, want EOF", err)
	}
	if _, err := med.SeekSample(-1); err == nil {
		t.Error("negative seek must fail")
	}
}

// boundaryEOFSource exercises the io.ReaderAt contract corner: a read
// ending exactly at the end of the source returns (len(p), io.EOF), which
// the contract explicitly permits. Demuxers must treat that as a
// successful read, and media must not mistake the wrapped io.EOF of a
// real failure for a clean end of stream.
type boundaryEOFSource struct {
	container.Source
}

func (s boundaryEOFSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.Source.ReadAt(p, off)
	if err == nil && off+int64(n) == s.Size() {
		return n, io.EOF
	}
	return n, err
}

// TestBoundaryEOFSourceDecodesFully is the regression for the silently
// dropped final chunk: with a spec-compliant boundary-EOF source, every
// frame must still arrive, and a near-end seek must still land exactly.
func TestBoundaryEOFSourceDecodesFully(t *testing.T) {
	const frames = 10000
	raw := buildFile(t, "wav", frames)
	src := boundaryEOFSource{container.BytesSource(raw)}

	med, err := Open(src, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	dst := audio.Get(f, 1000)
	defer audio.Put(dst)

	var total int64
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		for i, v := range dst.ChanI(0) {
			if v != sampleAt(dst.Pos+int64(i)) {
				t.Fatalf("sample at %d = %d, want %d", dst.Pos+int64(i), v, sampleAt(dst.Pos+int64(i)))
			}
		}
		total += int64(dst.N)
	}
	if total != frames {
		t.Fatalf("decoded %d frames, want %d (final chunk dropped)", total, frames)
	}

	// Seek into the very last packet: pre-roll crosses the boundary read.
	target := int64(frames - 3)
	landed, err := med.SeekSample(target)
	if err != nil || landed != target {
		t.Fatalf("SeekSample(%d) = %d, %v", target, landed, err)
	}
	if err := med.ReadChunk(dst); err != nil {
		t.Fatal(err)
	}
	if dst.Pos != target || dst.N != 3 {
		t.Fatalf("post-seek chunk pos=%d n=%d, want pos=%d n=3", dst.Pos, dst.N, target)
	}
}

// failingSource returns a non-EOF error on every read past a threshold,
// simulating a transient I/O failure mid-file.
type failingSource struct {
	container.Source
	failAt int64
}

func (s failingSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.failAt {
		return 0, errors.New("simulated I/O failure")
	}
	return s.Source.ReadAt(p, off)
}

// TestReadFailureIsNotEOF pins the other half of the contract: a genuine
// mid-stream read failure must surface as an error, never as a clean
// (and short) end of stream.
func TestReadFailureIsNotEOF(t *testing.T) {
	const frames = 10000
	raw := buildFile(t, "wav", frames)
	src := failingSource{container.BytesSource(raw), int64(len(raw)) / 2}

	med, err := Open(src, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	dst := audio.Get(f, 1000)
	defer audio.Put(dst)
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			t.Fatal("mid-file I/O failure surfaced as clean EOF")
		}
		if err != nil {
			if !errors.Is(err, waxerr.ErrSourceUnreadable) {
				t.Fatalf("err = %v, want source-unreadable", err)
			}
			return
		}
	}
}

// TestProbeSurfacesHeadReadFailure pins that a failing source is reported
// as an I/O problem, not misclassified as an unsupported format.
func TestProbeSurfacesHeadReadFailure(t *testing.T) {
	src := failingSource{container.BytesSource(make([]byte, 1024)), 0}
	_, err := Probe(src, "", nil)
	if !errors.Is(err, waxerr.ErrSourceUnreadable) {
		t.Errorf("err = %v, want source-unreadable", err)
	}
}

func TestMediaRejectsWrongBufferFormat(t *testing.T) {
	raw := buildFile(t, "wav", 100)
	med, err := Open(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	wrong := audio.Get(audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, 64)
	defer audio.Put(wrong)
	if err := med.ReadChunk(wrong); !errors.Is(err, waxerr.ErrInvalidRequest) {
		t.Errorf("ReadChunk with wrong format = %v, want invalid-request", err)
	}
}

func TestMediaCloseIdempotent(t *testing.T) {
	raw := buildFile(t, "wav", 100)
	med, err := Open(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if med.Close() != nil || med.Close() != nil {
		t.Error("Close must be idempotent and nil")
	}
	f := med.Info().Default().Fmt
	dst := audio.Get(f, 64)
	defer audio.Put(dst)
	if err := med.ReadChunk(dst); err == nil {
		t.Error("ReadChunk after Close must fail")
	}
}
