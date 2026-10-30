package jobs

import (
	"log/slog"
	"testing"
	"time"
)

// TestClaimSkipsAWaitingJob pins the hold a retry puts on a job: it is not
// claimed before its time, and is claimed after it.
func TestClaimSkipsAWaitingJob(t *testing.T) {
	s, err := openStore(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.create(transcodeReq("lib/a.wav", ""))
	if err != nil {
		t.Fatal(err)
	}
	if s.claimNext() == nil {
		t.Fatal("a new job was not claimed")
	}
	q, waits, delay := s.requeue(j.ID, time.Hour, "root-unavailable", "m")
	if q == nil || waits != 1 || delay != time.Hour || q.State != StateQueued || q.Waiting == nil {
		t.Fatalf("requeue = %+v, %d, %v; want the job queued an hour with its reason", q, waits, delay)
	}
	if got := s.claimNext(); got != nil {
		t.Fatalf("claimed %s before its time", got.ID)
	}
	if _, waits, delay := s.requeue(j.ID, 0, "root-unavailable", "m"); waits != 2 || delay != 0 {
		t.Fatalf("second requeue = %d waits, %v; want 2 and no hold", waits, delay)
	}
	if got := s.claimNext(); got == nil || got.ID != j.ID || got.Waiting != nil {
		t.Fatalf("claim after its time = %+v, want the job, no longer waiting", got)
	}
}

// TestRestartClearsAWait pins that a wait does not outlive the daemon: a
// waiting job reloaded at the next start runs at once and no longer says it
// waits, though its job.json carried the wait.
func TestRestartClearsAWait(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	s, err := openStore(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.create(transcodeReq("lib/a.wav", ""))
	if err != nil {
		t.Fatal(err)
	}
	s.claimNext()
	if q, _, _ := s.requeue(j.ID, time.Hour, "root-unavailable", "m"); q == nil || q.Waiting == nil {
		t.Fatalf("requeue = %+v, want a waiting job", q)
	}
	again, err := openStore(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := again.get(j.ID); !ok || got.Waiting != nil || got.State != StateQueued {
		t.Fatalf("job after a restart = %+v, want queued with no wait", got)
	}
	if got := again.claimNext(); got == nil || got.ID != j.ID {
		t.Fatalf("claim after a restart = %+v, want the job at once", got)
	}
}
