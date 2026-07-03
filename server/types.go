package server

import (
	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/format"
)

// Wire types for the JSON API. These are the contract documented in
// docs/api.md and pinned by golden fixtures; the client package mirrors
// them (decoding side) and the CLI probe command shares ProbeInfo so the
// local and HTTP shapes cannot drift.

// VersionInfo is the GET /version body.
type VersionInfo struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
}

// ProbeInfo is the GET/POST /probe body (and `waxflow probe --json`).
type ProbeInfo struct {
	SchemaVersion int          `json:"schemaVersion"`
	Container     string       `json:"container"`
	Tracks        []ProbeTrack `json:"tracks"`
	Warnings      []string     `json:"warnings,omitempty"`
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

// ProbeJSON maps a format.Info onto the wire shape.
func ProbeJSON(info *format.Info) ProbeInfo {
	out := ProbeInfo{SchemaVersion: 1, Container: info.Container, Warnings: info.Warnings}
	for _, t := range info.Tracks {
		out.Tracks = append(out.Tracks, ProbeTrack{
			ID:              t.ID,
			Codec:           string(t.Codec),
			Rate:            t.Fmt.Rate,
			Channels:        t.Fmt.Channels,
			Layout:          t.Fmt.Layout.String(),
			SampleType:      t.Fmt.Type.String(),
			BitDepth:        t.Fmt.BitDepth,
			Samples:         t.Samples,
			DurationSeconds: DurationSeconds(t.Samples, t.Fmt.Rate),
			Default:         t.Default,
		})
	}
	return out
}

// DurationSeconds converts samples to seconds at the presentation
// boundary; positions stay integer samples everywhere else (ADR-0006).
// Unknown lengths (negative samples) yield -1.
func DurationSeconds(samples int64, rate int) float64 {
	if samples < 0 || rate <= 0 {
		return -1
	}
	return float64(samples) / float64(rate)
}

// Caps is the GET /caps body: tested capabilities only (the registry is
// capability-gated), never aspirations.
type Caps struct {
	SchemaVersion int          `json:"schemaVersion"`
	Inputs        []string     `json:"inputs"`
	Decoders      []string     `json:"decoders"`
	Outputs       []CapsOutput `json:"outputs"`
	Delivery      CapsDelivery `json:"delivery"`
	// Profiles are the named delivery profiles (apple-native, hls-js,
	// ...) whose contents will be tested facts from the client-matrix
	// verification; empty until that verification exists.
	Profiles map[string]any `json:"profiles"`
}

// CapsOutput is one writable format.
type CapsOutput struct {
	Name string `json:"name"`
	// Live: the format has a streaming form (progressive /stream can
	// serve it); false means job outputs only.
	Live bool     `json:"live"`
	Exts []string `json:"exts"`
}

// CapsDelivery flags the delivery surfaces this build serves.
type CapsDelivery struct {
	Progressive bool `json:"progressive"`
	HLS         bool `json:"hls"`
	Jobs        bool `json:"jobs"`
	Uploads     bool `json:"uploads"`
}

// SignRequest is the POST /sign body.
type SignRequest struct {
	// Path is the playback path to sign; empty means /stream.
	Path string `json:"path,omitempty"`
	// Params are the playback query parameters (src required). The
	// source identity (id), exp, kid, and sig are added by the server.
	Params map[string]string `json:"params"`
	// TTLSeconds overrides the default TTL max(6h, 2 x duration).
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
}

// SignResponse is the POST /sign body: a ready-to-fetch relative URL.
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
}

// buildCaps assembles Caps from the capability-gated tables.
func buildCaps() Caps {
	caps := Caps{
		SchemaVersion: 1,
		Inputs:        format.Inputs(),
		Delivery:      CapsDelivery{Progressive: true},
		Profiles:      map[string]any{},
	}
	for _, id := range format.Decoders() {
		caps.Decoders = append(caps.Decoders, string(id))
	}
	for _, o := range waxflow.Outputs() {
		caps.Outputs = append(caps.Outputs, CapsOutput{Name: o.Name, Live: o.Live, Exts: o.Exts})
	}
	return caps
}
