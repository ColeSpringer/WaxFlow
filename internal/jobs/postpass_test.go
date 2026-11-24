package jobs

import (
	"context"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/source"
)

// stoppingMapper stops the daemon from inside Apply, as a shutdown landing in
// the post-pass does, and reports success the way a mapper that ignores its
// context would.
type stoppingMapper struct {
	once sync.Once
	stop func()
}

func (m *stoppingMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	return &meta.Info{Tags: map[string][]string{"TITLE": {"job"}}}, nil
}

func (m *stoppingMapper) Apply(context.Context, string, *meta.Info, []container.Tag) error {
	m.once.Do(m.stop)
	return nil
}

// A shutdown in the post-pass leaves the job running on disk, so the next
// boot reruns it whole, post-pass included.
func TestShutdownInThePostPassLeavesTheJobToRerun(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()
	m := &stoppingMapper{}
	r := openRunner(t, Config{Dir: dir, Resolver: res, Meta: m})
	m.stop = r.cancel
	j, err := r.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the post-pass", func() bool { return r.ctx.Err() != nil })
	r.Close()
	if got, _ := r.Get(j.ID); got.State != StateRunning {
		t.Fatalf("the job is %s after a shutdown in its post-pass, want %s for the next boot", got.State, StateRunning)
	}

	next := openRunner(t, Config{Dir: dir, Resolver: res, Meta: &stoppingMapper{stop: func() {}}})
	waitJob(t, next, j.ID, StateDone)
}

// A shutdown in a split's post-pass leaves the job to rerun too, the last
// piece's included: that piece may be untagged.
func TestShutdownInASplitPostPassLeavesTheJobToRerun(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()
	m := &stoppingMapper{}
	r := openRunner(t, Config{Dir: dir, Resolver: res, Meta: m})
	m.stop = r.cancel
	j, err := r.Create(Request{
		Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID,
		Params: outshape.Params{Format: "flac", FLACLevel: -1}, Cuts: []int64{1000}, Skip: []int{0},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the post-pass", func() bool { return r.ctx.Err() != nil })
	r.Close()
	if got, _ := r.Get(j.ID); got.State != StateRunning {
		t.Fatalf("the job is %s after a shutdown in its post-pass, want %s for the next boot", got.State, StateRunning)
	}

	next := openRunner(t, Config{Dir: dir, Resolver: res, Meta: &stoppingMapper{stop: func() {}}})
	waitJob(t, next, j.ID, StateDone)
}
