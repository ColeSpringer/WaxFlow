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
	// Low is the lowest offset read since the last Reset, when Reads says
	// there was a read.
	Low int64
	// Log keeps each read's range in Spans, so a test can ask which reads
	// covered a given byte.
	Log   bool
	Spans [][2]int64
}

func (c *CountingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Src.ReadAt(p, off)
	if c.Reads == 0 || off < c.Low {
		c.Low = off
	}
	c.Reads++
	c.Bytes += int64(n)
	if c.Log {
		c.Spans = append(c.Spans, [2]int64{off, off + int64(len(p))})
	}
	return n, err
}

func (c *CountingSource) Size() int64 { return c.Src.Size() }

// Reset zeroes the counters, so one phase of a read can be measured alone.
func (c *CountingSource) Reset() { c.Reads, c.Bytes, c.Low, c.Spans = 0, 0, 0, nil }
