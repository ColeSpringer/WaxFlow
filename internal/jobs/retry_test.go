package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxflow/source"
)

// countingResolver counts the resolves it passes through.
type countingResolver struct {
	source.Resolver
	calls atomic.Int64
}

func (c *countingResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	c.calls.Add(1)
	return c.Resolver.Resolve(ctx, ref)
}

// TestJobWaitsForAnUnavailableRoot pins the restart contract against a
// library that mounts after the daemon: a job whose source root is
// unavailable when it runs goes back to the queue instead of failing, and
// runs once the root mounts.
func TestJobWaitsForAnUnavailableRoot(t *testing.T) {
	later := filepath.Join(t.TempDir(), "later")
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	res := &countingResolver{Resolver: roots}
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, RetryEvery: 10 * time.Millisecond})
	j, err := r.Create(transcodeReq("lib/"+fixtureWAV, ""))
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "three tries at the source", func() bool { return res.calls.Load() >= 3 })
	waitFor(t, "the job to say why it waits", func() bool {
		got, _ := r.Get(j.ID)
		return got.State == StateQueued && got.Waiting != nil
	})
	got, _ := r.Get(j.ID)
	if got.Waiting.Code != "root-unavailable" || got.Waiting.Message == "" || got.Waiting.Until.IsZero() || got.Error != nil {
		t.Fatalf("waiting job = state %s, waiting %+v, error %+v; want queued with its reason", got.State, got.Waiting, got.Error)
	}
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, later, fixtureWAV)
	if done := waitJob(t, r, j.ID, StateDone); done.Error != nil || done.Waiting != nil {
		t.Fatalf("job done with error %+v, waiting %+v", done.Error, done.Waiting)
	}
}

// TestRetryDelay pins the backoff a waiting job's reruns take: doubling from
// RetryEvery to a cap, so jobs behind a root gone for good do not rewrite
// their state every few seconds for ever.
func TestRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		waits int
		want  time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{5, 160 * time.Second},
		{6, 5 * time.Minute},
		{64, 5 * time.Minute},
		{1 << 30, 5 * time.Minute},
	} {
		if got := retryDelay(10*time.Second, tc.waits); got != tc.want {
			t.Errorf("retryDelay(10s, %d) = %v, want %v", tc.waits, got, tc.want)
		}
	}
	if got := retryDelay(10*time.Minute, 1); got != 10*time.Minute {
		t.Errorf("retryDelay(10m, 1) = %v, want the configured 10m", got)
	}
}
