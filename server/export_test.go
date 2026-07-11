package server

import "github.com/colespringer/waxflow/internal/metrics"

// Test-only seams exposed to the external server_test package.

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

// Metrics exposes the internal counter set to the test package. It
// lives here rather than on the public API because its type is
// internal/metrics, which external callers cannot name (the v1.0
// surface audit removed the public method as unusable).
func (s *Server) Metrics() *metrics.Metrics { return s.met }
