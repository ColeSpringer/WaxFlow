package jobs

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/admission"
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
	src, err := res.Resolve(ref)
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
	out := done.Output
	if out == nil {
		t.Fatal("done transcode has no output")
	}
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
	if p := r.OutputPath(done); p != outPath {
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
	if onDisk.SchemaVersion != 1 || onDisk.ID != j.ID {
		t.Errorf("job.json names %q schema %d, want %q schema 1", onDisk.ID, onDisk.SchemaVersion, j.ID)
	}
	if onDisk.State != StateDone {
		t.Errorf("job.json state = %s, want %s", onDisk.State, StateDone)
	}
	if onDisk.Output == nil || *onDisk.Output != *out {
		t.Errorf("job.json output = %+v, want %+v", onDisk.Output, out)
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
	if done.Output != nil {
		t.Errorf("analyze job grew an output: %+v", done.Output)
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
	raw, err := os.ReadFile(r.OutputPath(done))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("REPLAYGAIN")) {
		t.Fatal("non-MP4 output embeds ReplayGain placeholders with no post-pass to patch them")
	}
}
