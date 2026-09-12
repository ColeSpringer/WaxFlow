package oracletest

// The names WaxFlow refuses by are waxlabel's names, deliberately: a file
// scanned with one reader and refused by the other should be talking about the
// same codec, and two vocabularies for one WAVE format tag is how that stops
// being true. MP2 is the pin because it is a permanent refusal (this build has
// a Layer III decoder and no Layer II one), so the cell is about the wording
// rather than about a decoder that might arrive.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

func TestRefusalUsesWaxlabelsCodecName(t *testing.T) {
	testutil.FFmpegEncoder(t, "mp2")
	path := filepath.Join(t.TempDir(), "mp2.wav")
	testutil.FFmpegGenerate(t, path, 44100, 1, "mp2", "-f", "wav")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	name := waxlabelTrack(t, raw).Codec
	if name == "" {
		t.Fatal("waxlabel names no codec for an MP2 WAV")
	}
	_, err = waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err == nil {
		t.Fatal("waxflow decoded an MP2 WAV; this build has no Layer II decoder")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("waxflow refuses with %v, which does not carry waxlabel's name %q", err, name)
	}
}
