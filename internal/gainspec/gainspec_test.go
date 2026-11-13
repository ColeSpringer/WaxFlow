package gainspec

import (
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Spec
	}{
		{"off", Spec{Mode: Off}},
		{"track", Spec{Mode: Track}},
		{"album", Spec{Mode: Album}},
		{"ALBUM", Spec{Mode: Album}},
		{"-3", Spec{Mode: Fixed, DB: -3}},
		{"+4.5", Spec{Mode: Fixed, DB: 4.5}},
		{"0", Spec{Mode: Fixed}},
		{"6dB", Spec{Mode: Fixed, DB: 6}},
		{"-1.5db", Spec{Mode: Fixed, DB: -1.5}},
		// Positive gain is not clamped here: the ceiling depends on another
		// parameter.
		{"40", Spec{Mode: Fixed, DB: 40}},
	} {
		got, err := Parse(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

// TestParseRefuses: a spelling that is no mode and no finite number is an
// invalid request. The empty string is among them: it means a caller's
// default, which is the caller's to supply.
func TestParseRefuses(t *testing.T) {
	for _, in := range []string{"", "loud", "tracks", "NaN", "Inf", "-Inf", "+Infinity", "nandb", "3 dB", "dB", "0x10"} {
		got, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) = %+v, want a refusal", in, got)
			continue
		}
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("Parse(%q) refused with %s, want invalid-request", in, waxerr.CodeOf(err))
		}
	}
}

// TestSpellingsParse: every named mode parses to a mode of its own, none of
// them the fixed one.
func TestSpellingsParse(t *testing.T) {
	seen := map[Mode]string{}
	for _, name := range Spellings() {
		spec, err := Parse(name)
		if err != nil {
			t.Errorf("Spellings lists %q, which Parse refuses: %v", name, err)
			continue
		}
		if spec.Mode == Fixed {
			t.Errorf("%q parsed as a dB number", name)
		}
		if prev, dup := seen[spec.Mode]; dup {
			t.Errorf("%q and %q are one mode", prev, name)
		}
		seen[spec.Mode] = name
	}
	if len(seen) != 3 {
		t.Errorf("Spellings names %d modes, want off, track and album", len(seen))
	}
}
