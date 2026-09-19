package mp4

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpenReadsTheMoovAndAHeader pins what opening an MP3 movie costs: the
// boxes ahead of the payload, and the one frame header the entry is checked
// against. That header used to cost a whole 128 KiB window; the payloads
// here are longer than two windows so a round-up would show.
func TestOpenReadsTheMoovAndAHeader(t *testing.T) {
	var extras [2]int64
	for i, n := range []int{2000, 4000} {
		payload := testutil.SyntheticMP3Frames(n)
		raw := movie{entry: v1SoundEntry(".mp3", 2, 16, 44100, 0), unitBytes: testutil.SyntheticMP3FrameLen,
			frames: n, chunkFrames: 64, timescale: 44100, sttsDelta: 1152}.build()
		raw = append(raw[:len(raw)-len(payload)], payload...)
		head := int64(len(raw) - len(payload))
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d frames: %d bytes in %d reads, %d ahead of the payload", n, src.Bytes, src.Reads, head)
		extras[i] = src.Bytes - head
		if extras[i] > 64 {
			t.Errorf("opening a %d-frame movie read %d bytes past the %d ahead of the payload, want one frame header", n, extras[i], head)
		}
	}
	if extras[0] != extras[1] {
		t.Errorf("the open read %d bytes of payload for 2000 frames and %d for 4000; it must not scale with the payload", extras[0], extras[1])
	}
}

// TestFragmentedOpenReadsTheHead pins what opening a fragmented movie costs:
// the head, and nothing that scales with the fragments behind it.
//
// The walk used to run to EOF after the moov was in hand, reading two box
// headers per fragment (the moof and its mdat) off the raw source with no
// srcwin coalescing. On a ranged HTTP source that is one round trip apiece,
// which is what made a six-second file cost dozens of requests to open. The
// walk derives nothing past the moov, so it stops there.
func TestFragmentedOpenReadsTheHead(t *testing.T) {
	var reads [2]int
	var bytes [2]int64
	for i, npkt := range []int{20, 40} {
		// No declared length, so the init carries no edit-list duration and the
		// open runs the segment-index scan: the more demanding case, and the
		// one ffmpeg's default fragmented output produces.
		track, pkts := opusTrackFor(312, -1, npkt)
		init, media := segmentize(t, track, pkts, 960) // one packet per fragment
		raw := append(append([]byte(nil), init...), media...)
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d fragments: %d bytes in %d reads, head is %d bytes of %d",
			npkt, src.Bytes, src.Reads, len(init), len(raw))
		reads[i], bytes[i] = src.Reads, src.Bytes
		// The head, plus the box headers between the moov and the first moof
		// that the length resolver reads looking for a segment index. That is
		// a styp and a moof here, two headers; the bound is loose because the
		// sharp claim is the one below, that neither number moves with the
		// fragment count.
		if limit := int64(len(init)) + 64; src.Bytes > limit {
			t.Errorf("opening a %d-fragment movie read %d bytes, more than the %d-byte head plus a few headers",
				npkt, src.Bytes, len(init))
		}
	}
	if reads[0] != reads[1] || bytes[0] != bytes[1] {
		t.Errorf("the open cost %d reads/%d bytes at 20 fragments and %d/%d at 40; "+
			"it must not scale with the fragment count", reads[0], bytes[0], reads[1], bytes[1])
	}
}
