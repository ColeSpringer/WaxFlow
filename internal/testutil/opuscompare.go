package testutil

import "math"

// OpusCompare is a Go port of libopus's opus_compare.c, the
// RFC 6716 section 6 conformance metric. It returns the quality percentage Q
// comparing a candidate decode against a reference decode; a decoder is
// conformant on a test vector when Q >= 0. ref and test are interleaved samples
// at the 48 kHz output rate on the int16 amplitude scale (roughly [-32768,
// 32767]); the metric's additive masking constants assume that scale, so
// normalized [-1,1] output must be multiplied by 32768 before calling.
//
// Only the 48 kHz path is ported (WaxFlow always decodes Opus to 48 kHz), so
// downsample is 1, ybands is 21, and yfreqs equals NFREQS.
func OpusCompare(ref, test []float32, nchannels int) float64 {
	const (
		nbands  = 21
		nfreqs  = 240
		winSize = 480
		winStep = 120
		pi      = math.Pi
	)
	bands := [nbands + 1]int{0, 2, 4, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 40, 48, 56, 68, 80, 96, 120, 156, 200}

	x := append([]float32(nil), ref...)
	y := append([]float32(nil), test...)
	if nchannels == 1 && len(ref) >= 2 {
		// The reference downmixes a stereo file to mono; our inputs already
		// carry the right channel count, so nothing to do here.
	}
	xlen := len(x) / nchannels
	ylen := len(y) / nchannels
	if xlen != ylen || xlen < winSize {
		return math.Inf(-1)
	}
	nframes := (xlen - winSize + winStep) / winStep

	bandEnergy := func(in []float32, out, ps []float32) {
		window := make([]float32, winSize)
		c := make([]float32, winSize)
		s := make([]float32, winSize)
		xbuf := make([]float32, nchannels*winSize)
		psSz := winSize / 2
		for j := 0; j < winSize; j++ {
			window[j] = 0.5 - 0.5*float32(math.Cos((2*pi/(winSize-1))*float64(j)))
			c[j] = float32(math.Cos((2 * pi / winSize) * float64(j)))
			s[j] = float32(math.Sin((2 * pi / winSize) * float64(j)))
		}
		for xi := 0; xi < nframes; xi++ {
			for ci := 0; ci < nchannels; ci++ {
				for xk := 0; xk < winSize; xk++ {
					xbuf[ci*winSize+xk] = window[xk] * in[(xi*winStep+xk)*nchannels+ci]
				}
			}
			xj := 0
			for bi := 0; bi < nbands; bi++ {
				var p [2]float64
				for ; xj < bands[bi+1]; xj++ {
					for ci := 0; ci < nchannels; ci++ {
						var re, im float64
						ti := 0
						for xk := 0; xk < winSize; xk++ {
							re += float64(c[ti]) * float64(xbuf[ci*winSize+xk])
							im -= float64(s[ti]) * float64(xbuf[ci*winSize+xk])
							ti += xj
							if ti >= winSize {
								ti -= winSize
							}
						}
						v := float32(re*re+im*im) + 100000
						ps[(xi*psSz+xj)*nchannels+ci] = v
						p[ci] += float64(v)
					}
				}
				if out != nil {
					out[(xi*nbands+bi)*nchannels] = float32(p[0] / float64(bands[bi+1]-bands[bi]))
					if nchannels == 2 {
						out[(xi*nbands+bi)*nchannels+1] = float32(p[1] / float64(bands[bi+1]-bands[bi]))
					}
				}
			}
		}
	}

	xb := make([]float32, nframes*nbands*nchannels)
	X := make([]float32, nframes*nfreqs*nchannels)
	Y := make([]float32, nframes*nfreqs*nchannels)
	bandEnergy(x, xb, X)
	bandEnergy(y, nil, Y)

	for xi := 0; xi < nframes; xi++ {
		for bi := 1; bi < nbands; bi++ {
			for ci := 0; ci < nchannels; ci++ {
				xb[(xi*nbands+bi)*nchannels+ci] += 0.1 * xb[(xi*nbands+bi-1)*nchannels+ci]
			}
		}
		for bi := nbands - 1; bi > 0; {
			bi--
			for ci := 0; ci < nchannels; ci++ {
				xb[(xi*nbands+bi)*nchannels+ci] += 0.03 * xb[(xi*nbands+bi+1)*nchannels+ci]
			}
		}
		if xi > 0 {
			for bi := 0; bi < nbands; bi++ {
				for ci := 0; ci < nchannels; ci++ {
					xb[(xi*nbands+bi)*nchannels+ci] += 0.5 * xb[((xi-1)*nbands+bi)*nchannels+ci]
				}
			}
		}
		if nchannels == 2 {
			for bi := 0; bi < nbands; bi++ {
				l := xb[(xi*nbands+bi)*nchannels+0]
				r := xb[(xi*nbands+bi)*nchannels+1]
				xb[(xi*nbands+bi)*nchannels+0] += 0.01 * r
				xb[(xi*nbands+bi)*nchannels+1] += 0.01 * l
			}
		}
		for bi := 0; bi < nbands; bi++ {
			for xj := bands[bi]; xj < bands[bi+1]; xj++ {
				for ci := 0; ci < nchannels; ci++ {
					X[(xi*nfreqs+xj)*nchannels+ci] += 0.1 * xb[(xi*nbands+bi)*nchannels+ci]
					Y[(xi*nfreqs+xj)*nchannels+ci] += 0.1 * xb[(xi*nbands+bi)*nchannels+ci]
				}
			}
		}
	}

	// Average of consecutive frames.
	for bi := 0; bi < nbands; bi++ {
		for xj := bands[bi]; xj < bands[bi+1]; xj++ {
			for ci := 0; ci < nchannels; ci++ {
				xtmp := X[xj*nchannels+ci]
				ytmp := Y[xj*nchannels+ci]
				for xi := 1; xi < nframes; xi++ {
					xtmp2 := X[(xi*nfreqs+xj)*nchannels+ci]
					ytmp2 := Y[(xi*nfreqs+xj)*nchannels+ci]
					X[(xi*nfreqs+xj)*nchannels+ci] += xtmp
					Y[(xi*nfreqs+xj)*nchannels+ci] += ytmp
					xtmp, ytmp = xtmp2, ytmp2
				}
			}
		}
	}

	maxCompare := bands[nbands]
	var err float64
	for xi := 0; xi < nframes; xi++ {
		var Ef float64
		for bi := 0; bi < nbands; bi++ {
			var Eb float64
			for xj := bands[bi]; xj < bands[bi+1] && xj < maxCompare; xj++ {
				for ci := 0; ci < nchannels; ci++ {
					re := float64(Y[(xi*nfreqs+xj)*nchannels+ci] / X[(xi*nfreqs+xj)*nchannels+ci])
					im := re - math.Log(re) - 1
					if xj >= 79 && xj <= 81 {
						im *= 0.1
					}
					if xj == 80 {
						im *= 0.1
					}
					Eb += im
				}
			}
			Eb /= float64((bands[bi+1] - bands[bi]) * nchannels)
			Ef += Eb * Eb
		}
		Ef /= nbands
		Ef *= Ef
		err += Ef * Ef
	}
	err = math.Pow(err/float64(nframes), 1.0/16)
	return 100 * (1 - 0.5*math.Log(1+err)/math.Log(1.13))
}
