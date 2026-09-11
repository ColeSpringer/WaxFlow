//go:build !wmaprotablesgen

package wmapro_test

// Seeking, which for this codec is the same question as "where may a decode
// begin", and the answer is everywhere: every packet is a start point, at the
// cost of the one frame of lead-in that overlap-adds against a tail the
// decoder does not have.
//
// The property that gives is strong enough to assert exactly. The only state
// that crosses a frame boundary is that tail and the previous subframe
// length, so the second frame after any start is bit-identical to the same
// frame of a decode from the beginning. Measured at every packet of every
// file in the corpus note, and asserted here at every packet of every cell.

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmapro"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// wantLandings is how many of a cell's packets deliver samples when a decode
// starts there. A packet whose only frame is the lead-in delivers nothing,
// which is legal and not damage.
var wantLandings = map[string]int{
	"pro-44100-2ch-16-128k":       6, // of 6 packets
	"pro-44100-6ch-16-128k":       6, // of 6
	"pro-48000-6ch-24-384k":       7, // of 7
	"pro-96000-2ch-24-384k":       6, // of 7
	"pro-44100-2ch-16-128k-tonal": 7, // of 7
}

// decodeFrom decodes packets[k:] through a decoder that has just been Reset,
// which is what a seek landing on that packet produces.
//
// The run-up before the Reset is not decoration. Resetting a FRESH decoder
// tests nothing: a new decoder already holds every value Reset assigns, so an
// empty Reset body would leave every cell here green. The only Reset that can
// fail is one on a decoder with state to discard, which is also the only kind
// format.Media ever performs.
func decodeFrom(t *testing.T, track container.Track, pkts [][]byte, k int) []float32 {
	t.Helper()
	cfg, err := wmapro.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmapro.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	// The run-up stops exactly where the landing begins, so the sequence
	// numbers either side of the Reset are consecutive and the decoder's own
	// recovery from a jump cannot stand in for the Reset.
	drop := func(*audio.Buffer) error { return nil }
	for i := 0; i < k; i++ {
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
	for i := k; i < len(pkts); i++ {
		if err := dec.Decode(pkts[i], emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

// TestSeekLandsBitExact starts a decode at every packet of every committed
// cell. Whatever comes out has to be exactly the tail of the whole decode:
// same length or shorter, and identical bit for bit where they overlap.
func TestSeekLandsBitExact(t *testing.T) {
	for _, c := range committedCells() {
		if c.refused {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			full := decodeAll(t, track, pkts)
			landed := 0
			for k := range pkts {
				got := decodeFrom(t, track, pkts, k)
				if len(got) == 0 {
					continue
				}
				landed++
				if len(got) > len(full) {
					t.Fatalf("packet %d: a resumed decode produced %d samples, more than the whole file's %d",
						k, len(got), len(full))
				}
				tail := full[len(full)-len(got):]
				for i := range got {
					if got[i] != tail[i] {
						t.Fatalf("packet %d: sample %d of %d differs from the continuous decode: got %g, want %g",
							k, i, len(got), got[i], tail[i])
					}
				}
			}
			// Asserted rather than checked for zero: a decoder that dropped
			// the landing packet's own frames along with the carry would
			// still land, one packet later each time, and pass a
			// "more than none" check. The 96 kHz cell ends on a packet holding
			// a single frame, which is the lead-in and nothing else.
			if want := wantLandings[c.name]; landed != want {
				t.Errorf("%d of %d landings produced output, want %d", landed, len(pkts), want)
			}
		})
	}
}

// TestSeekToTheStartReproducesTheWholeFile is the degenerate landing and the
// one that proves Reset leaves nothing behind: a decoder that kept a carry, a
// rolling buffer or a previous subframe length across the reset would differ
// here and nowhere else.
func TestSeekToTheStartReproducesTheWholeFile(t *testing.T) {
	for _, c := range committedCells() {
		if c.refused {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			full := decodeAll(t, track, pkts)
			again := decodeFrom(t, track, pkts, 0)
			if len(again) != len(full) {
				t.Fatalf("a reset decode produced %d samples, the first pass %d", len(again), len(full))
			}
			for i := range full {
				if again[i] != full[i] {
					t.Fatalf("sample %d differs after a reset: got %g, want %g", i, again[i], full[i])
				}
			}
		})
	}
}
