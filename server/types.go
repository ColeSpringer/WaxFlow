package server

import (
	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/silence"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/timeline"
)

// probeMetadata summarizes an internal metadata read for ProbeJSON.
// The CLI probe command builds the identical summary (pinned against
// this one by TestProbeJSONMatchesHTTP in the cli module), keeping the
// internal meta type out of the public signature.
func probeMetadata(m *meta.Info) *ProbeMetadata {
	if m == nil {
		return nil
	}
	return &ProbeMetadata{
		Tags:      m.TagSummary(),
		Chapters:  m.Chapters,
		HasArt:    m.HasPictures,
		HasLyrics: m.HasLyrics(),
	}
}

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
	// EndSeconds is the chapter's end, absent for the start-only chapter
	// forms (Nero chpl) that mean "until the next chapter, or end of
	// stream". A caller deriving a span from a chapter reads this when it
	// is there and the next chapter's start when it is not.
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

// ProbeMetadata is the optional metadata summary a probe response
// carries; nil means no mapper ran (or the source has none). All fields
// are caller-owned public types, so external embedders can fill it from
// any metadata source.
type ProbeMetadata struct {
	// Tags is the canonical tag summary (TITLE, ARTIST, ...). The lyric
	// sheet does not belong here: it is served whole by /lyrics and
	// reported via HasLyrics.
	Tags map[string][]string
	// Chapters are embedded chapter markers in playback order.
	Chapters []container.Chapter
	// HasArt and HasLyrics report what /art and /lyrics can serve.
	HasArt    bool
	HasLyrics bool
}

// ProbeJSON maps a format.Info (and, when metadata was read, its
// summary) onto the wire shape.
func ProbeJSON(info *format.Info, m *ProbeMetadata) ProbeInfo {
	out := ProbeInfo{SchemaVersion: 1, Container: info.Container, Warnings: info.Warnings}
	if m != nil {
		out.Tags = m.Tags
		out.HasArt = m.HasArt
		out.HasLyrics = m.HasLyrics
	}
	// The mapper's chapters win when a mapper is wired, since that is where
	// this surface has always read them and a richer tag library may know
	// forms the container package does not. The container's own are the
	// fallback, and they are the reason Info carries them at all: without
	// this branch a daemon embedded by anyone who does not inject a mapper
	// reports no chapters for a file that plainly has them.
	chapters := info.Chapters
	if m != nil && len(m.Chapters) > 0 {
		chapters = m.Chapters
	}
	for _, ch := range chapters {
		out.Chapters = append(out.Chapters, ProbeChapter{
			StartSeconds: ch.Start.Seconds(),
			EndSeconds:   ch.End.Seconds(),
			Title:        ch.Title,
		})
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
	// DSP is the signal-processing surface, so a client routes by
	// capability rather than by sniffing a version.
	DSP CapsDSP `json:"dsp"`
	// Profiles are the named delivery profiles: per client family, the
	// playback capabilities established by the client matrix
	// (docs/client-matrix.md), so a UI picks a profile instead of
	// guessing codecs.
	Profiles map[string]CapsProfile `json:"profiles"`
}

// CapsDSP is the /caps DSP slot: what this build's signal path can do, so
// a client's format policy routes by capability instead of by version
// sniffing. It is deliberately orthogonal to Profiles, which are about
// client decoder support: dynamics is server-side and client-agnostic, so
// a profile has nothing to say about it.
type CapsDSP struct {
	// GainModes are the named gain spellings, not including the scalar
	// escape hatch. Every entry here must parse, which is why a "<db>"
	// placeholder cannot live in this list: parseGain would reject it, and
	// TestCapsDSPIsHonest checks exactly that rule. The escape hatch is
	// expressed by the ceilings below, which is what a client actually
	// needs to know.
	GainModes []string `json:"gainModes"`
	// GainMaxDB and GainMaxVoiceDB are the policy clamps on requested
	// positive gain, which differ by mode: a spoken-word dynamics preset
	// raises the ceiling (see gainCeilingFor). Both are advertised because
	// the clamp is a function of a neighbouring parameter, so without them
	// a client is back to sniffing a version to learn whether gain=16 is
	// legal, which is the exact failure this slot exists to prevent.
	GainMaxDB      float64 `json:"gainMaxDb"`
	GainMaxVoiceDB float64 `json:"gainMaxVoiceDb"`
	// Dynamics are the dynamics= spellings this build accepts.
	Dynamics []string `json:"dynamics"`
	// Loudness lists the loudness surfaces, which are jobs-only: WaxFlow
	// cannot measure a live stream, so exact measurement is a second pass.
	Loudness []string `json:"loudness"`
	// SilenceDetector is the silence detector's algorithm revision, the
	// same value a silence map carries in its version field
	// (silence.Version). A caller that persists maps invalidates by
	// inequality: a cached map whose version differs from this went stale
	// with a detector upgrade, learned here without running a job to find
	// out. Advertised even when delivery.jobs is false, exactly as
	// loudness already is: the dsp slot describes the build's signal
	// path, not this daemon's enabled routes. A build with the field
	// always sends it non-empty (TestCapsDSPIsHonest), so a client
	// seeing it absent or empty should read a server too old to
	// advertise the detector and fall back to the maps' own version
	// fields, not conclude that every cached map is stale; the
	// cutFormats advertisement carries the same rule.
	SilenceDetector string `json:"silenceDetector"`
	// TruePeakCeilingDB is the limiter's ceiling in dBTP, which every
	// gain- or dynamics-engaged output is held under.
	TruePeakCeilingDB float64 `json:"truePeakCeilingDb"`
}

// CapsProfile is one named delivery profile. The format lists are facts
// from the client-matrix verification, filtered against what this build
// actually serves, in preference order (first is the default choice).
type CapsProfile struct {
	// Delivery is the recommended surface for this client family:
	// "hls" or "progressive".
	Delivery string `json:"delivery"`
	// Progressive lists the output formats verified to play as
	// progressive streams (including live chunked transcodes).
	Progressive []string `json:"progressive"`
	// HLS lists the output formats verified to play as HLS rungs.
	HLS []string `json:"hls"`
	// Basis states how these facts were established: the automated
	// browser run or the documented manual checklist.
	Basis string `json:"basis"`
	// Notes carry client-version caveats a UI may need to surface.
	Notes []string `json:"notes,omitempty"`
}

// deliveryProfiles is the client matrix distilled (docs/client-matrix.md
// owns the per-cell evidence; this table must agree with it). Lists here
// may name formats a build does not register; buildCaps filters them so
// /caps never advertises more than the build serves.
var deliveryProfiles = map[string]CapsProfile{
	// Web players driving hls.js over MSE, and browser <audio> for
	// progressive. Verified in Chromium by scripts/client-e2e.mjs
	// (nightly); ALAC is absent because non-Safari browsers ship no
	// ALAC decoder.
	"hls-js": {
		Delivery:    "hls",
		Progressive: []string{"opus", "flac", "mp3", "aac", "wav"},
		HLS:         []string{"opus", "flac", "aac"},
		Basis:       "automated: hls.js + <audio> in Chromium (make client-e2e, nightly)",
	},
	// AVPlayer and Safari. HLS is Apple's own guaranteed-supported
	// path (fMP4 for FLAC/ALAC/Opus per the HLS authoring spec);
	// progressive live transcodes are chunked with no Content-Length,
	// and AVPlayer's tolerance for that is a per-release manual
	// checklist cell, so the recommendation steers to HLS.
	"apple-native": {
		Delivery:    "hls",
		Progressive: []string{"aac", "mp3", "flac", "alac", "opus"},
		HLS:         []string{"aac", "alac", "flac", "opus"},
		Basis:       "vendor-documented + manual checklist (docs/client-matrix.md)",
		Notes: []string{
			"hls opus needs iOS/tvOS 17 or macOS 14",
			"progressive opus (Ogg) needs Safari/iOS 18.4",
			"progressive live transcodes pending checklist verification; prefer hls",
		},
	},
	// Android Media3/ExoPlayer. No ALAC decoder exists on Android.
	"android-exoplayer": {
		Delivery:    "hls",
		Progressive: []string{"opus", "flac", "mp3", "aac", "wav"},
		HLS:         []string{"opus", "flac", "aac"},
		Basis:       "vendor-documented + manual checklist (docs/client-matrix.md)",
	},
	// Desktop players on libmpv/ffmpeg (mpv, VLC, media_kit): every
	// output plays; progressive is preferred on LAN (simpler, full
	// quality, instant seek via t=).
	"desktop-mpv": {
		Delivery:    "progressive",
		Progressive: []string{"opus", "flac", "mp3", "aac", "alac", "wav"},
		HLS:         []string{"opus", "flac", "aac", "alac"},
		Basis:       "manual checklist (docs/client-matrix.md)",
	},
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
	// CutFormats are the output formats the cut rung serves without
	// re-encoding; a from/to span requested as one of these is delivered by
	// moving the source's packets, on both /stream and HLS. Cut is a built-in
	// engine capability the honesty tests keep non-empty, so this build never
	// omits the field despite omitempty: a client seeing it absent should read
	// that as a server too old to advertise cut (fall back to inferring), not as
	// an offer of no cuts.
	CutFormats []string `json:"cutFormats,omitempty"`
	Jobs       bool     `json:"jobs"`
	Uploads    bool     `json:"uploads"`
	// PID: pid:<ULID> source references resolve against a WaxBin
	// catalog (a catalog resolver with catalogDB configured).
	PID bool `json:"pid"`
	// Timelines: POST /hls/timeline mints multi-source timelines, which the
	// tl= parameter then streams gaplessly. MaxTimelineMembers bounds one.
	Timelines          bool `json:"timelines"`
	MaxTimelineMembers int  `json:"maxTimelineMembers,omitempty"`
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

// TimelineRequest is the POST /hls/timeline body: a play queue, in order.
//
// Each member is an object rather than a bare string so the shape has room
// to grow (a span of one file is the obvious next member kind) without a
// second endpoint.
type TimelineRequest struct {
	Srcs []TimelineSrc `json:"srcs"`
	// CrossfadeSeconds blends each seam over this many seconds when the timeline
	// is rendered; 0 (the default) is a gapless butt-join. It shapes this
	// response's durationSeconds and boundaries only, and is not part of the
	// timeline's identity: the digest covers the members alone, so two mints of
	// one queue at different crossfades share a tl=. A client that mints with a
	// crossfade must also pass the same crossfadeSeconds on the signed
	// master.m3u8 it builds, since the boundaries here reflect the value minted
	// with; see waxflow.MemberBoundary and ADR-0009.
	CrossfadeSeconds float64 `json:"crossfadeSeconds,omitempty"`
}

// TimelineSrc is one member of a timeline request.
type TimelineSrc struct {
	Src string `json:"src"`
}

// TimelineResponse is POST /hls/timeline's 201 body. A mint that had to
// measure a member answers 202 with a job instead, and the job's timeline
// field carries the same values.
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
	// envelope timeline, in order. They are derived from the measured members,
	// not part of the timeline's identity, so the digest does not cover them.
	// Offsets overlap under a crossfade; see waxflow.MemberBoundary. Without a
	// crossfade the members tile exactly.
	Boundaries []waxflow.MemberBoundary `json:"boundaries"`
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
	// TimelinesRemoved counts stored timelines swept past their expiry.
	// They are not cache entries and free no cache bytes, so they are
	// counted separately rather than folded into Removed; they are swept on
	// this trigger because they are too small and too rarely minted to
	// deserve a janitor of their own.
	TimelinesRemoved int `json:"timelinesRemoved,omitempty"`
}

// buildCaps assembles Caps from the capability-gated tables plus the
// configured optional surfaces.
func buildCaps(jobs, uploads, pid, timelines bool) Caps {
	caps := Caps{
		SchemaVersion: 1,
		Inputs:        format.Inputs(),
		Delivery: CapsDelivery{
			Progressive: true,
			HLS:         true,
			HLSFormats:  waxflow.SegmentedFormats(),
			CutFormats:  waxflow.CutFormats(),
			Jobs:        jobs,
			Uploads:     uploads,
			PID:         pid,
			Timelines:   timelines,
		},
		DSP: CapsDSP{
			// Derived from the parsers rather than restated, so an
			// advertised value that does not parse is impossible to write
			// rather than merely caught by a test.
			GainModes:         gainSpellings(),
			GainMaxDB:         gainCeilingFor(gain.PresetOff),
			GainMaxVoiceDB:    gainCeilingFor(gain.PresetVoice),
			Dynamics:          dynamicsSpellings(),
			Loudness:          []string{"analyze"},
			SilenceDetector:   silence.Version,
			TruePeakCeilingDB: gain.DefaultCeilingDB,
		},
		Profiles: make(map[string]CapsProfile, len(deliveryProfiles)),
	}
	if timelines {
		caps.Delivery.MaxTimelineMembers = timeline.MaxMembers
	}
	for _, id := range format.Decoders() {
		caps.Decoders = append(caps.Decoders, string(id))
	}
	live := make(map[string]bool)
	for _, o := range waxflow.Outputs() {
		caps.Outputs = append(caps.Outputs, CapsOutput{Name: o.Name, Live: o.Live, Exts: o.Exts})
		if o.Live {
			live[o.Name] = true
		}
	}
	segmented := make(map[string]bool)
	for _, name := range caps.Delivery.HLSFormats {
		segmented[name] = true
	}
	for name, p := range deliveryProfiles {
		p.Progressive = keep(p.Progressive, live)
		p.HLS = keep(p.HLS, segmented)
		caps.Profiles[name] = p
	}
	return caps
}

// keep filters names to those present in the capability set, preserving
// preference order.
func keep(names []string, in map[string]bool) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if in[n] {
			out = append(out, n)
		}
	}
	return out
}
