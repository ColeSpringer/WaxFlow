package adts

// MaxResyncForTest is the bound on a scan for the next frame.
const MaxResyncForTest = maxResync

// FirstFrameForTest is the offset of the first audio frame.
func (d *Demuxer) FirstFrameForTest() int64 { return d.firstFrame }

// ResidentForTest is how many bytes the read window holds.
func (d *Demuxer) ResidentForTest() int {
	n, _ := d.w.Resident()
	return n
}
