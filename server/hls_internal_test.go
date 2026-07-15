package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/internal/timeline"
	"github.com/colespringer/waxflow/source"
)

// timelineSourcesEnv is a Server with a timeline store and a three-file queue.
func timelineSourcesEnv(t *testing.T) (*Server, []string) {
	t.Helper()
	root := t.TempDir()
	wav, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, name := range []string{"a.wav", "b.wav", "c.wav"} {
		if err := os.WriteFile(filepath.Join(root, name), wav, 0o644); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, "lib/"+name)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	store, err := timeline.Open(t.TempDir(), timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{eng: waxflow.New(), resolver: roots, timelines: store}, refs
}

// TestResolveHLSSourcesHoldsNoTimelineHandles pins what makes a timeline of
// any length affordable to serve: resolving its members holds no open file
// once each is checked and probed.
//
// The primitive opens members lazily so a queue of any length costs one
// descriptor at a time. The request front half runs per segment request and
// resolves every member to check its identity, so keeping those handles would
// hand the cost straight back and then some: a thousand-member queue would
// want a thousand descriptors, on every request, against a default limit of
// 1024, for a stream that only ever reads one member at a time.
//
// It is an internal test because the invariant is about the request's own
// state. Counting descriptors from outside cannot see it: each request closes
// its handles when it ends, so a sequence of requests looks identical either
// way, and only concurrent in-flight requests would differ, which is a race to
// sample rather than a fact to assert. This asserts the fact.
func TestResolveHLSSourcesHoldsNoTimelineHandles(t *testing.T) {
	s, refs := timelineSourcesEnv(t)
	members := make([]timeline.Member, len(refs))
	for i, ref := range refs {
		f, err := s.resolver.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		members[i] = timeline.Member{Src: ref, ID: f.ID.String()}
		f.Close()
	}
	digest, err := s.timelines.Put(members, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	req := &hlsRequest{desc: hls.Descriptor{Ver: hls.DescriptorVersion, Tl: digest, Format: "opus"}}
	defer req.Close()
	if err := s.resolveHLSSources(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(req.members) != len(refs) {
		t.Fatalf("resolved %d members, want %d", len(req.members), len(refs))
	}
	if len(req.srcs) != 0 {
		t.Fatalf("a timeline request holds %d open source handles after resolving; it must hold none, "+
			"because nothing reads a member again once it is checked and probed", len(req.srcs))
	}
	// The tracks are what the plan runs on, so dropping the handles must not
	// have dropped the facts: a timeline that resolved to empty tracks would
	// satisfy the assertion above for the wrong reason.
	for i, m := range req.members {
		if m.Track.Samples <= 0 || m.Track.Fmt.Rate == 0 {
			t.Fatalf("member %d kept no usable track (%d samples, %v)", i, m.Track.Samples, m.Track.Fmt)
		}
	}
}

// TestHLSDurationIsTheStreamsOwn pins what the TTL policy is sized from: the
// duration of the stream the URL actually plays, not of the file it was cut
// from.
//
// A signed URL is meant to outlive one playthrough, which is the rationale
// hlsDuration states for itself, and a span's playthrough is the span. Reading
// the whole track's length for a two-minute excerpt of an hour-long rip hands
// DefaultTTLFor a number 30x too large, and it is only ever too large, which is
// why nothing broke and why nothing caught it either.
func TestHLSDurationIsTheStreamsOwn(t *testing.T) {
	// An hour at 44100, so the file and any span of it are far apart.
	tracks := []container.Track{{Samples: 3600 * 44100, Fmt: audio.Format{Rate: 44100}}}
	for _, tc := range []struct {
		name     string
		from, to int64
		want     float64
	}{
		// The whole file: no span, so nothing narrows.
		{"no span", 0, 0, 3600},
		// A 60 s window starting one second in.
		{"from and to", 44100, 61 * 44100, 60},
		// from alone runs to the end, so the span is what remains.
		{"from alone", 3599 * 44100, 0, 1},
		// to alone starts at 0.
		{"to alone", 0, 90 * 44100, 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := hls.Descriptor{Ver: hls.DescriptorVersion, Src: "lib/rip.flac", Format: "aac",
				From: tc.from, To: tc.to}
			got, err := hlsDuration(desc, tracks)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("hlsDuration(from %d, to %d) = %g s, want %g: the TTL is sized from "+
					"the stream this URL plays, not from the file it was cut out of",
					tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestResolveHLSSourcesKeepsTheSingleSourceHandle is the other half: a
// single-track request does keep its one handle, because the metadata read
// that resolves tag-based gain still needs it. Without this the test above
// would pass just as well over a version that closed handles it still needed.
func TestResolveHLSSourcesKeepsTheSingleSourceHandle(t *testing.T) {
	s, refs := timelineSourcesEnv(t)
	f, err := s.resolver.Resolve(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID.String()
	f.Close()

	req := &hlsRequest{desc: hls.Descriptor{Ver: hls.DescriptorVersion, Src: refs[0], ID: id, Format: "opus"}}
	defer req.Close()
	if err := s.resolveHLSSources(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(req.srcs) != 1 {
		t.Fatalf("a single-source request holds %d handles, want the 1 the metadata read needs", len(req.srcs))
	}
}
