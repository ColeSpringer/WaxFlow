package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/metrics"
	"github.com/colespringer/waxflow/internal/testutil"
)

// Test-only seams exposed to the external server_test package.

// JobRequestJSONTags lists the POST /jobs body's field names as the wire
// spells them, so an external test can assert its own table covers every one.
func JobRequestJSONTags() []string { return jsonTags(reflect.TypeFor[jobRequest]()) }

// jsonTags lists the json names of the struct type t's fields, an embedded
// struct's among them, as encoding/json flattens it.
func jsonTags(t reflect.Type) []string {
	var tags []string
	for _, field := range reflect.VisibleFields(t) {
		if tag := jsonTag(field); tag != "" && !field.Anonymous {
			tags = append(tags, tag)
		}
	}
	return tags
}

// HoldLiveSlot takes one live admission slot and returns its idempotent
// release. It lets a test drive the live pool to saturation directly,
// rather than pinning a real session open and racing its socket
// backpressure: a held one-shot's slot is freed the instant its finite
// body finishes writing, and on loopback the kernel socket buffers can
// swallow the whole body before the assertion runs. ok is false when the
// pool is already full.
func (s *Server) HoldLiveSlot() (release func(), ok bool) {
	return s.pools.AcquireLive()
}

// GatePipelineDst wraps the cache entry every progressive pipeline writes, so
// a test can hold a running pipeline in place. Set it before the first request.
func (s *Server) GatePipelineDst(wrap func(io.Writer) io.Writer) { s.hooks.pipelineDst = wrap }

// GateAfterFlight runs gate between a /stream request's flight and its attach
// to the entry the flight returned. Set it before the first request.
func (s *Server) GateAfterFlight(gate func()) { s.hooks.afterFlight = gate }

// GateHLSPublish runs gate before a variant worker publishes segment n, and
// before a request that made n for itself keeps it. ctx is the run's, so a
// gate can release a canceled one. Set it before the first request.
func (s *Server) GateHLSPublish(gate func(ctx context.Context, n int64)) { s.hooks.hlsPublish = gate }

// HLSUncachedTurns reports how many variants have a turn at an uncached
// segment, and how many requests hold or wait for one.
func (s *Server) HLSUncachedTurns() (variants, requests int) {
	s.hlsUncached.mu.Lock()
	defer s.hlsUncached.mu.Unlock()
	for _, turn := range s.hlsUncached.keys {
		requests += turn.waiting
	}
	return len(s.hlsUncached.keys), requests
}

// SetHLSWaitTimeout bounds a segment request's wait, for a worker or for
// its turn at an uncached segment. Set it before the first request.
func (s *Server) SetHLSWaitTimeout(d time.Duration) { s.hlsMgr.WaitTimeout = d }

// HLSProbeEvery is how often a variant known to refuse writes is tried.
const HLSProbeEvery = hlsProbeEvery

// SetHLSClock sets the clock a variant known to refuse writes is probed by.
// Set it before the first request.
func (s *Server) SetHLSClock(now func() time.Time) { s.hlsBroken.now = now }

// SetHLSProbeWait bounds a probe of a variant known to refuse writes. Set
// it before the first request.
func (s *Server) SetHLSProbeWait(d time.Duration) { s.hlsBroken.wait = d }

// HLSWaiters reports the segment requests waiting on the session manager.
func (s *Server) HLSWaiters() int { return s.hlsMgr.Waiters() }

// SetHLSProbeDeadline replaces what makes a probe's context from its
// request's and its bound. Set it before the first request.
func (s *Server) SetHLSProbeDeadline(deadline func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
	s.hlsBroken.deadline = deadline
}

// SetHLSBrokenMax sets how many variants known to refuse writes are
// remembered. Set it before the first request.
func (s *Server) SetHLSBrokenMax(n int) { s.hlsBroken.max = n }

// GateHLSPublished runs gate after a variant worker has written segment n
// and before it says so. Set it before the first request.
func (s *Server) GateHLSPublished(gate func(ctx context.Context, n int64)) {
	s.hooks.hlsPublished = gate
}

// HLSBrokenVariants reports how many variants are known to refuse writes.
func (s *Server) HLSBrokenVariants() int {
	s.hlsBroken.mu.Lock()
	defer s.hlsBroken.mu.Unlock()
	if s.hlsBroken.seen == nil {
		return 0
	}
	return s.hlsBroken.seen.Len()
}

// HLSInitUncached is the log line of an init header served with no cache.
const HLSInitUncached = hlsInitUncached

// HLSProbeGivenUp is the log line of a probe given up at its bound.
const HLSProbeGivenUp = hlsProbeGivenUp

// HLSServedUncached is the log line of a segment made for one request.
const HLSServedUncached = hlsServedUncached

// OnKey reports every cache key as it is built: the surface, the ladder rung
// taken, and the identity, canonical parameter string and version terms it
// hashes. Set it before the first request.
func (s *Server) OnKey(fn func(surface, rung, identity, canonical string, versions []string)) {
	s.hooks.keyed = fn
}

// Metrics exposes the internal counter set to the test package. It
// lives here rather than on the public API because its type is
// internal/metrics, which external callers cannot name (the v1.0
// surface audit removed the public method as unusable).
func (s *Server) Metrics() *metrics.Metrics { return s.met }

// MidTrimWebM is the mid-stream-trim fixture both server test packages drive
// the ladder with: seed-opus.webm's own packets remuxed through our Matroska
// muxer with a DiscardPadding on the block at MidTrimBlock.
//
// It is the shape mkvmerge leaves at an append seam, which no encoder in this
// tree produces. Every block holds one packet, because the muxer never laces.
//
// declareLength writes it through a seekable destination, so the file carries
// an Info Duration and a probe of it reports an advisory total; without it the
// track declares nothing and the muxer writes no Duration element at all,
// which is the shape whose copy rungs cannot be planned without a walk.
//
// It returns the gapless length a read of the file delivers.
func MidTrimWebM(t *testing.T, path string, declareLength bool) int64 {
	t.Helper()
	raw, err := os.ReadFile(testutil.PkgFile("..", "container", "mka", "testdata", "seed-opus.webm"))
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	src := info.Default()

	track := src
	track.ID, track.Default = 0, true
	track.Samples = MidTrimSamples
	buf := &bytes.Buffer{}
	ws := &testutil.MemWriteSeeker{}
	var w io.Writer = buf
	if declareLength {
		w = ws
	} else {
		track.Samples = -1
	}
	m := mka.NewMuxer(w, &mka.MuxerOptions{WebM: true})
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out := container.Packet{Packet: pkt.Packet}
		if i == MidTrimBlock {
			out.Padding = MidTrimPad
		}
		if err := m.WritePacket(out); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.End(codec.Trailer{Samples: MidTrimSamples, Delay: src.Delay, Padding: MidTrimEndPad}); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()
	if declareLength {
		body = ws.Buf
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return MidTrimSamples
}

// The fixture's numbers, derived from seed-opus.webm: 7 packets of 960 samples
// (raw 6720) with a 312-sample pre-skip and a 648-sample tail trim.
//
// tests/waxflow_remux_test.go builds the same shape from a longer source and so
// pads a different block; the two cannot share a builder, since testutil cannot
// import container/mka (mka's own tests import testutil) and neither package
// can import the other's tests. What has to match is the shape, not the index.
const (
	MidTrimBlock   = 2
	MidTrimPad     = 480
	MidTrimEndPad  = 648
	MidTrimSamples = 6720 - MidTrimPad - 312 - MidTrimEndPad
)
