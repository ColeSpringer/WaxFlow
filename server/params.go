package server

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// The policy clamps on requested positive gain, by declared taste; the DSP
// chain's own +-120 dB bound is correctness, these are taste. See
// gainCeilingFor.
const (
	maxGainDB      = 12
	maxVoiceGainDB = 24
)

// gainCeilingFor returns the policy clamp on requested positive gain.
//
// Music normalization is bounded at +12 dB, where anything past it is
// amplifying noise. A spoken-word dynamics preset is a declaration that the
// source is speech, which is routinely recorded far below the music norm: a
// -30 LUFS amateur podcast or audiobook cannot reach a -14 LUFS target in
// one pass under +12, it lands at -18. So the preset raises the ceiling to
// +24 dB.
//
// Both bounds are taste, which is what the +12 clamp always said of itself.
// It was never a clipping guard: the true-peak limiter has always been what
// makes any ceiling safe, and the DSP chain's own +-120 dB bound exists to
// keep the float32 factor finite and reject NaN, nothing more. So an
// argument for or against a ceiling on safety grounds is answering a
// question the code never asked.
//
// The higher ceiling is safe by construction rather than by judgement,
// because a dynamics preset always engages the limiter (see dsp.NewChain).
// That makes this a deliberate coupling of two parameters, which docs/api.md
// states plainly rather than leaving a reader to discover that gain=16 means
// 12 or 16 depending on a neighbouring parameter.
func gainCeilingFor(d gain.Preset) float64 {
	if d == gain.PresetOff {
		return maxGainDB
	}
	return maxVoiceGainDB
}

// The policy bounds on the silence= parameters. Like the gain ceilings
// above, these are taste and so they live here rather than in the engine: a
// threshold under -90 dBFS is below the noise floor of any real source and a
// minimum span past a minute is not describing silence any more, but neither
// is wrong in the way a NaN threshold is wrong, and a library caller
// analyzing synthetic content may legitimately want either. The engine
// enforces only what dsp/silence needs to be correct (finite, negative,
// positive), exactly as it bounds gain at +-120 dB and leaves the +12 dB
// taste to this layer.
const (
	minSilenceThresholdDB = -90.0
	maxSilenceMinDuration = 60 * time.Second
)

// checkSilenceOptions applies the policy bounds at the API boundary, so an
// accepted job cannot fail on request shape later.
func checkSilenceOptions(o *waxflow.SilenceOptions) error {
	// Written to fail NaN as well as out-of-range values, the dsp.NewChain
	// idiom: a non-finite threshold would otherwise slip past a > check.
	if o.ThresholdDB != 0 && !(o.ThresholdDB >= minSilenceThresholdDB && o.ThresholdDB < 0) {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence threshold %g dBFS outside %g..0", o.ThresholdDB, minSilenceThresholdDB))
	}
	// 0 is legal and means the engine default, like the threshold's 0.
	if o.MinDuration < 0 || o.MinDuration > maxSilenceMinDuration {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence minimum duration %v outside 0..%v", o.MinDuration, maxSilenceMinDuration))
	}
	return nil
}

// dynamicsOff is the wire spelling of gain.PresetOff. The kernel's
// vocabulary spells it as the empty string, but a URL and a cache key both
// need it nameable.
const dynamicsOff = "off"

// gainSpellings lists the named gain modes parseGain accepts, which is
// exactly what /caps advertises. The scalar escape hatch is deliberately
// absent: every advertised value must parse, and a "<db>" placeholder does
// not. The ceilings carry that information instead.
func gainSpellings() []string {
	return []string{"off", "track", "album"}
}

// dynamicsSpellings lists what parseDynamics accepts, which is exactly what
// /caps advertises. Deriving the advertised list from the parser rather than
// restating it is what makes TestCapsDSPIsHonest structural instead of a
// promise.
func dynamicsSpellings() []string {
	out := []string{dynamicsOff}
	for _, p := range gain.Presets() {
		out = append(out, string(p))
	}
	return out
}

// dynSpelling is the inverse: a preset's wire name, for the cache key.
func dynSpelling(p gain.Preset) string {
	if p == gain.PresetOff {
		return dynamicsOff
	}
	return string(p)
}

// parseDynamics parses the dynamics= parameter.
func parseDynamics(v string) (gain.Preset, error) {
	s := strings.ToLower(v)
	if s == "" || s == dynamicsOff {
		return gain.PresetOff, nil
	}
	for _, p := range gain.Presets() {
		if s == string(p) {
			return p, nil
		}
	}
	return "", waxerr.New(waxerr.CodeInvalidRequest,
		fmt.Sprintf("dynamics %q: want %s", v, strings.Join(dynamicsSpellings(), ", ")))
}

// maxSeekSeconds bounds t=. Beyond any real recording, and small enough
// that t times any registered rate stays far inside int64: the Go spec
// leaves out-of-range float-to-int conversion implementation-specific,
// so the bound must hold before the conversion, not be inferred after.
const maxSeekSeconds = 1e9

type gainMode int

const (
	gainOff gainMode = iota
	gainTrack
	gainAlbum
	gainFixed
)

// gainSpec is a parsed gain= parameter.
type gainSpec struct {
	mode gainMode
	db   float64
}

// resolveDB is the dB the DSP chain applies: fixed values verbatim, tag
// modes from the source's ReplayGain (album falls back to track, the usual
// player behavior), 0 when the tags or the mapper are absent. Positive gain
// clamps at gainCeilingFor(dyn), whichever mode it came from.
//
// The clamp lives here rather than in parseGain because the ceiling is a
// function of the dynamics preset, which is a different parameter: nothing
// at parse time knows yet which ceiling applies.
func (g gainSpec) resolveDB(info *meta.Info, dyn gain.Preset) float64 {
	ceiling := gainCeilingFor(dyn)
	switch g.mode {
	case gainFixed:
		return min(g.db, ceiling)
	case gainTrack:
		if db, ok := meta.TrackGainDB(info); ok {
			return min(db, ceiling)
		}
	case gainAlbum:
		if db, ok := meta.AlbumGainDB(info); ok {
			return min(db, ceiling)
		}
		if db, ok := meta.TrackGainDB(info); ok {
			return min(db, ceiling)
		}
	}
	return 0
}

// checkTimelineGain refuses the tag-derived gain modes for a timeline.
//
// A timeline is one chain, so it has one gain, and there is no honest single
// answer to read out of N members' tags. Per-track gain is worse than merely
// ambiguous: applied to a continuous timeline it steps the level at every
// seam, which is the artifact album gain exists to prevent, so the mode that
// looks most reasonable is the one that would sound wrong. Album gain is
// uniform across a real album and arbitrary across anything else, and nothing
// here can tell which a queue is.
//
// So a timeline takes gain=off or the dB the caller wants, which is what a
// caller with its own loudness measurements sends anyway. The refusal is at
// mint time, where the client can act on it, rather than at playback.
func checkTimelineGain(spelling string, g gainSpec) error {
	if g.mode != gainTrack && g.mode != gainAlbum {
		return nil
	}
	how := "gain=" + spelling
	if spelling == "" {
		// The request named no gain at all, so this is the daemon's default
		// biting. Say that, or the error names a parameter the caller can
		// look at its own request and not find.
		how = "this daemon's default gain (" + gainModeName(g.mode) + ")"
	}
	return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
		"%s resolves from one source's tags and a timeline has many; pass gain=off or a dB number", how))
}

func gainModeName(m gainMode) string {
	switch m {
	case gainTrack:
		return "track"
	case gainAlbum:
		return "album"
	case gainFixed:
		return "a dB number"
	default:
		return "off"
	}
}

func parseGain(v string, dflt gainSpec) (gainSpec, error) {
	switch strings.ToLower(v) {
	case "":
		return dflt, nil
	case "off":
		return gainSpec{mode: gainOff}, nil
	case "track":
		return gainSpec{mode: gainTrack}, nil
	case "album":
		return gainSpec{mode: gainAlbum}, nil
	}
	num := strings.TrimSuffix(strings.ToLower(v), "db")
	db, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsNaN(db) || math.IsInf(db, 0) {
		return gainSpec{}, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain %q: want off, track, album, or a dB number", v))
	}
	// Positive gain is clamped rather than refused, but not here: the
	// ceiling depends on the dynamics preset, so resolveDB applies it once
	// both parameters are known.
	return gainSpec{mode: gainFixed, db: db}, nil
}

// streamParamNames is the closed parameter surface of /stream and
// /transcode. Unknown parameters are rejected rather than ignored: a
// typo that silently changes audio is the worst parameter bug, and
// signed URLs sign every parameter anyway.
var streamParamNames = map[string]bool{
	"src": true, "format": true, "rate": true, "ch": true, "bits": true,
	"gain": true, "dynamics": true, "t": true, "track": true, "maxBitRate": true,
	"bitrate": true, "q": true, "container": true,
	"id": true, sign.ParamExp: true, sign.ParamKID: true, sign.ParamSig: true,
}

// streamParams is a parsed, policy-checked /stream request.
type streamParams struct {
	src        string
	format     string // output name or "auto"
	rate       int
	ch         int
	bits       int
	gain       gainSpec
	dynamics   gain.Preset
	t          float64 // seconds; samples are derived after probe
	track      int     // -1 when absent
	maxBitRate int     // kbit/s cap for the decision ladder; 0 none
	bitrate    int     // lossy output bit rate in kbit/s; 0 selects the default
	container  string  // container override ("adts"); "" selects the format default
	identity   string  // id= parameter, "" when absent
}

// qPreset maps the q= quality preset to a lossy CBR bit rate in kbit/s. These
// are MP3 CBR points; when a VBR codec lands, q will need a per-codec quality
// mapping rather than a shared bit-rate ladder (the encoder clamps an
// out-of-range preset to a layer-legal rate, so a preset never fails).
var qPreset = map[string]int{"low": 96, "med": 128, "high": 192}

func parseStreamParams(q url.Values, defaultGain gainSpec) (*streamParams, error) {
	bad := func(format string, args ...any) (*streamParams, error) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	for k := range q {
		if !streamParamNames[k] {
			return bad("unknown parameter %q", k)
		}
	}
	p := &streamParams{src: q.Get("src"), format: q.Get("format"), track: -1, identity: q.Get("id")}
	if p.src == "" {
		return bad("src is required")
	}
	if p.format == "" {
		p.format = "auto"
	}
	// container= overrides the format's default packaging where one
	// exists (today: aac's adts opt-out). Whether the resolved format
	// honors it is the plan's call; format=auto never resolves to a
	// format with alternates, so pairing it with auto is a caller error.
	p.container = q.Get("container")
	if p.container != "" && p.format == "auto" {
		return bad("container requires an explicit format")
	}

	var err error
	if p.rate, err = intParam(q, "rate", 0); err != nil {
		return nil, err
	}
	if p.ch, err = intParam(q, "ch", 0); err != nil {
		return nil, err
	}
	if p.bits, err = intParam(q, "bits", 0); err != nil {
		return nil, err
	}
	if p.track, err = intParam(q, "track", -1); err != nil {
		return nil, err
	}
	if p.maxBitRate, err = intParam(q, "maxBitRate", 0); err != nil {
		return nil, err
	}
	if p.rate < 0 || p.ch < 0 || p.bits < 0 || p.maxBitRate < 0 {
		return bad("rate, ch, bits, and maxBitRate must be non-negative")
	}
	if q.Has("track") && p.track < 0 {
		return bad("track must be non-negative")
	}
	switch p.bits {
	case 0, 16, 24:
		// The public contract is bits=16|24; the chain could do more,
		// but the API stays small until a need exists.
	default:
		return bad("bits %d: want 16 or 24", p.bits)
	}
	// Lossy quality selection: q= is a named preset, bitrate= an explicit
	// kbit/s rate, mutually exclusive. Whether the resolved output can honor
	// them is checked after format resolution (planTranscode).
	if q.Get("q") != "" && q.Get("bitrate") != "" {
		return bad("q and bitrate are mutually exclusive")
	}
	if v := q.Get("q"); v != "" {
		kbps, ok := qPreset[v]
		if !ok {
			return bad("q %q: want low, med, or high", v)
		}
		p.bitrate = kbps
	} else if q.Get("bitrate") != "" {
		if p.bitrate, err = intParam(q, "bitrate", 0); err != nil {
			return nil, err
		}
		if p.bitrate <= 0 {
			return bad("bitrate must be a positive kbit/s value")
		}
	}
	if p.gain, err = parseGain(q.Get("gain"), defaultGain); err != nil {
		return nil, err
	}
	if p.dynamics, err = parseDynamics(q.Get("dynamics")); err != nil {
		return nil, err
	}
	if ts := q.Get("t"); ts != "" {
		t, err := strconv.ParseFloat(ts, 64)
		if err != nil || math.IsNaN(t) || t < 0 || t > maxSeekSeconds {
			return bad("t %q: want seconds within 0..%g", ts, float64(maxSeekSeconds))
		}
		p.t = t
	}
	return p, nil
}

func intParam(q url.Values, name string, dflt int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return dflt, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("%s %q: want an integer", name, v))
	}
	return n, nil
}

// identityString is the ADR-0004 source identity: reference plus
// size-mtimeNS, exactly what signed URLs pin.
func identityString(ref string, id source.Identity) string {
	return ref + "|" + id.String()
}

// canonicalCore serializes every output-shaping parameter both cache keys
// share, in one fixed order (ADR-0004). Values are the resolved plan, not
// the raw request: "rate=48000" and an absent rate on a 48 kHz source are
// the same output and must share an entry. The plan's BitRate stands in for
// the lossy bitrate/q request so two rates never share a cache entry.
//
// The progressive and segmented keys each append what is theirs alone (a
// seek position, a segment length). Sharing the core is what stops a new
// shaping parameter from landing in one key and not the other, which is how
// two different requests come to share a stale entry.
func canonicalCore(plan *waxflow.TranscodePlan, gainDB float64, dyn gain.Preset) string {
	return fmt.Sprintf("container=%s&rate=%d&ch=%d&type=%s&bits=%d&bitrate=%d&gain=%s&dynamics=%s",
		plan.Container, plan.Format.Rate, plan.Format.Channels, plan.Format.Type,
		plan.Format.BitDepth, plan.BitRate, strconv.FormatFloat(gainDB, 'g', -1, 64), dynSpelling(dyn))
}

// canonicalParams is the progressive cache key's parameter string: the
// shared core plus the seek position.
func canonicalParams(plan *waxflow.TranscodePlan, gainDB float64, dyn gain.Preset, from int64) string {
	return fmt.Sprintf("%s&from=%d", canonicalCore(plan, gainDB, dyn), from)
}
