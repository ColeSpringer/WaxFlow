package mpc_test

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
)

// sv7Bit is bit i of an SV7 stream in reading order: little-endian 32-bit
// words, each read from its most significant bit.
func sv7Bit(b []byte, i int64) byte { return b[(i>>3)^3] >> (7 - i&7) & 1 }

// sv7Field is the n-bit field at bit at of an SV7 stream.
func sv7Field(b []byte, at int64, n int) int64 {
	v := int64(0)
	for i := range int64(n) {
		v = v<<1 | int64(sv7Bit(b, at+i))
	}
	return v
}

// sv7Pack is bits, one a byte in reading order, as an SV7 stream: zero bits
// to a whole word, each word stored little-endian.
func sv7Pack(bits []byte) []byte {
	for len(bits)%32 != 0 {
		bits = append(bits, 0)
	}
	out := make([]byte, len(bits)/8)
	for i, v := range bits {
		out[(i>>3)^3] |= v << (7 - i&7)
	}
	return out
}

// longSV7 is seek-sv7.mpc with its run of frames repeated k times bit for
// bit, the header's frame count raised to match, and the last-frame field and
// decay frame behind it as they were. The committed SV7 fixtures are each
// under a window, so this is the stream a walk over windows reads.
func longSV7(t testing.TB, k int) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "seek-sv7.mpc"))
	if err != nil {
		t.Fatal(err)
	}
	const firstFrame = 200 // the bit the first frame's length field starts at
	frames := int64(binary.LittleEndian.Uint32(raw[4:]))
	end := int64(firstFrame)
	for range frames {
		end += 20 + sv7Field(raw, end, 20)
	}
	bits := make([]byte, 8*len(raw))
	for i := range bits {
		bits[i] = sv7Bit(raw, int64(i))
	}
	out := append(make([]byte, 0, firstFrame+k*int(end-firstFrame)+len(bits)), bits[:firstFrame]...)
	for range k {
		out = append(out, bits[firstFrame:end]...)
	}
	b := sv7Pack(append(out, bits[end:]...))
	binary.LittleEndian.PutUint32(b[4:], uint32(frames*int64(k)))
	return b
}

// readTwice is how many bytes reads longer than 128 bytes read again; the
// trailer probes read a few bytes through a window of their own.
func readTwice(spans [][2]int64) int64 {
	var held [][2]int64
	twice := int64(0)
	for _, s := range spans {
		if s[1]-s[0] <= 128 {
			continue
		}
		for _, h := range held {
			twice += max(min(s[1], h[1])-max(s[0], h[0]), 0)
		}
		held = append(held, s)
	}
	return twice
}

// TestALinearSV7ReadReadsEachByteOnce: reading every packet of an SV7 stream
// reads each byte once, a read a window: the packet path trims to the word
// its frame's bits start in, where its reads start.
func TestALinearSV7ReadReadsEachByteOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  func(t *testing.T) []byte
	}{
		{"inside-mp7.mpc", func(t *testing.T) []byte {
			raw, err := os.ReadFile(testutil.VectorPath(t, "musepack/inside-mp7.mpc"))
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}},
		{"seek-sv7.mpc's frames twenty times", func(t *testing.T) []byte { return longSV7(t, 20) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw(t)
			src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
			d, err := mpc.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			src.Reset()
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			windows := (len(raw) + srcwin.Chunk - 1) / srcwin.Chunk
			if twice := readTwice(src.Spans); twice > 0 || src.Reads > windows+1 {
				t.Errorf("a read of %d bytes took %d reads and read %d bytes twice, want each byte once in at most %d: %v",
					len(raw), src.Reads, twice, windows+1, src.Spans)
			}
		})
	}
}

// TestTheSV7WalkReadsEachByteOnce: the open's walk of a stream of many
// windows reads no byte twice. It trims at each checkpoint to the word the
// checkpoint's length field starts in, where its read starts.
func TestTheSV7WalkReadsEachByteOnce(t *testing.T) {
	raw := longSV7(t, 40)
	src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
	d, err := mpc.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; len(d.Warnings()) != 0 || tr.Samples != 40*80*1152 {
		t.Fatalf("the long stream opened at %d samples with warnings %v, want 40 times seek-sv7.mpc's 80 frames and none", tr.Samples, d.Warnings())
	}
	// The walk hops each window's frames and starts the next window where the
	// next length field is, so it reads each window once; a field across a
	// window's end extends it, and the checkpoint trims behind that are what
	// this holds. The stream must reach a trim: a read that extends the last,
	// with a window read after it.
	var windows [][2]int64
	for _, s := range src.Spans {
		if s[1]-s[0] > 128 {
			windows = append(windows, s)
		}
	}
	reached := false
	for i := 1; i+1 < len(windows); i++ {
		reached = reached || windows[i][0] == windows[i-1][1]
	}
	if !reached {
		t.Fatalf("the open read %v: no window read extends the last with one behind it, so the layout no longer reaches a checkpoint trim", src.Spans)
	}
	if twice := readTwice(src.Spans); twice > 0 {
		t.Errorf("the open of %d bytes read %d bytes twice: %v", len(raw), twice, src.Spans)
	}
}
