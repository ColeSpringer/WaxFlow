package wv_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/format"
)

// overrunTail is three muxed blocks whose middle one states 8 bytes more than
// it holds, so it ends inside the last block's header: the tail scan finds
// the last block, and a read never reaches it.
func overrunTail(t *testing.T) []byte {
	t.Helper()
	track, blocks := muxBlocks(t, 3*wavpack.BlockSamples, -1)
	var dst seekBuf
	writeAll(t, &dst, track, blocks, nil)
	raw := dst.Buf
	at := blockStarts(t, raw)[1]
	binary.LittleEndian.PutUint32(raw[at+4:], binary.LittleEndian.Uint32(raw[at+4:])+8)
	return raw
}

// settled fails unless d's track says samples and no longer calls them
// exact, counts truncated past them, and lists one Damage naming want at
// offset 0.
func settled(t *testing.T, d *wv.Demuxer, samples, truncated int64, want string) {
	t.Helper()
	if tr := d.Tracks()[0]; tr.Samples != samples || tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d and not exact", tr.Samples, tr.SamplesExact, samples)
	}
	if got := container.TruncatedOf(d, 0); got != truncated {
		t.Errorf("%d samples counted truncated, want %d", got, truncated)
	}
	n := 0
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, want) {
			n++
			if w.Kind != container.Damage || w.Offset != 0 {
				t.Errorf("the shortfall is %+v, want Damage at 0", w)
			}
		}
	}
	if n != 1 {
		t.Errorf("warnings %+v, want one naming %q", d.Warnings(), want)
	}
}

// TestAReadEndingShortOfTheLengthSettlesIt: a read that ends short of the
// exact length says so in the open's words, once, takes what the packets
// hold and counts the rest truncated. The length is no longer exact.
func TestAReadEndingShortOfTheLengthSettlesIt(t *testing.T) {
	raw := overrunTail(t)
	d := open(t, raw)
	exactAs(t, d, 3*wavpack.BlockSamples)
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 2*wavpack.BlockSamples {
		t.Fatalf("read %d samples, ending %v", samples, end)
	}
	const want = "the header declares 24576 samples but the blocks end at 16384"
	settled(t, d, 2*wavpack.BlockSamples, wavpack.BlockSamples, want)
	if _, end := readAll(t, d); end != io.EOF {
		t.Fatalf("a read past the end returned %v", end)
	}
	settled(t, d, 2*wavpack.BlockSamples, wavpack.BlockSamples, want)
	// A Strict read refuses sooner, at the bytes the read drops.
	strictRefuses(t, raw, "trailing bytes are not a block")
}

// TestASeekThenAReadToTheEndSettlesTheSame: where the stream ends does not
// depend on where the read began.
func TestASeekThenAReadToTheEndSettlesTheSame(t *testing.T) {
	d := open(t, overrunTail(t))
	if _, err := d.SeekSample(context.Background(), 0, 10000); err != nil {
		t.Fatal(err)
	}
	if _, end := readAll(t, d); end != io.EOF {
		t.Fatalf("the read ended %v", end)
	}
	settled(t, d, 2*wavpack.BlockSamples, wavpack.BlockSamples, "the header declares 24576 samples but the blocks end at 16384")
}

// TestAReadPastTheResyncBoundSettlesTheLength: a block past what a resync
// scans for is one no read reaches, so the read's end settles the length.
func TestAReadPastTheResyncBoundSettlesTheLength(t *testing.T) {
	known := fixture(t, "golden-known.wv")
	raw := spliced(known, blockStarts(t, known)[1], bytes.Repeat([]byte{0x11}, wv.MaxResyncForTest))
	d := open(t, raw)
	exactAs(t, d, 9000)
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 8192 {
		t.Fatalf("read %d samples, ending %v", samples, end)
	}
	settled(t, d, 8192, 808, "the header declares 9000 samples but the blocks end at 8192")
	strictRefuses(t, raw, "trailing bytes are not a block")

	// A Media's live counts take it from the read that found it.
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if got := med.Info().Truncated; got != 0 {
		t.Fatalf("%d truncated at open, want none", got)
	}
	f := med.Info().Default().Fmt
	var n int64
	for err == nil {
		buf := audio.Get(f, audio.StandardChunk)
		err = med.ReadChunk(buf)
		n += int64(buf.N)
		audio.Put(buf)
	}
	if info := med.Info(); err != io.EOF || n != 8192 || info.Truncated != 808 || info.Damage != 2 {
		t.Errorf("a decode delivered %d samples, ending %v, with %d truncated and %d damage, want 8192, io.EOF, 808 and 2",
			n, err, info.Truncated, info.Damage)
	}
	// And Walk, which takes a measurement at no cost, takes what the read
	// left; a measure still answers the length a seek reaches.
	if err := format.WalkMedia(context.Background(), med); err != nil {
		t.Fatal(err)
	}
	if tr := med.Info().Default(); tr.Samples != 8192 || tr.SamplesExact {
		t.Errorf("after the read and a walk the Media says %d samples, exact %v, want 8192 and not exact", tr.Samples, tr.SamplesExact)
	}
	if n, ok, err := format.MeasureMedia(context.Background(), med); err != nil || !ok || n != 9000 {
		t.Errorf("a measure after the read answered %d (%v, %v), want the 9000 the open called exact", n, ok, err)
	}
}

// pastBound is a stream over many seek windows with junk past what a resync
// crosses in front of block 40: a read stops there, and a bisection lands
// behind it.
func pastBound(t *testing.T) []byte {
	t.Helper()
	raw := noiseFile(t, muxFormat, 1<<19)
	return spliced(raw, blockStarts(t, raw)[40], bytes.Repeat([]byte{'x'}, wv.MaxResyncForTest+4096))
}

// decodeRest reads med to its end and returns the samples it delivered.
func decodeRest(t *testing.T, med format.Media) int64 {
	t.Helper()
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

// TestALengthAReadFallsShortOfIsNotHeldAgainstASeek: a seek lands by a
// block's own index and can reach blocks a read cannot, so no decode is cut
// at the length a short read leaves, and a walk measures what a seek reaches.
func TestALengthAReadFallsShortOfIsNotHeldAgainstASeek(t *testing.T) {
	raw := pastBound(t)
	const reached, stated, target = 40 * wavpack.BlockSamples, 1 << 19, 50 * wavpack.BlockSamples

	// A read to the end, a walk, then a seek past where the read stopped.
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if n := decodeRest(t, med); n != reached {
		t.Fatalf("a decode delivered %d samples, want the %d in front of the junk", n, reached)
	}
	if err := format.WalkMedia(context.Background(), med); err != nil {
		t.Fatal(err)
	}
	if tr, info := med.Info().Default(), med.Info(); tr.Samples != reached || tr.SamplesExact || info.Truncated != stated-reached {
		t.Errorf("after the read and a walk the Media says %d samples, exact %v, %d truncated; want %d, not exact, %d",
			tr.Samples, tr.SamplesExact, info.Truncated, reached, stated-reached)
	}
	if pos, err := med.SeekSample(context.Background(), target); err != nil || pos != target {
		t.Fatalf("a seek to %d landed at %d (%v)", target, pos, err)
	}
	if n := decodeRest(t, med); n != stated-target {
		t.Errorf("a decode from the seek delivered %d samples, want the %d behind it", n, stated-target)
	}

	// The other order: a seek and a read to the end, then a read from the
	// start. Only the read from the start finds where the stream ends.
	d := open(t, raw)
	if _, err := d.SeekSample(context.Background(), 0, target); err != nil {
		t.Fatal(err)
	}
	if _, end := readAll(t, d); end != io.EOF {
		t.Fatalf("the read from the seek ended %v", end)
	}
	exactAs(t, d, stated)
	if n := len(d.Warnings()); n != 0 {
		t.Errorf("a read from a seek to the end warned %v", d.Warnings())
	}
	if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != reached {
		t.Fatalf("the read from the start delivered %d samples, ending %v", samples, end)
	}
	settled(t, d, reached, stated-reached, "the header declares 524288 samples but the blocks end at 327680")

	// A read that ends short from a seek's landing is no whole read: where it
	// ends counts from the landing, so it judges nothing.
	d = open(t, raw)
	if _, err := d.SeekSample(context.Background(), 0, 100000); err != nil {
		t.Fatal(err)
	}
	if _, end := readAll(t, d); end != io.EOF {
		t.Fatalf("the read from the seek ended %v", end)
	}
	exactAs(t, d, stated)
	if got := container.TruncatedOf(d, 0); got != 0 {
		t.Errorf("%d samples counted truncated by a read from a seek, want none", got)
	}
}

// TestASeekAcrossIndexesOffTheirPlaceJudgesNothing: a seek's landing counts
// from the first block only if every block its walk crossed sat at its own
// index; behind any other, a read's end settles nothing.
func TestASeekAcrossIndexesOffTheirPlaceJudgesNothing(t *testing.T) {
	// Blocks 5 to 18 numbered 3000 early, and block 18 stating eight bytes
	// of the last one's header: a read from the start ends at 38000.
	raw := fixture(t, "seek-mono.wv")
	for k := 5; k < 19; k++ {
		raw = withIndex(t, raw, k, uint32(2000*k-3000))
	}
	at := blockStarts(t, raw)[18]
	binary.LittleEndian.PutUint32(raw[at+4:], binary.LittleEndian.Uint32(raw[at+4:])+8)
	d := open(t, raw)
	exactAs(t, d, 40000)
	if landed, err := d.SeekSample(context.Background(), 0, 20000); err != nil || landed != 19000 {
		t.Fatalf("a seek to 20000 landed at %d (%v), want block 11's own 19000", landed, err)
	}
	if _, end := readAll(t, d); end != io.EOF {
		t.Fatalf("the read from the seek ended %v", end)
	}
	exactAs(t, d, 40000)
	if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 38000 {
		t.Fatalf("the read from the start delivered %d samples, ending %v", samples, end)
	}
	settled(t, d, 38000, 2000, "the header declares 40000 samples but the blocks end at 38000")
}

// TestAReadPastTheLengthTakesTheClaimBack: a whole read that delivers more
// than the exact length leaves the length as it was and no longer exact,
// with nothing more warned than the step back that led there.
func TestAReadPastTheLengthTakesTheClaimBack(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	d := open(t, append(append([]byte(nil), mono...), mono...))
	exactAs(t, d, 40000)
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 80000 {
		t.Fatalf("read %d samples, ending %v", samples, end)
	}
	if tr := d.Tracks()[0]; tr.Samples != 40000 || tr.SamplesExact || container.TruncatedOf(d, 0) != 0 {
		t.Errorf("the track says %d samples, exact %v, %d truncated; want 40000, not exact, none",
			tr.Samples, tr.SamplesExact, container.TruncatedOf(d, 0))
	}
	oneDamage(t, d, len(mono), "the blocks step back from sample 40000 to 0")
	// The overrun is noted once, in the open's words, and a second end says
	// nothing more.
	const note = "the header declares 40000 samples but the blocks run to 80000"
	said := func() (n int) {
		for _, w := range d.Warnings() {
			if w.Kind == container.Note && strings.Contains(w.Msg, note) {
				n++
			}
		}
		return n
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != io.EOF || said() != 1 {
		t.Errorf("the overrun was noted %d times in %+v (next read %v), want once naming %q", said(), d.Warnings(), err, note)
	}
}

// swallowedTail is three muxed blocks whose middle one states the last one's
// bytes as its own: the tail scan finds the last block, and a read reaches
// the end of the data with no damage on the way.
func swallowedTail(t *testing.T) []byte {
	t.Helper()
	track, blocks := muxBlocks(t, 3*wavpack.BlockSamples, -1)
	var dst seekBuf
	writeAll(t, &dst, track, blocks, nil)
	raw := dst.Buf
	starts := blockStarts(t, raw)
	size := binary.LittleEndian.Uint32(raw[starts[1]+4:])
	binary.LittleEndian.PutUint32(raw[starts[1]+4:], size+uint32(starts[3]-starts[2]))
	return raw
}

// TestAShortfallTheReadAloneFindsSettles: a read that ends short with nothing
// warned on the way still settles, and a Strict read that refuses there
// refuses again on the next call.
func TestAShortfallTheReadAloneFindsSettles(t *testing.T) {
	raw := swallowedTail(t)
	d := open(t, raw)
	pkts, end := readAll(t, d)
	if samples, _ := inOrder(t, pkts); end != io.EOF || samples != 2*wavpack.BlockSamples {
		t.Fatalf("read %d samples, ending %v", samples, end)
	}
	settled(t, d, 2*wavpack.BlockSamples, wavpack.BlockSamples, "the header declares 24576 samples but the blocks end at 16384")
	if n := len(d.Warnings()); n != 1 {
		t.Errorf("%d warnings, want the shortfall alone", n)
	}

	ds, err := wv.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, first := readAll(t, ds)
	var pkt container.Packet
	if second := ds.ReadPacket(&pkt); first == io.EOF || second == nil || second.Error() != first.Error() {
		t.Fatalf("a Strict read ended %v, and the next call %v; want the shortfall refused twice", first, second)
	}
}

// TestAWholeStreamSettlesNothing: a read to the end of an intact stream,
// from its start or from a seek, keeps the length exact and warns nothing.
func TestAWholeStreamSettlesNothing(t *testing.T) {
	for _, name := range []string{"seek-mono.wv", "sine-s16.wv", "golden-pipe.wv"} {
		for _, from := range []int64{-1, 5000} {
			d := open(t, fixture(t, name))
			want := d.Tracks()[0].Samples
			if from >= 0 {
				if _, err := d.SeekSample(context.Background(), 0, from); err != nil {
					t.Fatal(err)
				}
			}
			if _, end := readAll(t, d); end != io.EOF {
				t.Fatalf("%s: the read ended %v", name, end)
			}
			exactAs(t, d, want)
			if got := container.TruncatedOf(d, 0); got != 0 || len(d.Warnings()) != 0 {
				t.Errorf("%s from %d: %d truncated and warnings %v, want none", name, from, got, d.Warnings())
			}
		}
	}
}
