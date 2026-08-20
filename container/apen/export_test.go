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
