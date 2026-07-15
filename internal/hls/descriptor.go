// Package hls is the service plumbing behind the HLS surface: the v=
// descriptor every HLS URL carries, the playlist writers, and the variant
// worker manager. The server package stays transport-only on top of it;
// nothing here is sample-shaped (the engine owns segments).
package hls

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/colespringer/waxflow/internal/timeline"
	"github.com/colespringer/waxflow/waxerr"
)

// DescriptorVersion is the descriptor schema version. The explicit field
// keeps the schema extensible (the album-session seed rides on it);
// decoding rejects anything else.
const DescriptorVersion = 1

// maxDescriptorBytes bounds the decoded JSON: real descriptors are under
// 300 bytes, and the parser must not inflate hostile input.
const maxDescriptorBytes = 4096

// Descriptor is the deserialized v= parameter: one variant's (or, with
// Bitrates, one ladder's) complete output selection plus the source
// identity, so every HLS URL is stateless and self-contained. Segments
// regenerate after eviction or restart from nothing but this.
//
// A multi-source timeline is the one thing that does not fit inside the URL,
// and it names a stored digest (Tl) instead. That is confined to Tl on
// purpose: a single-track stream keeps Src and ID and keeps the documented
// guarantee whole, which is the shape 99% of URLs have and which gains
// nothing from a store.
type Descriptor struct {
	// Ver is the schema version, always DescriptorVersion.
	Ver int `json:"ver"`
	// Src is the source reference (rootName/rel/path or upload:<id>) of a
	// single-track stream. Exactly one of Src and Tl is set.
	Src string `json:"src,omitempty"`
	// ID is Src's ADR-0003 source identity (size-mtimeNS). Embedded so a
	// stale URL can never serve surprise bytes: mismatch means 410. A
	// timeline needs no counterpart, because its members' identities are
	// inside the digest that names it.
	ID string `json:"id,omitempty"`
	// Tl is the digest of a stored multi-source timeline. Exactly one of Src
	// and Tl is set: these are not two spellings of one fact, the way
	// Bitrate and Bitrates are, but two different things a URL can name.
	Tl string `json:"tl,omitempty"`
	// Format is the output format name (an HLS-capable output row).
	Format string `json:"format"`
	// Bitrate is the variant's lossy bit rate in kbit/s, 0 for the
	// encoder default (or a lossless format).
	Bitrate int `json:"bitrate,omitempty"`
	// Bitrates is the master-playlist ladder in kbit/s; only master URLs
	// carry it, and each variant gets one entry as its Bitrate.
	Bitrates []int `json:"bitrates,omitempty"`
	// Bits selects the lossless output bit depth (16 or 24), 0 keeps.
	Bits int `json:"bits,omitempty"`
	// Rate resamples to this rate in Hz, 0 keeps (the encoder may still
	// impose its own, as Opus does).
	Rate int `json:"rate,omitempty"`
	// Ch converts the channel count, 0 keeps.
	Ch int `json:"ch,omitempty"`
	// Gain is the gain parameter exactly as /stream spells it (off,
	// track, album, or a dB number); empty means the daemon default.
	Gain string `json:"gain,omitempty"`
	// Dynamics is the dynamics preset exactly as /stream spells it (off
	// or voice); empty means off. It shapes the samples, so it is part of
	// the variant's identity.
	Dynamics string `json:"dynamics,omitempty"`
	// SegDur is the target segment duration in seconds, 0 for the
	// default. The plan snaps it to whole encoder frames, so it is part
	// of the variant's identity: a different SegDur is a different
	// segment numbering.
	SegDur float64 `json:"segDur,omitempty"`
	// From and To bound the stream to a sample range of Src: the virtual
	// track. To is exclusive, 0 meaning the end of the source. They are
	// exclusive with Tl, since a span narrows one file and a timeline is
	// several.
	//
	// Samples rather than seconds, and int64 rather than SegDur's float64,
	// for a reason that does not apply to SegDur: a span declares which
	// samples are this track, so it is content identity, while SegDur is a
	// target the plan snaps anyway. A CUE boundary at 245.32 s is not
	// exactly representable in binary and would land a sample off at every
	// track boundary of a gapless album.
	From int64 `json:"from,omitempty"`
	To   int64 `json:"to,omitempty"`
}

// Encode serializes the descriptor as base64url JSON for the v=
// parameter.
func (d Descriptor) Encode() string {
	d.Ver = DescriptorVersion
	b, err := json.Marshal(d)
	if err != nil {
		// The struct is plain data; Marshal cannot fail on it.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Variant returns the single-variant descriptor for one ladder rung: the
// master descriptor with Bitrate pinned and the ladder dropped.
func (d Descriptor) Variant(bitrate int) Descriptor {
	d.Bitrate = bitrate
	d.Bitrates = nil
	return d
}

// Ladder returns the bitrate rungs a master playlist lists: Bitrates when
// present, else the single Bitrate (0 meaning the encoder default).
func (d Descriptor) Ladder() []int {
	if len(d.Bitrates) > 0 {
		return slices.Clone(d.Bitrates)
	}
	return []int{d.Bitrate}
}

// DecodeDescriptor parses and validates a v= parameter. Unknown fields
// are rejected like unknown query parameters: a typo that silently
// changes audio is the worst parameter bug, and the signature covers the
// whole string anyway.
func DecodeDescriptor(s string) (Descriptor, error) {
	var d Descriptor
	bad := func(format string, args ...any) (Descriptor, error) {
		return Descriptor{}, waxerr.New(waxerr.CodeInvalidRequest, "hls: descriptor "+fmt.Sprintf(format, args...))
	}
	if s == "" {
		return bad("missing")
	}
	if len(s) > maxDescriptorBytes*4/3+4 {
		return bad("too large")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return bad("is not base64url: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return bad("is not valid JSON: %v", err)
	}
	if dec.More() {
		return bad("has trailing data")
	}
	switch {
	case d.Ver != DescriptorVersion:
		return bad("version %d, want %d", d.Ver, DescriptorVersion)
	case d.Src == "" && d.Tl == "":
		return bad("names neither a src nor a tl")
	case d.Src != "" && d.Tl != "":
		return bad("names both a src and a tl; a URL is one stream or one timeline")
	case d.Src != "" && d.ID == "":
		return bad("has no source identity")
	case d.Tl != "" && d.ID != "":
		// A timeline's members carry their identities inside its digest, so
		// an id here would be a second, unchecked one: it could only ever
		// disagree with what the digest already pins.
		return bad("names a tl and an id; a timeline's identities are inside its digest")
	case d.Tl != "" && !timeline.ValidDigest(d.Tl):
		return bad("tl %q is not a timeline digest", d.Tl)
	case d.Format == "":
		return bad("has no format")
	case d.Bitrate < 0:
		return bad("bitrate %d is negative", d.Bitrate)
	case d.Bits != 0 && d.Bits != 16 && d.Bits != 24:
		return bad("bits %d: want 16 or 24", d.Bits)
	case d.Rate < 0 || d.Ch < 0:
		return bad("rate and ch must be non-negative")
	case d.SegDur < 0:
		return bad("segDur %g is negative", d.SegDur)
	case d.From < 0:
		return bad("from %d is negative", d.From)
	case d.To < 0:
		return bad("to %d is negative", d.To)
	case d.To > 0 && d.To <= d.From:
		return bad("span [%d, %d) ends before it starts", d.From, d.To)
	case d.Tl != "" && (d.From > 0 || d.To > 0):
		// A span narrows one file; a timeline is several. Spanning a
		// timeline would be a coherent thing to want one day, but it would
		// have to address the timeline's own envelope, which is not what
		// these samples mean.
		return bad("names a tl and a span; a span bounds one source")
	}
	for _, b := range d.Bitrates {
		if b <= 0 {
			return bad("ladder bitrate %d is not positive", b)
		}
	}
	if len(d.Bitrates) > 0 && d.Bitrate != 0 {
		return bad("carries both bitrate and bitrates")
	}
	return d, nil
}
