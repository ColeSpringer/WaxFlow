package jobs

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/internal/admission"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// Config wires the runner's dependencies.
type Config struct {
	// Dir is the job store directory (dataDir/jobs).
	Dir string
	// Engine runs the transcodes and analyses.
	Engine *waxflow.Engine
	// Resolver opens job sources.
	Resolver source.Resolver
	// Meta maps metadata; nil disables the passthrough (outputs carry
	// no tags) but never fails a job.
	Meta meta.Mapper
	// Pools is consulted between chunks: a saturated live pool pauses
	// job pipelines so interactive streams keep every core.
	Pools *admission.Pools
	// ResolveGain resolves a gain= spelling against source metadata.
	// The server owns gain policy (modes, clamps); jobs stay policy-free.
	ResolveGain func(gain string, info *meta.Info) (float64, error)
	// Slots is the number of concurrently running jobs (jobSlots).
	Slots int
	// Profile is the daemon's resampler profile.
	Profile resample.Profile
	// Logger receives job lifecycle notes; nil discards.
	Logger *slog.Logger
}

// Runner owns the job store, the worker pool, and the event fan-out.
type Runner struct {
	cfg   Config
	log   *slog.Logger
	store *store

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	subs    map[string]map[chan *Job]struct{}
}

// Open loads the store (requeueing interrupted jobs) and starts the
// workers. Close stops them; jobs still running at Close are left in
// the running state on disk, which the next Open resets to queued (the
// restart contract).
func Open(cfg Config) (*Runner, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	st, err := openStore(cfg.Dir, log)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		cfg:     cfg,
		log:     log,
		store:   st,
		wake:    make(chan struct{}, 1),
		cancels: map[string]context.CancelFunc{},
		subs:    map[string]map[chan *Job]struct{}{},
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for range max(1, cfg.Slots) {
		r.wg.Add(1)
		go r.worker()
	}
	return r, nil
}

// Close stops the workers and waits for running jobs to unwind. Their
// persisted state stays running, so the next Open requeues them.
func (r *Runner) Close() {
	r.cancel()
	r.wg.Wait()
}

// Create persists and enqueues a validated request.
func (r *Runner) Create(req Request) (*Job, error) {
	j, err := r.store.create(req)
	if err != nil {
		return nil, err
	}
	r.kick()
	return j, nil
}

// Get returns a job snapshot.
func (r *Runner) Get(id string) (*Job, bool) { return r.store.get(id) }

// List returns all jobs, oldest first.
func (r *Runner) List() []*Job { return r.store.list() }

// Running reports how many jobs are executing right now (metrics).
func (r *Runner) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels)
}

// OutputPath resolves a finished transcode's file.
func (r *Runner) OutputPath(j *Job) string {
	if j.Output == nil {
		return ""
	}
	return filepath.Join(r.store.jobDir(j.ID), j.Output.File)
}

// OutputFile opens a finished transcode's product for serving.
func (r *Runner) OutputFile(j *Job) (*os.File, error) {
	p := r.OutputPath(j)
	if p == "" {
		return nil, waxerr.New(waxerr.CodeInternal, "jobs: job has no output")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "jobs: opening output", err)
	}
	return f, nil
}

// Delete cancels the job if it is running, tells subscribers it was
// canceled, and removes it from the index and disk. Unknown ids report
// not-found.
func (r *Runner) Delete(id string) error {
	j, ok := r.store.get(id)
	if !ok {
		return waxerr.New(waxerr.CodeNotFound, "jobs: no such job")
	}
	// Remove the record before canceling: once the index entry is gone
	// the worker's terminal update no-ops, so subscribers can only see
	// the canceled snapshot below, never a racing failed one.
	if !r.store.remove(id) {
		return waxerr.New(waxerr.CodeNotFound, "jobs: no such job")
	}
	r.mu.Lock()
	cancel := r.cancels[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	j.State = StateCanceled
	r.notify(j, true)
	return nil
}

// Subscribe returns a channel of job snapshots: the current state
// immediately, then every update, closed after a terminal event (or a
// delete). The cancel func must be called when the consumer leaves.
func (r *Runner) Subscribe(id string) (<-chan *Job, func(), bool) {
	j, ok := r.store.get(id)
	if !ok {
		return nil, nil, false
	}
	ch := make(chan *Job, 8)
	ch <- j
	if j.State.Terminal() {
		close(ch)
		return ch, func() {}, true
	}
	r.mu.Lock()
	set := r.subs[id]
	if set == nil {
		set = map[chan *Job]struct{}{}
		r.subs[id] = set
	}
	set[ch] = struct{}{}
	// The job may have finished (or been deleted) between the snapshot
	// and this registration, in which case the terminal notify already
	// passed and nothing would ever close the channel. Re-check under
	// the lock notify holds, emit the terminal state, and close out.
	if cur, ok := r.store.get(id); !ok || cur.State.Terminal() {
		if ok {
			ch <- cur
		}
		delete(set, ch)
		if len(set) == 0 {
			delete(r.subs, id)
		}
		close(ch)
		r.mu.Unlock()
		return ch, func() {}, true
	}
	r.mu.Unlock()
	cancel := func() {
		r.mu.Lock()
		if set, ok := r.subs[id]; ok {
			if _, live := set[ch]; live {
				delete(set, ch)
				close(ch)
				if len(set) == 0 {
					delete(r.subs, id)
				}
			}
		}
		r.mu.Unlock()
	}
	return ch, cancel, true
}

// notify fans a snapshot out to the job's subscribers; terminal closes
// them. A full subscriber channel drops its oldest snapshot first, so a
// slow consumer coalesces updates instead of stalling the runner.
func (r *Runner) notify(j *Job, terminal bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.subs[j.ID]
	for ch := range set {
		select {
		case ch <- j.clone():
		default:
			select {
			case <-ch:
			default:
			}
			ch <- j.clone()
		}
		if terminal {
			close(ch)
			delete(set, ch)
		}
	}
	if terminal {
		delete(r.subs, j.ID)
	}
}

// kick wakes one idle worker.
func (r *Runner) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) worker() {
	defer r.wg.Done()
	for {
		j := r.store.claimNext()
		if j == nil {
			select {
			case <-r.ctx.Done():
				return
			case <-r.wake:
			}
			continue
		}
		// Propagate the wake: a burst of creates can land while a sibling
		// is between its failed claim and its park, where one buffered
		// token is all that survives; the claimer re-kicks so parked
		// workers keep waking while the queue is non-empty.
		r.kick()
		r.notify(j, false)
		r.run(j)
		// Another job may already be queued; loop immediately.
	}
}

// run executes one claimed job to a terminal state.
func (r *Runner) run(j *Job) {
	ctx, cancel := context.WithCancel(r.ctx)
	r.mu.Lock()
	r.cancels[j.ID] = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, j.ID)
		r.mu.Unlock()
	}()

	var err error
	switch j.Type {
	case TypeTranscode:
		err = r.runTranscode(ctx, j)
	case TypeAnalyze:
		err = r.runAnalyze(ctx, j)
	default:
		err = waxerr.New(waxerr.CodeInvalidRequest, "jobs: unknown job type")
	}
	if err != nil && r.ctx.Err() != nil {
		// Daemon shutdown: leave the persisted state running so the next
		// boot requeues the job from zero.
		return
	}
	now := time.Now().UTC()
	final := r.store.update(j.ID, true, func(job *Job) {
		job.Finished = &now
		job.Progress = nil
		if err != nil {
			job.State = StateFailed
			job.Error = &ErrInfo{Code: string(waxerr.CodeOf(err)), Message: err.Error()}
		} else {
			job.State = StateDone
		}
	})
	if final == nil {
		// Deleted mid-run; Delete already told subscribers. Sweep the
		// directory again: files this worker created after Delete's
		// RemoveAll scanned (an output mid-creation) would otherwise
		// orphan it until the next boot drops it as unreadable.
		os.RemoveAll(r.store.jobDir(j.ID))
		return
	}
	if err != nil {
		r.log.Warn("job failed", "id", j.ID, "type", j.Type, "err", err)
	} else {
		r.log.Debug("job done", "id", j.ID, "type", j.Type)
	}
	r.notify(final, true)
}

// open resolves the job's source and enforces the pinned identity.
func (r *Runner) open(req Request) (*source.File, error) {
	src, err := r.cfg.Resolver.Resolve(req.Src)
	if err != nil {
		return nil, err
	}
	if req.SourceID != "" && req.SourceID != src.ID.String() {
		src.Close()
		return nil, waxerr.New(waxerr.CodeSourceChanged,
			"jobs: the source changed since the job was created")
	}
	return src, nil
}

// progressFunc builds the per-chunk hook: it updates the in-memory
// progress (broadcasting at most ~4 times a second), and pauses the
// pipeline while the live pool is saturated, which is the plan's
// job-yields-to-interactive admission rule.
func (r *Runner) progressFunc(ctx context.Context, id, phase string) func(done, total int64) {
	var last time.Time
	return func(done, total int64) {
		for r.cfg.Pools != nil && r.cfg.Pools.LiveSaturated() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		if now := time.Now(); now.Sub(last) >= 250*time.Millisecond {
			last = now
			p := &Progress{Phase: phase, Done: done, Total: total, Percent: -1}
			if total > 0 {
				p.Percent = min(100, float64(done)*100/float64(total))
			}
			if j := r.store.update(id, false, func(job *Job) { job.Progress = p }); j != nil {
				r.notify(j, false)
			}
		}
	}
}

// warn records a non-fatal note on the job.
func (r *Runner) warn(id, note string) {
	if j := r.store.update(id, false, func(job *Job) { job.Warnings = append(job.Warnings, note) }); j != nil {
		r.notify(j, false)
	}
}

// deriveOutputLoudness projects the output's loudness and true peak
// from the source measurement and the applied gain, for outputs the
// engine cannot decode back (fragmented MP4). A linear gain shifts both
// numbers exactly; positive gain engages the true-peak limiter, whose
// ceiling caps the derived peak.
func deriveOutputLoudness(src *waxflow.AnalyzeResult, gainDB float64) (lufs, truePeakDB float64) {
	lufs = math.Inf(-1)
	if !math.IsInf(src.IntegratedLUFS, -1) {
		lufs = src.IntegratedLUFS + gainDB
	}
	truePeakDB = src.TruePeakDB + gainDB
	if gainDB > 0 {
		truePeakDB = min(truePeakDB, gain.DefaultCeilingDB)
	}
	return lufs, truePeakDB
}

// analysisOf maps an engine measurement onto the wire shape.
func analysisOf(res *waxflow.AnalyzeResult) *Analysis {
	a := &Analysis{
		IntegratedLUFS: finiteOrNil(res.IntegratedLUFS),
		LoudnessRange:  res.LoudnessRange,
		TruePeakDB:     finiteOrNil(res.TruePeakDB),
		SamplePeakDB:   finiteOrNil(res.SamplePeakDB),
		Samples:        res.Samples,
		Rate:           res.Format.Rate,
		Channels:       res.Format.Channels,
	}
	if res.Format.Rate > 0 {
		a.DurationSeconds = float64(res.Samples) / float64(res.Format.Rate)
	}
	return a
}

func finiteOrNil(v float64) *float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	return &v
}

// runAnalyze measures the source and stores the numbers.
func (r *Runner) runAnalyze(ctx context.Context, j *Job) error {
	src, err := r.open(j.Request)
	if err != nil {
		return err
	}
	defer src.Close()
	res, err := r.cfg.Engine.Analyze(ctx, src, src.Ext, waxflow.AnalyzeOptions{
		Progress: r.progressFunc(ctx, j.ID, "analyze"),
	})
	if err != nil {
		return err
	}
	r.store.update(j.ID, false, func(job *Job) { job.Analysis = analysisOf(res) })
	return nil
}

// TranscodeOptions maps the request onto engine options. gainDB is the
// resolved gain and profile the daemon's resampler profile; the lossy
// bit rate feeds every encoder's field, since exactly one format is
// selected.
func (req Request) TranscodeOptions(gainDB float64, profile resample.Profile) waxflow.TranscodeOptions {
	return waxflow.TranscodeOptions{
		Format:          req.Format,
		Container:       req.Container,
		Rate:            req.Rate,
		Channels:        req.Channels,
		BitDepth:        req.Bits,
		GainDB:          gainDB,
		FLACLevel:       req.FLACLevel,
		MP3Bitrate:      req.Bitrate * 1000,
		OpusBitrate:     req.Bitrate * 1000,
		AACBitrate:      req.Bitrate * 1000,
		ResampleProfile: profile,
	}
}

// runTranscode is the full job pipeline: read metadata, resolve gain
// (measuring the source first under loudness:analyze), transcode into
// the job directory, then finish the metadata: measured ReplayGain
// patched into MP4 headers or written with the full tag set by the
// mapping post-pass.
func (r *Runner) runTranscode(ctx context.Context, j *Job) error {
	req := j.Request
	src, err := r.open(req)
	if err != nil {
		return err
	}
	defer src.Close()

	var info *meta.Info
	if r.cfg.Meta != nil {
		mi, err := r.cfg.Meta.Read(ctx, src, src.Ext, meta.ReadOptions{Pictures: true})
		if err != nil {
			return err
		}
		info = mi
		for _, w := range info.Warnings {
			r.warn(j.ID, w)
		}
	}

	analyzeLoudness := req.Loudness == "analyze"
	var gainDB float64
	var analysis *Analysis
	var srcRes *waxflow.AnalyzeResult
	if analyzeLoudness {
		res, err := r.cfg.Engine.Analyze(ctx, src, src.Ext, waxflow.AnalyzeOptions{
			Progress: r.progressFunc(ctx, j.ID, "analyze"),
		})
		if err != nil {
			return err
		}
		srcRes = res
		analysis = analysisOf(res)
		if !math.IsInf(res.IntegratedLUFS, -1) {
			// The exact gain to the RG2 reference, deliberately unclamped
			// (unlike the HTTP gain= policy bound): the measurement is
			// exact and the true-peak limiter guards the top end.
			gainDB = meta.ReplayGainGainDB(res.IntegratedLUFS)
		}
		analysis.AppliedGainDB = &gainDB
	} else if r.cfg.ResolveGain != nil {
		if gainDB, err = r.cfg.ResolveGain(req.Gain, info); err != nil {
			return err
		}
	}

	dropRG := gainDB != 0 || analyzeLoudness
	tagInfo := info
	if dropRG {
		tagInfo = meta.WithoutReplayGain(info)
	}
	opts := req.TranscodeOptions(gainDB, r.cfg.Profile)
	opts.Tags = meta.FullTags(tagInfo)
	if info != nil {
		opts.Chapters = info.Chapters
		if p := info.FrontPicture(); p != nil {
			opts.Art = &container.Picture{MIME: p.MIME, Data: p.Data}
		}
	}

	// Plan before running for the output's identity fields (media type,
	// container); creation validated this same plan, so failures here
	// mean the source itself changed shape.
	probe, err := r.cfg.Engine.Probe(src, src.Ext, nil)
	if err != nil {
		return err
	}
	plan, err := r.cfg.Engine.PlanTranscode(probe.Default(), opts)
	if err != nil {
		return err
	}
	isMP4 := plan.MediaType == "audio/mp4"
	if analyzeLoudness && isMP4 {
		// Fixed-width placeholders for the MP4 muxer to embed at Begin;
		// the measured values patch in place after the encode. Only the
		// MP4 path patches, so only it gets placeholders: any other
		// format would ship the unity values verbatim whenever the
		// best-effort post-pass cannot run (its real values arrive
		// through Apply's extra tags instead). Tags never shape the
		// plan, so appending after planning is safe.
		opts.Tags = append(opts.Tags,
			container.Tag{Key: "REPLAYGAIN_TRACK_GAIN", Value: meta.FormatGain(0)},
			container.Tag{Key: "REPLAYGAIN_TRACK_PEAK", Value: meta.FormatPeak(0)})
	}

	outName := "out." + plan.Container
	outPath := filepath.Join(r.store.jobDir(j.ID), outName)
	f, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: creating output", err)
	}
	defer f.Close()

	opts.Progress = r.progressFunc(ctx, j.ID, "transcode")
	res, err := r.cfg.Engine.Transcode(ctx, src, src.Ext, f, opts)
	if err != nil {
		return err
	}

	var rg []container.Tag
	if analyzeLoudness {
		var outLUFS, outTP float64
		if isMP4 {
			// There is no fragmented-MP4 read path, so the output cannot
			// be decoded back; derive its values from the source
			// measurement instead: exact for lossless ALAC, within the
			// encoder's fraction of a dB for AAC. Positive gain engages
			// the true-peak limiter, which caps the derived peak at its
			// ceiling.
			outLUFS, outTP = deriveOutputLoudness(srcRes, gainDB)
		} else {
			// Measure the finished output so the written ReplayGain
			// values describe exactly the bytes a player gets (the
			// limiter may have shaved the projected peak).
			fsrc, err := container.FileSource(f)
			if err != nil {
				return err
			}
			outRes, err := r.cfg.Engine.Analyze(ctx, fsrc, plan.Container, waxflow.AnalyzeOptions{
				Progress: r.progressFunc(ctx, j.ID, "finalize"),
			})
			if err != nil {
				return err
			}
			outLUFS, outTP = outRes.IntegratedLUFS, outRes.TruePeakDB
		}
		rg = meta.ReplayGainTags(outLUFS, outTP)
		analysis.ReplayGainTrackGain = rg[0].Value
		analysis.ReplayGainTrackPeak = rg[1].Value
		if isMP4 {
			if err := mp4.PatchFreeform(f, "REPLAYGAIN_TRACK_GAIN", meta.FormatGain(0), rg[0].Value); err != nil {
				return err
			}
			if err := mp4.PatchFreeform(f, "REPLAYGAIN_TRACK_PEAK", meta.FormatPeak(0), rg[1].Value); err != nil {
				return err
			}
		}
	}
	if err := f.Sync(); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: output sync", err)
	}
	if err := f.Close(); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: output close", err)
	}

	// The mapping post-pass writes the full set (tags, pictures,
	// chapters, synced lyrics, measured ReplayGain) onto formats the
	// mapper can rewrite; MP4 already got everything at Begin plus the
	// patches above. A post-pass failure is a warning, not a dead job:
	// the audio is finished and correct. The job context applies here
	// like everywhere else: the mapper's write is atomic, a shutdown
	// leaves the job running on disk to rerun from zero (post-pass
	// included), and a delete is discarding the directory anyway, so an
	// interrupted Apply never loses anything a rerun does not redo.
	if r.cfg.Meta != nil && !isMP4 && tagInfo != nil {
		if err := r.cfg.Meta.Apply(ctx, outPath, tagInfo, rg); err != nil {
			r.warn(j.ID, "metadata post-pass: "+err.Error())
		}
	}

	fi, err := os.Stat(outPath)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "jobs: output stat", err)
	}
	r.store.update(j.ID, false, func(job *Job) {
		job.Output = &Output{
			File:      outName,
			MediaType: plan.MediaType,
			Container: plan.Container,
			Bytes:     fi.Size(),
			Samples:   res.Samples,
			Rate:      plan.Format.Rate,
		}
		job.Analysis = analysis
	})
	return nil
}
