package mpa

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
)

func fixtureBytes(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestLAMETagGapless pins the tag arithmetic on a committed fixture:
// libmp3lame's standard 576-sample encoder delay plus the fixed
// 529-sample decoder latency, and the exact trimmed length.
func TestLAMETagGapless(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixtureBytes(t, "sine-cbr128.mp3")), nil)
	if err != nil {
		t.Fatal(err)
	}
	track := d.Tracks()[0]
	if track.Delay != 576+529 {
		t.Errorf("Delay = %d, want %d", track.Delay, 576+529)
	}
	if track.Samples != 22050 {
		t.Errorf("Samples = %d, want 22050", track.Samples)
	}
	if track.Padding <= 0 {
		t.Errorf("Padding = %d, want positive (the encoder pads the last frame)", track.Padding)
	}
}

// TestUntaggedStream pins the no-tag behavior: no trims, unknown length.
func TestUntaggedStream(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixtureBytes(t, "sine-untagged.mp3")), nil)
	if err != nil {
		t.Fatal(err)
	}
	track := d.Tracks()[0]
	if track.Delay != 0 || track.Padding != 0 || track.Samples != -1 {
		t.Errorf("untagged track = delay %d padding %d samples %d, want 0/0/-1",
			track.Delay, track.Padding, track.Samples)
	}
}

// TestMatch covers the sniff: real streams match, tag-led streams match
// after the caller's ID3 skip, close-but-wrong bytes do not.
func TestMatch(t *testing.T) {
	if !Match(fixtureBytes(t, "sine-untagged.mp3")) {
		t.Error("clean stream did not match")
	}
	if !Match(fixtureBytes(t, "sine-vbr.mp3")) {
		t.Error("Xing-led stream did not match")
	}
	if Match([]byte("RIFF....WAVEfmt ")) {
		t.Error("WAV matched as MP3")
	}
	if Match([]byte{0xFF, 0xFB}) {
		t.Error("bare sync word with no frame behind it matched")
	}
}

// synthStream builds a long CBR stream by repeating the first frame of a
// committed fixture: valid framing (the walk never decodes), thousands
// of frames, no oracle needed.
func synthStream(t testing.TB, frames int) []byte {
	t.Helper()
	raw := fixtureBytes(t, "sine-untagged.mp3")
	h, err := mp3.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	frame := raw[:h.Size()]
	return bytes.Repeat(frame, frames)
}

// TestIndexSnapshotRestore round-trips the frame index through the
// sidecar blob and rejects blobs that do not fit the source.
func TestIndexSnapshotRestore(t *testing.T) {
	stream := synthStream(t, 5000)
	src := container.BytesSource(stream)

	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if blob := d.IndexSnapshot(); blob != nil {
		t.Fatal("snapshot before any walk should be nil")
	}
	if _, err := d.SeekSample(0, int64(4999)*1152); err != nil {
		t.Fatal(err)
	}
	blob := d.IndexSnapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}

	// A fresh demuxer accepts the blob and seeks identically.
	d2, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d2.RestoreIndex(blob) {
		t.Fatal("snapshot rejected by an identical source")
	}
	if d2.IndexSnapshot() != nil {
		t.Error("restored index re-snapshots without growth")
	}
	l1, err := d.SeekSample(0, 3_000_000)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := d2.SeekSample(0, 3_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if l1 != l2 {
		t.Errorf("restored index seeks to %d, walked index to %d", l2, l1)
	}
	var p1, p2 container.Packet
	if err := d.ReadPacket(&p1); err != nil {
		t.Fatal(err)
	}
	if err := d2.ReadPacket(&p2); err != nil {
		t.Fatal(err)
	}
	if p1.PTS != p2.PTS || !bytes.Equal(p1.Data, p2.Data) {
		t.Error("restored index delivers different packets")
	}

	// Rejections: a shorter source, a corrupt count, a torn blob.
	short, err := NewDemuxer(container.BytesSource(stream[:len(stream)/2]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if short.RestoreIndex(blob) {
		t.Error("blob accepted by a truncated source")
	}
	corrupt := append([]byte(nil), blob...)
	corrupt[len(corrupt)/2] ^= 0xFF
	d3, _ := NewDemuxer(src, nil)
	_ = d3.RestoreIndex(corrupt) // must not panic; acceptance depends on where the flip landed
	if d3.RestoreIndex(blob[:len(blob)/3]) {
		t.Error("torn blob accepted")
	}
}

// TestRepeatedFrameStreamReads sanity-checks the synthetic stream: every
// frame is delivered with consecutive PTS.
func TestRepeatedFrameStreamReads(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(synthStream(t, 100)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for i := int64(0); ; i++ {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			if i != 100 {
				t.Fatalf("delivered %d frames, want 100", i)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != i*1152 {
			t.Fatalf("frame %d PTS = %d", i, pkt.PTS)
		}
	}
}
