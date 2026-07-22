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
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/dsp/silence"
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
// than at the first segment request minutes into playback.
func TestTimelineMintRejects(t *testing.T) {
	env, _ := timelineEnv(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"an empty queue", `{"srcs":[]}`},
		{"a member with no src", `{"srcs":[{"src":""}]}`},
		{"a member that does not resolve", `{"srcs":[{"src":"lib/nope.wav"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.postJSON(t, "/hls/timeline", tc.body)
			if resp.StatusCode < 400 {
				t.Fatalf("POST /hls/timeline = %d, want a refusal: %s",
					resp.StatusCode, readBody(t, resp))
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
}
