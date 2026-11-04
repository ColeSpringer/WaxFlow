package testutil

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestToolPinsAgree holds each reference tool's version to one value across
// the three places it is written: the tools.mk variable its recipe builds
// from, the directory the locators here look in, and the source archive
// pinned in Vectors. A bump that missed one would build one release and look
// for another. Under WAXFLOW_REQUIRE_* that reads as a puzzling "required but
// not found", and on a box that still holds the old build in testdata/tools
// the tests quietly run the old oracle.
func TestToolPinsAgree(t *testing.T) {
	mk, err := os.ReadFile("../../tools.mk")
	if err != nil {
		t.Fatal(err)
	}
	makeVar := func(name string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^` + name + `\s*:?=\s*(\S+)\s*$`).FindSubmatch(mk)
		if m == nil {
			t.Fatalf("tools.mk no longer sets %s", name)
		}
		return string(m[1])
	}
	pinned := map[string]bool{}
	for _, v := range Vectors {
		pinned[v.Name] = true
	}
	// Each archive name is derived the way tools.mk derives it.
	for _, c := range []struct{ makeVar, dir, archive string }{
		{"APE_TOOLS_VERSION", apeToolsVersion,
			"ape/MAC_" + strings.ReplaceAll(strings.TrimPrefix(apeToolsVersion, "mac-"), ".", "") + "_SDK.zip"},
		{"OPUS_TOOLS_VERSION", opusToolsVersion, "opus/" + opusToolsVersion + ".tar.gz"},
		{"MPC_TOOLS_VERSION", mpcToolsVersion,
			"musepack/musepack_src_" + strings.TrimPrefix(mpcToolsVersion, "musepack-") + ".tar.gz"},
		{"MPPENC_VERSION", mppencVersion, "musepack/" + mppencVersion + ".tar.bz2"},
	} {
		if got := makeVar(c.makeVar); got != c.dir {
			t.Errorf("tools.mk builds %s = %s, but the locator looks in testdata/tools/%s", c.makeVar, got, c.dir)
		}
		if !pinned[c.archive] {
			t.Errorf("the locator wants %s, but Vectors pins no %s", c.dir, c.archive)
		}
	}
}
