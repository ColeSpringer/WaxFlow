// Package client is the Go API client for a WaxFlow daemon (WaxSeal
// client/ precedent): thin typed wrappers over the HTTP surface plus an
// offline signed-URL mint helper, so WaxDeck and the CLI never
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

type VersionInfo struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
}

type ProbeInfo struct {
	SchemaVersion int          `json:"schemaVersion"`
	Container     string       `json:"container"`
	Tracks        []ProbeTrack `json:"tracks"`
	Warnings      []string     `json:"warnings,omitempty"`
}

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

type Caps struct {
	SchemaVersion int            `json:"schemaVersion"`
	Inputs        []string       `json:"inputs"`
	Decoders      []string       `json:"decoders"`
	Outputs       []CapsOutput   `json:"outputs"`
	Delivery      CapsDelivery   `json:"delivery"`
	Profiles      map[string]any `json:"profiles"`
}

type CapsOutput struct {
	Name string   `json:"name"`
	Live bool     `json:"live"`
	Exts []string `json:"exts"`
}

type CapsDelivery struct {
	Progressive bool `json:"progressive"`
	HLS         bool `json:"hls"`
	Jobs        bool `json:"jobs"`
	Uploads     bool `json:"uploads"`
}

type SignRequest struct {
	Path       string            `json:"path,omitempty"`
	Params     map[string]string `json:"params"`
	TTLSeconds int64             `json:"ttlSeconds,omitempty"`
}

type SignResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	URL           string `json:"url"`
	Exp           int64  `json:"exp"`
}

type CacheStats struct {
	SchemaVersion int    `json:"schemaVersion"`
	Entries       int    `json:"entries"`
	Bytes         int64  `json:"bytes"`
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
}

type CacheGCResult struct {
	SchemaVersion int   `json:"schemaVersion"`
	Removed       int   `json:"removed"`
	FreedBytes    int64 `json:"freedBytes"`
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

// CacheStats fetches cache shape and hit counters.
func (c *Client) CacheStats(ctx context.Context) (*CacheStats, error) {
	var v CacheStats
	if err := c.getJSON(ctx, "/cache/stats", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CacheGC runs eviction now.
func (c *Client) CacheGC(ctx context.Context) (*CacheGCResult, error) {
	var v CacheGCResult
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
