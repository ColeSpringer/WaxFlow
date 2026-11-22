package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/source"
)

// TestOldJobJSONStillRuns: a job.json an older build queued loads through the
// store into the shape it named, maps to the engine options that build gave
// it, and runs to done. Each fixture pins its source at a fixed mtime, which
// the test gives the copy.
func TestOldJobJSONStillRuns(t *testing.T) {
	const mtime = 1764547200000000000 // the fixtures' sourceId
	for _, c := range []struct {
		file  string
		shape outshape.Params
		want  waxflow.TranscodeOptions
	}{
		{"job-f30800b-flac.json",
			outshape.Params{Format: "flac", Rate: 22050, Channels: 1, Bits: 16, FLACLevel: -1},
			waxflow.TranscodeOptions{Format: "flac", Rate: 22050, Channels: 1, BitDepth: 16, FLACLevel: -1}},
		{"job-f30800b-opus.json",
			outshape.Params{Format: "opus", Channels: 1, Bitrate: 96},
			waxflow.TranscodeOptions{Format: "opus", Channels: 1, Bitrate: 96000}},
	} {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile(testutil.PkgFile("testdata", c.file))
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				ID      string         `json:"id"`
				Request map[string]any `json:"request"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, stored.ID), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stored.ID, jobFile), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			copyFixture(t, root, fixtureWAV)
			at := time.Unix(0, mtime)
			if err := os.Chtimes(filepath.Join(root, fixtureWAV), at, at); err != nil {
				t.Fatal(err)
			}
			r := openUnstartedRunner(t, Config{Dir: dir, Resolver: openRoots(t, root)})
			j, ok := r.Get(stored.ID)
			if !ok {
				t.Fatalf("the store did not load %s", c.file)
			}
			if j.Request.Params != c.shape {
				t.Errorf("shape = %+v, want %+v", j.Request.Params, c.shape)
			}
			if got := j.Request.TranscodeOptions(0, ""); !reflect.DeepEqual(got, c.want) {
				t.Errorf("options = %+v\nwant %+v", got, c.want)
			}
			// Opening requeued the job and wrote it back: the same keys.
			var again struct {
				Request map[string]any `json:"request"`
			}
			if b, err := os.ReadFile(filepath.Join(dir, stored.ID, jobFile)); err != nil {
				t.Fatal(err)
			} else if err := json.Unmarshal(b, &again); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(again.Request, stored.Request) {
				t.Errorf("the stored request came back as %v, want %v", again.Request, stored.Request)
			}
			r.start()
			waitJob(t, r, stored.ID, StateDone)
		})
	}
}

// TestQueuedJobWithAStrayKnobFailsByName: a stored job is checked again when
// it runs, so one holding a knob its format lacks, or a value the wire refuses,
// fails in the job's own field names, never the engine's.
func TestQueuedJobWithAStrayKnobFailsByName(t *testing.T) {
	res, ref, srcID := openLib(t)
	for _, c := range []struct {
		name string
		typ  Type
		set  func(*Request)
		code string
		want string
	}{
		{"a flac level on opus", TypeTranscode, func(r *Request) { r.FLACLevel = 5 },
			"unsupported-format", "flacLevel applies to flac output; this request names opus"},
		{"a wavpack level on a merge", TypeMerge, func(r *Request) { r.WavPackLevel = 3 },
			"unsupported-format", "wavpackLevel applies to wavpack output; this request names opus"},
		{"an ape level on a split", TypeSplit, func(r *Request) { r.APELevel = 3000 },
			"unsupported-format", "apeLevel applies to ape output; this request names opus"},
		{"a vorbis quality on opus", TypeTranscode, func(r *Request) { r.VorbisQuality = 4 },
			"unsupported-format", "vorbisQuality applies to vorbis output; this request names opus"},
		{"a bit rate past the bound", TypeTranscode, func(r *Request) { r.Bitrate = waxflow.MaxBitrate/1000 + 1 },
			"invalid-request", fmt.Sprintf("bitrate %d: want at most %d kbit/s", waxflow.MaxBitrate/1000+1, waxflow.MaxBitrate/1000)},
		{"a negative bit rate", TypeTranscode, func(r *Request) { r.Bitrate = -1 },
			"invalid-request", "bitrate -1: want 0 for the format's default or a positive integer"},
		{"a vorbis quality out of range", TypeTranscode, func(r *Request) { r.Format, r.VorbisQuality = "vorbis", 50 },
			"invalid-request", "vorbisQuality 50: want -1 to 10"},
		{"a flac level out of range", TypeTranscode, func(r *Request) { r.Format, r.FLACLevel = "flac", 9 },
			"invalid-request", "flacLevel 9: want -1 to 8"},
		{"a wavpack level out of range", TypeMerge, func(r *Request) { r.Format, r.WavPackLevel = "wavpack", 7 },
			"invalid-request", "wavpackLevel 7: want 1 to 4"},
		{"an ape level not written", TypeSplit, func(r *Request) { r.Format, r.APELevel = "ape", 4000 },
			"invalid-request", "apeLevel 4000 decodes but is not written; want 1000, 2000, or 3000"},
		{"an ape level off its set", TypeTranscode, func(r *Request) { r.Format, r.APELevel = "ape", 2500 },
			"invalid-request", "apeLevel 2500: want 1000, 2000, or 3000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := Request{Type: c.typ}
			switch c.typ {
			case TypeMerge:
				req.Srcs, req.SourceIDs = []source.Ref{source.Ref(ref)}, []string{srcID}
			case TypeSplit:
				req.Src, req.SourceID, req.Cuts = source.Ref(ref), srcID, []int64{1000}
			default:
				req.Src, req.SourceID = source.Ref(ref), srcID
			}
			req.Format = "opus"
			c.set(&req)
			dir := t.TempDir()
			writeJob(t, dir, &Job{SchemaVersion: SchemaVersion, ID: mustULID(t), Type: c.typ,
				State: StateQueued, Request: req, Created: time.Now().UTC()})
			r := openRunner(t, Config{Dir: dir, Resolver: res, MeasureTrack: measureTrack()})
			jobs := r.List()
			if len(jobs) != 1 {
				t.Fatalf("%d jobs after reopen, want 1", len(jobs))
			}
			failed := waitJob(t, r, jobs[0].ID, StateFailed)
			if failed.Error == nil || failed.Error.Code != c.code || failed.Error.Message != c.want {
				t.Errorf("error = %+v, want %s %q", failed.Error, c.code, c.want)
			}
		})
	}
}

// TestStoredJobFailsAsItsCreationWould: a stored job is held again, when it
// runs, to every rule creation applies to its type from the request alone, and
// fails in creation's words.
func TestStoredJobFailsAsItsCreationWould(t *testing.T) {
	res, ref, srcID := openLib(t)
	for _, c := range []struct {
		name string
		req  Request
		want string
	}{
		{"an analyze job with a bit rate",
			Request{Type: TypeAnalyze, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Bitrate: 128}},
			"type analyze takes src and the silence fields"},
		{"an analyze job with a threshold and no silence",
			Request{Type: TypeAnalyze, Src: source.Ref(ref), SourceID: srcID, SilenceThresholdDB: -50},
			"silenceThresholdDb and silenceMinSeconds need silence:true"},
		{"a transcode with no format",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID},
			"transcode jobs need an explicit format (" + strings.Join(waxflow.OutputFormats(), ", ") + ")"},
		{"a bogus loudness",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Loudness: "bogus"},
			`loudness "bogus": want analyze (or omit)`},
		{"loudness analyze beside a gain",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Loudness: "analyze", Gain: "track"},
			"loudness analyze replaces gain; drop the gain field"},
		{"a gain spelled wrong",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Gain: "loud"},
			`gain "loud": want off, track, album, or a dB number`},
		{"a merge with a gain",
			Request{Type: TypeMerge, Srcs: []source.Ref{source.Ref(ref)}, SourceIDs: []string{srcID}, Params: outshape.Params{Format: "flac"}, Gain: "track"},
			"gain and loudness apply to transcode jobs; merge writes the samples it is given"},
		{"a split with loudness",
			Request{Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID, Cuts: []int64{1000}, Params: outshape.Params{Format: "flac"}, Loudness: "analyze"},
			"gain and loudness apply to transcode jobs; split writes the samples it is given"},
		{"a transcode with silence",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Silence: true},
			"the silence fields apply to analyze jobs"},
		{"a transcode with cuts",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Cuts: []int64{1000}},
			"cuts applies to split jobs"},
		{"a transcode with skip",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Skip: []int{0}},
			"skip applies to split jobs"},
		{"a transcode with titles",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, MemberTitles: []string{"x"}},
			"titles applies to merge jobs"},
		{"a transcode with members",
			Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: outshape.Params{Format: "flac"}, Srcs: []source.Ref{source.Ref(ref)}},
			"srcs applies to merge jobs"},
		{"a merge with a src",
			Request{Type: TypeMerge, Src: source.Ref(ref), Srcs: []source.Ref{source.Ref(ref)}, SourceIDs: []string{srcID}, Params: outshape.Params{Format: "flac"}},
			"type merge takes srcs, not src"},
		{"a merge with no members",
			Request{Type: TypeMerge, Params: outshape.Params{Format: "flac"}},
			"type merge needs srcs: at least one member"},
		{"a merge past the member bound",
			Request{Type: TypeMerge, Srcs: slices.Repeat([]source.Ref{source.Ref(ref)}, 1001), Params: outshape.Params{Format: "flac"}},
			"1001 members is past the 1000-member bound; a merge is a play queue, not a library"},
		{"a transcode with no src",
			Request{Type: TypeTranscode, Params: outshape.Params{Format: "flac"}},
			"src is required"},
		{"a merge with a title short",
			Request{Type: TypeMerge, Srcs: []source.Ref{source.Ref(ref), source.Ref(ref)}, SourceIDs: []string{srcID, srcID},
				MemberTitles: []string{"x"}, Params: outshape.Params{Format: "flac"}},
			"titles has 1 entries but the merge has 2 members; a title per member, or none"},
		{"a merge with an empty member",
			Request{Type: TypeMerge, Srcs: []source.Ref{source.Ref(ref), ""}, SourceIDs: []string{srcID, srcID}, Params: outshape.Params{Format: "flac"}},
			"member 1 has no src"},
		{"an analyze threshold past its bound",
			Request{Type: TypeAnalyze, Src: source.Ref(ref), SourceID: srcID, Silence: true, SilenceThresholdDB: -100},
			"silence threshold -100 dBFS outside -90..0"},
		{"an analyze minimum past its bound",
			Request{Type: TypeAnalyze, Src: source.Ref(ref), SourceID: srcID, Silence: true, SilenceMinSeconds: 120},
			"silence minimum duration 2m0s outside 0..1m0s"},
		{"a timeline job with silence",
			Request{Type: TypeTimeline, Srcs: []source.Ref{source.Ref(ref)}, Silence: true},
			"type timeline takes srcs, spans and crossfadeSeconds"},
		{"a timeline job with cuts",
			Request{Type: TypeTimeline, Srcs: []source.Ref{source.Ref(ref)}, Cuts: []int64{1000}},
			"type timeline takes srcs, spans and crossfadeSeconds"},
		{"a timeline job with titles",
			Request{Type: TypeTimeline, Srcs: []source.Ref{source.Ref(ref)}, MemberTitles: []string{"x"}},
			"type timeline takes srcs, spans and crossfadeSeconds"},
		{"a timeline job with a format",
			Request{Type: TypeTimeline, Srcs: []source.Ref{source.Ref(ref)}, Params: outshape.Params{Format: "opus"}},
			"type timeline takes srcs, spans and crossfadeSeconds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeJob(t, dir, &Job{SchemaVersion: SchemaVersion, ID: mustULID(t), Type: c.req.Type,
				State: StateQueued, Request: c.req, Created: time.Now().UTC()})
			r := openRunner(t, Config{Dir: dir, Resolver: res, MeasureTrack: measureTrack()})
			jobs := r.List()
			if len(jobs) != 1 {
				t.Fatalf("%d jobs after reopen, want 1", len(jobs))
			}
			failed := waitJob(t, r, jobs[0].ID, StateFailed)
			if failed.Error == nil || failed.Error.Code != "invalid-request" || failed.Error.Message != c.want {
				t.Errorf("error = %+v, want invalid-request %q", failed.Error, c.want)
			}
		})
	}
}
