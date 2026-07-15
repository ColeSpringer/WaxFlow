package jobs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/colespringer/waxflow/internal/ulid"
	"github.com/colespringer/waxflow/waxerr"
)

// store is the file-backed job index: dir/<id>/job.json plus the job's
// output files. The in-memory map is authoritative after open; job.json
// writes are atomic (tmp then rename) so a crash leaves either the old
// or the new state, never a torn one.
type store struct {
	dir string
	log *slog.Logger

	mu   sync.Mutex
	jobs map[string]*Job
}

func openStore(dir string, log *slog.Logger) (*store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: creating store dir", err)
	}
	s := &store{dir: dir, log: log, jobs: map[string]*Job{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "jobs: scanning store dir", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !ulid.Valid(e.Name()) {
			continue
		}
		j, err := s.load(e.Name())
		if err != nil {
			// Job state must be trustworthy or out of the way, but the
			// directory may hold a finished output (disk corruption, or a
			// newer schema after a binary downgrade), so it is quarantined
			// by renaming out of the id namespace rather than deleted: the
			// scan skips non-ulid names, and the operator decides.
			log.Warn("jobs: quarantining unreadable job dir", "id", e.Name(), "err", err)
			from := filepath.Join(dir, e.Name())
			target := from + ".unreadable"
			rerr := os.Rename(from, target)
			if rerr != nil {
				// A leftover quarantine of the same name (an operator
				// restored the id and it broke again) blocks the rename on
				// most platforms; a unique suffix makes the retry land.
				target = fmt.Sprintf("%s.%d", target, time.Now().UnixNano())
				rerr = os.Rename(from, target)
			}
			if rerr != nil {
				log.Warn("jobs: quarantine rename failed", "id", e.Name(), "err", rerr)
			}
			continue
		}
		if !j.State.Terminal() {
			// The restart contract: incomplete jobs restart cleanly from
			// zero. Partial outputs are deleted so the rerun is idempotent.
			s.resetForRequeue(j)
			if err := s.persistLocked(j); err != nil {
				log.Warn("jobs: requeue persist failed", "id", j.ID, "err", err)
			}
		}
		s.jobs[j.ID] = j
	}
	return s, nil
}

// resetForRequeue returns an interrupted job to its just-created shape
// and removes everything but job.json from its directory.
func (s *store) resetForRequeue(j *Job) {
	j.State = StateQueued
	j.Started, j.Finished, j.Error, j.Output, j.Analysis, j.Progress = nil, nil, nil, nil, nil, nil
	j.Timeline = nil
	j.Warnings = nil
	dir := s.jobDir(j.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() != jobFile {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

const jobFile = "job.json"

func (s *store) jobDir(id string) string { return filepath.Join(s.dir, id) }

func (s *store) load(id string) (*Job, error) {
	b, err := os.ReadFile(filepath.Join(s.jobDir(id), jobFile))
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	if j.ID != id || j.SchemaVersion != 1 {
		return nil, fmt.Errorf("jobs: job.json for %s names %q schema %d", id, j.ID, j.SchemaVersion)
	}
	return &j, nil
}

// persistLocked writes job.json atomically. Callers hold s.mu (or own
// the job exclusively during creation).
func (s *store) persistLocked(j *Job) error {
	dir := s.jobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: creating job dir", err)
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "jobs: encoding job", err)
	}
	tmp := filepath.Join(dir, jobFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: writing job", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, jobFile)); err != nil {
		os.Remove(tmp)
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "jobs: promoting job", err)
	}
	return nil
}

// create persists a fresh queued job.
func (s *store) create(req Request) (*Job, error) {
	id, err := ulid.New()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "jobs: minting id", err)
	}
	j := &Job{
		SchemaVersion: 1,
		ID:            id,
		Type:          req.Type,
		State:         StateQueued,
		Request:       req,
		Created:       time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(j); err != nil {
		return nil, err
	}
	s.jobs[id] = j
	return j.clone(), nil
}

func (s *store) get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	return j.clone(), true
}

func (s *store) list() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j.clone())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// claimNext moves the oldest queued job to running and returns a copy,
// or nil when nothing is queued.
func (s *store) claimNext() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next *Job
	for _, j := range s.jobs {
		if j.State != StateQueued {
			continue
		}
		if next == nil || j.ID < next.ID {
			next = j
		}
	}
	if next == nil {
		return nil
	}
	now := time.Now().UTC()
	next.State = StateRunning
	next.Started = &now
	if err := s.persistLocked(next); err != nil {
		s.log.Warn("jobs: claim persist failed", "id", next.ID, "err", err)
	}
	return next.clone()
}

// update applies fn to the stored job under the lock and persists when
// fn reports a durable change. It returns the updated copy, or nil when
// the job no longer exists (deleted mid-run).
func (s *store) update(id string, persist bool, fn func(*Job)) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil
	}
	fn(j)
	if persist {
		if err := s.persistLocked(j); err != nil {
			s.log.Warn("jobs: persist failed", "id", id, "err", err)
		}
	}
	return j.clone()
}

// remove drops the job from the index and disk.
func (s *store) remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return false
	}
	delete(s.jobs, id)
	if err := os.RemoveAll(s.jobDir(id)); err != nil {
		s.log.Warn("jobs: removing job dir", "id", id, "err", err)
	}
	return true
}
