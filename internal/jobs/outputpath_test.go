package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// A job whose output file cannot be created fails as unwritable, as it did,
// and its message holds no part of the job directory, absolute or relative
// as the directory was configured.
func TestAnOutputThatCannotBeCreatedNamesNoPath(t *testing.T) {
	res, ref, srcID := openLib(t)
	for name, req := range map[string]Request{
		"a transcode": transcodeReq(ref, srcID),
		"a merge":     {Type: TypeMerge, Srcs: []source.Ref{source.Ref(ref), source.Ref(ref)}, Format: "flac"},
	} {
		for spelling, spell := range map[string]func(*testing.T, string) string{
			"an absolute directory": func(_ *testing.T, dir string) string { return dir },
			"a relative directory":  relTo,
		} {
			t.Run(name+" in "+spelling, func(t *testing.T) {
				abs := t.TempDir()
				dir := spell(t, abs)
				r := openRunner(t, Config{Dir: dir, Resolver: res, MeasureTrack: measureTrack()})
				j, err := r.store.create(req) // queued; no worker woken
				if err != nil {
					t.Fatal(err)
				}
				// A directory where the output goes, so the open is refused.
				if err := os.Mkdir(filepath.Join(abs, j.ID, "out.flac"), 0o755); err != nil {
					t.Fatal(err)
				}
				r.kick()
				failed := waitJob(t, r, j.ID, StateFailed)
				if failed.Error == nil || failed.Error.Code != string(waxerr.CodeOutputUnwritable) {
					t.Fatalf("the job failed with %+v, want %s", failed.Error, waxerr.CodeOutputUnwritable)
				}
				msg := failed.Error.Message
				if !strings.HasPrefix(msg, "jobs: creating output: open: ") {
					t.Errorf("the error %q is not the output's creation", msg)
				}
				if strings.Contains(msg, abs) || strings.Contains(msg, dir) || strings.Contains(msg, j.ID) || strings.Contains(msg, "out.flac") {
					t.Errorf("the error %q holds part of the job directory %s", msg, dir)
				}
			})
		}
	}
}
