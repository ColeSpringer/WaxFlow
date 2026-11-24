package jobs

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A transcode job's warnings come in the order things happen: the input
// damage each read finds and a failed post-pass, then the clipping and loss
// notes read from the finished result.
func TestTranscodeJobWarnsInTheOrderThingsHappen(t *testing.T) {
	root := t.TempDir()
	hot := hotLib(t, root, true)
	hole, _ := lossFixtures(t, root)
	const holed = "lib/holed.flac"
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: &failingMapper{}})

	warnings := func(ref string) []string {
		t.Helper()
		j, err := r.Create(transcodeReq(ref, pinID(t, res, ref)))
		if err != nil {
			t.Fatal(err)
		}
		return waitJob(t, r, j.ID, StateDone).Warnings
	}
	at := func(ws []string, what string, is func(string) bool) int {
		t.Helper()
		i := slices.IndexFunc(ws, is)
		if i < 0 {
			t.Fatalf("warnings = %q, want a %s line", ws, what)
		}
		return i
	}
	postPass := func(w string) bool { return strings.HasPrefix(w, "metadata post-pass: ") }
	lossLine := fmt.Sprintf("input damage: %d samples", hole)

	ws := warnings(hot)
	if at(ws, "post-pass", postPass) > at(ws, "clipping", func(w string) bool { return strings.HasPrefix(w, "clipping: ") }) {
		t.Errorf("warnings = %q, want the failed post-pass before the clipping note", ws)
	}

	ws = warnings(holed)
	finding := at(ws, "input finding", func(w string) bool {
		return strings.HasPrefix(w, "input damage: ") && !strings.HasPrefix(w, lossLine)
	})
	loss := at(ws, "loss", func(w string) bool { return strings.HasPrefix(w, lossLine) })
	if finding > loss || at(ws, "post-pass", postPass) > loss {
		t.Errorf("warnings = %q, want the input finding and the failed post-pass before the loss note", ws)
	}
}
