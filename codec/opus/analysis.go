package opus

// Tonality analyser, ported from libopus src/analysis.c. It runs on a 24 kHz
// downmix in 10 ms hops, computing per-frame tonality/noisiness/bandwidth
// features and the MLP-based music and activity probabilities that drive the
// SILK/CELT mode decision, plus the CELT hooks (leak_boost, max_pitch_ratio,
// tonality) that the earlier port left out.
//
// The 480-point complex FFT stands in for celt's kiss_fft: a plain
// mixed-radix Cooley-Tukey with the same forward 1/N scaling. The analysis
// features feed float thresholds, so bit-exactness with kiss_fft is not
// required (and the reference itself varies per architecture here).

import "math"

const (
	nbFrames          = 8
	nbTbands          = 18
	analysisBufSize   = 720 // 30 ms at 24 kHz
	analysisCountMax  = 10000
	detectSize        = 100
	nbTonalSkipBands  = 9
	leakBands         = 19
	transitionPenalty = 10.0
	leakageOffset     = 2.5
	leakageSlope      = 2.0
	analysisFFTSize   = 480
)

var analysisTbands = [nbTbands + 1]int{
	4, 8, 12, 16, 20, 24, 28, 32, 40, 48, 56, 64, 80, 96, 112, 136, 160, 192, 240,
}

// analysisInfo is one analysed frame's result (celt.h AnalysisInfo).
type analysisInfo struct {
	valid               bool
	tonality            float32
	tonalitySlope       float32
	noisiness           float32
	activity            float32
	musicProb           float32
	musicProbMin        float32
	musicProbMax        float32
	bandwidth           int
	activityProbability float32
	maxPitchRatio       float32
	leakBoost           [leakBands]uint8
}

// tonalityAnalysisState carries the analyser's history
// (TonalityAnalysisState).
type tonalityAnalysisState struct {
	angle            [240]float32
	dAngle           [240]float32
	d2Angle          [240]float32
	inmem            [analysisBufSize]float32
	memFill          int
	prevBandTonality [nbTbands]float32
	prevTonality     float32
	prevBandwidth    int
	E                [nbFrames][nbTbands]float32
	logE             [nbFrames][nbTbands]float32
	lowE             [nbTbands]float32
	highE            [nbTbands]float32
	meanE            [nbTbands + 1]float32
	mem              [32]float32
	cmean            [8]float32
	std              [9]float32
	eTracker         float32
	lowECount        float32
	eCount           int
	count            int
	analysisOffset   int
	writePos         int
	readPos          int
	readSubframe     int
	hpEnerAccum      float32
	initialized      bool
	rnnState         [mlpMaxNeurons]float32
	downmixState     [3]float32
	info             [detectSize]analysisInfo
}

// reset clears the analyser history (tonality_analysis_reset).
func (t *tonalityAnalysisState) reset() {
	*t = tonalityAnalysisState{}
}

// --- 480-point FFT --------------------------------------------------------

// fft480 is a mixed-radix DIT plan for N=480 (factors 2^5*3*5), with the
// forward 1/N scaling celt's kiss_fft applies in float builds.
type fftPlan struct {
	n       int
	factors []int
	twiddle []complex64 // e^{-2*pi*i*k/n} for k in [0, n)
}

var analysisFFT = newFFTPlan(analysisFFTSize)

func newFFTPlan(n int) *fftPlan {
	p := &fftPlan{n: n}
	rem := n
	for _, f := range []int{5, 3, 4, 2} {
		for rem%f == 0 {
			p.factors = append(p.factors, f)
			rem /= f
		}
	}
	if rem != 1 {
		panic("opus: fft size must factor into 2,3,5")
	}
	p.twiddle = make([]complex64, n)
	for k := 0; k < n; k++ {
		s, c := math.Sincos(-2 * math.Pi * float64(k) / float64(n))
		p.twiddle[k] = complex(float32(c), float32(s))
	}
	return p
}

// transform computes the scaled forward FFT of in into out (both length n).
func (p *fftPlan) transform(in, out []complex64) {
	scale := complex(float32(1.0/float64(p.n)), 0)
	tmp := make([]complex64, p.n)
	for i, v := range in {
		tmp[i] = v * scale
	}
	p.work(out, tmp, 1, p.factors)
}

// work is textbook recursive decimation-in-time: with N = f*m at this level
// and input stride s in the original array, the sub-DFTs Y_q of the f
// interleaved sequences combine as
// X[k2 + r*m] = sum_q Y_q[k2] * e^{-2*pi*i*(k2+r*m)*q*s/N_total}.
func (p *fftPlan) work(out, in []complex64, stride int, factors []int) {
	f := factors[0]
	length := 1
	for _, ff := range factors {
		length *= ff
	}
	m := length / f
	if m == 1 {
		for q := 0; q < f; q++ {
			out[q] = in[q*stride]
		}
	} else {
		for q := 0; q < f; q++ {
			p.work(out[q*m:], in[q*stride:], stride*f, factors[1:])
		}
	}
	scratch := make([]complex64, f)
	for k2 := 0; k2 < m; k2++ {
		for q := 0; q < f; q++ {
			scratch[q] = out[q*m+k2]
		}
		for r := 0; r < f; r++ {
			k := k2 + r*m
			acc := scratch[0]
			for q := 1; q < f; q++ {
				acc += scratch[q] * p.twiddle[(k*q*stride)%p.n]
			}
			out[k] = acc
		}
	}
}

// --- analysis-side DSP helpers --------------------------------------------

// fastAtan2 is celt's fast_atan2f approximation (mathops.h).
func fastAtan2(y, x float32) float32 {
	const (
		cA = 0.43157974
		cB = 0.67848403
		cC = 0.08595542
		cE = float32(math.Pi / 2)
	)
	x2 := x * x
	y2 := y * y
	if x2+y2 < 1e-18 {
		return 0
	}
	if x2 < y2 {
		den := (y2 + cB*x2) * (y2 + cC*x2)
		sign := cE
		if y < 0 {
			sign = -cE
		}
		return -x*y*(y2+cA*x2)/den + sign
	}
	den := (x2 + cB*y2) * (x2 + cC*y2)
	signY := cE
	if y < 0 {
		signY = -cE
	}
	signXY := cE
	if x*y < 0 {
		signXY = -cE
	}
	return x*y*(x2+cA*y2)/den + signY - signXY
}

// analysisDown2HP is the float silk_resampler_down2_hp: a 2x decimator that
// also reports the high-pass energy of the discarded band.
func analysisDown2HP(S []float32, out, in []float32, inLen int) float32 {
	len2 := inLen / 2
	var hpEner float64
	for k := 0; k < len2; k++ {
		in32 := in[2*k]
		Y := in32 - S[0]
		X := 0.6074371 * Y
		out32 := S[0] + X
		S[0] = in32 + X
		out32hp := out32

		in32 = in[2*k+1]
		Y = in32 - S[1]
		X = 0.15063 * Y
		out32 += S[1]
		out32 += X
		S[1] = in32 + X

		Y = -in32 - S[2]
		X = 0.15063 * Y
		out32hp += S[2]
		out32hp += X
		S[2] = -in32 + X

		// The reference spells SHR64(x*x, 8) here, but in the float build the
		// shift macros are no-ops (arch.h): the energy accumulates unscaled.
		// Dividing by 256 anyway made the >12 kHz bandwidth detector miss real
		// content (bw stuck at 18, CELT skipped the top band at high rates).
		hpEner += float64(out32hp) * float64(out32hp)
		out[k] = 0.5 * out32
	}
	return float32(hpEner)
}

// analysisDownmix mixes the requested channels of planar float input into
// the signal domain (downmix_float): output is scaled to +-32768.
func analysisDownmix(x [][]float32, y []float32, subframe, offset, c1, c2, C int) {
	for j := 0; j < subframe; j++ {
		y[j] = 32768 * x[c1][offset+j]
	}
	if c2 > -1 {
		for j := 0; j < subframe; j++ {
			y[j] += 32768 * x[c2][offset+j]
		}
	} else if c2 == -2 {
		for c := 1; c < C; c++ {
			for j := 0; j < subframe; j++ {
				y[j] += 32768 * x[c][offset+j]
			}
		}
	}
}

// downmixAndResample feeds the 24 kHz analysis buffer from 48 kHz input
// (downmix_and_resample, Fs==48000 branch) and returns the HP energy.
func (t *tonalityAnalysisState) downmixAndResample(x [][]float32, y []float32, subframe, offset, c1, c2, C int) float32 {
	if subframe == 0 {
		return 0
	}
	subframe *= 2
	offset *= 2
	tmp := make([]float32, subframe)
	analysisDownmix(x, tmp, subframe, offset, c1, c2, C)
	if (c2 == -2 && C == 2) || c2 > -1 {
		for j := range tmp {
			tmp[j] *= 0.5
		}
	}
	ret := analysisDown2HP(t.downmixState[:], y, tmp, subframe)
	return ret * (1.0 / 32768 / 32768)
}

// --- main analysis ---------------------------------------------------------

var analysisStdFeatureBias = [9]float32{
	5.684947, 3.475288, 1.770634, 1.599784, 3.773215,
	2.163313, 1.260756, 1.116868, 1.918795,
}

// tonalityAnalysis analyses one 10 ms hop (tonality_analysis). x is planar
// 48 kHz float input; len48/offset48 are in 48 kHz samples. lsbDepth is the
// input's true bit depth: the bandwidth detector's noise floor scales with it
// (a 16-bit source cannot carry HF below its quantization floor).
func (t *tonalityAnalysisState) tonalityAnalysis(x [][]float32, len48, offset48, c1, c2, C, lsbDepth int) {
	const N = analysisFFTSize
	const N2 = N / 2

	if !t.initialized {
		t.memFill = 240
		t.initialized = true
	}
	alpha := 1.0 / float32(minIntA(10, 1+t.count))
	alphaE := 1.0 / float32(minIntA(25, 1+t.count))
	alphaE2 := 1.0 / float32(minIntA(100, 1+t.count))
	if t.count <= 1 {
		alphaE2 = 1
	}

	// 48 kHz in, 24 kHz analysis domain.
	length := len48 / 2
	offset := offset48 / 2

	t.hpEnerAccum += t.downmixAndResample(x, t.inmem[t.memFill:],
		minIntA(length, analysisBufSize-t.memFill), offset, c1, c2, C)

	if t.memFill+length < analysisBufSize {
		t.memFill += length
		return
	}

	hpEner := t.hpEnerAccum
	info := &t.info[t.writePos]
	t.writePos++
	if t.writePos >= detectSize {
		t.writePos -= detectSize
	}

	isSilence := true
	for _, v := range t.inmem {
		if v != 0 {
			isSilence = false
			break
		}
	}

	var in, out [N]complex64
	var tonality, noisiness, tonality2 [240]float32
	for i := 0; i < N2; i++ {
		w := analysis_window_240[i]
		in[i] = complex(w*t.inmem[i], w*t.inmem[N2+i])
		in[N-i-1] = complex(analysis_window_240[i]*t.inmem[N-i-1], analysis_window_240[i]*t.inmem[N+N2-i-1])
	}
	// Note the reference windows in[N-i-1] with analysis_window[i] as well:
	// the window is symmetric in usage (rising half applied from both ends).
	copy(t.inmem[:240], t.inmem[analysisBufSize-240:])

	remaining := length - (analysisBufSize - t.memFill)
	t.hpEnerAccum = t.downmixAndResample(x, t.inmem[240:],
		remaining, offset+analysisBufSize-t.memFill, c1, c2, C)
	t.memFill = 240 + remaining

	if isSilence {
		prevPos := t.writePos - 2
		if prevPos < 0 {
			prevPos += detectSize
		}
		*info = t.info[prevPos]
		return
	}

	analysisFFT.transform(in[:], out[:])
	if math.IsNaN(float64(real(out[0]))) {
		info.valid = false
		return
	}

	pi4 := float32(math.Pi * math.Pi * math.Pi * math.Pi)
	for i := 1; i < N2; i++ {
		X1r := real(out[i]) + real(out[N-i])
		X1i := imag(out[i]) - imag(out[N-i])
		X2r := imag(out[i]) + imag(out[N-i])
		X2i := real(out[N-i]) - real(out[i])

		angle := float32(0.5/math.Pi) * fastAtan2(X1i, X1r)
		dAngle := angle - t.angle[i]
		d2Angle := dAngle - t.dAngle[i]

		angle2 := float32(0.5/math.Pi) * fastAtan2(X2i, X2r)
		dAngle2 := angle2 - angle
		d2Angle2 := dAngle2 - dAngle

		mod1 := d2Angle - float32(silkFloat2Int(d2Angle))
		noisiness[i] = absA(mod1)
		mod1 *= mod1
		mod1 *= mod1
		mod2 := d2Angle2 - float32(silkFloat2Int(d2Angle2))
		noisiness[i] += absA(mod2)
		mod2 *= mod2
		mod2 *= mod2

		avgMod := 0.25 * (t.d2Angle[i] + mod1 + 2*mod2)
		// Reliable but 2 frames delayed.
		tonality[i] = 1.0/(1.0+40.0*16.0*pi4*avgMod) - 0.015
		// Instant but less reliable.
		tonality2[i] = 1.0/(1.0+40.0*16.0*pi4*mod2) - 0.015

		t.angle[i] = angle2
		t.dAngle[i] = dAngle2
		t.d2Angle[i] = mod2
	}
	for i := 2; i < N2-1; i++ {
		tt := minA(tonality2[i], maxA(tonality2[i-1], tonality2[i+1]))
		tonality[i] = 0.9 * maxA(tonality[i], tt-0.1)
	}

	frameTonality := float32(0)
	maxFrameTonality := float32(0)
	info.activity = 0
	frameNoisiness := float32(0)
	frameStationarity := float32(0)
	if t.count == 0 {
		for b := 0; b < nbTbands; b++ {
			t.lowE[b] = 1e10
			t.highE[b] = -1e10
		}
	}
	relativeE := float32(0)
	frameLoudness := float32(0)

	var logE [nbTbands]float32
	var bandLog2 [nbTbands + 1]float32
	var bandTonality [nbTbands]float32
	{
		// First band is special because of DC.
		X1r := 2 * real(out[0])
		X2r := 2 * imag(out[0])
		E := X1r*X1r + X2r*X2r
		for i := 1; i < 4; i++ {
			binE := real(out[i])*real(out[i]) + real(out[N-i])*real(out[N-i]) +
				imag(out[i])*imag(out[i]) + imag(out[N-i])*imag(out[N-i])
			E += binE
		}
		E *= 1.0 / 32768 / 32768
		bandLog2[0] = 0.5 * 1.442695 * float32(math.Log(float64(E)+1e-10))
	}
	var slope float32
	for b := 0; b < nbTbands; b++ {
		var E, tE, nE float32
		for i := analysisTbands[b]; i < analysisTbands[b+1]; i++ {
			binE := real(out[i])*real(out[i]) + real(out[N-i])*real(out[N-i]) +
				imag(out[i])*imag(out[i]) + imag(out[N-i])*imag(out[N-i])
			binE *= 1.0 / 32768 / 32768
			E += binE
			tE += binE * maxA(0, tonality[i])
			nE += binE * 2.0 * (0.5 - noisiness[i])
		}
		if !(E < 1e9) || math.IsNaN(float64(E)) {
			info.valid = false
			return
		}

		t.E[t.eCount][b] = E
		frameNoisiness += nE / (1e-15 + E)
		frameLoudness += float32(math.Sqrt(float64(E) + 1e-10))
		logE[b] = float32(math.Log(float64(E) + 1e-10))
		bandLog2[b+1] = 0.5 * 1.442695 * logE[b]
		t.logE[t.eCount][b] = logE[b]
		if t.count == 0 {
			t.highE[b] = logE[b]
			t.lowE[b] = logE[b]
		}
		if t.highE[b] > t.lowE[b]+7.5 {
			if t.highE[b]-logE[b] > logE[b]-t.lowE[b] {
				t.highE[b] -= 0.01
			} else {
				t.lowE[b] += 0.01
			}
		}
		if logE[b] > t.highE[b] {
			t.highE[b] = logE[b]
			t.lowE[b] = maxA(t.highE[b]-15, t.lowE[b])
		} else if logE[b] < t.lowE[b] {
			t.lowE[b] = logE[b]
			t.highE[b] = minA(t.lowE[b]+15, t.highE[b])
		}
		relativeE += (logE[b] - t.lowE[b]) / (1e-5 + t.highE[b] - t.lowE[b])

		var L1, L2 float32
		for i := 0; i < nbFrames; i++ {
			L1 += float32(math.Sqrt(float64(t.E[i][b])))
			L2 += t.E[i][b]
		}
		stationarity := minA(0.99, L1/float32(math.Sqrt(1e-15+float64(nbFrames)*float64(L2))))
		stationarity *= stationarity
		stationarity *= stationarity
		frameStationarity += stationarity
		bandTonality[b] = maxA(tE/(1e-15+E), stationarity*t.prevBandTonality[b])
		frameTonality += bandTonality[b]
		if b >= nbTbands-nbTonalSkipBands {
			// Sliding window over the most recent NB_TONAL_SKIP_BANDS bands.
			frameTonality -= bandTonality[b-nbTbands+nbTonalSkipBands]
		}
		maxFrameTonality = maxA(maxFrameTonality, (1.0+0.03*float32(b-nbTbands))*frameTonality)
		slope += bandTonality[b] * float32(b-8)
		t.prevBandTonality[b] = bandTonality[b]
	}

	var leakageFrom, leakageTo [nbTbands + 1]float32
	leakageFrom[0] = bandLog2[0]
	leakageTo[0] = bandLog2[0] - leakageOffset
	for b := 1; b < nbTbands+1; b++ {
		leakSlope := leakageSlope * float32(analysisTbands[b]-analysisTbands[b-1]) / 4
		leakageFrom[b] = minA(leakageFrom[b-1]+leakSlope, bandLog2[b])
		leakageTo[b] = maxA(leakageTo[b-1]-leakSlope, bandLog2[b]-leakageOffset)
	}
	for b := nbTbands - 2; b >= 0; b-- {
		leakSlope := leakageSlope * float32(analysisTbands[b+1]-analysisTbands[b]) / 4
		leakageFrom[b] = minA(leakageFrom[b+1]+leakSlope, leakageFrom[b])
		leakageTo[b] = maxA(leakageTo[b+1]-leakSlope, leakageTo[b])
	}
	for b := 0; b < nbTbands+1; b++ {
		boost := maxA(0, leakageTo[b]-bandLog2[b]) +
			maxA(0, bandLog2[b]-(leakageFrom[b]+leakageOffset))
		v := int(math.Floor(0.5 + 64*float64(boost)))
		if v > 255 {
			v = 255
		}
		info.leakBoost[b] = uint8(v)
	}

	specVariability := float32(0)
	for i := 0; i < nbFrames; i++ {
		mindist := float32(1e15)
		for j := 0; j < nbFrames; j++ {
			var dist float32
			for k := 0; k < nbTbands; k++ {
				tmp := t.logE[i][k] - t.logE[j][k]
				dist += tmp * tmp
			}
			if j != i {
				mindist = minA(mindist, dist)
			}
		}
		specVariability += mindist
	}
	specVariability = float32(math.Sqrt(float64(specVariability) / nbFrames / nbTbands))

	bandwidthMask := float32(0)
	bandwidth := 0
	maxE := float32(0)
	noiseFloor := 5.7e-4 / float32(int32(1)<<uint(maxIntA(0, lsbDepth-8)))
	noiseFloor *= noiseFloor
	var belowMaxPitch, aboveMaxPitch float32
	var isMasked [nbTbands + 1]bool
	for b := 0; b < nbTbands; b++ {
		var E float32
		bandStart := analysisTbands[b]
		bandEnd := analysisTbands[b+1]
		for i := bandStart; i < bandEnd; i++ {
			binE := real(out[i])*real(out[i]) + real(out[N-i])*real(out[N-i]) +
				imag(out[i])*imag(out[i]) + imag(out[N-i])*imag(out[N-i])
			E += binE
		}
		E *= 1.0 / 32768 / 32768
		maxE = maxA(maxE, E)
		if bandStart < 64 {
			belowMaxPitch += E
		} else {
			aboveMaxPitch += E
		}
		t.meanE[b] = maxA((1-alphaE2)*t.meanE[b], E)
		Em := maxA(E, t.meanE[b])
		if E*1e9 > maxE && (Em > 3*noiseFloor*float32(bandEnd-bandStart) || E > noiseFloor*float32(bandEnd-bandStart)) {
			bandwidth = b + 1
		}
		thresh := float32(0.05)
		if t.prevBandwidth >= b+1 {
			thresh = 0.01
		}
		isMasked[b] = E < thresh*bandwidthMask
		bandwidthMask = maxA(0.05*bandwidthMask, E)
	}
	{
		// Bands 19-20 exist only as energy above 12 kHz from the HP side of
		// the decimator.
		E := hpEner * (1.0 / (60 * 60))
		noiseRatio := float32(30.0)
		if t.prevBandwidth == 20 {
			noiseRatio = 10.0
		}
		aboveMaxPitch += E
		t.meanE[nbTbands] = maxA((1-alphaE2)*t.meanE[nbTbands], E)
		Em := maxA(E, t.meanE[nbTbands])
		if Em > 3*noiseRatio*noiseFloor*160 || E > noiseRatio*noiseFloor*160 {
			bandwidth = 20
		}
		thresh := float32(0.05)
		if t.prevBandwidth == 20 {
			thresh = 0.01
		}
		isMasked[nbTbands] = E < thresh*bandwidthMask
	}
	if aboveMaxPitch > belowMaxPitch {
		info.maxPitchRatio = belowMaxPitch / aboveMaxPitch
	} else {
		info.maxPitchRatio = 1
	}
	if bandwidth == 20 && isMasked[nbTbands] {
		bandwidth -= 2
	} else if bandwidth > 0 && bandwidth <= nbTbands && isMasked[bandwidth-1] {
		bandwidth--
	}
	if t.count <= 2 {
		bandwidth = 20
	}

	frameLoudness = 20 * float32(math.Log10(float64(frameLoudness)))
	t.eTracker = maxA(t.eTracker-0.003, frameLoudness)
	t.lowECount *= 1 - alphaE
	if frameLoudness < t.eTracker-30 {
		t.lowECount += alphaE
	}

	var BFCC, midE [8]float32
	for i := 0; i < 8; i++ {
		var sum float32
		for b := 0; b < 16; b++ {
			sum += analysis_dct_table[i*16+b] * logE[b]
		}
		BFCC[i] = sum
	}
	for i := 0; i < 8; i++ {
		var sum float32
		for b := 0; b < 16; b++ {
			sum += analysis_dct_table[i*16+b] * 0.5 * (t.highE[b] + t.lowE[b])
		}
		midE[i] = sum
	}

	frameStationarity /= nbTbands
	relativeE /= nbTbands
	if t.count < 10 {
		relativeE = 0.5
	}
	frameNoisiness /= nbTbands
	info.activity = frameNoisiness + (1-frameNoisiness)*relativeE

	frameTonality = maxFrameTonality / float32(nbTbands-nbTonalSkipBands)
	frameTonality = maxA(frameTonality, t.prevTonality*0.8)
	t.prevTonality = frameTonality

	slope /= 8 * 8
	info.tonalitySlope = slope

	t.eCount = (t.eCount + 1) % nbFrames
	t.count = minIntA(t.count+1, analysisCountMax)
	info.tonality = frameTonality

	var features [25]float32
	for i := 0; i < 4; i++ {
		features[i] = -0.12299*(BFCC[i]+t.mem[i+24]) + 0.49195*(t.mem[i]+t.mem[i+16]) +
			0.69693*t.mem[i+8] - 1.4349*t.cmean[i]
	}
	for i := 0; i < 4; i++ {
		t.cmean[i] = (1-alpha)*t.cmean[i] + alpha*BFCC[i]
	}
	for i := 0; i < 4; i++ {
		features[4+i] = 0.63246*(BFCC[i]-t.mem[i+24]) + 0.31623*(t.mem[i]-t.mem[i+16])
	}
	for i := 0; i < 3; i++ {
		features[8+i] = 0.53452*(BFCC[i]+t.mem[i+24]) - 0.26726*(t.mem[i]+t.mem[i+16]) - 0.53452*t.mem[i+8]
	}
	if t.count > 5 {
		for i := 0; i < 9; i++ {
			t.std[i] = (1-alpha)*t.std[i] + alpha*features[i]*features[i]
		}
	}
	for i := 0; i < 4; i++ {
		features[i] = BFCC[i] - midE[i]
	}
	for i := 0; i < 8; i++ {
		t.mem[i+24] = t.mem[i+16]
		t.mem[i+16] = t.mem[i+8]
		t.mem[i+8] = t.mem[i]
		t.mem[i] = BFCC[i]
	}
	for i := 0; i < 9; i++ {
		features[11+i] = float32(math.Sqrt(float64(t.std[i]))) - analysisStdFeatureBias[i]
	}
	features[18] = specVariability - 0.78
	features[20] = info.tonality - 0.154723
	features[21] = info.activity - 0.724643
	features[22] = frameStationarity - 0.743717
	features[23] = info.tonalitySlope + 0.069216
	features[24] = t.lowECount - 0.067930

	var layerOut [mlpMaxNeurons]float32
	var frameProbs [2]float32
	analysisLayer0.compute(layerOut[:], features[:])
	analysisLayer1.compute(t.rnnState[:], layerOut[:])
	analysisLayer2.compute(frameProbs[:], t.rnnState[:])

	info.activityProbability = frameProbs[1]
	info.musicProb = frameProbs[0]

	info.bandwidth = bandwidth
	t.prevBandwidth = bandwidth
	info.noisiness = frameNoisiness
	info.valid = true
}

// tonalityGetInfo reads the analysis result for the coming frame
// (tonality_get_info), including the badness-minimizing music_prob_min/max
// hysteresis thresholds.
func (t *tonalityAnalysisState) tonalityGetInfo(infoOut *analysisInfo, length int) {
	pos := t.readPos
	currLookahead := t.writePos - t.readPos
	if currLookahead < 0 {
		currLookahead += detectSize
	}

	t.readSubframe += length / (48000 / 400)
	for t.readSubframe >= 8 {
		t.readSubframe -= 8
		t.readPos++
	}
	if t.readPos >= detectSize {
		t.readPos -= detectSize
	}

	// On long frames, look at the second analysis window.
	if length > 48000/50 && pos != t.writePos {
		pos++
		if pos == detectSize {
			pos = 0
		}
	}
	if pos == t.writePos {
		pos--
	}
	if pos < 0 {
		pos = detectSize - 1
	}
	pos0 := pos

	*infoOut = t.info[pos]
	if !infoOut.valid {
		return
	}
	tonalityMax := infoOut.tonality
	tonalityAvg := infoOut.tonality
	tonalityCount := 1

	bandwidthSpan := 6
	for i := 0; i < 3; i++ {
		pos++
		if pos == detectSize {
			pos = 0
		}
		if pos == t.writePos {
			break
		}
		tonalityMax = maxA(tonalityMax, t.info[pos].tonality)
		tonalityAvg += t.info[pos].tonality
		tonalityCount++
		if t.info[pos].bandwidth > infoOut.bandwidth {
			infoOut.bandwidth = t.info[pos].bandwidth
		}
		bandwidthSpan--
	}
	pos = pos0
	for i := 0; i < bandwidthSpan; i++ {
		pos--
		if pos < 0 {
			pos = detectSize - 1
		}
		if pos == t.writePos {
			break
		}
		if t.info[pos].bandwidth > infoOut.bandwidth {
			infoOut.bandwidth = t.info[pos].bandwidth
		}
	}
	infoOut.tonality = maxA(tonalityAvg/float32(tonalityCount), tonalityMax-0.2)

	mpos, vpos := pos0, pos0
	if currLookahead > 15 {
		mpos += 5
		if mpos >= detectSize {
			mpos -= detectSize
		}
		vpos++
		if vpos >= detectSize {
			vpos -= detectSize
		}
	}

	probMin := float32(1.0)
	probMax := float32(0.0)
	vadProb := t.info[vpos].activityProbability
	probCount := maxA(0.1, vadProb)
	probAvg := maxA(0.1, vadProb) * t.info[mpos].musicProb
	for {
		mpos++
		if mpos == detectSize {
			mpos = 0
		}
		if mpos == t.writePos {
			break
		}
		vpos++
		if vpos == detectSize {
			vpos = 0
		}
		if vpos == t.writePos {
			break
		}
		posVad := t.info[vpos].activityProbability
		probMin = minA((probAvg-transitionPenalty*(vadProb-posVad))/probCount, probMin)
		probMax = maxA((probAvg+transitionPenalty*(vadProb-posVad))/probCount, probMax)
		probCount += maxA(0.1, posVad)
		probAvg += maxA(0.1, posVad) * t.info[mpos].musicProb
	}
	infoOut.musicProb = probAvg / probCount
	probMin = minA(probAvg/probCount, probMin)
	probMax = maxA(probAvg/probCount, probMax)
	probMin = maxA(probMin, 0)
	probMax = minA(probMax, 1)

	if currLookahead < 10 {
		pmin, pmax := probMin, probMax
		pos = pos0
		for i := 0; i < minIntA(t.count-1, 15); i++ {
			pos--
			if pos < 0 {
				pos = detectSize - 1
			}
			pmin = minA(pmin, t.info[pos].musicProb)
			pmax = maxA(pmax, t.info[pos].musicProb)
		}
		pmin = maxA(0, pmin-0.1*vadProb)
		pmax = minA(1, pmax+0.1*vadProb)
		probMin += (1 - 0.1*float32(currLookahead)) * (pmin - probMin)
		probMax += (1 - 0.1*float32(currLookahead)) * (pmax - probMax)
	}
	infoOut.musicProbMin = probMin
	infoOut.musicProbMax = probMax
}

// runAnalysis drives the analyser over new input and reads the info for the
// frame about to be encoded (run_analysis). x is planar 48 kHz float32;
// lsbDepth is the input's true bit depth (see tonalityAnalysis).
func (t *tonalityAnalysisState) runAnalysis(x [][]float32, analysisFrameSize, frameSize, c1, c2, C, lsbDepth int, infoOut *analysisInfo) {
	analysisFrameSize -= analysisFrameSize & 1
	if x != nil {
		analysisFrameSize = minIntA((detectSize-5)*48000/50, analysisFrameSize)
		pcmLen := analysisFrameSize - t.analysisOffset
		offset := t.analysisOffset
		for pcmLen > 0 {
			t.tonalityAnalysis(x, minIntA(48000/50, pcmLen), offset, c1, c2, C, lsbDepth)
			offset += 48000 / 50
			pcmLen -= 48000 / 50
		}
		t.analysisOffset = analysisFrameSize
		t.analysisOffset -= frameSize
	}
	t.tonalityGetInfo(infoOut, frameSize)
}

func minA(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}
func maxA(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
func absA(a float32) float32 {
	if a < 0 {
		return -a
	}
	return a
}
func minIntA(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxIntA(a, b int) int {
	if a > b {
		return a
	}
	return b
}
