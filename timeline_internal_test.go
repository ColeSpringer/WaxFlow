package waxflow

import (
	"io"
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
			env, err := ConcatTrack(tracks)
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
	})
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
	})
	if err == nil {
		t.Fatal("a member laid out for other speakers joined a timeline silently")
	}
}
