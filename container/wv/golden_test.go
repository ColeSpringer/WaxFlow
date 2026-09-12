package wv_test

import (
	"bytes"
	"flag"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the muxer's byte-exact output, which is what the
// ADR-0004 MuxerVersion stands for: the encoder's blocks, the total-samples
// field in each of its three states, and the APEv2 block after the audio.
// Regenerate with `make goldens` and review the diff. The encoder is
// integer-only, so the bytes are the same on every architecture.
func TestGoldenMuxOutputs(t *testing.T) {
	// Two blocks: 8192 frames is the encoder's block length, so 9000 gives a
	// full block and a short one (0.20 s at 44.1 kHz). One block would leave
	// the per-block index field and the short-final-block path unpinned.
	const frames = 9000
	tags := []container.Tag{
		{Key: "TITLE", Value: "Golden"},
		{Key: "ARTIST", Value: "First"},
		{Key: "ARTIST", Value: "Second"}, // one item, NUL-separated values
		{Key: "ID3", Value: "dropped"},   // a reserved item name apev2.Build skips
	}
	// One encode for every case: only the Track's projected length differs
	// between them, and the blocks do not depend on it.
	track, pkts := muxBlocks(t, frames, frames)
	mux := func(projected int64, seekable bool) []byte {
		track := track
		track.Samples = projected
		if seekable {
			var file seekBuf
			writeAll(t, &file, track, pkts, &wv.MuxerOptions{Tags: tags})
			return file.Buf
		}
		var out bytes.Buffer
		writeAll(t, &out, track, pkts, &wv.MuxerOptions{Tags: tags})
		return out.Bytes()
	}

	known := mux(frames, false)
	patched := mux(-1, true)
	pipe := mux(-1, false)

	// The invariant the patched case exists for, asserted rather than implied
	// by two identical files: a caller who could not state the length ends up
	// with exactly what a caller who could would have written.
	if !bytes.Equal(known, patched) {
		t.Errorf("the back-patched stream differs from the known-length one (%d vs %d bytes)",
			len(patched), len(known))
	}
	for _, tt := range []struct {
		name string
		raw  []byte
	}{
		{"golden-known.wv", known},
		{"golden-patched.wv", patched},
		{"golden-pipe.wv", pipe},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testutil.Golden(t, filepath.Join("testdata", tt.name), tt.raw, *update)
		})
	}
}
