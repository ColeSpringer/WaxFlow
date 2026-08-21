//go:build !wmatablesgen

package wma_test

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// decodeFrom decodes from packet p onward with a decoder that has just been
// Reset, which is what a seek landing on that packet produces.
//
// The run-up matters and is not decoration. Resetting a FRESH decoder tests
// nothing: a new decoder already holds every value Reset assigns -- zeroed
// accumulator and exponents, empty carry, armed walk, noise index at zero, one
// frame of lead-in owed -- so an empty Reset body would leave every seek test
// in this file green, bit-exactness included. The only Reset that can fail is
// one on a decoder that has state to discard, which is also the only kind
// format.Media ever performs.
func decodeFrom(t *testing.T, track container.Track, pkts [][]byte, p int) []float32 {
	t.Helper()
	cfg, err := wma.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	dec, err := wma.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer dec.Release()
	drop := func(*audio.Buffer) error { return nil }
	for i := range p {
		if err := dec.Decode(pkts[i], drop); err != nil {
			t.Fatalf("run-up packet %d: %v", i, err)
		}
	}
	dec.Reset()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, testutil.InterleaveF(b)...)
		return nil
	}
	for i := p; i < len(pkts); i++ {
		if err := dec.Decode(pkts[i], emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

// TestSeekLandsWhereThePacketSays is the seek invariant, and it is two
// different gates because the format makes it two.
//
// Everything in this decoder resynchronises inside one packet, so a decode
// resumed at packet p is BIT-IDENTICAL to a linear one from p*frameLen -- one
// packet of run-up is enough, which is exactly what container/asf backs a
// landing up by. That holds only where noise coding is off. The noise index is
// never reset by the format, so it is a function of the entire decode history;
// a resumed decode draws different noise for the rest of the file and never
// converges. On those cells the gate is an energy bound and a correlation, not
// equality: a gate written as "converges after N samples" would pass by
// accident on the eight noise-off cells and be quietly disabled on the others.
func TestSeekLandsWhereThePacketSays(t *testing.T) {
	for _, c := range corpusCells {
		t.Run(c.name(), func(t *testing.T) {
			track, pkts := demux(t, corpusFile(t, c))
			linear := decodeAll(t, track, pkts)
			// Land well past the head, where a resumed decode has no chance of
			// inheriting the opening transient, and off the noise table's own
			// period (see TestNoiseIndexPeriodReproducesALinearDecode) so the
			// noise-on arm below is measuring a real disagreement.
			p := len(pkts) / 2
			for c.noise && p*c.frameLen*c.channels%noiseTableLen == 0 {
				p++
			}
			seeked := decodeFrom(t, track, pkts, p)
			// A landing at packet p emits from p*frameLen: the first frame it
			// decodes is the lead-in the head trim drops, and the one after it
			// is the first the packet timeline names.
			at := p * c.frameLen * c.channels
			if at >= len(linear) {
				t.Fatalf("landing offset %d past the %d-sample decode", at, len(linear))
			}
			ref := linear[at:]
			n := min(len(ref), len(seeked))
			if n < c.frameLen*c.channels {
				t.Fatalf("only %d samples to compare", n)
			}
			if len(seeked) != len(ref) {
				t.Errorf("seeked decode is %d samples, linear tail is %d", len(seeked), len(ref))
			}
			d := testutil.CompareF32(seeked[:n], ref[:n])
			if !c.noise {
				if d.MaxAbs != 0 {
					t.Errorf("noise coding is off, so a resumed decode must be bit-identical: %v", d)
				}
				return
			}
			// Noise-coded: the same audio with a different noise fill. Energy
			// and correlation are what survive that; sample equality does not.
			var ea, eb, dot float64
			for i := range n {
				a, b := float64(seeked[i]), float64(ref[i])
				ea, eb, dot = ea+a*a, eb+b*b, dot+a*b
			}
			if ea == 0 || eb == 0 {
				t.Fatal("a silent cell proves nothing")
			}
			ratio := math.Sqrt(ea / eb)
			corr := dot / math.Sqrt(ea*eb)
			if math.Abs(ratio-1) > 0.05 {
				t.Errorf("energy ratio %.4f against the linear decode", ratio)
			}
			if corr < 0.95 {
				t.Errorf("correlation %.4f against the linear decode", corr)
			}
			// And the difference must be steady state rather than a settling
			// transient, which is the shape the notes measured: the head and
			// the tail of the region read the same. A gate phrased as
			// "converges after N samples" would pass here by accident.
			third := n / 3
			head := testutil.CompareF32(seeked[:third], ref[:third])
			tail := testutil.CompareF32(seeked[n-third:n], ref[n-third:n])
			if head.RMS == 0 || tail.RMS == 0 {
				t.Fatalf("the noise fill matched exactly off the table period: head %v tail %v", head, tail)
			}
			if r := tail.RMS / head.RMS; r < 0.5 || r > 2 {
				t.Errorf("the difference is a transient, not steady state: head %v tail %v", head, tail)
			}
		})
	}
}

// noiseTableLen is the generated noise source's length. The running index wraps
// at it and is never reset by the format.
const noiseTableLen = 8192

// TestNoiseIndexPeriodReproducesALinearDecode pins the noise walk itself.
//
// Every coded channel draws exactly blockLen values per block: the three
// reconstruction regions -- below the first codeable coefficient, the coded
// span with its high bands, and the tail above the codeable span -- partition
// the block, and the noise is drawn across all of it, added even to coded
// coefficients. So the index advances by a fixed amount per frame, and a
// decode resumed at a frame where it wraps back to zero draws exactly the
// noise a linear decode drew there.
//
// That makes bit-exactness reachable on a noise-coded cell, which is worth
// having as a test rather than as a curiosity: a reconstruction that forgot to
// draw for the tail, drew twice in a band, or skipped an uncoded channel's
// draws would move the index off the period and fail here, and would be
// invisible to a differential that only ever decodes linearly.
func TestNoiseIndexPeriodReproducesALinearDecode(t *testing.T) {
	for _, c := range corpusCells {
		if !c.noise {
			continue
		}
		t.Run(c.name(), func(t *testing.T) {
			track, pkts := demux(t, corpusFile(t, c))
			perFrame := c.frameLen * c.channels
			// The smallest frame count that returns the index to zero.
			period := noiseTableLen / gcd(perFrame, noiseTableLen)
			p := period
			for p+period <= len(pkts)/2 {
				p += period
			}
			if p >= len(pkts)-2 {
				t.Skipf("the noise index period is %d frames, longer than the %d-packet cell", period, len(pkts))
			}
			linear := decodeAll(t, track, pkts)
			seeked := decodeFrom(t, track, pkts, p)
			ref := linear[p*perFrame:]
			n := min(len(ref), len(seeked))
			if d := testutil.CompareF32(seeked[:n], ref[:n]); d.MaxAbs != 0 {
				t.Errorf("landing on the noise index period should be bit-identical: %v", d)
			}
		})
	}
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
