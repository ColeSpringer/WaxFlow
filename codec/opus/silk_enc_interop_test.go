package opus

import (
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestSILKEncoderReferenceDecode decodes our SILK-only streams through the
// reference libopus decoder (opus_demo). The bitstream file carries each
// packet's range-coder final state, which opus_demo verifies per packet and
// hard-fails on mismatch, so a pass certifies our encoder drives the
// reference decoder to the exact same entropy-coder state on every packet.
// The reference decode must then match our own decoder's output bit-exactly:
// both run the same bit-exact fixed-point SILK decode path.
func TestSILKEncoderReferenceDecode(t *testing.T) {
	opusDemo, _ := testutil.OpusTools(t)
	pcm := synthSpeech(3 * 48000)

	for _, fsKHz := range []int{8, 12, 16} {
		e := newSILKEncoder(1)
		ctrl := &silkEncControl{
			nChannelsAPI:              1,
			nChannelsInternal:         1,
			apiSampleRate:             48000,
			maxInternalSampleRate:     fsKHz * 1000,
			minInternalSampleRate:     fsKHz * 1000,
			desiredInternalSampleRate: fsKHz * 1000,
			payloadSizeMS:             20,
			bitRate:                   int32(fsKHz) * 2000,
			complexity:                10,
			maxBits:                   (maxFrameBytes - 1) * 8,
		}
		const frame = 960
		var pkts [][]byte
		var ranges []uint32
		for off := 0; off+frame <= len(pcm); off += frame {
			enc := newRangeEncoder(make([]byte, maxFrameBytes))
			nBytesOut := 0
			e.encode(ctrl, pcm[off:off+frame], frame, enc, &nBytesOut, 0, -1)
			if nBytesOut <= 0 {
				t.Fatalf("fs %d: no payload at %d", fsKHz, off)
			}
			enc.shrink(nBytesOut)
			enc.done()
			payload := enc.payload()
			pkt := make([]byte, 1+len(payload))
			pkt[0] = silkTOC(fsKHz, false)
			copy(pkt[1:], payload)
			pkts = append(pkts, pkt)
			ranges = append(ranges, enc.rng)
		}

		bitPath := filepath.Join(t.TempDir(), "silk.bit")
		if err := testutil.WriteOpusBitstream(bitPath, pkts, ranges); err != nil {
			t.Fatal(err)
		}
		refOut := testutil.OpusDemoDecode(t, opusDemo, bitPath, 48000, 1)

		// Decode with our own decoder and compare bit-exactly.
		cfg := Config{Channels: 1}
		dec, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		var ours []float32
		for i, pkt := range pkts {
			err := dec.Decode(pkt, func(b *audio.Buffer) error {
				ours = append(ours, b.ChanF(0)[:b.N]...)
				return nil
			})
			if err != nil {
				t.Fatalf("fs %d packet %d: %v", fsKHz, i, err)
			}
		}
		if len(ours) != len(refOut) {
			t.Fatalf("fs %d: our decode %d samples, reference %d", fsKHz, len(ours), len(refOut))
		}
		for i := range ours {
			got := int32(ours[i] * 32768)
			if got != int32(refOut[i]) {
				t.Fatalf("fs %d: sample %d differs: ours %d, reference %d", fsKHz, i, got, refOut[i])
			}
		}
		t.Logf("fs %d kHz: %d packets, final-range verified, decode bit-exact vs libopus", fsKHz, len(pkts))
	}
}
