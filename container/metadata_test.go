package container_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestSplitNumberTotal pins the pair forms a number key holds, against the
// tag library's own rule (tag.NumberTotalSplit), which a mapper-wired read
// applies to the same values.
func TestSplitNumberTotal(t *testing.T) {
	for _, tc := range []struct {
		in, num, total string
		ok             bool
	}{
		{"3/12", "3", "12", true},
		{" 03 / 012 ", "03", "012", true},
		{"3/", "3", "", true},
		{"/12", "", "12", true},
		{"3 of 12", "3", "12", true},
		{"3OF12", "3", "12", true},
		{"3 Of ", "3", "", true},
		{"of 12", "", "12", true},
		{"-1/2", "-1", "2", true},
		{"3", "3", "", false},
		{"", "", "", false},
		{"/", "/", "", false},
		{"of", "of", "", false},
		{"A/B", "A/B", "", false},
		{"3/12/2", "3/12/2", "", false},
		{"Part 3 of 12", "Part 3 of 12", "", false},
		{"3 off 12", "3 off 12", "", false},
	} {
		num, total, ok := container.SplitNumberTotal(tc.in)
		if num != tc.num || total != tc.total || ok != tc.ok {
			t.Errorf("SplitNumberTotal(%q) = %q, %q, %v, want %q, %q, %v", tc.in, num, total, ok, tc.num, tc.total, tc.ok)
		}
	}
}

// TestValidTagKeyRefusesEdgeSpaces pins the one legal Vorbis field name the
// muxers still skip: a key with a space at an edge is no key's spelling.
func TestValidTagKeyRefusesEdgeSpaces(t *testing.T) {
	for key, want := range map[string]bool{
		"TITLE": true, "ALBUM ARTIST": true, "X": true,
		" TITLE": false, "TITLE ": false, " ": false, "": false, "A=B": false, "TÍTLE": false,
	} {
		if got := container.ValidTagKey(key); got != want {
			t.Errorf("ValidTagKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestSplitNumberPairs: a number splits only where the other reader's count
// says it holds one value, and the total it derives yields to any total that
// reader holds, even one this map lacks.
func TestSplitNumberPairs(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags map[string][]string
		seen map[string]int
		want map[string][]string
	}{
		{"split", map[string][]string{"TRACKNUMBER": {"3/12"}, "DISCNUMBER": {"1 of 2"}},
			map[string]int{"TRACKNUMBER": 1, "DISCNUMBER": 1},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"}, "DISCTOTAL": {"2"}}},
		{"the other reader sees two", map[string][]string{"TRACKNUMBER": {"3/12"}},
			map[string]int{"TRACKNUMBER": 2},
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
		{"the other reader holds a total", map[string][]string{"TRACKNUMBER": {"3/12"}},
			map[string]int{"TRACKNUMBER": 1, "TRACKTOTAL": 1},
			map[string][]string{"TRACKNUMBER": {"3"}}},
		{"total present", map[string][]string{"TRACKNUMBER": {"3/12"}, "TRACKTOTAL": {"10"}},
			map[string]int{"TRACKNUMBER": 1, "TRACKTOTAL": 1},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"10"}}},
		{"no number side", map[string][]string{"TRACKNUMBER": {"/12"}},
			map[string]int{"TRACKNUMBER": 1},
			map[string][]string{"TRACKTOTAL": {"12"}}},
		{"two values", map[string][]string{"TRACKNUMBER": {"3/12", "4/12"}},
			map[string]int{"TRACKNUMBER": 2},
			map[string][]string{"TRACKNUMBER": {"3/12", "4/12"}}},
		{"text", map[string][]string{"TRACKNUMBER": {"Part 3 of 12"}},
			map[string]int{"TRACKNUMBER": 1},
			map[string][]string{"TRACKNUMBER": {"Part 3 of 12"}}},
		// An unknown count derives nothing and drops only a blank number.
		{"unknown, blank number", map[string][]string{"TRACKNUMBER": {"/12"}},
			map[string]int{"TRACKNUMBER": -1, "TRACKTOTAL": -1},
			map[string][]string{}},
		{"unknown, a number", map[string][]string{"TRACKNUMBER": {"3/12"}},
			map[string]int{"TRACKNUMBER": -1, "TRACKTOTAL": -1},
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
	} {
		container.SplitNumberPairs(tc.tags, func(k string) int { return tc.seen[k] })
		if !maps.EqualFunc(tc.tags, tc.want, slices.Equal) {
			t.Errorf("%s: %v, want %v", tc.name, tc.tags, tc.want)
		}
	}
}
