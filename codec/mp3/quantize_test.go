package mp3

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/psy"
)

// TestPsyCalibration re-measures the psy-to-spectral energy mapping the
// thrCalib constant pins: full-scale sines across the band, analyzed by
// the model's FFT geometry on one side and the encoder's polyphase+MDCT
// on the other. Block phase and subband-edge leakage spread the per-tone
// ratio, so the pin is a band, not a point; a transform or model change
// that moves the geometry out of it must retune the constant.
func TestPsyCalibration(t *testing.T) {
	const rate = 44100
	row := 6 // 44.1 kHz
	offs := make([]int, len(sfbEdgesLong[row]))
	for i, v := range sfbEdgesLong[row] {
		offs[i] = v
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, freq := range []float64{220, 440, 1000, 2000, 4000, 7000, 12000, 16000} {
		var a analyzer
		var xr [576]float32
		const gs = 20
		pcm := make([]float32, 576*gs)
		for i := range pcm {
			pcm[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / rate))
		}
		for g := 0; g < gs; g++ {
			a.granuleMDCT(pcm[g*576:(g+1)*576], &xr)
		}
		xrE := 0.0
		for _, v := range xr {
			xrE += float64(v) * float64(v)
		}

		m, err := psy.New(psy.Config{Rate: rate, Lines: 576, FFTSize: 1024, BandOffsets: offs})
		if err != nil {
			t.Fatal(err)
		}
		var fftE float64
		for g := 4; g < gs; g++ {
			end := (g + 1) * 576
			res, err := m.Analyze(pcm[end-1024 : end])
			if err != nil {
				t.Fatal(err)
			}
			fftE = 0
			for _, e := range res.Energy {
				fftE += e
			}
		}
		ratio := xrE / fftE
		lo = math.Min(lo, ratio)
		hi = math.Max(hi, ratio)
		t.Logf("freq %6.0f: ratio %.3g", freq, ratio)
	}
	if lo < 2e-6 || hi > 2e-5 {
		t.Errorf("measured ratio band [%.3g, %.3g] left the pinned [2e-6, 2e-5]; retune thrCalib", lo, hi)
	}
	if thrCalib < lo/4 || thrCalib > hi*4 {
		t.Errorf("thrCalib %.3g is far outside the measured band [%.3g, %.3g]", thrCalib, lo, hi)
	}
}

// TestResolveScalefactorsMPEG1 checks the compress-table search: the
// chosen entry covers the values, missing (slen1, slen2) pairs fall to
// the cheapest covering entry, and preflag engages when it saves bits.
func TestResolveScalefactorsMPEG1(t *testing.T) {
	cases := []struct {
		name string
		set  func(sf *[nSfBands]int)
		// wantPre asserts preflag; wantMaxTx bounds every transmitted value.
		wantPre bool
	}{
		{"zero", func(sf *[nSfBands]int) {}, false},
		{"low-bands-only", func(sf *[nSfBands]int) { sf[0], sf[4] = 3, 1 }, false},
		{"needs-1-0", func(sf *[nSfBands]int) { sf[2] = 1 }, false},
		{"high-bands", func(sf *[nSfBands]int) { sf[12], sf[19] = 5, 3 }, false},
		{"pretab-shaped", func(sf *[nSfBands]int) {
			for i, p := range preamp {
				sf[11+i] = int(p) + 1
			}
		}, true},
		{"max-range", func(sf *[nSfBands]int) {
			for b := 0; b <= 10; b++ {
				sf[b] = 15
			}
			for b := 11; b < nSfBands; b++ {
				sf[b] = 7
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sf [nSfBands]int
			tc.set(&sf)
			part2, slen, compress, preflag, tx := resolveScalefactors(&sf, true)
			if compress < 0 || compress > 15 {
				t.Fatalf("compress %d outside the 4-bit field", compress)
			}
			// The decoder's view of this compress value.
			packed := scfcDecode[compress]
			s1, s2 := int(packed>>2), int(packed&3)
			if slen != [4]int{s1, s1, s2, s2} {
				t.Fatalf("slen %v disagrees with compress %d (decoder sees %d/%d)", slen, compress, s1, s2)
			}
			if part2 != 11*s1+10*s2 {
				t.Fatalf("part2 %d, want %d", part2, 11*s1+10*s2)
			}
			if preflag != tc.wantPre {
				t.Fatalf("preflag %v, want %v", preflag, tc.wantPre)
			}
			// Every transmitted value fits its field, and folding the
			// decoder's pretab back reconstructs the effective values.
			for b, v := range tx {
				w := s1
				if b > 10 {
					w = s2
				}
				if v < 0 || v >= 1<<w {
					t.Fatalf("band %d: transmitted %d does not fit %d bits", b, v, w)
				}
				eff := v
				if preflag && b >= 11 {
					eff += int(preamp[b-11])
				}
				if eff != sf[b] {
					t.Fatalf("band %d: decoder reconstructs %d, want %d", b, eff, sf[b])
				}
			}
		})
	}
}

// TestResolveScalefactorsLSF checks the MPEG-2 mixed-radix encoding by
// running the decoder's own extraction loop over the produced field.
func TestResolveScalefactorsLSF(t *testing.T) {
	var sf [nSfBands]int
	sf[0], sf[7], sf[12], sf[18] = 9, 15, 6, 2
	part2, slen, compress, preflag, tx := resolveScalefactors(&sf, false)
	if preflag {
		t.Fatal("LSF never sets preflag")
	}
	if compress >= 400 {
		t.Fatalf("compress %d outside the four-width range", compress)
	}
	// The decoder's mixed-radix extraction (readScalefactors' loop).
	var sizes [4]int
	sfc := compress
	k := 0
	for ; sfc >= 0; k += 4 {
		prod := 1
		for i := 3; i >= 0; i-- {
			m := int(scfMod[k+i])
			sizes[i] = sfc / prod % m
			prod *= m
		}
		sfc -= prod
	}
	if k != 4 {
		t.Fatalf("decoder lands on partition offset %d, want 4 (the 6/5/5/5 row)", k)
	}
	if sizes != slen {
		t.Fatalf("decoder extracts %v, muxer meant %v", sizes, slen)
	}
	if want := 6*slen[0] + 5*slen[1] + 5*slen[2] + 5*slen[3]; part2 != want {
		t.Fatalf("part2 %d, want %d", part2, want)
	}
	if tx != sf {
		t.Fatalf("LSF transmits raw values; got %v", tx)
	}
}

// TestMSStereoDecision checks the per-frame stereo choice and its wire
// form: correlated stereo chooses mid/side (mode_extension bit 2 under
// joint stereo) and survives the decoder round trip per channel;
// uncorrelated channels (disjoint spectra, where the shared conservative
// threshold makes mid/side expensive) keep independent L/R.
func TestMSStereoDecision(t *testing.T) {
	const rate, n = 44100, 30000
	gen := func(correlated bool) [][]float32 {
		chans := [][]float32{make([]float32, n), make([]float32, n)}
		for i := 0; i < n; i++ {
			v := 0.4*math.Sin(2*math.Pi*440*float64(i)/rate) +
				0.2*math.Sin(2*math.Pi*3000*float64(i)/rate)
			chans[0][i] = float32(v)
			if correlated {
				chans[1][i] = float32(v + 0.02*math.Sin(2*math.Pi*997*float64(i)/rate))
			} else {
				chans[1][i] = float32(0.4*math.Sin(2*math.Pi*1170*float64(i)/rate) +
					0.2*math.Sin(2*math.Pi*5100*float64(i)/rate))
			}
		}
		return chans
	}
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}

	msFrames := func(pkts [][]byte) (ms, total int) {
		for _, p := range pkts {
			h, err := ParseHeader(p)
			if err != nil {
				t.Fatal(err)
			}
			total++
			if h.Mode == ModeJoint && h.ModeExt&2 != 0 {
				ms++
			}
		}
		return ms, total
	}

	e, err := NewEncoder(f, &EncoderOptions{Bitrate: 128000})
	if err != nil {
		t.Fatal(err)
	}
	corr := gen(true)
	pkts := encodeSignal(t, e, corr, 1152)
	ms, total := msFrames(pkts)
	if ms == 0 {
		t.Errorf("correlated stereo chose mid/side on 0/%d frames", total)
	}
	out := decodeFrames(t, f, pkts)
	for ch := 0; ch < 2; ch++ {
		if _, snr := bestLagSNR(corr[ch], out[ch], 1600); snr < 30 {
			t.Errorf("correlated ch %d round-trip SNR %.1f dB below 30", ch, snr)
		}
	}

	e, err = NewEncoder(f, &EncoderOptions{Bitrate: 128000})
	if err != nil {
		t.Fatal(err)
	}
	uncorr := gen(false)
	pkts = encodeSignal(t, e, uncorr, 1152)
	ms, total = msFrames(pkts)
	if ms > total/4 {
		t.Errorf("uncorrelated stereo chose mid/side on %d/%d frames", ms, total)
	}
	out = decodeFrames(t, f, pkts)
	for ch := 0; ch < 2; ch++ {
		if _, snr := bestLagSNR(uncorr[ch], out[ch], 1600); snr < 30 {
			t.Errorf("uncorrelated ch %d round-trip SNR %.1f dB below 30", ch, snr)
		}
	}
}

// TestScalefactorsOnWire checks that shaped content actually transmits
// scalefactors (nonzero scalefac_compress somewhere) and that the decoder
// reconstructs the stream at high fidelity, proving the wire layout
// (partitions, compress field, part2 accounting) agrees end to end.
func TestScalefactorsOnWire(t *testing.T) {
	const rate, n = 44100, 30000
	// A strong low tone plus a quiet high tone: shaping the quiet band's
	// noise floor needs per-band amplification, not just global gain.
	chans := [][]float32{make([]float32, n)}
	for i := 0; i < n; i++ {
		chans[0][i] = float32(0.5*math.Sin(2*math.Pi*200*float64(i)/rate) +
			0.01*math.Sin(2*math.Pi*6000*float64(i)/rate))
	}
	f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	e, err := NewEncoder(f, &EncoderOptions{Bitrate: 128000})
	if err != nil {
		t.Fatal(err)
	}
	pkts := encodeSignal(t, e, chans, 1152)

	sawCompress := false
	var si sideInfo
	for _, p := range pkts {
		h, err := ParseHeader(p)
		if err != nil {
			t.Fatal(err)
		}
		if !parseSideInfo(h, p[HeaderLen:HeaderLen+h.SideInfoLen()], &si) {
			t.Fatal("side info does not parse")
		}
		for gr := 0; gr < 2; gr++ {
			if si.gr[gr][0].scfCompress != 0 {
				sawCompress = true
			}
		}
	}
	if !sawCompress {
		t.Error("no frame transmitted scalefactors on shaped content")
	}
	out := decodeFrames(t, f, pkts)
	if _, snr := bestLagSNR(chans[0], out[0], 1600); snr < 35 {
		t.Errorf("round-trip SNR %.1f dB below 35", snr)
	}
}

// TestVBREncode checks variable-bit-rate framing: frames pick their own
// legal rates (quiet content small, loud content large), every frame
// parses, the stream decodes coherently, and Bitrate reports 0 per the
// VBR plan contract.
func TestVBREncode(t *testing.T) {
	const rate, n = 44100, 60000
	chans := [][]float32{make([]float32, n), make([]float32, n)}
	for i := 0; i < n; i++ {
		v := 0.0
		if i >= n/2 { // half silence, half dense broadband
			for j, fq := range []float64{220, 700, 1900, 4300, 8100, 11000} {
				v += 0.11 * math.Sin(2*math.Pi*fq*float64(i)/rate+float64(j))
			}
		}
		chans[0][i] = float32(v)
		chans[1][i] = float32(v * 0.9)
	}
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	e, err := NewEncoder(f, &EncoderOptions{Bitrate: 128000, VBR: true})
	if err != nil {
		t.Fatal(err)
	}
	if e.Bitrate() != 0 {
		t.Errorf("VBR Bitrate() = %d, want 0 (plan contract)", e.Bitrate())
	}
	pkts := encodeSignal(t, e, chans, 1152)

	rates := map[int]int{}
	for _, p := range pkts {
		h, err := ParseHeader(p)
		if err != nil {
			t.Fatal(err)
		}
		if h.Size() != len(p) {
			t.Fatalf("frame size %d, header says %d", len(p), h.Size())
		}
		if h.Padding {
			t.Error("VBR frames never pad")
		}
		rates[h.Bitrate]++
	}
	if len(rates) < 2 {
		t.Errorf("VBR stream used a single rate %v; frames are not sizing to content", rates)
	}
	out := decodeFrames(t, f, pkts)
	if _, snr := bestLagSNR(chans[0], out[0], 1600); snr < 25 {
		t.Errorf("VBR round-trip SNR %.1f dB below 25", snr)
	}
	t.Logf("VBR rates used: %v", rates)
}
