package jobs

import (
	"os"
	"testing"

	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/source"
)

// TestATranscodeJobsBitrateReachesItsEncoder: a transcode job's bitrate
// reaches the mp3, opus and aac encoders, and its vorbisQuality the vorbis
// encoder, so its output differs in size from the same job at the default.
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
		shape  outshape.Params
		larger bool
	}{
		{outshape.Params{Format: "mp3", Bitrate: 192}, true},
		{outshape.Params{Format: "opus", Bitrate: 192}, true},
		{outshape.Params{Format: "aac", Bitrate: 48}, false},
		{outshape.Params{Format: "vorbis", VorbisQuality: 10}, true},
	} {
		def := size(Request{Type: TypeTranscode, Src: src, Params: outshape.Params{Format: c.shape.Format}})
		got := size(Request{Type: TypeTranscode, Src: src, Params: c.shape})
		if (c.larger && got <= def*4/3) || (!c.larger && got >= def*3/4) {
			t.Errorf("%+v wrote %d bytes, the default %d", c.shape, got, def)
		}
	}
}
