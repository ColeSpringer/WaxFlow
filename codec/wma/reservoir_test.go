//go:build !wmatablesgen

package wma_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/internal/testutil"
)

// bitbuf writes a most-significant-bit-first stream, which is what a
// superframe is.
type bitbuf struct {
	b []byte
	n int
}

func (w *bitbuf) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n&7 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.b[w.n>>3] |= 1 << (7 - w.n&7)
		}
		w.n++
	}
}

func (w *bitbuf) copyBits(src []byte, at, n int) {
	for ; n > 0; n-- {
		var bit uint32
		if at>>3 < len(src) && src[at>>3]>>(7-at&7)&1 != 0 {
			bit = 1
		}
		w.put(bit, 1)
		at++
	}
}

// reframe repacks a run of frames into bit-reservoir superframes of blockAlign
// bytes each. all/allBits is every frame's bits back to back and ends holds
// each frame's end position in that stream.
func reframe(t *testing.T, cfg wma.Config, all []byte, allBits int, ends []int, blockAlign int) (pkts [][]byte, spanning int) {
	t.Helper()
	offBits, err := wma.OffsetBitsForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	headerBits := 8 + offBits + 3
	payloadCap := blockAlign*8 - headerBits
	if payloadCap <= 8 {
		t.Fatalf("a %d-byte superframe leaves %d payload bits", blockAlign, payloadCap)
	}
	cursor, fi, lastEnd := 0, 0, 0
	for cursor < allBits {
		avail := min(payloadCap, allBits-cursor)
		end := cursor + avail
		// A frame is in progress when bits past the last completed frame have
		// already been placed.
		carry := cursor > lastEnd
		n, bitOff := 0, 0
		if carry && fi < len(ends) && ends[fi] <= end {
			// The carried frame finishes in the first bitOff bits.
			bitOff = ends[fi] - cursor
			lastEnd, fi, n = ends[fi], fi+1, n+1
			spanning++
		}
		for fi < len(ends) && ends[fi] <= end {
			lastEnd, fi, n = ends[fi], fi+1, n+1
		}
		// The count field names frames OUTPUT: with a carry pending one of
		// them is the carried frame, so the field is n; with none, the reader
		// subtracts one, so the field is n+1.
		f := n
		if !carry {
			f = n + 1
		}
		if f > 15 {
			t.Fatalf("frame count %d does not fit the 4-bit field", f)
		}
		if bitOff >= 1<<(offBits+3) {
			t.Fatalf("bit offset %d does not fit %d bits", bitOff, offBits+3)
		}
		var w bitbuf
		w.put(0, 4) // superframe index, which readers ignore
		w.put(uint32(f), 4)
		w.put(uint32(bitOff), offBits+3)
		w.copyBits(all, cursor, avail)
		pkt := make([]byte, blockAlign)
		copy(pkt, w.b)
		pkts = append(pkts, pkt)
		cursor = end
	}
	return pkts, spanning
}

// TestBitReservoirReframesToTheSameAudio exercises the superframe walk, which
// no differential can: `flags2` is 1 in every file either ffmpeg encoder
// writes, so the reservoir bit is never set and the header, the carry buffer
// and the cross-packet frame are unreachable from any generated corpus.
//
// So the frames are taken from a real stream and re-laid across superframes
// that deliberately do not align with them. The same frames in the same order
// must decode to the same samples, bit for bit: the reservoir changes where
// frames sit, not what they say. That catches the header layout, the frame
// count's carry-dependent subtraction, the bit offset, and the carry's
// sub-byte alignment -- get the last one wrong and the next frame resumes up
// to seven bits off, which desynchronises everything after it.
//
// The cells are every shape the decoder accepts a reservoir on. Stereo v1 is
// absent because it is refused: this test is what found that its per-channel
// byte alignment makes a frame mean different things at different offsets, so
// the premise above ("the reservoir changes where frames sit, not what they
// say") is false for exactly that combination. See Config.Validate.
func TestBitReservoirReframesToTheSameAudio(t *testing.T) {
	for _, c := range []cell{corpusCells[0], corpusCells[2], corpusCells[13], corpusCells[16]} {
		t.Run(c.name(), func(t *testing.T) {
			track, pkts := demux(t, corpusFile(t, c))
			cfg, err := wma.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Flags2&0x0002 != 0 {
				t.Fatal("the corpus cell already uses the reservoir; the re-frame proves nothing")
			}

			// Collect each frame's exact bit run and the linear decode.
			dec, err := wma.NewDecoder(cfg, track.Fmt)
			if err != nil {
				t.Fatal(err)
			}
			var linear []float32
			collect := func(b *audio.Buffer) error {
				linear = append(linear, testutil.InterleaveF(b)...)
				return nil
			}
			var all bitbuf
			var ends []int
			for i, p := range pkts {
				if err := dec.Decode(p, collect); err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
				all.copyBits(p, 0, wma.FrameBitsForTest(dec))
				ends = append(ends, all.n)
			}
			if err := dec.Drain(collect); err != nil {
				t.Fatal(err)
			}
			dec.Release()

			rpkts, spanning := reframe(t, cfg, all.b, all.n, ends, cfg.BlockAlign)
			if spanning == 0 {
				t.Fatal("no frame spans a superframe boundary; the re-frame is vacuous")
			}
			if len(rpkts) == len(pkts) && spanning < len(pkts)/4 {
				t.Fatalf("only %d of %d frames span a boundary", spanning, len(pkts))
			}

			rcfg := cfg
			rcfg.Flags2 |= 0x0002 // use_bit_reservoir
			rdec, err := wma.NewDecoder(rcfg, track.Fmt)
			if err != nil {
				t.Fatal(err)
			}
			defer rdec.Release()
			var got []float32
			emit := func(b *audio.Buffer) error {
				got = append(got, testutil.InterleaveF(b)...)
				return nil
			}
			for i, p := range rpkts {
				if err := rdec.Decode(p, emit); err != nil {
					t.Fatalf("reservoir packet %d of %d: %v", i, len(rpkts), err)
				}
			}
			if err := rdec.Drain(emit); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(linear) {
				t.Fatalf("the reservoir decode is %d samples, the linear one %d", len(got), len(linear))
			}
			if d := testutil.CompareF32(got, linear); d.MaxAbs != 0 {
				t.Errorf("re-framing the same frames changed the audio: %v", d)
			}
		})
	}
}
