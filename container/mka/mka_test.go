package mka

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
)

// TestParseBlockLacing exercises every lacing mode's frame split. ffmpeg writes
// unlaced blocks, so the differential corpus never reaches these paths; fixed
// lacing in particular was rejecting every multi-frame block before this test.
// Each block is track 1, timestamp 0, then the lacing bytes and frame data.
func TestParseBlockLacing(t *testing.T) {
	cases := []struct {
		name  string
		block []byte
		sizes []int
	}{
		{
			"none", // flags 0x00: one frame is the whole remainder
			[]byte{0x81, 0, 0, 0x00, 'x', 'y', 'z'},
			[]int{3},
		},
		{
			"fixed", // flags 0x04, count 2 -> three 2-byte frames
			[]byte{0x81, 0, 0, 0x04, 0x02, 'a', 'a', 'b', 'b', 'c', 'c'},
			[]int{2, 2, 2},
		},
		{
			"xiph", // flags 0x02, count 2, sizes 2 and 3, last is the rest
			[]byte{0x81, 0, 0, 0x02, 0x02, 0x02, 0x03, 0, 0, 0, 0, 0, 0},
			[]int{2, 3, 1},
		},
		{
			"ebml", // flags 0x06, count 2, first size 2, delta +1 (size 3), rest
			[]byte{0x81, 0, 0, 0x06, 0x02, 0x82, 0xC0, 0, 0, 0, 0, 0, 0},
			[]int{2, 3, 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := srcwin.New(container.BytesSource(c.block), int64(len(c.block)), "test")
			bh, err := parseBlock(&w, 0, int64(len(c.block)))
			if err != nil {
				t.Fatalf("parseBlock: %v", err)
			}
			if bh.track != 1 {
				t.Errorf("track = %d, want 1", bh.track)
			}
			if len(bh.frames) != len(c.sizes) {
				t.Fatalf("frames = %d, want %d", len(bh.frames), len(c.sizes))
			}
			off := bh.frames[0].off
			for i, f := range bh.frames {
				if f.size != c.sizes[i] {
					t.Errorf("frame %d size = %d, want %d", i, f.size, c.sizes[i])
				}
				if f.off != off {
					t.Errorf("frame %d off = %d, want %d (contiguous)", i, f.off, off)
				}
				off += int64(f.size)
			}
			if off != int64(len(c.block)) {
				t.Errorf("frames end at %d, block ends at %d", off, len(c.block))
			}
		})
	}
}

// TestNsToSamples pins the round-to-nearest conversion and, crucially, that a
// long or crafted duration does not overflow the ns*rate product (a naive
// multiply wraps past int64 around a day of 48 kHz audio).
func TestNsToSamples(t *testing.T) {
	cases := []struct {
		ns   int64
		rate int
		want int64
	}{
		{0, 48000, 0},
		{-1, 48000, 0},
		{6_500_000, 48000, 312},                 // Opus pre-skip round-trips exactly
		{1_000_000, 44100, 44},                  // 1 ms rounds down
		{500_000_000, 48000, 24000},             // half a second
		{3_600_000_000_000, 48000, 172_800_000}, // one hour
		// 100 hours: a naive ns*rate is ~1.7e19 and overflows int64; the
		// split-second form must still return the exact count.
		{360_000_000_000_000, 48000, 17_280_000_000},
	}
	for _, c := range cases {
		if got := nsToSamples(c.ns, c.rate); got != c.want {
			t.Errorf("nsToSamples(%d, %d) = %d, want %d", c.ns, c.rate, got, c.want)
		}
	}
}

// TestWalkElementsHugeSizeClamps checks that an element whose declared size
// dwarfs the buffer is clamped to the remaining bytes rather than sliced past
// the end. The size vint below decodes to 2^32-1, which a bare int() would wrap
// negative on a 32-bit build, panicking the slice.
func TestWalkElementsHugeSizeClamps(t *testing.T) {
	// ID 0x80 (1-byte), size 0x08 FF FF FF FF (5-byte vint = 0xFFFFFFFF), then a
	// three-byte body.
	buf := []byte{0x80, 0x08, 0xFF, 0xFF, 0xFF, 0xFF, 'a', 'b', 'c'}
	var seen []byte
	err := walkElements(buf, func(id uint32, data []byte) error {
		if id != 0x80 {
			t.Errorf("id = %#x, want 0x80", id)
		}
		seen = data
		return nil
	})
	if err != nil {
		t.Fatalf("walkElements: %v", err)
	}
	if string(seen) != "abc" {
		t.Errorf("clamped data = %q, want %q", seen, "abc")
	}
}
