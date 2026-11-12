package hls

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

func TestDescriptorRoundTrip(t *testing.T) {
	d := Descriptor{
		Src: "lib/a.flac", ID: "1234-5678", Format: "opus",
		Bitrate: 96, Bits: 0, Rate: 48000, Ch: 2, Gain: "track", SegDur: 4,
	}
	got, err := DecodeDescriptor(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	d.Ver = DescriptorVersion
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("round trip %+v, want %+v", got, d)
	}
}

// TestDescriptorCrossfadeRoundTrip pins that a timeline's crossfade survives the
// wire form, so a signed URL carries the blend the mint was asked for.
func TestDescriptorCrossfadeRoundTrip(t *testing.T) {
	d := Descriptor{Tl: strings.Repeat("a", 43), Format: "opus", CrossfadeSeconds: 0.25}
	got, err := DecodeDescriptor(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.CrossfadeSeconds != 0.25 {
		t.Fatalf("crossfadeSeconds round-tripped to %v, want 0.25", got.CrossfadeSeconds)
	}
}

func TestDescriptorLadder(t *testing.T) {
	d := Descriptor{Src: "s", ID: "i", Format: "opus", Bitrates: []int{64, 96, 160}}
	got, err := DecodeDescriptor(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ladder()) != 3 {
		t.Fatalf("ladder %v", got.Ladder())
	}
	v := got.Variant(96)
	if v.Bitrate != 96 || v.Bitrates != nil {
		t.Fatalf("variant %+v", v)
	}
	single := Descriptor{Src: "s", ID: "i", Format: "opus"}
	if l := single.Ladder(); len(l) != 1 || l[0] != 0 {
		t.Fatalf("default ladder %v", l)
	}
}

func TestDescriptorRejections(t *testing.T) {
	enc := func(json string) string { return base64.RawURLEncoding.EncodeToString([]byte(json)) }
	cases := map[string]string{
		"empty":            "",
		"not-base64":       "!!!",
		"not-json":         enc("nope"),
		"trailing":         enc(`{"ver":1,"src":"s","id":"i","format":"opus"}{}`),
		"unknown-field":    enc(`{"ver":1,"src":"s","id":"i","format":"opus","bogus":1}`),
		"wrong-ver":        enc(`{"ver":2,"src":"s","id":"i","format":"opus"}`),
		"no-src":           enc(`{"ver":1,"id":"i","format":"opus"}`),
		"no-id":            enc(`{"ver":1,"src":"s","format":"opus"}`),
		"no-format":        enc(`{"ver":1,"src":"s","id":"i"}`),
		"bad-bits":         enc(`{"ver":1,"src":"s","id":"i","format":"opus","bits":20}`),
		"neg-bitrate":      enc(`{"ver":1,"src":"s","id":"i","format":"opus","bitrate":-1}`),
		"neg-segdur":       enc(`{"ver":1,"src":"s","id":"i","format":"opus","segDur":-1}`),
		"zero-ladder-rung": enc(`{"ver":1,"src":"s","id":"i","format":"opus","bitrates":[96,0]}`),
		"both-bitrates":    enc(`{"ver":1,"src":"s","id":"i","format":"opus","bitrate":96,"bitrates":[64]}`),
		"oversized":        enc(`{"ver":1,"src":"` + strings.Repeat("a", maxDescriptorBytes) + `","id":"i","format":"opus"}`),
		"neg-crossfade":    enc(`{"ver":1,"src":"s","id":"i","format":"opus","crossfadeSeconds":-1}`),
		// Decoding these would substitute U+FFFD and hand back a src naming a
		// different file, so they are refused instead.
		"not-utf8":       enc("{\"ver\":1,\"src\":\"caf\xe9\",\"id\":\"i\",\"format\":\"opus\"}"),
		"lone-surrogate": enc(`{"ver":1,"src":"caf\udce9","id":"i","format":"opus"}`),
		// Every field is held to it, so a stray byte is refused here and by
		// name, not later as a changed source or an unknown format.
		"not-utf8-id":         enc("{\"ver\":1,\"src\":\"s\",\"id\":\"i\xff\",\"format\":\"opus\"}"),
		"not-utf8-format":     enc("{\"ver\":1,\"src\":\"s\",\"id\":\"i\",\"format\":\"opus\xff\"}"),
		"lone-surrogate-gain": enc(`{"ver":1,"src":"s","id":"i","format":"opus","gain":"\udce9"}`),
		// Fits the wire-form gate and blows the canonical one: json escapes
		// every & as the six-byte \u0026.
		"inflates-by-escaping": enc(`{"ver":1,"src":"s","id":"` +
			strings.Repeat("&", maxDescriptorBytes/2) + `","format":"opus"}`),
		// A crossfade blends a timeline's seam; a single source has none.
		"src-and-crossfade": enc(`{"ver":1,"src":"s","id":"i","format":"opus","crossfadeSeconds":0.5}`),
		"malformed-escape":  enc(`{"ver":1,"src":"pct:lib/100%.flac","id":"i","format":"opus"}`),
	}
	for name, v := range cases {
		if _, err := DecodeDescriptor(v); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: err %v, want invalid-request", name, err)
		}
	}
}

// TestDescriptorSizeRefusalsAreDistinct pins that the two size gates name
// their own cause. One bounds the string that arrived and the other what it
// decodes back to, and an operator reading a 400 cannot act on the difference
// unless the message states it.
func TestDescriptorSizeRefusalsAreDistinct(t *testing.T) {
	wire := descriptorWithID(t, strings.Repeat("a", 2*maxDescriptorBytes))
	canonical := descriptorWithID(t, strings.Repeat("&", maxDescriptorBytes/2))
	_, wireErr := DecodeDescriptor(wire)
	_, canonErr := DecodeDescriptor(canonical)
	if wireErr == nil || canonErr == nil {
		t.Fatalf("both must refuse: wire %v, canonical %v", wireErr, canonErr)
	}
	if wireErr.Error() == canonErr.Error() {
		t.Fatalf("both gates say %q; the two causes are indistinguishable", wireErr)
	}
	if strings.Contains(wireErr.Error(), "canonical") {
		t.Errorf("the wire-form refusal blames the canonical form: %v", wireErr)
	}
	if !strings.Contains(canonErr.Error(), "canonical") {
		t.Errorf("the canonical-form refusal does not name the canonical form: %v", canonErr)
	}
}

// descriptorWithID builds the wire form of a descriptor whose id is the
// given filler, bypassing Encode so the filler reaches the decoder verbatim.
func descriptorWithID(t *testing.T, id string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(
		[]byte(`{"ver":1,"src":"s","id":"` + id + `","format":"opus"}`))
}

// TestDescriptorCarriesAnyReference: the descriptor is JSON, which would
// turn a name that is not UTF-8 into another file's.
func TestDescriptorCarriesAnyReference(t *testing.T) {
	for _, src := range []string{"lib/caf\xe9.flac", "lib/100%\xff.flac", "lib/\xed\xa0\x80.wav", "lib/100% Hits/a.flac"} {
		d, err := DecodeDescriptor(Descriptor{Src: source.Ref(src), ID: "1-2", Format: "opus"}.Encode())
		if err != nil || string(d.Src) != src {
			t.Errorf("src %q came back %q (%v)", src, d.Src, err)
		}
	}
}

func FuzzDecodeDescriptor(f *testing.F) {
	f.Add(Descriptor{Src: "lib/a.flac", ID: "1-2", Format: "opus", Bitrates: []int{64, 96}}.Encode())
	f.Add(Descriptor{Src: "lib/caf\xe9.flac", ID: "1-2", Format: "flac"}.Encode())
	f.Add(Descriptor{Src: "s", ID: "i", Format: "flac", Bits: 24, SegDur: 6.5}.Encode())
	f.Add("AAAA")
	f.Fuzz(func(t *testing.T, s string) {
		d, err := DecodeDescriptor(s)
		if err != nil {
			return
		}
		// A decoded descriptor must survive its own canonical form, naming
		// the same source.
		again, err := DecodeDescriptor(d.Encode())
		if err != nil {
			t.Fatalf("re-encode of accepted descriptor rejected: %v", err)
		}
		if again.Src != d.Src {
			t.Fatalf("re-encode renamed the source: %q, then %q", d.Src, again.Src)
		}
	})
}
