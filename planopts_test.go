package waxflow

import (
	"reflect"
	"testing"
)

// TestPlanOptsCoverage pins planOpts to TranscodeOptions: every option
// field either shapes the plan (and must appear in planOpts with the
// same name and type) or is explicitly listed here as non-shaping. A new
// option that lands in neither set fails, because a plan-shaping field
// missing from the key would let different requests share a stale plan.
func TestPlanOptsCoverage(t *testing.T) {
	nonShaping := map[string]bool{
		"FromSample": true, // seek position, normalized out of the key
		"Tags":       true, // muxer payload, never shapes the chain
		"Chapters":   true,
		"Art":        true,
		"Progress":   true, // callback
	}
	opts := reflect.TypeFor[TranscodeOptions]()
	key := reflect.TypeFor[planOpts]()
	covered := map[string]bool{}
	for i := range opts.NumField() {
		f := opts.Field(i)
		if nonShaping[f.Name] {
			continue
		}
		kf, ok := key.FieldByName(f.Name)
		if !ok {
			t.Errorf("TranscodeOptions.%s is not in planOpts and not listed as non-shaping", f.Name)
			continue
		}
		if kf.Type != f.Type {
			t.Errorf("planOpts.%s has type %s, TranscodeOptions.%s has %s", f.Name, kf.Type, f.Name, f.Type)
		}
		covered[f.Name] = true
	}
	for i := range key.NumField() {
		if name := key.Field(i).Name; !covered[name] {
			t.Errorf("planOpts.%s has no matching TranscodeOptions field", name)
		}
	}
	// planOptsOf must copy every key field; a zero-valued projection of a
	// fully populated option struct means a forgotten assignment.
	populated := TranscodeOptions{
		Format: "x", Container: "x", Rate: 1, Channels: 1, BitDepth: 1,
		GainDB: 1, FLACLevel: 1, MP3Bitrate: 1, MP3VBR: true,
		OpusBitrate: 1, AACBitrate: 1, OpusComplexity: 1, OpusVBR: true,
		OpusSignal: "x", Shaping: 1, ResampleProfile: "x",
	}
	p := reflect.ValueOf(planOptsOf(populated))
	for i := range p.NumField() {
		if p.Field(i).IsZero() {
			t.Errorf("planOptsOf leaves %s zero for a populated option struct", p.Type().Field(i).Name)
		}
	}
}
