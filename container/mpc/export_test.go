package mpc

import "github.com/colespringer/waxflow/codec/musepack"

// Test-only seams exposed to the external mpc_test package.

// SeekTableForTest parses the seek table the stream carries and returns its
// entries as byte offsets with its spacing exponent over the block spacing, or
// nil when there is none. The table is never trusted for positions, so the
// parse lives here, for the cross-check against the walk: an entry count, the
// exponent, two absolute positions, then Golomb-coded second differences with
// the reference's own sign rule (an odd code is negative), all in bits.
func (d *Demuxer) SeekTableForTest() ([]int64, int) {
	if d.sv8.stHead < 0 {
		return nil, 0
	}
	blk, ok := d.sv8Header(d.sv8.stHead)
	if !ok || blk.key != "ST" {
		return nil, 0
	}
	p := d.w.BytesAt(d.sv8.stHead+int64(blk.hdrLen), int(blk.payload))
	r := musepack.NewBitReader(p)
	n, ok := r.Varint()
	if !ok {
		return nil, 0
	}
	exp := int(r.Bits(4))
	var table []int64
	var last [2]int64
	for i := uint64(0); i < n; i++ {
		var v int64
		if i < 2 {
			pos, ok := r.Varint()
			if !ok {
				return nil, 0
			}
			v = (int64(pos) + d.base) * 8
		} else {
			code, ok := r.Golomb(12)
			if !ok {
				return nil, 0
			}
			if code&1 != 0 {
				code = -(code &^ 1)
			}
			v = int64(code)<<2 + 2*last[(i-1)&1] - last[i&1]
		}
		last[i&1] = v
		table = append(table, v/8)
	}
	return table, exp
}

// BlockOffsetsForTest walks every audio block and returns its offset.
func (d *Demuxer) BlockOffsetsForTest() ([]int64, error) {
	out := make([]int64, 0, d.total)
	for b := int64(0); b < d.total; b++ {
		off, err := d.sv8BlockAt(b)
		if err != nil {
			return nil, err
		}
		out = append(out, off)
	}
	return out, nil
}

// TotalForTest is how many packets the stream delivers.
func (d *Demuxer) TotalForTest() int64 { return d.total }

// ScannedForTest is how far the seek scanner has walked: frames for SV7,
// blocks for SV8.
func (d *Demuxer) ScannedForTest() int64 {
	if d.cfg.StreamVersion == 7 {
		return d.sv7.scanned
	}
	return int64(len(d.sv8.rng)) - 1
}

// RetainForTest runs the block table's retention over a run of offsets and
// returns what it kept and the stride it settled on.
func RetainForTest(offsets []int64) ([]int64, uint) {
	var s sv8Stream
	for _, off := range offsets {
		s.retain(off)
		s.present++
	}
	return s.blocks, s.stride
}

// SetIdxMinStatesForTest lowers the snapshot threshold so a committed-size
// fixture can exercise the index round trip; it returns a restore function.
func SetIdxMinStatesForTest(n int) func() {
	old := idxMinStates
	idxMinStates = n
	return func() { idxMinStates = old }
}

// SV8HeaderForTest reads the packet header at off.
func (d *Demuxer) SV8HeaderForTest(off int64) (SV8BlockForTest, bool) {
	blk, ok := d.sv8Header(off)
	return SV8BlockForTest{Key: blk.key, HdrLen: blk.hdrLen, Payload: blk.payload}, ok
}

// SV8BlockForTest is one packet header as the tests see it.
type SV8BlockForTest struct {
	Key     string
	HdrLen  int
	Payload int64
}

// Caps the tests refer to.
const (
	MaxHeaderPacketForTest    = maxHeaderPacket
	MaxSeekEntriesKeptForTest = maxSeekEntriesKept
	CheckpointFramesForTest   = checkpointFrames
	SynthWarmupForTest        = synthWarmup
)
