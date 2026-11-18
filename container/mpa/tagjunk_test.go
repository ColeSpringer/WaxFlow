package mpa_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/waxerr"
)

// freeJunk is n bytes of junk that open with the header at hdr in the free
// format. A kin header of no stated size confirms the metadata frame in front
// of it and cannot be the first audio frame, so the scan starts behind that
// frame.
func freeJunk(hdr []byte, n int) []byte {
	junk := bytes.Repeat([]byte{0x11}, n)
	copy(junk, hdr[:4])
	junk[2] &= 0x0F
	return junk
}

// packetsOf reads every packet's bytes, tolerant.
func packetsOf(t *testing.T, raw []byte) (*mpa.Demuxer, [][]byte) {
	t.Helper()
	d, err := mpa.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return d, out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, bytes.Clone(pkt.Data))
	}
}

// TestJunkBehindTheMetadataFrameIsReported: bytes skipped between the
// metadata frame and the first audio frame are damage at the frame's end,
// which Strict refuses at the open. The packets are the clean file's, and
// the clean file, audio straight behind its metadata frame, warns nothing.
func TestJunkBehindTheMetadataFrameIsReported(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	tagEnd := frameOffsets(t, raw)[1]
	file := slices.Concat(raw[:tagEnd], freeJunk(raw[tagEnd:], 64), raw[tagEnd:])

	clean, want := packetsOf(t, raw)
	if ws := clean.Warnings(); len(ws) != 0 {
		t.Fatalf("the clean file warns: %v", ws)
	}
	d, got := packetsOf(t, file)
	if !slices.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("%d packets behind the junk, want the clean file's %d", len(got), len(want))
	}
	if a, b := d.Tracks()[0], clean.Tracks()[0]; a.Samples != b.Samples || a.Delay != b.Delay || a.Padding != b.Padding {
		t.Errorf("the track is %d samples trimmed %d/%d, the clean file's %d trimmed %d/%d",
			a.Samples, a.Delay, a.Padding, b.Samples, b.Delay, b.Padding)
	}
	finding := container.Warning{Offset: tagEnd, Msg: "64 unparsable bytes after the metadata frame", Kind: container.Damage}
	if ws := d.Warnings(); !slices.Equal(ws, []container.Warning{finding}) {
		t.Errorf("warnings = %v, want %v", ws, finding)
	}

	_, err := mpa.NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: true})
	if !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Fatalf("a strict open answered %v, want malformed", err)
	}
	if want := "mp3: 64 unparsable bytes after the metadata frame (at offset 417)"; err.Error() != want {
		t.Errorf("a strict open refused with %q, want %q", err, want)
	}
}
