package jobs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/admission"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/internal/ulid"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

const fixtureWAV = "sine-s16.wav"

// copyFixture copies a repo testdata file into dir under the same name.
func copyFixture(t *testing.T, dir, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// openRoots opens a single-root resolver named lib over dir.
func openRoots(t *testing.T, dir string) *source.Roots {
	t.Helper()
	res, err := source.OpenRoots([]source.Root{{Name: "lib", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Close() })
	return res
}

// openLib builds a library root holding the small sine fixture and pins
// its source identity, the setup every job request needs.
func openLib(t *testing.T) (res *source.Roots, ref, srcID string) {
	t.Helper()
	root := t.TempDir()
	copyFixture(t, root, fixtureWAV)
	res = openRoots(t, root)
	ref = "lib/" + fixtureWAV
	return res, ref, pinID(t, res, ref)
}

// pinID resolves ref once and returns its identity in the canonical
// size-mtimeNS form, the value a creating server would pin.
func pinID(t *testing.T, res source.Resolver, ref string) string {
	t.Helper()
	src, err := res.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	return src.ID.String()
}

// openRunner opens a runner with test defaults filled in. Close is
// registered as a cleanup; calling it again earlier is harmless.
func openRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	if cfg.Engine == nil {
		cfg.Engine = waxflow.New()
	}
	if cfg.Slots == 0 {
		cfg.Slots = 1
	}
	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func transcodeReq(ref, srcID string) Request {
	return Request{Type: TypeTranscode, Src: ref, SourceID: srcID, Format: "flac", FLACLevel: -1}
}

func analyzeReq(ref, srcID string) Request {
	return Request{Type: TypeAnalyze, Src: ref, SourceID: srcID}
}

// soleOutput returns the single product of a job that has exactly one. Most
// job types do, and asserting it here rather than indexing at each call site
// is what keeps a test that meant "the output" from silently reading the first
// of several.
func soleOutput(t *testing.T, j *Job) *Output {
	t.Helper()
	if len(j.Outputs) != 1 {
		t.Fatalf("job has %d outputs, want exactly 1", len(j.Outputs))
	}
	return &j.Outputs[0]
}

// waitJob polls until the job reaches want, failing fast when it lands
// on a different terminal state instead of timing out on it.
func waitJob(t *testing.T, r *Runner, id string, want State) *Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, ok := r.Get(id)
		if !ok {
			t.Fatalf("job %s disappeared while waiting for %s", id, want)
		}
		if j.State == want {
			return j
		}
		if j.State.Terminal() {
			t.Fatalf("job %s reached %s (error %+v), want %s", id, j.State, j.Error, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for job %s to reach %s (still %s)", id, want, j.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor polls cond with a deadline; what names the thing that never
// happened.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recv reads one subscription event with a deadline.
func recv(t *testing.T, ch <-chan *Job) (*Job, bool) {
	t.Helper()
	select {
	case j, ok := <-ch:
		return j, ok
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a subscription event")
		return nil, false
	}
}

// saturatedPool returns a live pool held full plus its release. Running
// jobs block inside their per-chunk progress hook while the pool is
// saturated, so a held pool is the deterministic stand-in for a long
// encode: the job reaches running and stays there until release.
func saturatedPool(t *testing.T) (*admission.Pools, func()) {
	t.Helper()
	p := admission.New(1)
	release, ok := p.AcquireLive()
	if !ok {
		t.Fatal("could not saturate the live pool")
	}
	t.Cleanup(release)
	return p, release
}

// writeLongWAV renders several seconds of sine into dir as long.wav. A
// job over it spans many decode chunks, so progress broadcasts fire and
// mid-run cancellation has room to land.
func writeLongWAV(t *testing.T, dir string) (ref string) {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	const frames = 5 * 44100
	buf := testutil.Sine(f, frames, 997, 0.5)
	defer audio.Put(buf)
	enc, err := pcm.NewEncoder(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, f)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := riff.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "long.wav"), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/long.wav"
}

// writeSilenceWAV renders a 44.1k stereo WAV alternating a -6 dBFS tone and
// true digital zero at hard cuts, returning the ref and the spans it
// contains by construction.
func writeSilenceWAV(t *testing.T, dir string) (ref string, want []waxflow.SilenceSpan) {
	t.Helper()
	const rate = 44100
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	// 1 s tone, 1 s silence, 1 s tone, 2 s silence, 1 s tone.
	regions := []struct {
		silent bool
		frames int
	}{{false, rate}, {true, rate}, {false, rate}, {true, 2 * rate}, {false, rate}}

	frames := 0
	for _, r := range regions {
		frames += r.frames
	}
	buf := audio.Get(f, frames)
	defer audio.Put(buf)
	buf.N = frames
	at := 0
	for _, r := range regions {
		if !r.silent {
			for i := range r.frames {
				v := int32(0.5 * 32767 * math.Sin(2*math.Pi*997*float64(at+i)/rate))
				for c := range f.Channels {
					buf.I[c*buf.Stride+at+i] = v
				}
			}
		} else {
			want = append(want, waxflow.SilenceSpan{From: int64(at), To: int64(at + r.frames)})
		}
		at += r.frames
	}

	enc, err := pcm.NewEncoder(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, f)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := riff.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "silence.wav"), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/silence.wav", want
}

// patchFLACTotalSamples rewrites STREAMINFO's 36-bit total_samples field in
// place, leaving the audio frames alone: the file goes on holding every sample
// it held, and only its header now says otherwise.
//
// STREAMINFO is the first metadata block, so the packed
// rate|channels|bits|total word sits at a fixed offset: the "fLaC" magic, a
// 4-byte block header, then 10 bytes of block and frame sizes.
func patchFLACTotalSamples(t *testing.T, path string, total int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 26 || string(raw[:4]) != "fLaC" {
		t.Fatalf("%s is not a FLAC stream", path)
	}
	const off = 4 + 4 + 10
	const mask = uint64(1)<<36 - 1
	w := binary.BigEndian.Uint64(raw[off:])
	binary.BigEndian.PutUint64(raw[off:], w&^mask|uint64(total)&mask)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFLACDeclaring renders a FLAC into dir under name whose STREAMINFO
// declares the given total, and reports the ref plus what the stream really
// holds. declared is written verbatim, so 0 spells FLAC's own "unknown" and
// any value short of the truth spells a header that under-declares.
//
// Both shapes are real rather than contrived. FLAC never marks its length
// exact, because a STREAMINFO total can lie and trusting it as a hard length
// would truncate an otherwise good file, so the decoder reads every frame that
// is actually there whatever the header claims. That is what makes the two
// cases differ in kind: an absent length is a question nothing has answered,
// while a wrong one is an answer the file is entitled to be held to.
func writeFLACDeclaring(t *testing.T, dir, name string, declared int64) (ref string, real int64) {
	t.Helper()
	// A genuine FLAC first: patching a header is only honest over a stream
	// that really holds the samples the patch denies.
	wavDir := t.TempDir()
	writeLongWAV(t, wavDir)
	wav, err := os.ReadFile(filepath.Join(wavDir, "long.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "wav", f,
		waxflow.TranscodeOptions{Format: "flac", FLACLevel: -1})
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	patchFLACTotalSamples(t, path, declared)
	return "lib/" + name, res.Samples
}

// measureTrack is the daemon's MeasureTrack hook in miniature: the declared
// length when the header marks it exact, a decode to the true end when it does
// not. The daemon's own (server.trackFor) memoizes the answer per source
// identity; the walk is the half that matters here.
func measureTrack() func(*source.File) (container.Track, error) {
	eng := waxflow.New()
	return func(src *source.File) (container.Track, error) {
		info, err := eng.Probe(src, src.Ext, nil)
		if err != nil {
			return container.Track{}, err
		}
		track := info.Default()
		if track.Samples < 0 || !track.SamplesExact {
			res, err := eng.Analyze(context.Background(), src, src.Ext, waxflow.AnalyzeOptions{})
			if err != nil {
				return container.Track{}, err
			}
			track.Samples, track.SamplesExact = res.Samples, true
		}
		return track, nil
	}
}

// TestSplitFillsAnAbsentLength covers the source a split has no number for at
// all: an ADTS stream, or this FLAC whose STREAMINFO total is 0, which is how
// FLAC spells "unknown".
//
// Nothing bounds such a split unless it measures. waxflow.SpanTrack bounds an
// explicit span end against the declared length and a track that declares
// nothing has none, so every cut is accepted however far past the end it sits,
// and the job dies on the piece that cannot exist with the pieces before it
// already written. Measuring is what turns that into a refusal.
func TestSplitFillsAnAbsentLength(t *testing.T) {
	// The fixture declares nothing and holds real samples; a cut past real is
	// the one nothing but a measurement can catch.
	const cut = int64(50_000)

	t.Run("it splits, and reports a real percent rather than an unknown one", func(t *testing.T) {
		root := t.TempDir()
		ref, real := writeFLACDeclaring(t, root, "undeclared.flac", 0)
		res := openRoots(t, root)
		pools, release := saturatedPool(t)
		r := openRunner(t, Config{
			Dir: t.TempDir(), Resolver: res, Pools: pools, MeasureTrack: measureTrack(),
		})

		j, err := r.Create(Request{
			Type: TypeSplit, Src: ref, SourceID: pinID(t, res, ref),
			Format: "flac", FLACLevel: -1, Cuts: []int64{cut},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		waitJob(t, r, j.ID, StateRunning)
		ch, cancelSub, ok := r.Subscribe(j.ID)
		if !ok {
			t.Fatal("Subscribe: job not found")
		}
		defer cancelSub()
		if snap, open := recv(t, ch); !open || snap.State != StateRunning {
			t.Fatalf("initial snapshot = %+v, want a running job", snap)
		}

		release()
		var seen int
		for {
			s, open := recv(t, ch)
			if !open {
				break
			}
			if s.State != StateRunning || s.Progress == nil {
				continue
			}
			seen++
			// The filled length is what the bar is drawn against: unmeasured,
			// this job knows no total and can only report an unknown percent
			// for its whole duration.
			if s.Progress.Total != real {
				t.Fatalf("progress total = %d, want the measured %d", s.Progress.Total, real)
			}
			if s.Progress.Percent < 0 {
				t.Fatalf("progress percent = %v, want a real one: the length was measured",
					s.Progress.Percent)
			}
		}
		if seen == 0 {
			t.Fatal("no running snapshot carried progress")
		}

		done := waitJob(t, r, j.ID, StateDone)
		if len(done.Outputs) != 2 {
			t.Fatalf("split produced %d outputs, want 2", len(done.Outputs))
		}
		if got, want := done.Outputs[1].Samples, real-cut; got != want {
			t.Errorf("last piece = %d samples, want %d", got, want)
		}
	})

	t.Run("a cut past the real end is refused before any piece is written", func(t *testing.T) {
		root := t.TempDir()
		ref, real := writeFLACDeclaring(t, root, "undeclared.flac", 0)
		past := real + 1000
		res := openRoots(t, root)
		dir := t.TempDir()
		r := openRunner(t, Config{Dir: dir, Resolver: res, MeasureTrack: measureTrack()})

		j, err := r.Create(Request{
			Type: TypeSplit, Src: ref, SourceID: pinID(t, res, ref),
			Format: "flac", FLACLevel: -1, Cuts: []int64{cut, past},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		failed := waitJob(t, r, j.ID, StateFailed)
		// Not fatal: the state of the directory below is the harm, and it is
		// worth reporting even when the job failed for the wrong reason.
		// Unmeasured this fails as source-unreadable, deep in the encode
		// ("the source ended 170500 samples into a span that declared
		// 171500"), which is the shape of a bad cut list discovered far too
		// late rather than refused.
		if failed.Error == nil || failed.Error.Code != string(waxerr.CodeInvalidRequest) {
			t.Errorf("error = %+v, want %s: an impossible cut is a bad request, not a broken source",
				failed.Error, waxerr.CodeInvalidRequest)
		}

		// The refusal lands at the funnel, before the loop, so a cut list that
		// cannot be cut costs no pieces. Unmeasured, the cut past the end is
		// accepted, the pieces before it are written, and the piece that runs
		// off the end of the audio fails the job with them already on disk: a
		// failed job is terminal, so nothing ever sweeps them.
		entries, err := os.ReadDir(filepath.Join(dir, j.ID))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != jobFile {
				t.Errorf("the refused split left %q in the job directory; the pieces before the "+
					"impossible one were written before anything noticed", e.Name())
			}
		}
	})
}

// TestSplitRefusesAnUnderDeclaredCutConsistently pins the other half of the
// rule: a declared length is not overridden, however wrong it is.
//
// The source really holds more than it says, and those samples stay
// unaddressable, which is what a lying header buys. The point is that every
// layer says so alike: the server refuses this cut at creation against the
// declared length, this refuses it at run against the same number, and
// waxflow.SpanTrack would refuse the span for the same reason a layer further
// down. A measurement here would overrule only the first two, and the caller
// would trade a 400 for a 201 that fails at run.
func TestSplitRefusesAnUnderDeclaredCutConsistently(t *testing.T) {
	const declared, cut = int64(100_000), int64(150_000)
	root := t.TempDir()
	ref, real := writeFLACDeclaring(t, root, "under.flac", declared)
	// The cut has to fall in the gap between the lie and the truth, or this
	// pins nothing: past the declared end, but over audio that is really there.
	if cut <= declared || cut >= real {
		t.Fatalf("cut %d must fall between the declared %d and the real %d", cut, declared, real)
	}
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, MeasureTrack: measureTrack()})

	j, err := r.Create(Request{
		Type: TypeSplit, Src: ref, SourceID: pinID(t, res, ref),
		Format: "flac", FLACLevel: -1, Cuts: []int64{cut},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	failed := waitJob(t, r, j.ID, StateFailed)
	if failed.Error == nil || failed.Error.Code != string(waxerr.CodeInvalidRequest) {
		t.Fatalf("error = %+v, want %s", failed.Error, waxerr.CodeInvalidRequest)
	}
	// The message names the declared length, which is the number the API's own
	// refusal quotes: the two answers have to be the same answer.
	msg := failed.Error.Message
	if !strings.Contains(msg, strconv.FormatInt(declared, 10)) {
		t.Errorf("error %q does not name the declared %d", msg, declared)
	}
	if strings.Contains(msg, strconv.FormatInt(real, 10)) {
		t.Errorf("error %q names the measured %d: a declared length is not overridden", msg, real)
	}
	// And it is the cut funnel's own refusal, not a span error from three
	// layers down. Measuring here would widen SplitSpans past the header and
	// leave waxflow.Slice to refuse the same cut on the same number anyway:
	// the caller would still be told no, so only the layer that says it would
	// change, and SplitSpans is where every rule about a cut list lives.
	if !strings.HasPrefix(msg, "jobs: cut ") {
		t.Errorf("error %q is not the cut funnel's: SplitSpans stopped holding the cut list to "+
			"the length the run cuts against", msg)
	}
}

// TestSilenceJobEndToEnd drives the whole A12 path: an analyze job asks for
// the map, the runner writes it into the job directory as an output file
// rather than onto the job, and the job carries only the summary.
func TestSilenceJobEndToEnd(t *testing.T) {
	root := t.TempDir()
	ref, want := writeSilenceWAV(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	dir := t.TempDir()
	r := openRunner(t, Config{Dir: dir, Resolver: res})

	req := analyzeReq(ref, srcID)
	req.Silence = true
	j, err := r.Create(req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)

	// The loudness half is unaffected by asking for the map.
	if done.Analysis == nil || done.Analysis.IntegratedLUFS == nil {
		t.Fatal("silence analyze lost the loudness measurement")
	}
	s := done.Analysis.Silence
	if s == nil {
		t.Fatal("done silence analyze has no summary")
	}
	if s.Version == "" {
		t.Error("summary carries no detector version; a caller's cache cannot tell when the map went stale")
	}
	if s.Spans != len(want) {
		t.Errorf("summary spans = %d, want %d", s.Spans, len(want))
	}
	if s.ThresholdDB != waxflow.DefaultSilenceThresholdDB {
		t.Errorf("summary thresholdDb = %v, want the default %v", s.ThresholdDB, waxflow.DefaultSilenceThresholdDB)
	}
	if s.MinSeconds != waxflow.DefaultSilenceMinDuration.Seconds() {
		t.Errorf("summary minSeconds = %v, want the default %v", s.MinSeconds, waxflow.DefaultSilenceMinDuration.Seconds())
	}
	if s.TotalSeconds < 2.9 || s.TotalSeconds > 3.1 {
		t.Errorf("summary totalSeconds = %v, want ~3 (a 1 s and a 2 s silence)", s.TotalSeconds)
	}
	// The fixture's silences are clean, so almost nothing is dropped by
	// length: this is the healthy side of the DroppedSeconds diagnostic.
	if s.DroppedSeconds > 0.05 {
		t.Errorf("summary droppedSeconds = %v, want ~0 for clean digital silence", s.DroppedSeconds)
	}

	// The map is an output file, not an inline field: job.json is broadcast
	// whole on every progress event, so a 4800-span map cannot live there.
	out := soleOutput(t, done)
	if out.File != silenceMapFile {
		t.Errorf("output file = %q, want %q", out.File, silenceMapFile)
	}
	if out.MediaType != "application/json" {
		t.Errorf("output mediaType = %q, want application/json", out.MediaType)
	}
	if out.Samples != 0 || out.Rate != 0 {
		t.Errorf("output samples/rate = %d/%d, want 0/0: the map is not audio", out.Samples, out.Rate)
	}

	var doc SilenceMap
	raw, err := os.ReadFile(filepath.Join(dir, j.ID, silenceMapFile))
	if err != nil {
		t.Fatalf("reading the map: %v", err)
	}
	if int64(len(raw)) != out.Bytes {
		t.Errorf("map is %d bytes, output claims %d", len(raw), out.Bytes)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the map: %v", err)
	}
	if doc.Rate != 44100 {
		t.Errorf("map rate = %d, want 44100", doc.Rate)
	}
	if len(doc.Spans) != len(want) {
		t.Fatalf("map has %d spans, want %d: %+v", len(doc.Spans), len(want), doc.Spans)
	}
	const tol = 44100 / 1000 // 1 ms
	for i, w := range want {
		got := doc.Spans[i]
		if abs64(got.FromSample-w.From) > tol || abs64(got.ToSample-w.To) > tol {
			t.Errorf("span %d = [%d,%d), want ~[%d,%d)", i, got.FromSample, got.ToSample, w.From, w.To)
		}
		// Both spellings must agree, which is the point of carrying both:
		// the samples are exact and the seconds are the convenience.
		if wantSec := float64(got.FromSample) / 44100; math.Abs(got.FromSeconds-wantSec) > 1e-9 {
			t.Errorf("span %d fromSeconds = %v, want %v (it must match fromSample)", i, got.FromSeconds, wantSec)
		}
		if wantSec := float64(got.ToSample) / 44100; math.Abs(got.ToSeconds-wantSec) > 1e-9 {
			t.Errorf("span %d toSeconds = %v, want %v (it must match toSample)", i, got.ToSeconds, wantSec)
		}
	}
}

// TestBareAnalyzeMapsNoSilence pins that the map is opt-in: an analyze job
// that did not ask for one is byte-identical to what it always was.
func TestBareAnalyzeMapsNoSilence(t *testing.T) {
	root := t.TempDir()
	ref, _ := writeSilenceWAV(t, root)
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(analyzeReq(ref, pinID(t, res, ref)))
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if done.Analysis.Silence != nil {
		t.Error("a bare analyze job grew a silence summary")
	}
	if len(done.Outputs) != 0 {
		t.Errorf("a bare analyze job grew an output: %+v", done.Outputs)
	}
}

// TestSilenceCloneIsIndependent pins clone's new pointer branch: the
// summary is a pointer, so a shallow copy would share it with the store's
// live job and let a handed-out snapshot mutate underneath its reader.
func TestSilenceCloneIsIndependent(t *testing.T) {
	j := &Job{Analysis: &Analysis{Silence: &SilenceSummary{Spans: 3}}}
	c := j.clone()
	if c.Analysis.Silence == j.Analysis.Silence {
		t.Fatal("clone shares the silence summary pointer with the original")
	}
	c.Analysis.Silence.Spans = 99
	if j.Analysis.Silence.Spans != 3 {
		t.Errorf("mutating the clone changed the original: Spans = %d, want 3", j.Analysis.Silence.Spans)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// fixtureM4B is the chaptered m4b fixture; the MP4 demuxer parses its three
// markers off the header, with no metadata mapper involved.
const fixtureM4B = "chapters.m4b"

// fixtureM4BChapters are that fixture's own titles, in order.
var fixtureM4BChapters = []string{"Intro", "Middle", "Coda"}

// chapterMapper is a metadata mapper that reports the chapters it was built
// with and writes nothing, standing in for an injected tag library. Its Apply
// is never reached by these tests (the MP4 path embeds at the muxer instead),
// so it does no work.
type chapterMapper struct{ chapters []container.Chapter }

func (m chapterMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	return &meta.Info{Chapters: m.chapters}, nil
}

func (m chapterMapper) Apply(context.Context, string, *meta.Info, []container.Tag) error { return nil }

// transcodedChapterTitles transcodes the chaptered fixture under the given
// mapper (nil for an embedder that wires none) and returns the chapter titles
// the finished output actually carries.
//
// It reads them back off the written file rather than off the job, which is
// the whole point: the field being set on an options struct proves nothing
// about the bytes a client downloads.
func transcodedChapterTitles(t *testing.T, m meta.Mapper) []string {
	t.Helper()
	root := t.TempDir()
	copyFixture(t, root, fixtureM4B)
	res := openRoots(t, root)
	ref := "lib/" + fixtureM4B
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: m})

	j, err := r.Create(Request{
		Type: TypeTranscode, Src: ref, SourceID: pinID(t, res, ref), Format: "alac",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	out := soleOutput(t, done)
	raw, err := os.ReadFile(r.OutputPath(done, 0))
	if err != nil {
		t.Fatalf("reading the output: %v", err)
	}
	info, err := format.Probe(bytes.NewReader(raw), out.Container, nil)
	if err != nil {
		t.Fatalf("probing the output: %v", err)
	}
	titles := make([]string, len(info.Chapters))
	for i, ch := range info.Chapters {
		titles[i] = ch.Title
	}
	return titles
}

// TestTranscodeEmbedsChaptersWithoutMapper pins the write path's own chapter
// source, and it is the write half of the defect format.Info.Chapters was
// added to fix.
//
// The read half (GET /probe) got its fallback and the write half did not, so a
// server embedded by anyone who injects no metadata mapper transcoded a
// chaptered audiobook into a chapterless file: the mapper route reaches a
// transcode only through the CLI's injected mapper, while the demuxer had the
// markers parsed off the header the whole time. Config.Meta is nil here for
// exactly that reason; it is the configuration that was broken.
func TestTranscodeEmbedsChaptersWithoutMapper(t *testing.T) {
	got := transcodedChapterTitles(t, nil)
	if !slices.Equal(got, fixtureM4BChapters) {
		t.Errorf("output chapters = %v, want %v: a daemon with no metadata mapper wrote a chapterless output",
			got, fixtureM4BChapters)
	}
}

// TestTranscodeChapterPrecedence pins which source wins when both have
// something to say, and pins it to the answer GET /probe gives (server's
// ProbeJSON): the mapper's chapters win when it read any, the container's are
// the fallback. Read and write must not disagree about which source wins, or a
// caller probes a file and then transcodes it into different chapters.
func TestTranscodeChapterPrecedence(t *testing.T) {
	t.Run("a mapper that read chapters wins", func(t *testing.T) {
		// Deliberately not the fixture's own, so this cannot pass by both
		// sources happening to agree.
		mapped := []container.Chapter{
			{Start: 0, End: time.Second, Title: "One"},
			{Start: time.Second, End: 2 * time.Second, Title: "Two"},
		}
		got := transcodedChapterTitles(t, chapterMapper{chapters: mapped})
		if want := []string{"One", "Two"}; !slices.Equal(got, want) {
			t.Errorf("output chapters = %v, want the mapper's %v", got, want)
		}
	})

	t.Run("a mapper that read none falls back to the container's", func(t *testing.T) {
		got := transcodedChapterTitles(t, chapterMapper{})
		if !slices.Equal(got, fixtureM4BChapters) {
			t.Errorf("output chapters = %v, want the container's %v: a wired mapper that "+
				"knows no chapter form must not erase the ones the demuxer parsed", got, fixtureM4BChapters)
		}
	})
}

// TestSplitProgressAt pins the piece-to-job progress mapping, the arithmetic
// the split's bar is built out of. It is a pure function so the rules can be
// stated exactly, rather than inferred from whichever chunk boundaries an
// encoder happened to land on.
func TestSplitProgressAt(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		base, span, done, pieceTotal int64
		want                         int64
	}{
		{"the start of an untouched piece", 100, 400, 0, 400, 100},
		{"halfway through a piece", 100, 400, 200, 400, 300},
		{"a finished piece lands on its end", 100, 400, 400, 400, 500},
		// The bar is on the source's timeline, so a piece that doubles its
		// sample count on the way out still covers its own span and no more.
		{"an upsampling piece stays inside its span", 100, 400, 400, 800, 300},
		{"a downsampling piece still reaches its end", 100, 400, 200, 200, 500},
		// A projection the encode overshoots must not push into the next piece.
		{"an overshot projection clamps to the span", 100, 400, 500, 400, 500},
		// Both are the "nothing declares a length" case; the start is all
		// that can be told honestly.
		{"an unknown piece total reports the start", 100, 400, 999, 0, 100},
		{"an open-ended span reports the start", 100, -1, 999, 400, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitProgressAt(tc.base, tc.span, tc.done, tc.pieceTotal); got != tc.want {
				t.Errorf("splitProgressAt(base %d, span %d, done %d, pieceTotal %d) = %d, want %d",
					tc.base, tc.span, tc.done, tc.pieceTotal, got, tc.want)
			}
		})
	}
}

// TestSplitJobProgressIsOnTheSourceTimeline pins that a split's progress
// measures the split.
//
// It reported the piece index against the piece count, recomputed identically
// on every chunk, so each 250 ms window broadcast a byte-identical Progress:
// a store update, a clone, and an SSE frame per window, all carrying a number
// that had not changed, for the whole duration of every piece. The unit is the
// tell, and it is what this pins: a bar in samples of the source cannot be the
// old constant, which counted pieces.
func TestSplitJobProgressIsOnTheSourceTimeline(t *testing.T) {
	root := t.TempDir()
	ref := writeLongWAV(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	pools, release := saturatedPool(t)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Pools: pools})

	// The long fixture is 5 s at 44100; the cuts are interior points, so
	// these three make four pieces of deliberately uneven length.
	const srcSamples = 5 * 44100
	cuts := []int64{1 * 44100, 2 * 44100, 4 * 44100}
	j, err := r.Create(Request{
		Type: TypeSplit, Src: ref, SourceID: srcID, Format: "flac", FLACLevel: -1, Cuts: cuts,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitJob(t, r, j.ID, StateRunning)
	ch, cancelSub, ok := r.Subscribe(j.ID)
	if !ok {
		t.Fatal("Subscribe: job not found")
	}
	defer cancelSub()
	if snap, open := recv(t, ch); !open || snap.State != StateRunning {
		t.Fatalf("initial snapshot = %+v, want a running job", snap)
	}

	release()
	var seen []Progress
	for {
		s, open := recv(t, ch)
		if !open {
			break
		}
		if s.State == StateRunning && s.Progress != nil {
			seen = append(seen, *s.Progress)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no running snapshot carried progress")
	}
	for i, p := range seen {
		if p.Total != srcSamples {
			t.Errorf("progress %d total = %d, want the source's %d samples: a split's bar "+
				"counts the source, not its %d pieces", i, p.Total, srcSamples, len(cuts)+1)
		}
		if p.Done < 0 || p.Done > srcSamples {
			t.Errorf("progress %d done = %d, outside the source's [0,%d]", i, p.Done, srcSamples)
		}
		if i > 0 && p.Done < seen[i-1].Done {
			t.Errorf("progress %d went backwards: done %d after %d", i, p.Done, seen[i-1].Done)
		}
	}

	done := waitJob(t, r, j.ID, StateDone)
	if len(done.Outputs) != len(cuts)+1 {
		t.Errorf("split produced %d outputs, want %d", len(done.Outputs), len(cuts)+1)
	}
}

// TestMP4OutputsAreNamedByTheirExtension pins that a product is named with the
// file's extension rather than the container's name.
//
// The two part company across the whole mp4 family, which is the family an
// audiobook merge defaults to: alac's row declares no extension of its own, so
// the container name wrote out.alac, and progressive is not a second container
// but the row's own MP4 with its boxes flattened, so it wrote out.progressive.
// Neither is a file a player opens.
//
// The wire field is checked beside the name on purpose: it goes on carrying
// the container, and only the filename takes the extension.
func TestMP4OutputsAreNamedByTheirExtension(t *testing.T) {
	for _, tc := range []struct {
		name          string
		container     string
		wantContainer string
	}{
		{"a default mp4 row is not named for its row", "", "alac"},
		{"a flattened mp4 is not named for its override", "progressive", "progressive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, ref, srcID := openLib(t)
			dir := t.TempDir()
			r := openRunner(t, Config{Dir: dir, Resolver: res})

			j, err := r.Create(Request{
				Type: TypeTranscode, Src: ref, SourceID: srcID,
				Format: "alac", Container: tc.container,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			done := waitJob(t, r, j.ID, StateDone)

			out := soleOutput(t, done)
			if out.File != "out.m4a" {
				t.Errorf("output file = %q, want out.m4a", out.File)
			}
			if _, err := os.Stat(filepath.Join(dir, j.ID, "out.m4a")); err != nil {
				t.Errorf("no out.m4a on disk: %v", err)
			}
			if out.Container != tc.wantContainer {
				t.Errorf("Output.Container = %q, want %q: the field names the container, and the "+
					"extension is derived from it rather than replacing it", out.Container, tc.wantContainer)
			}
		})
	}

	// The split names its pieces at its own call site, so it gets its own
	// check: the index varies, the extension does not.
	t.Run("every piece of a split", func(t *testing.T) {
		root := t.TempDir()
		ref := writeLongWAV(t, root)
		res := openRoots(t, root)
		dir := t.TempDir()
		r := openRunner(t, Config{Dir: dir, Resolver: res})

		j, err := r.Create(Request{
			Type: TypeSplit, Src: ref, SourceID: pinID(t, res, ref),
			Format: "alac", Cuts: []int64{2 * 44100},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		done := waitJob(t, r, j.ID, StateDone)
		if len(done.Outputs) != 2 {
			t.Fatalf("split produced %d outputs, want 2", len(done.Outputs))
		}
		for i, out := range done.Outputs {
			want := fmt.Sprintf("out.%d.m4a", i)
			if out.File != want {
				t.Errorf("piece %d file = %q, want %q", i, out.File, want)
			}
			if _, err := os.Stat(filepath.Join(dir, j.ID, want)); err != nil {
				t.Errorf("no %s on disk: %v", want, err)
			}
		}
	})
}

func TestTranscodeJobEndToEnd(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()
	r := openRunner(t, Config{Dir: dir, Resolver: res})

	j, err := r.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !ulid.Valid(j.ID) {
		t.Errorf("Create id %q is not a valid ulid", j.ID)
	}
	if j.State != StateQueued {
		t.Errorf("Create state = %s, want %s", j.State, StateQueued)
	}

	done := waitJob(t, r, j.ID, StateDone)
	out := soleOutput(t, done)
	if out.Container != "flac" {
		t.Errorf("Output.Container = %q, want flac", out.Container)
	}
	if out.File != "out.flac" {
		t.Errorf("Output.File = %q, want out.flac", out.File)
	}
	if out.Samples <= 0 {
		t.Errorf("Output.Samples = %d, want > 0", out.Samples)
	}
	if out.MediaType == "" {
		t.Error("Output.MediaType is empty")
	}
	if done.Started == nil || done.Finished == nil {
		t.Errorf("done job is missing timestamps: started %v, finished %v", done.Started, done.Finished)
	}

	outPath := filepath.Join(dir, j.ID, out.File)
	if p := r.OutputPath(done, 0); p != outPath {
		t.Errorf("OutputPath = %q, want %q", p, outPath)
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("output file: %v", err)
	}
	if fi.Size() != out.Bytes {
		t.Errorf("output file is %d bytes, Output.Bytes says %d", fi.Size(), out.Bytes)
	}

	// job.json round-trips: the persisted document is the wire shape.
	raw, err := os.ReadFile(filepath.Join(dir, j.ID, jobFile))
	if err != nil {
		t.Fatalf("reading job.json: %v", err)
	}
	var onDisk Job
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshaling job.json: %v", err)
	}
	if onDisk.SchemaVersion != SchemaVersion || onDisk.ID != j.ID {
		t.Errorf("job.json names %q schema %d, want %q schema %d",
			onDisk.ID, onDisk.SchemaVersion, j.ID, SchemaVersion)
	}
	if onDisk.State != StateDone {
		t.Errorf("job.json state = %s, want %s", onDisk.State, StateDone)
	}
	if len(onDisk.Outputs) != 1 || onDisk.Outputs[0] != *out {
		t.Errorf("job.json outputs = %+v, want [%+v]", onDisk.Outputs, out)
	}
}

func TestAnalyzeJobEndToEnd(t *testing.T) {
	// The R128 integrated gate needs 400 ms blocks, so this test measures
	// the synthesized multi-second sine, not the tiny repo fixture.
	root := t.TempDir()
	ref := writeLongWAV(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(analyzeReq(ref, srcID))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	a := done.Analysis
	if a == nil {
		t.Fatal("done analyze has no analysis")
	}
	// The fixture is a loud sine, so integrated loudness must gate in
	// as a finite number, never the silence null.
	if a.IntegratedLUFS == nil {
		t.Fatal("IntegratedLUFS is nil, want a finite measurement")
	}
	if math.IsInf(*a.IntegratedLUFS, 0) || math.IsNaN(*a.IntegratedLUFS) {
		t.Errorf("IntegratedLUFS = %v, want finite", *a.IntegratedLUFS)
	}
	if a.Samples <= 0 {
		t.Errorf("Analysis.Samples = %d, want > 0", a.Samples)
	}
	if a.DurationSeconds <= 0 {
		t.Errorf("Analysis.DurationSeconds = %v, want > 0", a.DurationSeconds)
	}
	if a.Rate <= 0 {
		t.Errorf("Analysis.Rate = %d, want > 0", a.Rate)
	}
	if len(done.Outputs) != 0 {
		t.Errorf("analyze job grew an output: %+v", done.Outputs)
	}
}

func TestSourceChangedFails(t *testing.T) {
	res, ref, _ := openLib(t)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	// A well-formed identity that cannot match the fixture: the job must
	// refuse to transcode bytes other than the ones it was created over.
	j, err := r.Create(transcodeReq(ref, "1-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	failed := waitJob(t, r, j.ID, StateFailed)
	if failed.Error == nil {
		t.Fatal("failed job has no error")
	}
	if failed.Error.Code != string(waxerr.CodeSourceChanged) {
		t.Errorf("Error.Code = %q, want %q", failed.Error.Code, waxerr.CodeSourceChanged)
	}
	if failed.Finished == nil {
		t.Error("failed job has no finished time")
	}
}

func TestDeleteMidRun(t *testing.T) {
	root := t.TempDir()
	ref := writeLongWAV(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	pools, _ := saturatedPool(t)
	dir := t.TempDir()
	r := openRunner(t, Config{Dir: dir, Resolver: res, Pools: pools})

	j, err := r.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ch, cancelSub, ok := r.Subscribe(j.ID)
	if !ok {
		t.Fatal("Subscribe: job not found")
	}
	t.Cleanup(cancelSub)

	// The held pool keeps the job pinned in running, so waiting on the
	// subscription for the running snapshot cannot race its completion.
	for {
		s, open := recv(t, ch)
		if !open {
			t.Fatal("subscription closed before the job ran")
		}
		if s.State == StateRunning {
			break
		}
	}
	// Wait for the output file before deleting: it proves the worker is
	// past its last file creation and parked in the progress hook, so
	// Delete's RemoveAll cannot race a concurrent create (a real window;
	// deleting between the claim and the output open can strand the job
	// directory on disk until the next boot sweeps it).
	waitFor(t, "output file creation", func() bool {
		_, err := os.Stat(filepath.Join(dir, j.ID, "out.flac"))
		return err == nil
	})

	if err := r.Delete(j.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var last *Job
	for {
		s, open := recv(t, ch)
		if !open {
			break
		}
		last = s
	}
	if last == nil || last.State != StateCanceled {
		t.Fatalf("last subscription event = %+v, want a canceled snapshot", last)
	}
	if _, ok := r.Get(j.ID); ok {
		t.Error("Get after Delete still finds the job")
	}
	if err := r.Delete(j.ID); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Errorf("second Delete: code %q, want %q", waxerr.CodeOf(err), waxerr.CodeNotFound)
	}
	// The worker may still be unwinding, but nothing recreates the
	// directory once the record is gone.
	waitFor(t, "job dir removal", func() bool {
		_, err := os.Stat(filepath.Join(dir, j.ID))
		return os.IsNotExist(err)
	})
}

func TestSubscribeSemantics(t *testing.T) {
	t.Run("terminal job yields one snapshot then close", func(t *testing.T) {
		res, ref, srcID := openLib(t)
		r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})
		j, err := r.Create(analyzeReq(ref, srcID))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		waitJob(t, r, j.ID, StateDone)

		ch, cancelSub, ok := r.Subscribe(j.ID)
		if !ok {
			t.Fatal("Subscribe: job not found")
		}
		defer cancelSub()
		snap, open := recv(t, ch)
		if !open {
			t.Fatal("channel closed before the snapshot")
		}
		if snap.State != StateDone {
			t.Errorf("snapshot state = %s, want %s", snap.State, StateDone)
		}
		if _, open := recv(t, ch); open {
			t.Error("terminal subscription delivered a second event, want close")
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		res, _, _ := openLib(t)
		r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})
		if _, _, ok := r.Subscribe("01ARZ3NDEKTSV4RRFFQ69G5FAV"); ok {
			t.Error("Subscribe on an unknown id reported ok")
		}
	})

	t.Run("running job streams progress", func(t *testing.T) {
		root := t.TempDir()
		ref := writeLongWAV(t, root)
		res := openRoots(t, root)
		srcID := pinID(t, res, ref)
		pools, release := saturatedPool(t)
		r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Pools: pools})

		j, err := r.Create(transcodeReq(ref, srcID))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		waitJob(t, r, j.ID, StateRunning)
		ch, cancelSub, ok := r.Subscribe(j.ID)
		if !ok {
			t.Fatal("Subscribe: job not found")
		}
		defer cancelSub()
		snap, open := recv(t, ch)
		if !open || snap.State != StateRunning {
			t.Fatalf("initial snapshot = %+v, want a running job", snap)
		}

		release()
		var sawProgress bool
		var last *Job
		for {
			s, open := recv(t, ch)
			if !open {
				break
			}
			last = s
			if s.State == StateRunning && s.Progress != nil {
				sawProgress = true
				if s.Progress.Phase != "transcode" {
					t.Errorf("Progress.Phase = %q, want transcode", s.Progress.Phase)
				}
			}
		}
		if !sawProgress {
			t.Error("no running snapshot carried progress")
		}
		if last == nil || last.State != StateDone {
			t.Fatalf("last subscription event = %+v, want done", last)
		}
	})
}

func TestFIFOSingleSlot(t *testing.T) {
	root := t.TempDir()
	ref := writeLongWAV(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	pools, release := saturatedPool(t)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Pools: pools, Slots: 1})

	first, err := r.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	waitJob(t, r, first.ID, StateRunning)
	second, err := r.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	// One slot, one worker, and it is inside the first job: the second
	// cannot have been claimed.
	got, ok := r.Get(second.ID)
	if !ok {
		t.Fatal("Get second: not found")
	}
	if got.State != StateQueued {
		t.Errorf("second job state = %s while the first runs, want %s", got.State, StateQueued)
	}
	if n := r.Running(); n != 1 {
		t.Errorf("Running = %d, want 1", n)
	}

	release()
	firstDone := waitJob(t, r, first.ID, StateDone)
	secondDone := waitJob(t, r, second.ID, StateDone)
	if firstDone.Finished == nil || secondDone.Started == nil {
		t.Fatal("done jobs are missing timestamps")
	}
	if secondDone.Started.Before(*firstDone.Finished) {
		t.Errorf("second started %v before first finished %v", secondDone.Started, firstDone.Finished)
	}
}

// TestLoudnessAnalyzeNonMP4OmitsPlaceholders pins the placeholder
// gating: only the MP4 path patches ReplayGain placeholders after the
// encode, so a non-MP4 output must never embed them at the muxer. With
// no mapper wired there is no post-pass to replace them, which is
// exactly the configuration that used to ship unity ReplayGain.
func TestLoudnessAnalyzeNonMP4OmitsPlaceholders(t *testing.T) {
	res, ref, srcID := openLib(t)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(Request{
		Type: TypeTranscode, Src: ref, SourceID: srcID,
		Format: "flac", Loudness: "analyze",
	})
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if done.Analysis == nil || done.Analysis.ReplayGainTrackGain == "" {
		t.Fatalf("analysis: %+v", done.Analysis)
	}
	raw, err := os.ReadFile(r.OutputPath(done, 0))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("REPLAYGAIN")) {
		t.Fatal("non-MP4 output embeds ReplayGain placeholders with no post-pass to patch them")
	}
}
