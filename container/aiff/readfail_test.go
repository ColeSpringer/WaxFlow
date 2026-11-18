package aiff

import (
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAFailedPayloadReadNamesTheChunk: a payload read the source cannot serve
// fails under the SSND chunk's text, as unreadable, at the packet that meets
// the failing byte.
func TestAFailedPayloadReadNamesTheChunk(t *testing.T) {
	const frames, frameBytes = 2 * audio.StandardChunk, 4
	raw := buildAIFCn(compNONE, 2, 16, make([]byte, frames*frameBytes), frames)
	at := int64(len(raw)-frames*frameBytes) + audio.StandardChunk*frameBytes
	d, err := NewDemuxer(&testutil.FailingSource{Src: container.BytesSource(raw), From: at, To: at + 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("first packet: %v", err)
	}
	err = d.ReadPacket(&pkt)
	if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || !strings.HasPrefix(err.Error(), "aiff: reading SSND data: ") {
		t.Errorf("second packet: %v (%s), want an unreadable source under \"aiff: reading SSND data\"", err, waxerr.CodeOf(err))
	}
}
