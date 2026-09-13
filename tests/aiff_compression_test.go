package waxflow_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestAIFCWidthStatingTypesMatchFFmpeg scores the two AIFF-C compression
// types that name their own width, 'in24' and 'in32', against ffmpeg.
//
// They have no committed fixture because no encoder in reach writes one:
// ffmpeg emits a plain AIFF with no compression type at all for 24-bit
// output, so the type is a spelling only older Apple tools produce. Reading
// it wrong is silent, though (the samples are the right length and plausible
// audio at the wrong width), so the file is built here from the committed
// 24-bit AIFF and handed to ffmpeg, which does read the spelling: it answers
// pcm_s24be for 'in24' and pcm_s32be for 'in32'.
//
// Building it keeps the recipe next to the assertion, the way the mp4 'enda'
// case does.
func TestAIFCWidthStatingTypesMatchFFmpeg(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	for _, comp := range []string{"in24", "in32"} {
		t.Run(comp, func(t *testing.T) {
			width := 3
			if comp == "in32" {
				width = 4
			}
			raw := retypeAIFF(t, repoPath("testdata", "sine-s24.aiff"), comp, width)
			path := filepath.Join(t.TempDir(), comp+".aifc")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			got := decodeAll(t, container.BytesSource(raw), "aiff")
			defer audio.Put(got)
			ref := testutil.FFmpegDecodeS32(t, path)
			if len(ref) != got.N*got.Fmt.Channels {
				t.Fatalf("we decode %d samples, ffmpeg %d", got.N*got.Fmt.Channels, len(ref))
			}
			if i := testutil.DiffI32(testutil.Interleave(got), ref); i != -1 {
				t.Fatalf("sample %d differs from ffmpeg", i)
			}
		})
	}
}

// retypeAIFF rebuilds a plain AIFF as an AIFF-C carrying the given
// compression type, keeping the sample payload and re-cutting it to whole
// frames at the type's own width. COMM's sampleSize is written as 16, the
// conventional value for a type that states its width in its name, so a
// reader taking the field instead of the fourcc produces the wrong answer
// rather than the same one.
func retypeAIFF(t *testing.T, path, comp string, width int) []byte {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	be := binary.BigEndian
	var channels int
	var rate [10]byte
	var payload []byte
	for off := 12; off+8 <= len(src); {
		id, size := string(src[off:off+4]), int(be.Uint32(src[off+4:]))
		switch id {
		case "COMM":
			channels = int(int16(be.Uint16(src[off+8:])))
			copy(rate[:], src[off+16:off+26])
		case "SSND":
			payload = src[off+16 : off+8+size]
		}
		off += 8 + size + size&1
	}
	if channels == 0 || payload == nil {
		t.Fatalf("%s has no COMM or SSND", path)
	}
	frameBytes := width * channels
	payload = payload[:len(payload)/frameBytes*frameBytes]

	var out []byte
	put := func(b ...byte) { out = append(out, b...) }
	u32 := func(v uint32) { out = be.AppendUint32(out, v) }
	out = append(out, "FORM"...)
	u32(0) // patched below
	out = append(out, "AIFC"...)
	out = append(out, "FVER"...)
	u32(4)
	u32(0xA2805140)
	out = append(out, "COMM"...)
	u32(22 + 2)
	out = be.AppendUint16(out, uint16(channels))
	u32(uint32(len(payload) / frameBytes))
	out = be.AppendUint16(out, 16) // the conventional field, not the width
	out = append(out, rate[:]...)
	out = append(out, comp...)
	put(0, 0) // the empty compression name, padded
	out = append(out, "SSND"...)
	u32(uint32(8 + len(payload)))
	u32(0)
	u32(0)
	out = append(out, payload...)
	be.PutUint32(out[4:], uint32(len(out)-8))
	return out
}
