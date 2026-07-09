package opus

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/ogg"
)

// TestOpusGaplessInvariant checks the gapless sample-count contract end to end:
// encode N input samples to Ogg-Opus, then demux and decode, and require the
// trimmed output length to equal N exactly for a range of N (including
// non-frame-multiples). This exercises the OpusHead pre-skip (front trim) and
// the final-page granule (end trim) the muxer writes.
func TestOpusGaplessInvariant(t *testing.T) {
	for _, C := range []int{1, 2} {
		for _, n := range []int{1, 480, 959, 960, 961, 2000, 48000, 57601} {
			og := encodeN(t, C, n, 96000)

			d, err := ogg.NewDemuxer(container.BytesSource(og), nil)
			if err != nil {
				t.Fatalf("C=%d n=%d: demux: %v", C, n, err)
			}
			tr := d.Tracks()[0]
			if tr.Samples != int64(n) {
				t.Errorf("C=%d n=%d: Track.Samples = %d, want %d", C, n, tr.Samples, n)
			}
			if tr.Delay != EncoderDelay {
				t.Errorf("C=%d n=%d: Track.Delay = %d, want %d", C, n, tr.Delay, EncoderDelay)
			}

			// Decode every packet and apply the front trim + end cap; the kept
			// length must equal n.
			dec := newCELTDecoder(C)
			var decoded int64
			out := make([][]float32, C)
			for c := range out {
				out[c] = make([]float32, opusFrameSize)
			}
			var pkt container.Packet
			for {
				if err := d.ReadPacket(&pkt); err != nil {
					break
				}
				fr, ferr := splitPacket(pkt.Data)
				if ferr != nil {
					t.Fatalf("C=%d n=%d: split: %v", C, n, ferr)
				}
				for range fr {
					if err := dec.celtDecode(pkt.Data, opusFrameLM, C, 0, opusCELTBands, out); err != nil {
						t.Fatalf("C=%d n=%d: decode: %v", C, n, err)
					}
					decoded += opusFrameSize
				}
			}
			// The read side trims Delay from the front and caps the tail at
			// Track.Samples (the Ogg-Opus granule signals end-trim via Samples,
			// not a separate padding count).
			kept := min(decoded-tr.Delay, tr.Samples)
			if kept != int64(n) {
				t.Errorf("C=%d n=%d: kept %d samples after trims (decoded %d, delay %d, samples %d), want %d",
					C, n, kept, decoded, tr.Delay, tr.Samples, n)
			}
		}
	}
}

// encodeN encodes n samples of a simple signal to Ogg-Opus bytes.
func encodeN(t *testing.T, C, n, bitrate int) []byte {
	t.Helper()
	f := audio.Format{Rate: SampleRate, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: bitrate})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := ogg.NewMuxer(&out, nil)
	if err := mux.Begin([]container.Track{{Codec: codec.Opus, CodecConfig: enc.CodecConfig(), Fmt: f}}); err != nil {
		t.Fatal(err)
	}
	flat := make([]float32, C*n)
	for c := 0; c < C; c++ {
		for i := 0; i < n; i++ {
			flat[c*n+i] = float32(0.2 * float64((i%400)-200) / 200)
		}
	}
	buf := &audio.Buffer{Fmt: f, F: flat, Stride: n, N: n}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(tr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
