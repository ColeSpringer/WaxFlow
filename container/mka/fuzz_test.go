package mka

import (
	"errors"
	"io"
	"os"
	"path/filepath"
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
			var rawDur, padding int64
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
				rawDur += pkt.Dur
				padding += pkt.Padding
			}

			// A read that reached the end inside the bound saw the whole
			// payload, so a fresh demuxer's finished walk must settle on
			// exactly what those packets add up to: the walk and the read
			// convert each block's trim the same way, by construction.
			if clean && !strict {
				w, err := NewDemuxer(container.BytesSource(data), nil)
				if err == nil && w.Walk() == nil && w.Walked() {
					tr := w.Tracks()[0]
					if want := max(rawDur-tr.Delay-min(padding, rawDur), 0); tr.Samples != want {
						t.Fatalf("walk settled %d, the packets hold %d (raw %d, delay %d, padding %d)",
							tr.Samples, want, rawDur, tr.Delay, padding)
					}
					// A trim can never exceed the run it trims, however much
					// the file's DiscardPaddings add up to, and a settled
					// length can never exceed the samples the walk counted.
					if tr.Padding < 0 || tr.Padding > rawDur {
						t.Fatalf("settled padding %d against a raw run of %d", tr.Padding, rawDur)
					}
					if tr.Samples < 0 || tr.Samples > rawDur {
						t.Fatalf("settled length %d against a raw run of %d", tr.Samples, rawDur)
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
