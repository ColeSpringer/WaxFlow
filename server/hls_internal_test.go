package server

import (
	"context"
	"errors"
	"io"
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
	"github.com/colespringer/waxflow/waxerr"
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
	return bareServer(t, root), refs
}

// bareServer is a Server with a timeline store over the library at root, and
// a cold track memo.
func bareServer(t *testing.T, root string) *Server {
	t.Helper()
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	store, err := timeline.Open(t.TempDir(), timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{eng: waxflow.New(), resolver: roots, timelines: store}
}

// shortXingMP3 writes sine-cbr128.mp3 cut 100 bytes short into root: its Xing
// count still says 22050 samples, and its frames hold fewer. It returns the
// file's reference and the count a decode delivers.
func shortXingMP3(t *testing.T, root string) (string, int64) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	cut := raw[:len(raw)-100]
	if err := os.WriteFile(filepath.Join(root, "short.mp3"), cut, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(cut), "mp3", io.Discard,
		waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples >= 22050 {
		t.Fatalf("the cut decodes %d samples; this fixture needs fewer than its Xing count of 22050", res.Samples)
	}
	return "lib/short.mp3", res.Samples
}

// TestResolveTimelineMembersMeasuresEveryMember: a timeline member's total is
// measured, never taken at its header's word: a member whose Xing count says
// more than its frames hold would desync every position after it. The memo is
// cold, as after an upgrade, so no mint measured it first.
func TestResolveTimelineMembersMeasuresEveryMember(t *testing.T) {
	root := t.TempDir()
	ref, want := shortXingMP3(t, root)
	s := bareServer(t, root)
	members, err := s.resolveTimelineMembers(context.Background(), putTimeline(t, s, []string{ref}))
	if err != nil {
		t.Fatal(err)
	}
	if got := members[0].Track; got.Samples != want || !got.SamplesExact {
		t.Errorf("the member resolved to %d samples (exact %t), want the %d its frames hold",
			got.Samples, got.SamplesExact, want)
	}
}

// TestResolveHLSSourcesMeasuresASpannedSource: a span is checked against its
// source's total, so a spanned single source is measured too.
func TestResolveHLSSourcesMeasuresASpannedSource(t *testing.T) {
	root := t.TempDir()
	ref, want := shortXingMP3(t, root)
	s := bareServer(t, root)
	f, err := s.resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID.String()
	f.Close()
	req := &hlsRequest{desc: hls.Descriptor{Ver: hls.DescriptorVersion, Src: ref, ID: id, Format: "aac", To: 1000}}
	defer req.Close()
	if err := s.resolveHLSSources(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := req.members[0].SrcTrack.Samples; got != want {
		t.Errorf("the spanned source measured %d samples, want the %d its frames hold", got, want)
	}
}

// putTimeline stores a timeline of refs, each at its current identity, without
// minting it: nothing measures the members, so the track memo stays cold.
func putTimeline(t *testing.T, s *Server, refs []string) string {
	t.Helper()
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
	return digest
}

// openedFiles records every handle its resolver hands out.
type openedFiles struct {
	source.Resolver
	files []*source.File
}

func (r *openedFiles) Resolve(ctx context.Context, ref string) (*source.File, error) {
	f, err := r.Resolver.Resolve(ctx, ref)
	if err == nil {
		r.files = append(r.files, f)
	}
	return f, err
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
	digest := putTimeline(t, s, refs)

	opened := &openedFiles{Resolver: s.resolver}
	s.resolver = opened
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
	if len(opened.files) != len(refs) {
		t.Fatalf("the resolver opened %d handles for %d members; the check below would see too few", len(opened.files), len(refs))
	}
	for i, f := range opened.files {
		if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("handle %d (%s) is still open after resolving (read: %v)", i, f.Ref, err)
		}
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

// TestResolveTimelineMembersStopsWhenCanceled: a request that is gone stops
// resolving, rather than measuring the rest of a queue nobody will read.
func TestResolveTimelineMembersStopsWhenCanceled(t *testing.T) {
	s, refs := timelineSourcesEnv(t)
	digest := putTimeline(t, s, refs)
	opened := &openedFiles{Resolver: s.resolver}
	s.resolver = opened
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.resolveTimelineMembers(ctx, digest); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("resolving for a canceled request = %v, want %s", err, waxerr.CodeCanceled)
	}
	if len(opened.files) != 0 {
		t.Errorf("a canceled request still opened %d of %d members", len(opened.files), len(refs))
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
			got, err := hlsDuration(desc, tracks, waxflow.ConcatOptions{})
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
