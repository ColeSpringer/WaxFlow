package waxflow

import "testing"

// TestOutputEmbedsTags pins the switch all three post-pass callers share (the
// CLI's transcode and split, the job runner). The behavioral halves live with
// those callers: the split and runner tests prove the tags arrive from the
// mux, and the runner's mock counts post-pass calls. What none of them can see
// anymore is a deleted arm here, because since the tag library learned APEv2 a
// wrongly-run post-pass over a .wv or .ape SUCCEEDS silently instead of
// printing a failure, so this table is the one place the skip itself is
// asserted.
func TestOutputEmbedsTags(t *testing.T) {
	for _, tc := range []struct {
		format, container string
		want              bool
	}{
		{"wavpack", "", true},
		{"ape", "", true},
		{"flac", ContainerOgg, true},
		{"alac", "", true},
		{"aac", "", true},
		{"aac", ContainerProgressive, true},
		{"aac", ContainerFragmented, true},
		{"he-aac", "", true},
		{"aac", "adts", false},
		{"flac", "", false},
		{"flac", "mka", false},
		{"opus", "", false},
		{"mp3", "", false},
		{"wav", "", false},
	} {
		if got := OutputEmbedsTags(tc.format, tc.container); got != tc.want {
			t.Errorf("OutputEmbedsTags(%q, %q) = %v, want %v", tc.format, tc.container, got, tc.want)
		}
	}
}
