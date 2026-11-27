package jobs

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/source"
)

// recordingMapper reads info, or nothing past the container's tags, and
// records each post-pass, the tags it would write included.
type recordingMapper struct {
	info    *meta.Info // what Read finds; nil reads nothing
	mu      sync.Mutex
	applied map[string]map[string][]string // by file name
}

func (m *recordingMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	if m.info != nil {
		return m.info, nil
	}
	return &meta.Info{}, nil
}

func (m *recordingMapper) Apply(_ context.Context, path string, info *meta.Info, extra []container.Tag) error {
	tags := maps.Clone(info.Tags)
	for _, t := range extra {
		delete(tags, t.Key)
	}
	for _, t := range extra {
		tags[t.Key] = append(tags[t.Key], t.Value)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.applied == nil {
		m.applied = map[string]map[string][]string{}
	}
	m.applied[filepath.Base(path)] = tags
	return nil
}

// writeTaggedRip writes long.wav's audio as a WavPack rip carrying an album's
// tags, a stale ReplayGain and a track number of its own: WavPack's APEv2
// tags are ones a probe reads back.
func writeTaggedRip(t *testing.T, root string) string {
	t.Helper()
	in, err := os.Open(filepath.Join(root, "long.wav"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	src, err := container.FileSource(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(root, "rip.wv"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, err = waxflow.New().Transcode(context.Background(), src, "wav", out,
		waxflow.TranscodeOptions{Format: "wavpack"},
		waxflow.WithTags([]container.Tag{
			{Key: "ALBUM", Value: "The Album"}, {Key: "ARTIST", Value: "The Band"},
			{Key: "TITLE", Value: "The Rip"}, {Key: "TRACKNUMBER", Value: "7"},
			{Key: "REPLAYGAIN_TRACK_GAIN", Value: "-3.00 dB"},
		}))
	if err != nil {
		t.Fatal(err)
	}
	return "lib/rip.wv"
}

// TestSplitJobTagsItsPieces: a split job's pieces carry the source's tags
// with their own number, total and title on top, and none of the rip's
// ReplayGain, whether the muxer embeds them (WavPack, probed back) or the
// post-pass writes them (FLAC, whose tags the engine's probe does not read).
func TestSplitJobTagsItsPieces(t *testing.T) {
	root := t.TempDir()
	writeLongWAV(t, root)
	ref := writeTaggedRip(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	titles := []string{"One", "", "Three"}

	for _, tc := range []struct {
		name   string
		params outshape.Params
	}{
		{"embedded by the muxer", outshape.Params{Format: "wavpack"}},
		{"through the post-pass", outshape.Params{Format: "flac", FLACLevel: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordingMapper{}
			r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: m})
			j, err := r.Create(Request{
				Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID,
				Params: tc.params,
				Cuts:   []int64{2 * 44100, 4 * 44100}, Pieces: TitledPieces(titles), TrackTotal: 3,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			if len(done.Outputs) != 3 {
				t.Fatalf("%d outputs, want 3", len(done.Outputs))
			}
			for i, out := range done.Outputs {
				var tags map[string][]string
				if tc.params.Format == "flac" {
					tags = m.applied[out.File]
				} else {
					tags = probeTags(t, r.OutputPath(done, i))
				}
				title := titles[i]
				if title == "" {
					title = "The Rip"
				}
				for key, want := range map[string]string{
					"ALBUM": "The Album", "ARTIST": "The Band", "TITLE": title,
					"TRACKNUMBER": strconv.Itoa(i + 1), "TRACKTOTAL": "3",
				} {
					if got := tags[key]; !slices.Equal(got, []string{want}) {
						t.Errorf("%s: %s = %q, want %q", out.File, key, got, want)
					}
				}
				for key := range tags {
					if strings.HasPrefix(key, "REPLAYGAIN_") {
						t.Errorf("%s carries the rip's %s", out.File, key)
					}
				}
			}
		})
	}
}

func probeTags(t *testing.T, path string) map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := container.FileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(context.Background(), src, strings.TrimPrefix(filepath.Ext(path), "."), nil)
	if err != nil {
		t.Fatal(err)
	}
	return info.Tags
}

// TestSplitJobWithNoPiecesNumbersFromOne: a stored split with no pieces
// numbers each piece by its place, skipped ones counted, totals them, and
// keeps the source's title.
func TestSplitJobWithNoPiecesNumbersFromOne(t *testing.T) {
	root := t.TempDir()
	writeLongWAV(t, root)
	ref := writeTaggedRip(t, root)
	res := openRoots(t, root)
	m := &recordingMapper{}
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: m})
	j, err := r.Create(Request{
		Type: TypeSplit, Src: source.Ref(ref), SourceID: pinID(t, res, ref),
		Params: outshape.Params{Format: "flac", FLACLevel: -1},
		Cuts:   []int64{2 * 44100, 4 * 44100}, Skip: []int{0},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	for i, out := range done.Outputs {
		tags := m.applied[out.File]
		for key, want := range map[string]string{
			"TITLE": "The Rip", "TRACKNUMBER": strconv.Itoa(i + 2), "TRACKTOTAL": "3",
		} {
			if got := tags[key]; !slices.Equal(got, []string{want}) {
				t.Errorf("%s: %s = %q, want %q", out.File, key, got, want)
			}
		}
	}
}

// albumMapper reads a fixed album and fails or cancels the post-pass as
// told.
type albumMapper struct {
	tags     map[string][]string
	applyErr error
	onApply  func()
}

func (m *albumMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	return &meta.Info{Tags: m.tags}, nil
}

func (m *albumMapper) Apply(context.Context, string, *meta.Info, []container.Tag) error {
	if m.onApply != nil {
		m.onApply()
	}
	return m.applyErr
}

// writeNearCapRip writes the sine fixture as a WavPack rip whose tags sit just
// under the embedding cap: an ALBUM and a long COMMENT, which a probe reads
// back with no mapper.
func writeNearCapRip(t *testing.T, root, comment string) string {
	t.Helper()
	copyFixture(t, root, fixtureWAV)
	in, err := os.Open(filepath.Join(root, fixtureWAV))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	src, err := container.FileSource(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(root, "nearcap.wv"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(context.Background(), src, "wav", out, waxflow.TranscodeOptions{Format: "wavpack"},
		waxflow.WithTags([]container.Tag{{Key: "ALBUM", Value: "The Album"}, {Key: "COMMENT", Value: comment}})); err != nil {
		t.Fatal(err)
	}
	return "lib/nearcap.wv"
}

// TestSplitJobMakesRoomForEachPiece: a rip whose tags sit just under the
// embedding cap writes every piece, a long title included. A piece's writer
// is handed the rip's tags then the piece's own wherever its block holds
// them; where it cannot, the rip's are left out until the set fits. That is
// one warning for the split where the loss is final (the muxer is the final
// tag writer, or there is no mapper), and silent where the post-pass writes
// the full set.
func TestSplitJobMakesRoomForEachPiece(t *testing.T) {
	comment := strings.Repeat("c", 49030)
	long := strings.Repeat("t", 120)
	root := t.TempDir()
	ref := writeNearCapRip(t, root, comment)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	flac := outshape.Params{Format: "flac", FLACLevel: -1}
	oggFLAC := outshape.Params{Format: "flac", Container: waxflow.ContainerOgg, FLACLevel: -1}
	wavpack := outshape.Params{Format: "wavpack"}
	for _, tc := range []struct {
		name     string
		params   outshape.Params
		title    string // the first piece's
		noMapper bool
		kept     [2]bool // whether each piece carries COMMENT where nothing rewrites it
		warned   bool
	}{
		{"wavpack", wavpack, "", false, [2]bool{false, false}, true},
		{"alac", outshape.Params{Format: "alac", Container: waxflow.ContainerProgressive}, "", false, [2]bool{true, true}, false},
		{"ogg flac", oggFLAC, "", false, [2]bool{true, true}, false},
		{"flac", flac, "", false, [2]bool{}, false},
		{"flac, a long title", flac, long, false, [2]bool{}, false},
		{"opus, a long title", outshape.Params{Format: "opus"}, long, false, [2]bool{}, false},
		{"vorbis, a long title", outshape.Params{Format: "vorbis"}, long, false, [2]bool{}, false},
		{"mp3, a long title", outshape.Params{Format: "mp3"}, long, false, [2]bool{}, false},
		{"ogg flac, a long title", oggFLAC, long, false, [2]bool{false, true}, true},
		{"wavpack, a long title", wavpack, long, false, [2]bool{false, false}, true},
		{"flac, a long title, no mapper", flac, long, true, [2]bool{false, true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordingMapper{}
			cfg := Config{Dir: t.TempDir(), Resolver: res, Meta: m}
			if tc.noMapper {
				cfg.Meta = nil
			}
			r := openRunner(t, cfg)
			j, err := r.Create(Request{
				Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID, Params: tc.params, Cuts: []int64{1000},
				Pieces: TitledPieces([]string{tc.title, ""}),
			})
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			var want []string
			if tc.warned {
				want = []string{"tag COMMENT is too large for the output and was not embedded"}
			}
			if !slices.Equal(done.Warnings, want) {
				t.Errorf("warnings = %q, want %q", done.Warnings, want)
			}
			if len(done.Outputs) != 2 {
				t.Fatalf("%d pieces written, want 2", len(done.Outputs))
			}
			postPassed := !tc.noMapper && !waxflow.OutputEmbedsTags(tc.params.Format, tc.params.Container)
			for i, out := range done.Outputs {
				raw, err := os.ReadFile(r.OutputPath(done, i))
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 && tc.title != "" && !bytes.Contains(raw, []byte(tc.title)) {
					t.Errorf("piece 0 lacks its own title")
				}
				if postPassed {
					got := m.applied[out.File]
					if len(got["COMMENT"]) != 1 || len(got["COMMENT"][0]) != len(comment) || len(got["ALBUM"]) != 1 {
						t.Errorf("the post-pass of piece %d wrote %d COMMENT and %d ALBUM values, want the rip's",
							i, len(got["COMMENT"]), len(got["ALBUM"]))
					}
					continue
				}
				if kept := bytes.Contains(raw, []byte(comment)); kept != tc.kept[i] {
					t.Errorf("piece %d carries COMMENT %t, want %t", i, kept, tc.kept[i])
				}
			}
		})
	}
}

// TestTranscodeJobMakesRoomForItsReplayGain: a source whose tags sit just
// under the embedding cap transcodes with loudness analyze and carries the
// ReplayGain it embeds. Where the writer's block cannot hold the source's tags
// beside it, the source's are left out until the set fits, with one warning;
// an MP4 holds them all.
func TestTranscodeJobMakesRoomForItsReplayGain(t *testing.T) {
	comment := strings.Repeat("c", 49060)
	const warning = "tag COMMENT is too large for the output and was not embedded"
	for _, tc := range []struct {
		name    string
		params  outshape.Params
		dropped bool
	}{
		{"wavpack", outshape.Params{Format: "wavpack"}, true},
		{"ogg flac", outshape.Params{Format: "flac", Container: waxflow.ContainerOgg, FLACLevel: -1}, true},
		{"alac", outshape.Params{Format: "alac", Container: waxflow.ContainerProgressive}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ref := writeLongWAV(t, root)
			res := openRoots(t, root)
			m := &recordingMapper{info: &meta.Info{Tags: map[string][]string{"COMMENT": {comment}}}}
			r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: m})
			j, err := r.Create(Request{
				Type: TypeTranscode, Src: source.Ref(ref), SourceID: pinID(t, res, ref), Params: tc.params, Loudness: "analyze",
			})
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			if got := slices.Contains(done.Warnings, warning); got != tc.dropped {
				t.Errorf("warned %t, want %t: %q", got, tc.dropped, done.Warnings)
			}
			raw, err := os.ReadFile(r.OutputPath(done, 0))
			if err != nil {
				t.Fatal(err)
			}
			// An MP4 freeform spells the name in lowercase, as taggers do.
			upper := bytes.ToUpper(raw)
			for _, key := range []string{"REPLAYGAIN_TRACK_GAIN", "REPLAYGAIN_TRACK_PEAK"} {
				if !bytes.Contains(upper, []byte(key)) {
					t.Errorf("the output carries no %s", key)
				}
			}
			if kept := bytes.Contains(raw, []byte(comment)); kept == tc.dropped {
				t.Errorf("the output carries COMMENT %t, want %t", kept, !tc.dropped)
			}
		})
	}
}

// TestSplitJobWarnsOfAFailedPostPass: a post-pass that fails names its piece
// and the job finishes; one that fails because the daemon stopped is no
// warning, and the job is left to rerun.
func TestSplitJobWarnsOfAFailedPostPass(t *testing.T) {
	// stop, when set, is told the runner before the job is created.
	split := func(t *testing.T, m *albumMapper, stop func(*Runner)) (*Runner, string) {
		t.Helper()
		res, ref, srcID := openLib(t)
		r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: m})
		if stop != nil {
			stop(r)
		}
		j, err := r.Create(Request{
			Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID,
			Params: outshape.Params{Format: "flac", FLACLevel: -1}, Cuts: []int64{1000},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r, j.ID
	}
	album := map[string][]string{"ALBUM": {"The Album"}}

	t.Run("live", func(t *testing.T) {
		r, id := split(t, &albumMapper{tags: album, applyErr: errors.New("tag write refused")}, nil)
		done := waitJob(t, r, id, StateDone)
		want := []string{
			"metadata post-pass of out.0.flac: tag write refused",
			"metadata post-pass of out.1.flac: tag write refused",
		}
		if !slices.Equal(done.Warnings, want) {
			t.Errorf("warnings = %q, want %q", done.Warnings, want)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		m := &albumMapper{tags: album, applyErr: context.Canceled}
		var once sync.Once
		r, id := split(t, m, func(r *Runner) { m.onApply = func() { once.Do(r.cancel) } })
		waitFor(t, "the post-pass", func() bool { return r.ctx.Err() != nil })
		r.Close()
		got, _ := r.Get(id)
		if got.State != StateRunning || len(got.Warnings) != 0 {
			t.Errorf("the job is %s with warnings %q, want %s and none", got.State, got.Warnings, StateRunning)
		}
	})
}
