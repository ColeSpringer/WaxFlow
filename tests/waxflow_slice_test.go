package waxflow_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// sliceOf opens raw and bounds it to [from, to). The Media owns the opened
// source, so closing the slice closes everything.
func sliceOf(t *testing.T, e *waxflow.Engine, raw []byte, from, to int64) format.Media {
	t.Helper()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(med, from, to)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	return sl
}

// TestSliceSplitRoundTrip is M24a's gate, and it proves two things with one
// assertion: a split at arbitrary cut points is sample-exact, and Concat is
// Slice's inverse.
//
// One continuous signal is cut into pieces at cut points that are on no
// convenient boundary, each piece is encoded to FLAC through its own slice,
// then every piece is decoded and concatenated back. The result must be the
// original bit for bit. A transient at any piece's sample 0, a lost or
// repeated sample at any cut, or an off-by-one in the window arithmetic all
// fail here.
//
// FLAC at the source rate is the case that matters and the reason this is
// exact rather than nearly exact: the chain has no resampler and no
// limiter, so there is no state to prime and each piece's sample 0 is the
// source's sample from, bit for bit.
func TestSliceSplitRoundTrip(t *testing.T) {
	const frames = 200000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(44100, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 4242)
	raw := wavFrom(t, cfg, whole)

	// Cut points on no convenient boundary: not chunk-aligned, not
	// frame-aligned, and one of them landing at a CD frame offset the way a
	// real CUE sheet's would (588 samples per frame at 44100).
	cuts := []int64{0, 33 * 588, 45197, 118_000, 176_543, frames}
	e := waxflow.New()

	pieces := make([][]byte, 0, len(cuts)-1)
	for i := 0; i+1 < len(cuts); i++ {
		from, to := cuts[i], cuts[i+1]
		sl := sliceOf(t, e, raw, from, to)
		if got := sl.Info().Default().Samples; got != to-from {
			sl.Close()
			t.Fatalf("piece %d promises %d samples, want %d", i, got, to-from)
		}
		var out bytes.Buffer
		_, err := e.TranscodeMedia(context.Background(), sl, &out, waxflow.TranscodeOptions{Format: "flac"})
		sl.Close()
		if err != nil {
			t.Fatalf("piece %d: %v", i, err)
		}
		pieces = append(pieces, out.Bytes())
	}

	// Rejoin. Probing each piece and wiring it as a timeline member is
	// exactly what a caller with a split album does.
	members := make([]waxflow.ConcatSource, len(pieces))
	for i, p := range pieces {
		info, err := e.Probe(container.BytesSource(p), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		members[i] = waxflow.ConcatSource{
			Track: info.Default(),
			Open:  func() (format.Media, error) { return e.OpenStream(container.BytesSource(p), "flac") },
		}
	}
	joined, err := waxflow.Concat(members, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer joined.Close()
	if got := joined.Info().Default().Samples; got != frames {
		t.Fatalf("the rejoined timeline promises %d samples, want %d", got, frames)
	}
	got := drainMedia(t, joined, frames)
	defer audio.Put(got)
	equalPCM(t, whole, got)
}

// TestSliceWindow pins the window arithmetic directly: the span's sample 0
// is the source's sample from, and it delivers exactly to-from samples.
func TestSliceWindow(t *testing.T) {
	const frames = 40000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 77)
	raw := wavFrom(t, cfg, whole)
	e := waxflow.New()

	for _, tc := range []struct {
		name     string
		from, to int64
	}{
		{"interior", 9001, 27333},
		{"from the top", 0, 12345},
		{"to the end", 31000, frames},
		{"the whole thing", 0, frames},
		{"one sample", 1234, 1235},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sl := sliceOf(t, e, raw, tc.from, tc.to)
			defer sl.Close()
			want := audio.Get(f, int(tc.to-tc.from))
			defer audio.Put(want)
			want.N = int(tc.to - tc.from)
			audio.CopyFrames(want, 0, whole, int(tc.from), want.N)

			got := drainMedia(t, sl, int(tc.to-tc.from))
			defer audio.Put(got)
			if got.N != int(tc.to-tc.from) {
				t.Fatalf("span delivered %d samples, want %d", got.N, tc.to-tc.from)
			}
			equalPCM(t, want, got)
		})
	}
}

// TestSliceEndTrimNeedsNoSeek is F3's case, and it pins a property worth
// keeping: a span that only trims the end never seeks, so it works over a
// source that cannot seek at all.
func TestSliceEndTrimNeedsNoSeek(t *testing.T) {
	const frames = 20000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 3)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(unseekableMedia{med}, 0, 5000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()
	got := drainMedia(t, sl, 5000)
	defer audio.Put(got)
	if got.N != 5000 {
		t.Fatalf("end trim delivered %d samples, want 5000", got.N)
	}
	want := audio.Get(f, 5000)
	defer audio.Put(want)
	want.N = 5000
	audio.CopyFrames(want, 0, whole, 0, 5000)
	equalPCM(t, want, got)
}

// unseekableMedia hides SeekSample behind a refusal, so a test can prove a
// path never reaches for it.
type unseekableMedia struct{ format.Media }

func (unseekableMedia) SeekSample(int64) (int64, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestSliceSeek(t *testing.T) {
	const frames = 40000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 5150)
	raw := wavFrom(t, cfg, whole)
	e := waxflow.New()

	const from, to = 10000, 30000
	sl := sliceOf(t, e, raw, from, to)
	defer sl.Close()

	// A seek inside the window is relative to the window, and lands there
	// exactly: the sample read after it is the source's from+target.
	landed, err := sl.SeekSample(4000)
	if err != nil {
		t.Fatal(err)
	}
	if landed != 4000 {
		t.Fatalf("seek landed at %d, want 4000 on the span's own timeline", landed)
	}
	buf := audio.Get(f, 16)
	defer audio.Put(buf)
	if err := sl.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if buf.Pos != 4000 {
		t.Errorf("chunk after seek is stamped %d, want 4000", buf.Pos)
	}
	want := audio.Get(f, 16)
	defer audio.Put(want)
	want.N = buf.N
	audio.CopyFrames(want, 0, whole, from+4000, buf.N)
	equalPCM(t, want, buf)

	// Past the window's end lands at its end, as a Media does at the end of
	// a file, and reads nothing more.
	landed, err = sl.SeekSample(999999)
	if err != nil {
		t.Fatal(err)
	}
	if landed != to-from {
		t.Fatalf("a seek past the span landed at %d, want its end %d", landed, to-from)
	}
	if err := sl.ReadChunk(buf); err != io.EOF {
		t.Errorf("reading past the span's end = %v, want io.EOF", err)
	}
}

func TestSliceRejects(t *testing.T) {
	const frames = 10000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 1)
	raw := wavFrom(t, cfg, whole)
	e := waxflow.New()

	for _, tc := range []struct {
		name     string
		from, to int64
		want     string
	}{
		{"negative start", -1, 100, "negative span start"},
		{"inverted", 500, 100, "ends before it starts"},
		{"bad sentinel", 0, -7, "want a sample offset"},
		{"start past the end", frames + 1, -1, "past the source's"},
		{"end past the end", 0, frames + 1, "past the source's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			med, err := e.OpenStream(container.BytesSource(raw), "wav")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			_, err = waxflow.Slice(med, tc.from, tc.to)
			if err == nil {
				t.Fatalf("Slice(%d, %d) succeeded, want an error mentioning %q", tc.from, tc.to, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSliceShortSourceFails pins the enforcement half of a bounded span: a
// span declares a length, a plan promises a segment count built from it, so
// a source that ends early has to fail loudly rather than deliver a short
// stream and a tail of 404s.
func TestSliceShortSourceFails(t *testing.T) {
	const frames = 10000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 9)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	// A Media that lies: its headers promise the full length, but it stops
	// early. That is the mismatched-CUE-sheet case, arrived at from the
	// other side.
	sl, err := waxflow.Slice(&truncatedMedia{Media: med, at: 4000}, 0, frames)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()

	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	for {
		err := sl.ReadChunk(buf)
		if err == nil {
			continue
		}
		if err == io.EOF {
			t.Fatal("a span whose source ended early returned io.EOF; it must fail rather than deliver a short stream")
		}
		if !strings.Contains(err.Error(), "do not describe this file") {
			t.Fatalf("error = %v, want it to name the mismatch", err)
		}
		return
	}
}

// truncatedMedia ends the stream early while its Info still promises the
// full length.
type truncatedMedia struct {
	format.Media
	at   int64
	seen int64
}

func (m *truncatedMedia) ReadChunk(dst *audio.Buffer) error {
	if m.seen >= m.at {
		return io.EOF
	}
	if err := m.Media.ReadChunk(dst); err != nil {
		return err
	}
	if left := m.at - m.seen; int64(dst.N) > left {
		dst.N = int(left)
	}
	m.seen += int64(dst.N)
	return nil
}

// flakyMedia refuses its first seeks and then behaves. A source that cannot
// seek right now (a ranged request dropped, a sidecar index rejected
// mid-stream) is the case a span's lazy start has to survive: the start is
// the one thing standing between a read and the wrong audio.
type flakyMedia struct {
	format.Media
	fails int // seeks left to refuse; a negative refuses every one
}

func (m *flakyMedia) SeekSample(target int64) (int64, error) {
	if m.fails != 0 {
		m.fails--
		return 0, errors.New("waxflow_test: the source refused a seek")
	}
	return m.Media.SeekSample(target)
}

// TestSliceFailedStartDeliversNothing pins that a span whose positioning
// failed answers no read with samples anyway.
//
// The failure it catches is silent and total. A span that marks itself
// started before the seek that can fail reports the error exactly once, and
// then reads on from wherever the source happens to sit, which is its own
// sample 0: every sample after that is the wrong audio carrying a position
// the span does not hold, and nothing downstream can tell.
func TestSliceFailedStartDeliversNothing(t *testing.T) {
	const frames, from = 20000, 1000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 24)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(&flakyMedia{Media: med, fails: -1}, from, 5000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()

	buf := audio.Get(f, 8)
	defer audio.Put(buf)
	if err := sl.ReadChunk(buf); err == nil {
		t.Fatal("a span whose seek failed delivered a chunk")
	}
	// The second read is the whole point. The first one reported the
	// failure; a span that took the failure for a position hands this one the
	// source's sample 0, stamped as the window's.
	if err := sl.ReadChunk(buf); err == nil {
		t.Fatalf("the read after a failed seek delivered %d samples stamped %d; "+
			"the span was never positioned, so it has no samples to give", buf.N, buf.Pos)
	}
}

// TestSliceFailedStartRetries pins the other half of the same choice: the
// window's start is one fixed target for the life of the Media, so a source
// that refuses one seek and then answers leaves the span able to reach it,
// and the audio the next read delivers is the window's own.
func TestSliceFailedStartRetries(t *testing.T) {
	const frames, from = 20000, 1000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 25)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(&flakyMedia{Media: med, fails: 1}, from, 5000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()

	buf := audio.Get(f, 8)
	defer audio.Put(buf)
	if err := sl.ReadChunk(buf); err == nil {
		t.Fatal("a span whose seek failed delivered a chunk")
	}
	if err := sl.ReadChunk(buf); err != nil {
		t.Fatalf("a span did not re-attempt its start after a failed seek: %v", err)
	}
	if buf.Pos != 0 {
		t.Errorf("chunk is stamped %d, want the window's sample 0", buf.Pos)
	}
	// What it delivers is the window's sample 0, not the source's: the seek
	// that never succeeded cannot be what a read reads past.
	want := audio.Get(f, 8)
	defer audio.Put(want)
	want.N = buf.N
	audio.CopyFrames(want, 0, whole, from, buf.N)
	equalPCM(t, want, buf)
}

// chapteredMedia gives a Media a chapter list, which the WAV a test can build
// has no form for. Slice reads the list off Info and nothing else, so this is
// the whole of what it takes to put one in front of a span.
type chapteredMedia struct {
	format.Media
	info format.Info
}

func (m *chapteredMedia) Info() *format.Info { return &m.info }

// TestSliceChapters pins that a span rebases the source's chapters onto its
// own timeline: shifted so the window starts at zero, clipped to the window,
// and with everything outside it gone.
//
// Forwarded chapters fail nowhere and are wrong everywhere downstream. A
// consumer reads the list off Info and writes it into what it produces, so a
// split job stamps every piece of an album with the whole album's chapters,
// at the times they sat at in a file the piece is not.
func TestSliceChapters(t *testing.T) {
	const rate = 48000
	// The bounded cases all cut [1s, 3s) out of a 4.16 s source.
	const from, to = 1 * rate, 3 * rate
	ms := func(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

	cases := []struct {
		name     string
		from, to int64
		in, want []container.Chapter
	}{
		{
			name: "shifted, clipped, and dropped",
			from: from, to: to,
			in: []container.Chapter{
				{Start: 0, End: ms(600), Title: "before"},
				{Start: ms(600), End: ms(1400), Title: "front"},
				{Start: ms(1400), End: ms(2000), Title: "inside"},
				{Start: ms(2000), End: ms(3400), Title: "back"},
				{Start: ms(3400), End: ms(4000), Title: "after"},
			},
			// The straddlers keep their titles and the part of their range
			// the window holds; what lies wholly outside is not the span's.
			want: []container.Chapter{
				{Start: 0, End: ms(400), Title: "front"},
				{Start: ms(400), End: ms(1000), Title: "inside"},
				{Start: ms(1000), End: ms(2000), Title: "back"},
			},
		},
		{
			// The window's bounds are exclusive at the far end, exactly as
			// the span's samples are.
			name: "the edges are the window's own",
			from: from, to: to,
			in: []container.Chapter{
				{Start: 0, End: ms(1000), Title: "ends where the window starts"},
				{Start: ms(1000), End: ms(2000), Title: "starts where the window starts"},
				{Start: ms(3000), End: ms(3500), Title: "starts where the window ends"},
			},
			want: []container.Chapter{
				{Start: 0, End: ms(1000), Title: "starts where the window starts"},
			},
		},
		{
			// A zero End is the start-only form: it stays zero, because what
			// a consumer resolves it against (the next chapter, the end of
			// the stream) is already this span's own.
			name: "start-only chapters keep their open end",
			from: from, to: to,
			in: []container.Chapter{
				{Start: 0, Title: "one"},
				{Start: ms(2000), Title: "two"},
			},
			want: []container.Chapter{
				{Start: 0, Title: "one"},
				{Start: ms(1000), Title: "two"},
			},
		},
		{
			// Where a start-only chapter ends is the next one's start, which
			// is what decides whether it reaches the window at all: "one" is
			// over at 500 ms and the window has none of it, while "two" runs
			// to the end of the stream and holds the whole window.
			name: "a start-only chapter the next one ends before the window",
			from: from, to: to,
			in: []container.Chapter{
				{Start: 0, Title: "one"},
				{Start: ms(500), Title: "two"},
			},
			want: []container.Chapter{
				{Start: 0, Title: "two"},
			},
		},
		{
			// The open form holds the source to no length of its own, so it
			// clips nothing at the far end either.
			name: "an open window clips only its front",
			from: from, to: waxflow.ToEnd,
			in: []container.Chapter{
				{Start: 0, End: ms(600), Title: "before"},
				{Start: ms(600), End: ms(1400), Title: "front"},
				{Start: ms(1400), End: ms(2000), Title: "inside"},
				{Start: ms(2000), End: ms(3400), Title: "back"},
				{Start: ms(3400), End: ms(4000), Title: "after"},
			},
			want: []container.Chapter{
				{Start: 0, End: ms(400), Title: "front"},
				{Start: ms(400), End: ms(1000), Title: "inside"},
				{Start: ms(1000), End: ms(2400), Title: "back"},
				{Start: ms(2400), End: ms(3000), Title: "after"},
			},
		},
	}

	const frames = 200_000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(rate, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 26)
	raw := wavFrom(t, cfg, whole)
	e := waxflow.New()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			med, err := e.OpenStream(container.BytesSource(raw), "wav")
			if err != nil {
				t.Fatal(err)
			}
			info := *med.Info()
			info.Chapters = tc.in
			sl, err := waxflow.Slice(&chapteredMedia{Media: med, info: info}, tc.from, tc.to)
			if err != nil {
				med.Close()
				t.Fatal(err)
			}
			defer sl.Close()
			if got := sl.Info().Chapters; !slices.Equal(got, tc.want) {
				t.Errorf("chapters =\n\t%+v\nwant\n\t%+v", got, tc.want)
			}
		})
	}
}
