package mpc_test

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
)

// Hand-built streams for the shapes no encoder writes, and the helpers the
// demuxer tests share.

func repoPath(rel ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, rel...)...)
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(repoPath("container", "mpc", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func codecFixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(repoPath("codec", "musepack", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func open(t testing.TB, raw []byte, strict bool) *mpc.Demuxer {
	t.Helper()
	d, err := mpc.NewDemuxer(container.BytesSource(raw), &mpc.DemuxerOptions{Strict: strict})
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	return d
}

// readAll returns every packet, copied.
func readAll(t testing.TB, d *mpc.Demuxer) []container.Packet {
	t.Helper()
	var out []container.Packet
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("packet %d: %v", len(out), err)
		}
		out = append(out, container.Packet{Track: pkt.Track, Packet: codec.Packet{
			Data: append([]byte(nil), pkt.Data...), PTS: pkt.PTS, Dur: pkt.Dur, Sync: pkt.Sync,
		}})
	}
}

// decodeAll decodes a stream's packets and returns the raw timeline,
// interleaved.
func decodeAll(t testing.TB, d *mpc.Demuxer) []float32 {
	t.Helper()
	cfg, err := musepack.ParseConfig(d.Tracks()[0].CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := musepack.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	var out []float32
	emit := func(b *audio.Buffer) error {
		out = append(out, testutil.InterleaveF(b)...)
		return nil
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if err := dec.Decode(pkt.Data, emit); err != nil {
			t.Fatalf("decoding packet %d: %v", i, err)
		}
	}
}

// bitWriter packs MSB-first fields.
type bitWriter struct {
	buf  []byte
	bits int
}

func (w *bitWriter) write(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.bits&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.buf[w.bits>>3] |= 1 << (7 - uint(w.bits&7))
		}
		w.bits++
	}
}

// bitAt reads bit i of b, MSB first.
func bitAt(b []byte, i int) uint64 {
	if i>>3 >= len(b) {
		return 0
	}
	return uint64(b[i>>3] >> (7 - uint(i&7)) & 1)
}

// sv7Header is what the hand-built SV7 header carries.
type sv7Header struct {
	frames               uint32
	ms                   bool
	maxBand, profile     int
	freq                 int
	titleGain, titlePeak uint16
	albumGain, albumPeak uint16
	gapless              bool
	last                 int
	encVersion           byte
	pns                  bool
}

// sv7Frame is one hand-built frame's bit content.
type sv7Frame struct {
	bits []byte // MSB first
	n    int    // bit count
}

// randomFrame is a frame of n pseudo-random bits.
func randomFrame(seed, n int) sv7Frame {
	f := sv7Frame{n: n}
	var w bitWriter
	x := uint64(seed)*6364136223846793005 + 1442695040888963407
	for i := 0; i < n; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		w.write(x>>63, 1)
	}
	f.bits = w.buf
	return f
}

// buildSV7 lays a header, frames, the 11-bit tail field and an optional
// decay frame out as the format has them: little-endian words read MSB
// first, the first frame's length field at bit 200. tail < 0 omits the tail
// field.
func buildSV7(h sv7Header, frames []sv7Frame, tail int, decay *sv7Frame) []byte {
	var w bitWriter
	w.write(0, 32) // the magic's word, filled below
	w.write(uint64(h.frames), 32)
	w.write(0, 1) // intensity stereo
	if h.ms {
		w.write(1, 1)
	} else {
		w.write(0, 1)
	}
	w.write(uint64(h.maxBand), 6)
	w.write(uint64(h.profile), 4)
	w.write(0, 2) // link
	w.write(uint64(h.freq), 2)
	w.write(0, 16) // estimated peak
	w.write(uint64(h.titleGain), 16)
	w.write(uint64(h.titlePeak), 16)
	w.write(uint64(h.albumGain), 16)
	w.write(uint64(h.albumPeak), 16)
	if h.gapless {
		w.write(1, 1)
	} else {
		w.write(0, 1)
	}
	w.write(uint64(h.last), 11)
	w.write(0, 1)  // fast seek
	w.write(0, 19) // unused
	w.write(uint64(h.encVersion), 8)
	if w.bits != 200 {
		panic("header layout")
	}
	put := func(f sv7Frame) {
		w.write(uint64(f.n), 20)
		for i := 0; i < f.n; i++ {
			w.write(bitAt(f.bits, i), 1)
		}
	}
	for _, f := range frames {
		put(f)
	}
	if tail >= 0 {
		w.write(uint64(tail), 11)
	}
	if decay != nil {
		put(*decay)
	}
	for len(w.buf)%4 != 0 {
		w.buf = append(w.buf, 0)
	}
	// Logical to physical: each word's bytes reversed, the magic unswapped.
	out := make([]byte, len(w.buf))
	for i := 4; i < len(w.buf); i++ {
		out[(i&^3)|(3-i&3)] = w.buf[i]
	}
	copy(out, "MP+")
	out[3] = 7
	if h.pns {
		out[3] |= 0x10
	}
	return out
}

// varint encodes an SV8 variable-length size.
func varint(v uint64) []byte {
	var out []byte
	for shift := 63; shift > 0; shift -= 7 {
		if v>>uint(shift) != 0 || len(out) > 0 {
			out = append(out, byte(v>>uint(shift))&0x7F|0x80)
		}
	}
	return append(out, byte(v)&0x7F)
}

// sv8Packet frames a payload under a key: the size counts the key, the size
// field and the payload.
func sv8Packet(key string, payload []byte) []byte {
	size := uint64(2 + len(payload))
	for {
		n := uint64(len(varint(size + uint64(len(varint(size))))))
		if len(varint(size+n)) == int(n) {
			size += n
			break
		}
		size++
	}
	out := append([]byte(key), varint(size)...)
	return append(out, payload...)
}

// sv8Header is what the hand-built SH carries.
type sv8Header struct {
	count, silence uint64
	freq, maxBand  int
	channels       int
	ms             bool
	blockPwr       int
}

// shPayload builds the stream header payload with its CRC.
func shPayload(h sv8Header) []byte {
	var w bitWriter
	w.write(8, 8)
	for _, b := range varint(h.count) {
		w.write(uint64(b), 8)
	}
	for _, b := range varint(h.silence) {
		w.write(uint64(b), 8)
	}
	w.write(uint64(h.freq), 3)
	w.write(uint64(h.maxBand-1), 5)
	w.write(uint64(h.channels-1), 4)
	if h.ms {
		w.write(1, 1)
	} else {
		w.write(0, 1)
	}
	w.write(uint64(h.blockPwr/2), 3)
	body := w.buf
	out := binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(body))
	return append(out, body...)
}

// rgPayload builds a replay gain packet.
func rgPayload(version byte, tg, tp, ag, ap uint16) []byte {
	out := []byte{version}
	for _, v := range []uint16{tg, tp, ag, ap} {
		out = binary.BigEndian.AppendUint16(out, v)
	}
	return out
}

// eiPayload builds an encoder info packet.
func eiPayload(profile int, pns bool, major, minor, build byte) []byte {
	var w bitWriter
	w.write(uint64(profile), 7)
	if pns {
		w.write(1, 1)
	} else {
		w.write(0, 1)
	}
	w.write(uint64(major), 8)
	w.write(uint64(minor), 8)
	w.write(uint64(build), 8)
	return w.buf
}

// ctPayload builds a chapter packet: the start sample, gain and peak, then
// raw APEv2 items.
func ctPayload(t testing.TB, sample uint64, title string) []byte {
	t.Helper()
	tag, err := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: title}})
	if err != nil {
		t.Fatal(err)
	}
	items := tag[apev2.FooterLen : len(tag)-apev2.FooterLen]
	out := append(varint(sample), 0, 0, 0, 0)
	return append(out, items...)
}

// apBlock is an audio block payload of n arbitrary bytes; the demuxer never
// decodes them.
func apBlock(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*37 + 11)
	}
	return b
}

// stPayload builds an empty seek table.
func stPayload() []byte { return append(varint(0), 0) }

// buildSV8 concatenates the magic and packets.
func buildSV8(packets ...[]byte) []byte {
	out := []byte("MPCK")
	for _, p := range packets {
		out = append(out, p...)
	}
	return out
}

// framesFor is the frame total the reference decodes for a sample count.
func framesFor(count int) int {
	return (count + musepack.SynthDelay + musepack.FrameLength - 1) / musepack.FrameLength
}
