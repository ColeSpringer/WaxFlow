package asf

// UncarriedForTest replaces the codec's carry check on d's frame count, so a
// test can place a frame that ends on its packet's last bit without laying a
// bitstream that does.
func UncarriedForTest(d *Demuxer, f func(prev, pkt []byte) (bool, error)) {
	d.counter = uncarriedHook{d.counter, f}
}

type uncarriedHook struct {
	frameCounter
	f func(prev, pkt []byte) (bool, error)
}

func (h uncarriedHook) CarryCheck() func(prev, pkt []byte) (bool, error) { return h.f }
