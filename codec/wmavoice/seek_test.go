//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
	"github.com/colespringer/waxflow/container"
)

// Resuming. Every media object is a decode start point: discard the carry,
// skip the packet's spillover bits unread, and begin at the first superframe
// that starts in the packet. What a resumed decode does NOT do is come back
// exact at a fixed pre-roll, and that is a property of the codec rather than
// of this decoder: the adaptive codebook is a pitch-lag copy of the decoder's
// own past output with a gain near 1, so a wrong history decays only as fast
// as that gain lets it (docs/notes/wma-voice-bitstream.md section 13).

// seekTail is how much of a file a resume needs behind it before "did it
// become exact" is a question about the decoder rather than about the file
// running out. Measured on this corpus, every resume with a shorter tail than
// this is still converging when the stream ends, and every resume with a
// longer one converges.
const seekTail = 64

// seekMedian is the median convergence this decoder reaches, with headroom.
// It is a measurement and not a property of the format: "exact and stays
// exact" is a bit-equality threshold on a recursive float filter, so two
// correct decoders whose arithmetic differs in the last bits cross it at
// different points. The note's own decoder crossed it about twice as early.
const seekMedian = 16

// TestResumeConvergesFromEveryObject restarts the decoder at every media
// object of every cell and measures how long the resumed decode takes to match
// the linear one.
func TestResumeConvergesFromEveryObject(t *testing.T) {
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			ref := decodeAll(t, track, pkts)
			starts := superframeStarts(t, track, pkts)
			total := (len(ref) + wmavoice.SuperframeSamples - 1) / wmavoice.SuperframeSamples

			var dist []int
			for k := 1; k < len(pkts); k++ {
				at := starts[k]
				got := resume(t, track, pkts[k:], int64(at)*wmavoice.SuperframeSamples)
				n := convergedAfter(got, ref[at*wmavoice.SuperframeSamples:])
				if n < 0 {
					// Still converging when the stream ended. That is only a
					// finding when there was room to converge in.
					if total-at >= seekTail {
						t.Errorf("a resume at object %d (superframe %d of %d) never matches the linear decode",
							k, at, total)
					}
					continue
				}
				dist = append(dist, n)
			}
			if len(dist) == 0 {
				t.Fatal("no resume point had room to converge")
			}
			slices.Sort(dist)
			median := dist[len(dist)/2]
			if median > seekMedian {
				t.Errorf("the median resume converges after %d superframes, want at most %d", median, seekMedian)
			}
			t.Logf("%d resume points with room: median %d, worst %d superframes",
				len(dist), median, dist[len(dist)-1])
		})
	}
}

// TestResumeWithoutThePositionNeverConverges is what gives SetPosition teeth.
// Comfort noise is drawn from a codebook at an offset that is a function of the
// frame index since the decoder started, so a resumed decode whose counter
// starts at zero draws different noise in every silent passage, for good. With
// the counter left alone the decode is not merely slow to converge: it never
// does.
func TestResumeWithoutThePositionNeverConverges(t *testing.T) {
	c := cells[0] // the cell with the most silence frames per superframe
	track, pkts := demux(t, corpusPath(t, c.name))
	ref := decodeAll(t, track, pkts)
	starts := superframeStarts(t, track, pkts)

	never := 0
	for k := 1; k < len(pkts); k++ {
		at := starts[k]
		got := resume(t, track, pkts[k:], -1)
		if convergedAfter(got, ref[at*wmavoice.SuperframeSamples:]) < 0 {
			never++
		}
	}
	if never == 0 {
		t.Error("every resume converged without its position, so the frame counter reaches nothing")
	}
	t.Logf("%d of %d resumes never converge without the landing", never, len(pkts)-1)

	// The direct statement, which does not depend on how long convergence
	// takes: the same landing with and without its position draws different
	// comfort noise, so the counter is reaching the generator.
	at := starts[1]
	with := resume(t, track, pkts[1:], int64(at)*wmavoice.SuperframeSamples)
	without := resume(t, track, pkts[1:], -1)
	same := true
	for i := range min(len(with), len(without)) {
		if with[i] != without[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("a resume decodes the same samples with and without its landing")
	}
}

// resume decodes from one packet on, telling the decoder where it landed. A
// position below zero leaves the counter alone, which is what the test above
// measures.
func resume(t testing.TB, track container.Track, pkts [][]byte, at int64) []float32 {
	t.Helper()
	dec := newDecoder(t, track)
	defer dec.Release()
	dec.Reset()
	if at >= 0 {
		dec.SetPosition(at)
	}
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
		return nil
	}
	for i, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("resumed packet %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("resumed drain: %v", err)
	}
	return got
}

// superframeStarts is the linear superframe index each packet's BODY begins
// at, which is where a resume at that packet lands. Packet k emits the
// superframe its predecessor carried and then its own body, so the body starts
// one past what the packets before it emitted.
func superframeStarts(t testing.TB, track container.Track, pkts [][]byte) []int {
	t.Helper()
	dec := newDecoder(t, track)
	defer dec.Release()
	out := make([]int, len(pkts))
	n := 0
	emit := func(*audio.Buffer) error { n++; return nil }
	for i, p := range pkts {
		if i > 0 {
			out[i] = n + 1
		}
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	return out
}

// convergedAfter reports how many superframes of got differ from ref before it
// becomes exact and stays exact, or -1 when it never does.
func convergedAfter(got, ref []float32) int {
	n := min(len(got), len(ref))
	last := -1
	for i := range n {
		if got[i] != ref[i] {
			last = i
		}
	}
	if last < 0 {
		return 0
	}
	if last >= n-1 {
		return -1
	}
	return last/wmavoice.SuperframeSamples + 1
}

// TestResumeStaysADecodeOfTheSameAudio is the other half of the seek story.
// A resume into sustained voiced speech can stay apart from the linear decode
// for a long time (the note's own 90th percentile is still 0.97 of the signal
// three superframes in), so sample agreement is the wrong thing to ask for
// there. What must hold is that the resume is a decode of the same material
// and not a divergence: the right length, no infinities, and a level that
// tracks the linear decode's.
func TestResumeStaysADecodeOfTheSameAudio(t *testing.T) {
	const preRoll = 3 * wmavoice.SuperframeSamples
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			ref := decodeAll(t, track, pkts)
			starts := superframeStarts(t, track, pkts)
			for k := 1; k < len(pkts); k++ {
				at := starts[k] * wmavoice.SuperframeSamples
				got := resume(t, track, pkts[k:], int64(at))
				// The landing itself: a resume produces exactly the linear
				// decode's tail, sample for sample in count.
				if len(got)+at != len(ref) {
					t.Fatalf("a resume at object %d gives %d samples, the linear tail is %d",
						k, len(got), len(ref)-at)
				}
				if len(got) <= preRoll {
					continue
				}
				var gs, rs float64
				var gp, rp float64
				for i := preRoll; i < len(got); i++ {
					g, r := float64(got[i]), float64(ref[at+i])
					if math.IsNaN(g) || math.IsInf(g, 0) {
						t.Fatalf("a resume at object %d gives %v at sample %d", k, got[i], i)
					}
					gs += g * g
					rs += r * r
					gp = math.Max(gp, math.Abs(g))
					rp = math.Max(rp, math.Abs(r))
				}
				if rs == 0 {
					continue
				}
				// The bounds are lopsided on purpose. A resume into sustained
				// voiced speech UNDER-shoots while the adaptive codebook's
				// memory rebuilds, and measured, a landing three superframes
				// from the end of the final loud segment runs at a tenth of
				// the linear decode's level. Over-shooting is the failure that
				// matters: it is what a wrong gain or a diverging filter
				// looks like.
				if ratio := math.Sqrt(gs / rs); ratio < 0.05 || ratio > 2 {
					t.Errorf("a resume at object %d runs at %.2f of the linear decode's level", k, ratio)
				}
				if gp > 2*rp+0.05 {
					t.Errorf("a resume at object %d peaks at %.3f against the linear decode's %.3f", k, gp, rp)
				}
			}
		})
	}
}
