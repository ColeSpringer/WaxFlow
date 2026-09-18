package flacn_test

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpenReadsTheHeadAndOneTailWindow pins what opening a FLAC stream
// costs: the metadata, the first frame header, and the one window at the
// tail the cheap length confirmation scans backward through, which is a span
// by design. The bound is one window plus the head, and it is the same for a
// stream of three windows and one of six.
func TestOpenReadsTheHeadAndOneTailWindow(t *testing.T) {
	var costs [2][2]int64
	for i, n := range []int{100000, 200000} {
		src := audio.Get(muxFmt(44100, 2, 16), n)
		defer audio.Put(src)
		src.N = n
		r := rand.New(rand.NewPCG(uint64(n), 7))
		for c := range src.Fmt.Channels {
			s := src.ChanI(c)
			for j := range n {
				s[j] = int32(r.IntN(1<<16)) - 1<<15
			}
		}
		var buf bytes.Buffer
		encodeStream(t, src, 0, &buf, int64(n))
		raw := buf.Bytes()
		if len(raw) < 2*srcwin.Chunk {
			t.Fatalf("%d samples wrote %d bytes, want over two windows", n, len(raw))
		}
		cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := flacn.NewDemuxer(cs, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d samples (%d bytes): %d bytes in %d reads", n, len(raw), cs.Bytes, cs.Reads)
		if cs.Bytes > srcwin.Chunk+8192 {
			t.Errorf("opening a %d-byte stream read %d bytes, want at most one tail window plus the head", len(raw), cs.Bytes)
		}
		costs[i] = [2]int64{int64(cs.Reads), cs.Bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening the shorter stream cost %v (reads, bytes) and the longer %v; the open must not scale with the stream", costs[0], costs[1])
	}
}

// TestZeroTotalSettlesFromTheTail is the streaming encoder's file: STREAMINFO
// total 0, which is FLAC's "unknown", written by an encode whose length was
// not known up front. The closing frame answers it, so the open reports a real
// length instead of -1, and it costs the same one tail window an intact file's
// confirmation pays.
//
// The bound is what makes the cell worth writing: settling a total-less file by
// walking its frames would be correct and would cost the whole payload.
func TestZeroTotalSettlesFromTheTail(t *testing.T) {
	const n = 200000
	src := audio.Get(muxFmt(44100, 2, 16), n)
	defer audio.Put(src)
	src.N = n
	r := rand.New(rand.NewPCG(uint64(n), 7))
	for c := range src.Fmt.Channels {
		s := src.ChanI(c)
		for j := range n {
			s[j] = int32(r.IntN(1<<16)) - 1<<15
		}
	}
	var buf bytes.Buffer
	encodeStream(t, src, 0, &buf, -1) // unknown: the muxer writes total 0
	raw := buf.Bytes()
	if len(raw) < 2*srcwin.Chunk {
		t.Fatalf("%d samples wrote %d bytes, want over two windows", n, len(raw))
	}

	cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := flacn.NewDemuxer(cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d bytes: %d bytes in %d reads", len(raw), cs.Bytes, cs.Reads)
	tr := d.Tracks()[0]
	if tr.Samples != n || !tr.SamplesExact {
		t.Errorf("samples = %d (exact %v), want %d exact off the closing frame",
			tr.Samples, tr.SamplesExact, n)
	}
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("a total of 0 is not a claim, so nothing disagrees with it: %v", w)
	}
	if cs.Bytes > srcwin.Chunk+8192 {
		t.Errorf("opening a %d-byte total-less stream read %d bytes, want one tail window plus the head",
			len(raw), cs.Bytes)
	}
}
