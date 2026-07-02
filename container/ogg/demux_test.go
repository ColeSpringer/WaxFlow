package ogg

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
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

// buildPageRaw assembles one CRC-valid Ogg page from a prepared lacing
// table and body.
func buildPageRaw(flags byte, granule int64, serial, seq uint32, lacing, body []byte) []byte {
	hdr := make([]byte, headerLen)
	copy(hdr, "OggS")
	hdr[5] = flags
	binary.LittleEndian.PutUint64(hdr[6:], uint64(granule))
	binary.LittleEndian.PutUint32(hdr[14:], serial)
	binary.LittleEndian.PutUint32(hdr[18:], seq)
	hdr[26] = byte(len(lacing))
	page := append(hdr, lacing...)
	page = append(page, body...)
	crc := crc32(0, page)
	binary.LittleEndian.PutUint32(page[22:], crc)
	// CRC covers the page with the field zeroed, which it was.
	return page
}

// buildPage assembles one page holding whole packets.
func buildPage(flags byte, granule int64, serial, seq uint32, packets ...[]byte) []byte {
	var lacing []byte
	var body []byte
	for _, p := range packets {
		n := len(p)
		for n >= 255 {
			lacing = append(lacing, 255)
			n -= 255
		}
		lacing = append(lacing, byte(n))
		body = append(body, p...)
	}
	return buildPageRaw(flags, granule, serial, seq, lacing, body)
}

// repaginate rebuilds a one-page-per-frame stream from a fixture,
// optionally splitting every frame across three pages (continued
// packets). Returns the stream and the source's packet walk for
// comparison.
func repaginate(t *testing.T, name string, split bool) ([]byte, []container.Packet) {
	t.Helper()
	raw := fixture(t, name)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	serial := binary.LittleEndian.Uint32(raw[14:18])
	var packets []container.Packet
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		cp := pkt
		cp.Data = append([]byte(nil), pkt.Data...)
		packets = append(packets, cp)
	}

	// Copy the BOS and header pages verbatim.
	var out []byte
	off := 0
	for i := 0; i < 2; i++ {
		n := int(raw[off+26])
		size := headerLen + n
		for _, l := range raw[off+headerLen : off+headerLen+n] {
			size += int(l)
		}
		out = append(out, raw[off:off+size]...)
		off += size
	}
	seq := uint32(2)
	for i, p := range packets {
		granule := p.PTS + p.Dur
		flags := byte(0)
		if i == len(packets)-1 {
			flags = flagEOS
		}
		if !split || len(p.Data) < 600 {
			out = append(out, buildPage(flags, granule, serial, seq, p.Data)...)
			seq++
			continue
		}
		// Split into three pages: the first two carry 255-byte segments
		// only (packet unfinished), the last carries the remainder.
		third := len(p.Data) / 3 / 255 * 255
		part1, part2, rest := p.Data[:third], p.Data[third:2*third], p.Data[2*third:]
		fullLacing := func(n int) []byte {
			l := make([]byte, n/255)
			for i := range l {
				l[i] = 255
			}
			return l
		}
		out = append(out, buildPageRaw(0, -1, serial, seq, fullLacing(len(part1)), part1)...)
		out = append(out, buildPageRaw(flagContinued, -1, serial, seq+1, fullLacing(len(part2)), part2)...)
		var lastLacing []byte
		n := len(rest)
		for n >= 255 {
			lastLacing = append(lastLacing, 255)
			n -= 255
		}
		lastLacing = append(lastLacing, byte(n))
		out = append(out, buildPageRaw(flagContinued|flags, granule, serial, seq+2, lastLacing, rest)...)
		seq += 3
	}
	return out, packets
}

// TestContinuedPackets round-trips frames split across pages.
func TestContinuedPackets(t *testing.T) {
	stream, want := repaginate(t, "noise-s24.oga", true)
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			if i != len(want) {
				t.Fatalf("got %d packets, want %d", i, len(want))
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if i >= len(want) {
			t.Fatal("more packets than the source")
		}
		if pkt.PTS != want[i].PTS || pkt.Dur != want[i].Dur || string(pkt.Data) != string(want[i].Data) {
			t.Fatalf("packet %d differs after repagination", i)
		}
	}
	// Seeking still lands exactly on reassembled frames.
	landed, err := d.SeekSample(0, want[2].PTS+10)
	if err != nil {
		t.Fatal(err)
	}
	if landed != want[2].PTS {
		t.Fatalf("seek landed at %d, want %d", landed, want[2].PTS)
	}
}

func TestDemuxPackets(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixture(t, "sine-s16.oga")), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.FLAC || tr.Fmt.Rate != 44100 || tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 {
		t.Fatalf("track = %+v", tr)
	}
	if tr.Samples != 15435 {
		t.Fatalf("samples = %d (granule-derived length)", tr.Samples)
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("warnings on clean fixture: %v", d.Warnings())
	}
	var pkt container.Packet
	next := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != next || pkt.Dur <= 0 || len(pkt.Data) == 0 || !pkt.Sync {
			t.Fatalf("packet pts=%d dur=%d len=%d, want pts %d", pkt.PTS, pkt.Dur, len(pkt.Data), next)
		}
		next += pkt.Dur
	}
	if next != 15435 {
		t.Fatalf("walked %d samples", next)
	}
}

func TestSeek(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixture(t, "noise-s24.oga")), nil)
	if err != nil {
		t.Fatal(err)
	}
	total := d.Tracks()[0].Samples
	for _, target := range []int64{0, 1, 4096, 9000, total - 1} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target {
			t.Fatalf("seek to %d landed at %d", target, landed)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if pkt.PTS != landed {
			t.Fatalf("first packet at %d, landed said %d", pkt.PTS, landed)
		}
		if target >= pkt.PTS+pkt.Dur {
			t.Fatalf("seek to %d landed a frame too early [%d, %d)", target, pkt.PTS, pkt.PTS+pkt.Dur)
		}
	}
	// Past-the-end: land on the last frame.
	landed, err := d.SeekSample(0, total+5000)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.PTS != landed || pkt.PTS+pkt.Dur != total {
		t.Fatalf("past-end landing [%d, %d), want end %d", pkt.PTS, pkt.PTS+pkt.Dur, total)
	}
}

func TestRejectsNonFLACMappings(t *testing.T) {
	vorbisBOS := append([]byte("\x01vorbis"), make([]byte, 23)...)
	stream := buildPage(flagBOS, 0, 111, 0, vorbisBOS)
	stream = append(stream, buildPage(flagEOS, 0, 111, 1, []byte{0})...)
	_, err := NewDemuxer(container.BytesSource(stream), nil)
	if err == nil {
		t.Fatal("accepted a vorbis-only stream")
	}
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Fatalf("error code = %v", waxerr.CodeOf(err))
	}
	if got := err.Error(); !contains(got, "vorbis") {
		t.Errorf("error does not name the found mapping: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSkipsForeignStream(t *testing.T) {
	// Multiplex a second (unknown) stream's pages between FLAC pages:
	// the demuxer must ignore them and warn once about the extra stream.
	raw := fixture(t, "sine-s16.oga")
	var pages [][]byte
	for off := 0; off < len(raw); {
		n := int(raw[off+26])
		size := headerLen + n
		for _, l := range raw[off+headerLen : off+headerLen+n] {
			size += int(l)
		}
		pages = append(pages, raw[off:off+size])
		off += size
	}
	var mixed []byte
	foreignBOS := buildPage(flagBOS, 0, 999, 0, append([]byte("junkhdr!"), make([]byte, 12)...))
	mixed = append(mixed, pages[0]...) // FLAC BOS first
	mixed = append(mixed, foreignBOS...)
	for i, p := range pages[1:] {
		mixed = append(mixed, p...)
		mixed = append(mixed, buildPage(0, int64(i), 999, uint32(i+1), []byte("x"))...)
	}
	d, err := NewDemuxer(container.BytesSource(mixed), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings()) == 0 {
		t.Error("no warning about the ignored extra stream")
	}
	var pkt container.Packet
	total := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		total += pkt.Dur
	}
	if total != 15435 {
		t.Fatalf("walked %d samples through the multiplexed stream", total)
	}
}

// flacBOSPacketBlocks builds an Ogg-FLAC identification packet (44.1k
// stereo 16-bit STREAMINFO, total 0) with the given block size bounds,
// declaring one further header packet.
func flacBOSPacketBlocks(minBlock, maxBlock int) []byte {
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], uint16(minBlock))
	binary.BigEndian.PutUint16(si[2:], uint16(maxBlock))
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40|1<<1
	si[13] = 0xF0
	pkt := []byte("\x7FFLAC\x01\x00\x00\x01fLaC\x00\x00\x00\x22")
	return append(pkt, si...)
}

func flacBOSPacket() []byte { return flacBOSPacketBlocks(4096, 4096) }

// buildFrameBytes crafts a parseable frame header (fixed-strategy bit,
// explicit 16-bit block size, 44.1k stereo 16-bit) followed by dummy
// payload. The CRC-8 is brute-forced through the real parser so the
// helper cannot drift from the wire format.
func buildFrameBytes(t *testing.T, coded uint64, blockSize int) []byte {
	t.Helper()
	b := []byte{0xFF, 0xF8, 0x79, 0x18}
	switch {
	case coded < 0x80:
		b = append(b, byte(coded))
	case coded < 0x800:
		b = append(b, 0xC0|byte(coded>>6), 0x80|byte(coded)&0x3F)
	default:
		b = append(b, 0xE0|byte(coded>>12), 0x80|byte(coded>>6)&0x3F, 0x80|byte(coded)&0x3F)
	}
	b = append(b, byte((blockSize-1)>>8), byte(blockSize-1))
	for crc := 0; crc < 256; crc++ {
		if fi, err := flac.ParseFrameHeader(append(b, byte(crc))); err == nil && fi.BlockSize == blockSize {
			return append(b, byte(crc), 0xAA, 0xBB) // header + dummy body
		}
	}
	t.Fatal("no CRC-8 satisfies the crafted header")
	return nil
}

// TestOldFormatNumbering covers pre-1.0 "old format" streams carried in
// Ogg: the variable-blocksize bit is clear but unequal STREAMINFO block
// bounds mean coded numbers are sample positions (flac.Numbering, shared
// with container/flacn). Positions must not be multiplied by the block
// size.
func TestOldFormatNumbering(t *testing.T) {
	frames := []struct {
		coded uint64
		bs    int
	}{{0, 4096}, {4096, 2304}, {6400, 2304}}
	stream := buildPage(flagBOS, 0, 55, 0, flacBOSPacketBlocks(1000, 4096))
	stream = append(stream, buildPage(0, 0, 55, 1, commentPacket())...)
	for i, f := range frames {
		flags := byte(0)
		if i == len(frames)-1 {
			flags = flagEOS
		}
		granule := int64(f.coded) + int64(f.bs)
		stream = append(stream, buildPage(flags, granule, 55, uint32(i+2), buildFrameBytes(t, f.coded, f.bs))...)
	}

	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for _, f := range frames {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != int64(f.coded) || pkt.Dur != int64(f.bs) {
			t.Fatalf("packet pts=%d dur=%d, want pts=%d dur=%d (coded numbers are sample positions)",
				pkt.PTS, pkt.Dur, f.coded, f.bs)
		}
	}
	landed, err := d.SeekSample(0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if landed != 4096 {
		t.Fatalf("seek to 5000 landed at %d, want 4096", landed)
	}
}

// commentPacket is a minimal VORBIS_COMMENT metadata block packet.
func commentPacket() []byte {
	return []byte{0x84, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0}
}

// TestEmptyStream tolerates headers with no audio (an encoder fed zero
// input, or a capture cut short): empty track, EOF reads, seeks land at
// 0, with a warning so strict mode still refuses.
func TestEmptyStream(t *testing.T) {
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)

	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Tracks()[0].Samples; s != 0 {
		t.Fatalf("samples = %d, want 0", s)
	}
	if len(d.Warnings()) == 0 {
		t.Error("no warning about the missing audio")
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

	if _, err := NewDemuxer(container.BytesSource(stream), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted a stream with no audio")
	}
}

// buildTailOnlyStream rebuilds the noise fixture so the final page holds
// nothing but the continuation tail of the last frame, with enough
// foreign-stream filler before it that seek bisection can converge onto
// that page. trailingForeign appends more filler after end of stream,
// which hides the tail granule from the length probe (unknown length).
func buildTailOnlyStream(t *testing.T, trailingForeign bool) ([]byte, []container.Packet) {
	t.Helper()
	raw := fixture(t, "noise-s24.oga")
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	serial := binary.LittleEndian.Uint32(raw[14:18])
	var packets []container.Packet
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		cp := pkt
		cp.Data = append([]byte(nil), pkt.Data...)
		packets = append(packets, cp)
	}
	if len(packets) < 3 {
		t.Fatal("fixture too short")
	}

	var out []byte
	off := 0
	for i := 0; i < 2; i++ { // BOS and header pages verbatim
		n := int(raw[off+26])
		size := headerLen + n
		for _, l := range raw[off+headerLen : off+headerLen+n] {
			size += int(l)
		}
		out = append(out, raw[off:off+size]...)
		off += size
	}
	seq := uint32(2)
	foreign := func(count int) {
		for i := 0; i < count; i++ {
			out = append(out, buildPage(0, int64(i), 999, seq, make([]byte, 3500))...)
			seq++
		}
	}
	for i, p := range packets[:len(packets)-1] {
		_ = i
		out = append(out, buildPage(0, p.PTS+p.Dur, serial, seq, p.Data)...)
		seq++
	}
	last := packets[len(packets)-1]
	head := last.Data[:len(last.Data)/2/255*255]
	tail := last.Data[len(head):]
	lacing255 := make([]byte, len(head)/255)
	for i := range lacing255 {
		lacing255[i] = 255
	}
	out = append(out, buildPageRaw(0, -1, serial, seq, lacing255, head)...)
	seq++
	foreign(40) // ~140 KiB, past the bisection window
	var tl []byte
	n := len(tail)
	for n >= 255 {
		tl = append(tl, 255)
		n -= 255
	}
	tl = append(tl, byte(n))
	out = append(out, buildPageRaw(flagContinued|flagEOS, last.PTS+last.Dur, serial, seq, tl, tail)...)
	seq++
	if trailingForeign {
		foreign(40) // hides the tail granule from the length probe
	}
	return out, packets
}

// TestPastEndSeekOnContinuedTail reproduces the tail-only-page case:
// bisection converges onto the final page, whose continued packet a
// restart rightly drops; the seek must still land on the last frame.
func TestPastEndSeekOnContinuedTail(t *testing.T) {
	for _, trailing := range []bool{false, true} {
		stream, packets := buildTailOnlyStream(t, trailing)
		last := packets[len(packets)-1]
		total := last.PTS + last.Dur

		d, err := NewDemuxer(container.BytesSource(stream), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !trailing {
			if s := d.Tracks()[0].Samples; s != total {
				t.Fatalf("samples = %d, want %d", s, total)
			}
		}
		for _, target := range []int64{total + 999, total - 1} {
			landed, err := d.SeekSample(0, target)
			if err != nil {
				t.Fatalf("trailing=%v seek to %d: %v", trailing, target, err)
			}
			if landed != last.PTS {
				t.Fatalf("trailing=%v seek to %d landed at %d, want %d", trailing, target, landed, last.PTS)
			}
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
			if pkt.PTS != last.PTS || string(pkt.Data) != string(last.Data) {
				t.Fatalf("trailing=%v landed packet is not the last frame", trailing)
			}
		}
	}
}

func TestDamagedPageResync(t *testing.T) {
	raw, want := repaginate(t, "noise-s24.oga", false)
	if len(want) < 4 {
		t.Fatalf("fixture has only %d frames", len(want))
	}
	// Corrupt one byte inside the body of the second data page (pages 0
	// and 1 are the headers, page 2 the first frame).
	off := 0
	for i := 0; i < 3; i++ {
		n := int(raw[off+26])
		size := headerLen + n
		for _, l := range raw[off+headerLen : off+headerLen+n] {
			size += int(l)
		}
		off += size
	}
	raw[off+headerLen+40] ^= 0xFF
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tolerant mode failed on a damaged page: %v", err)
		}
	}
	if len(d.Warnings()) == 0 {
		t.Error("no warning about the damaged page")
	}

	// Strict mode refuses.
	d, err = NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		return // damage may already surface at parse time
	}
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			t.Fatal("strict mode silently skipped a damaged page")
		}
		if err != nil {
			return
		}
	}
}
