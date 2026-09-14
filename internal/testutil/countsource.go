package testutil

import "github.com/colespringer/waxflow/container"

// CountingSource wraps a Source and counts the reads served through it, so a
// test can pin what a demuxer asked the source for and not only what it got
// back. A byte count alone cannot see a scan that asks for the same window
// over and over; the read count can.
type CountingSource struct {
	Src   container.Source
	Reads int
	Bytes int64
}

func (c *CountingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Src.ReadAt(p, off)
	c.Reads++
	c.Bytes += int64(n)
	return n, err
}

func (c *CountingSource) Size() int64 { return c.Src.Size() }

// Reset zeroes the counters, so one phase of a read can be measured alone.
func (c *CountingSource) Reset() { c.Reads, c.Bytes = 0, 0 }
