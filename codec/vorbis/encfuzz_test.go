package vorbis

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// FuzzEncode drives arbitrary PCM through the encoder and then its own decoder.
// The encoder must never panic on any input, every packet it emits must decode,
// and the gapless sample-count invariant must hold: the round-trip after
// trimming Delay and Padding recovers exactly the input length. This guards the
// bitwriter, floor fit, and residue classification against hostile magnitudes
// (NaN/Inf/huge) and odd lengths.
func FuzzEncode(f *testing.F) {
	f.Add(uint32(1), 1, 5000, float32(0.5))
	f.Add(uint32(7), 2, 300, float32(2.0))
	f.Add(uint32(99), 2, 44100, float32(0.01))
	f.Fuzz(func(t *testing.T, seed uint32, ch, n int, amp float32) {
		if ch < 1 || ch > 2 {
			ch = 1 + int(seed%2)
		}
		if n < 0 {
			n = -n
		}
		n %= 60000 // bound the work
		if n == 0 {
			n = 1
		}
		if math.IsNaN(float64(amp)) || math.IsInf(float64(amp), 0) {
			amp = 1
		}

		const rate = 44100
		fm := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		src := make([][]float32, ch)
		rng := seed
		next := func() float32 {
			rng = rng*1664525 + 1013904223
			// A spread of values including the occasional non-finite/huge one.
			switch rng % 32 {
			case 0:
				return float32(math.NaN())
			case 1:
				return float32(math.Inf(1))
			case 2:
				return 1e30
			default:
				return (float32(rng>>8)/float32(1<<24)*2 - 1) * amp
			}
		}
		for c := range src {
			src[c] = make([]float32, n)
			for i := range src[c] {
				src[c][i] = next()
			}
		}

		e, err := NewEncoder(fm, nil)
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		var packets [][]byte
		emit := func(p codec.Packet) error {
			packets = append(packets, append([]byte(nil), p.Data...))
			return nil
		}
		off := 0
		for off < n {
			end := min(off+1024, n)
			buf := audio.Get(fm, end-off)
			buf.N = end - off
			for c := 0; c < ch; c++ {
				copy(buf.ChanF(c), src[c][off:end])
			}
			if err := e.Encode(buf, emit); err != nil {
				t.Fatalf("Encode: %v", err)
			}
			audio.Put(buf)
			off = end
		}
		tr, err := e.Finish(emit)
		if err != nil {
			t.Fatalf("Finish: %v", err)
		}
		if tr.Samples != int64(n) {
			t.Fatalf("trailer Samples %d, want %d", tr.Samples, n)
		}

		cfg, err := ParseConfig(e.CodecConfig())
		if err != nil {
			t.Fatalf("our own CodecConfig does not parse: %v", err)
		}
		out := decodePackets(t, cfg, packets)
		framesOut := int64(len(out) / ch)
		got := framesOut - tr.Delay - tr.Padding
		if got != tr.Samples {
			t.Fatalf("gapless: recovered %d frames, want %d (delay=%d padding=%d raw=%d)",
				got, tr.Samples, tr.Delay, tr.Padding, framesOut)
		}
	})
}

// TestEncodeDeterministic confirms deterministic-mode byte-exactness: two
// independent encodes of the same input produce identical headers and packets,
// the property golden streams and the ADR-0004 cache key depend on. dsp/fft's
// fixed float32 op order is what makes this hold.
func TestEncodeDeterministic(t *testing.T) {
	const rate = 44100
	for _, ch := range []int{1, 2} {
		fm := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		src := musicish(rate, rate, ch, 3)
		a := encodeAll(t, fm, src)
		b := encodeAll(t, fm, src)
		if len(a) != len(b) {
			t.Fatalf("ch=%d: packet counts differ: %d vs %d", ch, len(a), len(b))
		}
		for i := range a {
			if !bytesEqual(a[i], b[i]) {
				t.Fatalf("ch=%d: packet %d differs between runs (nondeterministic)", ch, i)
			}
		}
	}
}

// TestEncodeGolden pins a hash of a fixed encode so a bitstream change is a
// deliberate, reviewed event (EncoderVersion must bump with it). The hash is a
// cheap stand-in for a committed golden file; it fails loud on any drift.
func TestEncodeGolden(t *testing.T) {
	const rate = 44100
	fm := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	src := musicish(rate, rate, 2, 11)
	packets := encodeAll(t, fm, src)
	var h uint64 = 1469598103934665603 // FNV-1a
	mix := func(b []byte) {
		for _, x := range b {
			h ^= uint64(x)
			h *= 1099511628211
		}
	}
	e, _ := NewEncoder(fm, nil)
	mix(e.CodecConfig())
	for _, p := range packets {
		var lb [4]byte
		binary.LittleEndian.PutUint32(lb[:], uint32(len(p)))
		mix(lb[:])
		mix(p)
	}
	const golden = goldenEncodeHash
	if h != golden {
		t.Errorf("encode hash %#016x != golden %#016x: the bitstream changed. If intended, bump EncoderVersion and update goldenEncodeHash.", h, golden)
	}
}

func encodeAll(t *testing.T, fm audio.Format, src [][]float32) [][]byte {
	t.Helper()
	e, err := NewEncoder(fm, nil)
	if err != nil {
		t.Fatal(err)
	}
	packets, _, _ := encodeSignal(t, e, src)
	return packets
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// goldenEncodeHash pins the FNV-1a hash of the reference encode (headers plus
// length-prefixed packets). Regenerate it and bump EncoderVersion whenever a
// deliberate bitstream change lands.
const goldenEncodeHash uint64 = 0x3b0da36a84fe62f5
