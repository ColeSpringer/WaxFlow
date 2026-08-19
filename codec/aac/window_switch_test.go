package aac

import (
	"fmt"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// clickAt adds one 2 kHz decaying burst at start; overlapping bursts
// superpose instead of truncating each other.
func clickAt(samples []float32, rate, start int) {
	for i := 0; i < 600 && start+i < len(samples); i++ {
		env := math.Exp(-float64(i) / float64(rate) * 60)
		samples[start+i] += float32(env * math.Sin(2*math.Pi*2000*float64(i)/float64(rate)) * 0.6)
	}
}

// encodeSeqsPlanar feeds planar samples through an encoder and returns
// the window sequence and group count of every emitted AU plus the
// packets and ASC.
func encodeSeqsPlanar(t *testing.T, rate, bps int, chans [][]float32) (seqs, groups []int, pkts [][]byte, asc []byte) {
	t.Helper()
	ch := len(chans)
	f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
	e, err := NewEncoder(f, &EncoderOptions{Bitrate: bps})
	if err != nil {
		t.Fatal(err)
	}
	buf := audio.Get(f, len(chans[0]))
	defer audio.Put(buf)
	buf.N = len(chans[0])
	for c := range chans {
		copy(buf.ChanF(c), chans[c])
	}
	emit := func(p codec.Packet) error {
		seqs = append(seqs, e.prevSeq)
		groups = append(groups, e.cq[0].nGroups)
		pkts = append(pkts, append([]byte(nil), p.Data...))
		return nil
	}
	if err := e.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Finish(emit); err != nil {
		t.Fatal(err)
	}
	return seqs, groups, pkts, e.CodecConfig()
}

// encodeSeqs is encodeSeqsPlanar's mono shorthand.
func encodeSeqs(t *testing.T, rate, bps int, samples []float32) ([]int, [][]byte, []byte) {
	t.Helper()
	seqs, _, pkts, asc := encodeSeqsPlanar(t, rate, bps, [][]float32{samples})
	return seqs, pkts, asc
}

// checkSeqLegal asserts the window-sequence chain reconstructs: an
// EIGHT_SHORT window only follows LONG_START or EIGHT_SHORT, LONG_START
// is always followed by EIGHT_SHORT, and LONG_STOP only follows
// EIGHT_SHORT. The first AU is unconstrained: it has no overlap partner
// and its left flank lands entirely in the trimmed priming block, so an
// attack in the stream's first samples may open on EIGHT_SHORT.
func checkSeqLegal(t *testing.T, seqs []int) {
	t.Helper()
	prev := -1
	for i, s := range seqs {
		if prev < 0 {
			prev = s
			continue
		}
		switch s {
		case eightShort:
			if prev != longStart && prev != eightShort {
				t.Errorf("AU %d: EIGHT_SHORT after sequence %d breaks overlap-add", i, prev)
			}
		case longStop:
			if prev != eightShort {
				t.Errorf("AU %d: LONG_STOP after sequence %d breaks overlap-add", i, prev)
			}
		case onlyLong, longStart:
			if prev == longStart {
				t.Errorf("AU %d: sequence %d after LONG_START breaks overlap-add", i, s)
			}
			if prev == eightShort {
				t.Errorf("AU %d: sequence %d right after EIGHT_SHORT breaks overlap-add", i, s)
			}
		}
		prev = s
	}
	if prev == longStart {
		t.Error("stream ends on LONG_START; the promised EIGHT_SHORT never followed")
	}
}

// requireCovered asserts the AU whose short windows reach a click is
// EIGHT_SHORT. AU j windows blocks (j-1, j); its eight shorts cover
// block j-1 offsets [448, 1024) and block j offsets [0, 576), so a
// click in block k at offset < 512 needs AU k short (the lookahead
// side) and one at offset >= 512 needs AU k+1 short.
func requireCovered(t *testing.T, seqs []int, block, off int) {
	t.Helper()
	need := block
	if off >= 512 {
		need = block + 1
	}
	if need >= len(seqs) {
		t.Fatalf("AU %d not emitted (%d AUs)", need, len(seqs))
	}
	if seqs[need] != eightShort {
		t.Errorf("click in block %d offset %d: AU %d is sequence %d, want EIGHT_SHORT (shorts cover it)",
			block, off, need, seqs[need])
	}
}

// TestWindowSwitchCoversAttacks pins the coverage property behind the
// closed HE ledger's core-bound transient cells: every attack lands
// inside some EIGHT_SHORT frame's short windows. Before the deferred
// window decision, an early-offset click after quiet was coded at full
// weight by the preceding LONG_START window's flat region instead
// (noise smeared over ~70 ms, the measured -0.9..-1.9 ODG loss against
// fdk on click material).
func TestWindowSwitchCoversAttacks(t *testing.T) {
	const rate = 22050
	const blocks = 24
	for _, off := range []int{0, 100, 300, 440, 512, 600, 900} {
		t.Run(fmt.Sprintf("offset%d", off), func(t *testing.T) {
			samples := make([]float32, blocks*frameLen)
			var clickBlocks []int
			for k := 3; k+1 < blocks; k += 3 {
				clickAt(samples, rate, k*frameLen+off)
				clickBlocks = append(clickBlocks, k)
			}
			seqs, _, _ := encodeSeqs(t, rate, 32000, samples)
			checkSeqLegal(t, seqs)
			for _, k := range clickBlocks {
				requireCovered(t, seqs, k, off)
			}
		})
	}

	// Two attacks in ONE source block (offsets 100 and 800: hi-hat
	// 16ths, drum rolls). The early one needs AU k, the late one AU
	// k+1; a detector that reports only the first attack per block
	// leaves the late one to a long window.
	t.Run("paired-in-block", func(t *testing.T) {
		samples := make([]float32, blocks*frameLen)
		var clickBlocks []int
		for k := 3; k+1 < blocks; k += 3 {
			clickAt(samples, rate, k*frameLen+100)
			clickAt(samples, rate, k*frameLen+800)
			clickBlocks = append(clickBlocks, k)
		}
		seqs, _, _ := encodeSeqs(t, rate, 32000, samples)
		checkSeqLegal(t, seqs)
		for _, k := range clickBlocks {
			requireCovered(t, seqs, k, 100)
			requireCovered(t, seqs, k, 800)
		}
	})

	// A click in the stream's first samples: the first AU opens on
	// EIGHT_SHORT (legal: its left flank is the trimmed priming block).
	t.Run("head", func(t *testing.T) {
		samples := make([]float32, 12*frameLen)
		clickAt(samples, rate, 100)
		seqs, pkts, asc := encodeSeqs(t, rate, 32000, samples)
		checkSeqLegal(t, seqs)
		requireCovered(t, seqs, 0, 100)
		out := decodeAll(t, asc, pkts)
		if got := snrDB(samples[100:700], out[0][EncoderDelay+100:EncoderDelay+700]); got < 5 {
			t.Errorf("head click reconstructs at %.1f dB SNR, want at least 5", got)
		}
	})

	// Irregularly spaced clicks: bridging shorts, back-to-back shorts,
	// and one AU holding two attack windows (block 5's late click plus
	// block 6's early click both belong to AU 6, which must isolate two
	// windows) all in one stream, with a decode sanity leg. No pair of
	// clicks shares a required AU except the block 5/6 pair, which is
	// the point of it.
	t.Run("irregular", func(t *testing.T) {
		samples := make([]float32, 20*frameLen)
		clicks := [][2]int{{3, 100}, {5, 600}, {6, 300}, {8, 900}, {9, 700}, {12, 512}, {13, 700}, {15, 0}}
		for _, c := range clicks {
			clickAt(samples, rate, c[0]*frameLen+c[1])
		}
		seqs, groups, pkts, asc := encodeSeqsPlanar(t, rate, 32000, [][]float32{samples})
		checkSeqLegal(t, seqs)
		for _, c := range clicks {
			requireCovered(t, seqs, c[0], c[1])
		}
		// AU 6 isolates both attack windows: {pre, atk, mid, atk} or
		// more, never a single-attack 3-group shape.
		if groups[6] < 4 {
			t.Errorf("AU 6 holds two attacks but groups into %d (want >= 4: both isolated)", groups[6])
		}
		out := decodeAll(t, asc, pkts)
		if len(out[0]) < len(samples)+EncoderDelay {
			t.Fatalf("decode returned %d samples, want at least %d", len(out[0]), len(samples)+EncoderDelay)
		}
	})

	// Stereo channels attacking in different windows of the same block:
	// the shared grouping isolates both channels' attack windows rather
	// than pooling the later one into a tail group.
	t.Run("stereo-split", func(t *testing.T) {
		l := make([]float32, 16*frameLen)
		r := make([]float32, 16*frameLen)
		for k := 3; k+1 < 16; k += 4 {
			clickAt(l, rate, k*frameLen+900)
			clickAt(r, rate, k*frameLen+550)
		}
		seqs, groups, _, _ := encodeSeqsPlanar(t, rate, 64000, [][]float32{l, r})
		checkSeqLegal(t, seqs)
		for k := 3; k+1 < 16; k += 4 {
			requireCovered(t, seqs, k, 900)
			requireCovered(t, seqs, k, 550)
			// Offsets 900 and 550 land in windows 4 and 1: two isolated
			// windows plus pre/mid/tail is at least 5 groups.
			if g := groups[k+1]; g < 5 {
				t.Errorf("AU %d holds split-channel attacks (windows 1 and 4) but groups into %d (want >= 5)", k+1, g)
			}
		}
	})
}
