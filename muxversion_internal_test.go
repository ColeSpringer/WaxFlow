package waxflow

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/container/wv"
)

// muxerVersionByType maps the muxer a row constructs to the term the cache key
// should carry for it, keyed by Go type rather than by container name.
//
// The independent derivation is the whole point: a form's version is declared
// beside its mux, and this is keyed on the object that actually wrote the bytes.
var muxerVersionByType = map[string]string{
	"*riff.Muxer":           riff.MuxerVersion,
	"*aiff.Muxer":           aiff.MuxerVersion,
	"*ogg.Muxer":            ogg.MuxerVersion,
	"*mka.Muxer":            mka.MuxerVersion,
	"*flacn.Muxer":          flacn.MuxerVersion,
	"*mpa.Muxer":            mpa.MuxerVersion,
	"*adts.Muxer":           adts.MuxerVersion,
	"*wv.Muxer":             wv.MuxerVersion,
	"*apen.Muxer":           apen.MuxerVersion,
	"*mp4.Muxer":            mp4.MuxerVersion,
	"*mp4.ProgressiveMuxer": mp4.MuxerVersion,
}

// TestMuxerVersionNamesTheMuxerThatRuns holds ADR-0004's container term to the
// muxer it is supposed to describe, for every form of every row: a form whose
// mux gains a branch, or whose version names another muxer, would key two
// different containers alike.
func TestMuxerVersionNamesTheMuxerThatRuns(t *testing.T) {
	pairs := 0
	for i := range outputs {
		row := &outputs[i]
		for j := range row.forms {
			f := &row.forms[j]
			opts := TranscodeOptions{Format: row.name, Container: f.name}
			mux, err := f.mux(container.Track{Fmt: audio.Format{Rate: 44100, Channels: 2, BitDepth: 16}}, opts, runConfig{}, nil, io.Discard)
			if err != nil {
				t.Errorf("%s/%q: mux: %v", row.name, f.name, err)
				continue
			}
			typ := fmt.Sprintf("%T", mux)
			want, ok := muxerVersionByType[typ]
			if !ok {
				t.Errorf("%s/%q builds %s, which muxerVersionByType does not name", row.name, f.name, typ)
				continue
			}
			if f.version != want {
				t.Errorf("%s/%q muxes through %s but keys on %q, want %q", row.name, f.name, typ, f.version, want)
			}
			pairs++
		}
	}
	// Every row has at least its default form, so a pass over nothing means
	// the enumeration broke rather than that the table is clean.
	if pairs < len(outputs) {
		t.Errorf("checked %d (row, form) pairs for %d rows", pairs, len(outputs))
	}
}

// TestMuxerVersionsAreDistinctPerMuxer keeps the term precise in the direction
// the test above cannot see: two muxers sharing one constant would key their
// outputs alike, so a fix to one would serve the other's stale bytes.
//
// mp4's two writers are the deliberate exception, and it is named rather than
// tolerated: they share the sample entries and movie boxes a change here would
// be about.
func TestMuxerVersionsAreDistinctPerMuxer(t *testing.T) {
	owner := map[string]string{}
	for typ, ver := range muxerVersionByType {
		if strings.HasPrefix(typ, "*mp4.") {
			continue
		}
		if prev, ok := owner[ver]; ok {
			t.Errorf("%s and %s both key on %q", prev, typ, ver)
		}
		owner[ver] = typ
	}
	if mp4.MuxerVersion == mp4.SegmenterVersion {
		t.Errorf("mp4 muxer and segmenter share %q; the progressive and segmented forms are separate keys", mp4.MuxerVersion)
	}
}
