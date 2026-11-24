package oracletest

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/cli"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestSplitJobMatchesCLISplit: one sheet over one tagged rip, split by
// `waxflow split` and by a split job, gives the same pieces: the same tags,
// art among them, and the same audio, piece for piece.
func TestSplitJobMatchesCLISplit(t *testing.T) {
	env, _ := newJobsEnv(t)
	wav := filepath.Join(env.root, "rip.wav")
	if err := os.WriteFile(wav, rampWAV(t, 44100, 2, 6*44100), 0o644); err != nil {
		t.Fatal(err)
	}
	rip := filepath.Join(env.root, "rip.flac")
	runCLI(t, "transcode", wav, rip)
	tagFixture(t, rip, map[tag.Key]string{
		tag.Album: "The Album", tag.Artist: "The Band", tag.Title: "The Rip",
		tag.TrackNumber: "7", tag.ReplayGainTrackGain: "-3.00 dB",
	}, tinyPNG(t), "")
	// A lead-in before track 1, and an untitled track 3.
	sheet := filepath.Join(env.root, "rip.cue")
	if err := os.WriteFile(sheet, []byte("FILE \"rip.flac\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:01:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:03:00\n"+
		"  TRACK 03 AUDIO\n    INDEX 01 00:04:30\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	runCLI(t, "split", rip, out, "--cue", sheet)
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)

	j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
		"type": "split", "src": "lib/rip.flac", "format": "flac", "cue": "lib/rip.cue",
	}), http.StatusCreated)
	done := waitJob(t, env, j.ID)
	if done.State != jobs.StateDone {
		t.Fatalf("split job ended %s: %+v", done.State, done.Error)
	}
	if len(names) != 4 || len(done.Outputs) != 4 {
		t.Fatalf("the CLI wrote %q and the job %d pieces, want four each", names, len(done.Outputs))
	}

	for i, name := range names {
		cliPiece, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		resp := env.get(t, fmt.Sprintf("/jobs/%s/result/%d", j.ID, i), nil)
		jobPiece := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("result %d: status %d", i, resp.StatusCode)
		}
		cliInfo, jobInfo := pieceMeta(t, cliPiece), pieceMeta(t, jobPiece)
		if !reflect.DeepEqual(cliInfo.Tags, jobInfo.Tags) {
			t.Errorf("piece %d: the CLI's tags %v, the job's %v", i, cliInfo.Tags, jobInfo.Tags)
		}
		if got := jobInfo.Tags["ALBUM"]; !slices.Equal(got, []string{"The Album"}) {
			t.Errorf("piece %d carries ALBUM %q, want the rip's", i, got)
		}
		for key := range jobInfo.Tags {
			if strings.HasPrefix(key, "REPLAYGAIN_") {
				t.Errorf("piece %d carries the rip's %s", i, key)
			}
		}
		if p, q := cliInfo.FrontPicture(), jobInfo.FrontPicture(); p == nil || q == nil || !bytes.Equal(p.Data, q.Data) {
			t.Errorf("piece %d: the job's art is not the CLI's", i)
		}
		if !samePCM(decodePCM(t, cliPiece), decodePCM(t, jobPiece)) {
			t.Errorf("piece %d: the CLI's audio and the job's differ", i)
		}
	}
}

// TestSplitJobMatchesCLISplitNearTheCap: a rip whose tags sit just under the
// embedding cap, split by a sheet with a long first title, gives the same
// pieces from `waxflow split` and a split job in every format. Each piece is
// written; the post-pass tags flac, opus, vorbis and mp3 in full with no
// warning, and FLAC in Ogg and WavPack drop only the rip's COMMENT their
// block cannot hold, with one warning.
func TestSplitJobMatchesCLISplitNearTheCap(t *testing.T) {
	env, _ := newJobsEnv(t)
	wav := filepath.Join(env.root, "rip.wav")
	if err := os.WriteFile(wav, rampWAV(t, 44100, 2, 6*44100), 0o644); err != nil {
		t.Fatal(err)
	}
	rip := filepath.Join(env.root, "rip.flac")
	runCLI(t, "transcode", wav, rip)
	comment := strings.Repeat("c", 49030)
	tagFixture(t, rip, map[tag.Key]string{tag.Album: "The Album", tag.Comment: comment}, nil, "")
	long := strings.Repeat("t", 120)
	sheet := filepath.Join(env.root, "rip.cue")
	if err := os.WriteFile(sheet, []byte("FILE \"rip.flac\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \""+long+"\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 00:03:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		format, container string
		kept              [2]bool // whether each piece carries COMMENT
		warned            bool
	}{
		{"flac", "", [2]bool{true, true}, false},
		{"opus", "", [2]bool{true, true}, false},
		{"vorbis", "", [2]bool{true, true}, false},
		{"mp3", "", [2]bool{true, true}, false},
		{"flac", "ogg", [2]bool{false, true}, true},
		{"wavpack", "", [2]bool{false, false}, true},
	} {
		t.Run(tc.format+"/"+tc.container, func(t *testing.T) {
			out := t.TempDir()
			args := []string{"split", rip, out, "--cue", sheet, "--format", tc.format}
			body := map[string]any{"type": "split", "src": "lib/rip.flac", "format": tc.format, "cue": "lib/rip.cue"}
			if tc.container != "" {
				args = append(args, "--container", tc.container)
				body["container"] = tc.container
			}
			runCLI(t, args...)
			entries, err := os.ReadDir(out)
			if err != nil {
				t.Fatal(err)
			}
			j := decodeJob(t, env.postJSON(t, "/jobs", body), http.StatusCreated)
			done := waitJob(t, env, j.ID)
			if done.State != jobs.StateDone {
				t.Fatalf("split job ended %s: %+v", done.State, done.Error)
			}
			if len(entries) != 2 || len(done.Outputs) != 2 {
				t.Fatalf("the CLI wrote %d pieces and the job %d, want two each", len(entries), len(done.Outputs))
			}
			var want []string
			if tc.warned {
				want = []string{"tag COMMENT is too large for the output and was not embedded"}
			}
			if !slices.Equal(done.Warnings, want) {
				t.Errorf("the job warned %q, want %q", done.Warnings, want)
			}
			for i, e := range entries {
				cliPiece, err := os.ReadFile(filepath.Join(out, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				resp := env.get(t, fmt.Sprintf("/jobs/%s/result/%d", j.ID, i), nil)
				if jobPiece := readBody(t, resp); !bytes.Equal(cliPiece, jobPiece) {
					t.Errorf("piece %d: the CLI's bytes and the job's differ", i)
				}
				info, err := label.New().Read(context.Background(), container.BytesSource(cliPiece),
					strings.TrimPrefix(filepath.Ext(e.Name()), "."), meta.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if kept := slices.Equal(info.Tags["COMMENT"], []string{comment}); kept != tc.kept[i] {
					t.Errorf("piece %d carries COMMENT %t, want %t", i, kept, tc.kept[i])
				}
				if i == 0 && !slices.Equal(info.Tags["TITLE"], []string{long}) {
					t.Errorf("piece 0 is titled %.20q, want its own long title", info.Tags["TITLE"])
				}
			}
		})
	}
}

func runCLI(t *testing.T, args ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cli.Execute("test", args, &stdout, &stderr); code != 0 {
		t.Fatalf("waxflow %s: exit %d: %s", strings.Join(args, " "), code, stderr.String())
	}
}

func pieceMeta(t *testing.T, raw []byte) *meta.Info {
	t.Helper()
	info, err := label.New().Read(context.Background(), container.BytesSource(raw), "flac", meta.ReadOptions{Pictures: true})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// samePCM reports whether two int-domain buffers hold the same frames, and
// some.
func samePCM(a, b *audio.Buffer) bool {
	if a.N == 0 || a.N != b.N || a.Fmt.Channels != b.Fmt.Channels {
		return false
	}
	for c := range a.Fmt.Channels {
		if !slices.Equal(a.ChanI(c), b.ChanI(c)) {
			return false
		}
	}
	return true
}
