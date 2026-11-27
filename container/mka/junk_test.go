package mka

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// resyncWindow is how far a resync looks, waxlabel's and ffmpeg's 64 KiB.
const resyncWindow = 64 << 10

// insertJunk returns file with junk inserted at at, a Segment child's
// offset, and every position that names a byte at or past at moved to match:
// the Cues, the SeekHead entries, and a stated Segment size.
func insertJunk(t *testing.T, file []byte, at int, junk []byte) []byte {
	t.Helper()
	_, segData, definite := segment(t, file)
	rel := uint64(at - segData)
	out := file
	if _, _, ok := segChild(t, file, idCues); ok {
		pts := cueEntriesOf(t, file)
		for i := range pts {
			if uint64(pts[i].pos) >= rel {
				pts[i].pos += int64(len(junk))
			}
		}
		out = replaceCues(t, file, pts)
	}
	out = append([]byte(nil), out...)
	// SeekPosition is a fixed 8-byte integer in this muxer's entries.
	_, shPos, _ := segChild(t, out, idSeekHead)
	_, shBody, shEnd := readElemAt(t, out, segData+shPos)
	for off := shBody; off < shEnd; {
		id, body, end := readElemAt(t, out, off)
		if id == idSeek {
			for c := body; c < end; {
				cid, cb, ce := readElemAt(t, out, c)
				if cid == idSeekPosition {
					if v := binary.BigEndian.Uint64(out[cb:ce]); v >= rel {
						binary.BigEndian.PutUint64(out[cb:ce], v+uint64(len(junk)))
					}
				}
				c = ce
			}
		}
		off = end
	}
	if definite {
		_, _, segIDOff := readElemAt(t, out, 0)
		v := binary.BigEndian.Uint64(out[segIDOff+4:])
		binary.BigEndian.PutUint64(out[segIDOff+4:], v+uint64(len(junk)))
	}
	return append(append(append([]byte(nil), out[:at]...), junk...), out[at:]...)
}

// junkDecoys is junk holding false Segment-level IDs whose sizes do not fit:
// a Cluster and a Tags running past the end of the file, and a Tags stating no
// size, which only a Cluster may.
func junkDecoys() []byte {
	zeros := func(n int) []byte { return make([]byte, n) }
	b := append(zeros(10), 0x1F, 0x43, 0xB6, 0x75, 0x08, 0xFF, 0, 0, 0, 0, 0, 0)
	b = append(append(b, zeros(10)...), 0x12, 0x54, 0xC3, 0x67, 0x08, 0xFF, 0, 0, 0)
	return append(append(append(b, zeros(10)...), 0x12, 0x54, 0xC3, 0x67, 0xFF), zeros(10)...)
}

// TestJunkBetweenSegmentChildren: bytes that head no element are stepped over
// to the next Segment-level element, up to 64 KiB of them, so every block
// arrives on every path, and a seek bounded by Cues past the junk lands where
// the read does. Junk ahead of the first cluster is met at open, which a
// strict open refuses; junk after it is met by the read and the walk, which
// strict refuses there, having delivered the blocks before it.
func TestJunkBetweenSegmentChildren(t *testing.T) {
	file := IndexPCM(t, true)
	children, blocks := segmentChildren(t, file)
	clusters := children[idCluster]
	zeros := func(n int) []byte { return make([]byte, n) }
	decoys := junkDecoys()

	for _, tc := range []struct {
		name   string
		at     int
		junk   []byte
		header bool // met at open
	}{
		{"before Info", children[idInfo][0].start, zeros(100), true},
		{"before Tracks", children[idTracks][0].start, zeros(100), true},
		{"before the first cluster", clusters[0].start, zeros(100), true},
		{"between clusters", clusters[2].start, zeros(100), false},
		{"after the last cluster", children[idCues][0].start, zeros(100), false},
		{"64 KiB between clusters", clusters[2].start, zeros(resyncWindow), false},
		{"decoys between clusters", clusters[2].start, decoys, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := insertJunk(t, file, tc.at, tc.junk)
			checkDamaged(t, damaged{
				file:         bad,
				blocks:       indexBlocks,
				strictOpen:   tc.header,
				strictBlocks: wholeBefore(blocks, tc.at),
			})
			// A seek past the junk takes the Cues bound, the cue after the
			// fourth cluster, and lands where the same seek on the clean file
			// does.
			target := int64(130) * indexBlock
			if !boundTaken(t, bad, target) {
				t.Error("a seek past the junk did not take the Cues bound")
			}
			got, _, _ := landAll(t, bad, []int64{target})
			want, _, _ := landAll(t, file, []int64{target})
			if got[0] != want[0] {
				t.Errorf("a seek to %d past the junk landed at %d, on the clean file at %d", target, got[0], want[0])
			}
		})
	}
}

// TestJunkAtTheEndOfTheSegment: junk that runs to the Segment's end follows
// every block, so all of them arrive and the junk is reported.
func TestJunkAtTheEndOfTheSegment(t *testing.T) {
	file := IndexPCM(t, true)
	bad := insertJunk(t, file, len(file), make([]byte, 100))
	checkDamaged(t, damaged{file: bad, blocks: indexBlocks, strictBlocks: indexBlocks})
}

// TestJunkPastTheResyncWindow: a run longer than 64 KiB is not stepped over.
// Between clusters the stream ends at it, so the blocks before it arrive and
// none after; ahead of the first cluster nothing is readable and the open
// refuses.
func TestJunkPastTheResyncWindow(t *testing.T) {
	file := IndexPCM(t, true)
	children, blocks := segmentChildren(t, file)
	clusters := children[idCluster]
	long := make([]byte, resyncWindow+1)

	at := clusters[2].start
	bad := insertJunk(t, file, at, long)
	d, err := NewDemuxer(container.BytesSource(bad), nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := readRun(t, d, 0)
	if err != nil || n != wholeBefore(blocks, at) {
		t.Errorf("read %d blocks (%v), want the %d before the junk", n, err, wholeBefore(blocks, at))
	}
	if dmg, _ := countWarnings(d.Warnings()); dmg != 1 {
		t.Errorf("listed %d Damage warnings, want 1: %v", dmg, d.Warnings())
	}

	head := insertJunk(t, file, clusters[0].start, long)
	if _, err := NewDemuxer(container.BytesSource(head), nil); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("an open over %d bytes of junk ahead of the first cluster = %v, want malformed", len(long), err)
	}
}

// TestJunkInAStreamedFile: a Segment of unknown size with no Cues reads past
// junk between clusters too, on every path. There any size past the file
// reads as a cut, so the decoys are the case that matters: a false ID the
// end would cut short is passed over for the real cluster behind it.
func TestJunkInAStreamedFile(t *testing.T) {
	file := IndexPCM(t, false)
	children, blocks := segmentChildren(t, file)
	at := children[idCluster][3].start
	for name, junk := range map[string][]byte{"zeros": make([]byte, 300), "decoys": junkDecoys()} {
		t.Run(name, func(t *testing.T) {
			bad := append(append(append([]byte(nil), file[:at]...), junk...), file[at:]...)
			checkDamaged(t, damaged{file: bad, blocks: indexBlocks, strictBlocks: wholeBefore(blocks, at)})
		})
	}

	// Junk ahead of the last cluster in a file the end cuts inside that
	// cluster: the cluster is real, cut short, and read up to the cut, over
	// decoys ahead of it and over a false ID inside its blocks that fits.
	last := children[idCluster][4].start
	plants := map[string][]byte{
		"": nil, "an empty Tags": {0x12, 0x54, 0xC3, 0x67, 0x80},
		"a Cues": {0x1C, 0x53, 0xBB, 0x6B, 0x84}, "a Cluster": {0x1F, 0x43, 0xB6, 0x75, 0x84},
	}
	for jname, junk := range map[string][]byte{"zeros": make([]byte, 100), "decoys": junkDecoys()} {
		for pname, plant := range plants {
			if jname == "decoys" && plant != nil {
				continue
			}
			name := "cut short after " + jname
			if plant != nil {
				name += ", " + pname + " inside a block"
			}
			t.Run(name, func(t *testing.T) {
				bad := append(append(append([]byte(nil), file[:last]...), junk...), file[last:]...)
				shift := len(junk)
				if plant != nil {
					copy(bad[blocks[166].start+shift+1000:], plant)
				}
				bad = bad[:(blocks[190].start+blocks[190].end)/2+shift]
				want := make([]int, 190)
				for i := range want {
					want[i] = i
				}
				checkBlocks(t, bad, want)
			})
		}
	}
}

// TestDamagedClusterChildSkipsTheCluster: a cluster child whose header does
// not parse loses the rest of its cluster and nothing more, since a cluster
// that states its size says where the next begins. The read and the walk
// agree on what follows, and a seek past it reads the same blocks.
func TestDamagedClusterChildSkipsTheCluster(t *testing.T) {
	file := IndexPCM(t, true)
	children, blocks := segmentChildren(t, file)
	third := children[idCluster][2]
	bad := append([]byte(nil), file...)
	bad[blocks[100].start] = 0 // no element ID
	want := []int{}
	for i, b := range blocks {
		if i < 100 || b.start >= third.end {
			want = append(want, i)
		}
	}
	values := func(d *Demuxer) []int {
		var out []int
		for {
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				if err != io.EOF {
					t.Fatalf("ReadPacket: %v", err)
				}
				return out
			}
			out = append(out, int(binary.LittleEndian.Uint32(pkt.Data))/indexBlock)
		}
	}
	d, err := NewDemuxer(container.BytesSource(bad), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := values(d); !slices.Equal(got, want) {
		t.Fatalf("read blocks %v, want %v", got, want)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != int64(len(want))*indexBlock || !tr.SamplesExact {
		t.Errorf("the walk settled %d samples, the read delivered %d", tr.Samples, len(want)*indexBlock)
	}
	s, err := NewDemuxer(container.BytesSource(bad), nil)
	if err != nil {
		t.Fatal(err)
	}
	landed, err := s.SeekSample(context.Background(), 0, 150*indexBlock)
	if err != nil {
		t.Fatal(err)
	}
	from := int(landed / indexBlock)
	if got := values(s); !slices.Equal(got, want[from:]) {
		t.Errorf("after a seek landing on delivered block %d: blocks %v, want %v", from, got, want[from:])
	}
	if _, err := readRun(t, mustStrict(t, bad), 0); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a strict read = %v, want malformed", err)
	}
}

func mustStrict(t *testing.T, file []byte) *Demuxer {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestJunkInAnUnsizedCluster: a Cluster of unknown size ends at the next
// Segment-level element, so a child whose header does not parse is stepped
// over to it, losing the rest of that cluster. Past the resync window the
// stream ends there, as between Segment children.
func TestJunkInAnUnsizedCluster(t *testing.T) {
	clean := IndexPCM(t, false)
	children, blocks := segmentChildren(t, clean)
	file := unsizeClusters(clean)
	if file == nil {
		t.Fatal("the fixture's clusters were not rewritten")
	}
	d, err := NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := readRun(t, d, 0); err != nil || n != indexBlocks {
		t.Fatalf("a strict read of unsized clusters delivered %d blocks (%v), want all %d", n, err, indexBlocks)
	}
	fourth := children[idCluster][3].start
	for _, tc := range []struct {
		name    string
		damaged int // the block whose header is broken
		resumes bool
	}{
		{"the next cluster within the window", 117, true},
		{"the next cluster past the window", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if far := fourth - blocks[tc.damaged].start; (far <= resyncWindow) != tc.resumes {
				t.Fatalf("the fourth cluster is %d bytes past block %d; the case needs it %v the window", far, tc.damaged, tc.resumes)
			}
			bad := append([]byte(nil), file...)
			bad[blocks[tc.damaged].start] = 0 // no element ID
			var want []int
			for i, b := range blocks {
				if i < tc.damaged || (tc.resumes && b.start >= fourth) {
					want = append(want, i)
				}
			}
			checkBlocks(t, bad, want)
		})
	}
}
