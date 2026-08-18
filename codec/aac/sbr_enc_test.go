package aac

import (
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// heEncodeAll drives an HEEncoder over interleaved samples and returns
// the emitted access units.
func heEncodeAll(t *testing.T, e *HEEncoder, f audio.Format, samples [][]float32) [][]byte {
	t.Helper()
	var aus [][]byte
	emit := func(p codec.Packet) error {
		if p.Dur != 2048 {
			t.Fatalf("packet dur %d, want 2048", p.Dur)
		}
		aus = append(aus, append([]byte(nil), p.Data...))
		return nil
	}
	n := len(samples[0])
	buf := audio.Get(f, 2048)
	defer audio.Put(buf)
	for off := 0; off < n; off += 2048 {
		m := min(2048, n-off)
		buf.N = m
		for c := 0; c < f.Channels; c++ {
			copy(buf.ChanF(c)[:m], samples[c][off:off+m])
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := e.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Samples != int64(n) || tr.Delay != HEEncoderDelay {
		t.Fatalf("trailer %+v, want samples %d delay %d", tr, n, HEEncoderDelay)
	}
	if got := int64(len(aus))*2048 - tr.Delay - tr.Samples; got != tr.Padding {
		t.Fatalf("padding %d does not close the AU count (want %d)", tr.Padding, got)
	}
	return aus
}

// heDecodeAll decodes access units through this package's own decoder.
func heDecodeAll(t *testing.T, cfg Config, aus [][]byte) [][]float32 {
	t.Helper()
	f, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDecoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	out := make([][]float32, f.Channels)
	for _, au := range aus {
		if err := d.Decode(au, func(b *audio.Buffer) error {
			for c := 0; c < f.Channels; c++ {
				out[c] = append(out[c], b.ChanF(c)[:b.N]...)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestHEEncoderASC pins the encoder's AudioSpecificConfig: the
// hierarchical AOT-5 form, byte-exact ({0x2B, 0x11, 0x88} plus the zero
// padding byte for a 24 kHz core stereo pair), and self-parseable back to
// the HE identity.
func TestHEEncoderASC(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
		Type: audio.Float, BitDepth: 32}
	e, err := NewHEEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	asc := e.CodecConfig()
	want := []byte{0x2B, 0x11, 0x88, 0x00}
	if len(asc) != len(want) {
		t.Fatalf("ASC = % X, want % X", asc, want)
	}
	for i := range want {
		if asc[i] != want[i] {
			t.Fatalf("ASC = % X, want % X", asc, want)
		}
	}
	cfg, err := ParseASC(asc)
	if err != nil {
		t.Fatal(err)
	}
	if TrackID(cfg) != codec.HEAAC || cfg.SampleRate != 24000 || cfg.ExtensionRate != 48000 ||
		cfg.Channels != 2 || cfg.OutputSamplesPerAU() != 2048 {
		t.Errorf("ASC parses to %+v", cfg)
	}
	got, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	if got != f {
		t.Errorf("round-tripped format %v, want %v", got, f)
	}
}

// TestHEEncoderRates pins the accepted output rates: the three dual-rate
// pairs whose core has scalefactor tables, everything else refused by
// name at construction (which is plan time for the engine row).
func TestHEEncoderRates(t *testing.T) {
	for _, rate := range []int{32000, 44100, 48000} {
		f := audio.Format{Rate: rate, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
			Type: audio.Float, BitDepth: 32}
		if _, err := NewHEEncoder(f, nil); err != nil {
			t.Errorf("rate %d refused: %v", rate, err)
		}
	}
	for _, rate := range []int{24000, 22050, 16000, 96000, 8000} {
		f := audio.Format{Rate: rate, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
			Type: audio.Float, BitDepth: 32}
		if _, err := NewHEEncoder(f, nil); err == nil {
			t.Errorf("rate %d accepted, want a named refusal", rate)
		}
	}
}

// heDelayBurst synthesizes the delay-measurement train: Hann-windowed
// 3 kHz tonebursts, band-limited well under the SBR crossover so every
// matched-filter sample rides the exact linear-phase core path. A raw
// impulse would spread energy into the SBR band, whose envelope gating
// is 128-sample-coarse and tips the integer peak by content (measured:
// the same chain reads 3009 or 3010 depending on pulse phasing).
// Returns the signal, the burst start positions, and the burst pattern.
func heDelayBurst(n int) ([]float32, []int, []float32) {
	const burstLen = 480
	burst := make([]float32, burstLen)
	for i := range burst {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/burstLen)
		burst[i] = float32(0.7 * w * math.Sin(2*math.Pi*3000*float64(i)/48000))
	}
	in := make([]float32, n)
	var starts []int
	for p := 5000; p < n-8000; p += 4800 {
		copy(in[p:], burst)
		starts = append(starts, p)
	}
	return in, starts, burst
}

// heDelayPeak is the matched-filter delay estimate: the lag maximizing
// the burst-pattern correlation summed over every burst.
func heDelayPeak(out []float32, starts []int, burst []float32) int {
	bestLag, bestCorr := -1, 0.0
	for lag := 0; lag < 6000; lag++ {
		var corr float64
		for _, p := range starts {
			for i, w := range burst {
				if j := p + lag + i; j < len(out) {
					corr += float64(w) * float64(out[j])
				}
			}
		}
		if corr > bestCorr {
			bestLag, bestCorr = lag, corr
		}
	}
	return bestLag
}

// TestHEEncoderDelayPin freezes HEEncoderDelay by measurement: the
// toneburst train encoded and decoded through this package's own decoder
// must matched-filter at exactly the declared delay. The ffmpeg leg of
// the same measurement lives in the tests module.
func TestHEEncoderDelayPin(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 1, Layout: audio.FrontCenter,
		Type: audio.Float, BitDepth: 32}
	e, err := NewHEEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 48000 * 2
	in, starts, burst := heDelayBurst(n)
	aus := heEncodeAll(t, e, f, [][]float32{in})

	cfg, err := ParseASC(e.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	out := heDecodeAll(t, cfg, aus)[0]
	got := heDelayPeak(out, starts, burst)
	t.Logf("matched-filter delay %d (declared %d)", got, HEEncoderDelay)
	if got != HEEncoderDelay {
		t.Errorf("measured delay %d, declared HEEncoderDelay %d", got, HEEncoderDelay)
	}
}

// TestHEEncoderRoundTrip encodes a bright multitone and checks the decode
// through our own decoder: full-length output, the low band carried
// faithfully through the core path, and real energy above the crossover
// where the source has it (the report's F1 criterion, encode side).
func TestHEEncoderRoundTrip(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
		Type: audio.Float, BitDepth: 32}
	e, err := NewHEEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 48000 * 2
	in := make([][]float32, 2)
	seed := uint64(7)
	for c := range in {
		in[c] = make([]float32, n)
	}
	for i := 0; i < n; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		noise := 0.02 * (2*float64(seed>>11)/float64(1<<53) - 1)
		v := 0.25*math.Sin(2*math.Pi*440*float64(i)/48000) +
			0.20*math.Sin(2*math.Pi*2000*float64(i)/48000) +
			0.15*math.Sin(2*math.Pi*9500*float64(i)/48000) +
			0.10*math.Sin(2*math.Pi*14200*float64(i)/48000) + noise
		in[0][i] = float32(v)
		in[1][i] = float32(0.8 * v)
	}
	aus := heEncodeAll(t, e, f, in)
	cfg, err := ParseASC(e.CodecConfig())
	if err != nil {
		t.Fatal(err)
	}
	out := heDecodeAll(t, cfg, aus)
	if len(out[0]) != len(aus)*2048 {
		t.Fatalf("decoded %d samples from %d AUs", len(out[0]), len(aus))
	}

	// SBR is parametric: the high band carries transposed content shaped
	// to the coded envelopes, not the source waveform, so the assertion is
	// band-region energy in the codec's own QMF domain (steady-state
	// window, delay-compensated), not tone-exact spectra. The cross-decoder
	// and quality gates in the tests module judge the rest.
	lo, hi := 24000, 72000
	bandsOf := func(x []float32) [64]float64 {
		var ana qmfAnalyzer64
		var e [64]float64
		var re, im [64]float32
		for off := 0; off+64 <= len(x); off += 64 {
			ana.analyze(x[off:off+64], re[:], im[:])
			for k := range e {
				e[k] += float64(re[k])*float64(re[k]) + float64(im[k])*float64(im[k])
			}
		}
		return e
	}
	got := bandsOf(out[0][lo+HEEncoderDelay : hi+HEEncoderDelay])
	want := bandsOf(in[0][lo:hi])
	region := func(e *[64]float64, lo, hi int) float64 {
		var s float64
		for k := lo; k <= hi; k++ {
			s += e[k]
		}
		return s
	}
	for _, r := range []struct {
		name   string
		lo, hi int
		tol    float64
	}{
		{"440 Hz core", 0, 2, 2},
		{"2 kHz core", 4, 6, 2},
		{"9.5 kHz sbr", 24, 26, 6},
		{"14.2 kHz sbr", 36, 39, 6},
	} {
		db := 10 * math.Log10(region(&got, r.lo, r.hi)/region(&want, r.lo, r.hi))
		t.Logf("%-12s bands %d-%d: %+.1f dB against the source", r.name, r.lo, r.hi, db)
		if math.Abs(db) > r.tol {
			t.Errorf("%s came through at %+.1f dB, want within %.0f dB", r.name, db, r.tol)
		}
	}
	// The report's F1 criterion, encode side: real energy above 12 kHz.
	if above := region(&got, 33, 60); above < region(&want, 33, 60)/8 {
		t.Errorf("energy above 12 kHz is %.2e, want at least an eighth of the source's %.2e",
			above, region(&want, 33, 60))
	}
}

// TestHEEncoderStereoBalance pins the coupled-stereo balance end to end:
// encode a stereo pair with a known right/left energy ratio, decode with
// this package's own decoder, and require BOTH channels' high bands
// within 3 dB of the source. This is the test whose absence let the
// balance quantizer center on twice the decoder's pan offset (every
// right channel came back ~35 dB down while ch0 absorbed the pair, and
// the mono-downmixing quality metric barely moved).
func TestHEEncoderStereoBalance(t *testing.T) {
	for _, ratio := range []float64{1.0, 0.5, 0.25} {
		f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
			Type: audio.Float, BitDepth: 32}
		e, err := NewHEEncoder(f, nil)
		if err != nil {
			t.Fatal(err)
		}
		const n = 48000
		g := math.Sqrt(ratio)
		in := make([][]float32, 2)
		for c := range in {
			in[c] = make([]float32, n)
		}
		seed := uint64(21)
		for i := 0; i < n; i++ {
			seed = seed*6364136223846793005 + 1442695040888963407
			noise := 0.03 * (2*float64(seed>>11)/float64(1<<53) - 1)
			v := 0.2*math.Sin(2*math.Pi*500*float64(i)/48000) +
				0.15*math.Sin(2*math.Pi*12500*float64(i)/48000) +
				0.1*math.Sin(2*math.Pi*14800*float64(i)/48000) + noise
			in[0][i] = float32(v)
			in[1][i] = float32(g * v)
		}
		aus := heEncodeAll(t, e, f, in)
		cfg, err := ParseASC(e.CodecConfig())
		if err != nil {
			t.Fatal(err)
		}
		out := heDecodeAll(t, cfg, aus)

		hiEnergy := func(x []float32) float64 {
			var ana qmfAnalyzer64
			var re, im [64]float32
			var sum float64
			for off := 0; off+64 <= len(x); off += 64 {
				ana.analyze(x[off:off+64], re[:], im[:])
				for k := 30; k < 48; k++ { // ~11.2-18 kHz
					sum += float64(re[k])*float64(re[k]) + float64(im[k])*float64(im[k])
				}
			}
			return sum
		}
		lo, hi := 12000, 44000
		for c := 0; c < 2; c++ {
			got := hiEnergy(out[c][lo+HEEncoderDelay : hi+HEEncoderDelay])
			want := hiEnergy(in[c][lo:hi])
			db := 10 * math.Log10(got/want)
			t.Logf("ratio %.2f ch%d: high band %+.2f dB against the source", ratio, c, db)
			if math.Abs(db) > 3 {
				t.Errorf("ratio %.2f ch%d high band off by %+.2f dB, want within 3 dB", ratio, c, db)
			}
		}
	}
}

// TestHEEncoderChunkInvariance pins Encode's internal frame-sized
// stepping: the slot ring holds bounded lookback, so a caller feeding
// large buffers must produce the same access units as the engine's
// 2048-sample framer (pre-fix, a 4096-sample feed silently corrupted 38
// of 49 AUs by overwriting ring slots a pending payload still read).
func TestHEEncoderChunkInvariance(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.FrontRight,
		Type: audio.Float, BitDepth: 32}
	const n = 48000 * 2
	in := make([][]float32, 2)
	for c := range in {
		in[c] = make([]float32, n)
	}
	for i := 0; i < n; i++ {
		v := 0.25*math.Sin(2*math.Pi*440*float64(i)/48000) +
			0.15*math.Sin(2*math.Pi*9500*float64(i)/48000)
		in[0][i] = float32(v)
		in[1][i] = float32(0.8 * v)
	}
	encodeChunked := func(chunk int) [][]byte {
		e, err := NewHEEncoder(f, nil)
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
	for _, chunk := range []int{3000, 4096, 32768} {
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
