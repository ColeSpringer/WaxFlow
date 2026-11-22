package waxflow

import (
	"reflect"
	"testing"
)

// TranscodeOptions stays a comparable value, so a plan can be keyed on it
// whole. A payload that rides along with one run is a RunOption instead.
var _ = map[TranscodeOptions]struct{}{}

// TestTranscodeOptionsHoldsOnlyValues: every option is a bool, an integer, a
// float or a string. A pointer or an interface still compiles as a map key,
// and would key the plan cache by identity.
func TestTranscodeOptionsHoldsOnlyValues(t *testing.T) {
	for f := range reflect.TypeFor[TranscodeOptions]().Fields() {
		switch f.Type.Kind() {
		case reflect.Bool, reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
		default:
			t.Errorf("TranscodeOptions.%s has kind %s; an option is a bool, an integer, a float or a string",
				f.Name, f.Type.Kind())
		}
	}
}
