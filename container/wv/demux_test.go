package wv_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/waxerr"
)

// fixture loads a committed testdata file, from this package's own testdata
// or from the shared one.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for _, base := range []string{filepath.Join(dir, "testdata"), filepath.Join(dir, "..", "..", "testdata")} {
		if raw, err := os.ReadFile(filepath.Join(base, name)); err == nil {
			return raw
		}
	}
	t.Fatalf("fixture %s not found", name)
	return nil
}

// walk reads every packet, asserting the stream is contiguous: each block
// starts where the previous one ended.
func walk(t *testing.T, d *wv.Demuxer) (blocks int, samples int64) {
	t.Helper()
	var pkt container.Packet
	next := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return blocks, next
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != next {
			t.Fatalf("block %d starts at %d, want %d", blocks, pkt.PTS, next)
		}
		if pkt.Dur <= 0 || len(pkt.Data) == 0 || !pkt.Sync {
			t.Fatalf("bad packet: dur=%d len=%d sync=%v", pkt.Dur, len(pkt.Data), pkt.Sync)
		}
		if !wavpack.Match(pkt.Data) {
			t.Fatalf("block %d does not start with its own header", blocks)
		}
		next += pkt.Dur
		blocks++
	}
}

func TestDemuxPackets(t *testing.T) {
	d, err := wv.NewDemuxer(container.BytesSource(fixture(t, "sine-s16.wv")), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.WavPack || tr.Fmt.Rate != 44100 || tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 {
		t.Fatalf("track = %+v", tr)
	}
	// SamplesExact stays false: every block declares its own count, so the
	// decoder never over-produces and there is nothing to trim to.
	if tr.Samples != 15435 || tr.SamplesExact {
		t.Fatalf("samples = %d exact=%v, want 15435 and not exact", tr.Samples, tr.SamplesExact)
	}
	if !tr.Default {
		t.Error("the only track is not the default")
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("warnings on a clean fixture: %v", d.Warnings())
	}
	if blocks, samples := walk(t, d); samples != 15435 || blocks < 2 {
		t.Fatalf("walked %d samples in %d blocks", samples, blocks)
	}
}

func TestStrictCleanFixture(t *testing.T) {
	for _, name := range []string{"sine-s16.wv", "seek-mono.wv", "tagged.wv"} {
		t.Run(name, func(t *testing.T) {
			d, err := wv.NewDemuxer(container.BytesSource(fixture(t, name)), &wv.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, samples := walk(t, d); samples == 0 {
				t.Fatal("walked no samples")
			}
		})
	}
}

// TestAPEv2Tags reads the tag block a .wv file carries after its audio: the
// fields must surface under canonical keys, and the tag bytes must stay out of
// the audio walk.
func TestAPEv2Tags(t *testing.T) {
	raw := fixture(t, "tagged.wv")
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tags := d.Tags()
	for key, want := range map[string]string{
		"TITLE":                 "WavPack Fixture",
		"ARTIST":                "WaxFlow",
		"ALBUM":                 "Stage Four",
		"TRACKNUMBER":           "7", // stored as the APEv2 spelling "Track"
		"REPLAYGAIN_TRACK_GAIN": "-3.21 dB",
	} {
		got := tags[key]
		if len(got) != 1 || got[0] != want {
			t.Errorf("tag %s = %v, want [%s]", key, got, want)
		}
	}
	if _, samples := walk(t, d); samples != 15435 {
		t.Fatalf("walked %d samples; the tag block must not reach the audio walk", samples)
	}
}

// TestUnknownTotalSamples covers the encoder that never learned its length: the
// declared total is the all-ones escape, and the length comes from a scan of
// the last block instead.
func TestUnknownTotalSamples(t *testing.T) {
	raw := append([]byte(nil), fixture(t, "seek-mono.wv")...)
	binary.LittleEndian.PutUint32(raw[12:], 0xffffffff)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Samples != 40000 {
		t.Fatalf("samples = %d, want 40000 from the tail scan", tr.Samples)
	}
	if _, samples := walk(t, d); samples != 40000 {
		t.Fatalf("walked %d samples", samples)
	}
}

// TestTrailingMetadataBlock pins the shape every real .wv ends with: a
// sample-free block carrying the MD5 signature and the block checksum. Its
// block index is not a position, so nothing may treat it as one.
func TestTrailingMetadataBlock(t *testing.T) {
	raw := fixture(t, "seek-mono.wv")
	// The last block must be the sample-free one, or the cases below are
	// testing a shape the fixture no longer has.
	h, err := lastBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.Audio() {
		t.Fatal("the fixture no longer ends in a sample-free block; regenerate it with `wavpack -m`")
	}
	if h.BlockIndex != 0 {
		t.Logf("trailing block index = %d", h.BlockIndex)
	}

	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A seek at or past the end lands on the last block that carried samples,
	// not on the trailing one, whose index is zero here.
	for _, target := range []int64{40000, 1 << 40} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed != 38000 {
			t.Errorf("seek to %d landed at %d, want the last audio block at 38000", target, landed)
		}
	}
	// And the length scan reaches past it to the last audio block.
	unknown := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(unknown[12:], 0xffffffff)
	du, err := wv.NewDemuxer(container.BytesSource(unknown), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := du.Tracks()[0].Samples; got != 40000 {
		t.Errorf("tail scan = %d, want 40000; the trailing block must not stop it", got)
	}
}

// lastBlock walks a .wv to its final block header.
func lastBlock(raw []byte) (wavpack.BlockHeader, error) {
	var h wavpack.BlockHeader
	for off := int64(0); off+wavpack.BlockHeaderLen <= int64(len(raw)); {
		next, err := wavpack.ParseBlockHeader(raw[off:])
		if err != nil {
			return h, err
		}
		h = next
		off += next.Size
	}
	return h, nil
}

// TestSeekLandsOnBlockStart checks that a seek lands on the block containing
// the target, never past it, and that reads resume there.
func TestSeekLandsOnBlockStart(t *testing.T) {
	raw := fixture(t, "seek-mono.wv")
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	const blockLen = 2000
	for _, target := range []int64{0, 1, 1999, 2000, 12345, 39999, 40000, 1 << 40} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed%blockLen != 0 {
			t.Errorf("seek to %d landed at %d, not on a block boundary", target, landed)
		}
		if landed > target {
			t.Errorf("seek to %d landed past it at %d", target, landed)
		}
		if target < 40000 && target-landed >= blockLen {
			t.Errorf("seek to %d landed at %d, more than one block early", target, landed)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if pkt.PTS != landed {
			t.Errorf("post-seek packet at %d, want %d", pkt.PTS, landed)
		}
	}
	if _, err := d.SeekSample(1, 0); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("seek on a nonexistent track: %v", err)
	}
	if _, err := d.SeekSample(0, -1); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("negative seek target: %v", err)
	}
}

// TestTrailingGarbage checks that bytes after the last block are dropped with
// a warning rather than mistaken for audio, and that strict mode rejects them.
func TestTrailingGarbage(t *testing.T) {
	raw := append(append([]byte(nil), fixture(t, "sine-s16.wv")...), bytes.Repeat([]byte{'w', 'x'}, 64)...)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, samples := walk(t, d); samples != 15435 {
		t.Fatalf("walked %d samples, want the audio without the garbage", samples)
	}
	if len(d.Warnings()) == 0 {
		t.Error("trailing garbage was dropped with no warning")
	}
	ds, err := wv.NewDemuxer(container.BytesSource(raw), &wv.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var strictErr error
	for strictErr == nil {
		strictErr = ds.ReadPacket(&pkt)
	}
	if errors.Is(strictErr, io.EOF) {
		t.Error("strict mode accepted trailing garbage")
	}
}

// TestAppendedID3v2IsNotAudio covers the third trailer taggers write, the one
// only a footer makes findable from behind. apen and flacn already peeled it
// and wv did not, so the same tag on a .wv left twenty bytes standing inside
// the block walk's extent, drawing a "trailing bytes are not a block" warning
// rather than being recognized.
func TestAppendedID3v2IsNotAudio(t *testing.T) {
	// Header, ten bytes of body, matching footer.
	tag := []byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 10}
	tag = append(tag, make([]byte, 10)...)
	tag = append(tag, '3', 'D', 'I', 4, 0, 0x10, 0, 0, 0, 10)
	raw := append(append([]byte(nil), fixture(t, "sine-s16.wv")...), tag...)

	// Non-strict, so an unrecognized trailer lands in the warning list rather
	// than coming back as an error: the list is the assertion.
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, samples := walk(t, d); samples != 15435 {
		t.Fatalf("walked %d samples, want 15435", samples)
	}
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("an appended ID3v2 tag is a trailer, not damage: %v", w)
	}

	// And strict mode must not refuse a file for carrying one.
	ds, err := wv.NewDemuxer(container.BytesSource(raw), &wv.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, samples := walk(t, ds); samples != 15435 {
		t.Fatalf("strict mode walked %d samples, want 15435", samples)
	}
}

// TestTruncatedTail checks a file whose last block is cut short: the whole
// blocks before it still decode, and the partial one is dropped.
func TestTruncatedTail(t *testing.T) {
	full := fixture(t, "seek-mono.wv")
	d, err := wv.NewDemuxer(container.BytesSource(full[:len(full)-500]), nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks, samples := walk(t, d)
	if blocks == 0 || samples == 0 {
		t.Fatal("a truncated tail lost the whole stream")
	}
	if samples >= 40000 {
		t.Fatalf("walked %d samples from a truncated file", samples)
	}
}

// TestWarningsAreBoundedAndDeduped checks the tolerated-damage list cannot
// grow without limit: a seek re-walks spans a linear read already crossed, and
// the same damage must not be recorded twice.
func TestWarningsAreBoundedAndDeduped(t *testing.T) {
	raw := append(append([]byte(nil), fixture(t, "seek-mono.wv")...), bytes.Repeat([]byte{0x5a}, 512)...)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	drain := func() {
		var pkt container.Packet
		for d.ReadPacket(&pkt) == nil {
		}
	}
	drain()
	first := len(d.Warnings())
	if first == 0 {
		t.Fatal("trailing garbage produced no warning")
	}
	for range 20 {
		if _, err := d.SeekSample(0, 1<<40); err != nil {
			t.Fatal(err)
		}
		drain()
	}
	if got := len(d.Warnings()); got != first {
		t.Errorf("warnings grew from %d to %d over repeated walks of the same damage", first, got)
	}
}

// TestRejectsNonWavPack checks the demuxer refuses input that is not a .wv,
// with the public container name in the message.
func TestRejectsNonWavPack(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("wvpk"), bytes.Repeat([]byte{'z'}, 4096)} {
		_, err := wv.NewDemuxer(container.BytesSource(raw), nil)
		if err == nil {
			t.Fatalf("accepted %d bytes of junk", len(raw))
		}
		if !bytes.HasPrefix([]byte(err.Error()), []byte("wavpack: ")) {
			t.Errorf("error %q does not carry the public container name", err)
		}
	}
}

// TestLeadingMetadataBlock covers the sample-free block an encoder can write
// before the audio: it is stepped over rather than delivered as a packet.
func TestLeadingMetadataBlock(t *testing.T) {
	raw := fixture(t, "sine-s16.wv")
	// A minimal block with no samples: header only, with the block index and
	// flags of the stream it precedes.
	head := append([]byte(nil), raw[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(head[4:], wavpack.BlockHeaderLen-8)
	binary.LittleEndian.PutUint32(head[20:], 0) // no samples
	d, err := wv.NewDemuxer(container.BytesSource(append(head, raw...)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != 15435 {
		t.Fatalf("samples = %d", tr.Samples)
	}
	if _, samples := walk(t, d); samples != 15435 {
		t.Fatalf("walked %d samples", samples)
	}
}

// TestUnknownTotalWithUnscannableTail covers the length scan when it finds
// nothing: a stream larger than the scan's chunk, whose declared total is the
// unknown escape and whose tail is damaged. The length stays unknown, the
// readable blocks still decode, and the scan terminates.
func TestUnknownTotalWithUnscannableTail(t *testing.T) {
	base := fixture(t, "seek-mono.wv")
	// Well past the scan chunk, so the backward walk takes several passes.
	raw := make([]byte, 0, 8*len(base))
	raw = append(raw, base...)
	for len(raw) < 400<<10 {
		raw = append(raw, bytes.Repeat([]byte{0x11, 0x22}, 4096)...)
	}
	binary.LittleEndian.PutUint32(raw[12:], 0xffffffff)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Error(err)
			return
		}
		if tr := d.Tracks()[0]; tr.Samples != -1 {
			t.Errorf("samples = %d, want unknown", tr.Samples)
		}
		if _, samples := walk(t, d); samples != 40000 {
			t.Errorf("walked %d samples, want the 40000 that are readable", samples)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the tail scan did not terminate")
	}
}

// TestSeekOverAHoleLandsAtOrBeforeTheTarget: a block header states its own
// position and its own sample count, and nothing in the header is checksummed,
// so a damaged stream can leave a hole between one block's end and the next
// one's start. A target inside that hole is contained by no block at all, and
// the Seeker contract is to land at or before it: the caller decodes forward
// from the landing and discards, so it can recover from landing early and
// cannot recover from landing late. Found by FuzzDemux.
func TestSeekOverAHoleLandsAtOrBeforeTheTarget(t *testing.T) {
	raw := append([]byte(nil), fixture(t, "sine-s16.wv")...)
	h, err := wavpack.ParseBlockHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Shrink the first block's sample count. Its index stays 0 and the second
	// block's stays 7717, so the timeline now holds nothing from 549 to 7717.
	// The checksum is restated over the edit, leaving the hole as the one
	// thing wrong with the stream.
	const covered, hole = 549, 7717
	binary.LittleEndian.PutUint32(raw[20:], covered)
	wavpack.UpdateBlockChecksum(raw[:h.Size])

	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		target, want int64
	}{
		{100, 0},           // inside the first block, as always
		{1000, 0},          // inside the hole: the block before it
		{hole, hole},       // the first sample the second block does hold
		{hole + 500, hole}, // inside the second block
	} {
		landed, err := d.SeekSample(0, tt.target)
		if err != nil {
			t.Fatalf("seek to %d: %v", tt.target, err)
		}
		if landed != tt.want {
			t.Errorf("seek to %d landed at %d, want %d", tt.target, landed, tt.want)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek to %d: %v", tt.target, err)
		}
		if pkt.PTS != landed {
			t.Errorf("post-seek packet at %d, want the landing %d", pkt.PTS, landed)
		}
	}
}
