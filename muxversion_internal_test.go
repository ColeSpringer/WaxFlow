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

// containerCandidates is every Container override any row accepts. A row that
// refuses one is skipped, so the list may be a superset; what it may not be
// is short, since a name missing here is a muxer this test never reaches.
var containerCandidates = []string{
	"", "mka", "webm", ContainerOgg, "adts", ContainerProgressive, ContainerFragmented,
}

// muxerVersionByType maps the muxer a row constructs to the term the cache key
// should carry for it, keyed by Go type rather than by container name.
//
// The independent derivation is the whole point. muxerVersion answers from the
// resolved container name, which is what row.mux branches on, so a table
// keyed the same way would agree with it by construction and catch nothing.
// This one is keyed on the object that actually wrote the bytes.
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
// muxer it is supposed to describe, for every (row, container) pair the table
// admits.
//
// Two things could go wrong and neither is visible from one side alone. A row
// whose mux hook gains a branch (a new wrapper for an existing format) would
// leave muxerVersion answering the old container's term, so two different
// containers would share a cache key. And a name accepted by a row's container
// hook but missing from muxerVersion would fail every plan for it, which is
// loud but only if something plans it.
func TestMuxerVersionNamesTheMuxerThatRuns(t *testing.T) {
	pairs := 0
	for i := range outputs {
		row := &outputs[i]
		for _, name := range containerCandidates {
			containerName, _, err := resolveContainer(row, name)
			if err != nil {
				continue
			}
			opts := TranscodeOptions{Format: row.name, Container: name}
			mux, err := row.mux(container.Track{Fmt: audio.Format{Rate: 44100, Channels: 2, BitDepth: 16}}, opts, nil, io.Discard)
			if err != nil {
				t.Errorf("%s/%q: mux: %v", row.name, name, err)
				continue
			}
			typ := fmt.Sprintf("%T", mux)
			want, ok := muxerVersionByType[typ]
			if !ok {
				t.Errorf("%s/%q builds %s, which muxerVersionByType does not name", row.name, name, typ)
				continue
			}
			got, err := muxerVersion(containerName)
			if err != nil {
				t.Errorf("%s/%q resolves to container %q: muxerVersion: %v", row.name, name, containerName, err)
				continue
			}
			if got != want {
				t.Errorf("%s/%q (container %q) muxes through %s but keys on %q, want %q",
					row.name, name, containerName, typ, got, want)
			}
			pairs++
		}
	}
	// Every row has at least its default container, so a pass over nothing
	// means the enumeration broke rather than that the table is clean.
	if pairs < len(outputs) {
		t.Errorf("checked %d (row, container) pairs for %d rows", pairs, len(outputs))
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
