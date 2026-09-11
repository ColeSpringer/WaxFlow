package wmalossless

// The predictors. Everything here is integer and every shift is stated in the
// notes with its signedness, because the two kinds disagree on negative values
// and two shifts that differ only in rounding are two different codecs.
//
// Three stages run after the residual layer, in this order and no other:
// the per-channel CDLMS cascade, then MCLMS across all channels, then the
// two-channel lifting, then the AC filter. Reordering any two produces
// different samples.

// sign is 1, 0 or -1.
func sign(x int32) int32 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

// cdlms is one filter of a channel's cascade: a sign-sign LMS predictor over
// the channel's own reconstructed samples.
//
// The history and update windows are double length and contiguous rather than
// modular. The window is history[recent : recent+order], recent walks down to
// zero and then the low half is copied up and recent restarts at order-1, so
// the window is always one run of memory. A modular ring would need different
// indexing and would not match.
type cdlms struct {
	order   int
	scaling int
	coeff   []int32
	history []int32
	update  []int32
	recent  int
}

// resize prepares a filter for a new definition, clearing everything. A
// seekable tile clears every filter it is about to use.
func (f *cdlms) resize(order, scaling int) {
	f.order, f.scaling = order, scaling
	if cap(f.coeff) < order {
		f.coeff = make([]int32, order)
		f.history = make([]int32, 2*order)
		f.update = make([]int32, 2*order)
	}
	f.coeff = f.coeff[:order]
	f.history = f.history[:2*order]
	f.update = f.update[:2*order]
	clear(f.coeff)
	clear(f.history)
	clear(f.update)
	// recent starts at order, so the first window is entirely zeros and the
	// first advance moves it to order-1.
	f.recent = order
}

// advance records one reconstructed sample and ages the update window.
func (f *cdlms) advance(sample int32, updateSpeed int32, clipLo, clipHi int32) {
	if f.recent > 0 {
		f.recent--
	} else {
		copy(f.history[f.order:], f.history[:f.order])
		copy(f.update[f.order:], f.update[:f.order])
		f.recent = f.order - 1
	}
	v := sample
	if v < clipLo {
		v = clipLo
	} else if v > clipHi {
		v = clipHi
	}
	f.history[f.recent] = v
	f.update[f.recent] = sign(sample) * updateSpeed
	// The update-magnitude decay. Both shifts are arithmetic, so a magnitude
	// of one with a negative sign stays at -1 rather than reaching 0. At order
	// 8 the first index is recent itself, so the newest update is quartered in
	// the same step that writes it: that reads like a bug and is not one, and
	// the 24-bit files that use order 8 decode bit-exactly with it.
	f.update[f.recent+(f.order>>4)] >>= 2
	f.update[f.recent+(f.order>>3)] >>= 1
}

// run applies the filter to a whole subframe in place, and updates the
// coefficients and history as it goes. The dot product uses the coefficients
// as they were before this sample's update.
func (f *cdlms) run(x []int32, updateSpeed int32, clipLo, clipHi int32) {
	bias := int32(1<<uint(f.scaling)) >> 1
	sh := uint(f.scaling)
	// The dot product and the coefficient update are one pass, not two. They
	// read the same two windows and the update's direction is known before
	// either starts, so fusing them halves the memory traffic through the
	// hottest loop in the decoder; the dot still uses each coefficient as it
	// stood before this sample's update, which is what the notes require, by
	// taking the value the range gives and writing the new one after.
	//
	// The windows are sliced to the coefficient count rather than to the
	// order, so the compiler can see the three slices are the same length and
	// drop the bounds checks.
	for i, residue := range x {
		var dot int32
		coeff := f.coeff
		hist := f.history[f.recent:][:len(coeff)]
		up := f.update[f.recent:][:len(coeff)]
		switch {
		case residue > 0:
			for h, c := range coeff {
				dot += c * hist[h]
				coeff[h] = c + up[h]
			}
		case residue < 0:
			for h, c := range coeff {
				dot += c * hist[h]
				coeff[h] = c - up[h]
			}
		default:
			for h, c := range coeff {
				dot += c * hist[h]
			}
		}
		sample := residue + ((bias + dot) >> sh)
		x[i] = sample
		f.advance(sample, updateSpeed, clipLo, clipHi)
	}
}

// rescaleUpdates is the update-speed switch. Doubling uses a shift and halving
// uses truncating division, which is not the same operation and differs on
// every negative odd value.
//
// The window is the live one under decode-flags bit 8 and the low half without
// it. Bit 8 is set on every stream any encoder writes, so the first form is
// the measured one.
func (f *cdlms) rescaleUpdates(faster, liveWindow bool) {
	at := 0
	if liveWindow {
		at = f.recent
	}
	w := f.update[at : at+f.order]
	if faster {
		for i := range w {
			w[i] *= 2
		}
		return
	}
	for i, v := range w {
		w[i] = v / 2
	}
}

// mclms is a single sign-sign LMS predictor across all channels at once. It
// runs after the CDLMS cascade and before the two-channel lifting.
//
// NOTHING TESTS THIS. No encoder that exists sets its flag, on stereo or on
// 5.1, with correlated channels or without, so every line below is structural:
// recovered from the analysis pass and never run against a real file.
// docs/quality-gates.md records it as a named coverage gap rather than
// pretending otherwise. It is implemented rather than refused because the
// description is complete, and a stream that uses it would otherwise be a
// valid file this build cannot play.
type mclms struct {
	order    int
	scaling  int
	channels int
	coeff    []int32
	curCoeff []int32
	history  []int32
	update   []int32
	recent   int
	pred     []int32
}

func (m *mclms) resize(order, scaling, channels int) {
	m.order, m.scaling, m.channels = order, scaling, channels
	span := order * channels
	m.coeff = grow(m.coeff, span*channels)
	m.curCoeff = grow(m.curCoeff, channels*channels)
	m.history = grow(m.history, 2*span)
	m.update = grow(m.update, 2*span)
	m.pred = grow(m.pred, channels)
	m.recent = span
}

// run applies the predictor over a whole subframe. The channel loop must run
// in ascending order and must not be reordered: channel c predicts partly from
// the already reconstructed samples of the channels below it at the same
// index, which is what makes this a multichannel predictor.
func (m *mclms) run(x [][]int32, coded []bool, n int, clipLo, clipHi int32) {
	span := m.order * m.channels
	bias := int32(1<<uint(m.scaling)) >> 1
	sh := uint(m.scaling)
	for i := 0; i < n; i++ {
		for c := 0; c < m.channels; c++ {
			if !coded[c] {
				m.pred[c] = 0
				continue
			}
			var p int32
			hist := m.history[m.recent : m.recent+span]
			base := m.coeff[c*span : c*span+span]
			for h, v := range hist {
				p += v * base[h]
			}
			cur := m.curCoeff[c*m.channels:]
			for j := 0; j < c; j++ {
				p += x[j][i] * cur[j]
			}
			p = (p + bias) >> sh
			m.pred[c] = p
			x[c][i] += p
		}
		// The prediction error is the residual as it stood before the add.
		for c := 0; c < m.channels; c++ {
			err := x[c][i] - m.pred[c]
			if err == 0 {
				continue
			}
			base := m.coeff[c*span : c*span+span]
			up := m.update[m.recent : m.recent+span]
			cur := m.curCoeff[c*m.channels:]
			if err > 0 {
				for h, v := range up {
					base[h] += v
				}
				for j := 0; j < c; j++ {
					cur[j] += sign(x[j][i])
				}
			} else {
				for h, v := range up {
					base[h] -= v
				}
				for j := 0; j < c; j++ {
					cur[j] -= sign(x[j][i])
				}
			}
		}
		// Highest channel index down to lowest, so that afterwards
		// history[recent] is channel 0's newest sample and the window holds
		// the last `order` sample instants, most recent first, channel 0
		// first within an instant.
		for c := m.channels - 1; c >= 0; c-- {
			if m.recent > 0 {
				m.recent--
			} else {
				copy(m.history[span:], m.history[:span])
				copy(m.update[span:], m.update[:span])
				m.recent = span - 1
			}
			v := x[c][i]
			if v < clipLo {
				v = clipLo
			} else if v > clipHi {
				v = clipHi
			}
			m.history[m.recent] = v
			m.update[m.recent] = sign(x[c][i])
		}
	}
}

// acFilter is a short FIR predictor over a channel's own reconstructed
// samples. Its coefficients come from the seekable tile and are shared; its
// history is per channel and survives across subframes.
type acFilter struct {
	order   int
	scaling int
	coeff   []int32
}

// run applies the filter to one channel's subframe in place, against that
// channel's carried history, and refreshes the history.
func (a *acFilter) run(x []int32, prev []int32) {
	sh := uint(a.scaling)
	for i := range x {
		var p int32
		for j, c := range a.coeff {
			// x[i-j-1], falling back to the carried history before the start.
			if k := i - j - 1; k >= 0 {
				p += c * x[k]
			} else {
				p += c * prev[-k-1]
			}
		}
		x[i] += p >> sh
	}
	// Most recent first, computed from the highest index down: a subframe
	// shorter than the order shifts the older entries along instead of
	// discarding them, and going upward would read entries already overwritten.
	n := len(x)
	for j := len(prev) - 1; j >= 0; j-- {
		if j < n {
			prev[j] = x[n-1-j]
		} else {
			prev[j] = prev[j-n]
		}
	}
}

// grow returns a zeroed slice of exactly n, reusing the backing array.
func grow(s []int32, n int) []int32 {
	if cap(s) < n {
		return make([]int32, n)
	}
	s = s[:n]
	clear(s)
	return s
}
