package aac

// finishChannel applies TNS and the filterbank, writing the channel's 1024
// output samples with overlap-add against the previous frame.
func (d *Decoder) finishChannel(cd *channelData, outCh int) {
	if cd.hasTNS {
		applyTNS(cd, d.rateIdx)
	}
	info := &cd.info
	curShape := info.windowShape
	prevShape := d.prevWin[outCh]
	var cur [2048]float64
	if info.windowSequence == eightShort {
		shortFilterbank(cd, prevShape, curShape, &cur)
	} else {
		var z [2048]float64
		planLong.imdct(cd.spec[:1024], z[:])
		longWindowApply(&z, &cur, info.windowSequence, prevShape, curShape)
	}
	out := d.buf.ChanF(outCh)[:1024]
	ov := &d.overlap[outCh]
	// AAC dequantization yields integer-PCM-scale samples; normalize to the
	// pipeline's [-1, 1] float convention.
	const norm = 1.0 / 32768.0
	for i := 0; i < 1024; i++ {
		out[i] = float32((cur[i] + ov[i]) * norm)
		ov[i] = cur[1024+i]
	}
	d.prevWin[outCh] = curShape
}

// longWindowApply windows a 2048-sample long IMDCT output. The left half
// uses the previous frame's window shape, the right half the current's;
// LONG_START and LONG_STOP taper into and out of the short-block region.
func longWindowApply(z, cur *[2048]float64, seq, prevShape, curShape int) {
	// Left half [0,1024).
	if seq == longStop {
		wl := &shortWindow[prevShape]
		for n := 0; n < 448; n++ {
			cur[n] = 0
		}
		for n := 0; n < 128; n++ {
			cur[448+n] = z[448+n] * wl[n]
		}
		for n := 576; n < 1024; n++ {
			cur[n] = z[n]
		}
	} else {
		wl := &longWindow[prevShape]
		for n := 0; n < 1024; n++ {
			cur[n] = z[n] * wl[n]
		}
	}
	// Right half [1024,2048).
	if seq == longStart {
		for n := 1024; n < 1472; n++ {
			cur[n] = z[n]
		}
		wr := &shortWindow[curShape]
		for n := 0; n < 128; n++ {
			cur[1472+n] = z[1472+n] * wr[128+n]
		}
		for n := 1600; n < 2048; n++ {
			cur[n] = 0
		}
	} else {
		wr := &longWindow[curShape]
		for n := 1024; n < 2048; n++ {
			cur[n] = z[n] * wr[n]
		}
	}
}

// shortFilterbank runs the eight short IMDCTs, windows each with the short
// window, and overlap-adds them into the 2048-sample frame at 128-sample
// hops starting at offset 448.
func shortFilterbank(cd *channelData, prevShape, curShape int, cur *[2048]float64) {
	*cur = [2048]float64{}
	for i := 0; i < 8; i++ {
		var z [256]float64
		planShort.imdct(cd.spec[i*128:i*128+128], z[:])
		lShape := curShape
		if i == 0 {
			lShape = prevShape
		}
		wl := &shortWindow[lShape]
		wr := &shortWindow[curShape]
		off := 448 + i*128
		for n := 0; n < 128; n++ {
			cur[off+n] += z[n] * wl[n]
			cur[off+128+n] += z[128+n] * wr[128+n]
		}
	}
}
