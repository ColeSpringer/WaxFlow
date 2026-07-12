package vorbis

import (
	"math"
	"math/rand"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// TestFloorPostRoundTrip proves the floor-1 differential encoding inverts the
// decoder's predictive step for every prediction/target pair, including the
// out-of-room high encoding.
func TestFloorPostRoundTrip(t *testing.T) {
	for _, rng := range []int{64, 86, 128, 256} {
		for pred := 0; pred < rng; pred++ {
			for target := 0; target < rng; target++ {
				val := floor1EncodeVal(pred, target, rng)
				if val < 0 {
					t.Fatalf("rng=%d pred=%d target=%d: negative val %d", rng, pred, target, val)
				}
				got := floor1DecodeVal(pred, val, rng)
				if got != target {
					t.Fatalf("rng=%d pred=%d target=%d -> val=%d -> decoded %d", rng, pred, target, val, got)
				}
			}
		}
	}
}

// TestSetupHeaderParses confirms the serialized three headers parse back into a
// Config the decoder accepts, matching the declared geometry: the surest check
// that setup serialization inverts the parser field for field.
func TestSetupHeaderParses(t *testing.T) {
	for _, ch := range []int{1, 2} {
		cfg := newEncConfig(ch, 44100)
		blob := cfg.codecConfig(encVendor, nil)
		parsed, err := ParseConfig(blob)
		if err != nil {
			t.Fatalf("ch=%d: ParseConfig of our own headers: %v", ch, err)
		}
		if parsed.Channels != ch || parsed.Rate != 44100 {
			t.Fatalf("ch=%d: parsed %dch %dHz", ch, parsed.Channels, parsed.Rate)
		}
		if parsed.blockSizes != cfg.blockSizes {
			t.Fatalf("block sizes %v, want %v", parsed.blockSizes, cfg.blockSizes)
		}
		if len(parsed.codebooks) != len(cfg.specs) {
			t.Fatalf("parsed %d codebooks, want %d", len(parsed.codebooks), len(cfg.specs))
		}
		if len(parsed.floors) != 2 || len(parsed.residues) != 2 || len(parsed.modes) != 2 {
			t.Fatalf("floors=%d residues=%d modes=%d", len(parsed.floors), len(parsed.residues), len(parsed.modes))
		}
	}
}

// encodeSignal runs planar input through the encoder and returns the packets,
// each packet's cumulative decoded-sample granule (PTS+Dur, carrying variable
// block sizes), and the gapless trailer.
func encodeSignal(t *testing.T, e *Encoder, src [][]float32) ([][]byte, []int64, codec.Trailer) {
	t.Helper()
	f := e.InputFormat()
	var packets [][]byte
	var granules []int64
	emit := func(p codec.Packet) error {
		packets = append(packets, append([]byte(nil), p.Data...))
		granules = append(granules, p.PTS+p.Dur)
		return nil
	}
	const chunk = 1024
	n := len(src[0])
	for off := 0; off < n; off += chunk {
		end := min(off+chunk, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off // ChanF's length is N, so set it before copying in
		for c := range src {
			copy(buf.ChanF(c), src[c][off:end])
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatalf("encode: %v", err)
		}
		audio.Put(buf)
	}
	tr, err := e.Finish(emit)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return packets, granules, tr
}

// decodePackets decodes audio packets through our own decoder into interleaved
// output.
func decodePackets(t *testing.T, cfg Config, packets [][]byte) []float32 {
	t.Helper()
	d, err := NewDecoder(cfg, cfg.Format())
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
		if err := d.Decode(pkt, emit); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out
}

// TestEncodeDecodeRoundTrip is the 4a correctness gate: encode a signal, decode
// it through our own decoder, confirm the gapless sample-count invariant holds
// exactly and the reconstruction tracks the source under a lossy bound.
// TestNewEncoderRejectsBitrate confirms a nonzero ABR Bitrate target is rejected
// rather than silently ignored: ABR rate control is not implemented, the stream
// is pure VBR, and Bitrate() must not report a rate the stream never holds.
func TestNewEncoderRejectsBitrate(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	if _, err := NewEncoder(f, &EncoderOptions{Bitrate: 128000}); err == nil {
		t.Fatal("NewEncoder accepted a nonzero Bitrate; expected rejection (ABR unimplemented)")
	}
	e, err := NewEncoder(f, &EncoderOptions{Quality: 4})
	if err != nil {
		t.Fatalf("NewEncoder(VBR): %v", err)
	}
	if got := e.Bitrate(); got != 0 {
		t.Errorf("Bitrate() = %d, want 0 for VBR", got)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	const rate = 44100
	for _, tc := range []struct {
		name string
		ch   int
		n    int
	}{
		{"mono_1s", 1, rate},
		{"mono_short", 1, 5000},
		{"mono_tiny", 1, 300},
		{"stereo_1s", 2, rate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: rate, Channels: tc.ch, Layout: audio.DefaultLayout(tc.ch), Type: audio.Float, BitDepth: 32}
			src := make([][]float32, tc.ch)
			rng := rand.New(rand.NewSource(int64(tc.n)))
			for c := range src {
				src[c] = make([]float32, tc.n)
				for i := range src[c] {
					// A couple of tones plus light noise: a decodable, non-trivial signal.
					src[c][i] = float32(0.4*math.Sin(2*math.Pi*float64(i)*(440+float64(c)*110)/rate) +
						0.2*math.Sin(2*math.Pi*float64(i)*1500/rate) +
						0.05*(rng.Float64()*2-1))
				}
			}

			e, err := NewEncoder(f, nil)
			if err != nil {
				t.Fatal(err)
			}
			packets, _, tr := encodeSignal(t, e, src)
			if len(packets) < 2 {
				t.Fatalf("only %d packets", len(packets))
			}
			if tr.Samples != int64(tc.n) {
				t.Fatalf("trailer Samples %d, want %d", tr.Samples, tc.n)
			}

			cfg, err := ParseConfig(e.CodecConfig())
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			out := decodePackets(t, cfg, packets)

			// Apply the gapless trim: drop Delay frames from the front and
			// Padding from the back; what remains must be exactly Samples frames.
			framesOut := len(out) / tc.ch
			trimmed := out[tr.Delay*int64(tc.ch) : (int64(framesOut)-tr.Padding)*int64(tc.ch)]
			gotFrames := len(trimmed) / tc.ch
			if int64(gotFrames) != tr.Samples {
				t.Fatalf("gapless: decoded %d frames after trim, want %d (delay=%d padding=%d rawframes=%d)",
					gotFrames, tr.Samples, tr.Delay, tr.Padding, framesOut)
			}

			// Reconstruction error against the source, per channel.
			var sqErr, sqSig float64
			for i := 0; i < gotFrames; i++ {
				for c := 0; c < tc.ch; c++ {
					d := float64(trimmed[i*tc.ch+c]) - float64(src[c][i])
					sqErr += d * d
					sqSig += float64(src[c][i]) * float64(src[c][i])
				}
			}
			nrmse := math.Sqrt(sqErr / sqSig)
			t.Logf("%s: %d packets, %d frames, NRMSE=%.4f", tc.name, len(packets), gotFrames, nrmse)
			if nrmse > 0.25 {
				t.Errorf("normalized RMS error %.4f exceeds the 4a lossy bound 0.25", nrmse)
			}
		})
	}
}
