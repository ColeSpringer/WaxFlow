package ogg

// What verifying an Ogg-FLAC's declared total costs at open.

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// synthOggFLAC builds a stream of n one-frame pages (4096 samples each) whose
// STREAMINFO declares the total those frames add up to. It exists to be longer
// than the tail scan's window, which no committed fixture is.
func synthOggFLAC(t *testing.T, n int) []byte {
	t.Helper()
	const bs = 4096
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	for i := range n {
		flags := byte(0)
		if i == n-1 {
			flags = flagEOS
		}
		// Equal STREAMINFO block bounds, so coded numbers are frame numbers.
		stream = append(stream, buildPage(flags, int64(i+1)*bs, 77, uint32(i+2),
			buildFrameBytes(t, uint64(i), bs))...)
	}
	return patchOggFLACTotalB(t, stream, int64(n)*bs)
}

// TestOggFLACDeclaredTotalDoesNotScaleWithTheStream pins the price of the
// verification, which is a read the open used to skip whenever a total was
// declared: the scan starts one window from the end of the file and follows
// each page's own length forward, so the open costs the same for a stream of
// any length and no more than that window plus the one resync buffer its first
// page needs.
//
// Two lengths, both past the window, and the cost has to be identical. The
// bound matters as much: these streams put one tiny frame in each page, which
// is the densest an Ogg stream can be, and asking nextPageAt for every page
// instead of following their lengths re-read a 64 KiB resync buffer per page,
// which cost 160 MB here.
func TestOggFLACDeclaredTotalDoesNotScaleWithTheStream(t *testing.T) {
	const window = maxPageSize + 64<<10
	var costs [2][2]int64
	for i, pages := range []int{6000, 12000} {
		raw := synthOggFLAC(t, pages)
		if int64(len(raw)) < window {
			t.Fatalf("%d pages is %d bytes, want more than the %d-byte window", pages, len(raw), window)
		}
		cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := NewDemuxer(cs, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d pages (%d bytes): %d bytes in %d reads", pages, len(raw), cs.Bytes, cs.Reads)
		if tr := d.Tracks()[0]; !tr.SamplesExact {
			t.Fatal("the declared total was not verified")
		}
		if want := int64(window + 64<<10 + 8192); cs.Bytes > want {
			t.Errorf("opening a %d-byte stream read %d bytes, want at most the window plus one resync buffer (%d)",
				len(raw), cs.Bytes, want)
		}
		costs[i] = [2]int64{int64(cs.Reads), cs.Bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening the shorter stream cost %v (reads, bytes) and the longer %v; the open must not scale with the stream",
			costs[0], costs[1])
	}
}

// TestOggFLACDeclaredTotalCostsOneTailWindow is the same price in the shape a
// producer actually writes: the committed fixtures hold their audio in one
// page, so the scan covers the file itself and the open stays inside the head
// plus one window.
func TestOggFLACDeclaredTotalCostsOneTailWindow(t *testing.T) {
	for _, name := range []string{"sine-s16.oga", "noise-s24.oga"} {
		t.Run(name, func(t *testing.T) {
			base, _ := probeTotal(t, fixture(t, name))
			raw := patchOggFLACTotal(t, fixture(t, name), base.Samples)
			cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
			d, err := NewDemuxer(cs, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d bytes of %d in %d reads", name, cs.Bytes, len(raw), cs.Reads)
			if tr := d.Tracks()[0]; !tr.SamplesExact {
				t.Fatal("the declared total was not verified")
			}
			if want := int64(len(raw)) + maxPageSize + 64<<10; cs.Bytes > want {
				t.Errorf("opening a %d-byte stream read %d bytes, want at most the file plus one tail window (%d)",
					len(raw), cs.Bytes, want)
			}
		})
	}
}
