package musepack_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/musepack"
)

// TestScannersLeaveTheDecoderState pins the seek path's foundation: after N
// frames (SV7) or N blocks (SV8), the scanner's state equals the state a full
// decode leaves, on streams with and without noise substitution, across key
// frames. A seek landing is only as exact as this.
func TestScannersLeaveTheDecoderState(t *testing.T) {
	for _, f := range mpcFixtures {
		t.Run(f.name(), func(t *testing.T) {
			cfg, _, pkts := demuxAll(t, readFile(t, f.path))
			dec, err := musepack.NewDecoder(cfg, cfg.Format())
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Release()
			st := musepack.InitialState()
			drop := func(*audio.Buffer) error { return nil }
			for i, p := range pkts {
				h, _, payload, err := musepack.ParsePacketHeader(p, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.StreamVersion == 7 {
					err = musepack.ScanSV7Frame(payload, h.BitLen, cfg, &st)
				} else {
					err = musepack.ScanSV8Block(payload, h.Frames, cfg, &st)
				}
				if err != nil {
					t.Fatalf("scanning packet %d: %v", i, err)
				}
				if err := dec.Decode(p, drop); err != nil {
					t.Fatalf("decoding packet %d: %v", i, err)
				}
				if got := dec.DecoderState(); got != st {
					t.Fatalf("after packet %d the scanner's state differs from the decoder's:\nscan   RNG %v SCF %v\ndecode RNG %v SCF %v",
						i, st.RNG, st.SCF[0][:8], got.RNG, got.SCF[0][:8])
				}
			}
			if st.RNG == musepack.InitialState().RNG && cfg.PNS == 1 {
				t.Error("a stream declaring noise substitution never drew from the generator")
			}
		})
	}
}

// TestStateBlockRestoresADecode pins the packet state block: a fresh decoder
// handed the state before packet p, in front of packet p, decodes packets p
// onward exactly as the linear decode did from 512 samples in, where the
// synthesis history has refilled.
func TestStateBlockRestoresADecode(t *testing.T) {
	for _, name := range []string{"sv7-pns.mpc", "sv7-standard.mpc", "sv8-q0-pns.mpc", "sv8-frames0.mpc"} {
		t.Run(name, func(t *testing.T) {
			cfg, _, pkts := demuxAll(t, readFile(t, codecFixture(name)))
			linear := decodePackets(t, cfg, pkts)
			p := len(pkts) / 2
			st := musepack.InitialState()
			var at int
			for i := 0; i < p; i++ {
				h, _, payload, err := musepack.ParsePacketHeader(pkts[i], cfg)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.StreamVersion == 7 {
					err = musepack.ScanSV7Frame(payload, h.BitLen, cfg, &st)
				} else {
					err = musepack.ScanSV8Block(payload, h.Frames, cfg, &st)
				}
				if err != nil {
					t.Fatal(err)
				}
				at += h.Frames * musepack.FrameLength * cfg.Channels
			}
			h, _, payload, err := musepack.ParsePacketHeader(pkts[p], cfg)
			if err != nil {
				t.Fatal(err)
			}
			withState := make([]byte, musepack.PacketHeaderLen)
			musepack.PutPacketHeader(withState, h.Frames, h.BitLen, true)
			withState = st.AppendBinary(withState, cfg.StreamVersion)
			withState = append(withState, payload...)
			resumed := decodePackets(t, cfg, append([][]byte{withState}, pkts[p+1:]...))
			ref := linear[at:]
			if len(resumed) != len(ref) {
				t.Fatalf("resumed decode is %d samples, the linear tail %d", len(resumed), len(ref))
			}
			warm := 512 * cfg.Channels
			if d := compare(resumed[warm:], ref[warm:]); d.max != 0 {
				t.Errorf("resumed decode differs from the linear one past the warm-up: %+v", d)
			}
			// And without the state the same landing is wrong on a stream
			// whose state moved, which is what makes the block load-bearing.
			bare := decodePackets(t, cfg, pkts[p:])
			if d := compare(bare[warm:], ref[warm:]); d.max == 0 && st != musepack.InitialState() {
				// For SV8 only the generator is state; an unmoved generator
				// means a bare resume is legitimately identical.
				t.Error("a resume without the state block matched the linear decode; the test proves nothing here")
			}
		})
	}
}
