package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// pathMapper is a mapper whose Apply does what the test says with the output
// path it is handed.
type pathMapper struct {
	apply func(path string) error
}

func (pathMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	return &meta.Info{Tags: map[string][]string{"TITLE": {"job"}}}, nil
}

func (m pathMapper) Apply(_ context.Context, path string, _ *meta.Info, _ []container.Tag) error {
	return m.apply(path)
}

// relTo is dir as a path relative to the working directory, which it moves
// to a temp directory so that a relative path exists even where the package
// is on another volume.
func relTo(t *testing.T, dir string) string {
	t.Helper()
	wd := t.TempDir()
	testutil.Chdir(t, wd)
	rel, err := filepath.Rel(wd, dir)
	if err != nil {
		t.Fatalf("no relative path from %s to %s: %v", wd, dir, err)
	}
	return rel
}

// A mapper is handed the output's absolute path and quotes it in its own
// words; the job's warning keeps the words and drops the directory, however
// the jobs directory was spelt and wherever a symlink in it leads.
func TestThePostPassWarningNamesNoPath(t *testing.T) {
	res, ref, srcID := openLib(t)
	quoted := func(path, dir string) error {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "meta: metadata write",
			fmt.Errorf("replace %s: create temp file in %s: permission denied", path, dir))
	}
	asHanded := func(path string) error { return quoted(path, filepath.Dir(path)) }
	for _, tc := range []struct {
		name  string
		dir   func(t *testing.T) string
		apply func(path string) error
	}{
		{"an absolute directory", func(t *testing.T) string { return t.TempDir() }, asHanded},
		{"a relative directory", func(t *testing.T) string { return relTo(t, t.TempDir()) }, asHanded},
		{
			"a symlinked directory, quoted as it resolves",
			func(t *testing.T) string {
				link := filepath.Join(t.TempDir(), "jobs")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Skipf("no symlinks here: %v", err)
				}
				return link
			},
			func(path string) error {
				real, err := filepath.EvalSymlinks(filepath.Dir(path))
				if err != nil {
					return err
				}
				return quoted(filepath.Join(real, filepath.Base(path)), real)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := openRunner(t, Config{Dir: tc.dir(t), Resolver: res, Meta: pathMapper{apply: tc.apply}})
			j, err := r.Create(transcodeReq(ref, srcID))
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			const want = "metadata post-pass: meta: metadata write: replace out.flac: create temp file in .: permission denied"
			if !slices.Contains(done.Warnings, want) {
				t.Errorf("the job warned %q, want %q", done.Warnings, want)
			}
		})
	}
}

// Every message a job records loses the job's directory, in each spelling a
// message may quote it: as the store was given it, absolute, and with its
// symlinks resolved. The error, the wait and the warnings alike.
func TestEveryMessageAJobRecordsLosesItsDirectory(t *testing.T) {
	real := t.TempDir()
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	link := filepath.Join(t.TempDir(), "jobs")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	for _, given := range []string{link, relTo(t, link)} {
		s, err := openStore(given, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		j, err := s.create(analyzeReq("lib/a.wav", ""))
		if err != nil {
			t.Fatal(err)
		}
		for _, quoted := range []string{link, real} {
			dir := filepath.Join(quoted, j.ID)
			text := fmt.Sprintf("replace %s: no space in %s", filepath.Join(dir, "out.flac"), dir)
			const want = "replace out.flac: no space in ."
			got := s.update(j.ID, false, func(job *Job) {
				job.Warnings = append(job.Warnings, text)
				job.Error = &ErrInfo{Code: string(waxerr.CodeOutputUnwritable), Message: text}
			})
			if w := got.Warnings[len(got.Warnings)-1]; w != want || got.Error.Message != want {
				t.Errorf("under %s, quoting %s: warning %q and error %q, want %q", given, quoted, w, got.Error.Message, want)
			}
			waiting, _, _ := s.requeue(j.ID, time.Hour, string(waxerr.CodeRootUnavailable), text)
			if waiting.Waiting.Message != want {
				t.Errorf("under %s, quoting %s: the wait says %q, want %q", given, quoted, waiting.Waiting.Message, want)
			}
		}
	}
}

// A tagging library quotes the path it is handed with %q, which escapes
// every separator on Windows and, anywhere, a quote in a folder's name. The
// quoted spelling of the job's directory goes too.
func TestAMessageQuotingTheJobDirectoryWithQLosesIt(t *testing.T) {
	root := t.TempDir()
	if runtime.GOOS != "windows" { // where a name cannot hold a quote
		root = filepath.Join(root, `data"dir`)
	}
	s, err := openStore(filepath.Join(root, "jobs"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.create(analyzeReq("lib/a.wav", ""))
	if err != nil {
		t.Fatal(err)
	}
	dir := s.jobDir(j.ID)
	text := fmt.Sprintf("could not identify %q; %q is empty", filepath.Join(dir, "out.aac"), dir)
	const want = `could not identify "out.aac"; "." is empty`
	got := s.update(j.ID, false, func(job *Job) { job.Warnings = append(job.Warnings, text) })
	if w := got.Warnings[0]; w != want {
		t.Errorf("the warning %q, want %q", w, want)
	}
}

// A job read back at open reads cut, whatever an older build recorded, and
// its file stays as it was.
func TestAJobLoadedAtOpenNamesNoDirectory(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.create(analyzeReq("lib/a.wav", ""))
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("no space in %s", s.jobDir(j.ID))
	const want = "no space in ."
	s.mu.Lock()
	stored := s.jobs[j.ID]
	stored.State = StateFailed
	stored.Error = &ErrInfo{Code: string(waxerr.CodeOutputUnwritable), Message: text}
	stored.Waiting = &Wait{Code: string(waxerr.CodeRootUnavailable), Message: text}
	stored.Warnings = []string{text}
	err = s.persistLocked(stored) // as written before the cut
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.jobDir(j.ID), jobFile)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := openStore(root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.get(j.ID)
	if !ok {
		t.Fatal("the job did not load")
	}
	if got.Error.Message != want || got.Waiting.Message != want || !slices.Equal(got.Warnings, []string{want}) {
		t.Errorf("loaded with error %q, wait %q, warnings %q; want %q in each", got.Error.Message, got.Waiting.Message, got.Warnings, want)
	}
	if after, err := os.ReadFile(file); err != nil || !bytes.Equal(after, before) {
		t.Errorf("open rewrote the job's file (%v):\n%s\nwas\n%s", err, after, before)
	}
}

// Every string a job holds is either a message the cut reaches or a value
// it must leave as it is (an output's name, a reference, a code). A string
// field added to Job fails here until it is one or the other.
func TestEveryStringOfAJobIsCutOrKept(t *testing.T) {
	cut := map[string]bool{"Error.Message": true, "Waiting.Message": true, "Warnings[]": true}
	kept := map[string]bool{
		"Type": true, "State": true,
		"Request.Type": true, "Request.Src": true, "Request.SourceID": true, "Request.SourceIDs[]": true,
		"Request.Params.Format": true, "Request.Params.Container": true, "Request.Gain": true, "Request.Loudness": true,
		"Request.Srcs[]": true, "Request.MemberTitles[]": true, "Request.Pieces[].Title": true,
		"Error.Code": true, "Waiting.Code": true,
		"Outputs[].File": true, "Outputs[].MediaType": true, "Outputs[].Container": true,
		"Analysis.ReplayGainTrackGain": true, "Analysis.ReplayGainTrackPeak": true, "Analysis.Silence.Version": true,
		"Timeline.Tl": true, "Progress.Phase": true,
	}
	s, err := openStore(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j := &Job{ID: "01M7RKJQGK9DXJMA032T84AT6T"}
	text := fmt.Sprintf("no space in %s", s.jobDir(j.ID))
	strs := map[string]reflect.Value{}
	var fill func(v reflect.Value, at string)
	fill = func(v reflect.Value, at string) {
		switch v.Kind() {
		case reflect.String:
			strs[at] = v
		case reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem()))
			fill(v.Elem(), at)
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			fill(v.Index(0), at+"[]")
		case reflect.Struct:
			for i := range v.NumField() {
				if f := v.Type().Field(i); f.IsExported() {
					fill(v.Field(i), strings.TrimPrefix(at+"."+f.Name, "."))
				}
			}
		}
	}
	fill(reflect.ValueOf(j).Elem(), "")
	delete(strs, "ID") // the cut needs it, and nothing writes a path there
	for _, v := range strs {
		v.SetString(text)
	}
	s.cutDir(j)
	for at, v := range strs {
		switch got := v.String(); {
		case cut[at] && got != "no space in .":
			t.Errorf("%s reads %q: a message the cut missed", at, got)
		case kept[at] && got != text:
			t.Errorf("%s reads %q: the cut changed a value it must keep", at, got)
		case !cut[at] && !kept[at]:
			t.Errorf("%s is a string the cut neither reaches nor is told to keep", at)
		}
	}
}

// quotingMapper fails as a tagging library does, quoting the job's directory
// in its own words at each place the runner calls it.
type quotingMapper struct {
	root      string // the runner's jobs directory, which holds one job
	failRead  bool
	failApply bool
}

// jobDir is the one job's directory under the root.
func (m quotingMapper) jobDir() string {
	entries, _ := os.ReadDir(m.root)
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(m.root, e.Name())
		}
	}
	return m.root
}

func (m quotingMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	if m.failRead {
		return nil, waxerr.Wrap(waxerr.CodeMalformedInput, "meta: metadata read",
			fmt.Errorf("cache %s: unparsable frame", filepath.Join(m.jobDir(), "tags.tmp")))
	}
	return &meta.Info{Tags: map[string][]string{"TITLE": {"job"}}}, nil
}

func (m quotingMapper) Apply(context.Context, string, *meta.Info, []container.Tag) error {
	if m.failApply {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "meta: metadata write",
			fmt.Errorf("create temp file in %s: permission denied", m.jobDir()))
	}
	return nil
}

// A mapper that bakes the job's directory into an error's text, wherever the
// runner calls it, leaves the warning it causes and no part of the directory.
func TestAMapperQuotingTheJobDirectoryIsRecordedWithoutIt(t *testing.T) {
	res, ref, srcID := openLib(t)
	mergeRoot := t.TempDir()
	merge := Request{Type: TypeMerge, Params: outshape.Params{Format: "alac", Container: waxflow.ContainerProgressive}, Srcs: []source.Ref{
		source.Ref(writeMergeWAV(t, mergeRoot, "a.wav", 4800)), source.Ref(writeMergeWAV(t, mergeRoot, "b.wav", 4800)),
	}}
	for _, tc := range []struct {
		name   string
		res    source.Resolver
		req    Request
		mapper quotingMapper
		want   string
	}{
		{"reading", res, transcodeReq(ref, srcID), quotingMapper{failRead: true}, "metadata unread: meta: metadata read: cache tags.tmp: unparsable frame"},
		{"applying", res, transcodeReq(ref, srcID), quotingMapper{failApply: true}, "metadata post-pass: meta: metadata write: create temp file in .: permission denied"},
		{"a member's title", openRoots(t, mergeRoot), merge, quotingMapper{failRead: true}, "member 0: reading title: meta: metadata read: cache tags.tmp: unparsable frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mapper.root = t.TempDir()
			r := openRunner(t, Config{Dir: tc.mapper.root, Resolver: tc.res, Meta: tc.mapper, MeasureTrack: measureTrack()})
			j, err := r.Create(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			if !slices.Contains(done.Warnings, tc.want) {
				t.Errorf("the job warned %q, want %q", done.Warnings, tc.want)
			}
			for _, w := range done.Warnings {
				if strings.Contains(w, tc.mapper.root) || strings.Contains(w, j.ID) {
					t.Errorf("the warning %q holds part of the job directory %s", w, r.store.jobDir(j.ID))
				}
			}
		})
	}
}

// An output that is not there when the job sizes it fails the job as it did,
// without the output's path.
func TestAnOutputGoneBeforeItsStatNamesNoPath(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()
	r := openRunner(t, Config{Dir: dir, Resolver: res, Meta: pathMapper{apply: os.Remove}})
	j, err := r.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatal(err)
	}
	failed := waitJob(t, r, j.ID, StateFailed)
	if failed.Error == nil || failed.Error.Code != string(waxerr.CodeInternal) {
		t.Fatalf("the job failed with %+v, want %s", failed.Error, waxerr.CodeInternal)
	}
	msg := failed.Error.Message
	if !strings.HasPrefix(msg, "jobs: output stat: ") || strings.Contains(msg, dir) || strings.Contains(msg, "out.flac") {
		t.Errorf("the error %q, want the stat's refusal with no part of %s", msg, dir)
	}
}

// The silence map's write fails the job as unwritable, as it did, without
// the job directory.
func TestASilenceMapThatCannotBeWrittenNamesNoPath(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()
	r := openUnstartedRunner(t, Config{Dir: dir, Resolver: res})
	req := Request{Type: TypeAnalyze, Src: source.Ref(ref), SourceID: srcID, Silence: true}
	j, err := r.store.create(req) // queued; no worker yet
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the map goes, so the write is refused.
	if err := os.Mkdir(filepath.Join(r.store.jobDir(j.ID), silenceMapFile), 0o755); err != nil {
		t.Fatal(err)
	}
	r.start()
	failed := waitJob(t, r, j.ID, StateFailed)
	if failed.Error == nil || failed.Error.Code != string(waxerr.CodeOutputUnwritable) {
		t.Fatalf("the job failed with %+v, want %s", failed.Error, waxerr.CodeOutputUnwritable)
	}
	msg := failed.Error.Message
	if !strings.HasPrefix(msg, "jobs: writing the silence map: ") || strings.Contains(msg, dir) || strings.Contains(msg, silenceMapFile) {
		t.Errorf("the error %q, want the write's refusal with no part of %s", msg, dir)
	}
}

// downResolver is an embedder's resolver whose root is away: it refuses
// every reference with the system's error, path and all.
type downResolver struct{ dir string }

func (d downResolver) Resolve(context.Context, string) (*source.File, error) {
	return nil, waxerr.Wrap(waxerr.CodeRootUnavailable, "library is not mounted",
		&fs.PathError{Op: "open", Path: d.dir, Err: fs.ErrNotExist})
}

// A job that waits for its source says why without the directory its
// resolver's error quotes.
func TestAWaitingJobNamesNoPath(t *testing.T) {
	dir := t.TempDir()
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: downResolver{dir}, RetryEvery: time.Hour})
	j, err := r.Create(transcodeReq("lib/"+fixtureWAV, ""))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to say why it waits", func() bool {
		got, _ := r.Get(j.ID)
		return got.State == StateQueued && got.Waiting != nil
	})
	got, _ := r.Get(j.ID)
	const want = "library is not mounted: open: file does not exist"
	if got.Waiting.Code != string(waxerr.CodeRootUnavailable) || got.Waiting.Message != want {
		t.Errorf("the job waits with %+v, want %s and %q", got.Waiting, waxerr.CodeRootUnavailable, want)
	}
}
