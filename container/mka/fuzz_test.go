package mka

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FuzzDemux exercises the EBML parser, the block/lacing splitter, the Cues
// parser, and the seek paths. EBML nesting is an attack surface, so the
// invariants are: no panic, no unbounded work, accepted tracks are well-formed,
// packet production is bounded by the input size, seeks never overshoot the
// target, and a walk's settled length is the run the packets reported (a
// tolerated hole in the payload shortens both sides, so they still agree).
// The packets are stamped on the trimmed timeline, so the run they report is
// what they deliver plus the final packet's own trim.
// seed-cues.mka is the only seed carrying a Cues index.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"seed-opus.webm", "seed-flac.mka", "seed-pcm.mka", "seed-cues.mka"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:min(len(full), 200)])
	}
	// A bare EBML magic so the sniff accepts the input and the parser runs.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80})
	// EBML header then a Segment with an unknown (all-ones) size.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xFF})
	// A vint claiming an 8-byte width to probe the size guard.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			tracks := d.Tracks()
			if len(tracks) != 1 {
				t.Fatalf("accepted input with %d tracks", len(tracks))
			}
			if err := tracks[0].Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}

			// Every packet consumes input, so the count is bounded by the size.
			maxPackets := len(data) + 16
			var pkt container.Packet
			// rawDur is every frame's own length; kept is what the packets
			// deliver, which is the timeline they are stamped on; lastPad and
			// lastDur describe the final packet, whose trim is the only one
			// that stays inside the run. inner is every earlier packet's trim
			// with its place on the raw timeline, which is what a walk records.
			var rawDur, kept, lastPad, lastDur int64
			var inner []container.PacketTrim
			clean := false
			for i := 0; i < maxPackets; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					clean = true
					break
				}
				if err != nil {
					break
				}
				if pkt.Dur < 0 {
					t.Fatalf("packet with negative duration %d", pkt.Dur)
				}
				if pkt.Padding < 0 {
					t.Fatalf("packet with negative padding %d", pkt.Padding)
				}
				if c := min(lastPad, lastDur); c > 0 {
					inner = append(inner, container.PacketTrim{Pos: rawDur - c, Samples: c})
				}
				rawDur += pkt.Dur
				kept += pkt.Dur - min(pkt.Padding, pkt.Dur)
				lastPad, lastDur = pkt.Padding, pkt.Dur
			}

			// A read that reached the end inside the bound saw the whole
			// payload, so a fresh demuxer's finished walk must settle on
			// exactly what those packets add up to: the walk and the read
			// convert each block's trim the same way, by construction.
			//
			// excess is a final trim past its own frame, which no frame can
			// absorb and only the settled length can remove.
			if clean && !strict {
				w, err := NewDemuxer(container.BytesSource(data), nil)
				if err == nil && w.Walk() == nil && w.Walked() {
					tr := w.Tracks()[0]
					rawTotal := kept + min(lastPad, lastDur)
					excess := lastPad - min(lastPad, lastDur)
					if want := max(kept-tr.Delay-excess, 0); tr.Samples != want {
						t.Fatalf("walk settled %d, the packets hold %d (kept %d, delay %d, final trim %d of %d)",
							tr.Samples, want, kept, tr.Delay, lastPad, lastDur)
					}
					// A trim can never exceed the run it trims, however much
					// the file's DiscardPaddings add up to, and a settled
					// length can never exceed the samples the walk counted.
					if want := min(lastPad, rawTotal); tr.Padding != want {
						t.Fatalf("settled padding %d, want the final packet's trim bounded by the run (%d)",
							tr.Padding, want)
					}
					if tr.Samples < 0 || tr.Samples > rawDur {
						t.Fatalf("settled length %d against a raw run of %d", tr.Samples, rawDur)
					}
					// The inner trims are real frames the packets held, so
					// their sum cannot exceed what the frames decode to, and
					// the walk places each one where the read met it.
					if tr.MidPadding < 0 || tr.MidPadding > rawDur {
						t.Fatalf("settled mid padding %d against a raw run of %d", tr.MidPadding, rawDur)
					}
					if len(inner) <= maxMidTrims {
						if !slices.Equal(tr.MidTrims, inner) {
							t.Fatalf("walk recorded trims %v, the packets carried %v", tr.MidTrims, inner)
						}
						if !tr.MidTrimsComplete() {
							t.Fatalf("walk recorded trims %v, which do not add up to MidPadding %d", tr.MidTrims, tr.MidPadding)
						}
					}
				}
			}

			// Seeks land at or before the target (or at the earliest sync point).
			for _, target := range []int64{0, 1, 1000, 1 << 20, 1 << 40} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					continue
				}
				if landed > target {
					t.Fatalf("seek to %d overshot to %d", target, landed)
				}
			}
		}
	})
}
