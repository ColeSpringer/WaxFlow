package server

import (
	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
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
// The metadata fields appear only when a mapper is wired and the source
// carries them.
type ProbeInfo struct {
	SchemaVersion int          `json:"schemaVersion"`
	Container     string       `json:"container"`
	Tracks        []ProbeTrack `json:"tracks"`
	Warnings      []string     `json:"warnings,omitempty"`

	// Tags is the canonical tag summary (TITLE, ARTIST, REPLAYGAIN_*, ...).
	Tags map[string][]string `json:"tags,omitempty"`
	// Chapters are embedded chapter markers.
	Chapters []ProbeChapter `json:"chapters,omitempty"`
	// HasArt and HasLyrics report the passthrough surfaces /art and
	// /lyrics can serve.
	HasArt    bool `json:"hasArt,omitempty"`
	HasLyrics bool `json:"hasLyrics,omitempty"`
}

// ProbeChapter is one chapter marker.
type ProbeChapter struct {
	StartSeconds float64 `json:"startSeconds"`
	Title        string  `json:"title,omitempty"`
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

// ProbeJSON maps a format.Info (and, when a mapper ran, the source's
// metadata) onto the wire shape.
func ProbeJSON(info *format.Info, m *meta.Info) ProbeInfo {
	out := ProbeInfo{SchemaVersion: 1, Container: info.Container, Warnings: info.Warnings}
	if m != nil {
		// A tag summary, not the payloads: the lyric sheet (potentially
		// many KB on every probe) is served by /lyrics and reported here
		// as hasLyrics. Copied, never aliased: the meta Info may be a
		// shared cache entry.
		if len(m.Tags) > 0 {
			out.Tags = make(map[string][]string, len(m.Tags))
			for k, v := range m.Tags {
				if k != "LYRICS" {
					out.Tags[k] = v
				}
			}
		}
		out.HasArt = m.HasPictures
		out.HasLyrics = m.Lyrics() != "" || len(m.Synced) > 0
		for _, ch := range m.Chapters {
			out.Chapters = append(out.Chapters, ProbeChapter{
				StartSeconds: ch.Start.Seconds(),
				Title:        ch.Title,
			})
		}
	}
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
	// HLSFormats are the output formats with a segmented (fMP4) form.
	HLSFormats []string `json:"hlsFormats,omitempty"`
	Jobs       bool     `json:"jobs"`
	Uploads    bool     `json:"uploads"`
	// PID: pid:<ULID> source references resolve against a WaxBin
	// catalog (the resolver flavor with catalogDB configured).
	PID bool `json:"pid"`
}

// UploadResponse is the POST /uploads body.
type UploadResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	// Ref is the ready-to-use source reference (upload:<id>).
	Ref   string `json:"ref"`
	Name  string `json:"name,omitempty"`
	Bytes int64  `json:"bytes"`
	// ExpiresAt is the unix time the TTL janitor removes the upload, 0
	// when uploads never expire.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// SignRequest is the POST /sign body.
type SignRequest struct {
	// Path is the signing target; empty means /stream. Signable paths:
	// /stream, /hls/master.m3u8, /art, /lyrics, /jobs/<id>/events, and
	// /jobs/<id>/result.
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

// buildCaps assembles Caps from the capability-gated tables plus the
// configured optional surfaces.
func buildCaps(jobs, uploads, pid bool) Caps {
	caps := Caps{
		SchemaVersion: 1,
		Inputs:        format.Inputs(),
		Delivery: CapsDelivery{
			Progressive: true,
			HLS:         true,
			HLSFormats:  waxflow.SegmentedFormats(),
			Jobs:        jobs,
			Uploads:     uploads,
			PID:         pid,
		},
		Profiles: map[string]any{},
	}
	for _, id := range format.Decoders() {
		caps.Decoders = append(caps.Decoders, string(id))
	}
	for _, o := range waxflow.Outputs() {
		caps.Outputs = append(caps.Outputs, CapsOutput{Name: o.Name, Live: o.Live, Exts: o.Exts})
	}
	return caps
}
