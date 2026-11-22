package jobs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// A member whose file fails to close costs the merge nothing: the job is
// done, its output is the one a clean merge writes, and one warning names
// the member with no path of the daemon's.
func TestAMergeMemberCloseErrorIsAWarningAndTheJobCompletes(t *testing.T) {
	merged := func(t *testing.T, hook func(i int, f io.Closer) io.Closer) (*Job, []byte, []source.Ref) {
		r, refs := mergeMembers(t, []int{24000, 24000}, nil)
		r.hooks.memberCloser = hook
		j, err := r.Create(Request{Type: TypeMerge, Srcs: refs, Params: outshape.Params{Format: "flac"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		done := waitJob(t, r, j.ID, StateDone)
		out, err := os.ReadFile(r.OutputPath(done, 0))
		if err != nil {
			t.Fatal(err)
		}
		return done, out, refs
	}
	_, clean, _ := merged(t, nil)
	for member := range 2 {
		t.Run(fmt.Sprintf("member %d", member), func(t *testing.T) {
			// A file closed twice refuses the second close with its path.
			twice, err := os.Create(filepath.Join(t.TempDir(), "member"))
			if err != nil {
				t.Fatal(err)
			}
			done, out, refs := merged(t, func(i int, f io.Closer) io.Closer {
				if i != member {
					return f
				}
				return closerFunc(func() error {
					f.Close()
					twice.Close()
					return twice.Close()
				})
			})
			if got := soleOutput(t, done).Samples; got != 48000 {
				t.Errorf("the output holds %d samples, want 48000", got)
			}
			if !bytes.Equal(out, clean) {
				t.Errorf("the output is %d bytes and differs from a clean merge's %d", len(out), len(clean))
			}
			want := fmt.Sprintf("member %d (%s): close: file already closed", member, refs[member].Text())
			if !slices.Equal(done.Warnings, []string{want}) {
				t.Errorf("the job warned %q, want only %q", done.Warnings, want)
			}
		})
	}
}

// A member whose file stops reading fails the job as an unreadable source,
// with no path of the daemon's in the demuxer's message.
func TestMergeMemberReadErrorNamesNoPath(t *testing.T) {
	r, refs := mergeMembers(t, []int{24000, 24000}, nil)
	// Closed as the member opens, the file refuses every read after its
	// headers.
	r.hooks.memberCloser = func(i int, f io.Closer) io.Closer {
		if i == 1 {
			f.Close()
		}
		return f
	}
	j, err := r.Create(Request{Type: TypeMerge, Srcs: refs, Params: outshape.Params{Format: "flac"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	failed := waitJob(t, r, j.ID, StateFailed)
	if failed.Error == nil || failed.Error.Code != string(waxerr.CodeSourceUnreadable) {
		t.Fatalf("the job failed with %+v, want %s", failed.Error, waxerr.CodeSourceUnreadable)
	}
	msg := failed.Error.Message
	if !strings.Contains(msg, "reading") {
		t.Errorf("the error %q is not a read's", msg)
	}
	if dir := filepath.Dir(t.TempDir()); strings.Contains(msg, dir) {
		t.Errorf("the error %q holds the daemon's directory %s", msg, dir)
	}
}
