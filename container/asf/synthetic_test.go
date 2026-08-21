package asf_test

// Hand-built ASF files, for the shapes ffmpeg's muxer never writes. Two of
// them are ordinary in files from Windows Media Encoder and unreachable
// through the corpus: a packet whose payload area stops short of the packet
// because it is padded, and a compressed payload, where several whole media
// objects share one payload behind a single presentation time and a delta.
// Both are implemented, so both need a file that proves it, or the code is
// unverified rather than merely untested.

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
)

// builder assembles a minimal ASF file: one header carrying File Properties
// and one Stream Properties object, then a Data Object of fixed-size packets
// the caller lays out.
type builder struct {
	rate       int
	channels   int
	blockAlign int
	packetLen  int
	prerollMS  uint64
	playHNS    uint64
	streamNum  byte
	packets    [][]byte
}

func newBuilder() *builder {
	return &builder{rate: 44100, channels: 2, blockAlign: 743, packetLen: 512, prerollMS: 3100, streamNum: 1}
}

// object writes one ASF object: its GUID, its total size, and its body.
func object(id []byte, body []byte) []byte {
	out := make([]byte, 24, 24+len(body))
	copy(out, id)
	le.PutUint64(out[16:], uint64(24+len(body)))
	return append(out, body...)
}

func (b *builder) fileProperties() []byte {
	body := make([]byte, 80)
	le.PutUint64(body[32:], uint64(len(b.packets)))
	le.PutUint64(body[40:], b.playHNS)
	le.PutUint64(body[56:], b.prerollMS)
	le.PutUint32(body[64:], 2) // seekable
	le.PutUint32(body[68:], uint32(b.packetLen))
	le.PutUint32(body[72:], uint32(b.packetLen))
	return object(guidFileProperties, body)
}

func (b *builder) streamProperties() []byte {
	wfx := make([]byte, 18)
	le.PutUint16(wfx, 0x0161) // WMAV2
	le.PutUint16(wfx[2:], uint16(b.channels))
	le.PutUint32(wfx[4:], uint32(b.rate))
	le.PutUint32(wfx[8:], 16000)
	le.PutUint16(wfx[12:], uint16(b.blockAlign))
	le.PutUint16(wfx[14:], 16)
	body := make([]byte, 54)
	copy(body, guidAudioMedia)
	le.PutUint32(body[40:], uint32(len(wfx)))
	le.PutUint16(body[48:], uint16(b.streamNum))
	return object(guidStreamProperties, append(body, wfx...))
}

// guidAudioMedia is spelled here too, so the synthetic files do not borrow the
// stream type from the package they exercise.
var guidAudioMedia = []byte{0x40, 0x9E, 0x69, 0xF8, 0x4D, 0x5B, 0xCF, 0x11, 0xA8, 0xFD, 0x00, 0x80, 0x5F, 0x5C, 0x44, 0x2B}

func (b *builder) build() []byte {
	children := append(b.fileProperties(), b.streamProperties()...)
	header := make([]byte, 30, 30+len(children))
	copy(header, guidHeader)
	le.PutUint64(header[16:], uint64(30+len(children)))
	le.PutUint32(header[24:], 2)
	header[28], header[29] = 1, 2
	header = append(header, children...)

	data := make([]byte, 50)
	copy(data, guidData)
	le.PutUint64(data[16:], uint64(50+len(b.packets)*b.packetLen))
	le.PutUint64(data[40:], uint64(len(b.packets)))
	le.PutUint16(data[48:], 0x0101)
	for _, p := range b.packets {
		data = append(data, p...)
	}
	return append(header, data...)
}

// packet lays a single-payload packet. The payload's own length is implied by
// where the padding starts, which is the field this exercises: a reader that
// ignores the padding length hands the padding out as part of the object.
func (b *builder) packet(sendMS uint32, objNum byte, objOff, objSize, presMS uint32, data []byte) *builder {
	p := make([]byte, 0, b.packetLen)
	// Length Type Flags: single payload, no sequence, dword padding length,
	// packet length implied by the fixed size.
	p = append(p, 0x18)
	// Property Flags: byte replicated-data length, dword offset, byte media
	// object number, byte stream number.
	p = append(p, 0x5D)
	body := make([]byte, 0, 32)
	body = append(body, b.streamNum, objNum)
	body = appendU32(body, objOff)
	body = append(body, 8)
	body = appendU32(body, objSize)
	body = appendU32(body, presMS)
	body = append(body, data...)
	// padding length (4) + send time (4) + duration (2) sit between the flags
	// and the payload.
	pad := b.packetLen - len(p) - 10 - len(body)
	if pad < 0 {
		panic("payload does not fit the packet")
	}
	p = appendU32(p, uint32(pad))
	p = appendU32(p, sendMS)
	p = append(p, 0, 0)
	p = append(p, body...)
	p = append(p, make([]byte, pad)...)
	b.packets = append(b.packets, p)
	return b
}

// compressedPacket lays a packet whose single payload is compressed: one byte
// of replicated data holding the presentation-time delta, the offset field
// carrying the first sub-object's presentation time, and a run of
// length-prefixed sub-objects behind it.
func (b *builder) compressedPacket(sendMS, presMS uint32, deltaMS byte, subs [][]byte) *builder {
	p := []byte{0x18, 0x5D}
	body := make([]byte, 0, 64)
	body = append(body, b.streamNum, 1)
	body = appendU32(body, presMS)
	body = append(body, 1, deltaMS)
	for _, s := range subs {
		body = append(body, byte(len(s)))
		body = append(body, s...)
	}
	pad := b.packetLen - len(p) - 10 - len(body)
	if pad < 0 {
		panic("payload does not fit the packet")
	}
	p = appendU32(p, uint32(pad))
	p = appendU32(p, sendMS)
	p = append(p, 0, 0)
	p = append(p, body...)
	p = append(p, make([]byte, pad)...)
	b.packets = append(b.packets, p)
	return b
}

func appendU32(b []byte, v uint32) []byte {
	var buf [4]byte
	le.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

// collect reads every packet out of a built file.
func collect(t *testing.T, raw []byte) []container.Packet {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []container.Packet
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, container.Packet{Track: pkt.Track, Packet: pkt.Packet})
		out[len(out)-1].Data = append([]byte(nil), pkt.Data...)
	}
}

// TestPaddedPacketsStopAtTheirPayload is the padding-length check. Every
// packet here is mostly padding, so a reader that took the payload as
// everything up to the end of the packet would hand out an object hundreds of
// bytes too long.
func TestPaddedPacketsStopAtTheirPayload(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 16
	b.playHNS = b.prerollMS*10_000 + 3*10_000_000
	want := [][]byte{
		{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		{17, 18, 19, 20},
		{21},
	}
	for i, data := range want {
		b.packet(uint32(i*20), byte(i+1), 0, uint32(len(data)), uint32(b.prerollMS)+uint32(i*20), data)
	}
	got := collect(t, b.build())
	if len(got) != len(want) {
		t.Fatalf("%d objects, want %d", len(got), len(want))
	}
	for i, pkt := range got {
		if string(pkt.Data) != string(want[i]) {
			t.Errorf("object %d = %v, want %v", i, pkt.Data, want[i])
		}
	}
}

// TestCompressedPayloadSplitsIntoObjects covers the payload form where one
// payload carries several whole media objects, each a fixed presentation-time
// delta after the last. Nothing in the ffmpeg corpus writes it, and a reader
// that treated the replicated data as an ordinary descriptor would emit one
// object of the concatenated bytes at a nonsense presentation time.
func TestCompressedPayloadSplitsIntoObjects(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 8
	b.prerollMS = 1000
	b.playHNS = b.prerollMS*10_000 + 3*10_000_000
	subs := [][]byte{
		{0xA0, 0xA1, 0xA2},
		{0xB0, 0xB1, 0xB2, 0xB3},
		{0xC0},
	}
	const delta = 20
	b.compressedPacket(0, uint32(b.prerollMS), delta, subs)
	got := collect(t, b.build())
	if len(got) != len(subs) {
		t.Fatalf("%d objects, want %d one per sub-payload", len(got), len(subs))
	}
	for i, pkt := range got {
		if string(pkt.Data) != string(subs[i]) {
			t.Errorf("object %d = %v, want %v", i, pkt.Data, subs[i])
		}
		// Each sub-object sits one delta after the last, measured from the
		// payload's own presentation time.
		want := int64(i) * delta * int64(b.rate) / 1000
		if pkt.PTS != want {
			t.Errorf("object %d is at %d, want %d", i, pkt.PTS, want)
		}
	}
}

// TestPayloadsForOtherStreamsAreSkipped covers the interleave: an ASF file
// carries every stream's payloads in one packet run, and only the selected
// stream's are ours.
func TestPayloadsForOtherStreamsAreSkipped(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.streamNum = 3
	b.playHNS = b.prerollMS*10_000 + 2*10_000_000
	b.packet(0, 1, 0, 4, uint32(b.prerollMS), []byte{1, 2, 3, 4})
	// Same shape, a different stream number: the reader must step over it.
	b.streamNum = 4
	b.packet(20, 2, 0, 4, uint32(b.prerollMS)+20, []byte{9, 9, 9, 9})
	b.streamNum = 3
	b.packet(40, 3, 0, 4, uint32(b.prerollMS)+40, []byte{5, 6, 7, 8})

	got := collect(t, b.build())
	if len(got) != 2 {
		t.Fatalf("%d objects, want the 2 belonging to stream 3", len(got))
	}
	if got[0].Data[0] != 1 || got[1].Data[0] != 5 {
		t.Errorf("objects are %v and %v; a foreign stream's payload leaked in", got[0].Data, got[1].Data)
	}
}

// TestFragmentsSpanningPacketsReassemble builds the fragmented case by hand so
// the boundary arithmetic is pinned by construction rather than by whatever
// ffmpeg's packer happened to produce.
func TestFragmentsSpanningPacketsReassemble(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 12
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	whole := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	pres := uint32(b.prerollMS)
	b.packet(0, 7, 0, 12, pres, whole[:5])
	b.packet(0, 7, 5, 12, pres, whole[5:9])
	b.packet(0, 7, 9, 12, pres, whole[9:])

	got := collect(t, b.build())
	if len(got) != 1 {
		t.Fatalf("%d objects, want 1 reassembled from 3 fragments", len(got))
	}
	if string(got[0].Data) != string(whole) {
		t.Errorf("object = %v, want %v", got[0].Data, whole)
	}
}

// TestBrokenFragmentRunIsDamage is the other side: a run whose middle fragment
// is missing must not silently concatenate into a short object.
func TestBrokenFragmentRunIsDamage(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 12
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	pres := uint32(b.prerollMS)
	b.packet(0, 7, 0, 12, pres, []byte{1, 2, 3, 4, 5})
	// Jumps straight to the last fragment: offset 9 does not continue offset 5.
	b.packet(0, 7, 9, 12, pres, []byte{10, 11, 12})
	raw := b.build()

	ds, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("strict open: %v", err)
	}
	var strictPkt container.Packet
	if err := ds.ReadPacket(&strictPkt); err == nil {
		t.Fatalf("strict mode handed out %v from a broken fragment run", strictPkt.Data)
	} else if errors.Is(err, io.EOF) {
		t.Fatal("strict mode read a broken fragment run to a clean end")
	}

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
		t.Fatalf("tolerant mode produced %v (%v); the object never completed", pkt.Data, err)
	}
	if len(d.Warnings()) == 0 {
		t.Error("a broken fragment run recorded no warning")
	}
}

// TestAReadFailureIsSticky pins what happens after a strict-mode refusal
// mid-walk. The lookahead runs a packet ahead of what the caller sees, so the
// object that raised the error is not the object about to be handed out: a
// caller that logged the error and read on would otherwise be given data after
// a failure, from a file the demuxer had already refused. A seek clears it,
// because repositioning re-derives every bit of that state.
func TestAReadFailureIsSticky(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	pres := uint32(b.prerollMS)
	b.packet(0, 1, 0, 4, pres, []byte{1, 1, 1, 1})
	b.packet(20, 2, 0, 4, pres+20, []byte{2, 2, 2, 2})
	// A continuation of an object nothing started: damage, and in strict mode
	// a refusal, raised while looking ahead past the second object.
	b.packet(40, 3, 8, 12, pres+40, []byte{9, 9, 9, 9})
	b.packet(60, 4, 0, 4, pres+60, []byte{4, 4, 4, 4})
	b.packet(80, 5, 0, 4, pres+80, []byte{5, 5, 5, 5})

	d, err := asf.NewDemuxer(container.BytesSource(b.build()), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var first error
	for i := 0; i < 8 && first == nil; i++ {
		first = d.ReadPacket(&pkt)
	}
	if first == nil || errors.Is(first, io.EOF) {
		t.Fatalf("strict mode walked past the damaged packet (%v)", first)
	}
	for i := 0; i < 3; i++ {
		if err := d.ReadPacket(&pkt); err == nil {
			t.Fatalf("read %d after a refusal handed out %v", i, pkt.Data)
		} else if err.Error() != first.Error() {
			t.Fatalf("read %d returned %v, want the original %v", i, err, first)
		}
	}
	// Landing past the damage clears it: the failure belonged to one packet,
	// not to the file.
	landed, err := d.SeekSample(0, 80*int64(b.rate)/1000)
	if err != nil {
		t.Fatalf("seek past the damaged packet: %v", err)
	}
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("read after seeking to %d: %v", landed, err)
	}
	if pkt.Data[0] != 4 && pkt.Data[0] != 5 {
		t.Errorf("landed on object %v, want one of the two behind the damage", pkt.Data)
	}
}

// TestSeekSurvivesNonMonotonicSendTimes is a fuzz finding, pinned. Bisection
// needs the send times to rise across the file and a damaged one owes nothing
// of the sort; here they fold back on themselves, so bisection picks a packet
// in the middle for a target at the start. SeekSample's landing check is what
// has to catch it.
func TestSeekSurvivesNonMonotonicSendTimes(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	// Presentation times rise; send times do not, and the one at index 4
	// invites the bisection to jump forward for a target of zero.
	send := []uint32{0, 10, 20, 30, 0, 50, 60, 70}
	for i, s := range send {
		b.packet(s, byte(i+1), 0, 4, uint32(b.prerollMS)+uint32(i*20), []byte{byte(i), 0, 0, 0})
	}
	raw := b.build()

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 1, 100} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target {
			t.Errorf("seek to %d landed at %d", target, landed)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("reading after a seek to %d: %v", target, err)
		}
		if pkt.PTS != landed {
			t.Errorf("seek to %d reported %d but the next packet is at %d", target, landed, pkt.PTS)
		}
	}
}

// TestSeekWithNothingReadableAtTheLanding covers the other half of the same
// finding: when the packets behind the landing hold no whole media object, the
// fetch has run to the end of the data and nothing will follow, so the landing
// reported has to be one no caller can be misled by.
func TestSeekWithNothingReadableAtTheLanding(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	b.packet(0, 1, 0, 4, uint32(b.prerollMS), []byte{1, 2, 3, 4})
	// Two packets of an object that never completes: the tail of the file
	// yields nothing, whatever its send times say.
	for i := 0; i < 6; i++ {
		b.packet(uint32(20+i*20), 2, 0, 999, uint32(b.prerollMS)+uint32(20+i*20), []byte{9, 9, 9, 9})
	}
	d, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 1000, 1 << 40} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target {
			t.Errorf("seek to %d landed at %d", target, landed)
		}
		// The landing reported is where reads resume, so the one object this
		// file does hold has to come back. Reporting a position and then
		// returning EOF from it is the failure this pins.
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Errorf("seek to %d reported %d, then read %v", target, landed, err)
		} else if pkt.PTS != landed {
			t.Errorf("seek to %d reported %d but the next packet is at %d", target, landed, pkt.PTS)
		}
	}
}

// packetRep0 lays a packet whose Replicated Data Length type is 00, so the
// field is absent and the length is zero.
func (b *builder) packetRep0(sendMS uint32, objNum byte, data []byte) *builder {
	p := []byte{0x18, 0x5C}
	body := make([]byte, 0, 32)
	body = append(body, b.streamNum, objNum)
	body = appendU32(body, 0)
	body = append(body, data...)
	pad := b.packetLen - len(p) - 10 - len(body)
	if pad < 0 {
		panic("payload does not fit the packet")
	}
	p = appendU32(p, uint32(pad))
	p = appendU32(p, sendMS)
	p = append(p, 0, 0)
	p = append(p, body...)
	p = append(p, make([]byte, pad)...)
	b.packets = append(b.packets, p)
	return b
}

// TestTruncatedTailIsDamage covers the object still under assembly when the
// packet run ends. Identical damage mid-stream warns, so saying nothing here
// certified a file with its tail audio missing as clean, and the Data Object's
// packet count only catches it when the writer never back-patched one.
func TestTruncatedTailIsDamage(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 3*10_000_000
	pres := uint32(b.prerollMS)
	b.packet(0, 1, 0, 4, pres, []byte{1, 2, 3, 4})
	b.packet(20, 2, 0, 4, pres+20, []byte{2, 2, 2, 2})
	// Starts a 999-byte object and stops: the file ends mid-object.
	b.packet(40, 3, 0, 999, pres+40, []byte{9, 9, 9, 9})
	raw := b.build()

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	n := 0
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("tolerant read: %v", err)
			}
			break
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d objects, want the 2 that completed", n)
	}
	if len(d.Warnings()) == 0 {
		t.Error("a file ending mid-object probed clean")
	}

	ds, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 8; i++ {
		if last = ds.ReadPacket(&pkt); last != nil {
			break
		}
	}
	if last == nil || errors.Is(last, io.EOF) {
		t.Errorf("strict mode walked a file ending mid-object to a clean end (%v)", last)
	}
}

// TestResumeSuppressionExpires bounds the one thing a post-seek landing is
// allowed to step over. A seek lands on a packet boundary, so the tail of the
// object it bisected is not damage, but a suppression that never expired
// swallowed every later fragment error in the file: damage a linear read
// refuses would be accepted on a post-seek read of the same bytes.
func TestResumeSuppressionExpires(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 20*10_000_000
	pres := uint32(b.prerollMS)
	for i := 0; i < 6; i++ {
		b.packet(uint32(i*20), byte(i+1), 0, 4, pres+uint32(i*20), []byte{byte(i), 0, 0, 0})
	}
	// A tail of nothing but stray continuations, so no payload after a
	// landing inside it ever starts an object.
	for i := 6; i < 14; i++ {
		b.packet(uint32(i*20), byte(i+1), 8, 12, pres+uint32(i*20), []byte{9, 9, 9, 9})
	}
	raw := b.build()

	linear := walkErr(t, raw)
	if linear == nil || errors.Is(linear, io.EOF) {
		t.Fatalf("a strict linear walk accepted the stray fragments (%v)", linear)
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(0, 200*int64(b.rate)/1000); err == nil {
		var pkt container.Packet
		var post error
		for i := 0; i < 20; i++ {
			if post = d.ReadPacket(&pkt); post != nil {
				break
			}
		}
		if post == nil || errors.Is(post, io.EOF) {
			t.Errorf("a strict post-seek walk accepted damage the linear walk refused (%v)", post)
		}
	}
}

// walkErr reads a built file to its first error under strict mode.
func walkErr(t *testing.T, raw []byte) error {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		return err
	}
	var pkt container.Packet
	for i := 0; i < 40; i++ {
		if err := d.ReadPacket(&pkt); err != nil {
			return err
		}
	}
	return nil
}

// TestBackwardsPresentationTimesAreDamage covers the clamp on a file whose
// presentation times fold back. Emitting the fold verbatim would hand a
// consumer a packet timeline that runs backwards, and strict has to refuse it
// on every walk: latching the warning before taking it let a second walk of
// the same file read to completion and report itself clean.
func TestBackwardsPresentationTimesAreDamage(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	pres := uint32(b.prerollMS)
	b.packet(0, 1, 0, 4, pres+100, []byte{1, 1, 1, 1})
	b.packet(20, 2, 0, 4, pres+40, []byte{2, 2, 2, 2}) // folds back
	b.packet(40, 3, 0, 4, pres+150, []byte{3, 3, 3, 3})
	raw := b.build()

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	last := int64(-1)
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		if pkt.PTS < last {
			t.Fatalf("emitted %d after %d: the timeline runs backwards", pkt.PTS, last)
		}
		last = pkt.PTS
	}
	if len(d.Warnings()) == 0 {
		t.Error("a file whose presentation times fold back probed clean")
	}

	// Strict refuses, and refuses again after a seek re-walks the same fold.
	ds, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	first := readToErr(ds)
	if first == nil || errors.Is(first, io.EOF) {
		t.Fatalf("strict mode accepted the fold (%v)", first)
	}
	if _, err := ds.SeekSample(0, 0); err != nil {
		t.Logf("seek after the refusal: %v", err)
	}
	if again := readToErr(ds); again == nil || errors.Is(again, io.EOF) {
		t.Errorf("strict mode certified the same file clean on a second walk (%v)", again)
	}
	// The same in tolerant mode: the second walk has to report the fold too,
	// or a caller that seeks before reading is told the file is clean.
	dt, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dt.SeekSample(0, 0); err != nil {
		t.Fatal(err)
	}
	readToErr(dt)
	if len(dt.Warnings()) == 0 {
		t.Error("a walk that began with a seek reported no fold")
	}
}

func readToErr(d *asf.Demuxer) error {
	var pkt container.Packet
	for i := 0; i < 20; i++ {
		if err := d.ReadPacket(&pkt); err != nil {
			return err
		}
	}
	return nil
}

// TestRunUpWhenAnObjectBarelyOutgrowsAPacket pins the gap a size comparison
// alone leaves. A packet carries less payload than its own length, so a media
// object exactly as long as one already spans two, and sizing the run-up
// against the packet length would give a seek no history at all for every
// blockAlign in that band. ffmpeg's muxer picks neither end of it, so only a
// hand-built file reaches this.
func TestRunUpWhenAnObjectBarelyOutgrowsAPacket(t *testing.T) {
	const packetLen, object = 512, 512
	b := newBuilder()
	b.packetLen = packetLen
	b.blockAlign = object
	b.playHNS = b.prerollMS*10_000 + 20*10_000_000
	// 485 bytes is what one packet has room for once its parsing information
	// and payload header are paid for, so each object takes two packets.
	for i := 0; i < 20; i++ {
		whole := make([]byte, object)
		whole[0] = byte(i)
		pres := uint32(b.prerollMS) + uint32(i*20)
		b.packet(uint32(i*20), byte(i+1), 0, object, pres, whole[:485])
		b.packet(uint32(i*20), byte(i+1), 485, object, pres, whole[485:])
	}
	d, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	objectDur := pkt.Dur
	for _, i := range []int64{5, 10, 15} {
		target := i * 20 * int64(b.rate) / 1000
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		if landed > target {
			t.Fatalf("seek to %d landed at %d", target, landed)
		}
		if target-landed < objectDur {
			t.Errorf("seek to %d landed at %d: %d samples of run-up for a %d-sample object",
				target, landed, target-landed, objectDur)
		}
	}
}

// TestASubObjectStormIsBounded covers the one payload form whose entry count
// grows with the packet rather than with a six-bit field: a compressed payload
// splits into as many media objects as it has length-prefixed sub-objects, so
// without a cap a single legal packet expands into hundreds of thousands.
func TestASubObjectStormIsBounded(t *testing.T) {
	b := newBuilder()
	b.packetLen = 1 << 16
	b.blockAlign = 1
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	subs := make([][]byte, (b.packetLen-32)/2)
	for i := range subs {
		subs[i] = []byte{byte(i)}
	}
	b.compressedPacket(0, uint32(b.prerollMS), 0, subs)

	d, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	n := 0
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		n++
	}
	if n == 0 {
		t.Fatal("no objects from a packet full of sub-objects")
	}
	if n >= len(subs) {
		t.Errorf("%d objects from one %d-byte packet; the payload list is unbounded", n, b.packetLen)
	}
}

// TestPayloadWithNoReplicatedData pins a shape this build reports rather than
// interprets. A Replicated Data Length of zero leaves a payload with no media
// object size and no presentation time of its own, and there is no agreed
// reading of it: ffmpeg computes an object size of zero, rejects the fragment
// as out of range ("packet fragment position invalid 0,4 not in 0"), and emits
// nothing. So this emits nothing either, and says why, rather than inventing a
// meaning no other reader assigns.
func TestPayloadWithNoReplicatedData(t *testing.T) {
	b := newBuilder()
	b.blockAlign = 4
	b.playHNS = b.prerollMS*10_000 + 3*10_000_000
	for i := 0; i < 3; i++ {
		b.packetRep0(uint32(i*20), byte(i+1), []byte{byte(i), 2, 3, 4})
	}
	raw := b.build()

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
		t.Fatalf("read = %v, want EOF: nothing here can be reassembled", err)
	}
	if len(d.Warnings()) == 0 {
		t.Error("a file that decodes to nothing probed clean")
	}
	if _, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true}); err == nil {
		ds, _ := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
		if err := ds.ReadPacket(&pkt); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("strict mode accepted a file it can read nothing out of (%v)", err)
		}
	}
}
