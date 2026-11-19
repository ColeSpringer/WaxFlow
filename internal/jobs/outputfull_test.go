package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAFullDiskNamesNoPath: a job whose output stops taking writes fails as
// unwritable, as it did, and the message the muxer builds on the write's
// error holds no part of the job directory.
func TestAFullDiskNamesNoPath(t *testing.T) {
	const full = "/dev/full"
	if runtime.GOOS == "windows" {
		t.Skipf("%s is a unix device: a Windows binary run from a WSL path can stat the name and cannot link to it", full)
	}
	if _, err := os.Stat(full); err != nil {
		t.Skipf("no %s on this system", full)
	}
	res, ref, srcID := openLib(t)
	for name, req := range map[string]Request{
		"a transcode": transcodeReq(ref, srcID),
		"a merge":     {Type: TypeMerge, Srcs: []source.Ref{source.Ref(ref), source.Ref(ref)}, Format: "flac"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := openRunner(t, Config{Dir: dir, Resolver: res, MeasureTrack: measureTrack()})
			j, err := r.store.create(req) // queued; no worker woken
			if err != nil {
				t.Fatal(err)
			}
			// The output's name opens, and every write to it finds no space.
			if err := os.Symlink(full, filepath.Join(r.store.jobDir(j.ID), "out.flac")); err != nil {
				t.Fatal(err)
			}
			r.kick()
			failed := waitJob(t, r, j.ID, StateFailed)
			if failed.Error == nil || failed.Error.Code != string(waxerr.CodeOutputUnwritable) {
				t.Fatalf("the job failed with %+v, want %s", failed.Error, waxerr.CodeOutputUnwritable)
			}
			msg := failed.Error.Message
			if !strings.Contains(msg, "no space left on device") {
				t.Errorf("the error %q is not the write's", msg)
			}
			if strings.Contains(msg, dir) || strings.Contains(msg, j.ID) || strings.Contains(msg, "out.flac") {
				t.Errorf("the error %q holds part of the job directory %s", msg, r.store.jobDir(j.ID))
			}
		})
	}
}
