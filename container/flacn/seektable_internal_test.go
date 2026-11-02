package flacn

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// seekableStream encodes seconds of a sine through this package's muxer onto a
// destination it can seek, so the stream carries a SEEKTABLE.
func seekableStream(t *testing.T, seconds int) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	src := testutil.Sine(f, seconds*f.Rate, 440, 0.5)
	defer audio.Put(src)
	enc, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: 5})
	if err != nil {
		t.Fatal(err)
	}
	ws := &testutil.MemWriteSeeker{}
	m := NewMuxer(ws, &MuxerOptions{MD5: enc.MD5})
	if err := m.Begin([]container.Track{{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(src.N)}}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Packet: p}) }
	chunk := audio.Get(f, enc.FrameSize())
	defer audio.Put(chunk)
	for off := 0; off < src.N; off += enc.FrameSize() {
		chunk.N = min(enc.FrameSize(), src.N-off)
		audio.CopyFrames(chunk, 0, src, off, chunk.N)
		if err := enc.Encode(chunk, emit); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(tr); err != nil {
		t.Fatal(err)
	}
	return ws.Buf
}

// cutCopy is raw from frame k on, as ffmpeg -ss X -c copy writes one: the
// frames' bytes and numbers untouched, the SEEKTABLE counted from the cut.
func cutCopy(t *testing.T, raw []byte, k int) []byte {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	starts := []int64{d.firstFrame}
	var pkt container.Packet
	var block int64
	for d.ReadPacket(&pkt) == nil {
		starts = append(starts, starts[len(starts)-1]+int64(len(pkt.Data)))
		block = max(block, pkt.Dur)
	}
	cutAt, cutSample := starts[k]-d.firstFrame, int64(k)*block
	out := append([]byte(nil), raw[:d.firstFrame]...)
	for pos := 4; pos < int(d.firstFrame); {
		n := int(out[pos+1])<<16 | int(out[pos+2])<<8 | int(out[pos+3])
		if out[pos]&0x7F == 3 { // SEEKTABLE
			for p := pos + 4; p+18 <= pos+4+n; p += 18 {
				sample, off := binary.BigEndian.Uint64(out[p:]), binary.BigEndian.Uint64(out[p+8:])
				switch {
				case sample == ^uint64(0):
				case int64(sample) < cutSample:
					binary.BigEndian.PutUint64(out[p:], ^uint64(0)) // before the cut: a placeholder
				default:
					binary.BigEndian.PutUint64(out[p:], sample-uint64(cutSample))
					binary.BigEndian.PutUint64(out[p+8:], off-uint64(cutAt))
				}
			}
		}
		if out[pos]&0x80 != 0 {
			break
		}
		pos += 4 + n
	}
	return append(out, raw[starts[k]:]...)
}

// TestSeekTableCountedFromTheCut: a stream-copy cut keeps its frames' numbers
// but counts its SEEKTABLE from the cut, and the table still places a seek
// rather than sending it to bisection.
func TestSeekTableCountedFromTheCut(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(cutCopy(t, seekableStream(t, 120), 300)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.seekTable) < 3 || d.origin == 0 {
		t.Fatalf("%d seek points, origin %d; this cell needs a table and a stream numbered past 0", len(d.seekTable), d.origin)
	}
	pt := d.seekTable[len(d.seekTable)/2]
	at := d.origin + pt.sample + 10
	off, fi, ok := d.seekTableHint(at)
	if !ok || off != d.firstFrame+pt.off || d.num.Start(fi) > at {
		t.Fatalf("the hint for %d: ok %v at %d, want the point at %d", at, ok, off, d.firstFrame+pt.off)
	}
}
