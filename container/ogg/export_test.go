package ogg

import "github.com/colespringer/waxflow/container"

// scanDemuxer is a demuxer over src with nothing parsed, for driving its page
// reads alone.
func scanDemuxer(src container.Source) *Demuxer {
	return &Demuxer{src: src, totalSize: src.Size(), win: newWindow(src)}
}

// HeldForTest is how many bytes the read window holds.
func (d *Demuxer) HeldForTest() int64 { return int64(len(d.win.buf)) }

// SummedForTest is how many bytes the demuxer has fed to the page checksum.
func (d *Demuxer) SummedForTest() int64 { return d.win.summed }
