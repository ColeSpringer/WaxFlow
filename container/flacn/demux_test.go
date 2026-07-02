package flacn_test

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
)

// fixture loads a committed testdata file.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// walk reads every packet, asserting the stream is contiguous: each
// frame starts where the previous ended.
func walk(t *testing.T, d *flacn.Demuxer) (frames int, samples int64) {
	t.Helper()
	var pkt container.Packet
	next := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return frames, next
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != next {
			t.Fatalf("frame %d starts at %d, want %d", frames, pkt.PTS, next)
		}
		if pkt.Dur <= 0 || len(pkt.Data) == 0 || !pkt.Sync {
			t.Fatalf("bad packet: dur=%d len=%d sync=%v", pkt.Dur, len(pkt.Data), pkt.Sync)
		}
		next += pkt.Dur
		frames++
	}
}

func TestDemuxPackets(t *testing.T) {
	d, err := flacn.NewDemuxer(container.BytesSource(fixture(t, "sine-s16.flac")), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.FLAC || tr.Fmt.Rate != 44100 || tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 {
		t.Fatalf("track = %+v", tr)
	}
	if tr.Samples != 15435 {
		t.Fatalf("samples = %d", tr.Samples)
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("warnings on clean fixture: %v", d.Warnings())
	}
	if _, samples := walk(t, d); samples != 15435 {
		t.Fatalf("walked %d samples", samples)
	}
}

func TestStrictCleanFixture(t *testing.T) {
	d, err := flacn.NewDemuxer(container.BytesSource(fixture(t, "noise-s16.flac")), &flacn.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, samples := walk(t, d); samples != 15435 {
		t.Fatalf("walked %d samples", samples)
	}
}

// seekAndCheck seeks and verifies the demuxer contract: landing on the
// frame containing the target (or the last frame for past-end targets).
func seekAndCheck(t *testing.T, d *flacn.Demuxer, target int64) {
	t.Helper()
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatalf("seek to %d: %v", target, err)
	}
	if landed > target {
		t.Fatalf("seek to %d landed after it at %d", target, landed)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("read after seek to %d: %v", target, err)
	}
	if pkt.PTS != landed {
		t.Fatalf("first packet at %d, landed said %d", pkt.PTS, landed)
	}
	if target < 15435 && target >= pkt.PTS+pkt.Dur {
		t.Fatalf("seek to %d landed a frame too early [%d, %d)", target, pkt.PTS, pkt.PTS+pkt.Dur)
	}
}

func TestSeekBisection(t *testing.T) {
	// ffmpeg writes no SEEKTABLE, so this is the pure bisection+walk path.
	d, err := flacn.NewDemuxer(container.BytesSource(fixture(t, "noise-s16.flac")), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 1, 4095, 4096, 8191, 12000, 15434, 99999} {
		seekAndCheck(t, d, target)
	}
	// Interleave a seek with linear reading: stream stays contiguous.
	landed, err := d.SeekSample(0, 6000)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	sum := landed
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != sum {
			t.Fatalf("post-seek stream not contiguous: %d, want %d", pkt.PTS, sum)
		}
		sum += pkt.Dur
	}
	if sum != 15435 {
		t.Fatalf("post-seek walk ended at %d", sum)
	}
}

// withSeekTable inserts a SEEKTABLE metadata block after STREAMINFO.
func withSeekTable(t *testing.T, raw []byte, points [][3]uint64) []byte {
	t.Helper()
	if string(raw[:4]) != "fLaC" {
		t.Fatal("not flac")
	}
	// STREAMINFO must be first; clear its last-block flag.
	out := append([]byte(nil), raw[:4+4+flac.StreamInfoLen]...)
	wasLast := out[4]&0x80 != 0
	out[4] &^= 0x80
	// The SEEKTABLE block (type 3).
	hdr := [4]byte{3}
	if wasLast {
		hdr[0] |= 0x80
	}
	body := make([]byte, 18*len(points))
	for i, p := range points {
		binary.BigEndian.PutUint64(body[i*18:], p[0])
		binary.BigEndian.PutUint64(body[i*18+8:], p[1])
		binary.BigEndian.PutUint16(body[i*18+16:], uint16(p[2]))
	}
	hdr[1] = byte(len(body) >> 16)
	hdr[2] = byte(len(body) >> 8)
	hdr[3] = byte(len(body))
	out = append(out, hdr[:]...)
	out = append(out, body...)
	return append(out, raw[4+4+flac.StreamInfoLen:]...)
}

// frameOffsets scans a headerless-metadata fixture for its real frame
// starts (offset, sample) the same way an encoder would have recorded
// them, using the exported header parser.
func frameOffsets(t *testing.T, raw []byte) (offs []int64, samples []int64) {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Walk packets for sample positions, and find byte offsets by
	// scanning for each frame's header bytes.
	var pkt container.Packet
	off := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		for ; ; off++ {
			if string(raw[off:off+2]) == string(pkt.Data[:2]) && string(raw[off:off+int64(min(len(pkt.Data), 16))]) == string(pkt.Data[:min(len(pkt.Data), 16)]) {
				break
			}
		}
		offs = append(offs, off)
		samples = append(samples, pkt.PTS)
		off += int64(len(pkt.Data))
	}
}

func TestSeekTable(t *testing.T) {
	raw := fixture(t, "noise-s16.flac")
	offs, samples := frameOffsets(t, raw)
	if len(offs) < 3 {
		t.Fatalf("fixture has only %d frames", len(offs))
	}
	firstFrame := offs[0]
	var points [][3]uint64
	for i := range offs {
		points = append(points, [3]uint64{uint64(samples[i]), uint64(offs[i] - firstFrame), 4096})
	}
	// A placeholder point must be ignored.
	points = append(points, [3]uint64{^uint64(0), 0, 0})

	// The inserted table shifts every frame by its own length; rebuild
	// offsets relative to the new first-frame position (table length is
	// constant, so relative offsets are unchanged).
	d, err := flacn.NewDemuxer(container.BytesSource(withSeekTable(t, raw, points)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("warnings: %v", d.Warnings())
	}
	for _, target := range []int64{0, 4096, 5000, 12000, 15434, 20000} {
		seekAndCheck(t, d, target)
	}

	// Bogus offsets must not derail seeking: the hint is verified and
	// falls back to bisection.
	bogus := [][3]uint64{{4096, 5, 4096}, {8192, 99999999, 4096}}
	d, err = flacn.NewDemuxer(container.BytesSource(withSeekTable(t, raw, bogus)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{4100, 9000, 15434} {
		seekAndCheck(t, d, target)
	}
}

// Trailer builders for TestTrailingTags.
func id3v1Tag() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAG")
	copy(tag[3:], "some title")
	return tag
}

func apeTag(withHeader bool) []byte {
	block := func(kind uint32) []byte {
		b := make([]byte, 32)
		copy(b, "APETAGEX")
		binary.LittleEndian.PutUint32(b[8:], 2000) // version
		binary.LittleEndian.PutUint32(b[12:], 32)  // size: items + footer
		binary.LittleEndian.PutUint32(b[16:], 0)   // item count
		binary.LittleEndian.PutUint32(b[20:], kind)
		return b
	}
	if !withHeader {
		return block(0)
	}
	return append(block(1<<31|1<<29), block(1<<31)...)
}

func appendedID3v2() []byte {
	// Header, 10 bytes of body, matching footer.
	body := make([]byte, 10)
	tag := append([]byte("ID3\x04\x00\x10\x00\x00\x00\x0A"), body...)
	return append(tag, []byte("3DI\x04\x00\x10\x00\x00\x00\x0A")...)
}

// TestTrailingTags verifies the last frame survives every recognized
// trailer shape (taggers bolt these onto FLAC files), including stacked
// tags, and that strict mode refuses them all.
func TestTrailingTags(t *testing.T) {
	raw := fixture(t, "sine-s16.flac")
	trailers := map[string][]byte{
		"id3v1":              id3v1Tag(),
		"apev2":              apeTag(false),
		"apev2+header":       apeTag(true),
		"appended id3v2":     appendedID3v2(),
		"ape then id3v1":     append(apeTag(true), id3v1Tag()...),
		"padding then id3v1": append(make([]byte, 300), id3v1Tag()...),
	}
	for name, trailer := range trailers {
		t.Run(name, func(t *testing.T) {
			tagged := append(append([]byte(nil), raw...), trailer...)
			d, err := flacn.NewDemuxer(container.BytesSource(tagged), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, samples := walk(t, d); samples != 15435 {
				t.Fatalf("walked %d samples (last frame lost to the trailer)", samples)
			}
			if len(d.Warnings()) == 0 {
				t.Fatal("no warning about the trailer")
			}

			// Strict mode refuses the trailer at read time.
			d, err = flacn.NewDemuxer(container.BytesSource(tagged), &flacn.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if err == io.EOF {
					t.Fatal("strict mode silently accepted trailing junk")
				}
				if err != nil {
					break // expected
				}
			}
		})
	}

	// Pure NUL padding is a special case: this CRC-16 (zero init, no
	// final XOR) satisfies CRC(frame || frameCRC) == 0 and stays 0 over
	// zeros, so the padding checksums as part of the final frame and is
	// absorbed silently in both modes; the decoder never reads past the
	// subframes, so decode stays exact.
	t.Run("nul padding absorbed", func(t *testing.T) {
		tagged := append(append([]byte(nil), raw...), make([]byte, 300)...)
		for _, strict := range []bool{false, true} {
			d, err := flacn.NewDemuxer(container.BytesSource(tagged), &flacn.DemuxerOptions{Strict: strict})
			if err != nil {
				t.Fatal(err)
			}
			if _, samples := walk(t, d); samples != 15435 {
				t.Fatalf("walked %d samples", samples)
			}
		}
	})
}

// emptyFLAC builds a metadata-only stream: fLaC marker plus a lone
// STREAMINFO (44.1k stereo 16-bit) declaring the given total.
func emptyFLAC(declared int64) []byte {
	si := make([]byte, flac.StreamInfoLen)
	si[0], si[1] = 0x10, 0x00 // min block 4096
	si[2], si[3] = 0x10, 0x00 // max block 4096
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40|1<<1
	si[13] = 0xF0 | byte(declared>>32)&0xF
	binary.BigEndian.PutUint32(si[14:], uint32(declared))
	out := []byte("fLaC\x80\x00\x00\x22")
	return append(out, si...)
}

// TestZeroFrameStream accepts metadata-only streams: encoding empty
// input produces exactly this shape, and it must open with an empty
// track rather than fail (reads EOF, seeks land at 0).
func TestZeroFrameStream(t *testing.T) {
	for _, strict := range []bool{false, true} {
		d, err := flacn.NewDemuxer(container.BytesSource(emptyFLAC(0)), &flacn.DemuxerOptions{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: %v", strict, err)
		}
		if s := d.Tracks()[0].Samples; s != 0 {
			t.Fatalf("samples = %d, want 0", s)
		}
		if len(d.Warnings()) != 0 {
			t.Fatalf("warnings on a valid empty stream: %v", d.Warnings())
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != io.EOF {
			t.Fatalf("ReadPacket = %v, want EOF", err)
		}
		landed, err := d.SeekSample(0, 500)
		if err != nil || landed != 0 {
			t.Fatalf("seek = %d, %v; want 0, nil", landed, err)
		}
		if err := d.ReadPacket(&pkt); err != io.EOF {
			t.Fatalf("post-seek ReadPacket = %v, want EOF", err)
		}
	}

	// Declaring samples without any frames is an inconsistency: warned
	// in tolerant mode, refused in strict.
	d, err := flacn.NewDemuxer(container.BytesSource(emptyFLAC(1000)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings()) == 0 {
		t.Error("no warning about declared samples with no frames")
	}
	if s := d.Tracks()[0].Samples; s != 0 {
		t.Errorf("samples = %d, want 0 (content wins over the declaration)", s)
	}
	if _, err := flacn.NewDemuxer(container.BytesSource(emptyFLAC(1000)), &flacn.DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted declared samples with no frames")
	}
}

func TestRejectsTruncatedMetadata(t *testing.T) {
	raw := fixture(t, "sine-s16.flac")
	for _, n := range []int{0, 3, 4, 7, 20} {
		if _, err := flacn.NewDemuxer(container.BytesSource(raw[:n]), nil); err == nil {
			t.Errorf("accepted %d-byte prefix", n)
		}
	}
	if _, err := flacn.NewDemuxer(container.BytesSource([]byte("fLaC")), nil); err == nil {
		t.Error("accepted marker-only file")
	}
}

var errSentinel = errors.New("sentinel")

// failAfter fails reads past a byte limit, for I/O error propagation.
type failAfter struct {
	data  []byte
	limit int64
}

func (s failAfter) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > s.limit {
		return 0, errSentinel
	}
	return copy(p, s.data[off:]), nil
}

func (s failAfter) Size() int64 { return int64(len(s.data)) }

func TestReadErrorPropagates(t *testing.T) {
	raw := fixture(t, "sine-s16.flac")
	d, err := flacn.NewDemuxer(failAfter{raw, int64(len(raw)) / 2}, nil)
	if err != nil {
		// Header area unreadable is also a valid failure point.
		return
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			t.Fatal("mid-file read failure surfaced as clean EOF")
		}
		if err != nil {
			if !errors.Is(err, errSentinel) {
				t.Fatalf("error chain lost the cause: %v", err)
			}
			return
		}
	}
}
