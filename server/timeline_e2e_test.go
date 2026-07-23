// The multi-source timeline acceptance suite: minting a play queue into a
// content-addressed digest, streaming it gaplessly over HLS behind one init
// and one edit list, and the enforcement that keeps a tl= URL as honest as a
// src= one (identity, gain policy, the re-mint contract).
package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/dsp/silence"
	"github.com/colespringer/waxflow/internal/timeline"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// timelineEnv is a daemon with timelines enabled and an album of WAVs whose
// lengths differ, so a bug that assumed uniform members shows up as an
// arithmetic error rather than passing by symmetry.
func timelineEnv(t *testing.T) (*testEnv, []string) {
	t.Helper()
	var env *testEnv
	env = newTestEnv(t, func(cfg *server.Config) {
		cfg.TimelineDir = filepath.Join(t.TempDir(), "timelines")
	})
	lens := []int{48000, 24000, 72000} // 1 s, 0.5 s, 1.5 s at 48 kHz
	refs := make([]string, len(lens))
	for i, n := range lens {
		name := fmt.Sprintf("tl-%d.wav", i)
		if err := os.WriteFile(filepath.Join(env.root, name), rampWAV(t, 48000, 2, n), 0o644); err != nil {
			t.Fatal(err)
		}
		refs[i] = "lib/" + name
	}
	return env, refs
}

// mintTimeline posts a queue and returns the decoded 201 body.
func mintTimeline(t *testing.T, env *testEnv, refs []string) client.TimelineResponse {
	t.Helper()
	body, err := json.Marshal(client.TimelineRequest{Srcs: timelineSrcsOf(refs)})
	if err != nil {
		t.Fatal(err)
	}
	resp := env.postJSON(t, "/hls/timeline", string(body))
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /hls/timeline = %d, want 201: %s", resp.StatusCode, raw)
	}
	var v client.TimelineResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func timelineSrcsOf(refs []string) []client.TimelineSrc {
	out := make([]client.TimelineSrc, len(refs))
	for i, r := range refs {
		out[i] = client.TimelineSrc{Src: r}
	}
	return out
}

// TestTimelineEndToEnd is M23's delivery gate: a queue mints, signs, and
// plays over HLS as one continuous stream.
//
// The playlist is what proves the design rather than the bytes: a timeline is
// continuous, so it is one media playlist with one EXT-X-MAP and no
// EXT-X-DISCONTINUITY anywhere. If the seams were real, HLS would need a
// discontinuity and a second init at each, which one chain and one edit list
// forbid structurally.
func TestTimelineEndToEnd(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)

	if tl.Members != 3 {
		t.Fatalf("the timeline holds %d members, want 3", tl.Members)
	}
	if want := 3.0; tl.DurationSeconds != want {
		t.Fatalf("durationSeconds = %v, want %v (1 + 0.5 + 1.5)", tl.DurationSeconds, want)
	}
	if len(tl.Tl) != 43 {
		t.Fatalf("tl = %q, want a 43-character digest", tl.Tl)
	}

	// Minting the same queue again is the same timeline: content-addressed,
	// so a client that lost the digest pays nothing to ask for it again.
	if again := mintTimeline(t, env, refs); again.Tl != tl.Tl {
		t.Fatalf("re-minting the same queue gave %q, want %q", again.Tl, tl.Tl)
	}

	masterURL := mintHLS(t, env, map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off"})
	master := readBody(t, keyless(t, env, masterURL))
	var mediaURL string
	for _, l := range playlistLines(master) {
		if l != "" && l[0] != '#' {
			mediaURL = "/hls/" + l
		}
	}
	if mediaURL == "" {
		t.Fatalf("no variant in the master playlist: %s", master)
	}

	media := readBody(t, keyless(t, env, mediaURL))
	initURI, segURIs, extinf := mediaURIs(t, media)
	if initURI == "" || len(segURIs) == 0 {
		t.Fatalf("media playlist has no init or no segments: %s", media)
	}
	for _, l := range playlistLines(media) {
		if l == "#EXT-X-DISCONTINUITY" {
			t.Fatalf("the playlist marks a discontinuity; a concatenated timeline is continuous: %s", media)
		}
	}
	// One init for the whole timeline, not one per member.
	var total float64
	for _, d := range extinf {
		total += d
	}
	if total < 2.9 || total > 3.2 {
		t.Fatalf("the playlist promises %.3f s, want the timeline's 3 s", total)
	}

	// The init and every segment must actually serve: the workers open the
	// members lazily, mid-stream, so a broken member handoff shows up here
	// and nowhere earlier.
	if resp := keyless(t, env, "/hls/"+initURI); resp.StatusCode != 200 {
		t.Fatalf("init = %d, want 200", resp.StatusCode)
	}
	for i, u := range segURIs {
		resp := keyless(t, env, "/hls/"+u)
		body := readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("segment %d = %d, want 200: %s", i, resp.StatusCode, body)
		}
		if len(body) == 0 {
			t.Fatalf("segment %d is empty", i)
		}
	}
}

// TestTimelineBoundaries pins A17: the mint reports per-member sample offsets
// and durations at the envelope rate, so a client need not re-probe every
// member to know where each one lands. This mint requests no crossfade, so the
// members tile exactly: boundaries[0] starts at 0, each starts where the last
// ends, and the last one's end is the whole timeline's length.
func TestTimelineBoundaries(t *testing.T) {
	env, refs := timelineEnv(t) // 1 s, 0.5 s, 1.5 s at 48 kHz
	tl := mintTimeline(t, env, refs)

	if tl.EnvelopeRate != 48000 {
		t.Fatalf("envelopeRate = %d, want 48000 (the members' common rate)", tl.EnvelopeRate)
	}
	if len(tl.Boundaries) != len(refs) {
		t.Fatalf("got %d boundaries, want one per member (%d)", len(tl.Boundaries), len(refs))
	}
	if tl.Boundaries[0].OffsetSamples != 0 {
		t.Fatalf("boundaries[0].offsetSamples = %d, want 0", tl.Boundaries[0].OffsetSamples)
	}
	wantDurations := []int64{48000, 24000, 72000}
	for i, want := range wantDurations {
		if tl.Boundaries[i].DurationSamples != want {
			t.Errorf("boundaries[%d].durationSamples = %d, want %d",
				i, tl.Boundaries[i].DurationSamples, want)
		}
	}
	// With no crossfade the members tile: each starts where the previous ended.
	for i := 0; i+1 < len(tl.Boundaries); i++ {
		got := tl.Boundaries[i+1].OffsetSamples
		want := tl.Boundaries[i].OffsetSamples + tl.Boundaries[i].DurationSamples
		if got != want {
			t.Errorf("boundaries[%d].offsetSamples = %d, want %d (the previous member's end); "+
				"without a crossfade the members must tile", i+1, got, want)
		}
	}
	// The last member's end is the whole timeline: durationSeconds * envelopeRate.
	last := tl.Boundaries[len(tl.Boundaries)-1]
	end := last.OffsetSamples + last.DurationSamples
	if want := int64(tl.DurationSeconds * float64(tl.EnvelopeRate)); end != want {
		t.Fatalf("the last member ends at sample %d, but durationSeconds*envelopeRate is %d: "+
			"the boundaries and the reported length disagree", end, want)
	}
}

// TestTimelineBoundariesMixedRate pins that offsets normalize to the envelope
// rate rather than any one member's: a 44.1 kHz member in a 48 kHz timeline is
// reported at its resampled length, so a consumer reads one clock.
func TestTimelineBoundariesMixedRate(t *testing.T) {
	env, _ := timelineEnv(t)
	// A 44.1 kHz member alongside a 48 kHz one: the envelope rate is 48 kHz.
	if err := os.WriteFile(filepath.Join(env.root, "tl-44.wav"), rampWAV(t, 44100, 2, 44100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "tl-48.wav"), rampWAV(t, 48000, 2, 48000), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := mintTimeline(t, env, []string{"lib/tl-44.wav", "lib/tl-48.wav"})

	if tl.EnvelopeRate != 48000 {
		t.Fatalf("envelopeRate = %d, want 48000 (the higher member rate)", tl.EnvelopeRate)
	}
	// One second at 44.1 kHz resampled to 48 kHz is 48000 samples, so the second
	// member starts there rather than at the source's 44100.
	if got := tl.Boundaries[1].OffsetSamples; got != 48000 {
		t.Fatalf("member 1 starts at sample %d, want 48000 (member 0 resampled to the envelope rate), "+
			"not its source length 44100", got)
	}
}

// postTimeline posts a queue and returns the raw response, for callers that
// assert on the status themselves.
func postTimeline(t *testing.T, env *testEnv, req client.TimelineRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return env.postJSON(t, "/hls/timeline", string(body))
}

// mintTimelineXfade posts a queue with a crossfade and returns the decoded 201.
func mintTimelineXfade(t *testing.T, env *testEnv, refs []string, seconds float64) client.TimelineResponse {
	t.Helper()
	resp := postTimeline(t, env, client.TimelineRequest{Srcs: timelineSrcsOf(refs), CrossfadeSeconds: seconds})
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /hls/timeline (crossfade %v) = %d, want 201: %s", seconds, resp.StatusCode, raw)
	}
	var v client.TimelineResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestTimelineCrossfadeReshapesResponse pins A16 at the mint: crossfadeSeconds
// shapes the response's duration and boundaries. A crossfade of X at each of the
// N-1 seams shortens the timeline by (N-1)X, the members' raw durations are
// unchanged, and their offsets overlap by X.
func TestTimelineCrossfadeReshapesResponse(t *testing.T) {
	env, refs := timelineEnv(t) // 48000, 24000, 72000 samples at 48 kHz
	const seconds = 0.1
	const x = 4800 // round(0.1 * 48000)
	tl := mintTimelineXfade(t, env, refs, seconds)

	// sum(members) - (N-1)X = 144000 - 2*4800 = 134400 samples = 2.8 s.
	if want := (144000.0 - 2*float64(x)) / 48000.0; tl.DurationSeconds != want {
		t.Fatalf("durationSeconds = %v, want %v (sum - (N-1)X)", tl.DurationSeconds, want)
	}
	wantDur := []int64{48000, 24000, 72000}
	wantOff := []int64{0, 48000 - x, 48000 - x + 24000 - x} // 0, 43200, 62400
	for i := range refs {
		if tl.Boundaries[i].DurationSamples != wantDur[i] {
			t.Errorf("boundaries[%d].durationSamples = %d, want the raw %d (a blend does not shorten a member)",
				i, tl.Boundaries[i].DurationSamples, wantDur[i])
		}
		if tl.Boundaries[i].OffsetSamples != wantOff[i] {
			t.Errorf("boundaries[%d].offsetSamples = %d, want %d", i, tl.Boundaries[i].OffsetSamples, wantOff[i])
		}
	}
	// Consecutive members overlap: the crossfade shares the seam region.
	for i := 0; i+1 < len(tl.Boundaries); i++ {
		end := tl.Boundaries[i].OffsetSamples + tl.Boundaries[i].DurationSamples
		if end <= tl.Boundaries[i+1].OffsetSamples {
			t.Errorf("member %d ends at %d but member %d starts at %d; a crossfade must overlap them",
				i, end, i+1, tl.Boundaries[i+1].OffsetSamples)
		}
	}
}

// TestTimelineCrossfadeKeepsOneDigest pins the identity decision A16 records in
// ADR-0009: a crossfade is a render option, not part of the timeline's identity.
// The same members minted with and without a crossfade name one tl=; only the
// response the mint shapes differs.
func TestTimelineCrossfadeKeepsOneDigest(t *testing.T) {
	env, refs := timelineEnv(t)
	plain := mintTimeline(t, env, refs)
	faded := mintTimelineXfade(t, env, refs, 0.1)
	if faded.Tl != plain.Tl {
		t.Fatalf("crossfade changed the digest (%q vs %q); it is a render option, not identity", faded.Tl, plain.Tl)
	}
	if !(faded.DurationSeconds < plain.DurationSeconds) {
		t.Fatalf("the crossfade did not take: faded duration %v is not shorter than plain %v",
			faded.DurationSeconds, plain.DurationSeconds)
	}
}

// TestTimelineCrossfadeMintRefusals pins that a crossfade the members cannot
// carry, or a nonsensical one, is refused at the mint where the client can act
// on it rather than at the first segment request.
func TestTimelineCrossfadeMintRefusals(t *testing.T) {
	env, refs := timelineEnv(t)
	for _, tc := range []struct {
		name    string
		seconds float64
	}{
		{"a negative crossfade", -1},
		{"a crossfade longer than the shortest member can carry", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postTimeline(t, env, client.TimelineRequest{Srcs: timelineSrcsOf(refs), CrossfadeSeconds: tc.seconds})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("mint with a %v s crossfade = %d, want 400: %s", tc.seconds, resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

// renderTimeline drives a signed master to its media playlist and returns the
// playlist's promised length and segment 0's bytes, so two renders of one
// timeline can be compared.
func renderTimeline(t *testing.T, env *testEnv, masterURL string) (totalSeconds float64, seg0 []byte) {
	t.Helper()
	master := readBody(t, keyless(t, env, masterURL))
	var mediaURL string
	for _, l := range playlistLines(master) {
		if l != "" && l[0] != '#' {
			mediaURL = "/hls/" + l
		}
	}
	if mediaURL == "" {
		t.Fatalf("no variant in the master playlist: %s", master)
	}
	_, segURIs, extinf := mediaURIs(t, readBody(t, keyless(t, env, mediaURL)))
	for _, d := range extinf {
		totalSeconds += d
	}
	if len(segURIs) == 0 {
		t.Fatal("media playlist has no segments")
	}
	resp := keyless(t, env, "/hls/"+segURIs[0])
	seg0 = readBody(t, resp)
	if resp.StatusCode != 200 || len(seg0) == 0 {
		t.Fatalf("segment 0 = %d (%d bytes)", resp.StatusCode, len(seg0))
	}
	return totalSeconds, seg0
}

// TestTimelineCrossfadeRender pins the render half of A16: the signed master
// carries crossfadeSeconds, the media playlist promises the shortened length,
// and the crossfaded render does not collide in the cache with the butt-joined
// one. The two renders share one tl= and one canonical parameter set, so only
// the crossfadeVersion the plan folds into the cache key keeps them apart; if it
// did not, the faded render would serve the plain render's cached segment.
func TestTimelineCrossfadeRender(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)

	// segDur 2 s puts both seams (at 1 s and 1.5 s) inside segment 0, so the
	// blend is observable there.
	base := map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off", "segDur": "2"}
	faded := map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off", "segDur": "2", "crossfadeSeconds": "0.2"}

	plainTotal, plainSeg0 := renderTimeline(t, env, mintHLS(t, env, base))
	fadedTotal, fadedSeg0 := renderTimeline(t, env, mintHLS(t, env, faded))

	// A 0.2 s blend at 2 seams shortens the timeline by ~0.4 s.
	if !(fadedTotal < plainTotal-0.3) {
		t.Fatalf("faded playlist promises %.3f s, plain %.3f s; a 0.2 s crossfade at 2 seams should shorten it ~0.4 s",
			fadedTotal, plainTotal)
	}
	if bytes.Equal(plainSeg0, fadedSeg0) {
		t.Fatal("the crossfaded and butt-joined renders returned identical segment 0 bytes: " +
			"either the blend did not apply or the cache key did not separate the two renders of one tl=")
	}
}

// TestTimelineCrossfadeSignRefusals pins that an invalid crossfade in a
// URL-minting request is refused at sign time, not served: an oversized one on a
// timeline (refused at plan time by checkCrossfade), and any crossfade on a
// single source (which has no seam).
func TestTimelineCrossfadeSignRefusals(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		params map[string]string
	}{
		{"a crossfade longer than the shortest member is refused at plan time",
			map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off", "crossfadeSeconds": "100"}},
		{"a crossfade on a single source is refused",
			map[string]string{"src": refs[0], "format": "opus", "crossfadeSeconds": "0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8", Params: tc.params})
			switch {
			case err == nil:
				t.Fatal("signing accepted an invalid crossfade")
			case waxerr.CodeOf(err) != waxerr.CodeInvalidRequest:
				t.Fatalf("refusal was %v, want an invalid-request", waxerr.CodeOf(err))
			}
		})
	}
}

// TestTimelineCrossfadeSingleMemberIsButtJoin pins a deliberate N=1 edge: a
// one-member timeline accepts a crossfade and applies it to its zero seams (a
// butt-join) rather than refusing it. This is the engine's documented N=1
// handling (checkCrossfade "makes N=1 pass with no special case") surfaced on
// the wire, and it is on purpose: a queue-driven client sends one render for a
// queue of any length, so a crossfade that happens to meet a single-track queue
// must no-op, not 400. It differs from a single src= URL, which refuses a
// crossfade because that is a different kind of URL (one file, no seam by type,
// not a timeline with a count of one).
func TestTimelineCrossfadeSingleMemberIsButtJoin(t *testing.T) {
	env, refs := timelineEnv(t)
	one := refs[:1]
	plain := mintTimeline(t, env, one)
	faded := mintTimelineXfade(t, env, one, 0.5)
	if faded.Tl != plain.Tl {
		t.Fatalf("a crossfade changed a one-member timeline's digest (%q vs %q)", faded.Tl, plain.Tl)
	}
	if len(faded.Boundaries) != 1 {
		t.Fatalf("got %d boundaries, want 1", len(faded.Boundaries))
	}
	if faded.DurationSeconds != plain.DurationSeconds {
		t.Errorf("a crossfade shortened a seamless one-member timeline: %v vs %v (there is no seam to blend)",
			faded.DurationSeconds, plain.DurationSeconds)
	}
	// The one bound that still applies at N=1 is the blend buffer: an absurd
	// crossfade is refused even with no seam, which is why the no-op is scoped to
	// "for want of a seam" and not "never refused".
	resp := postTimeline(t, env, client.TimelineRequest{Srcs: timelineSrcsOf(one), CrossfadeSeconds: 100})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an oversized crossfade on a one-member timeline = %d, want 400 (the blend-buffer bound "+
			"applies at any length): %s", resp.StatusCode, readBody(t, resp))
	}
}

// TestClientCreateTimelineCrossfade pins that the Go client can actually mint a
// crossfaded timeline: CreateTimeline takes the request whole (like Sign), so
// its CrossfadeSeconds reaches the wire and the response reflects it. The field
// on TimelineRequest would be unreachable through the client without this.
func TestClientCreateTimelineCrossfade(t *testing.T) {
	env, refs := timelineEnv(t)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	tl, jobID, err := c.CreateTimeline(t.Context(), client.TimelineRequest{
		Srcs: timelineSrcsOf(refs), CrossfadeSeconds: 0.1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "" || tl == nil {
		t.Fatalf("CreateTimeline returned job %q / tl %v, want an inline digest", jobID, tl)
	}
	if len(tl.Boundaries) != len(refs) {
		t.Fatalf("got %d boundaries, want one per member (%d)", len(tl.Boundaries), len(refs))
	}
	// The crossfade took: consecutive members overlap (a butt-join would tile).
	end0 := tl.Boundaries[0].OffsetSamples + tl.Boundaries[0].DurationSamples
	if tl.Boundaries[1].OffsetSamples >= end0 {
		t.Errorf("member 1 starts at %d and member 0 ends at %d; the client's crossfade did not reach the wire",
			tl.Boundaries[1].OffsetSamples, end0)
	}
}

// TestTimelineIdentityIs410 pins that a tl= URL is exactly as honest as a
// src= one. The digest covers its members' identities, so a member replaced
// on disk cannot match the digest minted against it, and the URL must 410
// rather than quietly serve different audio.
func TestTimelineIdentityIs410(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	masterURL := mintHLS(t, env, map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off"})
	if resp := keyless(t, env, masterURL); resp.StatusCode != 200 {
		t.Fatalf("master = %d before the edit, want 200", resp.StatusCode)
	}

	// Replace the middle member: a new size and mtime, so a new identity.
	if err := os.WriteFile(filepath.Join(env.root, "tl-1.wav"), rampWAV(t, 48000, 2, 96000), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := keyless(t, env, masterURL)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("master = %d after a member was replaced, want 410: %s",
			resp.StatusCode, readBody(t, resp))
	}
}

// TestTimelineUnknownDigestIsNotFound pins the re-mint contract: a timeline
// that is gone is a 404 the client answers by minting again from the queue it
// still has, without resetting its position.
func TestTimelineUnknownDigestIsNotFound(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	masterURL := mintHLS(t, env, map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off"})

	// A second daemon that never saw this digest, which is what an eviction,
	// a wiped data directory, or a sweep looks like from the URL's side. The
	// signing keys are the fixture's, so the URL still verifies: the 404 has
	// to come from the timeline being gone, not from the signature failing,
	// which is exactly the case a client must handle by re-minting.
	cold, _ := timelineEnv(t)
	resp := keyless(t, cold, masterURL)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a master naming an unknown timeline = %d, want 404: %s",
			resp.StatusCode, readBody(t, resp))
	}
}

// TestTimelineRefusesTagGain pins the gain policy, at mint time where a
// client can act on it. A timeline is one chain, so it has one gain, and
// there is no honest single answer to read out of N members' tags; per-track
// gain in particular would step the level at every seam, which is the very
// artifact album gain exists to prevent.
func TestTimelineRefusesTagGain(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		params map[string]string
		wantOK bool
	}{
		{"track gain is refused", map[string]string{"tl": tl.Tl, "format": "opus", "gain": "track"}, false},
		{"album gain is refused", map[string]string{"tl": tl.Tl, "format": "opus", "gain": "album"}, false},
		{"the daemon default is refused, and says so", map[string]string{"tl": tl.Tl, "format": "opus"}, false},
		{"gain=off is fine", map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off"}, true},
		{"an explicit dB is fine", map[string]string{"tl": tl.Tl, "format": "opus", "gain": "-6.2"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8", Params: tc.params})
			switch {
			case tc.wantOK && err != nil:
				t.Fatalf("signing failed: %v", err)
			case !tc.wantOK && err == nil:
				t.Fatal("signing accepted a tag-derived gain for a timeline")
			case !tc.wantOK && waxerr.CodeOf(err) != waxerr.CodeInvalidRequest:
				t.Fatalf("refusal was %v, want an invalid-request", waxerr.CodeOf(err))
			}
		})
	}
}

// TestTimelineAndSrcAreExclusive pins that a URL names one stream or one
// timeline. They are not two spellings of one fact, so accepting both would
// leave the daemon to pick, silently.
func TestTimelineAndSrcAreExclusive(t *testing.T) {
	env, refs := timelineEnv(t)
	tl := mintTimeline(t, env, refs)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8",
		Params: map[string]string{"src": refs[0], "tl": tl.Tl, "format": "opus", "gain": "off"}})
	if err == nil {
		t.Fatal("signing accepted both a src and a tl")
	}
}

// TestTimelineMintRejects pins the mint's own gate: a queue that cannot be
// timelined fails at the mint, where the client is still listening, rather
// than at the first segment request minutes into playback. The window rows
// name the member in the message (want), because a thousand-member queue's
// caller needs to know which member's window is nonsense; the past-the-end
// row is SpanTrack's refuse-don't-clamp reaching the wire, and the empty
// window is the one shape that passes both the sanity check and SpanTrack
// ({from: measured length, to: 0}) and is refused by the mint itself.
func TestTimelineMintRejects(t *testing.T) {
	env, _ := timelineEnv(t) // tl-0.wav holds 48000 samples
	for _, tc := range []struct {
		name string
		body string
		want string // required substring of the refusal, "" for any
	}{
		{"an empty queue", `{"srcs":[]}`, ""},
		{"a member with no src", `{"srcs":[{"src":""}]}`, ""},
		{"a member that does not resolve", `{"srcs":[{"src":"lib/nope.wav"}]}`, ""},
		{"a negative window start", `{"srcs":[{"src":"lib/tl-0.wav","from":-1}]}`,
			"member 0: from -1"},
		{"a negative window end", `{"srcs":[{"src":"lib/tl-0.wav","to":-1}]}`,
			"member 0: to -1"},
		{"a window ending before its start", `{"srcs":[{"src":"lib/tl-0.wav","from":200,"to":100}]}`,
			"member 0: span [200, 100) ends before it starts"},
		{"a window ending at its start", `{"srcs":[{"src":"lib/tl-0.wav","from":100,"to":100}]}`,
			"member 0: span [100, 100) ends before it starts"},
		{"a window past the measured end", `{"srcs":[{"src":"lib/tl-0.wav","to":48001}]}`,
			"past the source's 48000 samples"},
		{"an empty window at the measured end", `{"srcs":[{"src":"lib/tl-0.wav","from":48000}]}`,
			"member 0: window is empty on this source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.postJSON(t, "/hls/timeline", tc.body)
			body := readBody(t, resp)
			if resp.StatusCode < 400 {
				t.Fatalf("POST /hls/timeline = %d, want a refusal: %s", resp.StatusCode, body)
			}
			if tc.want != "" && !strings.Contains(string(body), tc.want) {
				t.Fatalf("the refusal does not name the problem: want %q in %s", tc.want, body)
			}
		})
	}
}

// TestTimelineDisabledByDefault pins that the surface follows its store: a
// daemon with no timeline directory must not advertise timelines in /caps and
// must not route the endpoint, rather than accepting a mint it cannot keep.
func TestTimelineDisabledByDefault(t *testing.T) {
	env := newTestEnv(t, nil) // no TimelineDir
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := c.Caps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Delivery.Timelines {
		t.Error("/caps advertises timelines on a daemon with no timeline store")
	}
	resp := env.postJSON(t, "/hls/timeline", `{"srcs":[{"src":"lib/sine.wav"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /hls/timeline = %d with timelines disabled, want 404", resp.StatusCode)
	}
}

// TestTimelineCapsAreHonest pins the other direction: an enabled daemon says
// so, and says what it will accept, so a client routes by capability instead
// of by trying.
func TestTimelineCapsAreHonest(t *testing.T) {
	env, _ := timelineEnv(t)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := c.Caps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Delivery.Timelines {
		t.Fatal("/caps does not advertise timelines on a daemon that serves them")
	}
	if caps.Delivery.MaxTimelineMembers != 1000 {
		t.Fatalf("maxTimelineMembers = %d, want the enforced 1000", caps.Delivery.MaxTimelineMembers)
	}

	// The cut/HLS wire coverage rides this enabled-daemon client scaffold rather
	// than standing up a second one: this is the only place a real client.Caps()
	// over HTTP proves the newly-mirrored client Delivery.CutFormats and
	// Delivery.HLSFormats carry the right JSON tags and survive the wire.
	if want := waxflow.CutFormats(); !slices.Equal(caps.Delivery.CutFormats, want) {
		t.Errorf("delivery.cutFormats = %v over the wire, want %v", caps.Delivery.CutFormats, want)
	}
	if want := waxflow.SegmentedFormats(); !slices.Equal(caps.Delivery.HLSFormats, want) {
		t.Errorf("delivery.hlsFormats = %v over the wire, want %v", caps.Delivery.HLSFormats, want)
	}
	if want := silence.Version; caps.DSP.SilenceDetector != want {
		t.Errorf("dsp.silenceDetector = %q over the wire, want %q", caps.DSP.SilenceDetector, want)
	}
	// An enabled daemon advertises member windows, so a CUE-driven client
	// routes by capability (absent = older daemon, per-item fallback).
	if !caps.Delivery.TimelineMemberWindows {
		t.Error("/caps does not advertise timelineMemberWindows on a daemon that serves them")
	}
}

// mintTimelineSrcs posts a queue of full member objects (windows included)
// and returns the decoded 201, the windowed sibling of mintTimeline.
func mintTimelineSrcs(t *testing.T, env *testEnv, srcs []client.TimelineSrc) client.TimelineResponse {
	t.Helper()
	resp := postTimeline(t, env, client.TimelineRequest{Srcs: srcs})
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /hls/timeline = %d, want 201: %s", resp.StatusCode, raw)
	}
	var v client.TimelineResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// fetchVariant drives a signed master to its media playlist and returns the
// playlist body plus the init's and every segment's bytes, so two renders can
// be compared segment for segment.
func fetchVariant(t *testing.T, env *testEnv, masterURL string) (media, init []byte, segs [][]byte) {
	t.Helper()
	master := readBody(t, keyless(t, env, masterURL))
	var mediaURL string
	for _, l := range playlistLines(master) {
		if l != "" && l[0] != '#' {
			mediaURL = "/hls/" + l
		}
	}
	if mediaURL == "" {
		t.Fatalf("no variant in the master playlist: %s", master)
	}
	media = readBody(t, keyless(t, env, mediaURL))
	initURI, segURIs, _ := mediaURIs(t, media)
	if initURI == "" || len(segURIs) == 0 {
		t.Fatalf("media playlist has no init or no segments: %s", media)
	}
	init = readBody(t, keyless(t, env, "/hls/"+initURI))
	for i, u := range segURIs {
		resp := keyless(t, env, "/hls/"+u)
		body := readBody(t, resp)
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("segment %d = %d (%d bytes)", i, resp.StatusCode, len(body))
		}
		segs = append(segs, body)
	}
	return media, init, segs
}

// TestTimelineWindowedMembersTileOneFile is A23's headline gapless claim,
// TestSpansJoinGaplessly extended into timelines: two windows tiling one file
// are the whole file, delivered as one continuous stream. The split falls at
// a CD-frame-style offset that is not a whole second (137 * 588 samples),
// which is exactly where a seconds-based window would drop or repeat a sample.
//
// Byte equality against the whole-file timeline is the honest assertion, not
// an over-tight one: the encoders are deterministic, both timelines carry the
// same envelope and segment plan, and the members butt-join within one source
// at one rate, so any difference is a real seam.
func TestTimelineWindowedMembersTileOneFile(t *testing.T) {
	env, _ := timelineEnv(t)
	const frames = 4 * 48000
	const boundary = 137 * 588 // 80556: 1.678...s at 48 kHz, no round number
	if err := os.WriteFile(filepath.Join(env.root, "tile.wav"), rampWAV(t, 48000, 2, frames), 0o644); err != nil {
		t.Fatal(err)
	}

	tlA := mintTimelineSrcs(t, env, []client.TimelineSrc{
		{Src: "lib/tile.wav", From: 0, To: boundary},
		{Src: "lib/tile.wav", From: boundary}, // open end: to the end of the source
	})
	tlB := mintTimelineSrcs(t, env, []client.TimelineSrc{{Src: "lib/tile.wav"}})

	if tlA.DurationSeconds != tlB.DurationSeconds {
		t.Fatalf("the tiling windows sum to %v s, the whole file to %v s",
			tlA.DurationSeconds, tlB.DurationSeconds)
	}
	params := func(tl string) map[string]string {
		return map[string]string{"tl": tl, "format": "opus", "gain": "off", "segDur": "1"}
	}
	mediaA, initA, segsA := fetchVariant(t, env, mintHLS(t, env, params(tlA.Tl)))
	mediaB, initB, segsB := fetchVariant(t, env, mintHLS(t, env, params(tlB.Tl)))

	for _, l := range playlistLines(mediaA) {
		if l == "#EXT-X-DISCONTINUITY" {
			t.Fatalf("the windowed timeline marks a discontinuity; two windows tiling one file are continuous: %s", mediaA)
		}
	}
	_, _, extA := mediaURIs(t, mediaA)
	_, _, extB := mediaURIs(t, mediaB)
	var totalA, totalB float64
	for _, d := range extA {
		totalA += d
	}
	for _, d := range extB {
		totalB += d
	}
	if totalA != totalB {
		t.Fatalf("the windowed playlist promises %.6f s, the whole-file one %.6f s", totalA, totalB)
	}
	if len(segsA) != len(segsB) {
		t.Fatalf("the windowed render has %d segments, the whole-file one %d", len(segsA), len(segsB))
	}
	if !bytes.Equal(initA, initB) {
		t.Error("the init segments differ; one file windowed into tiles is not the same stream as the file")
	}
	for i := range segsA {
		if !bytes.Equal(segsA[i], segsB[i]) {
			t.Errorf("segment %d differs between the tiled and whole-file renders; "+
				"the windows do not join gaplessly", i)
		}
	}
}

// TestTimelineWindowDigestIsDistinct pins the identity decision: a window says
// which samples are this member, so it joins the digest, the opposite call
// from crossfade by the same doctrine (ADR-0009's A23 amendment). Re-minting
// the same windowed body is idempotent, exactly as for whole files.
func TestTimelineWindowDigestIsDistinct(t *testing.T) {
	env, refs := timelineEnv(t)
	whole := mintTimeline(t, env, refs[:1])
	windowed := mintTimelineSrcs(t, env, []client.TimelineSrc{{Src: refs[0], From: 4800, To: 24000}})
	if windowed.Tl == whole.Tl {
		t.Fatalf("a windowed member kept the whole-file digest %q; a window is content identity", whole.Tl)
	}
	if again := mintTimelineSrcs(t, env, []client.TimelineSrc{{Src: refs[0], From: 4800, To: 24000}}); again.Tl != windowed.Tl {
		t.Fatalf("re-minting the same windowed body gave %q, want %q", again.Tl, windowed.Tl)
	}
}

// TestTimelineWindowBoundaries pins that the mint's reported shape reads the
// spanned lengths: durations are window lengths (an open To running to the
// measured end), offsets are their prefix sum, and durationSeconds and
// envelopeRate follow.
func TestTimelineWindowBoundaries(t *testing.T) {
	env, refs := timelineEnv(t) // 48000, 24000, 72000 samples at 48 kHz
	tl := mintTimelineSrcs(t, env, []client.TimelineSrc{
		{Src: refs[0], From: 4800, To: 28800}, // 24000 samples
		{Src: refs[1]},                        // whole file: 24000
		{Src: refs[2], From: 36000},           // open end: 72000 - 36000 = 36000
	})
	if tl.EnvelopeRate != 48000 {
		t.Fatalf("envelopeRate = %d, want 48000", tl.EnvelopeRate)
	}
	if want := 84000.0 / 48000.0; tl.DurationSeconds != want {
		t.Fatalf("durationSeconds = %v, want %v (the windows' sum)", tl.DurationSeconds, want)
	}
	wantDur := []int64{24000, 24000, 36000}
	wantOff := []int64{0, 24000, 48000}
	for i := range wantDur {
		if tl.Boundaries[i].DurationSamples != wantDur[i] {
			t.Errorf("boundaries[%d].durationSamples = %d, want the window's %d",
				i, tl.Boundaries[i].DurationSamples, wantDur[i])
		}
		if tl.Boundaries[i].OffsetSamples != wantOff[i] {
			t.Errorf("boundaries[%d].offsetSamples = %d, want %d", i, tl.Boundaries[i].OffsetSamples, wantOff[i])
		}
	}
}

// TestTimelineCrossfadeFitsSpannedMembers pins that a crossfade is held to the
// window, not the file: checkCrossfade reads the members' lengths after
// SpanTrack narrowed them, so a blend the whole file could carry is refused
// when the window cannot.
func TestTimelineCrossfadeFitsSpannedMembers(t *testing.T) {
	env, refs := timelineEnv(t) // member 0 holds 48000 samples
	// 0.1 s at 48 kHz is 4800 samples of tail zone; a 2400-sample window
	// cannot carry it, though its whole file could.
	resp := postTimeline(t, env, client.TimelineRequest{
		Srcs:             []client.TimelineSrc{{Src: refs[0], To: 2400}, {Src: refs[2]}},
		CrossfadeSeconds: 0.1,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a crossfade longer than the window = %d, want 400: %s", resp.StatusCode, readBody(t, resp))
	}
	if whole := mintTimelineXfade(t, env, []string{refs[0], refs[2]}, 0.1); len(whole.Boundaries) != 2 {
		t.Fatalf("the same crossfade on the whole pair did not mint: %+v", whole)
	}
	// A window that fits blends: consecutive members overlap by the zone.
	tl := postTimeline(t, env, client.TimelineRequest{
		Srcs:             []client.TimelineSrc{{Src: refs[0], To: 24000}, {Src: refs[2]}},
		CrossfadeSeconds: 0.1,
	})
	raw := readBody(t, tl)
	if tl.StatusCode != http.StatusCreated {
		t.Fatalf("a crossfade the window carries = %d, want 201: %s", tl.StatusCode, raw)
	}
	var v client.TimelineResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	end0 := v.Boundaries[0].OffsetSamples + v.Boundaries[0].DurationSamples
	if v.Boundaries[1].OffsetSamples >= end0 {
		t.Errorf("member 1 starts at %d and member 0's window ends at %d; the crossfade did not overlap them",
			v.Boundaries[1].OffsetSamples, end0)
	}
}

// TestClientCreateTimelineWindows pins that the client's TimelineSrc windows
// reach the wire and the response reflects them, the TestClientCreateTimelineCrossfade
// pattern: without this the fields would be dead weight on the client type.
func TestClientCreateTimelineWindows(t *testing.T) {
	env, refs := timelineEnv(t)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	tl, jobID, err := c.CreateTimeline(t.Context(), client.TimelineRequest{
		Srcs: []client.TimelineSrc{{Src: refs[0], From: 4800, To: 28800}, {Src: refs[1]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "" || tl == nil {
		t.Fatalf("CreateTimeline returned job %q / tl %v, want an inline digest", jobID, tl)
	}
	if got := tl.Boundaries[0].DurationSamples; got != 24000 {
		t.Errorf("boundaries[0].durationSamples = %d, want the window's 24000; "+
			"the client's window did not reach the wire", got)
	}
}

// TestTimelineStoredEmptyWindowRefusedAtResolve pins resolveMember's own
// empty-window refusal, the resolve-side twin of the mint's. The mint refuses
// an empty window against the length it measured, but that refusal does not
// survive the one thing the store does and the measure does not: a binary
// upgrade. The identity pins bytes, not header interpretation, and measured
// lengths are deliberately re-derived per revision, so a window minted valid
// (from == the old measure's total) can meet a shorter measure as exactly
// from == total, which SpanTrack permits and which yields a zero-sample
// member nothing downstream pins.
//
// The fixture stands in for that stored past: a correctly content-addressed
// document written into the store before boot, because every honest API
// refuses to mint it against today's measure and a hand-tampered one dies at
// load's digest re-check (TestLoadRejectsTamperedWindow). The library file's
// mtime is pinned so the stored identity matches the resolved one, exactly
// as it would across a real upgrade.
func TestTimelineStoredEmptyWindowRefusedAtResolve(t *testing.T) {
	tlDir := filepath.Join(t.TempDir(), "timelines")
	if err := os.MkdirAll(tlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const frames = 48000
	wav := rampWAV(t, 48000, 2, frames)
	mtime := time.Unix(1700000000, 123456789)
	members := []timeline.Member{
		{Src: "lib/short.wav", ID: fmt.Sprintf("%d-%d", len(wav), mtime.UnixNano()), From: frames},
		{Src: "lib/short.wav", ID: fmt.Sprintf("%d-%d", len(wav), mtime.UnixNano())},
	}
	doc, err := json.Marshal(struct {
		SchemaVersion int               `json:"schemaVersion"`
		Expires       time.Time         `json:"expires"`
		Members       []timeline.Member `json:"members"`
	}{1, time.Now().Add(time.Hour), members})
	if err != nil {
		t.Fatal(err)
	}
	digest := timeline.Digest(members)
	if err := os.WriteFile(filepath.Join(tlDir, digest+".json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t, func(cfg *server.Config) { cfg.TimelineDir = tlDir })
	path := filepath.Join(env.root, "short.wav")
	if err := os.WriteFile(path, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8",
		Params: map[string]string{"tl": digest, "format": "opus", "gain": "off"}})
	switch {
	case err == nil:
		t.Fatal("a stored timeline whose window is empty on today's measure planned anyway; " +
			"the zero-sample member reached HLS planning unrefused")
	case waxerr.CodeOf(err) != waxerr.CodeInvalidRequest:
		t.Fatalf("refusal was %v, want an invalid-request", waxerr.CodeOf(err))
	case !strings.Contains(err.Error(), "member 0: window is empty on this source"):
		t.Fatalf("the refusal does not name the member and the emptiness: %v", err)
	}
}

// TestTimelineWindowedMixedRate pins the envelope arithmetic for a windowed
// member that resamples: the window is source samples, its normalized length
// is the resampler's own output count, and the segments serve. The first
// samples of such a member prime cold (concat consults no Headroomer), the
// same status quo as crossfade-seek exactness; the motivating CUE carve is
// same-file, hence uniform, hence exact, so the transient is bounded and
// documented rather than fixed here.
func TestTimelineWindowedMixedRate(t *testing.T) {
	env, _ := timelineEnv(t)
	if err := os.WriteFile(filepath.Join(env.root, "tl-44.wav"), rampWAV(t, 44100, 2, 44100), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := mintTimelineSrcs(t, env, []client.TimelineSrc{
		{Src: "lib/tl-44.wav", To: 22050}, // 0.5 s at 44.1k: 24000 envelope samples
		{Src: "lib/tl-0.wav"},             // 48000 samples at 48k
	})
	if tl.EnvelopeRate != 48000 {
		t.Fatalf("envelopeRate = %d, want 48000", tl.EnvelopeRate)
	}
	if got := tl.Boundaries[0].DurationSamples; got != 24000 {
		t.Fatalf("the 22050-sample window at 44.1k normalizes to %d envelope samples, want 24000", got)
	}
	if got := tl.Boundaries[1].OffsetSamples; got != 24000 {
		t.Fatalf("member 1 starts at %d, want 24000 (the window's resampled length)", got)
	}
	if want := 72000.0 / 48000.0; tl.DurationSeconds != want {
		t.Fatalf("durationSeconds = %v, want %v", tl.DurationSeconds, want)
	}
	// The render must actually serve: the windowed member opens mid-worker
	// through Slice, resampling to the envelope.
	_, _, segs := fetchVariant(t, env, mintHLS(t, env,
		map[string]string{"tl": tl.Tl, "format": "opus", "gain": "off"}))
	if len(segs) == 0 {
		t.Fatal("the mixed-rate windowed timeline served no segments")
	}
}
