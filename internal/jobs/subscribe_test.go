package jobs

import "testing"

// A worker that claims the job while Subscribe stands between its snapshot
// and its registration sends a running event the subscriber still gets: the
// held pool keeps the job from sending anything after it.
func TestSubscribeSeesAClaimBehindItsSnapshot(t *testing.T) {
	root := t.TempDir()
	ref := writeLongWAV(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	pools, _ := saturatedPool(t)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Pools: pools})

	j, err := r.store.create(transcodeReq(ref, srcID)) // queued; no worker woken
	if err != nil {
		t.Fatal(err)
	}
	r.hooks.subscribed = func() {
		r.hooks.subscribed = nil
		r.kick()
		waitFor(t, "the claim", func() bool {
			cur, _ := r.store.get(j.ID)
			return cur.State == StateRunning
		})
		// Where Subscribe does not hold the lock notify takes here, the
		// worker's running event goes out before the registration.
		if r.mu.TryLock() {
			r.mu.Unlock()
			waitFor(t, "the running event", func() bool { return r.Running() == 1 })
		}
	}
	ch, cancel, ok := r.Subscribe(j.ID)
	if !ok {
		t.Fatal("Subscribe: job not found")
	}
	t.Cleanup(cancel)
	for {
		s, open := recv(t, ch)
		if !open {
			t.Fatal("subscription closed before the job ran")
		}
		if s.State == StateRunning {
			return
		}
	}
}
