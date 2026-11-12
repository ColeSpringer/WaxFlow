package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/format"
)

// TestSrcIsReadInOnePlace: src carries a reference in text form, and a
// handler that read the parameter for itself would resolve pct: text as a
// reference. srcParam is the one reader of a query, and SignParams' own
// methods the only ones that index it.
func TestSrcIsReadInOnePlace(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no package sources found (%v)", err)
	}
	var reads []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			query := strings.Contains(line, `Get("src")`) && name != "params.go"
			index := strings.Contains(line, `["src"]`) && name != "types.go"
			if query || index {
				reads = append(reads, fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(reads) != 0 {
		t.Errorf("src is read outside srcParam and SignParams:\n%s", strings.Join(reads, "\n"))
	}
}

// TestSignParamsHoldAReferencesBytes: src is the reference's own bytes in
// Go, text form in JSON, and whatever a query reader decodes back in a
// query, so one built here marshals as losslessly as one that was read.
func TestSignParamsHoldAReferencesBytes(t *testing.T) {
	var read SignParams
	if err := json.Unmarshal([]byte(`{"src":"pct:lib/caf%E9.flac","format":"wav"}`), &read); err != nil {
		t.Fatal(err)
	}
	if read["src"] != "lib/caf\xe9.flac" || read["format"] != "wav" {
		t.Fatalf("read %q", read)
	}
	b, err := json.Marshal(SignParams{"src": "lib/caf\xe9.flac", "format": "wav"})
	if err != nil || string(b) != `{"format":"wav","src":"pct:lib/caf%E9.flac"}` {
		t.Fatalf("marshaled %s, %v", b, err)
	}
	for _, raw := range []string{"lib/caf\xe9.flac", "pct:abc", "lib/a.flac"} {
		got, err := srcParam(SignParams{"src": raw}.query())
		if err != nil || got != raw {
			t.Errorf("a query reader gets %q (%v) for %q", got, err, raw)
		}
		var back SignParams
		b, _ := json.Marshal(SignParams{"src": raw})
		if err := json.Unmarshal(b, &back); err != nil || back["src"] != raw {
			t.Errorf("%q came back from JSON as %q (%v)", raw, back["src"], err)
		}
	}
}

// TestProbeJSONKeepsWarningsLegible: a demuxer's warning can quote bytes of
// the file that are not UTF-8, and a job document shows the same warning
// with them written \xNN. The engine's own list is shared, so it is not
// written to.
func TestProbeJSONKeepsWarningsLegible(t *testing.T) {
	info := &format.Info{Container: "wav", Warnings: []string{"ok", "chunk \"\xff\xfe\" skipped"}, Notes: []string{"tag \xe9"}}
	out := ProbeJSON(info, nil)
	if out.Warnings[1] != `chunk "\xff\xfe" skipped` || out.Notes[0] != `tag \xe9` || out.Warnings[0] != "ok" {
		t.Errorf("ProbeJSON gave warnings %q, notes %q", out.Warnings, out.Notes)
	}
	if info.Warnings[1] != "chunk \"\xff\xfe\" skipped" || info.Notes[0] != "tag \xe9" {
		t.Errorf("ProbeJSON wrote to the engine's lists: %q, %q", info.Warnings, info.Notes)
	}
	clean := &format.Info{Container: "wav", Warnings: []string{"ok"}}
	if got := ProbeJSON(clean, nil); &got.Warnings[0] != &clean.Warnings[0] {
		t.Error("a clean list was copied")
	}
}
