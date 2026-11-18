package mp4

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// readToEnd reads d until its read stops and returns the packets and the end.
func readToEnd(d *Demuxer) (int, error) {
	var pkt container.Packet
	for n := 0; ; n++ {
		if err := d.ReadPacket(&pkt); err != nil {
			return n, err
		}
	}
}

// headDeclares reports whether d lists the end of the fragments falling short
// of a declared length.
func headDeclares(d *Demuxer) bool {
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, "the head declares") {
			return true
		}
	}
	return false
}

// TestAnUnstatedLengthIsNotHeldAgainstTheRead: a head that states no length
// has nothing for the fragments to fall short of, so a read ends where its
// packets end, whether a walk ran before it, during it, after it or never.
// The pre-skip runs past the fragments, so a walk settles the length at 0
// with the Delay alone past the run.
func TestAnUnstatedLengthIsNotHeldAgainstTheRead(t *testing.T) {
	const npkt = 2
	raw := fragFileWithSidx(t, npkt, 3000, nil)
	// walkAt is the packets read before the walk, -1 for none; npkt walks
	// just before the read that ends, and after walks once the read has
	// ended and then reads once more.
	for _, strict := range []bool{false, true} {
		for _, tc := range []struct {
			walkAt int
			after  bool
		}{{-1, false}, {0, false}, {1, false}, {npkt, false}, {-1, true}} {
			t.Run(fmt.Sprintf("strict %v walk at %d after %v", strict, tc.walkAt, tc.after), func(t *testing.T) {
				d, err := openFrag(t, raw, strict)
				if err != nil {
					t.Fatal(err)
				}
				if tr := d.Tracks()[0]; tr.Samples >= 0 && !tr.SamplesAdvisory {
					t.Fatalf("the head states %d samples, want none", tr.Samples)
				}
				damage := container.DamageOf(d)
				walk := func() {
					if err := d.Walk(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				var pkt container.Packet
				var end error
				n := 0
				for ; ; n++ {
					if n == tc.walkAt {
						walk()
					}
					if end = d.ReadPacket(&pkt); end != nil {
						break
					}
				}
				if n != npkt || end != io.EOF {
					t.Errorf("the read ended with %v after %d packets, want io.EOF after %d", end, n, npkt)
				}
				if tc.after {
					walk()
					if err := d.ReadPacket(&pkt); err != io.EOF {
						t.Errorf("a read past the end after a walk = %v, want io.EOF", err)
					}
				}
				if headDeclares(d) {
					t.Errorf("a head that states no length was held to one: %v", d.Warnings())
				}
				if got := container.DamageOf(d); got != damage {
					t.Errorf("the read counted %d damage, %d at open", got, damage)
				}
			})
		}
	}
}

// TestAStatedLengthTheFragmentsFallShortOfIsDamage: a segment index that
// states more than the fragments hold is damage the end of the fragments
// finds, tolerated with a warning and refused under Strict, walked or not.
func TestAStatedLengthTheFragmentsFallShortOfIsDamage(t *testing.T) {
	const npkt = 4
	raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, dur int64) []byte {
		return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(dur + 5*960)}})
	})
	for _, strict := range []bool{false, true} {
		for _, walk := range []bool{false, true} {
			t.Run(fmt.Sprintf("strict %v walk %v", strict, walk), func(t *testing.T) {
				d, err := openFrag(t, raw, strict)
				if err != nil {
					t.Fatal(err)
				}
				if walk {
					err := d.Walk(context.Background())
					if strict != (err != nil) {
						t.Fatalf("Walk = %v under strict %v", err, strict)
					}
				}
				n, end := readToEnd(d)
				switch {
				case n != npkt:
					t.Errorf("the read delivered %d packets, want %d", n, npkt)
				case strict && (end == io.EOF || !strings.Contains(end.Error(), "the head declares")):
					t.Errorf("a strict read ended with %v, want the shortfall refused", end)
				case !strict && end != io.EOF:
					t.Errorf("a tolerant read ended with %v, want io.EOF", end)
				case !strict && (!headDeclares(d) || container.DamageOf(d) == 0):
					t.Errorf("a tolerant read listed %v, want the shortfall as damage", d.Warnings())
				}
			})
		}
	}
}
