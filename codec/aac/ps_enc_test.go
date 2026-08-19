package aac

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// TestPSHuffEncTableRoundTrip pins the encode tables against the decode
// trees they are walked from: every value each run can produce must
// code, and decode back to itself through the parser's own walker.
func TestPSHuffEncTableRoundTrip(t *testing.T) {
	tabs := []struct {
		name   string
		enc    *psHuffEncTable
		tree   [][2]int16
		lo, hi int
	}{
		{"IIDDF", &psEncIIDDF, psTreeIIDDF, -14, 14},
		{"ICCDF", &psEncICCDF, psTreeICCDF, -7, 7},
	}
	for _, tc := range tabs {
		for d := tc.lo; d <= tc.hi; d++ {
			if tc.enc.bits[d+30] == 0 {
				t.Errorf("%s: delta %d has no code", tc.name, d)
				continue
			}
			var w bitWriter
			tc.enc.write(&w, int32(d))
			w.align()
			r := newBitReader(w.buf)
			if got := psHuffDecode(r, tc.tree); got != d {
				t.Errorf("%s: delta %d decodes to %d", tc.name, d, got)
			}
		}
	}
}

// v2Encoder builds a stereo 48 kHz HE-AAC v2 encoder.
func v2Encoder(t *testing.T, bitrate int) (*HEEncoder, audio.Format) {
	t.Helper()
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
		Type: audio.Float, BitDepth: 32}
	e, err := NewHEEncoder(f, &EncoderOptions{Bitrate: bitrate, ParametricStereo: true})
	if err != nil {
		t.Fatal(err)
	}
	return e, f
}

// TestHEEncoderV2ASC pins the v2 AudioSpecificConfig: the hierarchical
// AOT-29 form over the mono core, byte-exact, parsing back to the HE
// identity with a stereo output format.
func TestHEEncoderV2ASC(t *testing.T) {
	e, f := v2Encoder(t, 0)
	asc := e.CodecConfig()
	want := []byte{0xEB, 0x09, 0x88, 0x00}
	if !slices.Equal(asc, want) {
		t.Fatalf("ASC = % X, want % X", asc, want)
	}
	cfg, err := ParseASC(asc)
	if err != nil {
		t.Fatal(err)
	}
	if TrackID(cfg) != codec.HEAAC || !cfg.PS || !cfg.psDecode() ||
		cfg.SampleRate != 24000 || cfg.ExtensionRate != 48000 ||
		cfg.ChannelConfig != 1 || cfg.OutputSamplesPerAU() != 2048 {
		t.Errorf("ASC parses to %+v", cfg)
	}
	got, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	if got != f {
		t.Errorf("round-tripped format %v, want %v", got, f)
	}
	if e.Bitrate() != HEV2DefaultBitrate {
		t.Errorf("default bitrate %d, want %d", e.Bitrate(), HEV2DefaultBitrate)
	}
}

// TestHEEncoderV2MonoRefused pins the named refusal: v2 codes a stereo
// pair, so a mono format fails construction (which is plan time for the
// engine row).
func TestHEEncoderV2MonoRefused(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 1, Layout: audio.FrontCenter,
		Type: audio.Float, BitDepth: 32}
	if _, err := NewHEEncoder(f, &EncoderOptions{ParametricStereo: true}); err == nil {
		t.Fatal("mono input accepted under ParametricStereo, want a named refusal")
	}
}

// stereoSig synthesizes a stereo test pair: right = transform(left).
func stereoSig(n int, right func(v float64, i int) float64) [][]float32 {
	out := [][]float32{make([]float32, n), make([]float32, n)}
	seed := uint64(17)
	for i := 0; i < n; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		noise := 0.02 * (2*float64(seed>>11)/float64(1<<53) - 1)
		v := 0.25*math.Sin(2*math.Pi*440*float64(i)/48000) +
			0.18*math.Sin(2*math.Pi*2100*float64(i)/48000) +
			0.12*math.Sin(2*math.Pi*9500*float64(i)/48000) +
			0.08*math.Sin(2*math.Pi*14100*float64(i)/48000) + noise
		out[0][i] = float32(v)
		out[1][i] = float32(right(v, i))
	}
	return out
}

// TestHEEncoderV2PayloadRoundTrip parses every written access unit back
// through the decoder's own parser: the SBR side must survive as for v1,
// and the ps_data must land with the header's modes, one envelope, and
// the encoder's exact quantized IID/ICC values.
//
// The signal carries a genuinely wide image (tones panned alternately
// hard left and right, independent noise beds, a drifting pan) so the
// IID/ICC runs vary per band and the freq-coded refresh frames push
// ps_data past 14 bytes: the extended-data block's escaped
// bs_extension_size then actually serializes, which is the path a
// band-flat corpus never reaches (a crossed escape base once cost every
// such frame its whole SBR payload, and every fixture was blind to it).
func TestHEEncoderV2PayloadRoundTrip(t *testing.T) {
	const frames = 30
	in := wideImageSig(frames * 2048)
	e, f := v2Encoder(t, 32000)

	var wantIID, wantICC [psEncBands]int32
	aus, escaped := 0, false

	// Elements are entropy-coded with no length prefix, so the way to the
	// fill is the full syntax parse: drive each AU through this package's
	// decoder and read the PS state off its element.
	cfg, err := ParseASC(e.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	df, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDecoder(cfg, df)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()

	check := func(p codec.Packet) error {
		copy(wantIID[:], e.sbr.ps.iid[:])
		copy(wantICC[:], e.sbr.ps.icc[:])
		escaped = escaped || (2+e.sbr.ps.data.bitLen()+7)/8 >= 15
		if err := d.Decode(p.Data, func(*audio.Buffer) error { return nil }); err != nil {
			t.Fatalf("au %d: decode: %v", aus, err)
		}
		var ps *psDec
		for _, sel := range d.sbrEls {
			if sel != nil {
				if !sel.dataOK {
					t.Fatalf("au %d: SBR payload did not parse", aus)
				}
				ps = sel.ps
			}
		}
		if ps == nil {
			t.Fatalf("au %d: no PS state on the decoded element", aus)
		}
		pd := &ps.data
		if !pd.start || !pd.enableIID || !pd.enableICC || pd.iidQuant != 0 ||
			pd.nrIIDPar != psEncBands || pd.nrICCPar != psEncBands || pd.iccMode != 1 ||
			pd.enableIPDOPD {
			t.Fatalf("au %d: PS header state %+v", aus, pd)
		}
		if pd.numEnv != 1 || pd.border[1] != sbrSlots-1 {
			t.Fatalf("au %d: numEnv %d border %v", aus, pd.numEnv, pd.border)
		}
		for b := 0; b < psEncBands; b++ {
			if int32(pd.iid[0][b]) != wantIID[b] {
				t.Fatalf("au %d band %d: iid parsed %d, wrote %d", aus, b, pd.iid[0][b], wantIID[b])
			}
			if int32(pd.icc[0][b]) != wantICC[b] {
				t.Fatalf("au %d band %d: icc parsed %d, wrote %d", aus, b, pd.icc[0][b], wantICC[b])
			}
		}
		aus++
		return nil
	}

	buf := audio.Get(f, 2048)
	defer audio.Put(buf)
	for off := 0; off < frames*2048; off += 2048 {
		buf.N = 2048
		for c := 0; c < 2; c++ {
			copy(buf.ChanF(c)[:2048], in[c][off:off+2048])
		}
		if err := e.Encode(buf, check); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Finish(check); err != nil {
		t.Fatal(err)
	}
	if aus < frames {
		t.Fatalf("checked %d AUs, want at least %d", aus, frames)
	}
	if !escaped {
		t.Fatal("no AU reached the escaped bs_extension_size; the signal no longer exercises the path this test exists to pin")
	}
}

// TestSBRPayloadRollback pins the drop path's mirror discipline: when
// encodeFrame discards an oversized payload, rollbackPayload must
// restore every delta-time mirror (the SBR anchors and refresh
// countdown; ps_data carries no cross-frame state) exactly, so
// re-rendering the same access unit reproduces the same bytes the
// decoder would have been owed.
func TestSBRPayloadRollback(t *testing.T) {
	e, f := v2Encoder(t, 32000)
	in := wideImageSig(6 * 2048)
	buf := audio.Get(f, 2048)
	defer audio.Put(buf)
	emit := func(codec.Packet) error { return nil }
	for off := 0; off < 6*2048; off += 2048 {
		buf.N = 2048
		for c := 0; c < 2; c++ {
			copy(buf.ChanF(c)[:2048], in[c][off:off+2048])
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	s := e.sbr
	render := func() []byte {
		p := s.payloadFor(3)
		out := append([]byte(nil), p.buf...)
		if p.n > 0 {
			out = append(out, byte(p.cache<<(8-p.n)))
		}
		return out
	}
	first := render()
	if s.anch == s.snapAnch {
		t.Fatal("payloadFor advanced nothing; the rollback test is checking a no-op")
	}
	s.rollbackPayload()
	if s.anch != s.snapAnch || s.refreshCountdown != s.snapRefresh {
		t.Fatal("rollbackPayload left a mirror advanced")
	}
	if again := render(); !slices.Equal(again, first) {
		t.Fatal("re-rendering after rollback produced different bytes; the mirrors did not restore")
	}
}

// wideImageSig synthesizes a stereo pair whose image varies per band and
// per frame: tones panned alternately hard left and right, independent
// noise beds, and a slow pan drift, so quantized IID/ICC differ across
// bands and the delta runs are expensive.
func wideImageSig(n int) [][]float32 {
	out := [][]float32{make([]float32, n), make([]float32, n)}
	tones := []float64{180, 440, 950, 1700, 2600, 3800, 5200, 7000, 9200, 12000}
	s0, s1 := uint64(3), uint64(7919)
	for i := 0; i < n; i++ {
		drift := 0.5 + 0.45*math.Sin(2*math.Pi*float64(i)/48000)
		var l, r float64
		for j, fq := range tones {
			v := 0.12 / float64(j/3+1) * math.Sin(2*math.Pi*fq*float64(i)/48000+float64(j))
			if j%2 == 0 {
				l += v
				r += v * (1 - drift)
			} else {
				r += v
				l += v * drift
			}
		}
		s0 = s0*6364136223846793005 + 1442695040888963407
		s1 = s1*6364136223846793005 + 1442695040888963407
		l += 0.06 * (2*float64(s0>>11)/float64(1<<53) - 1)
		r += 0.06 * (2*float64(s1>>11)/float64(1<<53) - 1)
		out[0][i] = float32(l)
		out[1][i] = float32(r)
	}
	return out
}

// TestHEEncoderV2DelayPin freezes the v2 delay by measurement: the PS
// front end is delay-compensated, so the toneburst train must land at
// exactly the shared HEEncoderDelay through our own decoder, on both
// output channels.
func TestHEEncoderV2DelayPin(t *testing.T) {
	e, f := v2Encoder(t, 0)
	const n = 48000 * 2
	mono, starts, burst := heDelayBurst(n)
	in := [][]float32{mono, append([]float32(nil), mono...)}
	aus := heEncodeAll(t, e, f, in)

	cfg, err := ParseASC(e.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	out := heDecodeAll(t, cfg, aus)
	for c := 0; c < 2; c++ {
		got := heDelayPeak(out[c], starts, burst)
		t.Logf("ch%d matched-filter delay %d (declared %d)", c, got, HEEncoderDelay)
		if got != HEEncoderDelay {
			t.Errorf("ch%d measured delay %d, declared HEEncoderDelay %d", c, got, HEEncoderDelay)
		}
	}
}

// corrOf is the normalized cross-correlation of two equal windows.
func corrOf(a, b []float32) float64 {
	var dot, ea, eb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		ea += float64(a[i]) * float64(a[i])
		eb += float64(b[i]) * float64(b[i])
	}
	if ea <= 0 || eb <= 0 {
		return 0
	}
	return dot / math.Sqrt(ea*eb)
}

func energyOf(x []float32) float64 {
	var e float64
	for _, v := range x {
		e += float64(v) * float64(v)
	}
	return e
}

// TestHEEncoderV2StereoImage pins the reconstruction end to end for the
// stereo shapes that matter: panned pairs at three ratios plus a hard
// pan (IID, including the quantizer's saturation branch and the
// silent-channel downmix path), an anti-phase pair (the phase-aligned
// downmix's whole reason: a passive half-sum cancels to silence), a
// HALF-coherent anti-phase bed (the shape where a blended rotation once
// targeted the zero vector and lost level), and an uncorrelated pair
// (ICC drives the decorrelator). Both channels must come back at the
// source level, per the v1 balance lesson, and the correlation must
// land on the right side.
func TestHEEncoderV2StereoImage(t *testing.T) {
	const n = 48000
	lo, hi := 12000, 44000
	type imageCase struct {
		name     string
		in       [][]float32
		corrLo   float64 // decoded L/R correlation must be at least this
		corrHi   float64 // and at most this
		energyDB float64 // per-channel tolerance against the source
		imageDB  float64 // L/R ratio tolerance, 0 to skip (silent channel)
	}
	var cases []imageCase
	for _, ratio := range []float64{1.0, 0.5, 0.25} {
		g := math.Sqrt(ratio)
		cases = append(cases, imageCase{fmt.Sprintf("panned-%.2f", ratio),
			stereoSig(n, func(v float64, _ int) float64 { return g * v }), 0.8, 1, 3, 2})
	}
	cases = append(cases,
		imageCase{"antiphase", stereoSig(n, func(v float64, _ int) float64 { return -v }), -1, -0.5, 3, 2})

	noise := func(seed uint64) []float32 {
		out := make([]float32, n)
		for i := range out {
			seed = seed*6364136223846793005 + 1442695040888963407
			out[i] = float32(0.25 * (2*float64(seed>>11)/float64(1<<53) - 1))
		}
		return out
	}
	// Uncorrelated: independent noise beds with matched spectra.
	cases = append(cases, imageCase{"uncorrelated",
		[][]float32{noise(5), noise(1009)}, -0.4, 0.4, 3, 2})
	// Half-coherent anti-phase: a shared bed in anti-phase under equal
	// independent noise, smoothed coherence ~0.5 in every band. Full
	// alignment keeps the bed; the old coherence blend cancelled it and
	// saturated the active gain's clamp (~2 dB per-channel loss).
	shared := noise(77)
	n1, n2 := noise(131), noise(211)
	half := [][]float32{make([]float32, n), make([]float32, n)}
	for i := range shared {
		half[0][i] = shared[i] + n1[i]
		half[1][i] = -shared[i] + n2[i]
	}
	cases = append(cases, imageCase{"half-antiphase", half, -0.9, -0.1, 2.5, 2})
	// Hard pan: the right channel is silent, so quantIID saturates at +7
	// (25 dB) and the downmix runs with one dead input. The decoded image
	// is asserted as a floor, not a match: the quantizer tops out at
	// 25 dB where the source ratio is infinite.
	silent := make([]float32, n)
	cases = append(cases, imageCase{"hardpan",
		[][]float32{stereoSig(n, func(v float64, _ int) float64 { return v })[0], silent}, 0, 1, 3, 0})

	for _, tc := range cases {
		e, f := v2Encoder(t, 48000)
		aus := heEncodeAll(t, e, f, tc.in)
		cfg, err := ParseASC(e.CodecConfig())
		if err != nil {
			t.Fatal(err)
		}
		out := heDecodeAll(t, cfg, aus)

		for c := 0; c < 2; c++ {
			want := energyOf(tc.in[c][lo:hi])
			got := energyOf(out[c][lo+HEEncoderDelay : hi+HEEncoderDelay])
			if want == 0 {
				continue // the silent channel is judged by the image floor below
			}
			db := 10 * math.Log10(got/want)
			t.Logf("%s ch%d: %+.2f dB against the source", tc.name, c, db)
			if math.Abs(db) > tc.energyDB {
				t.Errorf("%s ch%d energy off by %+.2f dB, want within %.1f dB", tc.name, c, db, tc.energyDB)
			}
		}
		gotL := energyOf(out[0][lo+HEEncoderDelay : hi+HEEncoderDelay])
		gotR := energyOf(out[1][lo+HEEncoderDelay : hi+HEEncoderDelay])
		if tc.imageDB > 0 {
			gotDB := 10 * math.Log10(gotL/gotR)
			wantDB := 10 * math.Log10(energyOf(tc.in[0][lo:hi])/energyOf(tc.in[1][lo:hi]))
			t.Logf("%s image: decoded L/R %+.2f dB, source %+.2f dB", tc.name, gotDB, wantDB)
			if math.Abs(gotDB-wantDB) > tc.imageDB {
				t.Errorf("%s image off: decoded %+.2f dB vs source %+.2f dB", tc.name, gotDB, wantDB)
			}
			corr := corrOf(out[0][lo+HEEncoderDelay:hi+HEEncoderDelay], out[1][lo+HEEncoderDelay:hi+HEEncoderDelay])
			t.Logf("%s decoded L/R correlation %.3f", tc.name, corr)
			if corr < tc.corrLo || corr > tc.corrHi {
				t.Errorf("%s decoded correlation %.3f outside [%.2f, %.2f]", tc.name, corr, tc.corrLo, tc.corrHi)
			}
		} else {
			// Hard pan: the left must dominate by at least the coded 25 dB
			// step less the envelope slack.
			gotDB := 10 * math.Log10(gotL/gotR)
			t.Logf("%s image: decoded L/R %+.2f dB (source right silent)", tc.name, gotDB)
			if gotDB < 18 {
				t.Errorf("%s decoded L/R %+.2f dB, want at least 18 (the saturated IID step)", tc.name, gotDB)
			}
		}
	}
}

// TestHEEncoderV2ChunkInvariance pins the front end's staging: large
// feeds must produce the same access units as the engine's 2048-sample
// framer (the v1 lesson, now with the downmix FIFO in the path).
func TestHEEncoderV2ChunkInvariance(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
		Type: audio.Float, BitDepth: 32}
	const n = 48000 * 2
	in := stereoSig(n, func(v float64, i int) float64 {
		return 0.7 * math.Cos(2*math.Pi*1300*float64(i)/48000) * v
	})
	encodeChunked := func(chunk int) [][]byte {
		e, err := NewHEEncoder(f, &EncoderOptions{ParametricStereo: true})
		if err != nil {
			t.Fatal(err)
		}
		var aus [][]byte
		emit := func(p codec.Packet) error {
			aus = append(aus, append([]byte(nil), p.Data...))
			return nil
		}
		buf := audio.Get(f, max(chunk, 2048))
		defer audio.Put(buf)
		for off := 0; off < n; off += chunk {
			m := min(chunk, n-off)
			buf.N = m
			for c := 0; c < 2; c++ {
				copy(buf.ChanF(c)[:m], in[c][off:off+m])
			}
			if err := e.Encode(buf, emit); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.Finish(emit); err != nil {
			t.Fatal(err)
		}
		return aus
	}
	ref := encodeChunked(2048)
	for _, chunk := range []int{1000, 4096, 32768} {
		got := encodeChunked(chunk)
		if len(got) != len(ref) {
			t.Fatalf("chunk %d: %d AUs, want %d", chunk, len(got), len(ref))
		}
		for i := range ref {
			if !slices.Equal(got[i], ref[i]) {
				t.Fatalf("chunk %d: AU %d differs from the 2048-fed reference", chunk, i)
			}
		}
	}
}
