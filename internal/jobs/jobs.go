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
	"time"
)

// Type selects what a job does.
type Type string

const (
	// TypeTranscode writes a full-file transcode into the job directory.
	TypeTranscode Type = "transcode"
	// TypeAnalyze measures loudness (EBU R128) and stores the numbers.
	TypeAnalyze Type = "analyze"
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
	Type Type   `json:"type"`
	Src  string `json:"src"`
	// SourceID pins the source identity (size-mtimeNS) at creation; a
	// source that changed before the job ran fails with source-changed
	// rather than silently transcoding different bytes.
	SourceID string `json:"sourceId"`

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
}

// ErrInfo is a terminal failure, in the envelope vocabulary.
type ErrInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Output describes a finished job's product: a transcode's audio file, or
// an analyze job's silence map. GET /jobs/{id}/result serves it.
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
	Output        *Output    `json:"output,omitempty"`
	Analysis      *Analysis  `json:"analysis,omitempty"`
	Progress      *Progress  `json:"progress,omitempty"`
	// Warnings are non-fatal notes (metadata that could not be read or
	// written); the audio outcome is unaffected.
	Warnings []string `json:"warnings,omitempty"`
}

// clone returns an independent copy safe to hand out.
func (j *Job) clone() *Job {
	c := *j
	if j.Started != nil {
		t := *j.Started
		c.Started = &t
	}
	if j.Finished != nil {
		t := *j.Finished
		c.Finished = &t
	}
	if j.Error != nil {
		e := *j.Error
		c.Error = &e
	}
	if j.Output != nil {
		o := *j.Output
		c.Output = &o
	}
	if j.Analysis != nil {
		a := *j.Analysis
		if a.Silence != nil {
			s := *a.Silence
			a.Silence = &s
		}
		c.Analysis = &a
	}
	if j.Progress != nil {
		p := *j.Progress
		c.Progress = &p
	}
	c.Warnings = append([]string(nil), j.Warnings...)
	return &c
}
