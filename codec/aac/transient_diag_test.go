package aac

// White-box allocation diagnostic: dumps per-band threshold, energy,
// and achieved-noise state for frames around a click at the half-rate
// core's operating point. This dump is what isolated the closed HE
// ledger's transient deficit (attack energy in a LONG_START frame's
// spectrum instead of the shorts). Not a gate; runs only under
// WAXFLOW_HE_DIAG=1.

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

func TestCoreTransientAllocationDiag(t *testing.T) {
	if os.Getenv("WAXFLOW_HE_DIAG") != "1" {
		t.Skip("diagnostic; set WAXFLOW_HE_DIAG=1")
	}
	const rate = 22050
	const bps = 22000
	frames := 3 * rate
	period := rate / 8
	ref := make([]float32, frames)
	for i := 0; i < frames; i++ {
		phase := i % period
		env := math.Exp(-float64(phase) / float64(rate) * 60)
		ref[i] = float32(env * math.Sin(2*math.Pi*2000*float64(phase)/float64(rate)) * 0.6)
	}

	f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	e, err := NewEncoder(f, &EncoderOptions{Bitrate: bps})
	if err != nil {
		t.Fatal(err)
	}

	buf := audio.Get(f, frames)
	buf.N = frames
	copy(buf.ChanF(0), ref)

	seqName := map[int]string{onlyLong: "LONG ", longStart: "START", eightShort: "SHORT", longStop: "STOP "}

	frameIdx := 0
	emit := func(p codec.Packet) error {
		seq := e.prevSeq
		bits := len(p.Data) * 8
		// AU m carries source blocks m-2..m-1 as its output; a click at
		// source sample s lands in AU floor(s/1024)+1. Clicks hit every
		// 2756 samples; log AUs 6..12 (clicks at 5512=AU6 pos 1395... and
		// 8268=AU9 pos 84).
		if frameIdx >= 5 && frameIdx <= 12 {
			var sb strings.Builder
			fmt.Fprintf(&sb, "AU%2d %s %4db |", frameIdx, seqName[seq], bits)
			cq := &e.cq[0]
			// Per band: index, energy dB, thr dB, coded SNR dB (or Z for
			// zeroed), grouped over all groups.
			for bi := range cq.bands {
				b := &cq.bands[bi]
				if b.energy < 1e-3 && b.thr < 1e-3 {
					continue
				}
				eDB := 10 * math.Log10(b.energy+1e-10)
				tDB := 10 * math.Log10(b.thr+1e-10)
				// The dump runs from the emit hook after assemble; it
				// re-reads memo entries the frame already computed, and
				// only for coded bands, so it never adds encoder state.
				tag := ""
				noise := b.energy
				if b.cb == 0 {
					tag = "Z"
				} else {
					noise = cq.bandAt(bi, b.sf).noise
				}
				nDB := 10 * math.Log10(noise+1e-10)
				fmt.Fprintf(&sb, " %d:e%.0f/t%.0f/n%.0f%s", bi, eDB, tDB, nDB, tag)
			}
			t.Log(sb.String())
		}
		frameIdx++
		return nil
	}
	if err := e.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Finish(emit); err != nil {
		t.Fatal(err)
	}
}
