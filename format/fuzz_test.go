package format

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

// FuzzProbe drives the sniff table, every driver, and the Media pipeline
// with arbitrary bytes: no panics, no invalid tracks, bounded reads.
func FuzzProbe(f *testing.F) {
	f.Add(buildFile(f, "wav", 64), "")
	f.Add(buildFile(f, "aiff", 64), "aiff")
	f.Add([]byte("RIFF\x24\x00\x00\x00WAVE"), "wav")
	f.Add([]byte("FORM\x00\x00\x00\x12AIFF"), "")
	f.Add([]byte("ID3\x04\x00\x00\x00\x00\x00\x0aRIFF"), "")
	f.Add([]byte{}, "wav")
	for _, name := range []string{"sine-s16.flac", "sine-s16.oga"} {
		raw, err := os.ReadFile(filepath.Join("..", "testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw, "")
	}
	f.Add([]byte("fLaC\x80\x00\x00\x22"), "flac")
	f.Add([]byte("OggS\x00\x02"), "ogg")

	f.Fuzz(func(t *testing.T, data []byte, hint string) {
		src := container.BytesSource(data)
		info, err := Probe(src, hint, nil)
		if err != nil {
			return
		}
		if len(info.Tracks) == 0 {
			t.Fatal("probe succeeded with no tracks")
		}
		if verr := info.Default().Fmt.Valid(); verr != nil {
			t.Fatalf("probe accepted invalid format: %v", verr)
		}

		med, err := Open(src, hint, nil)
		if err != nil {
			return
		}
		defer med.Close()
		fmt := med.Info().Default().Fmt
		dst := audio.Get(fmt, audio.StandardChunk)
		defer audio.Put(dst)
		// Compressed codecs expand: a minimal constant-subframe FLAC
		// frame of ~12 bytes carries up to 65535 samples, about 1.34
		// StandardChunk-sized output chunks per input byte. Doubled for
		// margin; still catches unbounded emission.
		maxChunks := 2*int64(len(data)) + 4
		var total int64
		for i := int64(0); ; i++ {
			if i > maxChunks {
				t.Fatalf("media produced more than %d chunks from %d bytes", maxChunks, len(data))
			}
			err := med.ReadChunk(dst)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				break
			}
			if dst.N == 0 {
				t.Fatal("empty chunk must be EOF or error")
			}
			if dst.Pos != total {
				t.Fatalf("linear read pos = %d, want %d", dst.Pos, total)
			}
			total += int64(dst.N)
		}

		samples := med.Info().Default().Samples
		if samples > 0 {
			target := samples / 2
			landed, err := med.SeekSample(target)
			if err == nil {
				// Media pre-rolls to the exact sample; landing short of the
				// target is legal only when the stream really ends first
				// (declared length and actual content may disagree).
				if landed > target {
					t.Fatalf("seek to %d landed past it at %d", target, landed)
				}
				if err := med.ReadChunk(dst); err == nil {
					if dst.Pos != landed || !dst.Discont {
						t.Fatalf("post-seek chunk pos=%d discont=%v, want pos=%d", dst.Pos, dst.Discont, landed)
					}
				}
			}
		}
	})
}
