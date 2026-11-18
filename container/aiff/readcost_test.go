package aiff

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestLinearReadReadsThePayloadOnce pins what a linear read of a PCM AIFF
// costs past the open: one read per packet of audio.StandardChunk frames,
// each beginning where the last ended, and the payload's bytes once.
func TestLinearReadReadsThePayloadOnce(t *testing.T) {
	const frames, frameBytes = 10*audio.StandardChunk + 7, 4
	raw := buildAIFCn(compNONE, 2, 16, make([]byte, frames*frameBytes), frames)
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	testutil.ReadsOnceInOrder(t, src, d.ReadPacket, int64(len(raw)-frames*frameBytes), int64(len(raw)), 11)
}
