package wmalossless_test

// Seeking, which for this codec is the same question as "where may a decode
// begin".
//
// Nothing in a packet says where its audio starts. A packet's payload usually
// opens in the middle of a frame, the frames it begins are emitted a packet
// later, and its presentation time is not a multiple of the frame length, so
// no arithmetic turns a packet index into a sample position. What a decoder
// can do is start anywhere, throw away what it cannot reconstruct, and resume
// at the first subframe that restates the filter state.
//
// The property that gives is strong enough to assert exactly: because a
// seekable tile clears every filter, the samples a resumed decode produces are
// bit-identical to the same samples of a decode that started at the beginning.
// No tolerance, no correlation gate, no ratio.

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
	"github.com/colespringer/waxflow/container"
)

// wantLandings is how many of a cell's packets are a landing that reaches a
// seekable tile before the end of the file. See the comment at the assertion.
var wantLandings = map[string]int{
	"ll-44100-2ch-16":      3, // of 4 packets
	"ll-44100-2ch-16-dup":  1, // of 2
	"ll-44100-2ch-24":      5, // of 6
	"ll-44100-2ch-16-skip": 3, // of 3
	"ll-44100-2ch-24-pad":  3, // of 4
	"ll-48000-6ch-24":      1, // of 9, and the one is packet 0
}

// decodeFrom decodes packets[k:] through a decoder that has just been Reset,
// which is what a seek landing on that packet produces.
//
// The run-up before the Reset matters and is not decoration. Resetting a FRESH
// decoder tests nothing: a new decoder already holds every value Reset assigns
// -- empty carry, unknown sequence number, no filter state, no latched failure
// -- so an empty Reset body would leave every seek test in this file green,
// bit-exactness included. The only Reset that can fail is one on a decoder
// that has state to discard, which is also the only kind format.Media ever
// performs. codec/wma's seek test carries the same note for the same reason.
func decodeFrom(t *testing.T, track container.Track, pkts [][]byte, k int) []int32 {
	t.Helper()
	cfg, err := wmalossless.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmalossless.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	// The run-up stops exactly where the landing begins, so the sequence
	// numbers either side of the Reset are consecutive. Running up further
	// would hide the whole thing: the decoder's own recovery from a sequence
	// jump drops the carry and the filter state too, so a run-up that leaves a
	// gap makes an empty Reset indistinguishable from a real one.
	drop := func(*audio.Buffer) error { return nil }
	for i := 0; i < k; i++ {
		if err := dec.Decode(pkts[i], drop); err != nil {
			t.Fatalf("run-up packet %d: %v", i, err)
		}
	}
	dec.Reset()
	var got []int32
	emit := func(b *audio.Buffer) error {
		got = append(got, interleave(b)...)
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
// same length or shorter, and identical sample for sample where they overlap.
func TestSeekLandsBitExact(t *testing.T) {
	for _, c := range cells {
		if !c.committed {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			full := decodeAll(t, track, pkts)
			landed := 0
			for k := range pkts {
				got := decodeFrom(t, track, pkts, k)
				if len(got) == 0 {
					// A landing that reaches no seekable tile before the end
					// of the file. Legal, and not damage.
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
						t.Fatalf("packet %d: sample %d of %d differs from the continuous decode: got %d, want %d",
							k, i, len(got), got[i], tail[i])
					}
				}
			}
			// Measured, and asserted rather than only checked for zero: a
			// decoder that dropped the landing packet's own frames along with
			// the undecodable carry still lands, one packet later each time,
			// and would pass a "more than none" check. The 5.1 cell recovers
			// only from its first packet because the encoder spaces seekable
			// tiles about 0.4 s apart and that cell is 0.17 s long, so eight
			// of its nine landings reach no tile. That is legal, not damage.
			if want := wantLandings[c.name]; landed != want {
				t.Errorf("%d of %d landings produced output, want %d", landed, len(pkts), want)
			}
		})
	}
}

// TestSeekToTheStartReproducesTheWholeFile is the degenerate landing, and it
// is worth its own cell: it is the one that proves Reset leaves nothing
// behind. A decoder that kept a carry or a filter across the reset would
// differ here and nowhere else.
func TestSeekToTheStartReproducesTheWholeFile(t *testing.T) {
	for _, c := range cells {
		if !c.committed {
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
					t.Fatalf("sample %d differs after a reset: got %d, want %d", i, again[i], full[i])
				}
			}
		})
	}
}
