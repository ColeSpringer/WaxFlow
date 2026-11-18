package testutil

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/codec/wavpack"
)

// WavPackBlockStarts are the offsets of raw's blocks, by each header's own
// length, and where the last one ends.
func WavPackBlockStarts(t testing.TB, raw []byte) []int {
	t.Helper()
	var starts []int
	off := 0
	for off+wavpack.BlockHeaderLen <= len(raw) && wavpack.SyncOK(raw[off:]) {
		h, err := wavpack.ParseBlockHeader(raw[off:])
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, off)
		off += int(h.Size)
	}
	return append(starts, off)
}

// WavPackAudioBlocks are the offsets of raw's audio blocks, and where each
// ends.
func WavPackAudioBlocks(t testing.TB, raw []byte) (starts, ends []int) {
	t.Helper()
	all := WavPackBlockStarts(t, raw)
	for k := 0; k+1 < len(all); k++ {
		if h, err := wavpack.ParseBlockHeader(raw[all[k]:]); err == nil && h.Audio() {
			starts, ends = append(starts, all[k]), append(ends, all[k+1])
		}
	}
	return starts, ends
}

// WavPackWithout is raw with its blocks ks gone.
func WavPackWithout(t testing.TB, raw []byte, ks ...int) []byte {
	t.Helper()
	starts := WavPackBlockStarts(t, raw)
	var out []byte
	for k := 0; k+1 < len(starts); k++ {
		if !slices.Contains(ks, k) {
			out = append(out, raw[starts[k]:starts[k+1]]...)
		}
	}
	return append(out, raw[starts[len(starts)-1]:]...)
}

// WavPackWithIndex is raw with block k's index set to index, its checksum
// restated.
func WavPackWithIndex(t testing.TB, raw []byte, k int, index uint32) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	starts := WavPackBlockStarts(t, out)
	binary.LittleEndian.PutUint32(out[starts[k]+16:], index)
	wavpack.UpdateBlockChecksum(out[starts[k]:starts[k+1]])
	return out
}

// WavPackOtherShape is an audio block that is mono and 8-bit, at index and of
// 3584 samples: a shape no stream the tests build is.
func WavPackOtherShape(index uint32) []byte {
	b := make([]byte, 56)
	copy(b, "wvpk")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	binary.LittleEndian.PutUint16(b[8:], 0x410)
	binary.LittleEndian.PutUint32(b[16:], index)
	binary.LittleEndian.PutUint32(b[20:], 3584)
	binary.LittleEndian.PutUint32(b[24:], 4)
	return b
}
