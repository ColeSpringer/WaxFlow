package waxflow_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/hls"
)

// wavFrom renders a buffer as a WAV, so a test can cut one continuous signal
// into several files and put it back together.
func wavFrom(t *testing.T, cfg pcm.Config, buf *audio.Buffer) []byte {
	t.Helper()
	enc, err := pcm.NewEncoder(cfg, buf.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(),
		Fmt: buf.Fmt, Samples: int64(buf.N), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return ws.b
}

// ratedWAV renders frames of synthetic PCM at rate as a WAV, plus the
// source-of-truth buffer.
func ratedWAV(t *testing.T, rate, channels, frames int, seed uint64) ([]byte, *audio.Buffer) {
	t.Helper()
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	buf := audio.Get(f, frames)
	buf.N = frames
	synth(buf, seed)
	return wavFrom(t, cfg, buf), buf
}

// members probes each WAV and wires it as a timeline member, the way a
// caller with a play queue does: plan from the headers, open on demand.
func timelineMembers(t *testing.T, e *waxflow.Engine, raws ...[]byte) []waxflow.ConcatSource {
	t.Helper()
	out := make([]waxflow.ConcatSource, len(raws))
	for i, raw := range raws {
		info, err := e.Probe(container.BytesSource(raw), "wav", nil)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = waxflow.ConcatSource{
			Track: info.Default(),
			Open:  func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), "wav") },
		}
	}
	return out
}

// drain reads a Media to end of stream into one buffer, failing if it
// delivers more than capacity frames.
func drainMedia(t *testing.T, med format.Media, capacity int) *audio.Buffer {
	t.Helper()
	f := med.Info().Default().Fmt
	out := audio.Get(f, capacity)
	out.N = 0
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.N+tmp.N > out.Cap() {
			t.Fatalf("the timeline delivered more than the %d frames it promised", capacity)
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
}

// TestConcatGaplessSeam is the primitive's headline claim, checked the only
// way that means anything: cut one continuous signal in two, hand the pieces
// back as a timeline, and require the result to be the original bit for bit.
// A seam that lost, repeated, or filtered a single sample fails here.
//
// The cut is deliberately not on a chunk boundary, so the seam falls inside
// what would otherwise be one read.
func TestConcatGaplessSeam(t *testing.T) {
	const frames, cut = 30000, 12345
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 91)

	head, tail := audio.Get(f, cut), audio.Get(f, frames-cut)
	defer audio.Put(head)
	defer audio.Put(tail)
	head.N, tail.N = cut, frames-cut
	audio.CopyFrames(head, 0, whole, 0, cut)
	audio.CopyFrames(tail, 0, whole, cut, frames-cut)

	e := waxflow.New()
	med, err := waxflow.Concat(timelineMembers(t, e, wavFrom(t, cfg, head), wavFrom(t, cfg, tail)), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	if got := med.Info().Default().Samples; got != frames {
		t.Fatalf("the timeline promises %d samples, want %d", got, frames)
	}
	got := drainMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, whole, got)
}

// TestConcatNoDiscontAtSeam pins the rule that makes the seam invisible to
// everything downstream. A member's own first chunk may be marked as a
// discontinuity; passing that through would drain the downstream resampler
// to end of stream and re-anchor it, which both puts back the seam this
// primitive exists to remove and changes the timeline's length from
// ceil((nA+nB)*L/M) to ceil(nA*L/M)+ceil(nB*L/M).
//
// The crossfade row is the refutation of a diagnosis, and it belongs here
// rather than in a test of its own: a blend was read as blocked by this very
// override ("a pure override, never an OR"). It is the opposite. A zone's
// chunks are the incoming member's freshly opened first chunks, which are
// exactly the chunks that arrive marked, so the override is the prerequisite
// that makes a blend invisible rather than the obstacle to one.
func TestConcatNoDiscontAtSeam(t *testing.T) {
	for _, tc := range []struct {
		name string
		x    int64
	}{
		{"a butt-join", 0},
		{"a crossfade", 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := waxflow.New()
			a, _ := ratedWAV(t, 48000, 2, 9000, 5)
			b, _ := ratedWAV(t, 48000, 2, 9000, 6)
			med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{Crossfade: tc.x})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()

			buf := audio.Get(med.Info().Default().Fmt, 1024)
			defer audio.Put(buf)
			for n := 0; ; n++ {
				err := med.ReadChunk(buf)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if buf.Discont {
					t.Fatalf("chunk %d at position %d is marked as a discontinuity; "+
						"a timeline read from the top is continuous everywhere", n, buf.Pos)
				}
			}
		})
	}
}

// dcWAV renders frames of constant DC as a WAV.
//
// A curve is only legible against a signal that holds still. With DC in and DC
// out, every sample of a zone is the curve and nothing else, so the test can
// compute what each one must be rather than assert something weaker about it.
func dcWAV(t *testing.T, rate, channels, frames int, dc int32) []byte {
	t.Helper()
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	buf := audio.Get(f, frames)
	defer audio.Put(buf)
	buf.N = frames
	for c := 0; c < channels; c++ {
		s := buf.ChanI(c)
		for i := range s {
			s[i] = dc
		}
	}
	return wavFrom(t, cfg, buf)
}

// TestConcatCrossfadeCurve is the headline, and it pins the whole primitive in
// one assertion: the curve, the half-open parametrization, the rounding, the
// endpoints, and the declick itself.
//
// Two DC members of opposite sign meet at a zone. Every sample of that zone
// must be 10000*cos(k*pi/2X) - 10000*sin(k*pi/2X), computed here rather than
// read back from the code under test, so an equal-power blend that is subtly
// the wrong curve fails. The endpoints are the part that makes a declick a
// declick: the zone's first sample is A's own, exactly, and the first sample
// after the zone is B's own, exactly, because t runs [0,1) across the zone and
// reaches 1 only at the frame past it.
//
// And the point of the whole feature, in one number: at X=512 a 20000-sample
// step becomes a slew of about 43 per sample.
//
// # Why one of the lengths is not a power of two
//
// Every X a caller is likely to pick is a round binary number, and that is the
// reason to test one that is not. At 512 the phase k/X is exactly
// representable, so the curve is built from exact inputs and the ordinary case
// (a phase that rounds, for most k) never runs. 441 covers it, and lands the
// zone at a different offset in the members while it is there.
//
// It does not pin the *form* of the phase expression, which is worth writing
// down because the form looks fragile and is not. Hoisting the constant into a
// step (dTheta = (pi/2)/X, then k*dTheta) does shift the phase by an ulp for a
// non-power-of-two X -- about a quarter of the gains at 441 -- and changes no
// delivered sample at all, in either domain: the int path rounds to an integer
// and the float path narrows the gains to float32, and both swallow a 1e-16
// perturbation whole. Measured rather than assumed. So no test here can tell
// the two forms apart, and none should try.
func TestConcatCrossfadeCurve(t *testing.T) {
	for _, tc := range []struct {
		name string
		x    int
	}{
		{"a power-of-two zone", 512},
		{"a zone that is not a power of two", 441},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const lenA, lenB = 4000, 4000
			const dcA, dcB = 10000, -10000
			x := tc.x
			total := lenA + lenB - x
			zoneAt := lenA - x

			e := waxflow.New()
			a := dcWAV(t, 48000, 2, lenA, dcA)
			b := dcWAV(t, 48000, 2, lenB, dcB)
			med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{Crossfade: int64(x)})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()

			if got := med.Info().Default().Samples; got != int64(total) {
				t.Fatalf("the timeline promises %d samples, want %d", got, total)
			}
			got := drainMedia(t, med, total)
			defer audio.Put(got)
			if got.N != total {
				t.Fatalf("the timeline delivered %d samples, want %d", got.N, total)
			}

			for c := 0; c < 2; c++ {
				s := got.ChanI(c)
				for i := 0; i < zoneAt; i++ {
					if s[i] != dcA {
						t.Fatalf("ch%d[%d] = %d before the zone, want A's %d untouched", c, i, s[i], dcA)
					}
				}
				for k := 0; k < x; k++ {
					th := float64(k) / float64(x) * (math.Pi / 2)
					want := int32(math.Floor(dcA*math.Cos(th) + dcB*math.Sin(th) + 0.5))
					if s[zoneAt+k] != want {
						t.Fatalf("zone[%d] on ch%d = %d, want %d (equal-power cos out, sin in, "+
							"phase as k/X*(pi/2), rounded the quantizer's way)", k, c, s[zoneAt+k], want)
					}
				}
				for i := zoneAt + x; i < total; i++ {
					if s[i] != dcB {
						t.Fatalf("ch%d[%d] = %d after the zone, want B's %d untouched", c, i, s[i], dcB)
					}
				}
				// The endpoints, called out rather than left to fall out of the
				// loop above: they are the property the zone's half-open
				// interval exists to deliver, and the reason there is no dither.
				if s[zoneAt] != dcA {
					t.Errorf("the zone's first sample on ch%d is %d, want A's own %d exactly: "+
						"t starts at 0, so the outgoing member is at full gain there", c, s[zoneAt], dcA)
				}
				if s[zoneAt+x] != dcB {
					t.Errorf("the first sample past the zone on ch%d is %d, want B's own %d exactly: "+
						"t reaches 1 at the frame after the zone, not the last frame of it", c, s[zoneAt+x], dcB)
				}
				// The declick, which is what the caller actually asked for. The
				// bound comes from the curve rather than a constant: the
				// steepest point of A*cos+B*sin is hypot(A,B) per radian, and
				// the zone spans pi/2 over X frames.
				want := math.Hypot(dcA, dcB) * (math.Pi / 2) / float64(x)
				var worst int32
				for k := zoneAt; k < zoneAt+x; k++ {
					if d := s[k+1] - s[k]; d > worst {
						worst = d
					} else if -d > worst {
						worst = -d
					}
				}
				if float64(worst) > want+1 {
					t.Errorf("the steepest step across the zone on ch%d is %d; an equal-power fade over %d "+
						"samples turns the %d step at this seam into a slew of about %.0f",
						c, worst, x, dcA-dcB, want)
				}
			}
		})
	}
}

// noiseWAV renders frames of uncorrelated half-scale noise as a WAV, plus the
// buffer it came from.
//
// Half scale is deliberate. An equal-power blend of two uncorrelated
// full-scale signals reaches 1.414 times full scale where they meet, so it
// would saturate, and saturation is precisely what an RMS measurement would
// notice: the level would come back low and the curve would take the blame.
// TestConcatCrossfadeSaturates is where the rails are the point.
func noiseWAV(t *testing.T, rate, channels, frames int, seed uint64) ([]byte, *audio.Buffer) {
	t.Helper()
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	buf := audio.Get(f, frames)
	buf.N = frames
	synth(buf, seed)
	for c := 0; c < channels; c++ {
		s := buf.ChanI(c)
		for i := range s {
			s[i] /= 2
		}
	}
	return wavFrom(t, cfg, buf), buf
}

// rms is the root-mean-square of n frames of b from frame off, over every
// channel.
func rms(b *audio.Buffer, off, n int) float64 {
	var sum float64
	for c := 0; c < b.Fmt.Channels; c++ {
		s := b.ChanI(c)
		for i := off; i < off+n; i++ {
			sum += float64(s[i]) * float64(s[i])
		}
	}
	return math.Sqrt(sum / float64(n*b.Fmt.Channels))
}

// TestConcatCrossfadeEqualPower pins the claim rather than the code: across a
// blend of uncorrelated material, the level holds.
//
// That is what equal-power means and why the curve is cos/sin rather than a
// straight line. Two uncorrelated signals summed with gains that square to one
// keep their power, so the zone's RMS is the members' RMS. The same zone under
// a linear fade dips to 0.707 of it at the midpoint, which is 3 dB, and 3 dB
// is audible: it is the dip in the middle of every naive crossfade.
func TestConcatCrossfadeEqualPower(t *testing.T) {
	const x, lenA, lenB = 4096, 40000, 40000
	const total = lenA + lenB - x
	const zoneAt = lenA - x

	e := waxflow.New()
	a, abuf := noiseWAV(t, 48000, 2, lenA, 21)
	defer audio.Put(abuf)
	b, bbuf := noiseWAV(t, 48000, 2, lenB, 22)
	defer audio.Put(bbuf)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{Crossfade: x})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	got := drainMedia(t, med, total)
	defer audio.Put(got)

	// The members' own level, measured away from the zone, so the comparison
	// is against this material rather than a computed ideal.
	body := rms(got, 0, zoneAt)
	zone := rms(got, zoneAt, x)
	db := 20 * math.Log10(zone/body)
	if math.Abs(db) > 0.5 {
		t.Fatalf("the zone is %.2f dB from the members' own level (zone RMS %.0f, body RMS %.0f); "+
			"an equal-power blend of uncorrelated material holds its level, and a linear fade is the one that dips 3 dB",
			db, zone, body)
	}
}

// TestConcatCrossfadeSaturates guards the rails.
//
// An equal-power blend of correlated material peaks at +3 dB: two identical
// signals at 0.707 each reach 1.414 where they meet. For an Int/16 envelope
// delivered to FLAC, NewChain inserts nothing between the blend and the
// encoder, so an out-of-range int32 goes straight to it.
//
// The rails and the rounding are dither.Quantize's, because the quantizer is
// the only other producer of int samples in the pipeline and a crossfade is
// the first one that is not it. The second assertion is what keeps this test
// honest: the zone must actually reach the rail, or the first assertion is
// passing because nothing ever clipped.
func TestConcatCrossfadeSaturates(t *testing.T) {
	for _, tc := range []struct {
		name string
		dc   int32
		rail int32
	}{
		{"the positive rail", 32000, 32767},
		{"the negative rail", -32000, -32768},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const x, lenA, lenB = 512, 4000, 4000
			const total = lenA + lenB - x
			e := waxflow.New()
			// Both members at the same DC: correlated and in phase, which is
			// the worst case and the one a real crossfade of two takes of the
			// same material actually hits.
			a := dcWAV(t, 48000, 2, lenA, tc.dc)
			b := dcWAV(t, 48000, 2, lenB, tc.dc)
			med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{Crossfade: x})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()

			got := drainMedia(t, med, total)
			defer audio.Put(got)
			hit := false
			for c := 0; c < 2; c++ {
				s := got.ChanI(c)
				for i, v := range s {
					if v > 32767 || v < -32768 {
						t.Fatalf("ch%d[%d] = %d, outside the envelope's 16-bit rails; "+
							"nothing between here and the encoder will bring it back", c, i, v)
					}
					if v == tc.rail {
						hit = true
					}
				}
			}
			if !hit {
				t.Fatalf("no sample reached %d, so this test would pass without any clamp at all: "+
					"two members at DC %d must overshoot the rail by 3 dB where they meet", tc.rail, tc.dc)
			}
		})
	}
}

// TestConcatCrossfadeLengthIdentity is the plan-versus-run gate, over the grid
// where the arithmetic could differ.
//
// Four numbers have to be one number: what ConcatTrack promises from the
// headers, what the formula says, what the built timeline advertises, and what
// a full drain actually delivers. A disagreement between the first and the
// last is the tail 404 ADR-0009 exists to prevent, and it is invisible until
// the last segment of a stream.
func TestConcatCrossfadeLengthIdentity(t *testing.T) {
	for _, shape := range []struct {
		name  string
		rates []int
		lens  []int
	}{
		{"one member", []int{48000}, []int{9000}},
		{"two members", []int{48000, 48000}, []int{9000, 7000}},
		{"three members", []int{48000, 48000, 48000}, []int{9000, 7000, 5000}},
		{"two members, mixed rates", []int{44100, 48000}, []int{9000, 7000}},
		{"three members, mixed rates", []int{44100, 48000, 96000}, []int{9000, 7000, 5000}},
	} {
		for _, xc := range []struct {
			name string
			// fit means "the largest crossfade these members can carry",
			// computed below: the edge of the refusal is where an off-by-one
			// in the fit rule or the last hop shows up.
			fit bool
			x   int64
		}{
			{name: "butt-joined", x: 0},
			{name: "512", x: 512},
			{name: "at the fit limit", fit: true},
		} {
			t.Run(shape.name+", "+xc.name, func(t *testing.T) {
				e := waxflow.New()
				raws := make([][]byte, len(shape.lens))
				for i := range shape.lens {
					raws[i], _ = ratedWAV(t, shape.rates[i], 2, shape.lens[i], uint64(51+i))
				}
				ms := timelineMembers(t, e, raws...)
				tracks := make([]container.Track, len(ms))
				for i := range ms {
					tracks[i] = ms[i].Track
				}
				// The members' lengths on the envelope's timeline, which is
				// what a crossfade is measured in and what the fit rule bounds.
				env0, err := waxflow.ConcatTrack(tracks, waxflow.ConcatOptions{})
				if err != nil {
					t.Fatal(err)
				}
				lens := make([]int64, len(tracks))
				var sum int64
				for i, tr := range tracks {
					lens[i] = resample.OutputLen(tr.Samples, tr.Fmt.Rate, env0.Fmt.Rate)
					sum += lens[i]
				}
				x := xc.x
				if xc.fit {
					x = fitLimit(lens)
				}
				want := sum - int64(len(lens)-1)*x

				copts := waxflow.ConcatOptions{Crossfade: x}
				env, err := waxflow.ConcatTrack(tracks, copts)
				if err != nil {
					t.Fatalf("the fit limit %d was refused: %v", x, err)
				}
				if env.Samples != want {
					t.Fatalf("ConcatTrack promises %d samples, want sum(%d) - %d*%d = %d",
						env.Samples, sum, len(lens)-1, x, want)
				}
				med, err := waxflow.Concat(ms, copts)
				if err != nil {
					t.Fatal(err)
				}
				defer med.Close()
				if got := med.Info().Default().Samples; got != want {
					t.Fatalf("the built timeline advertises %d samples, the plan promised %d", got, want)
				}
				got := drainMedia(t, med, int(want))
				defer audio.Put(got)
				if int64(got.N) != want {
					t.Fatalf("the timeline delivered %d samples against the %d it promised: "+
						"the plan and the run disagree, which is a tail 404 at the last segment", got.N, want)
				}
			})
		}
	}
}

// fitLimit is the largest crossfade these members can carry: every member must
// hold the zones it sits between, and the first and last carry only one.
func fitLimit(lens []int64) int64 {
	limit := int64(math.MaxInt64)
	for i, l := range lens {
		zones := int64(2)
		if i == 0 || i == len(lens)-1 {
			zones = 1
		}
		if len(lens) == 1 {
			continue // no seam, so no zone to fit
		}
		limit = min(limit, l/zones)
	}
	if limit == math.MaxInt64 {
		return 512 // N=1: nothing constrains it, so the column still means something
	}
	return limit
}

// TestConcatCrossfadeZeroIsAButtJoin makes "no nonzero default" testable.
//
// It is TestConcatGaplessSeam's cut with the option set explicitly to zero.
// The gapless album is the artifact this primitive exists to deliver, and a
// crossfade is the one thing that would destroy it; the zero value is the
// promise that asking for a timeline never blends one. This is that promise
// with the field named out loud, so a default that drifted off zero fails here
// rather than in somebody's album.
func TestConcatCrossfadeZeroIsAButtJoin(t *testing.T) {
	const frames, cut = 30000, 12345
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 91)

	head, tail := audio.Get(f, cut), audio.Get(f, frames-cut)
	defer audio.Put(head)
	defer audio.Put(tail)
	head.N, tail.N = cut, frames-cut
	audio.CopyFrames(head, 0, whole, 0, cut)
	audio.CopyFrames(tail, 0, whole, cut, frames-cut)

	e := waxflow.New()
	med, err := waxflow.Concat(timelineMembers(t, e, wavFrom(t, cfg, head), wavFrom(t, cfg, tail)),
		waxflow.ConcatOptions{Crossfade: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	if got := med.Info().Default().Samples; got != frames {
		t.Fatalf("an explicit zero crossfade shortened the timeline to %d, want %d", got, frames)
	}
	got := drainMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, whole, got)
}

// TestConcatPositionsAreContinuous checks the other half of continuity: the
// positions themselves run 0..total with nothing skipped or repeated, which
// is what lets the segmenter treat tfdt as pure arithmetic on the index.
func TestConcatPositionsAreContinuous(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 5)
	b, _ := ratedWAV(t, 48000, 2, 4000, 6)
	c, _ := ratedWAV(t, 48000, 2, 7000, 7)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b, c), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	buf := audio.Get(med.Info().Default().Fmt, 1024)
	defer audio.Put(buf)
	var want int64
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if buf.Pos != want {
			t.Fatalf("chunk starts at %d, want %d", buf.Pos, want)
		}
		want += int64(buf.N)
	}
	if want != 20000 {
		t.Fatalf("the timeline delivered %d samples, want 20000", want)
	}
}

// TestConcatCrossfadeSeekLandsExact is what proves the pre-roll.
//
// Every target across and around the zone must land where it was asked to and
// deliver exactly what a continuous read from 0 delivers there. Landing is the
// easy half; the bit-exactness is the claim, and it is what a seek that
// re-derived the zone some other way (seeking both members into the middle of
// it, say) would fail.
//
// Uniform rates only, and that is the design's own condition rather than a gap
// in the test. seekIntoBlend opens the outgoing member fresh and seeks it to
// its body's end, so on a resampled member buildChain builds a cold chain
// whose FIR window is still zero-filled, and that transient lands at gain
// cos(0) = 1.0, at exactly zone[0] -- the one frame the curve promises is the
// outgoing member's own sample. A uniform timeline builds no chain at all, so
// there is no state to be cold, and that covers both the gapless album and the
// two-slice declick: every caller there is. A mixed-rate row here would assert
// a promise the design does not make. The restart below is where mixed rates
// do survive, and the priming window is the whole difference.
func TestConcatCrossfadeSeekLandsExact(t *testing.T) {
	const x, lenA, lenB = 512, 9000, 9000
	const total = lenA + lenB - x
	const zoneAt = lenA - x

	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, lenA, 31)
	b, _ := ratedWAV(t, 48000, 2, lenB, 32)
	copts := waxflow.ConcatOptions{Crossfade: x}

	// The continuous read, which is the answer every seek below is measured
	// against. A Media is consumed once, so each row builds its own.
	ref, err := waxflow.Concat(timelineMembers(t, e, a, b), copts)
	if err != nil {
		t.Fatal(err)
	}
	whole := drainMedia(t, ref, total)
	defer audio.Put(whole)
	ref.Close()
	if whole.N != total {
		t.Fatalf("the continuous read delivered %d samples, want %d", whole.N, total)
	}

	for _, tc := range []struct {
		name   string
		target int64
	}{
		{"the top", 0},
		{"the last sample before the zone", zoneAt - 1},
		{"the zone's first sample", zoneAt},
		{"the middle of the zone", zoneAt + x/2},
		{"the zone's last sample", zoneAt + x - 1},
		{"the first sample past the zone", zoneAt + x},
		{"the last sample of the timeline", total - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			med, err := waxflow.Concat(timelineMembers(t, e, a, b), copts)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			landed, err := med.SeekSample(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if landed != tc.target {
				t.Fatalf("a seek to %d landed at %d", tc.target, landed)
			}
			got := drainMedia(t, med, total)
			defer audio.Put(got)
			if int64(got.N) != total-tc.target {
				t.Fatalf("a seek to %d left %d samples, want %d", tc.target, got.N, total-tc.target)
			}
			for c := 0; c < 2; c++ {
				w, g := whole.ChanI(c), got.ChanI(c)
				for i := 0; i < got.N; i++ {
					if g[i] != w[int(tc.target)+i] {
						t.Fatalf("ch%d, %d samples after a seek to %d: got %d, a continuous read gives %d. "+
							"A seek into a zone must produce the zone a continuous read produces",
							c, i, tc.target, g[i], w[int(tc.target)+i])
					}
				}
			}
		})
	}
}

// TestConcatCrossfadeDeclicksASlice is WaxTap's own shape, end to end: two
// spans of one rip joined with a short blend, which is what they asked the
// option for. Two Slices into a Concat, and the zone is the declick.
//
// It is worth its own test because it is the composition rather than the
// primitive: Slice is where the members come from, and a slice's track is
// SpanTrack's, so this is also the assertion that a crossfade's arithmetic
// works on lengths that a span computed rather than a file declared.
//
// Half-scale noise, for noiseWAV's reason: at full scale the blend of two
// correlated peaks saturates, and the rails would then be part of what this
// test measures. They are TestConcatCrossfadeSaturates' subject; here the
// curve is, so the fixture leaves the headroom that keeps the two apart.
func TestConcatCrossfadeDeclicksASlice(t *testing.T) {
	const frames, x = 20000, 256
	e := waxflow.New()
	raw, whole := noiseWAV(t, 48000, 2, frames, 41)
	defer audio.Put(whole)

	spans := [][2]int64{{1000, 6000}, {12000, 18000}}
	info, err := e.Probe(container.BytesSource(raw), "wav", nil)
	if err != nil {
		t.Fatal(err)
	}
	ms := make([]waxflow.ConcatSource, len(spans))
	for i, sp := range spans {
		track, err := waxflow.SpanTrack(info.Default(), sp[0], sp[1])
		if err != nil {
			t.Fatal(err)
		}
		ms[i] = waxflow.ConcatSource{
			Track: track,
			Open:  func() (format.Media, error) { return sliceOf(t, e, raw, sp[0], sp[1]), nil },
		}
	}
	med, err := waxflow.Concat(ms, waxflow.ConcatOptions{Crossfade: x})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	lenA := int(spans[0][1] - spans[0][0])
	total := lenA + int(spans[1][1]-spans[1][0]) - x
	if got := med.Info().Default().Samples; got != int64(total) {
		t.Fatalf("the declicked join promises %d samples, want %d", got, total)
	}
	got := drainMedia(t, med, total)
	defer audio.Put(got)

	// The zone blends span A's last x samples with span B's first x, and both
	// are known: they are windows onto one buffer this test still holds.
	zoneAt := lenA - x
	for c := 0; c < 2; c++ {
		w, g := whole.ChanI(c), got.ChanI(c)
		for k := 0; k < x; k++ {
			th := float64(k) / float64(x) * (math.Pi / 2)
			out := float64(w[int(spans[0][0])+zoneAt+k])
			in := float64(w[int(spans[1][0])+k])
			want := int32(math.Floor(out*math.Cos(th) + in*math.Sin(th) + 0.5))
			if g[zoneAt+k] != want {
				t.Fatalf("zone[%d] on ch%d = %d, want %d: the blend of span A's sample %d and span B's sample %d",
					k, c, g[zoneAt+k], want, int(spans[0][0])+zoneAt+k, int(spans[1][0])+k)
			}
		}
		// Outside the zone each span is its own audio, untouched.
		if g[zoneAt-1] != w[int(spans[0][0])+zoneAt-1] {
			t.Errorf("ch%d: the sample before the zone is %d, want span A's own %d", c, g[zoneAt-1], w[int(spans[0][0])+zoneAt-1])
		}
		if g[zoneAt+x] != w[int(spans[1][0])+x] {
			t.Errorf("ch%d: the sample after the zone is %d, want span B's own %d", c, g[zoneAt+x], w[int(spans[1][0])+x])
		}
	}
}

// TestConcatMixedRatesLength pins the arithmetic that the plan and the run
// must agree on for a mixed queue: each member is resampled on its own, so
// the timeline's length is the sum of the members' own exact output counts,
// not the count of the summed input.
func TestConcatMixedRatesLength(t *testing.T) {
	const n44, n48 = 44100, 48000
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, n44, 11)
	b, _ := ratedWAV(t, 48000, 2, n48, 12)
	ms := timelineMembers(t, e, a, b)

	env, err := waxflow.ConcatTrack([]container.Track{ms[0].Track, ms[1].Track}, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if env.Fmt.Rate != 48000 {
		t.Fatalf("envelope rate %d, want 48000 (the maximum, so no member loses information)", env.Fmt.Rate)
	}
	want := resample.OutputLen(n44, 44100, 48000) + int64(n48)
	if env.Samples != want {
		t.Fatalf("the timeline promises %d samples, want %d", env.Samples, want)
	}

	med, err := waxflow.Concat(ms, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got := drainMedia(t, med, int(want)+1)
	defer audio.Put(got)
	// The run has to deliver exactly what the headers promised: a plan that
	// over-promises is a tail 404, and one that under-promises is an early
	// ENDLIST.
	if int64(got.N) != want {
		t.Fatalf("the timeline delivered %d samples, its own track promised %d", got.N, want)
	}
}

// TestConcatLengthDrift pins the enforcement an advisory length forces. A
// declared total that the file does not deliver is a tolerated oddity for
// one file (format.Media says so) and fatal for a timeline: the drift
// desyncs the prefix sum and every position after it, so the playlist
// promises segments the stream cannot fill. The run fails instead, which is
// why a timeline's mint measures any member whose length is not exact.
func TestConcatLengthDrift(t *testing.T) {
	e := waxflow.New()
	raw, _ := ratedWAV(t, 48000, 2, 9000, 21)

	for _, tc := range []struct {
		name    string
		declare int64
	}{
		{"a member shorter than its headers claim", 9500},
		{"a member longer than its headers claim", 8500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := timelineMembers(t, e, raw, raw)
			ms[0].Track.Samples = tc.declare
			med, err := waxflow.Concat(ms, waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			f := med.Info().Default().Fmt
			buf := audio.Get(f, 1024)
			defer audio.Put(buf)
			for {
				err := med.ReadChunk(buf)
				if err == io.EOF {
					t.Fatal("the timeline ran to the end with a member that lied about its length")
				}
				if err != nil {
					return // the drift was caught, which is the point
				}
			}
		})
	}
}

// TestConcatSeekLandsExact walks a seek target across the seam, which is the
// one landing where the member switch and the seek happen at once.
//
// The grid includes the seam sample itself, not only its neighbours, and
// every landing is checked twice: that it reports the sample it was asked
// for, and that the audio from there is bit-exact against the unsplit
// original. The second is what stops a micro-click passing as "landed
// correctly".
func TestConcatSeekLandsExact(t *testing.T) {
	const frames, cut = 30000, 12345
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 77)

	head, tail := audio.Get(f, cut), audio.Get(f, frames-cut)
	defer audio.Put(head)
	defer audio.Put(tail)
	head.N, tail.N = cut, frames-cut
	audio.CopyFrames(head, 0, whole, 0, cut)
	audio.CopyFrames(tail, 0, whole, cut, frames-cut)
	rawA, rawB := wavFrom(t, cfg, head), wavFrom(t, cfg, tail)

	e := waxflow.New()
	for _, target := range []int64{0, 1, cut - 1, cut, cut + 1, cut + 4096, frames - 1} {
		t.Run("", func(t *testing.T) {
			med, err := waxflow.Concat(timelineMembers(t, e, rawA, rawB), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			landed, err := med.SeekSample(target)
			if err != nil {
				t.Fatal(err)
			}
			if landed != target {
				t.Fatalf("seek to %d landed at %d", target, landed)
			}
			rest := drainMedia(t, med, frames)
			defer audio.Put(rest)
			if int64(rest.N) != frames-target {
				t.Fatalf("after seeking to %d the timeline delivered %d samples, want %d",
					target, rest.N, frames-target)
			}
			want := audio.Get(f, int(frames-target))
			defer audio.Put(want)
			want.N = int(frames - target)
			audio.CopyFrames(want, 0, whole, int(target), want.N)
			equalPCM(t, want, rest)
		})
	}
}

// TestConcatSeekMarksDiscont pins where a discontinuity does belong: a seek
// is one, a seam is not, and the chunk after a seek must say so or the
// downstream chain will filter across a splice that really happened.
func TestConcatSeekMarksDiscont(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 31)
	b, _ := ratedWAV(t, 48000, 2, 9000, 32)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if _, err := med.SeekSample(9000); err != nil { // the seam sample itself
		t.Fatal(err)
	}
	buf := audio.Get(med.Info().Default().Fmt, 1024)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if !buf.Discont {
		t.Fatal("the chunk after a seek is not marked as a discontinuity")
	}
	if buf.Pos != 9000 {
		t.Fatalf("the chunk after seeking to the seam starts at %d, want 9000", buf.Pos)
	}
	if err := med.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if buf.Discont {
		t.Fatal("the discontinuity mark stuck to a second chunk")
	}
}

// TestConcatSeekMixedRatesLandsExact is the same landing guarantee on the
// path that has to work for it: a member behind a resampler, whose source
// timeline runs at another rate, so the seek maps back to it, lands at or
// before the target, and discards the slop.
func TestConcatSeekMixedRatesLandsExact(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, 44100, 41)
	b, _ := ratedWAV(t, 48000, 2, 48000, 42)
	seam := resample.OutputLen(44100, 44100, 48000)

	for _, target := range []int64{0, 5000, seam - 1, seam, seam + 1, seam + 24000} {
		t.Run("", func(t *testing.T) {
			med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			landed, err := med.SeekSample(target)
			if err != nil {
				t.Fatal(err)
			}
			if landed != target {
				t.Fatalf("seek to %d landed at %d", target, landed)
			}
			buf := audio.Get(med.Info().Default().Fmt, 1024)
			defer audio.Put(buf)
			if err := med.ReadChunk(buf); err != nil {
				t.Fatal(err)
			}
			if buf.Pos != target {
				t.Fatalf("after seeking to %d the next chunk starts at %d", target, buf.Pos)
			}
		})
	}
}

// TestConcatSeekPastEnd pins the end-of-stream landing a single Media gives,
// which is what the exact-length walk (measureSamples) rides on.
func TestConcatSeekPastEnd(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 9000, 51)
	b, _ := ratedWAV(t, 48000, 2, 4000, 52)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	landed, err := med.SeekSample(1 << 40)
	if err != nil {
		t.Fatal(err)
	}
	if landed != 13000 {
		t.Fatalf("a past-the-end seek landed at %d, want the timeline's length 13000", landed)
	}
	buf := audio.Get(med.Info().Default().Fmt, 128)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err != io.EOF {
		t.Fatalf("reading past the end returned %v, want io.EOF", err)
	}
}

// TestConcatComposite pins the capability gate: a timeline reports its
// members' own tracks, which Info's synthetic envelope cannot.
func TestConcatComposite(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, 4410, 61)
	b, _ := ratedWAV(t, 48000, 2, 4800, 62)
	med, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	comp, ok := med.(format.Composite)
	if !ok {
		t.Fatal("a timeline does not satisfy format.Composite")
	}
	got := comp.Members()
	if len(got) != 2 || got[0].Fmt.Rate != 44100 || got[1].Fmt.Rate != 48000 {
		t.Fatalf("Members() = %v, want the two members' own tracks", got)
	}
}

// TestPlanSegmentsTimelineCrossfadeVersion pins the third hole, and the one
// guarantee that keeps it from costing anything.
//
// A crossfade is not a node's revision: the blend happens inside the timeline,
// so nothing else in the key can name it, and two timelines over identical
// members with different blends are different audio out of the same code. The
// length is in the entry for the same reason the profile is in resample-hq-1:
// it is part of what was done, not a parameter of it.
//
// The X=0 row is the compatibility guarantee. Every timeline cached before
// crossfades existed was butt-joined, so emitting nothing at zero is what
// keeps every one of those entries valid rather than silently re-encoding the
// world.
func TestPlanSegmentsTimelineCrossfadeVersion(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, 48000, 61)
	b, _ := ratedWAV(t, 48000, 2, 48000, 62)
	ms := timelineMembers(t, e, a, b)
	tracks := []container.Track{ms[0].Track, ms[1].Track}
	opts := waxflow.TranscodeOptions{Format: "flac"}

	versionsAt := func(x int64) []string {
		t.Helper()
		plan, err := e.PlanSegmentsTimeline(tracks, waxflow.ConcatOptions{Crossfade: x}, opts, 4)
		if err != nil {
			t.Fatal(err)
		}
		return plan.Versions
	}
	hasXfade := func(vs []string) string {
		for _, v := range vs {
			if strings.HasPrefix(v, "xfade-") {
				return v
			}
		}
		return ""
	}

	if got := hasXfade(versionsAt(0)); got != "" {
		t.Errorf("a butt-joined timeline keys on %q; every timeline cached before crossfades existed was "+
			"butt-joined, so a zero crossfade has to key exactly as it always did", got)
	}
	if got := hasXfade(versionsAt(512)); got != "xfade-512-1" {
		t.Errorf("a 512-sample crossfade keys on %q, want xfade-512-1", got)
	}
	if a, b := hasXfade(versionsAt(512)), hasXfade(versionsAt(256)); a == b {
		t.Errorf("a 512-sample blend and a 256-sample one both key on %q; they are different audio, "+
			"so one would be served the other's cached segments", a)
	}
}

// TestPlanSegmentsTimelineVersions pins the two holes a synthetic track
// opens in the ADR-0004 cache key, both of which are silent.
//
// The synthetic track's codec is PCM, so the plan names pcm's decoder and no
// member's: a FLAC decoder fix would leave every album's cached segments
// stale. And the plan's own chain runs envelope to output, which here is 48
// kHz to 48 kHz and holds no resampler at all, so it names none of the
// resampling the 44.1 kHz member does to reach the envelope: a resampler
// revision would go unnoticed for exactly the members that ran it.
//
// The second half of the test is the point of the first: the naive plan,
// over the same synthetic track, really is missing both, so the prepend is
// load-bearing rather than belt and braces.
func TestPlanSegmentsTimelineVersions(t *testing.T) {
	e := waxflow.New()
	f44 := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	f48 := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	tracks := []container.Track{
		{Codec: codec.FLAC, Fmt: f44, Samples: 44100, Default: true},
		{Codec: codec.FLAC, Fmt: f44, Samples: 44100, Default: true},
		{Codec: codec.Opus, Fmt: f48, Samples: 48000, Default: true},
	}
	opts := waxflow.TranscodeOptions{Format: "opus"}

	plan, err := e.PlanSegmentsTimeline(tracks, waxflow.ConcatOptions{}, opts, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{flac.Version, opus.Version, resample.HQ.Version()} {
		if !slices.Contains(plan.Versions, want) {
			t.Fatalf("the timeline plan's Versions %v do not name %q, so a revision of it "+
				"would not invalidate this timeline's cached segments", plan.Versions, want)
		}
	}
	if n := strings.Count(strings.Join(plan.Versions, ","), flac.Version); n != 1 {
		t.Fatalf("flac's version appears %d times; the member lists are deduplicated so a "+
			"thousand-member queue still keys on a handful of entries", n)
	}

	env, err := waxflow.ConcatTrack(tracks, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	naive, err := e.PlanSegments(env, opts, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{flac.Version, resample.HQ.Version()} {
		if slices.Contains(naive.Versions, absent) {
			t.Fatalf("planning the synthetic track alone already names %q, so this test is "+
				"no longer showing what PlanSegmentsTimeline is for", absent)
		}
	}
}

// TestHLSReadBackTimeline is the full-stack gapless proof, and the one test
// here that exercises every layer at once: one continuous signal is cut into
// three files, handed back as a timeline, planned, encoded to FLAC, framed
// into CMAF segments, served over HTTP, read back through the HLS client's
// fragmented-MP4 demuxer, and decoded. The result must be the original, bit
// for bit.
//
// Nothing weaker would catch a seam that survives the primitive and dies in
// delivery: TestConcatGaplessSeam proves the sample math and the server suite
// proves the wiring, but only this proves that what a player actually
// receives is what the timeline promised. FLAC is what makes it bit-exact
// rather than merely close.
func TestHLSReadBackTimeline(t *testing.T) {
	const frames = 100000
	cuts := []int{37000, 11000, frames - 48000} // uneven, none on a segment boundary
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 63)

	var raws [][]byte
	at := 0
	for _, n := range cuts {
		part := audio.Get(f, n)
		part.N = n
		audio.CopyFrames(part, 0, whole, at, n)
		raws = append(raws, wavFrom(t, cfg, part))
		audio.Put(part)
		at += n
	}

	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	ms := timelineMembers(t, e, raws...)
	tracks := make([]container.Track, len(ms))
	for i := range ms {
		tracks[i] = ms[i].Track
	}
	plan, err := e.PlanSegmentsTimeline(tracks, waxflow.ConcatOptions{}, opts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Samples != frames {
		t.Fatalf("the timeline plan promises %d samples, want %d", plan.Samples, frames)
	}
	init, err := e.InitSegment(plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	segs := timelineSegments(t, e, raws, waxflow.ConcatOptions{}, opts, plan.SegmentSamples, 0)

	files := map[string][]byte{"/init.mp4": init}
	var media []hls.MediaSegment
	for _, s := range segs {
		name := fmt.Sprintf("seg%d.m4s", s.Index)
		files["/"+name] = s.Data
		media = append(media, hls.MediaSegment{URI: name, Seconds: float64(s.Samples) / 48000})
	}
	files["/v0.m3u8"] = []byte(hls.Media("init.mp4", media))
	files["/master.m3u8"] = []byte(hls.Master([]hls.MasterVariant{
		{URI: "v0.m3u8", Bandwidth: plan.Bandwidth, Codecs: plan.Codecs},
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	med, err := hls.OpenVOD(context.Background(), hls.HTTPFetcher{}, srv.URL+"/master.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got := decodeMedia(t, med, frames)
	defer audio.Put(got)
	equalPCM(t, whole, got)
}

// timelineSegments runs a segmented transcode over a freshly built timeline.
// Every run needs its own Concat: a Media is consumed once, which is the
// same reason a worker restart re-opens its source.
//
// copts is threaded rather than rebuilt from opts, which is ConcatOptions' own
// convention: the plan and the run take the same struct, because a crossfade
// the plan does not know about is a plan promising a length the run does not
// deliver.
func timelineSegments(t *testing.T, e *waxflow.Engine, raws [][]byte, copts waxflow.ConcatOptions,
	opts waxflow.TranscodeOptions, segSamples int, start int64) []mp4.Segment {
	t.Helper()
	med, err := waxflow.Concat(timelineMembers(t, e, raws...), copts)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	var segs []mp4.Segment
	_, err = e.TranscodeSegmentsMedia(context.Background(), med, opts,
		waxflow.SegmentedOptions{SegmentSamples: segSamples, StartSegment: start},
		func(s mp4.Segment) error {
			if want := start + int64(len(segs)); s.Index != want {
				t.Fatalf("segment index %d, want %d", s.Index, want)
			}
			segs = append(segs, s)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

// TestSegmentedRestartAcrossConcatSeam is M23's gate, and it is a test
// rather than an argument on purpose.
//
// The restart guarantee rests on the priming window exceeding every stateful
// node's memory, and the argument extends across a seam because a timeline's
// output over any span is a pure function of position: it holds no
// per-member state that a seek resets differently. That is a good argument.
// It is also exactly the kind of argument the limiter's restart bug hid
// behind, so the seam is placed everywhere it could matter instead: inside
// the priming window, exactly at the restart point, one sample before it,
// and at a member too short to fill the window at all.
func TestSegmentedRestartAcrossConcatSeam(t *testing.T) {
	const segSamples = 49152 // 12 FLAC blocks of 4096
	const restartAt = 4
	const p0 = restartAt * segSamples
	// primeSeconds is 0.1, which is 4800 samples at 48 kHz, rounded up to a
	// whole 4096-sample block: the window opens 8192 samples before p0.
	const prime = 8192

	const frames = 300000

	for _, tc := range []struct {
		name string
		// lens are the members' lengths in samples; they sum to frames.
		lens []int
	}{
		{"the seam sits inside the priming window", []int{p0 - prime/2, frames - (p0 - prime/2)}},
		{"the seam sits exactly at the restart point", []int{p0, frames - p0}},
		{"the seam sits one sample before the restart point", []int{p0 - 1, frames - (p0 - 1)}},
		{"the seam sits before the window opens", []int{p0 - 3*prime, frames - (p0 - 3*prime)}},
		// A member shorter than the window lies entirely inside it, so the
		// restarted run opens, drains, and closes a whole member during
		// priming and has to arrive at p0 with the same state anyway. This
		// is the case that catches per-member bookkeeping a seek would reset
		// differently, which two long members cannot reach.
		{"a member shorter than the priming window, entirely inside it",
			[]int{p0 - 8000, 2000, frames - (p0 - 6000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := waxflow.New()
			var raws [][]byte
			total := 0
			for i, n := range tc.lens {
				raw, _ := ratedWAV(t, 48000, 2, n, uint64(71+i))
				raws = append(raws, raw)
				total += n
			}
			if total != frames {
				t.Fatalf("the members sum to %d samples, want %d", total, frames)
			}
			opts := waxflow.TranscodeOptions{Format: "flac"}
			full := timelineSegments(t, e, raws, waxflow.ConcatOptions{Profile: opts.ResampleProfile}, opts, segSamples, 0)
			tail := timelineSegments(t, e, raws, waxflow.ConcatOptions{Profile: opts.ResampleProfile}, opts, segSamples, restartAt)
			assertSegmentTailMatches(t, full, tail, restartAt)
		})
	}
}

// TestSegmentedRestartAcrossConcatCrossfade is the highest-value test here: a
// restarted worker must produce the continuous run's segments byte for byte,
// with a blend zone sitting wherever it could hurt.
//
// The zone is the hard case because it is the one place a timeline's output is
// not a function of one member's position: it comes from two members and a
// curve, and a restart reaches it by a seek rather than by reading into it. So
// the zone is placed inside the priming window, exactly at the restart point,
// one sample before it, and straddling the point where the priming seek itself
// lands inside the zone (which is the seekIntoBlend path, under a restart).
//
// The mixed-rate row is the one that looks like it contradicts
// TestConcatCrossfadeSeekLandsExact, and does not. That test is uniform-only
// because a seek into a zone captures the outgoing member's tail off a cold
// chain, and on a resampled member the FIR transient lands at zone[0] where
// the gain is 1.0. Here the same thing happens and does not matter, and the
// reason is specific rather than "the window covers it like it covers
// everything else": if the priming seek lands at or after the zone's start
// then pChain >= zoneStart, which is exactly p0 - zoneStart >= chainPrime. The
// transient at zone[0] is therefore at least a whole chain-priming window
// (about 0.1 s, see primeStarts) before the first kept sample, and a FIR
// window is a few dozen samples. It is always discarded before p0. And if the
// seek lands before the zone, the chain is already running when it reaches the
// zone, so the zone is produced exactly as a continuous read produces it. The
// priming window is the whole difference between the two claims.
func TestSegmentedRestartAcrossConcatCrossfade(t *testing.T) {
	const segSamples = 49152 // 12 FLAC blocks of 4096
	const restartAt = 4
	const p0 = restartAt * segSamples
	// primeSeconds is 0.1, which is 4800 samples at 48 kHz, rounded up to a
	// whole 4096-sample block: the window opens 8192 samples before p0.
	const prime = 8192
	const x = 4096
	const lenB = 120000 // enough that the timeline outlives the restart point

	for _, tc := range []struct {
		name  string
		rateA int
		// lenA is member A's length in its own samples.
		lenA int
		// wantZone is where the zone must start on the timeline. Asserted
		// rather than assumed: a row whose zone drifted somewhere harmless
		// would pass while testing nothing, which is the failure mode a
		// fixture has.
		wantZone int64
	}{
		{"the zone sits inside the priming window", 48000, p0 - prime/2 + x, p0 - prime/2},
		{"the zone starts exactly at the restart point", 48000, p0 + x, p0},
		{"the zone starts one sample before the restart point", 48000, p0 + x - 1, p0 - 1},
		// pChain is p0-prime = 188416, which lands 2048 frames into this zone,
		// so the restart's own priming seek is a seek into a blend.
		{"the priming seek lands inside the zone", 48000, p0 - prime - 2048 + x, p0 - prime - 2048},
		// 180633 at 44.1 kHz normalizes to exactly 196608 at 48 kHz, so this
		// zone lands in the priming window like the first row, on a member
		// that reaches the envelope through a resampler.
		{"a mixed-rate timeline, the zone inside the priming window", 44100, 180633, p0 - prime/2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := waxflow.New()
			a, _ := ratedWAV(t, tc.rateA, 2, tc.lenA, 91)
			b, _ := ratedWAV(t, 48000, 2, lenB, 92)
			// The zone starts where member A's normalized length ends, less x.
			normA := resample.OutputLen(int64(tc.lenA), tc.rateA, 48000)
			if got := normA - x; got != tc.wantZone {
				t.Fatalf("this row's zone starts at %d, want %d: the fixture is not testing what it says it is",
					got, tc.wantZone)
			}
			copts := waxflow.ConcatOptions{Crossfade: x}
			opts := waxflow.TranscodeOptions{Format: "flac"}
			raws := [][]byte{a, b}
			full := timelineSegments(t, e, raws, copts, opts, segSamples, 0)
			tail := timelineSegments(t, e, raws, copts, opts, segSamples, restartAt)
			assertSegmentTailMatches(t, full, tail, restartAt)
		})
	}
}

// TestSegmentedRestartAcrossConcatSeamWithGain runs the same seam with the
// limiter engaged, which is the blind spot TestSegmentedRestartFLAC had: with
// no gain the chain holds no decaying node, so the priming horizon is zero
// and the test above proves nothing about one. Here the chain's horizon is
// the limiter's 2 s, and the restart point sits well past it.
func TestSegmentedRestartAcrossConcatSeamWithGain(t *testing.T) {
	const segSamples = 49152
	const restartAt = 4 // 196608 samples = 4.1 s, past the limiter's 2 s horizon
	const frames = 400000
	const seam = restartAt*segSamples - 6000 // inside the priming window

	e := waxflow.New()
	a, _ := ratedWAV(t, 48000, 2, seam, 81)
	b, _ := ratedWAV(t, 48000, 2, frames-seam, 82)
	opts := waxflow.TranscodeOptions{Format: "flac", GainDB: 6}
	full := timelineSegments(t, e, [][]byte{a, b}, waxflow.ConcatOptions{Profile: opts.ResampleProfile}, opts, segSamples, 0)
	tail := timelineSegments(t, e, [][]byte{a, b}, waxflow.ConcatOptions{Profile: opts.ResampleProfile}, opts, segSamples, restartAt)
	assertSegmentTailMatches(t, full, tail, restartAt)
}

func assertSegmentTailMatches(t *testing.T, full, tail []mp4.Segment, startAt int64) {
	t.Helper()
	if int64(len(tail)) != int64(len(full))-startAt {
		t.Fatalf("the restarted run yielded %d segments, the continuous one %d from there",
			len(tail), int64(len(full))-startAt)
	}
	for i, s := range tail {
		if cont := full[int64(i)+startAt]; !bytes.Equal(s.Data, cont.Data) {
			t.Fatalf("restarted segment %d differs from the continuous run (%d bytes vs %d)",
				s.Index, len(s.Data), len(cont.Data))
		}
	}
}
