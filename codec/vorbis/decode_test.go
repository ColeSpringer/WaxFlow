package vorbis

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// oggPackets splits a single-stream Ogg file into its logical packets. It is a
// minimal reader for tests only; the real demuxer lives in container/ogg.
func oggPackets(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var packets [][]byte
	var partial []byte
	off := 0
	for off+27 <= len(data) {
		if string(data[off:off+4]) != "OggS" {
			t.Fatalf("no page capture at %d", off)
		}
		nsegs := int(data[off+26])
		if off+27+nsegs > len(data) {
			break
		}
		lacing := data[off+27 : off+27+nsegs]
		body := off + 27 + nsegs
		segLen := 0
		for _, l := range lacing {
			segLen += int(l)
		}
		if body+segLen > len(data) {
			break
		}
		p := body
		run := 0
		for _, l := range lacing {
			run += int(l)
			if l < 255 {
				partial = append(partial, data[p:p+run]...)
				packets = append(packets, partial)
				partial = nil
				p += run
				run = 0
			}
		}
		if run > 0 {
			partial = append(partial, data[p:p+run]...)
		}
		off = body + segLen
	}
	return packets
}

// decodeAll runs every audio packet through the decoder and returns
// interleaved float32 samples in the decoder's output (WAVE) order.
func decodeAll(t *testing.T, cfg Config, f audio.Format, packets [][]byte) []float32 {
	t.Helper()
	d, err := NewDecoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	var out []float32
	emit := func(b *audio.Buffer) error {
		for i := 0; i < b.N; i++ {
			for c := 0; c < b.Fmt.Channels; c++ {
				out = append(out, b.ChanF(c)[i])
			}
		}
		return nil
	}
	for _, pkt := range packets {
		if len(pkt) == 0 || pkt[0]&1 != 0 {
			continue // header packets have the low bit set (type is odd)
		}
		if err := d.Decode(pkt, emit); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out
}

// bestAlign finds the offset and scale minimizing the RMS between one channel
// of mine and ref (both interleaved with ch channels), searching offsets in
// [0, maxOff] frames. Vorbis output is un-trimmed, so it leads ref by the
// codec's priming; this recovers the alignment the way a listener would.
func bestAlign(mine, ref []float32, ch, maxOff int) (off int, rms, scale float64) {
	refFrames := len(ref) / ch
	best := math.Inf(1)
	bestOff, bestScale := 0, 1.0
	for o := 0; o <= maxOff; o++ {
		if (o+refFrames)*ch > len(mine) {
			break
		}
		// Optimal least-squares scale for this offset, then residual RMS with
		// it: isolates waveform-shape error from any constant scale factor.
		var dot, en, refEn float64
		for i := 0; i < refFrames*ch; i++ {
			m := float64(mine[o*ch+i])
			rr := float64(ref[i])
			dot += m * rr
			en += m * m
			refEn += rr * rr
		}
		s := 1.0
		if en > 0 {
			s = dot / en
		}
		var sum float64
		for i := 0; i < refFrames*ch; i++ {
			d := s*float64(mine[o*ch+i]) - float64(ref[i])
			sum += d * d
		}
		if r := math.Sqrt(sum / float64(refFrames*ch)); r < best {
			best, bestOff, bestScale = r, o, s
		}
	}
	return bestOff, best, bestScale
}

func TestDecodeDifferential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sine.ogg")
	for _, tc := range []struct {
		name     string
		channels int
		rate     int
	}{
		{"mono44k", 1, 44100},
		{"stereo44k", 2, 44100},
		{"stereo48k", 2, 48000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.FFmpegGenerate(t, path, tc.rate, tc.channels, "libvorbis", "-q:a", "5")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			packets := oggPackets(t, data)
			if len(packets) < 4 {
				t.Fatalf("only %d packets", len(packets))
			}
			cfg, err := ParseHeaders(packets[0], packets[1], packets[2])
			if err != nil {
				t.Fatalf("headers: %v", err)
			}
			if cfg.Channels != tc.channels || cfg.Rate != tc.rate {
				t.Fatalf("config %dch %dHz, want %dch %dHz", cfg.Channels, cfg.Rate, tc.channels, tc.rate)
			}
			mine := decodeAll(t, cfg, cfg.Format(), packets[3:])
			ref := testutil.FFmpegDecodeF32(t, path)
			if len(mine) == 0 || len(ref) == 0 {
				t.Fatalf("empty decode: mine=%d ref=%d", len(mine), len(ref))
			}
			off, rms, scale := bestAlign(mine, ref, tc.channels, cfg.blockSizes[1])
			t.Logf("aligned off=%d shapeRMS=%.6g scale=%.6g mineFrames=%d refFrames=%d",
				off, rms, scale, len(mine)/tc.channels, len(ref)/tc.channels)
			if rms > 1e-3 {
				t.Errorf("shape RMS %.6g exceeds 1e-3", rms)
			}
		})
	}
}
