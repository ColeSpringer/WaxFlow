package server

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// maxGainDB is the policy clamp on requested positive gain; the DSP
// chain's own +-120 dB bound is correctness, this is taste.
const maxGainDB = 12

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
// modes from the source's ReplayGain (album falls back to track, the
// usual player behavior), 0 when the tags or the mapper are absent.
// Tag-sourced positive gain clamps at the same +12 dB policy bound as
// fixed gain.
func (g gainSpec) resolveDB(info *meta.Info) float64 {
	switch g.mode {
	case gainFixed:
		return g.db
	case gainTrack:
		if db, ok := meta.TrackGainDB(info); ok {
			return min(db, maxGainDB)
		}
	case gainAlbum:
		if db, ok := meta.AlbumGainDB(info); ok {
			return min(db, maxGainDB)
		}
		if db, ok := meta.TrackGainDB(info); ok {
			return min(db, maxGainDB)
		}
	}
	return 0
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
	// Positive gain is clamped, not refused: the +12 dB policy bound.
	db = min(db, maxGainDB)
	return gainSpec{mode: gainFixed, db: db}, nil
}

// streamParamNames is the closed parameter surface of /stream and
// /transcode. Unknown parameters are rejected rather than ignored: a
// typo that silently changes audio is the worst parameter bug, and
// signed URLs sign every parameter anyway.
var streamParamNames = map[string]bool{
	"src": true, "format": true, "rate": true, "ch": true, "bits": true,
	"gain": true, "t": true, "track": true, "maxBitRate": true,
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

// canonicalParams serializes every output-shaping parameter in one fixed
// order for the cache key (ADR-0004). Values are the resolved plan, not
// the raw request: "rate=48000" and an absent rate on a 48 kHz source
// are the same output and must share an entry. The plan's BitRate stands in
// for the lossy bitrate/q request so two rates never share a cache entry.
func canonicalParams(plan *waxflow.TranscodePlan, gainDB float64, from int64) string {
	return fmt.Sprintf("container=%s&rate=%d&ch=%d&type=%s&bits=%d&bitrate=%d&gain=%s&from=%d",
		plan.Container, plan.Format.Rate, plan.Format.Channels, plan.Format.Type,
		plan.Format.BitDepth, plan.BitRate, strconv.FormatFloat(gainDB, 'g', -1, 64), from)
}
