package mka

import (
	"errors"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestCutShort: a file that ends before its Segment does delivers every
// whole block before the cut and not the one the end splits, on every path.
// A Segment that states its size says so at open, which a strict open
// refuses; one that states none is found cut where a read meets the end, so a
// strict read delivers the clusters before that one and refuses there.
func TestCutShort(t *testing.T) {
	sized := IndexPCM(t, true)
	children, blocks := segmentChildren(t, sized)
	cues := children[idCues][0]
	streamed := IndexPCM(t, false)
	sChildren, sBlocks := segmentChildren(t, streamed)
	third := sChildren[idCluster][2].start

	// A Tags element ahead of the first cluster, where ffmpeg writes one.
	tags := append([]byte{0x12, 0x54, 0xC3, 0x67, 0x40, 200, byte(idVoid), 0x40, 197}, make([]byte, 197)...)
	sizedTags := insertJunk(t, sized, children[idCluster][0].start, tags)
	first := sChildren[idCluster][0].start
	streamedTags := append(append(append([]byte(nil), streamed[:first]...), tags...), streamed[first:]...)

	mid := func(b span) int { return (b.start + b.end) / 2 }
	for _, tc := range []struct {
		name     string
		streamed bool
		cut      int
		strict   int    // blocks a strict read of the streamed file delivers
		file     []byte // the file cut, when not the plain fixture
	}{
		{name: "inside a Tags ahead of the first cluster", cut: children[idCluster][0].start + 100, file: sizedTags},
		{name: "streamed, inside a Tags ahead of the first cluster", streamed: true, cut: first + 100, file: streamedTags},
		{name: "two bytes into the first cluster's header", cut: children[idCluster][0].start + 2},
		{name: "streamed, two bytes into the first cluster's header", streamed: true, cut: first + 2},
		{name: "mid-block in the third cluster", cut: mid(blocks[100])},
		{name: "on a block boundary", cut: blocks[100].end},
		{name: "two bytes into a block header", cut: blocks[100].end + 2},
		{name: "mid-block in the first cluster", cut: mid(blocks[10])},
		{name: "where the Cues begin", cut: cues.start},
		{name: "one byte short", cut: len(sized) - 1},
		{name: "streamed, mid-block in the third cluster", streamed: true, cut: mid(sBlocks[100]), strict: wholeBefore(sBlocks, third)},
		{name: "streamed, two bytes into a block header", streamed: true, cut: sBlocks[100].end + 2, strict: wholeBefore(sBlocks, third)},
		{name: "streamed, mid-block in the first cluster", streamed: true, cut: mid(sBlocks[10])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, bl := sized, blocks
			if tc.streamed {
				file, bl = streamed, sBlocks
			}
			// A cut ahead of the first cluster is met by the open, whatever
			// the Segment states.
			headerCut := tc.file != nil || tc.cut < bl[0].start
			if tc.file != nil {
				file = tc.file
			}
			checkDamaged(t, damaged{
				file:         file[:tc.cut],
				blocks:       wholeBefore(bl, tc.cut),
				strictOpen:   !tc.streamed || headerCut,
				strictBlocks: tc.strict,
			})
		})
	}
}

// TestCutShortInsideTheHeader: a file the end cuts before its Tracks have
// been read has no track to read, and is refused as malformed rather than
// read as having none: cut inside Tracks, or inside the Void ffmpeg writes
// ahead of Info.
func TestCutShortInsideTheHeader(t *testing.T) {
	file := IndexPCM(t, true)
	children, _ := segmentChildren(t, file)
	tracks := children[idTracks][0]
	for name, cut := range map[string][]byte{
		"inside Tracks":   file[:(tracks.start+tracks.end)/2],
		"inside the Void": fixture(t, "seed-flac.mka")[:200],
	} {
		_, err := NewDemuxer(container.BytesSource(cut), nil)
		if !errors.Is(err, waxerr.ErrMalformedInput) {
			t.Errorf("a file cut %s opened with %v, want malformed", name, err)
		}
	}
}
