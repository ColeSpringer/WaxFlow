package server

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/internal/gainspec"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/internal/timeline"
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

// dynamicsOff is the wire spelling of gain.PresetOff. The kernel's
// vocabulary spells it as the empty string, but a URL and a cache key both
// need it nameable.
const dynamicsOff = "off"

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

// seekSample is t= as a sample at rate: the nearest one, since t*rate in
// float64 can fall just short of a whole sample (0.009 s at 48 kHz is
// 431.99999999999994), and a client seeking to a timeline member's start sends
// that start's offset over the envelope rate.
func seekSample(t float64, rate int) int64 {
	return int64(math.Round(t * float64(rate)))
}

// resolveGainDB is the dB the DSP chain applies: fixed values verbatim, tag
// modes from the source's ReplayGain (album falls back to track, the usual
// player behavior), 0 when the tags or the mapper are absent. Positive gain
// clamps at gainCeilingFor(dyn), whichever mode it came from.
//
// The clamp lives here rather than in parseGain because the ceiling is a
// function of the dynamics preset, which is a different parameter: nothing
// at parse time knows yet which ceiling applies.
func resolveGainDB(g gainspec.Spec, info *meta.Info, dyn gain.Preset) float64 {
	ceiling := gainCeilingFor(dyn)
	switch g.Mode {
	case gainspec.Fixed:
		return min(g.DB, ceiling)
	case gainspec.Track:
		if db, ok := meta.TrackGainDB(info); ok {
			return min(db, ceiling)
		}
	case gainspec.Album:
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
func checkTimelineGain(spelling string, g gainspec.Spec) error {
	if g.Mode != gainspec.Track && g.Mode != gainspec.Album {
		return nil
	}
	how := "gain=" + spelling
	if spelling == "" {
		// The request named no gain at all, so this is the daemon's default
		// biting. Say that, or the error names a parameter the caller can
		// look at its own request and not find.
		how = "this daemon's default gain (" + gainModeName(g.Mode) + ")"
	}
	return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
		"%s resolves from one source's tags and a timeline has many; pass gain=off or a dB number", how))
}

func gainModeName(m gainspec.Mode) string {
	switch m {
	case gainspec.Track:
		return "track"
	case gainspec.Album:
		return "album"
	case gainspec.Fixed:
		return "a dB number"
	default:
		return "off"
	}
}

// parseGain reads a gain= value by the grammar gainspec.Parse holds; the
// empty value is dflt.
func parseGain(v string, dflt gainspec.Spec) (gainspec.Spec, error) {
	if v == "" {
		return dflt, nil
	}
	return gainspec.Parse(v)
}

// streamParamNames is the closed parameter surface of /stream and
// /transcode. Unknown parameters are rejected rather than ignored: a
// typo that silently changes audio is the worst parameter bug, and
// signed URLs sign every parameter anyway.
var streamParamNames = map[string]bool{
	"src": true, "format": true, "rate": true, "ch": true, "bits": true,
	"gain": true, "dynamics": true, "t": true, "track": true, "maxBitRate": true,
	"bitrate": true, "q": true, "hev2": true, "container": true, "from": true, "to": true,
	"id": true, "tl": true, "crossfadeSeconds": true,
	sign.ParamExp: true, sign.ParamKID: true, sign.ParamSig: true,
}

// singleSourceParams are the parameters only a single source can mean, each
// with why a timeline refuses it. A fixed order, so a URL carrying several
// always gets the same refusal.
var singleSourceParams = []struct{ name, why string }{
	{"src", "a URL names one stream or one timeline"},
	{"from", "a member's window is minted into the timeline"},
	{"to", "a member's window is minted into the timeline"},
	{"id", "the digest pins every member's identity"},
	{"track", "a timeline plays each member's default track"},
}

// probeParamNames is the closed parameter surface of GET /probe, the same
// rule streamParamNames states. POST /probe has always been strict
// (decodeJSONBody rejects unknown fields); the GET branch read the two it
// knew and ignored the rest.
var probeParamNames = map[string]bool{"src": true, "strict": true}

// span is a request's sample window over the source timeline: the virtual
// track A11 asks for, one offset range of one file served as a stream in
// its own right.
//
// It is expressed in samples, not seconds, and that is load-bearing rather
// than a taste. ADR-0006's "seconds convert at the API boundary" is the
// right convention for a seek, where a sample either way is meaningless. A
// span is not a seek: it declares which samples are this track, so it is
// content identity. A CUE boundary at 245.32 s is not exactly representable
// in binary, and 245.32*44100 floors to 10818611 rather than 10818612,
// putting a one-sample error at every track boundary of a gapless album.
// That is the exact failure the CD-frame arithmetic in the cue package exists
// to prevent, and seconds here would quietly undo it.
//
// So the two coexist with different jobs and compose: from and to are
// samples on the source timeline, t= stays seconds and seeks within the
// span.
type span struct {
	// from is the window's first sample.
	from int64
	// to is one past the window's last sample, 0 when it runs to the end.
	//
	// Zero rather than waxflow.ToEnd so that the zero value of a span is
	// the whole source. A sentinel that had to be set explicitly would make
	// "nobody set this" mean "empty window", which is the one answer no
	// caller ever wants and the one a forgotten initializer would produce.
	to int64
}

// narrowed reports whether the span bounds the source at all.
func (sp span) narrowed() bool { return sp.from > 0 || sp.to > 0 }

// end is the window's end in Slice's spelling, where an open end is ToEnd.
func (sp span) end() int64 {
	if sp.to <= 0 {
		return waxflow.ToEnd
	}
	return sp.to
}

// srcParam reads a request's src: the reference's own bytes, out of the
// text form the parameter carries. Every reader of src goes through it
// (TestSrcIsReadInOnePlace), since one that did not would resolve pct: text.
func srcParam(q url.Values) (string, error) {
	ref, err := source.ParseRef(q.Get("src"))
	return string(ref), err
}

// streamParams is a parsed, policy-checked /stream request.
type streamParams struct {
	// src is the reference's own bytes, read from its text form.
	src string
	// tl is a minted timeline's digest, "" for a single source; the two are
	// exclusive.
	tl string
	// crossfade is the timeline's seam blend in seconds, 0 for a butt-join.
	crossfade float64
	// shape is the output shape as sent, its format "auto" when absent.
	shape outshape.Params
	// q is the quality preset as sent, "" when absent; the resolved format
	// says what it selects.
	q          string
	gain       gainspec.Spec
	gainParam  string // gain= as sent, "" when absent, which a timeline's refusal names
	dynamics   gain.Preset
	t          float64 // seconds; samples are derived after probe
	span       span    // source-sample window; the zero value is the whole source
	track      int     // -1 when absent
	maxBitRate int     // kbit/s cap for the decision ladder; 0 (absent) is no cap, an explicit 0 is refused
	identity   string  // id= parameter, "" when absent
	pinned     bool    // id= is present, empty or not
}

func parseStreamParams(q url.Values, defaultGain gainspec.Spec) (*streamParams, error) {
	bad := func(format string, args ...any) (*streamParams, error) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	for k := range q {
		if !streamParamNames[k] {
			return bad("unknown parameter %q", k)
		}
	}
	src, err := srcParam(q)
	if err != nil {
		return nil, err
	}
	p := &streamParams{src: src, tl: q.Get("tl"), track: -1, identity: q.Get("id"), pinned: q.Has("id"), gainParam: q.Get("gain")}
	if q.Has("tl") {
		switch {
		case p.tl == "":
			return bad("tl is present but empty; omit it to stream one src")
		case !timeline.ValidDigest(p.tl):
			return bad("tl %q is not a timeline digest", p.tl)
		}
		for _, sp := range singleSourceParams {
			if q.Has(sp.name) {
				return bad("tl and %s are exclusive: %s", sp.name, sp.why)
			}
		}
	} else if p.src == "" {
		return bad("src is required")
	}
	x, err := parseSeconds("crossfadeSeconds", "for a butt-join", q.Get("crossfadeSeconds"), q.Has("crossfadeSeconds"))
	if err != nil {
		return nil, err
	}
	// Refused rather than ignored, as on the HLS descriptor: one source has
	// no seam, so a crossfade on it would silently do nothing.
	if x > 0 && p.tl == "" {
		return bad("crossfadeSeconds %g on a src: a crossfade blends a timeline's seams, and one source has none", x)
	}
	p.crossfade = x
	get := func(name string) (string, bool) { return q.Get(name), q.Has(name) }
	// q stays as sent until the format resolves: what it selects is the
	// format's (streamOptions).
	if p.shape, p.q, err = outshape.ParseQuery(get); err != nil {
		return nil, err
	}
	if p.shape.Format == "" {
		p.shape.Format = "auto"
	}
	// container= overrides the format's default packaging where one
	// exists (today: aac's adts opt-out). Whether the resolved format
	// honors it is the plan's call; format=auto never resolves to a
	// format with alternates, so pairing it with auto is a caller error.
	if p.shape.Container != "" && p.shape.Format == "auto" {
		return bad("container requires an explicit format")
	}
	if p.track, err = intParam(q, "track", -1); err != nil {
		return nil, err
	}
	// Omitting maxBitRate is the only spelling of no cap, as with track below
	// and to=0 in parseSpan; a value is a kbit/s rate, refused as bitrate is.
	if v := q.Get("maxBitRate"); q.Has("maxBitRate") {
		if v == "" {
			return bad("maxBitRate is present but empty; omit it for no cap")
		}
		if p.maxBitRate, err = strconv.Atoi(v); err != nil || p.maxBitRate <= 0 {
			return bad("maxBitRate %q: want a positive integer", v)
		}
	}
	if p.maxBitRate > outshape.MaxKbps {
		return bad("maxBitRate %d: want at most %d kbit/s", p.maxBitRate, outshape.MaxKbps)
	}
	// track has its own message: 0 is a legal track, so it refuses at
	// negative.
	if q.Has("track") {
		if q.Get("track") == "" {
			return bad("track is present but empty; omit it to take the source's default track")
		}
		if p.track < 0 {
			return bad("track must be non-negative")
		}
	}
	if p.gain, err = parseGain(q.Get("gain"), defaultGain); err != nil {
		return nil, err
	}
	if p.dynamics, err = parseDynamics(q.Get("dynamics")); err != nil {
		return nil, err
	}
	if ts, ok := get("t"); ok {
		if ts == "" {
			return bad("t is present but empty; omit it to start at the beginning")
		}
		t, err := strconv.ParseFloat(ts, 64)
		if err != nil || math.IsNaN(t) || t < 0 || t > maxSeekSeconds {
			return bad("t %q: want seconds within 0..%g", ts, float64(maxSeekSeconds))
		}
		p.t = t
	}
	if p.span, err = parseSpan(get); err != nil {
		return nil, err
	}
	return p, nil
}

// parseSpan parses the from/to virtual-track window from get. Both are source
// samples and to is exclusive; either may be absent, and neither empty.
//
// The window is only checked for internal sense here (non-negative,
// ascending). Whether it fits the source is SpanTrack's call, which needs
// the probed length and makes it in one place for both the plan and the
// run.
func parseSpan(get func(name string) (string, bool)) (span, error) {
	bad := func(format string, args ...any) (span, error) {
		return span{}, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	var sp span
	var err error
	if fromV, ok := get("from"); ok {
		if fromV == "" {
			return bad("from is present but empty; omit it to start at the source's start")
		}
		if sp.from, err = strconv.ParseInt(fromV, 10, 64); err != nil || sp.from < 0 {
			return bad("from %q: want a non-negative sample offset on the source timeline", fromV)
		}
	}
	if toV, ok := get("to"); ok {
		if toV == "" {
			return bad("to is present but empty; omit it to run to the source's end")
		}
		if sp.to, err = strconv.ParseInt(toV, 10, 64); err != nil || sp.to <= 0 {
			// Zero is refused rather than read as "the whole source": to is
			// exclusive, so to=0 spells the empty span, and a caller who
			// means the whole source omits the parameter.
			return bad("to %q: want a positive sample offset on the source timeline, exclusive", toV)
		}
	}
	if sp.to > 0 && sp.to <= sp.from {
		return bad("span [%d, %d) ends before it starts", sp.from, sp.to)
	}
	return sp, nil
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
// a bit rate request (bitrate, or q on a format that takes a bit rate) so two
// rates never share a cache entry.
//
// The progressive and segmented keys each append what is theirs alone: the
// progressive key a seek position and a Vorbis quality (no segmented form
// encodes vorbis), the segmented key a segment length. Sharing the core is
// what stops a new shaping parameter from landing in one key and not the
// other, which is how two different requests come to share a stale entry.
//
// The span belongs here rather than in either key alone, and that is the
// same rule: two different windows of one file are two different streams,
// so a key that does not name the window would serve one span's bytes for
// another's request. Both surfaces carry a span, so both must name it.
// hev2 is the resolved HEAACv2 option (only ever true for he-aac output,
// which refuses it elsewhere): v1 and v2 at one explicit bitrate share
// every plan-visible value, so without this term they would share an
// entry across two different codecs.
func canonicalCore(plan *waxflow.TranscodePlan, gainDB float64, dyn gain.Preset, sp span, hev2 bool) string {
	return fmt.Sprintf("container=%s&rate=%d&ch=%d&type=%s&bits=%d&bitrate=%d&hev2=%t&gain=%s&dynamics=%s&from=%d&to=%d",
		plan.Container, plan.Format.Rate, plan.Format.Channels, plan.Format.Type,
		plan.Format.BitDepth, plan.BitRate, hev2, strconv.FormatFloat(gainDB, 'g', -1, 64), dynSpelling(dyn),
		sp.from, sp.end())
}

// canonicalParams is the progressive cache key's parameter string: the
// shared core plus the seek position, and the Vorbis quality when one is set,
// spelled as vorbis.EncoderVersion spells it.
//
// The seek is spelled seek=, not from=, because from= now names the span's
// start in the shared core and the two are different quantities: the span
// says which samples are this track, the seek says where inside it playback
// begins. Spelling both from= would put the term in one key twice, and the
// names now match the wire (from/to are parameters, the seek is t=).
func canonicalParams(plan *waxflow.TranscodePlan, gainDB float64, dyn gain.Preset, sp span, hev2 bool, seek int64, vorbisQuality float64) string {
	s := fmt.Sprintf("%s&seek=%d", canonicalCore(plan, gainDB, dyn, sp, hev2), seek)
	if vorbisQuality != 0 {
		s += "&vq=" + strconv.FormatFloat(vorbisQuality, 'g', -1, 64)
	}
	return s
}
