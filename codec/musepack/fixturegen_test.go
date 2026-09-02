//go:build mpcfixtures

package musepack_test

// Offline fixture generator. Run with:
//
//	go generate ./codec/musepack
//
// which is wired to `go test -tags mpcfixtures -run ^TestGenerateFixtures$`.
// It needs the reference tools from `make mpc-tools`: mpcenc and mppenc are
// the only Musepack encoders there are, mpc2sv8 the only repacker, mpccut the
// only writer of a beginning silence, mpcchap the only writer of chapters.
//
// The committed fixtures exist so the decoder has a gate on a machine with no
// tools at all: every one of them holds a signal internal/testutil can
// rebuild from its seed, and the tests compare the decode against that rather
// than against a stored decode. Regenerating them is therefore not a
// re-baselining: if a fixture changes, the same assertions hold or the file
// is wrong.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

func TestGenerateFixtures(t *testing.T) {
	if !testutil.HaveMPCTools(t) {
		t.Fatal("the Musepack reference tools are required (run `make mpc-tools`)")
	}
	tmp := t.TempDir()
	for _, f := range mpcFixtures {
		if f.chapters != "" {
			testutil.MPCChapTool(t) // resolved before anything is written
			break
		}
	}
	// Derived fixtures need their origin written first, which the table's
	// order provides: every origin precedes its repack, cut or copy.
	for _, f := range mpcFixtures {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			t.Fatal(err)
		}
		switch {
		case f.repackOf != "":
			testutil.MPC2SV8File(t, f.repackOf, f.path)
		case f.cutOf != "":
			testutil.MPCCutFile(t, f.cutOf, f.path, f.cutFrom, 0)
		case f.chaptersOf != "":
			raw, err := os.ReadFile(f.chaptersOf)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			wav := filepath.Join(tmp, f.name()+".wav")
			testutil.WriteWAV(t, wav, intFormat(f.rate, f.channels), f.samples())
			args := append([]string(nil), f.args...)
			for _, tag := range f.tags {
				args = append(args, "--tag", tag)
			}
			if f.sv7 {
				testutil.MPPEncodeFile(t, wav, f.path, args...)
			} else {
				testutil.MPCEncodeFile(t, wav, f.path, args...)
			}
		}
		if f.chapters != "" {
			testutil.MPCChapFile(t, f.path, f.chapters)
		}
		if f.id3v1 {
			// mppenc writes no ID3v1, so the stacked-trailer shape real
			// taggers leave is appended here.
			raw, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			tag := make([]byte, 128)
			copy(tag, "TAG")
			copy(tag[3:], "Tagged")
			copy(tag[33:], "Wax Test")
			copy(tag[63:], "Fixtures")
			copy(tag[93:], "2026")
			if err := os.WriteFile(f.path, append(raw, tag...), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fi, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", f.path, fi.Size())
	}
}
