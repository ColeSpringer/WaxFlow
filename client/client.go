// Package client is the Go API client for a WaxFlow daemon (WaxSeal
// client/ precedent): thin typed wrappers over the HTTP surface
// (control, playback, timelines, and the jobs lifecycle) plus an
// offline signed-URL mint helper, so the users and CLI never
// reimplement canonicalization or envelope decoding.
//
// Error returns carry waxerr codes decoded from the response envelope,
// so callers classify failures exactly like local engine errors (and the
// CLI's exit-code contract holds across the wire).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/waxerr"
)

// Client talks to one WaxFlow daemon.
type Client struct {
	base   *url.URL
	apiKey string
	// http runs the JSON round trips and stream the streaming opens
	// (Stream, JobResult, JobEvents). Two clients because the right
	// bounds differ by phase, not by taste: a streaming endpoint always
	// answers its headers immediately, so those get a header bound and
	// an unbounded body, while one JSON endpoint (a cold merge create)
	// legitimately holds its headers for minutes under the caller's
	// deadline, so that side cannot carry a header bound at all. See New.
	http   *http.Client
	stream *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying http.Client for every method,
// streaming included (timeouts, proxies). The default clients' bounds
// described on New are replaced wholesale: the caller's policy is
// trusted to be the whole policy.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
			c.stream = h
		}
	}
}

// streamHeaderTimeout bounds the wait for response headers on the
// streaming methods. Every streaming endpoint writes its headers
// immediately, so only a hung intermediary or a dead-but-connected peer
// ever spends this; the bodies stay unbounded, which is the point of
// keeping the bound off the JSON client's side (a cold merge create
// legitimately holds its headers for minutes; see CreateJob).
const streamHeaderTimeout = 30 * time.Second

// New returns a Client for the daemon at baseURL ("http://host:4418").
// apiKey may be empty against a keyless daemon.
//
// The time-bound contract: JSON round trips (every method returning a
// decoded value) are bounded at 30 s each when the caller's ctx carries
// no deadline of its own; a ctx deadline replaces that default rather
// than racing it. The streaming methods (Stream, JobResult, JobEvents)
// have their bodies bounded by the caller's ctx alone, because those
// legitimately outlive any fixed bound: an http.Client.Timeout would
// cover the entire body read and cut a long playback, download, or
// event stream mid-flight, which is why the default clients here carry
// none. The connection phase is not the body, though: dial and TLS
// ride the transport defaults, and the streaming methods additionally
// bound the wait for response headers at 30 s, which every streaming
// endpoint answers immediately and a hung intermediary never does. A
// request ended by its ctx reports canceled, distinguishable by code
// from a daemon that cannot be reached. WithHTTPClient replaces all of
// this with the caller's own policy.
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("client: base URL %q must be scheme://host[:port]", baseURL))
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = streamHeaderTimeout
	c := &Client{base: u, apiKey: apiKey, http: &http.Client{}, stream: &http.Client{Transport: tr}}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Wire types mirror the server's (docs/api.md); the golden fixtures and
// the server suite's job-mirror round trips pin both sides against
// drift.

// VersionInfo is the GET /version body.
type VersionInfo struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
}

// ProbeInfo is the GET /probe body.
type ProbeInfo struct {
	SchemaVersion int          `json:"schemaVersion"`
	Container     string       `json:"container"`
	Tracks        []ProbeTrack `json:"tracks"`
	Warnings      []string     `json:"warnings,omitempty"`

	// Metadata summary, present when the daemon maps metadata and the
	// source carries it.
	Tags      map[string][]string `json:"tags,omitempty"`
	Chapters  []ProbeChapter      `json:"chapters,omitempty"`
	HasArt    bool                `json:"hasArt,omitempty"`
	HasLyrics bool                `json:"hasLyrics,omitempty"`
}

// ProbeChapter is one chapter marker.
type ProbeChapter struct {
	StartSeconds float64 `json:"startSeconds"`
	// EndSeconds is the chapter's end, absent for start-only chapter forms
	// that mean "until the next chapter, or end of stream".
	EndSeconds float64 `json:"endSeconds,omitempty"`
	Title      string  `json:"title,omitempty"`
}

// ProbeTrack is one stream in a ProbeInfo.
type ProbeTrack struct {
	ID              int     `json:"id"`
	Codec           string  `json:"codec"`
	Rate            int     `json:"rate"`
	Channels        int     `json:"channels"`
	Layout          string  `json:"layout"`
	SampleType      string  `json:"sampleType"`
	BitDepth        int     `json:"bitDepth"`
	Samples         int64   `json:"samples"`
	DurationSeconds float64 `json:"durationSeconds"`
	Default         bool    `json:"default"`
}

// Caps is the GET /caps body: this daemon's tested capabilities.
type Caps struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Inputs        []string               `json:"inputs"`
	Decoders      []string               `json:"decoders"`
	Outputs       []CapsOutput           `json:"outputs"`
	Delivery      CapsDelivery           `json:"delivery"`
	DSP           CapsDSP                `json:"dsp"`
	Profiles      map[string]CapsProfile `json:"profiles"`
}

// CapsDSP is the daemon's signal-processing surface, so a format policy
// routes by capability rather than by sniffing a version. It is orthogonal
// to Profiles, which describe client decoder support; dynamics is
// server-side and client-agnostic.
type CapsDSP struct {
	// GainModes are the named gain= spellings. The scalar escape hatch (a
	// dB number) is always accepted and is not listed, because every
	// advertised value must parse; GainMaxDB and GainMaxVoiceDB describe
	// it instead.
	GainModes []string `json:"gainModes"`
	// GainMaxDB and GainMaxVoiceDB are the clamps on requested positive
	// gain. They differ because a spoken-word dynamics preset raises the
	// ceiling: gain=16 is clamped without dynamics=voice and honored with
	// it. Read the pair, not one of them.
	GainMaxDB      float64 `json:"gainMaxDb"`
	GainMaxVoiceDB float64 `json:"gainMaxVoiceDb"`
	// Dynamics are the dynamics= spellings this daemon accepts.
	Dynamics []string `json:"dynamics"`
	// Loudness lists the loudness surfaces, which are jobs-only: a live
	// stream cannot be measured before it is served.
	Loudness []string `json:"loudness"`
	// SilenceDetector is the silence detector's algorithm revision, the
	// same value a silence map carries in its version field. A caller
	// that persists maps invalidates by inequality: a cached map whose
	// version differs from this went stale with a detector upgrade.
	// Advertised even when delivery.jobs is false, like loudness: the
	// dsp slot describes the build, not this daemon's enabled routes.
	// A daemon new enough to have the field always sends it non-empty,
	// so empty means a server too old to advertise the detector: fall
	// back to reading versions off the maps themselves, do not read it
	// as every cached map being stale.
	SilenceDetector string `json:"silenceDetector"`
	// TruePeakCeilingDB is the limiter ceiling in dBTP that every gain- or
	// dynamics-engaged output is held under.
	TruePeakCeilingDB float64 `json:"truePeakCeilingDb"`
}

// CapsProfile is one named delivery profile: the playback capabilities
// of a client family per the server's client matrix, in preference
// order. Pick the profile matching the player stack instead of guessing
// codecs.
type CapsProfile struct {
	Delivery    string   `json:"delivery"`
	Progressive []string `json:"progressive"`
	HLS         []string `json:"hls"`
	Basis       string   `json:"basis"`
	Notes       []string `json:"notes,omitempty"`
}

// CapsOutput is one writable format; Live means the progressive
// /stream surface can serve it (false is jobs-only).
type CapsOutput struct {
	Name string   `json:"name"`
	Live bool     `json:"live"`
	Exts []string `json:"exts"`
}

// CapsDelivery flags the delivery surfaces the daemon serves.
type CapsDelivery struct {
	Progressive bool `json:"progressive"`
	HLS         bool `json:"hls"`
	// HLSFormats are the output formats with a segmented (fMP4) form.
	HLSFormats []string `json:"hlsFormats,omitempty"`
	// CutFormats are the output formats the cut rung serves without
	// re-encoding; a from/to span requested as one of these is delivered by the
	// daemon moving the source's packets, on both /stream and HLS. A daemon new
	// enough to advertise cut always sends a non-empty list, so this field
	// absent means a server too old to advertise cut: fall back to inferring cut
	// availability, do not read it as an offer of no cuts.
	CutFormats []string `json:"cutFormats,omitempty"`
	Jobs       bool     `json:"jobs"`
	Uploads    bool     `json:"uploads"`
	// PID: the daemon resolves pid:<ULID> source references against a
	// WaxBin catalog (a build with a catalog resolver).
	PID bool `json:"pid"`
	// Timelines: the daemon mints multi-source timelines (CreateTimeline),
	// which a tl= parameter then streams gaplessly.
	Timelines bool `json:"timelines"`
	// MaxTimelineMembers bounds one timeline's member count.
	MaxTimelineMembers int `json:"maxTimelineMembers,omitempty"`
}

// TimelineRequest is the POST /hls/timeline body: a play queue, in order.
type TimelineRequest struct {
	Srcs []TimelineSrc `json:"srcs"`
	// CrossfadeSeconds blends each seam over this many seconds when the timeline
	// is rendered; 0 (the default) is a gapless butt-join. It shapes the mint's
	// durationSeconds and boundaries only and is not part of the identity, so two
	// mints of one queue at different crossfades share a tl. A client that mints
	// with a crossfade must pass the same crossfadeSeconds on the signed
	// master.m3u8 it builds, since the boundaries reflect the value minted with.
	CrossfadeSeconds float64 `json:"crossfadeSeconds,omitempty"`
}

// TimelineSrc is one member of a timeline request.
type TimelineSrc struct {
	Src string `json:"src"`
}

// TimelineResponse is POST /hls/timeline's 201 body.
type TimelineResponse struct {
	SchemaVersion int `json:"schemaVersion"`
	// Tl is the timeline's digest: what a tl= parameter names.
	Tl string `json:"tl"`
	// Members is how many sources the timeline holds.
	Members int `json:"members"`
	// DurationSeconds is the concatenated timeline's length.
	DurationSeconds float64 `json:"durationSeconds"`
	// EnvelopeRate is the timeline's normalized sample rate (the maximum
	// member rate), the rate Boundaries' sample offsets are measured on.
	EnvelopeRate int `json:"envelopeRate"`
	// Boundaries are the per-member sample offsets and durations on the
	// envelope timeline, in order, so a client need not probe the members
	// itself to know where each one lands. They are derived, not part of the
	// timeline's identity, so the digest does not cover them.
	Boundaries []MemberBoundary `json:"boundaries"`
}

// MemberBoundary is one member's place on a concatenated timeline, mirroring
// waxflow.MemberBoundary. Both fields are in samples at the envelope rate.
// OffsetSamples is the member's actual start; DurationSamples is its own raw
// normalized length. Under a crossfade consecutive members overlap, so
// OffsetSamples + DurationSamples can exceed the next member's OffsetSamples;
// only without one do the members tile exactly.
type MemberBoundary struct {
	OffsetSamples   int64 `json:"offsetSamples"`
	DurationSamples int64 `json:"durationSamples"`
}

// SignRequest is the POST /sign body; Params carries the playback
// query parameters (src required).
type SignRequest struct {
	Path       string            `json:"path,omitempty"`
	Params     map[string]string `json:"params"`
	TTLSeconds int64             `json:"ttlSeconds,omitempty"`
}

// SignResponse is the POST /sign response: a ready-to-fetch relative
// URL and its expiry.
type SignResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	URL           string `json:"url"`
	Exp           int64  `json:"exp"`
}

// CacheStatsResponse is the GET /cache/stats body.
type CacheStatsResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Entries       int    `json:"entries"`
	Bytes         int64  `json:"bytes"`
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
}

// CacheGCResponse is the POST /cache/gc body.
type CacheGCResponse struct {
	SchemaVersion int   `json:"schemaVersion"`
	Removed       int   `json:"removed"`
	FreedBytes    int64 `json:"freedBytes"`
	// TimelinesRemoved counts stored timelines swept past their expiry.
	TimelinesRemoved int `json:"timelinesRemoved,omitempty"`
}

// JobRequest is the POST /jobs body and, equally, the request a job
// document echoes back: one type serves both directions, so the
// asymmetries between them are per-field facts rather than two
// near-identical structs.
//
// Echo-only, daemon-set (leave zero on create; the daemon refuses each
// with a 400 there, since unknown create fields are errors): SourceID,
// SourceIDs, and CrossfadeSeconds. The identity pins are computed
// server-side so a client cannot send the ones it wishes were true, and
// a timeline job's crossfade is set from the POST /hls/timeline body (a
// timeline is not a /jobs type; see CreateTimeline). A fetched job's
// Request carries them set, so zero them before reusing one as a
// create body.
//
// Create-only, never echoed: Cue. It resolves into Cuts at validation,
// so the document carries the boundaries the 201 accepted and an edit
// to the sheet afterward cannot change what runs.
type JobRequest struct {
	// Type is transcode, analyze, merge, or split. A fifth type,
	// timeline, appears on job documents but is not creatable here:
	// CreateTimeline mints those.
	Type string `json:"type"`
	// Src is the single source a transcode, an analyze, or a split
	// reads. A merge names its members in Srcs instead and refuses Src.
	Src string `json:"src"`
	// SourceID is the daemon's identity pin on Src (size-mtimeNS), so a
	// source replaced before the job ran fails with source-changed
	// rather than quietly transcoding different bytes. Echo-only.
	SourceID string `json:"sourceId,omitempty"`
	// SourceIDs pins Srcs' identities the same way, one per member and
	// in the same order. Echo-only.
	SourceIDs []string `json:"sourceIds,omitempty"`
	// Srcs are a merge's members, in order. Merge-only.
	Srcs []string `json:"srcs,omitempty"`
	// Titles are optional per-member chapter titles for a merge,
	// index-aligned to Srcs: a title per member, or none. Merge-only.
	Titles []string `json:"titles,omitempty"`
	// Cuts are a split's cut points, as sample offsets on the source's
	// own timeline, strictly ascending and interior: N cuts make N+1
	// pieces. Split-only.
	Cuts []int64 `json:"cuts,omitempty"`
	// Cue names a CUE sheet whose track boundaries are this split's cut
	// points, exclusive with Cuts. Split-only and create-only.
	Cue string `json:"cue,omitempty"`

	// Output shaping, mirroring /stream; transcode, merge, and split
	// take these, and Format is required by all three. Analyze takes
	// none of them.
	Format    string `json:"format,omitempty"`
	Container string `json:"container,omitempty"`
	Rate      int    `json:"rate,omitempty"`
	Ch        int    `json:"ch,omitempty"`
	Bits      int    `json:"bits,omitempty"`
	// Bitrate is the lossy output bit rate in kbit/s.
	Bitrate int `json:"bitrate,omitempty"`
	// Gain and Loudness are transcode-only. Loudness "analyze" selects
	// the two-pass form (measure, apply the exact ReplayGain-reference
	// gain, tag the output) and replaces Gain.
	Gain      string `json:"gain,omitempty"`
	Loudness  string `json:"loudness,omitempty"`
	FLACLevel int    `json:"flacLevel,omitempty"`

	// Silence adds the silence map to an analyze job; the two parameters
	// shape it and are refused without Silence true. Analyze-only.
	Silence            bool    `json:"silence,omitempty"`
	SilenceThresholdDB float64 `json:"silenceThresholdDb,omitempty"`
	SilenceMinSeconds  float64 `json:"silenceMinSeconds,omitempty"`

	// CrossfadeSeconds is a timeline job's per-seam blend. Echo-only.
	CrossfadeSeconds float64 `json:"crossfadeSeconds,omitempty"`
}

// JobOutput describes one of a finished job's product files, which
// JobResult serves by index.
type JobOutput struct {
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

// SilenceSummary is the silence map's headline, carried inline on the
// job; the full span map is the job's output file (silence.json),
// served by JobResult. Version is the detector revision, the same value
// /caps advertises as dsp.silenceDetector: a cached map whose version
// differs went stale with a detector upgrade.
type SilenceSummary struct {
	Version     string  `json:"version"`
	ThresholdDB float64 `json:"thresholdDb"`
	MinSeconds  float64 `json:"minSeconds"`
	// Spans is the number of silences found.
	Spans int `json:"spans"`
	// Dropped counts runs discarded for falling short of MinSeconds.
	// Read it with DroppedSeconds, never alone: ordinary audio dips
	// under any threshold at every zero crossing, so the count is large
	// even for a clean source.
	Dropped int `json:"dropped"`
	// DroppedSeconds against the source's duration is the diagnostic: a
	// sizeable share of the stream means the threshold sits at the
	// source's noise floor, not that the source is loud.
	DroppedSeconds float64 `json:"droppedSeconds"`
	// TotalSeconds is the summed length of the kept spans.
	TotalSeconds float64 `json:"totalSeconds"`
}

// JobAnalysis is a loudness measurement. The peak and loudness fields
// are pointers because digital silence measures negative infinity,
// which JSON cannot carry: nil means silence.
type JobAnalysis struct {
	IntegratedLUFS *float64 `json:"integratedLufs"`
	LoudnessRange  float64  `json:"loudnessRange"`
	TruePeakDB     *float64 `json:"truePeakDb"`
	SamplePeakDB   *float64 `json:"samplePeakDb"`
	// Samples, Rate, and Channels describe the basis the numbers were
	// measured on, which is not always the source's: a loudness-analyze
	// transcode that downmixes measures the fold.
	Samples         int64   `json:"samples"`
	Rate            int     `json:"rate"`
	Channels        int     `json:"channels"`
	DurationSeconds float64 `json:"durationSeconds"`
	// AppliedGainDB is the exact gain a loudness-analyze transcode
	// applied (the ReplayGain reference minus the measured loudness).
	AppliedGainDB *float64 `json:"appliedGainDb,omitempty"`
	// ReplayGain values written on the output (loudness-analyze).
	ReplayGainTrackGain string `json:"replaygainTrackGain,omitempty"`
	ReplayGainTrackPeak string `json:"replaygainTrackPeak,omitempty"`
	// Silence summarizes the silence map when the request asked for
	// one; the spans themselves are the job's output file.
	Silence *SilenceSummary `json:"silence,omitempty"`
}

// JobTimeline is a timeline job's product: the digest a tl= parameter
// names, carrying the same values CreateTimeline's inline answer does.
type JobTimeline struct {
	// Tl is the timeline's digest.
	Tl string `json:"tl"`
	// Members is how many sources the timeline holds.
	Members int `json:"members"`
	// DurationSeconds is the concatenated timeline's length.
	DurationSeconds float64 `json:"durationSeconds"`
	// EnvelopeRate is the timeline's normalized sample rate, the rate
	// Boundaries' sample offsets are measured on.
	EnvelopeRate int `json:"envelopeRate"`
	// Boundaries are the per-member sample offsets and durations on the
	// envelope timeline; see MemberBoundary.
	Boundaries []MemberBoundary `json:"boundaries"`
}

// JobProgress is the running job's position, broadcast on the event
// stream as it advances.
type JobProgress struct {
	// Phase is analyze, transcode, or finalize.
	Phase string `json:"phase"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	// Percent is -1 when the total is unknown.
	Percent float64 `json:"percent"`
}

// JobError is a terminal failure; Code is a waxerr code string, the
// same vocabulary the error envelope carries.
type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job is one job's full document: POST /jobs' 201 body, GET /jobs/{id},
// and each event on the JobEvents stream. The pointer fields are
// presence: nil means the section is absent from the document (a queued
// job has no Started, only a failed one has Error), distinguishable
// from a zero value.
type Job struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	// Type is transcode, analyze, merge, split, or timeline.
	Type string `json:"type"`
	// State is queued or running, then terminally done, failed, or
	// canceled.
	State string `json:"state"`
	// Request echoes what the job was accepted as, plus the daemon-set
	// fields; see JobRequest for which is which.
	Request  JobRequest `json:"request"`
	Created  time.Time  `json:"created"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Error    *JobError  `json:"error,omitempty"`
	// Outputs are the job's product files, in order; the index is the
	// one JobResult takes. A split has one per piece, the other types
	// at most one.
	Outputs  []JobOutput  `json:"outputs,omitempty"`
	Analysis *JobAnalysis `json:"analysis,omitempty"`
	Timeline *JobTimeline `json:"timeline,omitempty"`
	Progress *JobProgress `json:"progress,omitempty"`
	// Warnings are non-fatal notes (metadata that could not be read or
	// written); the audio outcome is unaffected.
	Warnings []string `json:"warnings,omitempty"`
}

// JobsList is the GET /jobs body, mirroring the server's envelope type
// name and shape.
type JobsList struct {
	SchemaVersion int    `json:"schemaVersion"`
	Jobs          []*Job `json:"jobs"`
}

// Ping checks daemon liveness.
func (c *Client) Ping(ctx context.Context) error {
	return c.getJSON(ctx, "/ping", nil, &struct{}{})
}

// Version fetches build information.
func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	var v VersionInfo
	if err := c.getJSON(ctx, "/version", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Caps fetches capability discovery.
func (c *Client) Caps(ctx context.Context) (*Caps, error) {
	var v Caps
	if err := c.getJSON(ctx, "/caps", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Probe identifies a source reference.
func (c *Client) Probe(ctx context.Context, src string) (*ProbeInfo, error) {
	var v ProbeInfo
	if err := c.getJSON(ctx, "/probe", url.Values{"src": {src}}, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Sign mints a signed playback URL on the daemon.
func (c *Client) Sign(ctx context.Context, req SignRequest) (*SignResponse, error) {
	var v SignResponse
	if err := c.postJSON(ctx, "/sign", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateTimeline mints a multi-source timeline from a play queue and returns
// the digest a tl= parameter names, for a signed /hls/master.m3u8 over the whole
// queue. It takes the request whole, like Sign, so the members and an optional
// CrossfadeSeconds ride one value the wire mirrors.
//
// A member whose headers cannot declare an exact length has to be measured,
// which means decoding it; the daemon then answers with a job rather than a
// digest, and jobID names it. Exactly one of tl and jobID is set. A queue of
// FLACs, of tagged MP3s, or any queue minted before (the daemon memoizes per
// file) returns the digest directly.
//
// A crossfade rides the response, not the digest: the returned boundaries and
// duration reflect req.CrossfadeSeconds, but the render must pass the same
// crossfadeSeconds on the master.m3u8 it signs, since the digest covers the
// members alone. See TimelineRequest.CrossfadeSeconds.
//
// Follow that job with Job or its JobEvents stream; the finished job's
// Timeline field carries the same values this returns.
func (c *Client) CreateTimeline(ctx context.Context, req TimelineRequest) (tl *TimelineResponse, jobID string, err error) {
	var v struct {
		TimelineResponse
		// The 202 body is the job, whose id is all this package can use.
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/hls/timeline", req, &v); err != nil {
		return nil, "", err
	}
	if v.Tl == "" {
		return nil, v.ID, nil
	}
	return &v.TimelineResponse, "", nil
}

// CacheStats fetches cache shape and hit counters.
func (c *Client) CacheStats(ctx context.Context) (*CacheStatsResponse, error) {
	var v CacheStatsResponse
	if err := c.getJSON(ctx, "/cache/stats", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CacheGC runs eviction now.
func (c *Client) CacheGC(ctx context.Context) (*CacheGCResponse, error) {
	var v CacheGCResponse
	if err := c.postJSON(ctx, "/cache/gc", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// openStream runs a GET whose successful body the caller owns: the
// shared back half of the streaming methods. It uses the stream client
// (response headers bounded, body not; see New) and applies no default
// deadline. ok lists the statuses that pass the body through; anything
// else is drained as the error envelope. One home for the status
// policies keeps the three methods from drifting apart.
func (c *Client) openStream(ctx context.Context, pathAndQuery, op string, ok ...int) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		if ce := ctxErr(err); ce != nil {
			return nil, ce
		}
		return nil, waxerr.Wrap(waxerr.CodeInternal, "client: "+op, err)
	}
	if !slices.Contains(ok, resp.StatusCode) {
		defer resp.Body.Close()
		return nil, decodeEnvelope(resp)
	}
	return resp, nil
}

// Stream opens a progressive stream. The caller owns the response body.
// The body read runs under the caller's ctx alone, with no client-side
// time bound: playback takes as long as it plays (see New).
func (c *Client) Stream(ctx context.Context, params url.Values) (*http.Response, error) {
	return c.openStream(ctx, "/stream?"+params.Encode(), "stream",
		http.StatusOK, http.StatusPartialContent)
}

// CreateJob submits an async job (POST /jobs) and returns the accepted
// job document, whose ID the other job methods take. Acceptance is a
// promise: the request validated whole (source resolved, identity
// pinned, plan checked), so a created job will not fail on request
// shape later. Follow it with Job or JobEvents, fetch products with
// JobResult.
//
// A merge create is the one slow call on this surface: that validation
// promise means the daemon resolves and exactly measures every member
// before answering, so a large cold queue can legitimately outlive the
// default 30 s JSON bound. Bring a ctx deadline sized to the queue; it
// replaces the default rather than racing it (see New).
func (c *Client) CreateJob(ctx context.Context, req JobRequest) (*Job, error) {
	var v Job
	if err := c.postJSON(ctx, "/jobs", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Job fetches one job's document.
func (c *Client) Job(ctx context.Context, id string) (*Job, error) {
	if err := checkJobID(id); err != nil {
		return nil, err
	}
	var v Job
	if err := c.getJSON(ctx, "/jobs/"+url.PathEscape(id), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Jobs lists every job the daemon holds.
func (c *Client) Jobs(ctx context.Context) (*JobsList, error) {
	var v JobsList
	if err := c.getJSON(ctx, "/jobs", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// DeleteJob cancels the job if it is running and removes it.
func (c *Client) DeleteJob(ctx context.Context, id string) error {
	if err := checkJobID(id); err != nil {
		return err
	}
	ctx, cancel := jsonDeadline(ctx)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodDelete, "/jobs/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// JobResult opens the job's nth output file (GET /jobs/{id}/result/{n}):
// a transcode's or a merge's audio, one piece of a split, or an analyze
// job's silence map. n < 0 asks the bare /result form, which answers
// only where it cannot be wrong: the one output of a job that has one,
// the product as JSON for a job whose product is not a file (an analyze
// job's numbers, a timeline job's digest), and a 400 naming the indexed
// form for a split with several. The caller owns the response body.
// Like Stream, the request runs under the caller's ctx alone, with no
// client-side time bound: a result download takes as long as it takes.
//
// The endpoint serves byte ranges, so like Stream a 206 passes through
// as success: this method never asks for one, but a range-requesting
// transport brought via WithHTTPClient (resuming a large download) gets
// its partial body back rather than an error.
func (c *Client) JobResult(ctx context.Context, id string, n int) (*http.Response, error) {
	if err := checkJobID(id); err != nil {
		return nil, err
	}
	p := "/jobs/" + url.PathEscape(id) + "/result"
	if n >= 0 {
		p += "/" + strconv.Itoa(n)
	}
	return c.openStream(ctx, p, "job result", http.StatusOK, http.StatusPartialContent)
}

// JobEvents opens the job's server-sent event stream (GET
// /jobs/{id}/events): one "event: job" per state or progress update,
// each data: line a full job document, with comment heartbeats every
// 15 s in between. The stream ends on its own after the job's terminal
// event; until then the connection is held open, so the caller must
// close the body (or cancel ctx) to release it. The body read runs
// under the caller's ctx alone, with no client-side time bound, since
// any absolute timeout would cut a long job's stream mid-wait.
func (c *Client) JobEvents(ctx context.Context, id string) (*http.Response, error) {
	if err := checkJobID(id); err != nil {
		return nil, err
	}
	return c.openStream(ctx, "/jobs/"+url.PathEscape(id)+"/events", "job events", http.StatusOK)
}

// checkJobID refuses an empty job id before it can misroute: "/jobs/"
// is not a job URL, and "/jobs//result" would be path-cleaned by the
// daemon's mux into a lookup of a job literally named "result", whose
// not-found would read as the daemon's word on a job that was never
// named.
func checkJobID(id string) error {
	if id == "" {
		return waxerr.New(waxerr.CodeInvalidRequest, "client: a job id is required")
	}
	return nil
}

// MintURL mints a signed playback URL offline: the same canonical scheme
// the daemon verifies (ADR-0003), for holders of the signing secret who
// do not want a round trip. secretSpec uses the signingSecret config
// syntax; params must already include the id identity parameter.
func MintURL(secretSpec, path string, params url.Values, exp time.Time) (string, error) {
	keys, err := sign.ParseKeys(secretSpec)
	if err != nil {
		return "", err
	}
	signer, err := sign.New(keys)
	if err != nil {
		return "", err
	}
	signed := signer.Sign(http.MethodGet, path, params, exp)
	return path + "?" + signed.Encode(), nil
}

func (c *Client) newRequest(ctx context.Context, method, pathAndQuery string, body io.Reader) (*http.Request, error) {
	u := strings.TrimSuffix(c.base.String(), "/") + pathAndQuery
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalidRequest, "client: building request", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return req, nil
}

// jsonTimeout bounds one JSON round trip (request through body decode)
// when the caller's ctx brings no deadline of its own. It lives on the
// context rather than on http.Client.Timeout because that timeout spans
// the entire body read, which would cut the streaming methods' long
// bodies; see New for the contract.
const jsonTimeout = 30 * time.Second

// jsonDeadline applies the default JSON round-trip bound, deferring to
// any deadline the caller already set.
func jsonDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, jsonTimeout)
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	ctx, cancel := jsonDeadline(ctx)
	defer cancel()
	p := path
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, p, nil)
	if err != nil {
		return err
	}
	return c.do(req, v)
}

func (c *Client) postJSON(ctx context.Context, path string, body, v any) error {
	ctx, cancel := jsonDeadline(ctx)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "client: encoding request", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, rdr)
	if err != nil {
		return err
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, v)
}

// ctxErr maps a request ended by its context onto canceled: the
// caller's deliberate abort and the default JSON deadline both land
// here, and reporting either as the daemon being unreachable would
// point an operator at a network that is fine. nil when the error is
// not the context's doing.
func ctxErr(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return waxerr.Wrap(waxerr.CodeCanceled, "client: request deadline exceeded", err)
	case errors.Is(err, context.Canceled):
		return waxerr.Wrap(waxerr.CodeCanceled, "client: request canceled", err)
	}
	return nil
}

// do runs the request and decodes the 2xx body into v; a nil v skips
// the decode, for endpoints whose success is a bare status (204).
func (c *Client) do(req *http.Request, v any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		if ce := ctxErr(err); ce != nil {
			return ce
		}
		return waxerr.Wrap(waxerr.CodeInternal, "client: daemon unreachable", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeEnvelope(resp)
	}
	if v == nil {
		return nil
	}
	// Bounded so a wrong URL (a server that is not this daemon streaming
	// something enormous) cannot balloon memory, and sized so no
	// legitimate response reaches it: the jobs listing is the one body
	// here with no server-side cap (jobs accumulate until deleted, and a
	// maximal 1000-member merge document runs ~100 KB), and 64 MB holds
	// hundreds of even those.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v); err != nil {
		if ce := ctxErr(err); ce != nil {
			return ce
		}
		return waxerr.Wrap(waxerr.CodeInternal, "client: decoding response", err)
	}
	return nil
}

// decodeEnvelope maps the family error envelope back onto a waxerr code,
// falling back to the HTTP status class when the body is not an envelope.
//
// The fallback is deliberately conservative. The daemon's error path
// always writes the envelope, so a response without one is something
// else speaking (a proxy's own error page, a load balancer), and
// reading daemon semantics out of its bare status would mislead the
// recovery paths the typed codes drive: not-found in particular means
// re-mint the timeline or give the job up for lost, which no proxy 404
// can honestly say. Internal is the truthful class for "something
// between us broke". Unauthorized is the one exception, because auth is
// commonly enforced in front of a daemon and the remedy is the same
// wherever the 401 came from.
func decodeEnvelope(resp *http.Response) error {
	var env struct {
		Error string      `json:"error"`
		Code  waxerr.Code `json:"code"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &env) == nil && env.Code != "" {
		return waxerr.New(env.Code, env.Error)
	}
	code := waxerr.CodeInternal
	if resp.StatusCode == http.StatusUnauthorized {
		code = waxerr.CodeUnauthorized
	}
	return waxerr.New(code, fmt.Sprintf("client: %s from daemon", resp.Status))
}
