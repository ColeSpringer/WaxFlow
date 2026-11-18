package aiff

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestAnOpenThatHasReadDeclinesARestore: the index answers for the reader
// that delivers the packets, so an open that has read one declines a blob an
// identical unread open accepts.
func TestAnOpenThatHasReadDeclinesARestore(t *testing.T) {
	raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, testutil.SyntheticMP3Frames(5000), 5000)
	testutil.DeclinesARestoreOnceRead(t, func() (container.Demuxer, error) { return NewDemuxer(container.BytesSource(raw), nil) })
}
