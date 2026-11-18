package apen

// Test-only seams exposed to the external apen_test package.

// ReserveEntriesForTest is the seek-table reservation policy. It is checked
// directly rather than through a written file because the sizes that matter
// are ones no test would write: the answer for a stream that will not state
// its length is a table for every block the header can count.
func ReserveEntriesForTest(samples int64, blocksPerFrame int) int {
	return reserveEntries(samples, blocksPerFrame)
}

// MaxSeekEntriesForTest is the cap that reservation is held under.
const MaxSeekEntriesForTest = maxSeekEntries

// MaxJunkForTest is the bound on the scan for the file header.
const MaxJunkForTest = maxJunk

// BaseForTest is the offset the file header was found at.
func (d *Demuxer) BaseForTest() int64 { return d.base }

// ResidentForTest is how many bytes the read window holds.
func (d *Demuxer) ResidentForTest() int {
	n, _ := d.w.Resident()
	return n
}
