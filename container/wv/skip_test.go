package wv_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// readAll reads d to its end: the packets, their data dropped, and the end.
func readAll(t *testing.T, d *wv.Demuxer) ([]container.Packet, error) {
	t.Helper()
	var pkts []container.Packet
	for {
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			return pkts, err
		}
		pkt.Data = nil
		pkts = append(pkts, pkt)
	}
}

// inOrder fails unless each packet starts where the read had reached, less
// what a hole in front of it took, and returns the samples the packets hold
// and the samples lost.
func inOrder(t *testing.T, pkts []container.Packet) (samples, lost int64) {
	t.Helper()
	for i, p := range pkts {
		if p.PTS-p.Lost != samples+lost {
			t.Fatalf("packet %d at %d owing %d, want it where the read reached, %d", i, p.PTS, p.Lost, samples+lost)
		}
		if (p.Lost > 0) != p.Discont {
			t.Errorf("packet %d owes %d with discont %v", i, p.Lost, p.Discont)
		}
		samples, lost = samples+p.Dur, lost+p.Lost
	}
	return samples, lost
}

// oneDamage fails unless d warned one Damage, at off and naming want.
func oneDamage(t *testing.T, d *wv.Demuxer, off int, want string) {
	t.Helper()
	ws := slices.DeleteFunc(slices.Clone(d.Warnings()), func(w container.Warning) bool { return w.Kind != container.Damage })
	if len(ws) != 1 || ws[0].Offset != int64(off) || !strings.Contains(ws[0].Msg, want) {
		t.Fatalf("damage %+v, want one at %d naming %q", ws, off, want)
	}
}

// strictRefuses fails unless a Strict read of raw stops on an error naming want.
func strictRefuses(t *testing.T, raw []byte, want string) {
	t.Helper()
	d, err := wv.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, d); errors.Is(err, io.EOF) || !strings.Contains(err.Error(), want) {
		t.Fatalf("a Strict read ended with %v, want a refusal naming %q", err, want)
	}
}

// shortFirstBlock is sine-s16.wv with its first block's count cut to n, its
// checksum restated: the second block starts past where the first ends.
func shortFirstBlock(t *testing.T, n uint32) []byte {
	t.Helper()
	raw := append([]byte(nil), fixture(t, "sine-s16.wv")...)
	binary.LittleEndian.PutUint32(raw[20:], n)
	wavpack.UpdateBlockChecksum(raw[:blockStarts(t, raw)[1]])
	return raw
}

// seeksLandWhereTheyRead seeks d to each target and fails unless the packet
// read next starts at the landing, less what it owes.
func seeksLandWhereTheyRead(t *testing.T, d *wv.Demuxer, targets ...int64) {
	t.Helper()
	for _, target := range targets {
		landed, err := d.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("a read after the seek to %d: %v", target, err)
		}
		if landed > target || pkt.PTS-pkt.Lost != landed {
			t.Errorf("seek to %d landed at %d, then read a packet at %d owing %d", target, landed, pkt.PTS, pkt.Lost)
		}
	}
}

// decoded is how many samples a decode of raw through format delivers.
func decoded(t *testing.T, raw []byte) int64 {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	var n int64
	for {
		buf := audio.Get(f, audio.StandardChunk)
		err := med.ReadChunk(buf)
		n += int64(buf.N)
		audio.Put(buf)
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// withIndex is raw with block k's index set to index, its checksum restated.
func withIndex(t *testing.T, raw []byte, k int, index uint32) []byte {
	t.Helper()
	return testutil.WavPackWithIndex(t, raw, k, index)
}

// otherShape is an audio block that is mono and 8-bit, which no stream these
// tests read is, at index and of 3584 samples.
func otherShape(index uint32) []byte { return testutil.WavPackOtherShape(index) }

// without is raw with its blocks ks gone.
func without(t testing.TB, raw []byte, ks ...int) []byte {
	t.Helper()
	return testutil.WavPackWithout(t, raw, ks...)
}

// open is a tolerant demuxer over raw.
func open(t *testing.T, raw []byte) *wv.Demuxer {
	t.Helper()
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// exactAs fails unless d's track says samples exact.
func exactAs(t *testing.T, d *wv.Demuxer, samples int64) {
	t.Helper()
	if tr := d.Tracks()[0]; tr.Samples != samples || !tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d exact", tr.Samples, tr.SamplesExact, samples)
	}
}

// TestASkipTheBlocksBearOutIsAHole: a skip the stream bears out is a hole.
// The packet behind it owes the samples skipped, one warning names them,
// Strict refuses there, and the exact length is what the packets hold and lost.
func TestASkipTheBlocksBearOutIsAHole(t *testing.T) {
	// Two blocks, the second the last: the first one's count shrunk, so the
	// second starts 7168 samples past where the first ends.
	raw := shortFirstBlock(t, 549)
	d := open(t, raw)
	pkts, end := readAll(t, d)
	if end != io.EOF || len(pkts) != 2 {
		t.Fatalf("read %d packets, ending %v", len(pkts), end)
	}
	samples, lost := inOrder(t, pkts)
	if lost != 7717-549 {
		t.Errorf("the packets owe %d, want the %d skipped", lost, 7717-549)
	}
	oneDamage(t, d, blockStarts(t, raw)[1], "the blocks skip from sample 549 to 7717: 7168 samples lost")
	exactAs(t, d, samples+lost)
	seeksLandWhereTheyRead(t, d, 100, 1000, 7717, 8217)
	strictRefuses(t, raw, "7168 samples lost")

	// A block gone from between two the read keeps: the silence is decoded.
	track, blocks := muxBlocks(t, 3*wavpack.BlockSamples, -1)
	var dst seekBuf
	writeAll(t, &dst, track, slices.Delete(blocks, 1, 2), nil)
	gone := dst.Buf
	if got := decoded(t, gone); got != 3*wavpack.BlockSamples {
		t.Errorf("a decode delivered %d samples, want the %d stated", got, 3*wavpack.BlockSamples)
	}
}

// TestAHoleBehindJunkIsOneWarning: bytes a resync steps over in front of a
// skip are the same damage, said once.
func TestAHoleBehindJunkIsOneWarning(t *testing.T) {
	raw := append([]byte(nil), fixture(t, "seek-mono.wv")...)
	at := blockStarts(t, raw)[10]
	copy(raw[at:], "xxxx")
	d := open(t, raw)
	pkts, end := readAll(t, d)
	if end != io.EOF || len(pkts) != 19 {
		t.Fatalf("read %d packets, ending %v", len(pkts), end)
	}
	samples, lost := inOrder(t, pkts)
	if lost != 2000 || pkts[10].Lost != 2000 {
		t.Errorf("the packets owe %d, packet 10 %d, want the lost block's 2000", lost, pkts[10].Lost)
	}
	oneDamage(t, d, at, "1462 unparsable bytes between blocks: 2000 samples lost")
	exactAs(t, d, samples+lost)
}

// TestASkipNothingBearsOutIsReadOnInOrder: one block's index raised, the next
// going on from the block before it, is read on in order. All the audio is
// delivered, as before the read looked at the indexes, with one warning.
func TestASkipNothingBearsOutIsReadOnInOrder(t *testing.T) {
	raw := withIndex(t, fixture(t, "seek-mono.wv"), 5, 30000)
	d := open(t, raw)
	pkts, end := readAll(t, d)
	if end != io.EOF || len(pkts) != 20 {
		t.Fatalf("read %d packets, ending %v", len(pkts), end)
	}
	if samples, lost := inOrder(t, pkts); samples != 40000 || lost != 0 {
		t.Errorf("the packets hold %d and owe %d, want 40000 and nothing", samples, lost)
	}
	oneDamage(t, d, blockStarts(t, raw)[5], "the blocks skip from sample 10000 to 30000")
	exactAs(t, d, 40000)
	seeksLandWhereTheyRead(t, d, 5000, 12345, 39999)
	if got := decoded(t, raw); got != 40000 {
		t.Errorf("a decode delivered %d samples, want 40000", got)
	}
	strictRefuses(t, raw, "10000 to 30000")

	// The same on a stream with no length for a decode to be cut at: the
	// block behind the skip does not carry on from it, so nothing is lost.
	bare := append([]byte(nil), raw...)
	if err := wavpack.SetTotalSamples(bare[:blockStarts(t, bare)[1]], -1); err != nil {
		t.Fatal(err)
	}
	d = open(t, append(bare, bytes.Repeat([]byte{'x'}, 64)...))
	if tr := d.Tracks()[0]; tr.Samples != -1 {
		t.Fatalf("the track says %d samples, want none: this pins nothing", tr.Samples)
	}
	pkts, end = readAll(t, d)
	if samples, lost := inOrder(t, pkts); end != io.EOF || samples != 40000 || lost != 0 {
		t.Errorf("with no length the packets hold %d and owe %d, ending %v; want 40000 and nothing", samples, lost, end)
	}
}

// TestAStepBackIsReadOnInOrder: one block's index lowered is placed where the
// read has reached, with one warning and nothing reported lost.
func TestAStepBackIsReadOnInOrder(t *testing.T) {
	raw := withIndex(t, fixture(t, "seek-mono.wv"), 5, 1000)
	d := open(t, raw)
	pkts, end := readAll(t, d)
	if end != io.EOF || len(pkts) != 20 {
		t.Fatalf("read %d packets, ending %v", len(pkts), end)
	}
	if samples, lost := inOrder(t, pkts); samples != 40000 || lost != 0 {
		t.Errorf("the packets hold %d and owe %d, want 40000 and nothing", samples, lost)
	}
	oneDamage(t, d, blockStarts(t, raw)[5], "the blocks step back from sample 10000 to 1000")
	exactAs(t, d, 40000)
	if got := decoded(t, raw); got != 40000 {
		t.Errorf("a decode delivered %d samples, want 40000", got)
	}
	strictRefuses(t, raw, "step back")
}

// TestTwoStreamsEndToEndAreWarnedOnce: a stream behind another steps back
// once and carries on from its own numbering. The open states the first
// one's length at its usual cost; the read delivers both with one warning.
func TestTwoStreamsEndToEndAreWarnedOnce(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	raw := append(append([]byte(nil), mono...), mono...)
	d := open(t, raw)
	exactAs(t, d, 40000)
	pkts, end := readAll(t, d)
	if end != io.EOF || len(pkts) != 40 {
		t.Fatalf("read %d packets, ending %v", len(pkts), end)
	}
	if samples, lost := inOrder(t, pkts); samples != 80000 || lost != 0 {
		t.Errorf("the packets hold %d and owe %d, want both streams' 80000 and nothing", samples, lost)
	}
	oneDamage(t, d, len(mono), "the blocks step back from sample 40000 to 0")
	if got := decoded(t, raw); got != 40000 {
		t.Errorf("a decode delivered %d samples, want the 40000 the open states", got)
	}

	// Two streams over a tail window each: the open reads no more for them.
	noise := noiseFile(t, muxFormat, 100000)
	if len(noise) < 2*srcwin.Chunk {
		t.Fatalf("%d bytes, want over two windows", len(noise))
	}
	src := &testutil.CountingSource{Src: container.BytesSource(append(append([]byte(nil), noise...), noise...))}
	dn, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAs(t, dn, 100000)
	if head := int64(blockStarts(t, noise)[1]) + 1024; src.Bytes > srcwin.Chunk+head {
		t.Errorf("the open read %d bytes, want at most one tail window plus the %d-byte head", src.Bytes, head)
	}
}

// TestATailOfAnotherShapeIsNoLength: a last block the read refuses for its
// shape gives no length. The track says what the read delivers, and a decode
// is not cut short of it.
func TestATailOfAnotherShapeIsNoLength(t *testing.T) {
	track, blocks := muxBlocks(t, 3*wavpack.BlockSamples, -1)
	var dst seekBuf
	writeAll(t, &dst, track, blocks, nil)
	// Mono and 8-bit where the stream is stereo and 16-bit, behind junk the
	// read resyncs over.
	other := make([]byte, 56)
	copy(other, "wvpk")
	binary.LittleEndian.PutUint32(other[4:], uint32(len(other)-8))
	binary.LittleEndian.PutUint16(other[8:], 0x410)
	binary.LittleEndian.PutUint32(other[20:], 3584)
	binary.LittleEndian.PutUint32(other[24:], 4)
	raw := append(append(append([]byte(nil), dst.Buf...), bytes.Repeat([]byte{'x'}, 64)...), other...)

	d := open(t, raw)
	if _, ok := d.ScanTailForTest(); ok {
		t.Error("the tail scan took a block of another shape")
	}
	if tr := d.Tracks()[0]; tr.Samples != 3*wavpack.BlockSamples {
		t.Errorf("the track says %d samples, want the %d the read delivers", tr.Samples, 3*wavpack.BlockSamples)
	}
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 3*wavpack.BlockSamples {
		t.Fatalf("read %d samples, ending %v", samples, end)
	}
	if got := decoded(t, raw); got != 3*wavpack.BlockSamples {
		t.Errorf("a decode delivered %d samples, want %d", got, 3*wavpack.BlockSamples)
	}
}

// realThroughFormat is what a decode of raw through format delivers: the real
// samples and the silence it filled holes with.
func realThroughFormat(t *testing.T, raw []byte) (real, lost int64) {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	var n int64
	for err == nil {
		buf := audio.Get(f, audio.StandardChunk)
		err = med.ReadChunk(buf)
		n += int64(buf.N)
		audio.Put(buf)
	}
	if err != io.EOF {
		t.Fatalf("a decode ended %v", err)
	}
	lost = med.Info().Lost
	return n - lost, lost
}

// TestCompoundDamageDeliversEveryRealSample: a skip is a hole only where the
// blocks after it keep the read within the exact length, so no damage makes
// a decode deliver less of the stream's own audio than reading it in order.
func TestCompoundDamageDeliversEveryRealSample(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	raised := func(by uint32) []byte {
		raw := mono
		for _, k := range []int{5, 6} {
			raw = withIndex(t, raw, k, uint32(2000*k)+by)
		}
		return raw
	}
	starts := blockStarts(t, mono)
	// Block 5 gone, and block 8 there twice: one block less, one more.
	gone := append(append(append([]byte(nil), mono[:starts[5]]...), mono[starts[6]:starts[9]]...), mono[starts[8]:]...)
	// Blocks 5 and 12 gone: two holes the blocks and the length bear out.
	two := append(append(append([]byte(nil), mono[:starts[5]]...), mono[starts[6]:starts[12]]...), mono[starts[13]:]...)
	for _, tc := range []struct {
		name          string
		raw           []byte
		samples, lost int64
	}{
		{"blocks 5 and 6 raised by 7777", raised(7777), 40000, 0},
		{"blocks 5 and 6 raised by 1000000", raised(1000000), 40000, 0},
		{"block 5 removed and block 8 repeated", gone, 40000, 0},
		{"blocks 5 and 12 removed", two, 36000, 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if real, lost := realThroughFormat(t, tc.raw); real != tc.samples || lost != tc.lost {
				t.Errorf("a decode delivered %d real samples and %d of silence, want %d and %d", real, lost, tc.samples, tc.lost)
			}
			d := open(t, tc.raw)
			pkts, end := readAll(t, d)
			if samples, lost := inOrder(t, pkts); end != io.EOF || samples != tc.samples || lost != tc.lost {
				t.Errorf("the read delivered %d samples owing %d, ending %v, want %d owing %d", samples, lost, end, tc.samples, tc.lost)
			}
		})
	}
}

// TestASkipPastWhatTheBytesHoldIsReadOn: a skip to an index the bytes in front
// of the block's end could not hold is read on in order, and an open's tail
// length past what the file's bytes hold is not exact.
func TestASkipPastWhatTheBytesHoldIsReadOn(t *testing.T) {
	t.Run("an index past the file", func(t *testing.T) {
		raw := append([]byte(nil), fixture(t, "sine-s16.wv")...)
		at := blockStarts(t, raw)[1]
		raw[at+10] = 0x80 // the index's fifth byte: 1<<39
		binary.LittleEndian.PutUint32(raw[at+16:], 0)
		d := open(t, raw)
		if tr := d.Tracks()[0]; tr.SamplesExact {
			t.Fatalf("the track says %d samples exact from a tail past what the bytes hold", tr.Samples)
		}
		pkts, end := readAll(t, d)
		if samples, lost := inOrder(t, pkts); end != io.EOF || samples != 15435 || lost != 0 {
			t.Fatalf("the read delivered %d samples owing %d, ending %v, want 15435 owing none", samples, lost, end)
		}
		oneDamage(t, d, at, "which nothing bears out")
		if real, lost := realThroughFormat(t, raw); real != 15435 || lost != 0 {
			t.Errorf("a decode delivered %d real samples and %d of silence, want 15435 and none", real, lost)
		}
	})
	t.Run("an index past what the bytes in front of it hold", func(t *testing.T) {
		raw := fixture(t, "seek-mono.wv")
		for k := 1; k < 20; k++ {
			raw = withIndex(t, raw, k, uint32(2000*k+50000000))
		}
		pkts, end := readAll(t, open(t, raw))
		if samples, lost := inOrder(t, pkts); end != io.EOF || samples != 40000 || lost != 0 {
			t.Fatalf("the read delivered %d samples owing %d, ending %v, want 40000 owing none", samples, lost, end)
		}
	})
}

// TestAStrayBlockIsSteppedOver: a block of another shape whose index does not
// carry on from the stream is not the stream's. A read steps over it with one
// finding, wherever it lies, and a length with one behind it is not exact.
func TestAStrayBlockIsSteppedOver(t *testing.T) {
	track, blocks := muxBlocks(t, 3*wavpack.BlockSamples, -1)
	var dst seekBuf
	writeAll(t, &dst, track, blocks, nil)

	raw := append(append([]byte(nil), dst.Buf...), otherShape(0)...)
	d := open(t, raw)
	if tr := d.Tracks()[0]; tr.Samples != 3*wavpack.BlockSamples || tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d and not exact", tr.Samples, tr.SamplesExact, 3*wavpack.BlockSamples)
	}
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 3*wavpack.BlockSamples {
		t.Fatalf("read %d samples, ending %v", samples, end)
	}
	oneDamage(t, d, len(dst.Buf), "56 bytes of blocks of another shape are not this stream's, stepped over")
	if real, _ := realThroughFormat(t, raw); real != 3*wavpack.BlockSamples {
		t.Errorf("a decode delivered %d samples, want %d", real, 3*wavpack.BlockSamples)
	}
	strictRefuses(t, raw, "blocks of another shape are not this stream's")

	// Two of them between two of the stream's blocks: the read goes on behind
	// them, where it once failed, and says so once.
	starts := blockStarts(t, dst.Buf)
	mid := spliced(dst.Buf, starts[1], append(otherShape(0), otherShape(3584)...))
	d = open(t, mid)
	pkts, end = readAll(t, d)
	if samples, lost := inOrder(t, pkts); end != io.EOF || samples != 3*wavpack.BlockSamples || lost != 0 {
		t.Fatalf("read %d samples owing %d over stray blocks mid-stream, ending %v", samples, lost, end)
	}
	oneDamage(t, d, starts[1], "112 bytes of blocks of another shape are not this stream's, stepped over")
	if real, _ := realThroughFormat(t, mid); real != 3*wavpack.BlockSamples {
		t.Errorf("a decode delivered %d samples, want %d", real, 3*wavpack.BlockSamples)
	}
	strictRefuses(t, mid, "blocks of another shape are not this stream's")

	// Junk behind the stream, so the open walks to its length: the walk
	// steps over them too, and finds no shortfall.
	d = open(t, append(append([]byte(nil), mid...), make([]byte, 64)...))
	if tr := d.Tracks()[0]; tr.Samples != 3*wavpack.BlockSamples || tr.SamplesExact || len(d.Warnings()) != 0 {
		t.Errorf("the track says %d samples, exact %v, warning %+v; want %d, not exact, none",
			tr.Samples, tr.SamplesExact, d.Warnings(), 3*wavpack.BlockSamples)
	}
}

// TestAChangeOfShapeIsAFormatChange: a block of another shape that carries
// on from the stream is the change of shape this pipeline refuses, through
// the demuxer and through a decode, which delivers the audio in front first.
func TestAChangeOfShapeIsAFormatChange(t *testing.T) {
	const three = 3 * wavpack.BlockSamples
	stereo := noiseFile(t, muxFormat, three)
	starts := blockStarts(t, stereo)
	mono := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}
	_, pkts := noiseBlocksIn(t, mono, three)
	// Three stereo blocks, then three mono ones numbered on from them.
	carried := append([]byte(nil), stereo[:starts[3]]...)
	for i, p := range pkts {
		b := append([]byte(nil), p.Data...)
		binary.LittleEndian.PutUint32(b[16:], uint32(three+i*wavpack.BlockSamples))
		wavpack.UpdateBlockChecksum(b)
		carried = append(carried, b...)
	}
	// The same with the header's total counting all six.
	counted := append([]byte(nil), carried...)
	if err := wavpack.SetTotalSamples(counted[:starts[1]], 2*three); err != nil {
		t.Fatal(err)
	}
	// One block of another shape numbered on from the first, mid-stream.
	mid := spliced(stereo, starts[1], otherShape(wavpack.BlockSamples))
	for _, tc := range []struct {
		name    string
		raw     []byte
		samples int64 // what the read delivers in front of the change
	}{
		{"mono blocks that carry on at the stated length", carried, three},
		{"mono blocks that carry on short of the stated length", counted, three},
		{"a block that carries on mid-stream", mid, wavpack.BlockSamples},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := open(t, tc.raw)
			pkts, end := readAll(t, d)
			if samples, _ := inOrder(t, pkts); samples != tc.samples || end == nil || !strings.Contains(end.Error(), "mid-stream format change") {
				t.Fatalf("read %d samples, ending %v; want %d and the format change", samples, end, tc.samples)
			}
			if ws := d.Warnings(); slices.ContainsFunc(ws, func(w container.Warning) bool { return strings.Contains(w.Msg, "dropped") }) {
				t.Errorf("the read dropped the change as damage: %v", ws)
			}
			med, err := format.Open(container.BytesSource(tc.raw), "wavpack", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			var n int64
			for err == nil {
				buf := audio.Get(muxFormat, audio.StandardChunk)
				if err = med.ReadChunk(buf); err == nil {
					n += int64(buf.N)
				}
				audio.Put(buf)
			}
			// A decode fails in the chunk the change falls in.
			if !strings.Contains(err.Error(), "mid-stream format change") || n > tc.samples || n+audio.StandardChunk <= tc.samples {
				t.Errorf("a decode delivered %d samples and ended %v; want up to %d and the format change", n, err, tc.samples)
			}
		})
	}
}

// decodeFrom seeks a decode of raw to target and returns where it landed and
// how many samples it then delivered.
func decodeFrom(t *testing.T, raw []byte, target int64) (pos, n int64) {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if pos, err = med.SeekSample(context.Background(), target); err != nil {
		t.Fatalf("a seek to %d: %v", target, err)
	}
	return pos, decodeRest(t, med)
}

// TestASeekStepsOverABlockOfAnotherShape: a seek's walk never lands on a
// block of another shape and never stops at one, so a seek past it reaches
// the stream's blocks behind it, where a read from the start cannot.
func TestASeekStepsOverABlockOfAnotherShape(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	// Behind block 10, short of the stated length: a read fails there.
	raw := spliced(mono, starts[11], otherShape(0))
	for _, tc := range [][3]int64{{22500, 22000, 17500}, {30000, 30000, 10000}, {39000, 38000, 1000}} {
		d := open(t, raw)
		landed, err := d.SeekSample(context.Background(), 0, tc[0])
		if err != nil || landed != tc[1] {
			t.Errorf("a seek to %d landed at %d (%v), want %d", tc[0], landed, err, tc[1])
		}
		if pos, n := decodeFrom(t, raw, tc[0]); pos != tc[0] || n != tc[2] {
			t.Errorf("a decode from a seek to %d started at %d and delivered %d samples, want %d", tc[0], pos, n, tc[2])
		}
	}
	// A target the block's own index would hold lands on the stream's last
	// block, where a seek past the end lands.
	far := open(t, spliced(mono, starts[11], otherShape(50000)))
	if landed, err := far.SeekSample(context.Background(), 0, 51000); err != nil || landed != 38000 {
		t.Errorf("a seek to 51000 landed at %d (%v), want 38000", landed, err)
	}

	// One that carries on from the stream is where a read fails, and a seek
	// past it still lands on the stream's blocks behind it.
	change := spliced(mono, starts[10], otherShape(20000))
	if landed, err := open(t, change).SeekSample(context.Background(), 0, 30000); err != nil || landed != 30000 {
		t.Errorf("a seek to 30000 past a change of shape landed at %d (%v), want 30000", landed, err)
	}
	for _, target := range []int64{21000, 30000} { // the first inside the block's own indexes
		if pos, n := decodeFrom(t, change, target); pos != target || n != 40000-target {
			t.Errorf("a decode from a seek to %d past a change of shape started at %d and delivered %d samples, want %d", target, pos, n, 40000-target)
		}
	}

	// The first block twenty times, a stray block, the rest: a read steps
	// over it as a seek does, and delivers the blocks behind it.
	var again []byte
	for range 20 {
		again = append(again, mono[:starts[1]]...)
	}
	again = append(append(again, otherShape(0)...), mono[starts[1]:]...)
	d := open(t, again)
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 78000 {
		t.Fatalf("read %d samples, ending %v, want all 78000 the blocks hold", samples, end)
	}
	if pos, n := decodeFrom(t, again, 30000); pos != 30000 || n != 10000 {
		t.Errorf("a decode from a seek to 30000 started at %d and delivered %d samples, want 10000", pos, n)
	}
}

// TestASkipInFrontOfABlockOfAnotherShapeIsAHole: where the read ends at the
// next audio block, a block of another shape, the block a skip lands on is
// the stream's last and nothing gainsays it.
func TestASkipInFrontOfABlockOfAnotherShapeIsAHole(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	// Block 18 gone, and a block of another shape behind block 19.
	raw := spliced(without(t, mono, 18), starts[20]-(starts[19]-starts[18]), otherShape(0))
	d := open(t, raw)
	pkts, end := readAll(t, d)
	samples, lost := inOrder(t, pkts)
	if end != io.EOF || samples != 38000 || lost != 2000 || pkts[18].Lost != 2000 {
		t.Fatalf("the read delivered %d samples owing %d, ending %v; want 38000 owing the 2000 in front of the last block", samples, lost, end)
	}
}

// TestALengthBehindABlockOfAnotherShapeIsNotExact: a block of another shape
// at the end gives the length, as it once did, where the bytes could hold
// it; not exact, so a decode is cut no shorter than it was.
func TestALengthBehindABlockOfAnotherShapeIsNotExact(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	// Two streams end to end, and a block far up the numbering behind them.
	raw := append(append(append(append([]byte(nil), mono[:starts[20]]...), mono[:starts[20]]...), otherShape(1000000)...), mono[starts[20]:]...)
	d := open(t, raw)
	if tr := d.Tracks()[0]; tr.Samples != 1003584 || tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want 1003584 and not exact", tr.Samples, tr.SamplesExact)
	}
	if real, lost := realThroughFormat(t, raw); real != 80000 || lost != 0 {
		t.Errorf("a decode delivered %d real samples and %d of silence, want both streams' 80000", real, lost)
	}
}

// TestAnExactLengthRestsOnIndexesTheOpenChecked: a tail the total does not
// bear out is exact only if the block in front carries on to it, and with no
// total the block behind the first must too. One bad index cuts no decode.
func TestAnExactLengthRestsOnIndexesTheOpenChecked(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	swapped := append(append(append([]byte(nil), mono[starts[1]:starts[2]]...), mono[:starts[1]]...), mono[starts[2]:]...)
	// Every block raised alike: a stream carved out of a longer one.
	carved := mono
	for k := range 20 {
		carved = withIndex(t, carved, k, uint32(2000*k+100000))
	}
	nototal := append([]byte(nil), mono...)
	if err := wavpack.SetTotalSamples(nototal[:starts[1]], -1); err != nil {
		t.Fatal(err)
	}
	junked := spliced(nototal, starts[19], bytes.Repeat([]byte{0x77, 0x11}, 50))
	for _, tc := range []struct {
		name    string
		raw     []byte
		samples int64 // what the track states
		exact   bool
		lost    int64 // the silence a decode fills a hole with
		note    string
	}{
		{"the first index just under the tail's end", withIndex(t, mono, 0, 39999), -1, false, 0, ""},
		{"the first index raised by one", withIndex(t, mono, 0, 1), -1, false, 0, ""},
		{"the first two blocks swapped", swapped, -1, false, 0, ""},
		{"the last index zeroed", withIndex(t, mono, 19, 0), 40000, false, 0, ""},
		{"the last index lowered", withIndex(t, mono, 19, 28000), 40000, false, 0, ""},
		// A last block past a skip is a hole, and nothing is cut for it; the
		// tail past the total is the length the open once stated.
		{"the last index raised", withIndex(t, mono, 19, 48000), 50000, false, 10000,
			"the header declares 40000 samples but the blocks run to 50000"},
		{"no total and the last index lowered", withIndex(t, nototal, 19, 28000), -1, false, 0, ""},
		{"a carved stream", carved, 40000, true, 0, ""},
		{"no total", nototal, 40000, true, 0, ""},
		{"no total and junk in front of the tail", junked, 40000, true, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := open(t, tc.raw)
			if tr := d.Tracks()[0]; tr.Samples != tc.samples || tr.SamplesExact != tc.exact {
				t.Errorf("the track says %d samples, exact %v, want %d, exact %v", tr.Samples, tr.SamplesExact, tc.samples, tc.exact)
			}
			if w := d.Warnings(); tc.note == "" && len(w) != 0 || tc.note != "" && (len(w) != 1 || w[0].Kind != container.Note || w[0].Msg != tc.note) {
				t.Errorf("the open warned %v, want %q alone as a note", w, tc.note)
			}
			if real, lost := realThroughFormat(t, tc.raw); real != 40000 || lost != tc.lost {
				t.Errorf("a decode delivered %d real samples and %d of silence, want all 40000 and %d", real, lost, tc.lost)
			}
		})
	}
	// A total the tail falls short of, with the block in front bearing the
	// tail out, is the cut file it always was.
	cut := without(t, mono, 19)
	d := open(t, cut)
	exactAs(t, d, 38000)
	if got := container.TruncatedOf(d, 0); got != 2000 {
		t.Errorf("%d samples counted truncated, want 2000", got)
	}
}

// TestACutFileWarnsWhateverBearsItsTailOut: a tail short of the header's
// total is told as the shortfall it is, and counted, where the block in front
// does not bear the tail's index out; the length is then not exact.
func TestACutFileWarnsWhateverBearsItsTailOut(t *testing.T) {
	raw := withIndex(t, without(t, fixture(t, "seek-mono.wv"), 18, 19), 17, 34001)
	d := open(t, raw)
	if tr := d.Tracks()[0]; tr.Samples != 36001 || tr.SamplesExact || container.TruncatedOf(d, 0) != 3999 {
		t.Errorf("the track says %d samples, exact %v, %d truncated; want 36001, not exact, 3999",
			tr.Samples, tr.SamplesExact, container.TruncatedOf(d, 0))
	}
	oneDamage(t, d, 0, "the header declares 40000 samples but the blocks end at 36001")
	// A tail past the total that nothing bears out is the length the open
	// once stated, not exact, and the overrun a note.
	d = open(t, withIndex(t, fixture(t, "seek-mono.wv"), 19, 38000+7777))
	if tr := d.Tracks()[0]; tr.Samples != 47777 || tr.SamplesExact || d.DamageCount() != 0 || container.TruncatedOf(d, 0) != 0 {
		t.Errorf("the track says %d samples, exact %v, %d truncated, warning %+v; want 47777, not exact, 0, no damage",
			tr.Samples, tr.SamplesExact, container.TruncatedOf(d, 0), d.Warnings())
	}
}

// TestABlockInFrontOfTheTailIsOneAReadStepsTo: bytes that spell a block
// inside the payload in front of the tail are not the block in front, so
// they take no exact length away.
func TestABlockInFrontOfTheTailIsOneAReadStepsTo(t *testing.T) {
	raw := append([]byte(nil), fixture(t, "seek-mono.wv")...)
	starts := blockStarts(t, raw)
	if err := wavpack.SetTotalSamples(raw[:starts[1]], -1); err != nil {
		t.Fatal(err)
	}
	decoy := raw[starts[19]-200:]
	copy(decoy, raw[:wavpack.BlockHeaderLen])
	binary.LittleEndian.PutUint32(decoy[4:], 32) // a block of 40 bytes
	binary.LittleEndian.PutUint32(decoy[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(decoy[16:], 5000)
	exactAs(t, open(t, raw), 40000)
}

// TestAStrictRefusalAtAHoleRefusesAgain: a Strict read that refuses at a hole
// stays where it was, so the next call refuses too.
func TestAStrictRefusalAtAHoleRefusesAgain(t *testing.T) {
	raw := append([]byte(nil), fixture(t, "seek-mono.wv")...)
	copy(raw[blockStarts(t, raw)[10]:], "xxxx")
	d, err := wv.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	pkts, first := readAll(t, d)
	var pkt container.Packet
	if second := d.ReadPacket(&pkt); first == io.EOF || second == nil || second.Error() != first.Error() {
		t.Fatalf("after %d packets a Strict read refused with %v, and the next call answered %v (packet at %d)",
			len(pkts), first, second, pkt.PTS)
	}
}

// TestASeekCanceledInItsWalkLandsWhenAskedAgain: a seek whose walk of the
// headers meets a context that has ended stops with CodeCanceled, and the
// same seek under a live one lands and reads where an uncanceled one does.
func TestASeekCanceledInItsWalkLandsWhenAskedAgain(t *testing.T) {
	raw := without(t, fixture(t, "seek-mono.wv"), 10)
	ref := open(t, raw)
	want, err := ref.SeekSample(context.Background(), 0, 30000)
	if err != nil {
		t.Fatal(err)
	}
	d := open(t, raw)
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 3), 0, 30000); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose walk was canceled answered %v, want CodeCanceled", err)
	}
	got, err := d.SeekSample(context.Background(), 0, 30000)
	if err != nil || got != want {
		t.Fatalf("the seek asked again landed at %d (%v), an uncanceled one at %d", got, err, want)
	}
	a, aEnd := readAll(t, d)
	b, bEnd := readAll(t, ref)
	if !slices.EqualFunc(a, b, func(x, y container.Packet) bool { return x.PTS == y.PTS && x.Lost == y.Lost }) || aEnd != bEnd {
		t.Errorf("the read behind it gave %d packets ending %v, an uncanceled one's %d ending %v", len(a), aEnd, len(b), bEnd)
	}
}

// TestAFirstIndexPastTheTailGivesNoLength: a stream whose first block's index
// is past where its last one ends states no length, where it once stated a
// negative one exact.
func TestAFirstIndexPastTheTailGivesNoLength(t *testing.T) {
	d := open(t, withIndex(t, fixture(t, "seek-mono.wv"), 0, 100000))
	if tr := d.Tracks()[0]; tr.SamplesExact || tr.Samples < -1 {
		t.Fatalf("the track says %d samples, exact %v, want no exact length", tr.Samples, tr.SamplesExact)
	}
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 40000 {
		t.Fatalf("the read delivered %d samples, ending %v", samples, end)
	}
	// The first two raised alike, so the block beside each end bears it out
	// and only the sign is left to refuse.
	two := withIndex(t, withIndex(t, fixture(t, "seek-mono.wv"), 0, 100000), 1, 102000)
	if tr := open(t, two).Tracks()[0]; tr.SamplesExact || tr.Samples < -1 {
		t.Fatalf("the track says %d samples, exact %v, want no exact length", tr.Samples, tr.SamplesExact)
	}
}

// TestTheOpensWalkEndsAtABlockOfAnotherShape: where the tail scan declines
// and the open walks, a stray block leaves the length the stream's. A change
// of shape, which a seek reaches past, leaves it the measure's.
func TestTheOpensWalkEndsAtABlockOfAnotherShape(t *testing.T) {
	track, blocks := muxBlocks(t, 3*wavpack.BlockSamples, -1)
	var dst seekBuf
	writeAll(t, &dst, track, blocks, nil)
	other := make([]byte, 56)
	copy(other, "wvpk")
	binary.LittleEndian.PutUint32(other[4:], uint32(len(other)-8))
	binary.LittleEndian.PutUint16(other[8:], 0x410)
	binary.LittleEndian.PutUint32(other[20:], 3584)
	binary.LittleEndian.PutUint32(other[24:], 4)
	// Junk behind it, so no block tiles to the end and the open walks.
	raw := append(append(append([]byte(nil), dst.Buf...), other...), bytes.Repeat([]byte{'x'}, 64)...)
	d := open(t, raw)
	if _, ok := d.ScanTailForTest(); ok {
		t.Fatal("the tail scan took a length; the case needs the walk")
	}
	if tr := d.Tracks()[0]; tr.Samples != 3*wavpack.BlockSamples || tr.SamplesExact {
		t.Errorf("the track says %d samples, exact %v, want the stream's %d and not exact", tr.Samples, tr.SamplesExact, 3*wavpack.BlockSamples)
	}
	// One that carries on from the stream, with nothing damaged behind it:
	// more sample-free blocks than the tail scan hops, so the open walks.
	free := append([]byte(nil), dst.Buf[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(free[4:], wavpack.BlockHeaderLen-8)
	binary.LittleEndian.PutUint32(free[20:], 0)
	raw = append(append(append([]byte(nil), dst.Buf...), otherShape(3*wavpack.BlockSamples)...), bytes.Repeat(free, 20)...)
	d = open(t, raw)
	if _, ok := d.ScanTailForTest(); ok {
		t.Fatal("the tail scan took a length; the case needs the walk")
	}
	want := int64(3*wavpack.BlockSamples + 3584) // the change's own end
	if tr := d.Tracks()[0]; tr.Samples != want || tr.SamplesExact || d.DamageCount() != 0 {
		t.Errorf("the track says %d samples, exact %v, %d findings; want %d, not exact, none", tr.Samples, tr.SamplesExact, d.DamageCount(), want)
	}
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != want {
		t.Errorf("measured %d (%v), want the %d stated", n, err, want)
	}
	// One mid-stream: a seek reaches the stream behind the change.
	mono := fixture(t, "seek-mono.wv")
	at := blockStarts(t, mono)[10]
	d = open(t, slices.Concat(mono[:at], otherShape(20000), mono[at:], make([]byte, 64)))
	if tr := d.Tracks()[0]; tr.Samples != 40000 || tr.SamplesExact || d.DamageCount() != 0 {
		t.Errorf("the track says %d samples, exact %v, %d findings; want 40000, not exact, none", tr.Samples, tr.SamplesExact, d.DamageCount())
	}
}

// TestAMeasuringWalkIsUnsureOfIndexesThatDoNotCarryOn: where the open walks
// to its length, a block whose index does not carry on leaves the length it
// finds not exact, as bytes it steps over do.
func TestAMeasuringWalkIsUnsureOfIndexesThatDoNotCarryOn(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	// More sample-free blocks behind the last than the tail scan hops.
	free := append([]byte(nil), mono[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(free[4:], wavpack.BlockHeaderLen-8)
	binary.LittleEndian.PutUint32(free[20:], 0)
	long := append(append([]byte(nil), mono...), bytes.Repeat(free, 20)...)
	d := open(t, long)
	if _, ok := d.ScanTailForTest(); ok {
		t.Fatal("the tail scan took a length; the case needs the walk")
	}
	exactAs(t, d, 40000)
	if tr := open(t, withIndex(t, long, 5, 13000)).Tracks()[0]; tr.Samples != 40000 || tr.SamplesExact {
		t.Errorf("the track says %d samples, exact %v, want 40000 and not exact", tr.Samples, tr.SamplesExact)
	}
}

// TestTrackNamesTheDemuxerRevision: the track carries DemuxerVersion, which
// is what puts this demuxer's revision in a cache key.
func TestTrackNamesTheDemuxerRevision(t *testing.T) {
	d := open(t, fixture(t, "sine-s16.wv"))
	if got := d.Tracks()[0].DemuxerVersion; got != wv.DemuxerVersion || got == "" {
		t.Fatalf("the track names demuxer revision %q, want %q", got, wv.DemuxerVersion)
	}
}
