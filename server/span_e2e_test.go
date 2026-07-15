// A11's acceptance suite: the virtual-track span surface over /stream and
// HLS. The ramp fixture is what makes these assertions mean something,
// since every sample value names its own position, so a span that
// delivered the wrong samples cannot pass by delivering the right count.
package server_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// spanOf streams [from, to) of the ramp as FLAC and decodes the response.
func spanOf(t *testing.T, e *testEnv, from, to int64) *audio.Buffer {
	t.Helper()
	q := fmt.Sprintf("/stream?src=lib/ramp.wav&format=flac&from=%d", from)
	if to > 0 {
		q += fmt.Sprintf("&to=%d", to)
	}
	resp := e.get(t, q, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", q, resp.StatusCode)
	}
	return decodePCM(t, readBody(t, resp))
}

// TestSpanDeliversItsOwnSamples is the assertion that makes a virtual track
// real: streaming from=X&to=Y yields exactly the source's samples [X, Y),
// so a span of a file and a split of that file at the same cut points are
// the same audio.
//
// FLAC at the source rate keeps it bit-exact: no resampler means no state
// to prime, so the span's sample 0 is the source's sample X exactly.
func TestSpanDeliversItsOwnSamples(t *testing.T) {
	e := newTestEnv(t, nil)
	const rate, channels, frames = 48000, 2, 4 * 48000
	whole := testutil.Ramp(audio.Format{Rate: rate, Channels: channels,
		Layout: audio.DefaultLayout(channels), Type: audio.Int, BitDepth: 16}, frames)
	defer audio.Put(whole)

	for _, tc := range []struct {
		name     string
		from, to int64
	}{
		// A boundary at a non-integer second, which is the case that would
		// pass for a seconds-based span and fail by one sample. 245.32 s is
		// not representable in binary; a CD frame offset is.
		{"non-integer second boundary", 47_101, 132_877},
		{"from the top with an end", 0, 60_000},
		{"open ended", 100_000, 0},
		{"interior", 33_333, 99_999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := spanOf(t, e, tc.from, tc.to)
			end := tc.to
			if end == 0 {
				end = frames
			}
			if want := int(end - tc.from); got.N != want {
				t.Fatalf("span [%d, %d) delivered %d samples, want %d", tc.from, end, got.N, want)
			}
			// The ramp names positions, so this checks which samples came
			// back, not merely how many.
			for ch := range channels {
				src, out := whole.ChanI(ch), got.ChanI(ch)
				for i := range got.N {
					if out[i] != src[int(tc.from)+i] {
						t.Fatalf("span [%d, %d) channel %d sample %d = %d, want the source's sample %d (%d)",
							tc.from, end, ch, i, out[i], int(tc.from)+i, src[int(tc.from)+i])
					}
				}
			}
		})
	}
}

// TestSpansJoinGaplessly is A11's headline claim over the wire: two
// consecutive virtual tracks of one rip must join with no sample lost and
// none repeated, which is the whole reason a span is samples and not
// seconds.
//
// The boundary deliberately falls at a CD frame offset that is not a whole
// second (588 samples per frame at 44100 means frame 2477 lands at
// 33.0266... s), because that is precisely where a seconds-based span
// rounds and drops or repeats a sample.
func TestSpansJoinGaplessly(t *testing.T) {
	e := newTestEnv(t, nil)
	const frames = 4 * 48000
	// 2477 CD frames at 44100 is 1_456_476 samples, past this fixture; use
	// a frame-derived offset that lands inside it and is still not on a
	// second boundary.
	const boundary = 137 * 588 // 80_556 samples: 1.678...s at 48k, no round number

	head := spanOf(t, e, 0, boundary)
	tail := spanOf(t, e, boundary, 0)

	if head.N != boundary {
		t.Fatalf("first virtual track delivered %d samples, want %d", head.N, boundary)
	}
	if want := frames - boundary; tail.N != want {
		t.Fatalf("second virtual track delivered %d samples, want %d", tail.N, want)
	}

	// The join: the last sample of the first track and the first of the
	// second must be adjacent in the source, with nothing between them and
	// nothing counted twice.
	whole := testutil.Ramp(audio.Format{Rate: 48000, Channels: 2,
		Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, frames)
	defer audio.Put(whole)
	for ch := range 2 {
		src := whole.ChanI(ch)
		if got, want := head.ChanI(ch)[head.N-1], src[boundary-1]; got != want {
			t.Errorf("channel %d: the first track ends on %d, want the source's sample %d (%d)",
				ch, got, boundary-1, want)
		}
		if got, want := tail.ChanI(ch)[0], src[boundary]; got != want {
			t.Errorf("channel %d: the second track begins on %d, want the source's sample %d (%d); "+
				"a sample was lost or repeated at the boundary", ch, got, boundary, want)
		}
	}
}

// TestSpanSeeksWithinItself pins that t= and the span compose rather than
// competing: t= seconds addresses the virtual track's own timeline, because
// that is the stream the caller is playing.
func TestSpanSeeksWithinItself(t *testing.T) {
	e := newTestEnv(t, nil)
	const from, to = 96_000, 192_000
	resp := e.get(t, fmt.Sprintf("/stream?src=lib/ramp.wav&format=flac&from=%d&to=%d&t=0.5", from, to), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodePCM(t, readBody(t, resp))

	// Half a second into a 2-second span: 24000 samples in, so 72000 left.
	const seek = 24_000
	if want := int(to-from) - seek; got.N != want {
		t.Fatalf("seek inside a span delivered %d samples, want %d", got.N, want)
	}
	whole := testutil.Ramp(audio.Format{Rate: 48000, Channels: 2,
		Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, 4*48000)
	defer audio.Put(whole)
	if got, want := got.ChanI(0)[0], whole.ChanI(0)[from+seek]; got != want {
		t.Errorf("t=0.5 inside span [%d, %d) starts at %d, want the source's sample %d (%d); "+
			"the seek addressed the file rather than the virtual track", from, to, got, from+seek, want)
	}
}

// TestSpanDeclinesDirectPlay guards the asymmetry that makes this easy to
// get wrong. A span that only trims the end leaves from at 0, so it passes
// every clause the direct-play check had before it, and the response would
// be the whole file's original bytes for a request that asked for one track
// of it.
func TestSpanDeclinesDirectPlay(t *testing.T) {
	e := newTestEnv(t, nil)
	// format=wav on a wav source is exactly the request that direct-plays
	// when nothing narrows it.
	full := e.get(t, "/stream?src=lib/ramp.wav&format=wav", nil)
	if full.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", full.StatusCode)
	}
	whole := decodePCM(t, readBody(t, full))
	if whole.N != 4*48000 {
		t.Fatalf("the unspanned request delivered %d samples, want the whole file", whole.N)
	}

	trimmed := e.get(t, "/stream?src=lib/ramp.wav&format=wav&to=48000", nil)
	if trimmed.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", trimmed.StatusCode)
	}
	got := decodePCM(t, readBody(t, trimmed))
	if got.N != 48000 {
		t.Fatalf("an end-trimmed span delivered %d samples, want 48000; "+
			"a to= with no from= direct-played the whole file", got.N)
	}
}

// TestSpanIsCheckedAgainstAMeasuredLength covers the source whose header
// cannot police a span: one that declares no length at all.
//
// SpanTrack refuses a window past the end, and that refusal is the span
// surface's honesty gate. It can only fire against a total it was given,
// though, so a source that declares none (-1) skips it entirely and any window
// is accepted, however far past the end it sits. The whole-file stream this
// path was built for depends on no number (it ends where the samples end),
// which is why trusting the header was free there and is not free here: a span
// IS a claim about the number.
//
// The 400 is what the surface promises, and it is checked against a real span
// of the same source so the refusal cannot be a version that refuses
// everything.
func TestSpanIsCheckedAgainstAMeasuredLength(t *testing.T) {
	e := newTestEnv(t, nil)
	ref := adtsFixture(t, e, "ramp.aac")

	// The fixture is a transcode of a 4 s source, so its real end is within one
	// AAC frame of 192000 samples: 400000 is past it, and [1000, 50000) is
	// inside it, without either restating a length no header declares.
	t.Run("past the real end", func(t *testing.T) {
		resp := e.get(t, "/stream?src="+ref+"&format=flac&from=0&to=400000", nil)
		wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
	})
	t.Run("inside it", func(t *testing.T) {
		resp := e.get(t, "/stream?src="+ref+"&format=flac&from=1000&to=50000", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a span inside the source = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
		}
		if got := decodePCM(t, readBody(t, resp)); got.N != 49000 {
			t.Errorf("the span delivered %d samples, want 49000", got.N)
		}
	})
}

func TestSpanRejects(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, tc := range []struct{ name, query string }{
		{"negative from", "from=-1"},
		{"zero to", "to=0"},
		{"inverted", "from=500&to=100"},
		{"not a number", "from=abc"},
		{"past the end", "from=0&to=999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.get(t, "/stream?src=lib/ramp.wav&format=flac&"+tc.query, nil)
			wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
		})
	}
}
