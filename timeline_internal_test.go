package waxflow

import (
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// fixedMedia is a member with a format and a length and no audio: the
// structural tests below are about which nodes a timeline builds, not about
// samples, which tests/waxflow_timeline_test.go covers end to end.
type fixedMedia struct {
	info  *format.Info
	track container.Track
	pos   int64
}

func newFixedMedia(f audio.Format, samples int64) *fixedMedia {
	track := container.Track{Codec: codec.PCM, Fmt: f, Samples: samples, SamplesExact: true, Default: true}
	return &fixedMedia{
		info:  &format.Info{Container: "fixed", Tracks: []container.Track{track}},
		track: track,
	}
}

func (m *fixedMedia) Info() *format.Info { return m.info }
func (m *fixedMedia) Close() error       { return nil }

func (m *fixedMedia) ReadChunk(dst *audio.Buffer) error {
	left := m.track.Samples - m.pos
	if left <= 0 {
		return io.EOF
	}
	dst.N = int(min(left, int64(dst.Cap())))
	dst.Pos = m.pos
	dst.Discont = false
	m.pos += int64(dst.N)
	return nil
}

func (m *fixedMedia) SeekSample(target int64) (int64, error) {
	m.pos = min(target, m.track.Samples)
	return m.pos, nil
}

func fixedMember(f audio.Format, samples int64) ConcatSource {
	return ConcatSource{
		Track: container.Track{Codec: codec.PCM, Fmt: f, Samples: samples, SamplesExact: true, Default: true},
		Open:  func() (format.Media, error) { return newFixedMedia(f, samples), nil },
	}
}

var (
	stereo48 = audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	stereo44 = audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	mono48   = audio.Format{Rate: 48000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}
)

// openFirst forces the first member open so the wiring is observable.
func openFirst(t *testing.T, c *concat) {
	t.Helper()
	buf := audio.Get(c.fmt, 128)
	defer audio.Put(buf)
	if err := c.ReadChunk(buf); err != nil {
		t.Fatalf("first ReadChunk: %v", err)
	}
}

// TestConcatUniformIsZeroCopy pins what ConcatTrack claims of the common
// case: a member whose format already equals the envelope is read straight
// through, with no normalization chain between the member and the caller's
// buffer. A gapless album is one master at one rate, so this is the case
// that matters, and it costs nothing structurally rather than by luck.
//
// It is an internal test because the claim is internal. From outside, a
// uniform member's chain would be an *empty* chain, which delegates straight
// to its source stage and yields identical samples, so no black-box
// assertion can tell the two apart. The difference is that the uniform case
// never builds one.
func TestConcatUniformIsZeroCopy(t *testing.T) {
	med, err := Concat([]ConcatSource{
		fixedMember(stereo48, 1000),
		fixedMember(stereo48, 2000),
	}, ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	c := med.(*concat)
	if c.fmt != stereo48 {
		t.Fatalf("envelope %v, want %v", c.fmt, stereo48)
	}
	openFirst(t, c)
	if c.chain != nil {
		t.Fatal("a uniform member built a normalization chain; it must be read straight through")
	}
}

// TestConcatMixedBuildsChain is the other half of the claim above: a member
// that does not match the envelope does get a chain, so the zero-copy test
// is passing for the right reason rather than because nothing is ever
// normalized.
func TestConcatMixedBuildsChain(t *testing.T) {
	med, err := Concat([]ConcatSource{
		fixedMember(stereo44, 1000),
		fixedMember(stereo48, 2000),
	}, ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	c := med.(*concat)
	openFirst(t, c)
	if c.chain == nil {
		t.Fatal("a 44.1 kHz member of a 48 kHz timeline was read without a resampler")
	}
	if got := c.chain.Format(); got != c.fmt {
		t.Fatalf("the normalization chain emits %v, the envelope is %v", got, c.fmt)
	}
}

// TestConcatTrackEnvelope pins the envelope rule itself: the maximum of
// every axis, so no member loses information reaching it.
func TestConcatTrackEnvelope(t *testing.T) {
	float48 := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	deep96 := audio.Format{Rate: 96000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}

	for _, tc := range []struct {
		name string
		in   []audio.Format
		want audio.Format
	}{
		{"uniform keeps the format", []audio.Format{stereo48, stereo48}, stereo48},
		{"mono in a stereo queue widens to stereo", []audio.Format{mono48, stereo48}, stereo48},
		{"the higher rate wins", []audio.Format{stereo44, stereo48}, stereo48},
		{"the deeper word wins", []audio.Format{stereo48, deep96}, deep96},
		{"any float member makes the envelope float", []audio.Format{stereo48, float48}, float48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracks := make([]container.Track, len(tc.in))
			for i, f := range tc.in {
				tracks[i] = container.Track{Codec: codec.PCM, Fmt: f, Samples: 48000}
			}
			env, err := ConcatTrack(tracks, ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if env.Fmt != tc.want {
				t.Fatalf("envelope %v, want %v", env.Fmt, tc.want)
			}
			if env.Delay != 0 || env.Padding != 0 {
				t.Fatalf("synthetic track carries trims (delay %d, padding %d); "+
					"format.Media already delivered trimmed PCM, so a downstream consumer would trim twice",
					env.Delay, env.Padding)
			}
			if !env.SamplesExact {
				t.Fatal("the summed length is enforced, so it must not be advertised as advisory")
			}
		})
	}
}

// TestConcatTrackRejectsUnmeasured pins the mint's obligation: a member with
// no declared length cannot be summed, so a timeline cannot be planned from
// one. The mint measures instead of estimating, and this is what makes that
// non-optional.
func TestConcatTrackRejectsUnmeasured(t *testing.T) {
	_, err := ConcatTrack([]container.Track{
		{Codec: codec.PCM, Fmt: stereo48, Samples: 48000},
		{Codec: codec.PCM, Fmt: stereo48, Samples: -1},
	}, ConcatOptions{})
	if err == nil {
		t.Fatal("a member with an unknown length planned a timeline")
	}
}

// seekFailMedia fails its first seek and then behaves: a transient I/O error
// on a real file looks like this from Concat's side, and it is the shape that
// lets the recovery below mean something.
// The flag is shared across opens, not per instance: a failed seek closes the
// member, so a per-instance flag would be reset by the very reopen the
// recovery depends on and the fake would never stop failing.
type seekFailMedia struct {
	*fixedMedia
	failed *bool
}

func (m *seekFailMedia) SeekSample(target int64) (int64, error) {
	if !*m.failed {
		*m.failed = true
		return 0, waxerr.New(waxerr.CodeSourceUnreadable, "seek failed")
	}
	return m.fixedMedia.SeekSample(target)
}

// TestConcatFailedSeekDoesNotDesync pins what a half-done seek must not do.
//
// A seek moves several things the position depends on (which member is open,
// its chain, where its media sits), so a failure part way through leaves pos
// describing where the stream used to be while the media sits somewhere else.
// Reading on from there would hand back real samples under positions they do
// not have, which is the worst shape of bug a timeline can have: silent, and
// wrong by exactly the amount nobody measured. Closing the member is not
// enough on its own, because the next read would reopen it and deliver from
// its start under the same stale pos.
func TestConcatFailedSeekDoesNotDesync(t *testing.T) {
	failed := false
	open := func() (format.Media, error) {
		return &seekFailMedia{fixedMedia: newFixedMedia(stereo48, 1000), failed: &failed}, nil
	}
	med, err := Concat([]ConcatSource{
		{Track: container.Track{Codec: codec.PCM, Fmt: stereo48, Samples: 1000, SamplesExact: true, Default: true}, Open: open},
		{Track: container.Track{Codec: codec.PCM, Fmt: stereo48, Samples: 1000, SamplesExact: true, Default: true}, Open: open},
	}, ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	buf := audio.Get(stereo48, 128)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := med.SeekSample(1500); err == nil {
		t.Fatal("a seek onto a member that cannot seek reported success")
	}
	// The read must fail rather than deliver samples stamped with the
	// position the timeline held before the seek moved it.
	if err := med.ReadChunk(buf); err == nil {
		t.Fatalf("reading after a failed seek delivered %d samples at position %d; "+
			"the timeline no longer knows where it is", buf.N, buf.Pos)
	}
	// A seek that succeeds puts it back in a known state, so the latch is not
	// a one-way door: the timeline is recoverable, not poisoned.
	if _, err := med.SeekSample(0); err != nil {
		t.Fatalf("seeking back to the top after a failed seek: %v", err)
	}
	if err := med.ReadChunk(buf); err != nil {
		t.Fatalf("reading after a recovering seek: %v", err)
	}
	if buf.Pos != 0 {
		t.Fatalf("the recovered read starts at %d, want 0", buf.Pos)
	}
}

// shortSeekMedia lands before the ask. The Media contract permits landing
// *past* a target (the first sync point may lie beyond it) and never short, so
// this is a caller's Media breaking its contract, which is the same class as
// stallMedia below and reachable the same way: a member's Open is a caller's
// function returning a caller's Media.
type shortSeekMedia struct{ *fixedMedia }

func (m *shortSeekMedia) SeekSample(target int64) (int64, error) {
	return m.fixedMedia.SeekSample(max(target-100, 0))
}

// TestConcatCrossfadeRefusesAShortSeekLanding pins the one precondition the
// blend's whole overflow argument rests on: capture begins at the body's end.
//
// captureTail sizes its buffer at X and reads the member's rest to io.EOF,
// which is safe only because blend.N == local-(L-X) throughout, so count's
// ceiling is the buffer's. A member that lands short of the body starts the
// capture early, and then the identity is false: the member has more than X
// frames left, count never fires (local stays under L), and CopyFrames writes
// past the buffer's stride. audio.CopyFrames bounds offsets by Stride as a
// contract on its caller rather than a check, so the overrun corrupts each
// channel into the next one's region and panics on the last -- silent for
// every channel but one.
//
// A seek into the zone is the only way in, because that is the only path that
// reaches captureTail from a member seek rather than from bound() == 0.
func TestConcatCrossfadeRefusesAShortSeekLanding(t *testing.T) {
	const n, x = 1000, 256
	open := func() (format.Media, error) { return &shortSeekMedia{newFixedMedia(stereo48, n)}, nil }
	track := container.Track{Codec: codec.PCM, Fmt: stereo48, Samples: n, SamplesExact: true, Default: true}
	med, err := Concat([]ConcatSource{{Track: track, Open: open}, {Track: track, Open: open}}, ConcatOptions{Crossfade: x})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	// Ten frames into member 1's head zone, which is member 0's tail: reaching
	// it opens member 0 and seeks it to its body's end, and this member lands
	// 100 frames short of that.
	if _, err := med.SeekSample(n - x + 10); err == nil {
		t.Fatal("a member that landed short of its own crossfade zone captured a tail anyway; " +
			"the blend buffer is sized at exactly the zone, so the extra frames run past it")
	}
	// The latch is the existing contract and it still holds: a failed seek
	// leaves the timeline unpositioned rather than reading on from nowhere.
	buf := audio.Get(stereo48, 128)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err == nil {
		t.Fatal("reading after the refused seek delivered samples")
	}
}

// stallMedia breaks the Stage contract: it returns no frames and no error,
// where the contract says io.EOF is the only empty answer.
type stallMedia struct{ *fixedMedia }

func (m *stallMedia) ReadChunk(dst *audio.Buffer) error {
	dst.N = 0
	return nil
}

// TestConcatSeekPreRollRefusesAStalledMember pins the guard on the one loop
// here that has no context to cancel against. A member's Open is a caller's
// function returning a caller's Media, so the Stage contract is theirs to
// break; a member that never progresses would otherwise spin this loop
// forever, holding a live admission slot nothing can reclaim. It must be an
// error, and the test would hang rather than fail without the guard, which is
// the point.
func TestConcatSeekPreRollRefusesAStalledMember(t *testing.T) {
	// A 44.1 kHz member in a 48 kHz timeline: the mismatch is what puts a
	// chain in front of the member, which is what makes the seek pre-roll
	// (and this loop) run at all.
	stall := ConcatSource{
		Track: container.Track{Codec: codec.PCM, Fmt: stereo44, Samples: 44100, SamplesExact: true, Default: true},
		Open:  func() (format.Media, error) { return &stallMedia{newFixedMedia(stereo44, 44100)}, nil },
	}
	med, err := Concat([]ConcatSource{stall, fixedMember(stereo48, 48000)}, ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	done := make(chan error, 1)
	go func() {
		_, err := med.SeekSample(5000)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a member that returns no samples and no error seeked successfully")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the seek pre-roll never returned: a member that makes no progress spins the loop forever")
	}
}

// TestConcatTrackCrossfadeRefusals pins every crossfade refusal, and pins them
// through both entry points.
//
// The funnel is the claim under test rather than the messages. ConcatTrack is
// where a crossfade is checked, and Concat resolves through it, so a request
// that cannot be planned must be one that cannot be run: if the two ever
// disagree, a plan promises a length no run delivers, which is the prefix-sum
// desync ADR-0009 exists to prevent. Asserting both is what makes that
// structural rather than a comment.
func TestConcatTrackCrossfadeRefusals(t *testing.T) {
	// The memory cap in the envelope's own terms: X*ch <= 4 Mi, which for
	// stereo is 2 Mi frames.
	const stereoCap = maxCrossfadeBytes / (2 * 4)
	for _, tc := range []struct {
		name  string
		lens  []int64
		x     int64
		want  bool
		names string
	}{
		{name: "zero is a butt-join", lens: []int64{1000, 1000}, x: 0},
		{name: "the ordinary declick", lens: []int64{1000, 1000}, x: 256},
		{name: "negative", lens: []int64{1000, 1000}, x: -1, want: true, names: "-1"},
		// Equality is legal at both bounds, and both are worth pinning: an
		// off-by-one in either refuses a request that fits exactly.
		{name: "fit exactly, two members", lens: []int64{256, 256}, x: 256},
		{name: "fit exactly, three members", lens: []int64{256, 512, 256}, x: 256},
		{name: "one short, edge member", lens: []int64{255, 256}, x: 256, want: true, names: "255"},
		{name: "one short, middle member", lens: []int64{256, 511, 256}, x: 256, want: true, names: "511"},
		// N=1 has no seam, so it passes for free rather than by special case:
		// its only member is both first and last and carries no zone at all.
		{name: "a single member never blends", lens: []int64{1}, x: stereoCap},
		{name: "the memory cap, exactly", lens: []int64{stereoCap, stereoCap}, x: stereoCap},
		{name: "one sample past the cap", lens: []int64{1 << 24, 1 << 24}, x: stereoCap + 1, want: true, names: "2097153"},
		// The cap is checked by dividing, never by multiplying: x*channels*4
		// wraps an int64 long before a number this size fails any comparison,
		// and a wrapped product compares small and is accepted. Neither the
		// check nor its message may touch the product, which is why the
		// message quotes no byte count.
		//
		// The members are absurd on purpose, and they are what makes this row
		// test the cap rather than the fit rule. Nothing in the tree bounds a
		// member's declared length above, so a header can say 2^62 samples;
		// with members that long, a 2^61 crossfade passes the fit rule
		// honestly and arrives at the cap as the only thing standing between
		// it and a multiply that overflows to zero.
		{name: "a crossfade whose byte count would overflow", lens: []int64{1 << 62, 1 << 62},
			x: 1 << 61, want: true, names: "2305843009213693952"},
		// A zero-length member is refused by name at mint time, where a client
		// can still act, rather than surfacing as a blend against nothing.
		{name: "a zero-length member", lens: []int64{0, 1000}, x: 256, want: true, names: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracks := make([]container.Track, len(tc.lens))
			members := make([]ConcatSource, len(tc.lens))
			for i, l := range tc.lens {
				tracks[i] = container.Track{Codec: codec.PCM, Fmt: stereo48, Samples: l, SamplesExact: true, Default: true}
				members[i] = fixedMember(stereo48, l)
			}
			opts := ConcatOptions{Crossfade: tc.x}
			_, planErr := ConcatTrack(tracks, opts)
			med, runErr := Concat(members, opts)
			if runErr == nil {
				med.Close()
			}
			if (planErr != nil) != tc.want {
				t.Fatalf("ConcatTrack error = %v, want refusal = %v", planErr, tc.want)
			}
			if (runErr != nil) != tc.want {
				t.Fatalf("Concat error = %v, want refusal = %v; the plan said %v. "+
					"ConcatTrack is the single funnel, so the two cannot disagree", runErr, tc.want, planErr)
			}
			// The number the caller has to act on is the one they set, or the
			// one that did not fit. A refusal they cannot locate is one they
			// answer by guessing.
			if tc.want && !strings.Contains(planErr.Error(), tc.names) {
				t.Errorf("the refusal does not name %s, so a caller cannot tell which member or which number is the problem: %v",
					tc.names, planErr)
			}
		})
	}
}

// TestConcatCrossfadeStartsOverlap pins the arithmetic that the naive prefix
// sum gets wrong.
//
// starts and lens are separate facts once members overlap, and the trap is the
// last hop: the last member has no tail zone, so subtracting X on every hop
// leaves the timeline X short of the length its own track declares. That is a
// tail 404, and it is invisible until the last segment of a stream.
func TestConcatCrossfadeStartsOverlap(t *testing.T) {
	const x = 256
	lens := []int64{1000, 2000, 3000}
	members := make([]ConcatSource, len(lens))
	tracks := make([]container.Track, len(lens))
	for i, l := range lens {
		members[i] = fixedMember(stereo48, l)
		tracks[i] = container.Track{Codec: codec.PCM, Fmt: stereo48, Samples: l, SamplesExact: true, Default: true}
	}
	med, err := Concat(members, ConcatOptions{Crossfade: x})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	c := med.(*concat)

	// Member i begins where the previous one's tail zone does: X before it
	// ends, because that region is the same region.
	wantStarts := []int64{0, 1000 - x, 1000 - x + 2000 - x, 6000 - 2*x}
	for i, want := range wantStarts {
		if c.starts[i] != want {
			t.Errorf("starts[%d] = %d, want %d", i, c.starts[i], want)
		}
	}
	// lens is untouched by the overlap: a member is as long as it is, wherever
	// it sits. Conflating the two is what the second field exists to prevent.
	for i, want := range lens {
		if c.lens[i] != want {
			t.Errorf("lens[%d] = %d, want %d; a member's length is not the distance to the next one's start", i, c.lens[i], want)
		}
	}
	env, err := ConcatTrack(tracks, ConcatOptions{Crossfade: x})
	if err != nil {
		t.Fatal(err)
	}
	if c.starts[len(lens)] != env.Samples {
		t.Fatalf("the walk ends at %d and the track declares %d: the plan and the run disagree about "+
			"how long this timeline is, which is the tail 404 the exact-length walk exists to prevent",
			c.starts[len(lens)], env.Samples)
	}
}

// TestConcatBoundaries pins the exported boundary contract A17 puts on the
// wire, so a consumer built on it is not surprised when a crossfade lands.
// OffsetSamples is an actual timeline position and DurationSamples is the raw
// per-member length: at X=0 they tile (offset[i+1] == offset[i] + duration[i]),
// and under a crossfade of X they OVERLAP by exactly X.
func TestConcatBoundaries(t *testing.T) {
	lens := []int64{1000, 2000, 3000}
	tracks := make([]container.Track, len(lens))
	for i, l := range lens {
		tracks[i] = container.Track{Codec: codec.PCM, Fmt: stereo48, Samples: l, SamplesExact: true, Default: true}
	}

	t.Run("butt-join tiles", func(t *testing.T) {
		bounds, env, err := ConcatBoundaries(tracks, ConcatOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if env.Rate != 48000 {
			t.Fatalf("envelope rate %d, want 48000", env.Rate)
		}
		if bounds[0].OffsetSamples != 0 {
			t.Fatalf("boundaries[0].OffsetSamples = %d, want 0", bounds[0].OffsetSamples)
		}
		for i, l := range lens {
			if bounds[i].DurationSamples != l {
				t.Errorf("boundaries[%d].DurationSamples = %d, want %d", i, bounds[i].DurationSamples, l)
			}
		}
		for i := 0; i+1 < len(bounds); i++ {
			if got, want := bounds[i+1].OffsetSamples, bounds[i].OffsetSamples+bounds[i].DurationSamples; got != want {
				t.Errorf("without a crossfade member %d starts at %d, want %d (the previous one's end): the members must tile",
					i+1, got, want)
			}
		}
	})

	t.Run("crossfade overlaps", func(t *testing.T) {
		const x = 256
		bounds, _, err := ConcatBoundaries(tracks, ConcatOptions{Crossfade: x})
		if err != nil {
			t.Fatal(err)
		}
		// Durations stay the raw per-member lengths, untouched by the blend.
		for i, l := range lens {
			if bounds[i].DurationSamples != l {
				t.Errorf("boundaries[%d].DurationSamples = %d, want the raw %d", i, bounds[i].DurationSamples, l)
			}
		}
		for i := 0; i+1 < len(bounds); i++ {
			end := bounds[i].OffsetSamples + bounds[i].DurationSamples
			if end <= bounds[i+1].OffsetSamples {
				t.Errorf("member %d ends at %d and member %d starts at %d; a crossfade shares that region, so they must overlap",
					i, end, i+1, bounds[i+1].OffsetSamples)
			}
			if got, want := bounds[i+1].OffsetSamples, end-x; got != want {
				t.Errorf("member %d starts at %d, want %d (X before the previous member's end)", i+1, got, want)
			}
		}
	})

	t.Run("offsets and durations are at the envelope rate", func(t *testing.T) {
		// A 44.1 kHz member of a 48 kHz timeline is reported at its resampled
		// length, so a consumer never has to know a member's own rate: both the
		// offset and the duration sit on the envelope's clock.
		mixed := []container.Track{
			{Codec: codec.PCM, Fmt: stereo44, Samples: 44100, SamplesExact: true, Default: true},
			{Codec: codec.PCM, Fmt: stereo48, Samples: 48000, SamplesExact: true, Default: true},
		}
		bounds, env, err := ConcatBoundaries(mixed, ConcatOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if env.Rate != 48000 {
			t.Fatalf("envelope rate %d, want 48000", env.Rate)
		}
		want := concatMemberSamples(mixed[0], env)
		if bounds[0].DurationSamples != want {
			t.Fatalf("the 44.1 kHz member reports %d samples, want its 48 kHz length %d", bounds[0].DurationSamples, want)
		}
		if bounds[1].OffsetSamples != want {
			t.Fatalf("member 1 starts at %d, want %d (the resampled member 0 length)", bounds[1].OffsetSamples, want)
		}
	})

	t.Run("surfaces concatLayout's refusals", func(t *testing.T) {
		// The funnel is only as honest as its error path: a caller handed
		// (nil, {}, nil) for an unbuildable timeline would read a zero envelope
		// and no boundaries as success. concatLayout refuses these; the wrapper
		// must not swallow the refusal.
		if _, _, err := ConcatBoundaries(nil, ConcatOptions{}); err == nil {
			t.Error("an empty timeline returned no error")
		}
		if _, _, err := ConcatBoundaries(tracks, ConcatOptions{Crossfade: -1}); err == nil {
			t.Error("a negative crossfade returned no error")
		}
		// Longer than the shortest member (1000 samples): refused by
		// checkCrossfade inside concatLayout, which the wrapper must relay.
		if _, _, err := ConcatBoundaries(tracks, ConcatOptions{Crossfade: 100000}); err == nil {
			t.Error("a crossfade longer than the shortest member returned no error")
		}
	})
}

// TestCrossfadeSamples pins the seconds-to-envelope-samples conversion the wire
// depends on: a crossfade is spelled in seconds because a caller cannot know the
// envelope rate, and the plan and the run agree only if they both convert it the
// same way, on that rate.
func TestCrossfadeSamples(t *testing.T) {
	mk := func(f audio.Format, samples int64) container.Track {
		return container.Track{Codec: codec.PCM, Fmt: f, Samples: samples, SamplesExact: true, Default: true}
	}
	// Long members, so the fit rule never intrudes on the conversion itself.
	stereo := []container.Track{mk(stereo48, 1<<30), mk(stereo48, 1<<30)}
	// A 44.1 kHz member beside a 48 kHz one: the envelope rate is the maximum,
	// 48 kHz, so the crossfade is counted on 48 kHz and not on either member's
	// own rate.
	mixed := []container.Track{mk(stereo44, 1<<30), mk(stereo48, 1<<30)}

	t.Run("counts on the envelope rate", func(t *testing.T) {
		got, err := CrossfadeSamples(stereo, 0.5)
		if err != nil {
			t.Fatal(err)
		}
		if got != 24000 {
			t.Errorf("0.5 s at 48 kHz = %d samples, want 24000", got)
		}
		if got, _ := CrossfadeSamples(mixed, 0.5); got != 24000 {
			t.Errorf("0.5 s of a mixed-rate timeline = %d, want 24000 (the 48 kHz envelope), not 22050", got)
		}
	})

	t.Run("a non-positive seconds is a butt-join", func(t *testing.T) {
		for _, sec := range []float64{0, -1, -0.001} {
			if got, err := CrossfadeSamples(stereo, sec); err != nil || got != 0 {
				t.Errorf("CrossfadeSamples(%v) = (%d, %v), want (0, nil): a non-positive crossfade is a butt-join", sec, got, err)
			}
		}
		// A butt-join needs no envelope, so an empty timeline is not an error
		// on this path: it is the members the render will refuse, not here.
		if got, err := CrossfadeSamples(nil, 0); err != nil || got != 0 {
			t.Errorf("CrossfadeSamples(nil, 0) = (%d, %v), want (0, nil)", got, err)
		}
	})

	t.Run("rounds to the nearest sample", func(t *testing.T) {
		// 100.7 and 100.2 samples' worth of seconds at 48 kHz: the round is what
		// keeps the plan and the run from splitting on a sub-sample.
		if got, _ := CrossfadeSamples(stereo, 100.7/48000); got != 101 {
			t.Errorf("100.7 samples rounded to %d, want 101", got)
		}
		if got, _ := CrossfadeSamples(stereo, 100.2/48000); got != 100 {
			t.Errorf("100.2 samples rounded to %d, want 100", got)
		}
	})

	t.Run("an absurd seconds clamps rather than wraps", func(t *testing.T) {
		// The refusal is checkCrossfade's, so the converter must hand it a value
		// too large to blend rather than a silently wrapped small one.
		if got, err := CrossfadeSamples(stereo, 1e300); err != nil || got != math.MaxInt64 {
			t.Errorf("CrossfadeSamples(1e300) = (%d, %v), want (MaxInt64, nil)", got, err)
		}
	})

	t.Run("surfaces the layout refusal for a blend", func(t *testing.T) {
		// A crossfade needs an envelope, so an unbuildable timeline is refused
		// here rather than read as a zero count.
		if _, err := CrossfadeSamples(nil, 0.5); err == nil {
			t.Error("a crossfade of an empty timeline returned no error")
		}
	})
}

// TestConcatButtJoinTakesNoBound pins the zero-copy claim at the only level it
// is observable.
//
// A bound costs a right-sized audio.Get and a CopyFrames, because Cap() ==
// Stride and a buffer has no sub-buffer view. Computed unconditionally, the
// bound would put the last chunk of every member of every butt-joined timeline
// through that copy: today's timelines, all of them, paying for a feature none
// of them use. bound() returning -1 is what makes X=0 take today's path
// structurally, and this is the assertion that it does.
func TestConcatButtJoinTakesNoBound(t *testing.T) {
	med, err := Concat([]ConcatSource{
		fixedMember(stereo48, 5000),
		fixedMember(stereo48, 5000),
	}, ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	c := med.(*concat)

	buf := audio.Get(stereo48, 1024)
	defer audio.Put(buf)
	for {
		// Checked before every read, including the reads that straddle both
		// seams, which is where a computed bound would bite.
		if got := c.bound(); got != -1 {
			t.Fatalf("bound() = %d at member %d local %d; a butt-joined timeline has no tail zone anywhere, "+
				"so it must have no bound to compute", got, c.cur, c.local)
		}
		if c.blend != nil {
			t.Fatalf("a butt-joined timeline captured a blend buffer at member %d", c.cur)
		}
		err := c.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if c.blend != nil {
		t.Fatal("a butt-joined timeline held a blend buffer at the end of the stream")
	}
}

// TestConcatTrackRejectsUnconventionalLayout pins the one envelope case the
// mix node cannot reach. It mixes to audio.DefaultLayout, so that has to be
// the envelope's layout, and a member already at the envelope's channel
// count runs no mix and keeps its own. Rather than relabel its channels
// (calling a back-left speaker front-right), the timeline says so.
func TestConcatTrackRejectsUnconventionalLayout(t *testing.T) {
	odd := audio.Format{Rate: 48000, Channels: 2, Layout: audio.FrontLeft | audio.BackLeft, Type: audio.Int, BitDepth: 16}
	_, err := ConcatTrack([]container.Track{
		{Codec: codec.PCM, Fmt: stereo48, Samples: 48000},
		{Codec: codec.PCM, Fmt: odd, Samples: 48000},
	}, ConcatOptions{})
	if err == nil {
		t.Fatal("a member laid out for other speakers joined a timeline silently")
	}
}
