package flac_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/flac"
)

// TestSettle: the frames decide a FLAC's length, and what STREAMINFO said is
// noted, warned or confirmed, with the header restated where its total was
// wrong and its MD5 dropped where the stream starts past frame 0.
func TestSettle(t *testing.T) {
	header := func(total int64) []byte {
		si := testStreamInfo
		si.Samples = total
		for i := range si.MD5 {
			si.MD5[i] = byte(i + 1)
		}
		b, err := si.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name                string
		total, origin, span int64
		samples             int64
		measured, restated  bool
		notes               []string
		damage              string
		md5                 bool
		truncated           int64
	}{
		{"nothing found or declared", 0, 0, -1, -1, false, false, nil, "", true, 0},
		{"nothing found", 5000, 0, -1, 5000, false, false, nil, "", true, 0},
		{"confirmed", 5000, 0, 5000, 5000, true, false, nil, "", true, 0},
		{"no claim", 0, 0, 5000, 5000, true, false, nil, "", true, 0},
		{"a shortfall", 5000, 0, 4000, 4000, true, true, nil, "frames end at 4000", false, 1000},
		{"an overrun", 5000, 0, 6000, 6000, true, true, []string{"frames run to 6000"}, "", false, 0},
		{"a cut's source length", 5000, 1000, 4000, 4000, true, true,
			[]string{"starts at sample 1000", "its source's length"}, "", false, 0},
		{"a cut with its own length", 4000, 1000, 4000, 4000, true, false,
			[]string{"starts at sample 1000"}, "", false, 0},
		// A cut that is short as well: the least its total can mean is its
		// source's length, which the frames end 500 short of.
		{"a cut short of its source's length", 5000, 1000, 3500, 3500, true, true,
			[]string{"starts at sample 1000"}, "frames end at 3500", false, 500},
		// Frames past where a source of that length ends: the total is the
		// cut's own, and they end 200 short of it.
		{"a cut short of its own length", 4000, 1000, 3800, 3800, true, true,
			[]string{"starts at sample 1000"}, "frames end at 3800", false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := header(tc.total)
			in := bytes.Clone(raw)
			st, err := flac.Settle(raw, tc.origin, tc.span)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, in) {
				t.Fatal("Settle wrote to its input")
			}
			if st.Samples != tc.samples || st.Measured != tc.measured {
				t.Errorf("%d samples (measured %v), want %d (%v)", st.Samples, st.Measured, tc.samples, tc.measured)
			}
			if len(st.Notes) != len(tc.notes) {
				t.Fatalf("notes %q, want ones naming %q", st.Notes, tc.notes)
			}
			for i, n := range tc.notes {
				if !strings.Contains(st.Notes[i], n) {
					t.Errorf("note %q, want one naming %q", st.Notes[i], n)
				}
			}
			if (st.Damage == "") != (tc.damage == "") || !strings.Contains(st.Damage, tc.damage) {
				t.Errorf("damage %q, want %q", st.Damage, tc.damage)
			}
			if st.Truncated != tc.truncated {
				t.Errorf("truncated by %d samples, want %d", st.Truncated, tc.truncated)
			}
			si, err := flac.ParseStreamInfo(st.StreamInfo)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int64{true: tc.span, false: tc.total}[tc.restated]; si.Samples != want {
				t.Errorf("the header states %d samples, want %d", si.Samples, want)
			}
			if kept := si.MD5 != [16]byte{}; kept != tc.md5 {
				t.Errorf("the header keeps its MD5: %v, want %v", kept, tc.md5)
			}
		})
	}
}
