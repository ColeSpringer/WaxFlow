// Package client is the Go API client for a WaxFlow daemon (WaxSeal
// client/ precedent): thin typed wrappers over the HTTP surface plus an
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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/waxerr"
)

// Client talks to one WaxFlow daemon.
type Client struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying http.Client (timeouts, proxies).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New returns a Client for the daemon at baseURL ("http://host:4418").
// apiKey may be empty against a keyless daemon.
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("client: base URL %q must be scheme://host[:port]", baseURL))
	}
	c := &Client{base: u, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Wire types mirror the server's (docs/api.md); the golden fixtures pin
// both sides against drift.

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
	Jobs        bool `json:"jobs"`
	Uploads     bool `json:"uploads"`
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

// CreateTimeline mints a multi-source timeline and returns the digest a tl=
// parameter names, for a signed /hls/master.m3u8 over a whole play queue.
//
// A member whose headers cannot declare an exact length has to be measured,
// which means decoding it; the daemon then answers with a job rather than a
// digest, and jobID names it. Exactly one of tl and jobID is set. A queue of
// FLACs, of tagged MP3s, or any queue minted before (the daemon memoizes per
// file) returns the digest directly.
//
// This package has no jobs surface, so following the job is GET /jobs/{id}
// or its event stream; the finished job's timeline field carries the same
// values this returns.
func (c *Client) CreateTimeline(ctx context.Context, srcs []string) (tl *TimelineResponse, jobID string, err error) {
	req := TimelineRequest{Srcs: make([]TimelineSrc, len(srcs))}
	for i, src := range srcs {
		req.Srcs[i] = TimelineSrc{Src: src}
	}
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

// Stream opens a progressive stream. The caller owns the response body.
func (c *Client) Stream(ctx context.Context, params url.Values) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/stream?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "client: stream", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return nil, decodeEnvelope(resp)
	}
	return resp, nil
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

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
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

func (c *Client) do(req *http.Request, v any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "client: daemon unreachable", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeEnvelope(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v); err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "client: decoding response", err)
	}
	return nil
}

// decodeEnvelope maps the family error envelope back onto a waxerr code,
// falling back to the HTTP status class when the body is not an envelope.
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
