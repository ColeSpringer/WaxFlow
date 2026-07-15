// Package metrics is WaxFlow's hand-rolled Prometheus surface (plan
// section 9): a fixed set of counters, gauges, and one histogram, exposed
// in text format by GET /metrics. No client library; the text exposition
// format is a dozen lines and the family promise is a short dependency
// list.
package metrics

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"sync/atomic"
)

// Metrics is the daemon's metric set. The zero value is ready to use.
type Metrics struct {
	// SessionsActive counts running transcode pipelines.
	SessionsActive atomic.Int64
	// SessionsLive, SessionsSync, and SessionsHLS count started pipelines
	// by kind: live /stream sessions, sync /transcode one-shots, and HLS
	// variant workers.
	SessionsLive atomic.Uint64
	SessionsSync atomic.Uint64
	SessionsHLS  atomic.Uint64
	// HLSSegments counts media segments served (cache hits included).
	HLSSegments atomic.Uint64
	// DirectPlays counts requests served as original bytes (ladder rung 1).
	DirectPlays atomic.Uint64
	// Remuxes counts pipelines served by rewriting the container around the
	// source's own packets (ladder rung 2), progressive and segmented alike. It
	// counts pipelines rather than requests, so a cache hit on a remuxed entry
	// does not add to it: the question this answers is how much work the rung
	// saved, and a hit did no work on any rung.
	Remuxes atomic.Uint64
	// AdmissionRejects counts 503s from saturated pools.
	AdmissionRejects atomic.Uint64
	// Degradations counts sessions downgraded to ring-fed streaming by
	// cache write failures.
	Degradations atomic.Uint64
	// TTFB observes seconds from request start to first stream byte.
	TTFB Histogram
}

// ttfbBuckets are the histogram upper bounds in seconds. The TTFA targets
// (300 ms warm, 800 ms cold) sit inside the ladder, not at its edges.
var ttfbBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Histogram is a fixed-bucket, lock-free histogram.
type Histogram struct {
	counts  [len(ttfbBuckets) + 1]atomic.Uint64 // +Inf last
	sumBits atomic.Uint64
	total   atomic.Uint64
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	i := 0
	for i < len(ttfbBuckets) && v > ttfbBuckets[i] {
		i++
	}
	h.counts[i].Add(1)
	h.total.Add(1)
	for {
		old := h.sumBits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sumBits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Gauges are values sampled at scrape time; the server supplies them so
// this package needs no view of the cache or the pools. CacheHits and
// CacheMisses are monotonic counters owned by the cache store (its
// Lookup is the single accounting point, shared with /cache/stats), just
// read here at scrape.
type Gauges struct {
	CacheBytes   int64
	CacheEntries int
	CacheHits    uint64
	CacheMisses  uint64
	LiveInUse    int
	JobInUse     int
}

// WritePrometheus renders the text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer, version string, g Gauges) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }

	p("# HELP waxflow_build_info Build metadata; the value is always 1.\n")
	p("# TYPE waxflow_build_info gauge\n")
	p("waxflow_build_info{version=%q} 1\n", version)

	p("# HELP waxflow_sessions_active Running transcode pipelines.\n")
	p("# TYPE waxflow_sessions_active gauge\n")
	p("waxflow_sessions_active %d\n", m.SessionsActive.Load())

	p("# HELP waxflow_sessions_total Started transcode pipelines by kind.\n")
	p("# TYPE waxflow_sessions_total counter\n")
	p("waxflow_sessions_total{kind=\"live\"} %d\n", m.SessionsLive.Load())
	p("waxflow_sessions_total{kind=\"sync\"} %d\n", m.SessionsSync.Load())
	p("waxflow_sessions_total{kind=\"hls\"} %d\n", m.SessionsHLS.Load())

	p("# HELP waxflow_direct_play_total Requests served as original bytes.\n")
	p("# TYPE waxflow_direct_play_total counter\n")
	p("waxflow_direct_play_total %d\n", m.DirectPlays.Load())

	p("# HELP waxflow_remux_total Pipelines served by rewriting the container around the source's own packets.\n")
	p("# TYPE waxflow_remux_total counter\n")
	p("waxflow_remux_total %d\n", m.Remuxes.Load())

	p("# HELP waxflow_hls_segments_total HLS media segments served.\n")
	p("# TYPE waxflow_hls_segments_total counter\n")
	p("waxflow_hls_segments_total %d\n", m.HLSSegments.Load())

	p("# HELP waxflow_cache_hits_total Cache lookups served from a completed entry.\n")
	p("# TYPE waxflow_cache_hits_total counter\n")
	p("waxflow_cache_hits_total %d\n", g.CacheHits)
	p("# HELP waxflow_cache_misses_total Cache lookups that found no completed entry.\n")
	p("# TYPE waxflow_cache_misses_total counter\n")
	p("waxflow_cache_misses_total %d\n", g.CacheMisses)

	p("# HELP waxflow_cache_bytes Transcode cache size on disk.\n")
	p("# TYPE waxflow_cache_bytes gauge\n")
	p("waxflow_cache_bytes %d\n", g.CacheBytes)
	p("# HELP waxflow_cache_entries Transcode cache entry count.\n")
	p("# TYPE waxflow_cache_entries gauge\n")
	p("waxflow_cache_entries %d\n", g.CacheEntries)

	p("# HELP waxflow_admission_rejects_total Requests refused with 503 by saturated pools.\n")
	p("# TYPE waxflow_admission_rejects_total counter\n")
	p("waxflow_admission_rejects_total %d\n", m.AdmissionRejects.Load())

	p("# HELP waxflow_admission_in_use Occupied admission slots.\n")
	p("# TYPE waxflow_admission_in_use gauge\n")
	p("waxflow_admission_in_use{pool=\"live\"} %d\n", g.LiveInUse)
	p("waxflow_admission_in_use{pool=\"job\"} %d\n", g.JobInUse)

	p("# HELP waxflow_session_degradations_total Sessions downgraded to ring-fed streaming by cache write failures.\n")
	p("# TYPE waxflow_session_degradations_total counter\n")
	p("waxflow_session_degradations_total %d\n", m.Degradations.Load())

	p("# HELP waxflow_ttfb_seconds Time from request start to first stream byte.\n")
	p("# TYPE waxflow_ttfb_seconds histogram\n")
	var cum uint64
	for i, le := range ttfbBuckets {
		cum += m.TTFB.counts[i].Load()
		p("waxflow_ttfb_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(le, 'g', -1, 64), cum)
	}
	cum += m.TTFB.counts[len(ttfbBuckets)].Load()
	p("waxflow_ttfb_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	p("waxflow_ttfb_seconds_sum %g\n", math.Float64frombits(m.TTFB.sumBits.Load()))
	p("waxflow_ttfb_seconds_count %d\n", m.TTFB.total.Load())
}
