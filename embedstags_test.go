package waxflow

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOutputEmbedsTags pins the answer all three post-pass callers share (the
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
		// An unknown format has no form, and so no muxer to write tags.
		{"xyz", ContainerOgg, false},
	} {
		if got := OutputEmbedsTags(tc.format, tc.container); got != tc.want {
			t.Errorf("OutputEmbedsTags(%q, %q) = %v, want %v", tc.format, tc.container, got, tc.want)
		}
	}
}

// TestEveryFormAnswersWhetherTagsFit: every form says whether a tag set fits
// its muxer, so no caller hands a muxer a set it would refuse unasked.
func TestEveryFormAnswersWhetherTagsFit(t *testing.T) {
	for _, o := range outputs {
		for _, f := range o.forms {
			if f.tagsFit == nil {
				t.Errorf("%s/%q has no tagsFit", o.name, f.name)
			}
		}
	}
}

// TestTagsFitAsksTheFormsOwnWriter: on every form, the longest TITLE TagsFit
// takes is written, and one byte more is refused in TagsFit's own words. A
// form whose muxer has no cap takes a TITLE past every other's.
func TestTagsFitAsksTheFormsOwnWriter(t *testing.T) {
	raw := sineWAV(t)
	e := New()
	tags := func(n int) []container.Tag {
		return []container.Tag{{Key: "ALBUM", Value: "The Album"}, {Key: "TITLE", Value: strings.Repeat("t", n)}}
	}
	// past is past every cap: a form that takes a TITLE this long has none.
	const past = 64 << 10
	for _, o := range outputs {
		for _, f := range o.forms {
			write := func(n int) error {
				_, err := e.Transcode(context.Background(), container.BytesSource(raw), "wav", &testutil.MemWriteSeeker{},
					TranscodeOptions{Format: o.name, Container: f.name}, WithTags(tags(n)))
				return err
			}
			n := sort.Search(past+1, func(n int) bool { return TagsFit(o.name, f.name, tags(n)) != nil }) - 1
			if err := write(n); err != nil {
				t.Errorf("%s/%q: a %d-byte TITLE TagsFit takes is refused: %v", o.name, f.name, n, err)
			}
			if n == past {
				continue
			}
			fitErr := TagsFit(o.name, f.name, tags(n+1))
			if err := write(n + 1); err == nil || err.Error() != fitErr.Error() {
				t.Errorf("%s/%q: a %d-byte TITLE: the write says %v, TagsFit %v", o.name, f.name, n+1, err, fitErr)
			}
		}
	}
}
