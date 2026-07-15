// Package jobs is the async job store and runner: full-file transcodes
// and loudness analyses that outlive any request, with restart-safe
// file-backed state under dataDir/jobs.
//
// Restart safety is deliberately simple (the plan's "no mid-encode state
// resume"): a completed job's results survive restarts; an incomplete
// job restarts cleanly from zero on boot, idempotent because its outputs
// live inside its own directory and are recreated whole. HLS segments
// remain the incremental path for resumable delivery.
package jobs

import (
	"fmt"
	"slices"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/waxerr"
)

// Type selects what a job does.
type Type string

const (
	// TypeTranscode writes a full-file transcode into the job directory.
	TypeTranscode Type = "transcode"
	// TypeAnalyze measures loudness (EBU R128) and stores the numbers.
	TypeAnalyze Type = "analyze"
	// TypeTimeline mints a multi-source HLS timeline, measuring the members
	// whose headers cannot declare an exact length. Unlike the others it is
	// not created through POST /jobs: POST /hls/timeline answers 201 with
	// the digest when no member needs measuring, and falls back to one of
	// these when one does.
	TypeTimeline Type = "timeline"
	// TypeMerge concatenates Srcs into one output file. It is the timeline
	// primitive pointed at a file rather than at a segment ladder: the same
	// ConcatTrack plans it and the same Concat runs it, so a merged
	// audiobook and an HLS play queue are gapless for one reason rather
	// than two.
	TypeMerge Type = "merge"
	// TypeSplit cuts one source at Cuts into N output files, each a Slice of
	// the source. It is Merge's inverse, and deliberately so: a split to a
	// lossless output at the source rate rejoins bit for bit, which is the
	// property the pair is worth having for.
	TypeSplit Type = "split"
)

// State is a job's lifecycle position. Queued and running reset to
// queued on restart; the other three are terminal.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

// Terminal reports whether s is a final state.
func (s State) Terminal() bool {
	return s == StateDone || s == StateFailed || s == StateCanceled
}

// Request is a job's parameters, transport-free: the server parses and
// validates the HTTP body into it, and the runner rebuilds the engine
// options from it after a restart.
type Request struct {
	Type Type `json:"type"`
	// Src is the single source a transcode, an analyze, or a split reads.
	// Empty for a merge, which names its members in Srcs instead.
	Src string `json:"src"`
	// SourceID pins the source identity (size-mtimeNS) at creation; a
	// source that changed before the job ran fails with source-changed
	// rather than silently transcoding different bytes.
	SourceID string `json:"sourceId"`
	// SourceIDs pins Srcs' identities the same way, one per member and in
	// the same order. It is a separate field rather than a widened SourceID
	// because the two guard different shapes and both must stay honest: a
	// merge has no single source to pin, and a single-source job has no
	// member list to be off by one against.
	//
	// Like SourceID it is server-computed and absent from the wire body. A
	// client that could send member identities could send the ones it wished
	// were true, which is the whole guarantee.
	SourceIDs []string `json:"sourceIds,omitempty"`

	// Transcode parameters, mirroring the /stream surface.
	Format    string `json:"format,omitempty"`
	Container string `json:"container,omitempty"`
	Rate      int    `json:"rate,omitempty"`
	Channels  int    `json:"ch,omitempty"`
	Bits      int    `json:"bits,omitempty"`
	// Bitrate is the lossy output bit rate in kbit/s.
	Bitrate int `json:"bitrate,omitempty"`
	// Gain is the gain parameter as /stream spells it (off, track,
	// album, or a dB number); empty means the daemon default. Ignored
	// when Loudness is analyze.
	Gain string `json:"gain,omitempty"`
	// Loudness "analyze" selects the two-pass form: measure the source,
	// apply the exact ReplayGain-reference gain, and write measured
	// ReplayGain tags on the output.
	Loudness string `json:"loudness,omitempty"`
	// FLACLevel is the FLAC compression level (1..8, -1 for 0); 0 keeps
	// the encoder default.
	FLACLevel int `json:"flacLevel,omitempty"`

	// Silence adds the silence map to an analyze job, from the same
	// decode the loudness measurement runs on. Analyze-only.
	Silence bool `json:"silence,omitempty"`
	// SilenceThresholdDB is the silence threshold in dBFS; 0 means the
	// engine default. Raw rather than a named level because the right
	// value is a property of the content, which the caller knows and the
	// daemon does not.
	SilenceThresholdDB float64 `json:"silenceThresholdDb,omitempty"`
	// SilenceMinSeconds is the shortest span worth reporting; 0 means the
	// engine default.
	SilenceMinSeconds float64 `json:"silenceMinSeconds,omitempty"`

	// Srcs are the members a timeline or a merge concatenates, in order. Src
	// and SourceID stay empty for both, since neither has a single source.
	// Where the two pin those members differs: a timeline pins them inside
	// the digest it mints, while a merge pins them in SourceIDs, because it
	// mints nothing and its product is a file.
	Srcs []string `json:"srcs,omitempty"`

	// Cuts are a split job's cut points, as sample offsets on the source's
	// own timeline, strictly ascending. Split-only.
	//
	// They are interior points, so N cuts make N+1 pieces: piece i runs
	// [Cuts[i-1], Cuts[i]) with 0 implied before the first and the source's
	// end implied after the last. A cut is where the tape is cut, which is
	// the only reading under which every offset in the list does something;
	// a leading 0 or a trailing end-of-source would each ask for an empty
	// piece, and are refused rather than tolerated (SplitSpans).
	//
	// Samples, not seconds, for the reason a span is: a cut point declares
	// which samples are this piece, so it is content identity, and 245.32 s
	// at 44100 floors to one sample short of the boundary a CUE sheet's
	// CD-frame arithmetic names exactly.
	Cuts []int64 `json:"cuts,omitempty"`
}

// SplitSpans resolves Cuts into the [from, to) span of each piece, in order,
// against a source of total samples (negative when the headers do not declare
// one). to is waxflow.ToEnd for the last piece, which inherits whatever the
// source turns out to hold rather than being held to a declaration.
//
// It is the single funnel for the cut arithmetic, in the SpanTrack spirit and
// for the same reason: the server validates cuts at creation and the runner
// cuts by them at run, and the two must not be able to disagree about which
// samples are piece 3. Every rule about what a cut list may say lives here.
func (req Request) SplitSpans(total int64) ([][2]int64, error) {
	if len(req.Cuts) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"jobs: a split needs at least one cut point")
	}
	// prev is both halves of the walk: the cut a cut must advance past, and
	// the sample the piece it opens starts at. They are the same number, so
	// checking and cutting in one pass is not a fusion of two loops that
	// happen to agree, it is the arithmetic saying so once.
	spans := make([][2]int64, 0, len(req.Cuts)+1)
	prev := int64(0)
	for i, c := range req.Cuts {
		switch {
		case c <= prev:
			// One message for "not ascending" and for a leading 0, because
			// they are one rule: every cut opens a piece, so a cut that does
			// not advance past the one before it (or past the implied 0) is
			// asking for an empty one.
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"jobs: cut %d is at sample %d, which does not advance past %d; "+
					"cuts are interior points, strictly ascending", i, c, prev))
		case total >= 0 && c >= total:
			// Refused rather than clamped, the call SpanTrack makes about a
			// span past the end: a cut list that overshoots does not describe
			// this source (a CUE sheet paired with the wrong rip), and
			// clamping would hand back fewer or emptier pieces than the
			// caller believes it asked for, with nothing to notice it by.
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"jobs: cut %d is at sample %d, at or past the source's %d samples", i, c, total))
		}
		spans = append(spans, [2]int64{prev, c})
		prev = c
	}
	return append(spans, [2]int64{prev, waxflow.ToEnd}), nil
}

// ErrInfo is a terminal failure, in the envelope vocabulary.
type ErrInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Output describes one of a finished job's products: a transcode's or a
// merge's audio file, one piece of a split, or an analyze job's silence map.
// GET /jobs/{id}/result/{n} serves it.
type Output struct {
	// File is the output's name within the job directory.
	File      string `json:"file"`
	MediaType string `json:"mediaType"`
	Container string `json:"container"`
	Bytes     int64  `json:"bytes"`
	// Samples and Rate describe an audio product; both are 0 for an
	// output that is not audio, such as the silence map, whose own
	// document carries the analyzed length instead.
	Samples int64 `json:"samples"`
	Rate    int   `json:"rate"`
}

// SilenceSummary is the silence map's headline, small enough to live
// inline on the job. The map itself is an output file: a 40-hour audiobook
// pausing every 30 s is ~4800 spans, and job.json is broadcast whole on
// every SSE progress event and returned in full by GET /jobs.
type SilenceSummary struct {
	// Version is the detector revision; a caller caching the map needs it
	// to know when the map went stale.
	Version     string  `json:"version"`
	ThresholdDB float64 `json:"thresholdDb"`
	MinSeconds  float64 `json:"minSeconds"`
	// Spans is the number of silences found.
	Spans int `json:"spans"`
	// Dropped counts runs discarded for falling short of MinSeconds. Read
	// it with DroppedSeconds, never alone: ordinary audio dips under any
	// threshold at every zero crossing, so a clean source with well-formed
	// silences still drops hundreds of one-sample runs per second.
	Dropped int `json:"dropped"`
	// DroppedSeconds is the summed length of those runs, and it is the
	// diagnostic. Against DurationSeconds it says how much of the source
	// sat below the threshold without ever staying there long enough to
	// report: a fraction of a percent for a healthy source however large
	// Dropped grows, and a sizeable share of the stream when a noise floor
	// sits near the threshold, crossing it repeatedly and fragmenting one
	// long silence into many short ones that are each then discarded. That
	// reports a plainly quiet source as having no silence at all, and it
	// means the threshold is wrong for this source rather than that the
	// source is loud.
	DroppedSeconds float64 `json:"droppedSeconds"`
	// TotalSeconds is the summed length of the kept spans, which is what a
	// "time saved by trimming" figure reads directly.
	TotalSeconds float64 `json:"totalSeconds"`
}

// SilenceMap is the silence.json document: the full span map, written into
// the job directory rather than carried on the job.
type SilenceMap struct {
	SchemaVersion int     `json:"schemaVersion"`
	Version       string  `json:"version"`
	ThresholdDB   float64 `json:"thresholdDb"`
	MinSeconds    float64 `json:"minSeconds"`
	// Rate and Samples describe the analyzed audio, so a consumer can
	// convert the sample fields without a second request.
	Rate            int              `json:"rate"`
	Samples         int64            `json:"samples"`
	DurationSeconds float64          `json:"durationSeconds"`
	Spans           []SilenceMapSpan `json:"spans"`
	Dropped         int              `json:"dropped"`
	DroppedSeconds  float64          `json:"droppedSeconds"`
	TotalSeconds    float64          `json:"totalSeconds"`
}

// SilenceMapSpan is one silence, carried in both samples and seconds.
// Seconds alone would be defensible (float64 resolves a 40-hour file to
// ~1e-11 s), but a span map's likely next stop is a cut-point list, and
// cut points are source-sample addressed: emitting the samples the engine
// already holds exactly removes any need to prove that round trip is
// lossless. To is exclusive.
type SilenceMapSpan struct {
	FromSample  int64   `json:"fromSample"`
	ToSample    int64   `json:"toSample"`
	FromSeconds float64 `json:"fromSeconds"`
	ToSeconds   float64 `json:"toSeconds"`
}

// Analysis is a loudness measurement. The peak and loudness fields are
// pointers because digital silence measures negative infinity, which
// JSON cannot carry: null means silence.
type Analysis struct {
	IntegratedLUFS  *float64 `json:"integratedLufs"`
	LoudnessRange   float64  `json:"loudnessRange"`
	TruePeakDB      *float64 `json:"truePeakDb"`
	SamplePeakDB    *float64 `json:"samplePeakDb"`
	Samples         int64    `json:"samples"`
	Rate            int      `json:"rate"`
	Channels        int      `json:"channels"`
	DurationSeconds float64  `json:"durationSeconds"`
	// AppliedGainDB is the exact gain a loudness:analyze transcode
	// applied (the RG2 reference minus the measured source loudness).
	AppliedGainDB *float64 `json:"appliedGainDb,omitempty"`
	// ReplayGain values written on the output (loudness:analyze).
	ReplayGainTrackGain string `json:"replaygainTrackGain,omitempty"`
	ReplayGainTrackPeak string `json:"replaygainTrackPeak,omitempty"`
	// Silence summarizes the silence map, present when the request asked
	// for one. The spans themselves are the job's output file.
	Silence *SilenceSummary `json:"silence,omitempty"`
}

// Timeline is a timeline job's product: the digest a client puts in a tl=
// parameter, and what it names.
//
// It is not an Output, because it is not a file in the job directory: the
// timeline lives in the timeline store, under the digest that is its
// identity. The restart contract still covers it, for the same reason by a
// different route: content-addressing makes a re-mint write the same digest,
// so re-running the job from zero is idempotent.
type Timeline struct {
	// Tl is the timeline's digest.
	Tl string `json:"tl"`
	// Members is how many sources it holds.
	Members int `json:"members"`
	// DurationSeconds is the concatenated timeline's length.
	DurationSeconds float64 `json:"durationSeconds"`
}

// Progress is the running job's position, updated in memory and
// broadcast to event subscribers; it is persisted only incidentally on
// state changes.
type Progress struct {
	// Phase is analyze, transcode, or finalize.
	Phase string `json:"phase"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	// Percent is -1 when the total is unknown.
	Percent float64 `json:"percent"`
}

// SchemaVersion is the job.json version this build writes, and the only one
// it reads: a job.json naming any other version is refused by the store, which
// quarantines the directory rather than deleting it.
//
// 2 replaced the single Output field with the Outputs list, a different JSON
// key. There is no migration because there is nothing released to migrate
// from, and a migration for a schema that never shipped is code that can only
// rot. Refusing is what keeps the alternative from happening quietly: a v1
// file still parses as a valid job whose product list is simply empty, so
// without the bump a finished output would sit on disk unreachable, its job
// terminal and therefore never requeued.
const SchemaVersion = 2

// Job is one job's full state: the job.json shape and the wire shape are
// the same document.
type Job struct {
	SchemaVersion int        `json:"schemaVersion"`
	ID            string     `json:"id"`
	Type          Type       `json:"type"`
	State         State      `json:"state"`
	Request       Request    `json:"request"`
	Created       time.Time  `json:"created"`
	Started       *time.Time `json:"started,omitempty"`
	Finished      *time.Time `json:"finished,omitempty"`
	Error         *ErrInfo   `json:"error,omitempty"`
	// Outputs are the job's product files, in order, and the index is the
	// one GET /jobs/{id}/result/{n} takes. It is a list rather than the
	// single output the other three types have, because a split has N: one
	// output plus a count would be the same list with a way to be
	// inconsistent, and every consumer would still have to handle N.
	Outputs  []Output  `json:"outputs,omitempty"`
	Analysis *Analysis `json:"analysis,omitempty"`
	Timeline *Timeline `json:"timeline,omitempty"`
	Progress *Progress `json:"progress,omitempty"`
	// Warnings are non-fatal notes (metadata that could not be read or
	// written); the audio outcome is unaffected.
	Warnings []string `json:"warnings,omitempty"`
}

// clone returns an independent copy safe to hand out.
//
// The struct copy is not a deep copy, and every reference field below is here
// because of that. Request in particular used to be safe to copy by value and
// no longer is: Srcs, SourceIDs, and Cuts are slices, so a shallow copy would
// hand a caller the stored job's own backing arrays. TestJobCloneIsDeep is the
// guard, and its checks are hand-written because what they must catch is a
// field that was added and forgotten here: a reflective deep-copy check would
// be this function written a second time.
func (j *Job) clone() *Job {
	c := *j
	c.Request.Srcs = slices.Clone(j.Request.Srcs)
	c.Request.SourceIDs = slices.Clone(j.Request.SourceIDs)
	c.Request.Cuts = slices.Clone(j.Request.Cuts)
	// Output holds no reference field of its own, so cloning the slice is
	// the whole of it; a pointer added to Output would need a loop here.
	c.Outputs = slices.Clone(j.Outputs)
	c.Timeline = clonePtr(j.Timeline)
	c.Started = clonePtr(j.Started)
	c.Finished = clonePtr(j.Finished)
	c.Error = clonePtr(j.Error)
	c.Progress = clonePtr(j.Progress)
	if j.Analysis != nil {
		// Analysis is the one struct here that holds pointers of its own, so
		// copying it is not the whole of it: the loudness and peak fields are
		// pointers to carry the negative infinity of digital silence, and the
		// summary is one to stay omittable, and all five would otherwise be
		// the stored job's own.
		a := *j.Analysis
		a.IntegratedLUFS = clonePtr(a.IntegratedLUFS)
		a.TruePeakDB = clonePtr(a.TruePeakDB)
		a.SamplePeakDB = clonePtr(a.SamplePeakDB)
		a.AppliedGainDB = clonePtr(a.AppliedGainDB)
		a.Silence = clonePtr(a.Silence)
		c.Analysis = &a
	}
	c.Warnings = slices.Clone(j.Warnings)
	return &c
}

// clonePtr copies what p points at, nil through. It is what keeps clone one
// line per pointer field: every such field holds a struct or a number with no
// references below it (Analysis excepted, which clone spells out), so copying
// the pointee is the whole job.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
