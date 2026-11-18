package flacn

import "github.com/colespringer/waxflow/codec/flac"

// MaxResyncForTest is the bound on a scan for a frame header, and
// MaxFrameLenForTest the bound on a frame.
const (
	MaxResyncForTest   = maxResync
	MaxFrameLenForTest = maxFrameLen
	MaxHeldForTest     = maxHeld
)

// FirstFrameForTest is the offset of the first audio frame.
func (d *Demuxer) FirstFrameForTest() int64 { return d.firstFrame }

// ResidentForTest is how many bytes the read window holds.
func (d *Demuxer) ResidentForTest() int {
	n, _ := d.w.Resident()
	return n
}

// BisectForTest runs a seek's bisection to sample and nothing behind it, and
// returns the offset of the frame it hands the walk.
func (d *Demuxer) BisectForTest(sample int64) (int64, bool) {
	off, _, ok := d.bisect(sample + d.origin)
	return off, ok
}

// WalkFromForTest runs a seek's walk to sample from the frame at off, as one
// that may start again further back, and reports whether it landed.
func (d *Demuxer) WalkFromForTest(off, sample int64) (int64, bool, error) {
	fi, err := flac.ParseFrameHeader(d.w.BytesAt(off, flac.MaxFrameHeaderLen))
	if err != nil {
		return 0, false, err
	}
	landed, how, err := d.walkTo(off, fi, nil, sample+d.origin, false, true)
	return landed - d.origin, how == lands || how == short || how == ended, err
}
