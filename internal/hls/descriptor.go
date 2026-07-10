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
type Descriptor struct {
	// Ver is the schema version, always DescriptorVersion.
	Ver int `json:"ver"`
	// Src is the source reference (rootName/rel/path or upload:<id>).
	Src string `json:"src"`
	// ID is the ADR-0003 source identity (size-mtimeNS). Embedded so a
	// stale URL can never serve surprise bytes: mismatch means 410.
	ID string `json:"id"`
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
	// SegDur is the target segment duration in seconds, 0 for the
	// default. The plan snaps it to whole encoder frames, so it is part
	// of the variant's identity: a different SegDur is a different
	// segment numbering.
	SegDur float64 `json:"segDur,omitempty"`
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
	case d.Src == "":
		return bad("has no src")
	case d.ID == "":
		return bad("has no source identity")
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
