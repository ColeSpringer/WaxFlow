package flacn_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production,
// positions that never go backwards across a resync, and seeks that land
// where the next packet starts and never past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-s16.flac")
	f.Add(full)
	f.Add(full[:200])
	f.Add(full[:4+4+34])
	f.Add([]byte("fLaC\x80\x00\x00\x22"))
	noise := fixture(f, "noise-s16.flac")
	f.Add(noise)
	// A stream-copy cut's shape: the first frame numbered past 0.
	f.Add(testutil.RenumberFLACFrames(f, full, 1000))
	// Frames whose bytes do not check, mid-stream: the resync.
	f.Add(damageFrames(f, full, frameSpans(f, full), 1))
	f.Add(damageFrames(f, noise, frameSpans(f, noise), 0, 2))
	// Frames behind a hole numbered past what its bytes held: a read ends
	// short of the length the closing frame confirms.
	f.Add(farBehindAHole(f, full, 1, 5000))
	// Block size bounds no stream may state, over frames that count by one.
	f.Add(withBounds(full, 4608, 256))
	// The short last frame in front of the one before it: read in order, it
	// lies wholly past the closing frame's end.
	fr := testutil.FLACFrames(f, full)
	part := func(k int) []byte { return full[fr[k].Off:fr[k].End] }
	f.Add(slices.Concat(full[:fr[0].Off], part(0), part(1), part(3), part(2)))
	// A tagger's stack behind the frames, a Lyrics3 block in the middle.
	f.Add(slices.Concat(full, apeTag(true), []byte("LYRICSBEGININD0000210LYR00005hello000034LYRICS200"), id3v1Tag()))
	// A frame longer than the step its number gives: the read delivers more
	// than the closing frame's end, which the open called exact.
	f.Add(longFrameAt(f, packageFixture(f, "golden-noise-s16-l0.flac"), 1, packageFixture(f, "golden-sine-s16-l5.flac")))
	// Both: frames overlapping in front of the length, and the short last
	// frame, swapped behind the closing one, wholly past it.
	long := longFrameAt(f, packageFixture(f, "golden-noise-s16-l0.flac"), 1, packageFixture(f, "golden-sine-s16-l5.flac"))
	lf := testutil.FLACFrames(f, long)
	f.Add(slices.Concat(long[:lf[0].Off], long[lf[0].Off:lf[2].Off], long[lf[3].Off:lf[3].End], long[lf[2].Off:lf[2].End]))

	f.Fuzz(func(t *testing.T, data []byte) {
		// The length the open stated, and where the last packet starting
		// inside it ends; positions only rise, so the rest start past it.
		var packets, lastPTS, opened, inEnd int64
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return flacn.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			},
			// A frame is at least 9 bytes (header, one subframe byte, and
			// the CRC-16), so packet production is bounded by the input.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/6 + 4 },
			Targets:    []int64{0, 1000, 5000, 1 << 40},
			// Landing past the target is only legitimate when the stream
			// itself starts after it.
			LateLanding: true,
			SeekPTS:     true,
			ExactLength: true,
			// A read may hold more than the open called exact only where
			// the packets starting inside it end inside it, the excess being
			// frames longer than their numbers' step, overlapping, and the
			// rest start past it, numbered past the closing frame the open saw.
			OpenLength: true,
			LongerWhen: func(container.Demuxer) bool { return inEnd <= opened },
			Opened: func(_ testing.TB, d container.Demuxer, _ bool) {
				packets, lastPTS, inEnd, opened = 0, -1, 0, d.Tracks()[0].Samples
			},
			Packet: func(t testing.TB, pkt *container.Packet) {
				if pkt.PTS < 0 {
					t.Fatalf("packet %d at %d: positions start at 0", packets, pkt.PTS)
				}
				if pkt.PTS <= lastPTS {
					t.Fatalf("packet %d at %d after one at %d: positions went backwards", packets, pkt.PTS, lastPTS)
				}
				if pkt.PTS < opened {
					inEnd = pkt.PTS + pkt.Dur
				}
				lastPTS = pkt.PTS
				packets++
			},
			Landed: func(t testing.TB, _ container.Demuxer, target, landed int64) {
				if landed < 0 {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
			},
		}.Run(t, data)
	})
}

// longFrameAt returns raw with the first frame of other, a stream of the same
// shape and a larger block size, put in front of frame at and numbered for
// that place, and every frame behind it numbered one on.
func longFrameAt(t testing.TB, raw []byte, at int, other []byte) []byte {
	t.Helper()
	frames, long := testutil.FLACFrames(t, raw), testutil.FLACFrames(t, other)[0]
	out := append([]byte(nil), raw[:frames[at].Off]...)
	out = append(out, testutil.SetFLACFrameNumber(t, other[long.Off:long.End], false, uint64(at))...)
	for k := at; k < len(frames); k++ {
		out = append(out, testutil.SetFLACFrameNumber(t, raw[frames[k].Off:frames[k].End], false, uint64(k+1))...)
	}
	return out
}

// packageFixture loads a file from this package's testdata.
func packageFixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
