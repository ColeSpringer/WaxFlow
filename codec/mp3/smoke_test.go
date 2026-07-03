package mp3

// Codec-level smoke tests against ffmpeg-generated frames, walking the
// elementary stream by header sizes (the real framing lives in
// container/mpa; this pins the decoder alone). The full differential,
// gapless, and seek gates live at the engine level.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// generate produces an MP3 with ffmpeg's lavfi sine source, without a
// Xing header so the raw decode aligns one to one with ffmpeg's output.
func generate(t *testing.T, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	all := append([]string{"-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=0.6"},
		args...)
	all = append(all, "-write_xing", "0", "-id3v2_version", "0", path)
	out, err := exec.Command(testutil.FFmpeg(t), all...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	return path
}

// frames splits a raw elementary stream into whole frames by header
// arithmetic.
func frames(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var out [][]byte
	off := 0
	for off+HeaderLen <= len(raw) {
		h, err := ParseHeader(raw[off:])
		if err != nil {
			t.Fatalf("frame header at %d: %v", off, err)
		}
		size := h.Size()
		if size == 0 || off+size > len(raw) {
			break
		}
		out = append(out, raw[off:off+size])
		off += size
	}
	if len(out) == 0 {
		t.Fatal("no frames found")
	}
	return out
}

func decodeStream(t *testing.T, raw []byte) ([]float32, audio.Format) {
	t.Helper()
	pkts := frames(t, raw)
	h, _ := ParseHeader(pkts[0])
	d, err := NewDecoder(h.PCMFormat())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	var got []float32
	for i, p := range pkts {
		err := d.Decode(p, func(b *audio.Buffer) error {
			got = append(got, testutil.InterleaveF(b)...)
			return nil
		})
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	return got, h.PCMFormat()
}

func TestDecodeDifferentialSmoke(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"mpeg1-cbr128.mp3", []string{"-ac", "2", "-c:a", "libmp3lame", "-b:a", "128k"}},
		{"mpeg1-cbr320-mono.mp3", []string{"-ac", "1", "-c:a", "libmp3lame", "-b:a", "320k"}},
		{"mpeg1-vbr.mp3", []string{"-ac", "2", "-c:a", "libmp3lame", "-q:a", "4"}},
		{"mpeg2-22050.mp3", []string{"-ac", "2", "-ar", "22050", "-c:a", "libmp3lame", "-b:a", "64k"}},
		{"mpeg25-8000.mp3", []string{"-ac", "1", "-ar", "8000", "-c:a", "libmp3lame", "-b:a", "16k"}},
		{"mpeg1-shine.mp3", []string{"-ac", "2", "-c:a", "libshine", "-b:a", "128k"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := generate(t, tt.name, tt.args...)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := decodeStream(t, raw)
			want := testutil.FFmpegDecodeF32(t, path)
			if len(got) != len(want) {
				t.Fatalf("decoded %d samples, ffmpeg %d", len(got), len(want))
			}
			d := testutil.CompareF32(got, want)
			t.Logf("diff: %v", d)
			if d.RMS > 1e-4 {
				t.Errorf("RMS %g exceeds 1e-4", d.RMS)
			}
			if d.MaxAbs > 1e-3 {
				t.Errorf("max abs %g exceeds 1e-3", d.MaxAbs)
			}
		})
	}
}

// TestSilenceOnDamage pins the constant-emission contract: a frame whose
// side info is garbage still emits exactly one silent frame.
func TestSilenceOnDamage(t *testing.T) {
	path := generate(t, "clean.mp3", "-ac", "2", "-c:a", "libmp3lame", "-b:a", "128k")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pkts := frames(t, raw)
	h, _ := ParseHeader(pkts[0])
	d, err := NewDecoder(h.PCMFormat())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()

	bad := append([]byte(nil), pkts[0]...)
	for i := HeaderLen; i < len(bad); i++ {
		bad[i] = 0xFF
	}
	n := 0
	err = d.Decode(bad, func(b *audio.Buffer) error {
		n = b.N
		for c := 0; c < b.Fmt.Channels; c++ {
			for _, v := range b.ChanF(c) {
				if v != 0 {
					t.Fatal("damaged frame emitted non-silence")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("damaged frame errored instead of silencing: %v", err)
	}
	if n != h.SamplesPerFrame() {
		t.Fatalf("emitted %d frames, want %d", n, h.SamplesPerFrame())
	}
}
