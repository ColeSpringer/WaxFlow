package riff

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestAnOpenThatHasReadDeclinesARestore: the index answers for the reader
// that delivers the packets, so an open that has read one declines a blob an
// identical unread open accepts.
func TestAnOpenThatHasReadDeclinesARestore(t *testing.T) {
	raw := wavHeader(tagMP3, 2, 44100, 1152, 0, mp3Extra(0), testutil.SyntheticMP3Frames(5000), -1)
	testutil.DeclinesARestoreOnceRead(t, func() (container.Demuxer, error) { return NewDemuxer(container.BytesSource(raw), nil) })
}
