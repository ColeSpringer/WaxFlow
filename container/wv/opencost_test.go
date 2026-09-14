package wv_test

import (
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

// noiseBlocks is muxBlocks over noise rather than a ramp: the ramp
// compresses to a few kilobytes, and these files have to outgrow a window.
func noiseBlocks(t testing.TB, n int) (container.Track, []container.Packet) {
	t.Helper()
	enc, err := wavpack.NewEncoder(muxFormat, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkts []container.Packet
	emit := func(p codec.Packet) error {
		pkts = append(pkts, container.Packet{Packet: codec.Packet{
			Data: append([]byte(nil), p.Data...), PTS: p.PTS, Dur: p.Dur, Sync: p.Sync,
		}})
		return nil
	}
	r := rand.New(rand.NewPCG(3, 11)) // one seed, so the first block is the same in every file
	buf := audio.Get(muxFormat, wavpack.BlockSamples)
	defer audio.Put(buf)
	for off := 0; off < n; off += wavpack.BlockSamples {
		buf.N = min(wavpack.BlockSamples, n-off)
		for c := range muxFormat.Channels {
			s := buf.ChanI(c)
			for i := range buf.N {
				s[i] = int32(r.IntN(1<<16)) - 1<<15
			}
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.WavPack, CodecConfig: enc.CodecConfig(),
		Fmt: muxFormat, Samples: int64(n), Default: true}
	return track, pkts
}

// TestOpenReadsTheHeadAndOneTailWindow pins what opening a WavPack file
// costs: the magic, the trailer probes, the whole first block (its header
// states the stream) and the one window at the tail the length check scans,
// which is a span by design. The bound is one window plus that head, the
// same for three and six windows of blocks. The first block used to come
// with a window of read-ahead of its own, so an open paid two.
func TestOpenReadsTheHeadAndOneTailWindow(t *testing.T) {
	var costs [2][2]int64
	for i, n := range []int{100000, 200000} {
		track, pkts := noiseBlocks(t, n)
		head := int64(len(pkts[0].Data)) + 1024
		var dst seekBuf
		writeAll(t, &dst, track, pkts, nil)
		raw := dst.Buf
		if len(raw) < 2*srcwin.Chunk {
			t.Fatalf("%d samples wrote %d bytes, want over two windows", n, len(raw))
		}
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := wv.NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d samples (%d bytes): %d bytes in %d reads", n, len(raw), src.Bytes, src.Reads)
		if src.Bytes > srcwin.Chunk+head {
			t.Errorf("opening a %d-byte file read %d bytes, want at most one tail window plus the %d-byte head", len(raw), src.Bytes, head)
		}
		costs[i] = [2]int64{int64(src.Reads), src.Bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening the shorter file cost %v (reads, bytes) and the longer %v; the open must not scale with the file", costs[0], costs[1])
	}
}
