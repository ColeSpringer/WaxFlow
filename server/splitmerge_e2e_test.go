// The merge and split acceptance suite: cutting one source into N products
// and concatenating N sources into one, over the wire, plus the /result
// indexing the pair forced onto the jobs API.
package server_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// rampFrames is lib/ramp.wav's length: 4 s of stereo s16 at 48 kHz. It is the
// source every case here cuts, because its every sample differs from its
// neighbours, so a piece delivered from the wrong offset is a mismatch rather
// than a coincidence.
const rampFrames = 4 * 48000

// TestSplitJobRoundTrip is the pair's headline claim, checked the only way
// that means anything: cut a source at points on no convenient boundary, then
// decode every piece and lay them end to end. The result must be the source,
// bit for bit.
//
// FLAC at the source rate is what makes "bit for bit" the right assertion and
// not an approximation. The chain has no resampler and no limiter, so nothing
// primes and each piece's sample 0 is the source's sample from exactly; a
// transient at any piece's head, a sample lost or repeated at any cut, or an
// off-by-one in the span arithmetic all land here as a mismatch.
//
// It is the wire end of the engine's own TestSliceSplitRoundTrip. That one
// proves Slice and Concat are inverses; this one proves the job wires them up
// to the cut points the caller actually sent.
func TestSplitJobRoundTrip(t *testing.T) {
	env := jobsEnv(t)

	// Cut points on no convenient boundary: not chunk-aligned, not aligned to
	// anything the encoder or the demuxer would land on by itself.
	cuts := []int64{4097, 45197, 118_000, 176_543}
	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/ramp.wav", "format": "flac", "cuts": cuts,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := createJob(t, env, string(body))
	job := awaitJob(t, env, id)

	if len(job.Outputs) != len(cuts)+1 {
		t.Fatalf("split at %d cuts made %d outputs, want %d: %+v",
			len(cuts), len(job.Outputs), len(cuts)+1, job.Outputs)
	}
	// The declared lengths first: N cuts make N+1 pieces, the first from the
	// implied 0 and the last to the implied end.
	wantLens := []int64{4097, 45197 - 4097, 118_000 - 45197, 176_543 - 118_000, rampFrames - 176_543}
	for i, out := range job.Outputs {
		if out.Samples != wantLens[i] {
			t.Errorf("piece %d declares %d samples, want %d", i, out.Samples, wantLens[i])
		}
		if out.Container != "flac" {
			t.Errorf("piece %d container = %q, want flac", i, out.Container)
		}
		if out.Rate != 48000 {
			t.Errorf("piece %d rate = %d, want the source's 48000", i, out.Rate)
		}
	}

	// And now the samples themselves, which is the assertion that matters.
	whole := decodePCM(t, readBody(t, env.get(t, "/stream?src=lib/ramp.wav&format=flac", nil)))
	rejoined := audio.Get(whole.Fmt, whole.N)
	defer audio.Put(rejoined)
	rejoined.N = 0
	for i := range job.Outputs {
		resp := env.get(t, fmt.Sprintf("/jobs/%s/result/%d", id, i), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("piece %d: status = %d, want 200: %s", i, resp.StatusCode, readBody(t, resp))
		}
		piece := decodePCM(t, readBody(t, resp))
		if int64(piece.N) != wantLens[i] {
			t.Fatalf("piece %d decoded to %d frames, want %d", i, piece.N, wantLens[i])
		}
		if rejoined.N+piece.N > rejoined.Cap() {
			t.Fatalf("the pieces hold more than the source's %d frames", whole.N)
		}
		audio.CopyFrames(rejoined, rejoined.N, piece, 0, piece.N)
		rejoined.N += piece.N
	}
	if rejoined.N != whole.N {
		t.Fatalf("the pieces rejoin to %d frames, want the source's %d", rejoined.N, whole.N)
	}
	for c := range whole.Fmt.Channels {
		w, g := whole.ChanI(c), rejoined.ChanI(c)
		for i := range w {
			if w[i] != g[i] {
				t.Fatalf("rejoined ch%d[%d] = %d, want %d; the split is not sample-exact", c, i, g[i], w[i])
			}
		}
	}
}

// TestMergeJobLengthIsTheSum drives the other direction: N members in, one
// product out, and its length is the members' lengths added up. A seam that
// dropped or duplicated a sample fails here on arithmetic alone.
func TestMergeJobLengthIsTheSum(t *testing.T) {
	env := jobsEnv(t)

	lens := []int{48000, 24000, 72000} // deliberately unequal: no passing by symmetry
	refs := make([]string, len(lens))
	var want int64
	for i, n := range lens {
		name := fmt.Sprintf("merge-%d.wav", i)
		if err := os.WriteFile(filepath.Join(env.root, name), rampWAV(t, 48000, 2, n), 0o644); err != nil {
			t.Fatal(err)
		}
		refs[i] = "lib/" + name
		want += int64(n)
	}
	body, err := json.Marshal(map[string]any{"type": "merge", "srcs": refs, "format": "flac"})
	if err != nil {
		t.Fatal(err)
	}
	id := createJob(t, env, string(body))
	job := awaitJob(t, env, id)

	if len(job.Outputs) != 1 {
		t.Fatalf("merge made %d outputs, want 1: %+v", len(job.Outputs), job.Outputs)
	}
	out := job.Outputs[0]
	if out.Samples != want {
		t.Errorf("merged length = %d samples, want the sum %d", out.Samples, want)
	}
	// The header's claim is not the file's: decode it back, or a muxer writing
	// a length it does not hold would pass.
	resp := env.get(t, "/jobs/"+id+"/result", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("result: status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	merged := decodePCM(t, readBody(t, resp))
	if int64(merged.N) != want {
		t.Errorf("the merged file decodes to %d frames, want %d", merged.N, want)
	}
}

// TestMergeSelectsProgressiveMP4 pins the M4B decision: an mp4-family merge
// written to a file takes the flat form, which is what Apple Books reads and
// the only shape that can carry a chapter text track, without the caller
// naming it. The row's default is the fragmented muxer because /stream needs
// one that streams, and a job is not /stream.
//
// The presence of a moof box is the discriminator, and it is the honest one:
// flat and fragmented are the same media type and both carry a moov, so the
// question "is this the streaming shape" is exactly "are there fragments".
func TestMergeSelectsProgressiveMP4(t *testing.T) {
	env := jobsEnv(t)

	t.Run("the default is flat", func(t *testing.T) {
		id := createJob(t, env, `{"type":"merge","srcs":["lib/ramp.wav"],"format":"alac"}`)
		job := awaitJob(t, env, id)
		// The stored request carries it, so the job document says what will be
		// produced rather than the runner quietly meaning something else.
		if job.Request.Container != "progressive" {
			t.Errorf("request container = %q, want progressive", job.Request.Container)
		}
		if len(job.Outputs) != 1 || job.Outputs[0].Container != "progressive" {
			t.Fatalf("outputs = %+v, want one progressive", job.Outputs)
		}
		if raw := readBody(t, env.get(t, "/jobs/"+id+"/result", nil)); hasBox(t, raw, "moof") {
			t.Error("the merged MP4 carries fragments; it is not the flat form")
		}
	})

	// The contrast, and the reason the case above proves anything: a transcode
	// job of the same format keeps the row's fragmented default, so the flat
	// form is a decision merge makes and not just what this daemon always does.
	t.Run("a transcode job keeps the fragmented default", func(t *testing.T) {
		id := createJob(t, env, `{"type":"transcode","src":"lib/ramp.wav","format":"alac"}`)
		job := awaitJob(t, env, id)
		if job.Request.Container != "" {
			t.Errorf("request container = %q, want the row default", job.Request.Container)
		}
		if raw := readBody(t, env.get(t, "/jobs/"+id+"/result", nil)); !hasBox(t, raw, "moof") {
			t.Error("the transcoded MP4 has no fragments, so the merge case above proves nothing")
		}
	})

	// An explicit container still wins: the default is a default. It is spelled
	// on aac because that is the mp4-family row with a second container to name
	// (alac has only the flat form to override to).
	t.Run("an explicit container wins", func(t *testing.T) {
		id := createJob(t, env, `{"type":"merge","srcs":["lib/ramp.wav"],"format":"aac","container":"adts","bitrate":96}`)
		job := awaitJob(t, env, id)
		if job.Request.Container != "adts" {
			t.Errorf("request container = %q, want the caller's adts", job.Request.Container)
		}
		if len(job.Outputs) != 1 || job.Outputs[0].Container != "adts" {
			t.Fatalf("outputs = %+v, want one adts", job.Outputs)
		}
	})
}

// TestMergeJobStampsChapters is A18 over the wire: an mp4-family merge with a
// titles field writes a QuickTime chapter text track, one chapter per member at
// its boundary, titled by the request. Members are whole-second so the offsets
// land on whole chapter ticks and round-trip through the muxer exactly.
//
// It reads the chapters off the downloaded file, not off the job, because the
// point is the bytes a client gets: a titles field the runner accepted but the
// muxer dropped would pass a job-shape check and fail here.
func TestMergeJobStampsChapters(t *testing.T) {
	env := jobsEnv(t)
	lens := []int{48000, 96000} // 1 s, 2 s at 48 kHz
	refs := make([]string, len(lens))
	for i, n := range lens {
		name := fmt.Sprintf("chap-%d.wav", i)
		if err := os.WriteFile(filepath.Join(env.root, name), rampWAV(t, 48000, 2, n), 0o644); err != nil {
			t.Fatal(err)
		}
		refs[i] = "lib/" + name
	}
	body, err := json.Marshal(map[string]any{
		"type": "merge", "srcs": refs, "format": "alac", "titles": []string{"Opening", "Closing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := createJob(t, env, string(body))
	job := awaitJob(t, env, id)
	if len(job.Outputs) != 1 {
		t.Fatalf("merge made %d outputs, want 1", len(job.Outputs))
	}
	raw := readBody(t, env.get(t, "/jobs/"+id+"/result", nil))
	info, err := format.Probe(bytes.NewReader(raw), job.Outputs[0].Container, nil)
	if err != nil {
		t.Fatalf("probing the merged output: %v", err)
	}
	if len(info.Chapters) != len(refs) {
		t.Fatalf("merged output carries %d chapters, want one per member (%d)", len(info.Chapters), len(refs))
	}
	wantTitles := []string{"Opening", "Closing"}
	wantStarts := []time.Duration{0, time.Second}
	for i, ch := range info.Chapters {
		if ch.Title != wantTitles[i] {
			t.Errorf("chapter %d title = %q, want the request's %q", i, ch.Title, wantTitles[i])
		}
		if ch.Start != wantStarts[i] {
			t.Errorf("chapter %d starts at %v, want %v (the member's boundary)", i, ch.Start, wantStarts[i])
		}
	}
}

// TestMergeRejectsMismatchedTitles pins the titles alignment rule at creation:
// a title per member, or none. A count between the two would leave which member
// each title belongs to a guess at run time, so it is a 400 rather than a job
// that stamps the wrong labels.
func TestMergeRejectsMismatchedTitles(t *testing.T) {
	env := jobsEnv(t)
	resp := env.postJSON(t, "/jobs",
		`{"type":"merge","srcs":["lib/sine.wav","lib/sine.wav"],"format":"alac","titles":["only one"]}`)
	wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
}

// TestMergeRejectsTitlesWithoutAChapterTrack pins that titles are refused at
// creation when the merge's output cannot carry a chapter track, rather than
// accepted and silently dropped. A field the request carries but the output
// cannot honor is a 400 where the caller can still fix it, the way a lossless
// bitrate is. It covers both a non-mp4 format and an mp4 format forced into a
// non-mp4 container, since either would write no chapters.
func TestMergeRejectsTitlesWithoutAChapterTrack(t *testing.T) {
	env := jobsEnv(t)
	for _, tc := range []struct{ name, body string }{
		{"a non-mp4 format",
			`{"type":"merge","srcs":["lib/sine.wav","lib/sine.wav"],"format":"flac","titles":["A","B"]}`},
		{"an mp4 format in a non-mp4 container",
			`{"type":"merge","srcs":["lib/sine.wav","lib/sine.wav"],"format":"aac","container":"adts","bitrate":96,"titles":["A","B"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.postJSON(t, "/jobs", tc.body)
			wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
		})
	}
}

// hasBox reports whether raw's top-level box list holds a box of this type. It
// walks the list rather than searching the bytes, since a four-byte name turns
// up inside sample data readily enough to matter.
func hasBox(t *testing.T, raw []byte, want string) bool {
	t.Helper()
	for off := 0; off+8 <= len(raw); {
		size := int64(binary.BigEndian.Uint32(raw[off:]))
		if string(raw[off+4:off+8]) == want {
			return true
		}
		switch size {
		case 1:
			// 64-bit largesize, which is what the flat form's mdat uses: its
			// size is back-patched after the encode, so the muxer reserves the
			// wide field up front rather than betting on the audio being small.
			if off+16 > len(raw) {
				return false
			}
			size = int64(binary.BigEndian.Uint64(raw[off+8:]))
		case 0:
			return false // extends to EOF: nothing follows to find
		}
		if size < 8 {
			t.Fatalf("box %q at offset %d declares a %d-byte size", raw[off+4:off+8], off, size)
		}
		off += int(size)
	}
	return false
}

// TestJobResultIndexing pins the endpoint's shape once a job can have several
// products: the index bounds, and the bare /result's rule that it answers only
// where it cannot be wrong.
func TestJobResultIndexing(t *testing.T) {
	env := jobsEnv(t)

	splitID := createJob(t, env, `{"type":"split","src":"lib/ramp.wav","format":"flac","cuts":[1000,2000]}`)
	awaitJob(t, env, splitID)
	oneID := createJob(t, env, `{"type":"transcode","src":"lib/ramp.wav","format":"flac"}`)
	awaitJob(t, env, oneID)

	t.Run("the bare path refuses a multi-output job", func(t *testing.T) {
		// Refused rather than answered with piece 0: a caller who never learned
		// the pieces existed would read a plausible-looking wrong answer, and
		// this daemon refuses ambiguity everywhere else it meets it.
		resp := env.get(t, "/jobs/"+splitID+"/result", nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bare /result on a 3-output job = %d, want 400: %s", resp.StatusCode, body)
		}
		// The message has to say what to do instead, or the refusal is a wall.
		if !strings.Contains(string(body), "3 outputs") || !strings.Contains(string(body), "/result/") {
			t.Errorf("the 400 does not tell the caller to index: %s", body)
		}
	})
	t.Run("the bare path answers a single-output job", func(t *testing.T) {
		resp := env.get(t, "/jobs/"+oneID+"/result", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bare /result on a 1-output job = %d, want 200", resp.StatusCode)
		}
	})
	t.Run("index 0 is the same file", func(t *testing.T) {
		bare := readBody(t, env.get(t, "/jobs/"+oneID+"/result", nil))
		idx := readBody(t, env.get(t, "/jobs/"+oneID+"/result/0", nil))
		if len(bare) != len(idx) || len(bare) == 0 {
			t.Fatalf("bare /result is %d bytes and /result/0 is %d", len(bare), len(idx))
		}
	})
	t.Run("the pieces have distinct ETags", func(t *testing.T) {
		// The id alone was a strong validator only while a job had one output
		// to be: unindexed, a split's pieces would all validate as each other.
		seen := map[string]bool{}
		for i := range 3 {
			resp := env.get(t, fmt.Sprintf("/jobs/%s/result/%d", splitID, i), nil)
			etag := resp.Header.Get("ETag")
			resp.Body.Close()
			if etag == "" || seen[etag] {
				t.Fatalf("piece %d ETag %q collides with an earlier piece's", i, etag)
			}
			seen[etag] = true
		}
	})
	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"past the end", "/result/3", http.StatusNotFound},
		{"negative", "/result/-1", http.StatusBadRequest},
		{"not a number", "/result/first", http.StatusBadRequest},
	} {
		t.Run("index "+tc.name, func(t *testing.T) {
			resp := env.get(t, "/jobs/"+splitID+tc.path, nil)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s = %d, want %d: %s", tc.path, resp.StatusCode, tc.want, readBody(t, resp))
			}
		})
	}
}

// TestSignedIndexedResult drives the signing surface's half of the indexed
// path: a browser download link cannot set headers, which is the whole reason
// /result is signable, and a split's pieces are exactly the case where a
// caller has several links to hand out.
func TestSignedIndexedResult(t *testing.T) {
	env := jobsEnv(t)
	id := createJob(t, env, `{"type":"split","src":"lib/ramp.wav","format":"flac","cuts":[1000]}`)
	awaitJob(t, env, id)

	body := fmt.Sprintf(`{"path":"/jobs/%s/result/1","ttlSeconds":3600}`, id)
	var signed server.SignResponse
	resp := env.postJSON(t, "/sign", body)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signing an indexed result = %d, want 200: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	// Keyless, which is what proves the signature is doing the work.
	got := env.get(t, signed.URL, map[string]string{"X-API-Key": ""})
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("the signed URL = %d, want 200: %s", got.StatusCode, readBody(t, got))
	}
}

// TestTimelineJobResultIsItsDigest covers the branch a timeline job used to
// land in: /result answered CodeInternal "job has no output" for a job that
// had succeeded, because Timeline is deliberately not an Output (it is not a
// file in the job directory; it lives in the timeline store under the digest
// that is its identity). That read as a broken endpoint rather than as the
// decision it was. /result means the job's product, and an analyze job has
// served its numbers there all along.
func TestTimelineJobResultIsItsDigest(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
		cfg.TimelineDir = filepath.Join(t.TempDir(), "timelines")
	})
	// An MP3 is the queue that becomes a job: its length is not authoritative
	// from its headers and its demuxer indexes, which is the daemon's own test
	// for measuring being expensive enough to be worth a job.
	raw, err := os.ReadFile("../testdata/noise-cbr320.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "noise.mp3"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	resp := env.postJSON(t, "/hls/timeline", `{"srcs":[{"src":"lib/noise.mp3"},{"src":"lib/noise.mp3"}]}`)
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Skipf("the cold MP3 queue minted inline (%d), so there is no timeline job to check: %s",
			resp.StatusCode, b)
	}
	var created jobs.Job
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, created.ID)
	if job.Timeline == nil || job.Timeline.Tl == "" {
		t.Fatalf("the finished timeline job carries no digest: %+v", job)
	}
	if len(job.Outputs) != 0 {
		t.Fatalf("a timeline job grew an output; its product is the digest: %+v", job.Outputs)
	}

	result := env.get(t, "/jobs/"+created.ID+"/result", nil)
	body := readBody(t, result)
	if result.StatusCode != http.StatusOK {
		t.Fatalf("a done timeline job's /result = %d, want 200: %s", result.StatusCode, body)
	}
	var tl jobs.Timeline
	if err := json.Unmarshal(body, &tl); err != nil {
		t.Fatalf("the result is not a timeline: %v\n%s", err, body)
	}
	if tl.Tl != job.Timeline.Tl {
		t.Errorf("result digest = %q, want the job's %q", tl.Tl, job.Timeline.Tl)
	}
	if tl.Members != 2 || tl.DurationSeconds <= 0 {
		t.Errorf("result timeline = %+v, want 2 members and a real duration", tl)
	}
}
