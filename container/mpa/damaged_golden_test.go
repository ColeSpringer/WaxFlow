package mpa

import (
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the frame scan lands at the head, behind the metadata frame,
// between frames and at the end. Regenerate with `make goldens`.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return NewDemuxer(src, nil) }
	for i, file := range []string{
		"testdata/golden-44k-mono-128-seek.mp3",   // MPEG-1
		"testdata/golden-24k-stereo-96-mpeg2.mp3", // MPEG-2
		"../../testdata/sine-8000-cbr16.mp3",      // MPEG-2.5
		"../../testdata/sine-22050-vbr.mp3",       // Xing, frames of several sizes
		"../../testdata/sine-untagged.mp3",        // no metadata frame
		"../../testdata/sine-id3.mp3",             // a leading ID3v2 tag, which the open skips
	} {
		testutil.GoldenDamagedRead(t, file, open, *update, func(t *testing.T, raw []byte) []testutil.Damage {
			// Frame starts behind any leading tag, by each header's own
			// length, then the end.
			head := int(id3.Size(raw))
			var frames []int
			var first mp3.Header
			for off := head; off < len(raw); {
				h, err := mp3.ParseHeader(raw[off:])
				if err != nil || h.Size() == 0 {
					t.Fatalf("no sized frame header at %d: %v", off, err)
				}
				if len(frames) == 0 {
					first = h
				}
				frames = append(frames, off)
				off += h.Size()
			}
			mid, last := len(frames)/2, len(frames)-1
			frames = append(frames, len(raw))
			damages := []testutil.Damage{
				testutil.JunkAt("junk-head", raw, head, 0xFF),
				testutil.JunkOf("junk-span", raw, head, testutil.SpanJunkLen, 0xFF),
			}
			if _, tagged := mpegframes.ParseVBRTag(first, raw[frames[0]:frames[1]]); tagged {
				// A kin header of no stated size confirms the metadata frame
				// and is refused as the first audio frame, which starts the
				// scan behind it.
				free := testutil.JunkAt("free-after-tag", raw, frames[1], 0xFF)
				copy(free.Raw[frames[1]:], raw[frames[1]:frames[1]+mp3.HeaderLen])
				free.Raw[frames[1]+2] &= 0x0F
				free.What += ", its first four a header of the free format"
				damages = append(damages,
					// Plain junk leaves the metadata frame unconfirmed, so the
					// first scan passes over it too.
					testutil.JunkAt("junk-after-tag", raw, frames[1], 0xFF), free)
			}
			damages = append(damages,
				testutil.JunkAt("junk-mid", raw, frames[mid], 0xFF),
				testutil.DecoyAt("decoy-mid", raw, frames[mid], 0xFF),
				// A lone frame has no header behind it to confirm it, one with
				// the next frame's head does, and the last frame needs none.
				testutil.IslandAt("junk-island", raw, frames[mid], frames[mid+1], 0xFF),
				testutil.IslandAt("junk-island-header", raw, frames[mid],
					frames[mid+1]+min(32, frames[mid+2]-frames[mid+1]-1), 0xFF),
				testutil.JunkAt("junk-last", raw, frames[last], 0xFF),
				testutil.JunkAt("junk-tail", raw, len(raw), 0xFF))
			if i == 0 {
				// A frame at the last offset the scan takes, and at the first
				// it does not. The literal mirrors maxResync in mpegframes
				// (walker.go), which is unexported there.
				const maxResync = 1 << 20
				damages = append(damages,
					testutil.JunkOf("junk-under-bound", raw, head, maxResync-1, 0xFF),
					testutil.JunkOf("junk-past-bound", raw, head, maxResync, 0xFF))
			}
			return damages
		})
	}
}
