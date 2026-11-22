package jobs

import (
	"os"
	"testing"

	"github.com/colespringer/waxflow/source"
)

// TestATranscodeJobsBitrateReachesItsEncoder: a transcode job's bitrate
// reaches the mp3, opus and aac encoders, so its output differs in size from
// the same job at the encoder default.
func TestATranscodeJobsBitrateReachesItsEncoder(t *testing.T) {
	root := t.TempDir()
	src := source.Ref(writeWidthWAV(t, root, "stereo.wav", 2, 48000))
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: openRoots(t, root), MeasureTrack: measureTrack()})
	size := func(req Request) int {
		t.Helper()
		j, err := r.Create(req)
		if err != nil {
			t.Fatalf("Create %+v: %v", req, err)
		}
		done := waitJob(t, r, j.ID, StateDone)
		raw, err := os.ReadFile(r.OutputPath(done, 0))
		if err != nil {
			t.Fatal(err)
		}
		return len(raw)
	}
	for _, c := range []struct {
		format string
		kbps   int
		larger bool
	}{
		{"mp3", 192, true},
		{"opus", 192, true},
		{"aac", 48, false},
	} {
		def := size(Request{Type: TypeTranscode, Src: src, Format: c.format})
		got := size(Request{Type: TypeTranscode, Src: src, Format: c.format, Bitrate: c.kbps})
		if (c.larger && got <= def*4/3) || (!c.larger && got >= def*3/4) {
			t.Errorf("%s at %d kbit/s wrote %d bytes, the default %d", c.format, c.kbps, got, def)
		}
	}
}
