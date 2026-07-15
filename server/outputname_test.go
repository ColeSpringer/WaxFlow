package server

import (
	"testing"

	"github.com/colespringer/waxflow/internal/jobs"
)

// TestOutputFilenameHasAName pins that the attachment name is always a name.
//
// The empty ref is unreachable through the API (every caller requires one),
// and it is checked anyway because the failure is silent in the worst way:
// path.Base("") is ".", the extension strip skips a leading dot on purpose,
// and the result is a hidden file that arrives and disappears from the
// folder it lands in.
func TestOutputFilenameHasAName(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"lib/a.flac", "a.flac"},
		{"lib/album/track.wav", "track.flac"},
		{"upload:abc123", "upload:abc123.flac"},
		{"noext", "noext.flac"},
		// A leading dot is a name, not an extension, and is kept: the
		// download mirrors the source, hidden as the source is.
		{"lib/.hidden.wav", ".hidden.flac"},
		// No name of its own: fall back rather than emit "..flac".
		{"", "output.flac"},
		{".", "output.flac"},
		{"..", "output.flac"},
		{"/", "output.flac"},
	} {
		if got := outputFilename(tc.ref, "flac"); got != tc.want {
			t.Errorf("outputFilename(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// TestJobResultFilenameNamesAFileForm pins that a download is named for the
// file form it actually is, which is a question only the output table can
// answer.
//
// The cases above pass a container name straight through as an extension, and
// that is exactly how this survived: flac, mp3, wav, and mka are all container
// names that happen to spell their own extension, so the arithmetic looked
// right for every case anyone tried. A container is a muxer-form selector, and
// the moment one does not coincide (progressive, which is the flat MP4 an
// mp4-family merge defaults to, and adts, whose files are .aac) the name it
// yields is a file nothing will open. The progressive case is the one that
// matters most: it is the M4B default that exists so an audiobook merge opens
// in Apple Books, and .progressive is not a file Apple Books opens.
func TestJobResultFilenameNamesAFileForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  jobs.Request
		outs []jobs.Output
		n    int
		want string
	}{
		{
			// The default an mp4-family merge picks for itself.
			name: "an mp4 merge's progressive default",
			req:  jobs.Request{Type: jobs.TypeMerge, Srcs: []string{"lib/ch1.mp3"}, Format: "aac", Container: "progressive"},
			outs: []jobs.Output{{Container: "progressive"}},
			want: "ch1.m4a",
		},
		{
			name: "alac in a flat mp4",
			req:  jobs.Request{Type: jobs.TypeMerge, Srcs: []string{"lib/book.flac"}, Format: "alac", Container: "progressive"},
			outs: []jobs.Output{{Container: "progressive"}},
			want: "book.m4a",
		},
		{
			// The row's own default form: aac writes .m4a, never .aac.
			name: "aac with no override",
			req:  jobs.Request{Type: jobs.TypeTranscode, Src: "lib/a.wav", Format: "aac"},
			outs: []jobs.Output{{Container: "aac"}},
			want: "a.m4a",
		},
		{
			// ...and the override that does rename the file.
			name: "aac as raw adts",
			req:  jobs.Request{Type: jobs.TypeTranscode, Src: "lib/a.wav", Format: "aac", Container: "adts"},
			outs: []jobs.Output{{Container: "adts"}},
			want: "a.aac",
		},
		{
			name: "flac in ogg is oga",
			req:  jobs.Request{Type: jobs.TypeTranscode, Src: "lib/a.wav", Format: "flac", Container: "ogg"},
			outs: []jobs.Output{{Container: "ogg"}},
			want: "a.oga",
		},
		{
			// The case that always worked, kept so the table cannot regress it.
			name: "flac plain",
			req:  jobs.Request{Type: jobs.TypeTranscode, Src: "lib/a.wav", Format: "flac"},
			outs: []jobs.Output{{Container: "flac"}},
			want: "a.flac",
		},
		{
			// A split's pieces carry the index before the extension, and the
			// extension is still the table's.
			name: "a split piece",
			req:  jobs.Request{Type: jobs.TypeSplit, Src: "lib/rip.wav", Format: "aac", Container: "progressive"},
			outs: []jobs.Output{{Container: "progressive"}, {Container: "progressive"}},
			n:    1,
			want: "rip.1.m4a",
		},
		{
			// An analyze job wrote no audio, so there is no row to ask: its
			// silence map is JSON and its container name IS its extension.
			// Routing this through the table would name it .bin.
			name: "an analyze job's silence map",
			req:  jobs.Request{Type: jobs.TypeAnalyze, Src: "lib/a.wav"},
			outs: []jobs.Output{{Container: "json"}},
			want: "a.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &jobs.Job{Request: tc.req, Outputs: tc.outs}
			if got := jobResultFilename(j, tc.n); got != tc.want {
				t.Errorf("jobResultFilename() = %q, want %q", got, tc.want)
			}
		})
	}
}
