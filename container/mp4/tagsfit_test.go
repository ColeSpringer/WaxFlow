package mp4

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/container"
)

// TestTagsFitAgreesWithTheWrite: an ilst has no block cap, so a set far past
// every other writer's is answered fitting and written.
func TestTagsFitAgreesWithTheWrite(t *testing.T) {
	src := metaTone(alac.FrameSize)
	defer audio.Put(src)
	tags := []container.Tag{{Key: "COMMENT", Value: strings.Repeat("c", 200<<10)}}
	if err := TagsFit(tags); err != nil {
		t.Fatalf("TagsFit = %v, want nil", err)
	}
	var out bytes.Buffer
	muxALACMeta(t, &out, src, &MuxerOptions{Tags: tags})
	d, err := NewDemuxer(container.BytesSource(out.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Tags()["COMMENT"]; len(got) != 1 || len(got[0]) != 200<<10 {
		t.Error("the muxer did not write the COMMENT TagsFit answered for")
	}
}
