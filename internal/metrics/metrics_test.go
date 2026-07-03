package metrics

import (
	"strings"
	"testing"
)

func TestWritePrometheus(t *testing.T) {
	var m Metrics
	m.SessionsActive.Add(2)
	m.SessionsLive.Add(3)
	m.SessionsSync.Add(1)
	m.DirectPlays.Add(4)
	m.AdmissionRejects.Add(7)
	m.Degradations.Add(1)
	m.TTFB.Observe(0.007) // le 0.01
	m.TTFB.Observe(0.2)   // le 0.25
	m.TTFB.Observe(99)    // +Inf

	var sb strings.Builder
	m.WritePrometheus(&sb, "v1.2.3", Gauges{CacheBytes: 1024, CacheEntries: 3, CacheHits: 5, CacheMisses: 6, LiveInUse: 1, JobInUse: 0})
	out := sb.String()

	want := []string{
		`waxflow_build_info{version="v1.2.3"} 1`,
		"waxflow_sessions_active 2",
		`waxflow_sessions_total{kind="live"} 3`,
		`waxflow_sessions_total{kind="sync"} 1`,
		"waxflow_direct_play_total 4",
		"waxflow_cache_hits_total 5",
		"waxflow_cache_misses_total 6",
		"waxflow_cache_bytes 1024",
		"waxflow_cache_entries 3",
		"waxflow_admission_rejects_total 7",
		`waxflow_admission_in_use{pool="live"} 1`,
		"waxflow_session_degradations_total 1",
		`waxflow_ttfb_seconds_bucket{le="0.005"} 0`,
		`waxflow_ttfb_seconds_bucket{le="0.01"} 1`,
		`waxflow_ttfb_seconds_bucket{le="0.25"} 2`,
		`waxflow_ttfb_seconds_bucket{le="+Inf"} 3`,
		"waxflow_ttfb_seconds_count 3",
	}
	for _, line := range want {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing line %q in output:\n%s", line, out)
		}
	}
	// Buckets are cumulative: sum must be ~99.207.
	if !strings.Contains(out, "waxflow_ttfb_seconds_sum 99.207") {
		t.Errorf("sum line missing or wrong:\n%s", out)
	}
}
