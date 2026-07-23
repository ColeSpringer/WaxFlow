package jobs

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/colespringer/waxflow/format"
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
	// MintTimeline stores a multi-source timeline and reports what it named,
	// measuring the members whose headers cannot declare an exact length.
	// Nil disables timeline jobs.
	//
	// It is a hook for the same reason ResolveGain is: the timeline store and
	// its identity rules belong to the server, and a job here is only the
	// async wrapper the measuring needs. The runner supplies the progress
	// callback so a cold mint of a long queue reports where it is, and passes
	// the request's crossfade and member spans so the async mint shapes the
	// same duration and boundaries, and mints the same digest, the sync path
	// would.
	MintTimeline func(ctx context.Context, srcs []string, spans []MemberSpan, crossfadeSeconds float64, progress func(done, total int64)) (*Timeline, error)
	// MeasureTrack reports a source's default track with an authoritative
	// length, walking the source when its headers cannot declare one. Nil
	// disables merge jobs.
	//
	// It is a hook for the reason the two above are: the memo that makes this
	// affordable belongs to the server (it is the same one the HLS timeline
	// mint fills, keyed by source identity), and a merge is the same measuring
	// problem the mint has. Sharing it is what makes a merge of an album that
	// was already timelined cost nothing, and what makes creation's
	// measurement the run's.
	//
	// A merge cannot use the declared length the way a single-source transcode
	// can. Concat holds every member to its track, so an advisory total that
	// is two samples out fails the run outright; and even if it did not, a
	// prefix sum desyncs every position after it.
	MeasureTrack func(src *source.File) (container.Track, error)
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

// OutputPath resolves the file of the job's nth output, empty for an index
// the job does not have.
func (r *Runner) OutputPath(j *Job, n int) string {
	if n < 0 || n >= len(j.Outputs) {
		return ""
	}
	return filepath.Join(r.store.jobDir(j.ID), j.Outputs[n].File)
}

// OutputFile opens the job's nth product for serving.
func (r *Runner) OutputFile(j *Job, n int) (*os.File, error) {
	p := r.OutputPath(j, n)
	if p == "" {
		return nil, waxerr.New(waxerr.CodeNotFound, "jobs: no such output")
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
	case TypeTimeline:
		err = r.runTimeline(ctx, j)
	case TypeMerge:
		err = r.runMerge(ctx, j)
	case TypeSplit:
		err = r.runSplit(ctx, j)
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
func (r *Runner) open(ctx context.Context, req Request) (*source.File, error) {
	return r.resolvePinned(ctx, req.Src, req.SourceID, -1)
}

// openMember resolves a merge's ith member and enforces its own pinned
// identity.
func (r *Runner) openMember(ctx context.Context, req Request, i int) (*source.File, error) {
	var id string
	if i < len(req.SourceIDs) {
		id = req.SourceIDs[i]
	}
	return r.resolvePinned(ctx, req.Srcs[i], id, i)
}

// resolvePinned resolves ref and holds it to the identity the job was created
// against. member is the member index for a merge, negative for a job with one
// source.
//
// The index is in the message rather than only in the log, and that is the
// whole reason this is shared rather than two functions. A merge of 40
// chapters that reports "the source changed" names 40 candidates; naming the
// one that moved is the difference between a caller re-uploading a file and a
// caller re-uploading a library.
func (r *Runner) resolvePinned(ctx context.Context, ref, id string, member int) (*source.File, error) {
	src, err := r.cfg.Resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if id != "" && id != src.ID.String() {
		src.Close()
		msg := "jobs: the source changed since the job was created"
		if member >= 0 {
			msg = fmt.Sprintf("jobs: member %d (%s): the source changed since the job was created", member, ref)
		}
		return nil, waxerr.New(waxerr.CodeSourceChanged, msg)
	}
	return src, nil
}

// closingMedia releases the source handle with the media, so a Concat member
// closed on advance gives its file descriptor back.
type closingMedia struct {
	format.Media
	f *source.File
}

func (m closingMedia) Close() error {
	err := m.Media.Close()
	m.f.Close()
	return err
}

// openMemberMedia is a merge member's Open: resolve, re-pin, decode.
//
// The pin is checked here as well as during the measuring pass, because these
// are different moments and the second is the one that matters: members open
// lazily, as the concat reaches them, which for a long queue is minutes after
// the pass that measured them. A member replaced in between would otherwise be
// concatenated at its old length, which is the desync the pin exists to catch.
//
// ctx is the job's own, never a request's, exactly as ConcatSource.Open
// requires: this fires deep inside the transcode, long after the call that
// built the Concat returned.
func (r *Runner) openMemberMedia(ctx context.Context, req Request, i int) (format.Media, error) {
	f, err := r.openMember(ctx, req, i)
	if err != nil {
		return nil, err
	}
	med, err := r.cfg.Engine.OpenStream(f, f.Ext)
	if err != nil {
		f.Close()
		return nil, err
	}
	return closingMedia{Media: med, f: f}, nil
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
// numbers exactly; when the encode chain's true-peak limiter is engaged
// (limited), it caps the projected peak at the ceiling. The limiter runs
// for positive gain and also for a downmix whose matrix can sum past unity,
// so a downmix measurement must pass limited even when the gain is negative:
// analyze runs the raw fold with no limiter, so src.TruePeakDB can already
// sit above the ceiling the encode holds.
func deriveOutputLoudness(src *waxflow.AnalyzeResult, gainDB float64, limited bool) (lufs, truePeakDB float64) {
	lufs = math.Inf(-1)
	if !math.IsInf(src.IntegratedLUFS, -1) {
		lufs = src.IntegratedLUFS + gainDB
	}
	truePeakDB = src.TruePeakDB + gainDB
	if limited {
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

// silenceMapFile is the silence map's name within the job directory.
const silenceMapFile = "silence.json"

// runAnalyze measures the source and stores the numbers, optionally mapping
// its silences from the same decode.
func (r *Runner) runAnalyze(ctx context.Context, j *Job) error {
	src, err := r.open(ctx, j.Request)
	if err != nil {
		return err
	}
	defer src.Close()
	res, err := r.cfg.Engine.Analyze(ctx, src, src.Ext, waxflow.AnalyzeOptions{
		Progress: r.progressFunc(ctx, j.ID, "analyze"),
		Silence:  j.Request.SilenceOptions(),
	})
	if err != nil {
		return err
	}
	a := analysisOf(res)
	var out *Output
	if res.Silence != nil {
		doc := silenceMapOf(res)
		if out, err = r.writeSilenceMap(j.ID, doc); err != nil {
			return err
		}
		a.Silence = doc.summary()
	}
	r.store.update(j.ID, false, func(job *Job) {
		job.Analysis = a
		if out != nil {
			job.Outputs = []Output{*out}
		}
	})
	return nil
}

// runTimeline mints a multi-source timeline, measuring the members that need
// it. The product is the digest, which lives in the timeline store rather
// than in the job directory; re-running from zero re-mints the same digest,
// so the restart contract holds by content-addressing rather than by the
// usual "outputs are recreated whole".
func (r *Runner) runTimeline(ctx context.Context, j *Job) error {
	if r.cfg.MintTimeline == nil {
		return waxerr.New(waxerr.CodeInvalidRequest, "jobs: timelines are not configured on this daemon")
	}
	tl, err := r.cfg.MintTimeline(ctx, j.Request.Srcs, j.Request.Spans, j.Request.CrossfadeSeconds, r.progressFunc(ctx, j.ID, "measure"))
	if err != nil {
		return err
	}
	r.store.update(j.ID, false, func(job *Job) { job.Timeline = tl })
	return nil
}

// silenceMapOf renders the engine's map into the silence.json document.
func silenceMapOf(res *waxflow.AnalyzeResult) SilenceMap {
	s := res.Silence
	rate := res.Format.Rate
	seconds := func(frames int64) float64 {
		if rate <= 0 {
			return 0
		}
		return float64(frames) / float64(rate)
	}
	doc := SilenceMap{
		SchemaVersion:   1,
		Version:         s.Version,
		ThresholdDB:     s.ThresholdDB,
		MinSeconds:      s.MinDuration.Seconds(),
		Rate:            rate,
		Samples:         res.Samples,
		DurationSeconds: seconds(res.Samples),
		Spans:           make([]SilenceMapSpan, len(s.Spans)),
		Dropped:         s.Dropped,
		DroppedSeconds:  seconds(s.DroppedSamples),
		TotalSeconds:    seconds(s.TotalSamples),
	}
	for i, sp := range s.Spans {
		doc.Spans[i] = SilenceMapSpan{
			FromSample:  sp.From,
			ToSample:    sp.To,
			FromSeconds: seconds(sp.From),
			ToSeconds:   seconds(sp.To),
		}
	}
	return doc
}

// summary reduces the document to the headline the job carries inline.
//
// It projects the map rather than recomputing from the measurement, so the
// two cannot disagree about what was found: a summary that contradicted its
// own map would be the worst kind of bug here, since the summary is what a
// caller reads first and the map is what it acts on.
func (m SilenceMap) summary() *SilenceSummary {
	return &SilenceSummary{
		Version:        m.Version,
		ThresholdDB:    m.ThresholdDB,
		MinSeconds:     m.MinSeconds,
		Spans:          len(m.Spans),
		Dropped:        m.Dropped,
		DroppedSeconds: m.DroppedSeconds,
		TotalSeconds:   m.TotalSeconds,
	}
}

// writeSilenceMap writes the document into the job directory, whole, like
// every other job output, so the restart contract ("outputs live inside the
// job's own directory and are recreated whole") covers it with nothing
// added.
func (r *Runner) writeSilenceMap(id string, doc SilenceMap) (*Output, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "jobs: encoding the silence map", err)
	}
	if err := os.WriteFile(filepath.Join(r.store.jobDir(id), silenceMapFile), data, 0o600); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: writing the silence map", err)
	}
	return &Output{
		File:      silenceMapFile,
		MediaType: "application/json",
		Container: "json",
		Bytes:     int64(len(data)),
		// Samples and Rate stay zero: this output is not audio. The map's
		// own document carries the analyzed length.
	}, nil
}

// SilenceOptions maps the request's silence fields onto engine options,
// nil when the request asked for no map. The server calls it at creation to
// validate, and the runner to execute, so the two cannot disagree about
// what a request means.
func (req Request) SilenceOptions() *waxflow.SilenceOptions {
	if !req.Silence {
		return nil
	}
	return &waxflow.SilenceOptions{
		ThresholdDB: req.SilenceThresholdDB,
		MinDuration: secondsToDuration(req.SilenceMinSeconds),
	}
}

// secondsToDuration converts a wire seconds field to a Duration without
// risking the out-of-range float-to-int conversion, whose result Go leaves
// implementation-defined: a value that large could otherwise wrap back
// inside a policy bound instead of failing it. Saturating instead maps
// every out-of-range input to one the bound rejects on sight, and NaN with
// it.
func secondsToDuration(seconds float64) time.Duration {
	const maxSeconds = float64(math.MaxInt64) / float64(time.Second)
	switch {
	case math.IsNaN(seconds):
		return -1
	case seconds >= maxSeconds:
		return time.Duration(math.MaxInt64)
	case seconds <= -maxSeconds:
		return time.Duration(math.MinInt64)
	}
	return time.Duration(seconds * float64(time.Second))
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
	src, err := r.open(ctx, req)
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
			// Measure after the encode's downmix so the gain (and the
			// derived-output RG for fragmented MP4) sits on the audio the
			// encode meters. The stored Analysis.Channels then reports this
			// measurement basis, not the source count, which is what makes
			// IntegratedLUFS + AppliedGainDB land on the RG reference.
			Channels: req.Channels,
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

	// Probe before the options rather than after: it fixes the output's
	// identity fields (media type, container) for the plan below, and it
	// carries the container's own chapters, which the options need. Creation
	// validated this same plan, so failures here mean the source itself
	// changed shape.
	probe, err := r.cfg.Engine.Probe(src, src.Ext, nil)
	if err != nil {
		return err
	}

	dropRG := gainDB != 0 || analyzeLoudness
	tagInfo := info
	if dropRG {
		tagInfo = meta.WithoutReplayGain(info)
	}
	opts := req.TranscodeOptions(gainDB, r.cfg.Profile)
	opts.Tags = meta.FullTags(tagInfo)
	// The container's own chapters are the floor, and the mapper's win when a
	// mapper is wired and read some: a richer tag library may know forms the
	// container package does not. GET /probe resolves the same two sources the
	// same way, which is the point of matching it here: a caller that probes a
	// file and then transcodes it must not be told about chapters the output
	// then lacks. Reading them only from the mapper is what made a daemon
	// embedded by anyone who injects none write chapterless outputs, for a
	// file whose chapters the demuxer had already parsed off the header.
	opts.Chapters = probe.Chapters
	if info != nil {
		if len(info.Chapters) > 0 {
			opts.Chapters = info.Chapters
		}
		if p := info.FrontPicture(); p != nil {
			opts.Art = &container.Picture{MIME: p.MIME, Data: p.Data}
		}
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

	outName := "out." + outputExt(opts)
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
			// encoder's fraction of a dB for AAC. The true-peak limiter
			// engages for positive gain or a downmix (its matrix can sum
			// past unity), and caps the derived peak at its ceiling; pass
			// that so a negative-gain downmix's peak is capped rather than
			// reading back the raw fold's overshoot.
			limited := gainDB > 0 || opts.Dynamics != gain.PresetOff ||
				(req.Channels != 0 && req.Channels < probe.Default().Fmt.Channels)
			outLUFS, outTP = deriveOutputLoudness(srcRes, gainDB, limited)
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
		job.Outputs = []Output{{
			File:      outName,
			MediaType: plan.MediaType,
			Container: plan.Container,
			Bytes:     fi.Size(),
			Samples:   res.Samples,
			Rate:      plan.Format.Rate,
		}}
		job.Analysis = analysis
	})
	return nil
}

// outputExt is the extension a job's product is written with.
//
// It is asked of the requested format and container, never of plan.Container,
// which is the trap: the plan reports the row's own name when nothing
// overrode it, so naming a file by it writes out.alac, and reports the
// override when something did, so an mp4-family merge writes out.progressive.
// Neither is a file a player will open, and progressive is not even a
// different container, it is the same MP4 with its boxes flattened.
//
// Output.Container goes on carrying the container name; only the filename
// takes the extension. The two answer different questions and deriving one
// from the other at each use is what keeps them from drifting apart on disk.
//
// Every caller is past its plan, and a plan is refused for a format with no
// row, so the bin fallback cannot fire here.
func outputExt(opts waxflow.TranscodeOptions) string {
	return waxflow.OutputExt(opts.Format, opts.Container)
}

// writeMedia transcodes med into the job directory under name and describes
// what landed. It is the tail every audio-writing job shares: create, run,
// sync, close, stat.
//
// The sync and the explicit close are not ceremony. A job's product outlives
// the process that made it, and its size is recorded in job.json from the stat
// below; a deferred close would let that stat run against a file whose last
// write is still in the writer, and a close error (a full disk reporting late)
// would be discarded exactly when it is the whole story.
func (r *Runner) writeMedia(ctx context.Context, j *Job, med format.Media, name string,
	opts waxflow.TranscodeOptions, mediaType string) (*Output, error) {
	path := filepath.Join(r.store.jobDir(j.ID), name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: creating output", err)
	}
	defer f.Close()
	res, err := r.cfg.Engine.TranscodeMedia(ctx, med, f, opts)
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: output sync", err)
	}
	if err := f.Close(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: output close", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "jobs: output stat", err)
	}
	return &Output{
		File:      name,
		MediaType: mediaType,
		Container: res.Container,
		Bytes:     fi.Size(),
		Samples:   res.Samples,
		Rate:      res.Format.Rate,
	}, nil
}

// mp4ProgressiveContainer mirrors the server's container override for the flat
// (moov+mdat) MP4 form: validateMergeRequest stamps it on an mp4-family merge's
// request, and it is the one output shape that carries a QuickTime chapter text
// track. It is a wire string, spelled the same by the CLI's --container flag,
// so it is re-declared here rather than exported from the engine, matching the
// server's own re-declaration.
const mp4ProgressiveContainer = "progressive"

// runMerge concatenates the request's members into one output.
//
// It is the timeline primitive with a file on the end, and that is the point
// rather than a convenience: the plan comes from ConcatTrack and the samples
// come from Concat, the same two functions the HLS timeline uses, so the seam
// between two chapters of an audiobook is gapless for the same reason and by
// the same code as the seam between two tracks of a play queue. Nothing here
// knows how a seam works.
//
// The members are measured first and opened later, in two passes. The measure
// pass needs every member's authoritative length before anything can be
// planned (a prefix sum has no partial answer), while the open pass is
// Concat's own, one member at a time, so a 500-chapter merge holds one file
// descriptor rather than 500.
func (r *Runner) runMerge(ctx context.Context, j *Job) error {
	req := j.Request
	if r.cfg.MeasureTrack == nil {
		return waxerr.New(waxerr.CodeInvalidRequest, "jobs: merges are not configured on this daemon")
	}
	// An mp4-family merge writes a QuickTime chapter text track, one chapter per
	// member; the server picked this container for exactly that shape, the only
	// one that carries the track. Only then are per-member titles read and
	// chapters computed, so a non-mp4 merge pays no per-member metadata reads
	// and gets no ignored chapters.
	wantChapters := req.Container == mp4ProgressiveContainer
	measure := r.progressFunc(ctx, j.ID, "measure")
	tracks := make([]container.Track, len(req.Srcs))
	tagTitles := make([]string, len(req.Srcs))
	for i := range req.Srcs {
		if err := ctx.Err(); err != nil {
			return waxerr.Wrap(waxerr.CodeCanceled, "merge canceled", err)
		}
		f, err := r.openMember(ctx, req, i)
		if err != nil {
			return err
		}
		track, err := r.cfg.MeasureTrack(f)
		if err != nil {
			f.Close()
			return err
		}
		tracks[i] = track
		// Read the member's TITLE while the handle is open, for the chapter it
		// will stamp. Best-effort: a read error warns and the title falls
		// through to the request field or the generated fallback, never failing
		// the merge (the metadata post-pass is best-effort too). The container
		// probe exposes chapters but not tags, so a tag title needs a mapper; a
		// daemon with none simply has no tag titles.
		//
		// Skipped when the request already names this member, since mergeTitle
		// prefers a non-empty request title: the tag would be parsed only to be
		// discarded. A fully-titled merge then does no per-member metadata reads.
		haveReqTitle := i < len(req.MemberTitles) && req.MemberTitles[i] != ""
		if wantChapters && !haveReqTitle && r.cfg.Meta != nil {
			if info, err := r.cfg.Meta.Read(ctx, f, f.Ext, meta.ReadOptions{}); err != nil {
				r.warn(j.ID, fmt.Sprintf("member %d: reading title: %v", i, err))
			} else if vs := info.Tags["TITLE"]; len(vs) > 0 {
				tagTitles[i] = vs[0]
			}
		}
		f.Close()
		measure(int64(i+1), int64(len(req.Srcs)))
	}

	members := make([]waxflow.ConcatSource, len(tracks))
	for i := range tracks {
		members[i] = waxflow.ConcatSource{
			Track: tracks[i],
			Open:  func() (format.Media, error) { return r.openMemberMedia(ctx, req, i) },
		}
	}
	// The daemon's profile and no crossfade, which is what the server plans a
	// merge's envelope with (its timelineOptions). The two are separate
	// literals because this package cannot see the server's, and they agree
	// because a merge has no crossfade to disagree about: nothing on the wire
	// asks for one. Thread a crossfade to a job and this is the second place it
	// has to reach, or the 201 validates an envelope of sum-(N-1)X while this
	// delivers the full sum.
	//
	// One copts (not one layout) is handed to both Concat and the chapter
	// offsets: ConcatOptions' convention is that the plan and the run take the
	// same options, and here Concat is the run and the offsets must match it.
	// Both walk concatLayout independently over the same tracks and copts, so
	// the seams ConcatBoundaries reports are exactly the ones Concat plays. The
	// second walk is header arithmetic with no I/O, once per job; the funnel is
	// what makes it agree rather than merely happen to.
	copts := waxflow.ConcatOptions{Profile: r.cfg.Profile}
	med, err := waxflow.Concat(members, copts)
	if err != nil {
		return err
	}
	defer med.Close()

	// Gain is zero and there are no source tags: a merge takes neither field,
	// since both are per-track answers and this has N tracks in and one file
	// out. The server refuses them at creation; this is only where that shows.
	opts := req.TranscodeOptions(0, r.cfg.Profile)
	opts.Progress = r.progressFunc(ctx, j.ID, "transcode")
	if wantChapters {
		// One chapter per member, at the member's start on the concatenated
		// timeline. The offsets come from the same ConcatBoundaries the timeline
		// path plans through, so a marker lands exactly where its seam does.
		chapters, err := mergeChapters(tracks, copts, req.MemberTitles, tagTitles)
		if err != nil {
			return err
		}
		opts.Chapters = chapters
	}
	plan, err := r.cfg.Engine.PlanTranscode(med.Info().Default(), opts)
	if err != nil {
		return err
	}
	out, err := r.writeMedia(ctx, j, med, "out."+outputExt(opts), opts, plan.MediaType)
	if err != nil {
		return err
	}
	r.store.update(j.ID, false, func(job *Job) { job.Outputs = []Output{*out} })
	return nil
}

// mergeChapters is a merge's QuickTime chapter list: one chapter per member, at
// the member's start on the concatenated timeline, titled by mergeTitle.
//
// The offsets come from ConcatBoundaries, the same walk the timeline path plans
// through, converted to a clock with waxflow.SampleTime (the overflow-safe
// converter a long audiobook needs; a naive n*Second/rate overflows past about
// 53 hours). A merge always butt-joins, so the offsets are the plain prefix
// sum. Chapters are emitted start-only (End zero): the muxer gives each chapter
// the next one's start as its end, and the movie end for the last, so a
// start-only list is correct for every member, not only the tail.
func mergeChapters(tracks []container.Track, copts waxflow.ConcatOptions, reqTitles, tagTitles []string) ([]container.Chapter, error) {
	boundaries, env, err := waxflow.ConcatBoundaries(tracks, copts)
	if err != nil {
		return nil, err
	}
	chapters := make([]container.Chapter, len(boundaries))
	for i, b := range boundaries {
		chapters[i] = container.Chapter{
			Start: waxflow.SampleTime(b.OffsetSamples, env.Rate),
			Title: mergeTitle(i, reqTitles, tagTitles),
		}
	}
	return chapters, nil
}

// mergeTitle resolves member i's chapter title by the A18 precedence: the
// request's title if non-empty, else the member's TITLE tag, else a generated
// "Chapter N" (1-based). An empty request entry does not force a blank title;
// the precedence cannot express "deliberately blank", so it falls through to
// the tag or the fallback.
func mergeTitle(i int, reqTitles, tagTitles []string) string {
	if i < len(reqTitles) && reqTitles[i] != "" {
		return reqTitles[i]
	}
	if i < len(tagTitles) && tagTitles[i] != "" {
		return tagTitles[i]
	}
	return fmt.Sprintf("Chapter %d", i+1)
}

// splitProgressAt places a piece's own progress on the source's timeline: the
// piece starts at source sample base and covers span of them, and the encoder
// has read done of a projected pieceTotal.
//
// The piece's fraction is scaled onto its span rather than added to its start,
// because the two are not the same unit: the engine counts encoder-input
// samples, which are the source's own only while nothing resamples. Adding
// them raw would run the bar a resample ratio past the next piece's start and
// step it backwards on arrival.
func splitProgressAt(base, span, done, pieceTotal int64) int64 {
	if span <= 0 || pieceTotal <= 0 {
		// A source that declares no length leaves the last piece open-ended,
		// and its start is then the only honest thing to report.
		return base
	}
	// Clamped because the projection is the muxer's estimate: a piece that
	// encodes a few samples past it must not push the bar into the next one.
	return base + int64(float64(span)*min(1, float64(done)/float64(pieceTotal)))
}

// splitLength is the source length a split's cuts are resolved against: the
// header's own, measured only when the header declares none.
//
// Filling an absent length is the whole of the measuring, and overriding a
// declared one is deliberately not. waxflow.Slice opens its own Media per
// piece and bounds each explicit span end through SpanTrack against that
// Media's declared track, with no seam to hand it a number measured out here.
// A length measured in defiance of a header could then only widen what this
// accepts into cuts Slice refuses anyway, moving a clear refusal at the funnel
// three layers down for no gain: a source that under-declares keeps its extra
// audio unaddressable either way, which is what a lying header buys and the
// position SpanTrack already takes.
//
// An absent length carries no such conflict, since SpanTrack bounds nothing
// against a track that declares nothing. There the measurement adds the
// refusal that was missing rather than contradicting one, and that refusal is
// the point: without it every cut is accepted, and the pieces before the one
// that cannot exist are already written when it fails the job, orphaned in the
// job directory.
//
// The server holds a split's cuts to this same rule at creation. What the two
// share is that rule, not a number: each fills an absent length and neither
// overrides a declared one, so they answer alike without consulting each
// other. The measuring is a memo hit rather than a second walk of the file:
// the pass that measured this source to validate the cuts filled it, keyed by
// source identity.
//
// A daemon with no MeasureTrack leaves an absent length absent rather than
// refusing the split, where a merge refuses outright. The asymmetry is the two
// jobs' own: a merge cannot run on an advisory length at all, since Concat
// holds every member to its track and a total two samples out fails the run,
// while a split's last piece is open-ended and inherits whatever the source
// turns out to hold. Such an embedder keeps the looser bound it already had,
// and nothing can disagree with it: the server always wires the hook, so a nil
// one means the server is not the caller and no validation of these cuts ran
// anywhere.
func (r *Runner) splitLength(src *source.File) (int64, error) {
	info, err := r.cfg.Engine.Probe(src, src.Ext, nil)
	if err != nil {
		return 0, err
	}
	if samples := info.Default().Samples; samples >= 0 || r.cfg.MeasureTrack == nil {
		return samples, nil
	}
	track, err := r.cfg.MeasureTrack(src)
	if err != nil {
		return 0, err
	}
	return track.Samples, nil
}

// runSplit cuts the source at the request's cut points, one output per piece.
//
// Each piece is its own Slice of its own freshly opened Media, rather than one
// Media seeked between pieces. That costs a header parse per piece and buys
// the property the whole feature rests on: a slice's sample 0 is the source's
// sample from with nothing primed from before it, so a lossless split at the
// source rate rejoins bit for bit. A reused, seeked Media would leave the
// answer depending on what the demuxer happened to hold.
func (r *Runner) runSplit(ctx context.Context, j *Job) error {
	req := j.Request
	src, err := r.open(ctx, req)
	if err != nil {
		return err
	}
	defer src.Close()
	srcSamples, err := r.splitLength(src)
	if err != nil {
		return err
	}
	spans, err := req.SplitSpans(srcSamples)
	if err != nil {
		return err
	}

	opts := req.TranscodeOptions(0, r.cfg.Profile)
	outs := make([]Output, 0, len(spans))
	// Every piece is the same format through the same options, so the pieces
	// differ by index alone; a plan is still made per piece, since only a
	// piece's own track can be planned against.
	ext := outputExt(opts)
	// One progress bar over the whole split rather than one per piece, on the
	// source's own timeline: the spans partition the source, so a piece's
	// start plus how far into that piece the encoder has read is the split's
	// position. Pieces are not the same length, so counting pieces would jump
	// the bar in N uneven steps, and each piece's own samples would reset it N
	// times. A source that declares no length leaves the total 0, which
	// progressFunc renders as an unknown percent rather than a wrong one.
	report := r.progressFunc(ctx, j.ID, "transcode")
	total := max(srcSamples, 0)
	for i, sp := range spans {
		// The piece's end is its own except for the last, which is ToEnd:
		// whatever the source turns out to hold, which is what total names.
		end := sp[1]
		if end == waxflow.ToEnd {
			end = srcSamples
		}
		base, span := sp[0], end-sp[0]
		// The hook still fires per chunk, and that is not a waste even when
		// the bar barely moves: progressFunc is also where a job yields to a
		// saturated live pool, and that has to be asked per chunk.
		opts.Progress = func(done, pieceTotal int64) {
			report(splitProgressAt(base, span, done, pieceTotal), total)
		}
		med, err := r.cfg.Engine.OpenStream(src, src.Ext)
		if err != nil {
			return err
		}
		// Slice takes ownership, so a failure here closes what it did not take.
		sl, err := waxflow.Slice(med, sp[0], sp[1])
		if err != nil {
			med.Close()
			return err
		}
		plan, err := r.cfg.Engine.PlanTranscode(sl.Info().Default(), opts)
		if err != nil {
			sl.Close()
			return err
		}
		out, err := r.writeMedia(ctx, j, sl, fmt.Sprintf("out.%d.%s", i, ext), opts, plan.MediaType)
		sl.Close()
		if err != nil {
			return err
		}
		outs = append(outs, *out)
		if end >= 0 {
			report(end, total)
		}
	}
	r.store.update(j.ID, false, func(job *Job) { job.Outputs = outs })
	return nil
}
