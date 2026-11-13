package server

import (
	"net/url"
	"regexp"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/silence"
	"github.com/colespringer/waxflow/internal/gainspec"
)

// TestDeliveryProfilesAreHonest pins the /caps profile contract: the
// four named profiles exist, every format they advertise is actually
// served by this build (live output for progressive, segmented for
// hls), the recommendation names a real surface, and every profile
// states its evidence basis. The per-cell facts live in
// docs/client-matrix.md; this keeps the wire shape mechanically inside
// the build's capabilities.
func TestDeliveryProfilesAreHonest(t *testing.T) {
	caps := buildCaps(capsFlags{jobs: true, uploads: true, pid: true, timelines: true})

	want := []string{"android-exoplayer", "apple-native", "desktop-mpv", "hls-js"}
	var got []string
	for name := range caps.Profiles {
		got = append(got, name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("profile names = %v, want %v", got, want)
	}

	live := map[string]bool{}
	for _, o := range waxflow.Outputs() {
		if o.Live {
			live[o.Name] = true
		}
	}
	segmented := map[string]bool{}
	for _, name := range caps.Delivery.HLSFormats {
		segmented[name] = true
	}

	for name, p := range caps.Profiles {
		if p.Delivery != "hls" && p.Delivery != "progressive" {
			t.Errorf("%s: delivery = %q, want hls or progressive", name, p.Delivery)
		}
		if p.Basis == "" {
			t.Errorf("%s: empty basis; profiles must state their evidence", name)
		}
		if len(p.Progressive) == 0 || len(p.HLS) == 0 {
			t.Errorf("%s: empty capability list (progressive %d, hls %d)", name, len(p.Progressive), len(p.HLS))
		}
		for _, f := range p.Progressive {
			if !live[f] {
				t.Errorf("%s: progressive advertises %q, which has no live output row", name, f)
			}
		}
		for _, f := range p.HLS {
			if !segmented[f] {
				t.Errorf("%s: hls advertises %q, which has no segmented form", name, f)
			}
		}
	}

	// The Apple recommendation steers to HLS until the progressive
	// live-transcode checklist cell is verified (the recorded
	// decision); a change here must be deliberate.
	if caps.Profiles["apple-native"].Delivery != "hls" {
		t.Error("apple-native must recommend hls (progressive live transcodes are a manual checklist cell)")
	}
}

// TestCutFormatsAreHonest pins the /caps cut advertisement to the same
// discipline as the profiles: delivery.cutFormats is non-empty and every
// format on it is one this build actually serves (a live output row), so the
// wire never offers a cut the daemon cannot take. The source-of-truth list and
// its dual-surface reachability live in waxflow.TestCutFormatsIsHonest; this
// keeps the wire shape mechanically inside the build's live outputs.
func TestCutFormatsAreHonest(t *testing.T) {
	caps := buildCaps(capsFlags{jobs: true, uploads: true, pid: true, timelines: true})

	if len(caps.Delivery.CutFormats) == 0 {
		t.Fatal("delivery.cutFormats is empty; this build serves the cut rung and must advertise it")
	}
	live := map[string]bool{}
	for _, o := range waxflow.Outputs() {
		if o.Live {
			live[o.Name] = true
		}
	}
	for _, f := range caps.Delivery.CutFormats {
		if !live[f] {
			t.Errorf("cutFormats advertises %q, which has no live output row", f)
		}
	}
}

// TestCapsDSPIsHonest pins the /caps DSP slot to the same discipline as
// TestDeliveryProfilesAreHonest: every advertised value must be one the
// daemon actually accepts, and every advertised number must be the one the
// daemon actually applies.
//
// The rule has real teeth on this slot. A literal "<db>" token cannot sit
// in GainModes to hint at the scalar escape hatch, because parseGain does
// not accept it and this test would fail; the escape hatch is expressed by
// the ceilings instead, which is what a client actually needs. And because
// gainCeilingFor makes the clamp a function of the dynamics preset, both
// ceilings have to be advertised, or a client is back to sniffing a version
// to learn whether gain=16 is legal, which is the exact failure the slot
// exists to prevent.
func TestCapsDSPIsHonest(t *testing.T) {
	dsp := buildCaps(capsFlags{jobs: true, uploads: true, pid: true, timelines: true}).DSP

	if len(dsp.GainModes) == 0 || len(dsp.Dynamics) == 0 || len(dsp.Loudness) == 0 {
		t.Fatalf("DSP slot advertises nothing: %+v", dsp)
	}
	for _, mode := range dsp.GainModes {
		if _, err := parseGain(mode, gainspec.Spec{}); err != nil {
			t.Errorf("advertised gain mode %q does not parse: %v", mode, err)
		}
	}
	// The empty spelling means "the daemon default" rather than a named
	// mode, so advertising it would be advertising a hole.
	if slices.Contains(dsp.GainModes, "") {
		t.Error("gainModes advertises the empty spelling, which means the default, not a mode")
	}
	for _, d := range dsp.Dynamics {
		if _, err := parseDynamics(d); err != nil {
			t.Errorf("advertised dynamics preset %q does not parse: %v", d, err)
		}
	}
	if !slices.Contains(dsp.Dynamics, "off") {
		t.Error("dynamics does not advertise off, which every build supports")
	}
	// Every preset the kernel implements must be reachable over the wire,
	// and nothing else may be advertised.
	for _, p := range gain.Presets() {
		if !slices.Contains(dsp.Dynamics, string(p)) {
			t.Errorf("kernel implements preset %q but /caps does not advertise it", p)
		}
	}
	if want := len(gain.Presets()) + 1; len(dsp.Dynamics) != want {
		t.Errorf("dynamics advertises %d spellings, want %d (off plus every preset)", len(dsp.Dynamics), want)
	}

	// The ceilings must be the ones the request path applies, not a copy
	// that can drift from gainCeilingFor.
	if got, want := dsp.GainMaxDB, gainCeilingFor(gain.PresetOff); got != want {
		t.Errorf("gainMaxDb = %v, but gainCeilingFor(off) = %v", got, want)
	}
	if got, want := dsp.GainMaxVoiceDB, gainCeilingFor(gain.PresetVoice); got != want {
		t.Errorf("gainMaxVoiceDb = %v, but gainCeilingFor(voice) = %v", got, want)
	}
	if dsp.GainMaxVoiceDB <= dsp.GainMaxDB {
		t.Errorf("the voice ceiling (%v) does not exceed the music ceiling (%v); "+
			"the whole point of the split is that speech is recorded lower",
			dsp.GainMaxVoiceDB, dsp.GainMaxDB)
	}
	if dsp.TruePeakCeilingDB != gain.DefaultCeilingDB {
		t.Errorf("truePeakCeilingDb = %v, but the limiter uses %v", dsp.TruePeakCeilingDB, gain.DefaultCeilingDB)
	}

	// The detector version must be the one the silence maps carry, not a
	// copy that can drift; empty would leave a map cache nothing to
	// invalidate against.
	if dsp.SilenceDetector == "" {
		t.Error("silenceDetector is empty; a caller caching silence maps has nothing to invalidate against")
	}
	if dsp.SilenceDetector != silence.Version {
		t.Errorf("silenceDetector = %q, but the detector is at %q", dsp.SilenceDetector, silence.Version)
	}

	// deliveryProfiles must stay orthogonal: profiles are about client
	// decoder support, dynamics is server-side and client-agnostic.
	for name, p := range buildCaps(capsFlags{jobs: true, uploads: true, pid: true, timelines: true}).Profiles {
		if slices.Contains(p.Progressive, "voice") || slices.Contains(p.HLS, "voice") {
			t.Errorf("profile %q lists a dynamics preset among its formats; the two are orthogonal", name)
		}
	}
}

// TestGainCeilingIsCoupledToDynamics pins the coupling itself, which is the
// surprising part of the policy and the reason it is documented rather than
// left to be discovered: the same gain= request resolves to a different dB
// depending on a neighbouring parameter.
func TestGainCeilingIsCoupledToDynamics(t *testing.T) {
	req := gainspec.Spec{Mode: gainspec.Fixed, DB: 16}
	if got := resolveGainDB(req, nil, gain.PresetOff); got != maxGainDB {
		t.Errorf("gain=16 with no dynamics resolved to %v, want the music ceiling %v", got, float64(maxGainDB))
	}
	if got := resolveGainDB(req, nil, gain.PresetVoice); got != 16 {
		t.Errorf("gain=16 with dynamics=voice resolved to %v, want 16 (under the %v voice ceiling)",
			got, float64(maxVoiceGainDB))
	}
	// The voice ceiling still clamps: it is a higher bound, not no bound.
	over := gainspec.Spec{Mode: gainspec.Fixed, DB: 40}
	if got := resolveGainDB(over, nil, gain.PresetVoice); got != maxVoiceGainDB {
		t.Errorf("gain=40 with dynamics=voice resolved to %v, want the voice ceiling %v", got, float64(maxVoiceGainDB))
	}
	// Negative gain is never clamped by either ceiling.
	down := gainspec.Spec{Mode: gainspec.Fixed, DB: -30}
	if got := resolveGainDB(down, nil, gain.PresetVoice); got != -30 {
		t.Errorf("gain=-30 resolved to %v, want -30: the ceiling bounds positive gain only", got)
	}
}

// capsNoteParam finds a request parameter spelled out in a profile note.
var capsNoteParam = regexp.MustCompile(`\b(bits|rate|ch|format|bitrate|q|container|hev2)=([A-Za-z0-9.]+)`)

// refusedInNote lists the parameters note spells out that /stream refuses.
// Each is parsed beside a src, with the note's other parameters at the
// first value the note gives them.
func refusedInNote(note string) []string {
	var refused []string
	tokens := capsNoteParam.FindAllStringSubmatch(note, -1)
	for _, tok := range tokens {
		q := url.Values{"src": {"lib/a.flac"}}
		for _, other := range tokens {
			if !q.Has(other[1]) {
				q.Set(other[1], other[2])
			}
		}
		q.Set(tok[1], tok[2])
		if _, err := parseStreamParams(q, gainspec.Spec{}); err != nil {
			refused = append(refused, tok[0])
		}
	}
	return refused
}

// TestCapsNotesNameOnlyAcceptedValues: a note that spells a parameter out
// is advice a client acts on, so the value it names has to be one /stream
// takes. The hls-js profile once told clients that bits=12 and bits=20 are
// honored, which /stream has never accepted; that note is gone.
func TestCapsNotesNameOnlyAcceptedValues(t *testing.T) {
	for name, p := range buildCaps(capsFlags{jobs: true, uploads: true, pid: true, timelines: true}).Profiles {
		for _, note := range p.Notes {
			if refused := refusedInNote(note); len(refused) > 0 {
				t.Errorf("profile %q tells a client about %v, which /stream refuses\nnote: %s", name, refused, note)
			}
		}
	}
	// No note spells a parameter out today, so the check is shown to bite
	// on the sentence it was written for and to pass one that is true.
	retired := "an explicit bits=12 or bits=20 is honored and mints a stream Chromium cannot play"
	if got := refusedInNote(retired); !slices.Equal(got, []string{"bits=12", "bits=20"}) {
		t.Errorf("the retired note's refused parameters = %v, want bits=12 and bits=20", got)
	}
	if got := refusedInNote("ask for format=flac with bits=16 or bits=24, or rate=48000"); len(got) > 0 {
		t.Errorf("a note of accepted values is flagged: %v", got)
	}
}
