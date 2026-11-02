package waxflow_test

// A native FLAC with damaged frames decodes to every frame that checks, as
// ffmpeg decodes it when told to drop a frame whose checksum fails.

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestFLACDamagedFramesMatchFFmpeg: 16 bytes inverted inside one frame, or
// inside two far apart, cost those frames and nothing else. Each lost frame is
// silence, so the decode keeps the clean length and what follows a hole sits
// where the clean file has it. Outside the holes it is ffmpeg's decode, sample
// for sample, with -err_detect crccheck+explode, which drops a frame whose
// CRC-16 fails and joins across it.
func TestFLACDamagedFramesMatchFFmpeg(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	clean := transcodeBytes(t, loopedWAV(t, 30), waxflow.TranscodeOptions{Format: "flac"})
	frames := payloads(t, clean, "flac")
	n := len(frames)
	starts := []int{bytes.Index(clean, frames[0])}
	for _, f := range frames {
		starts = append(starts, starts[len(starts)-1]+len(f))
	}
	ref, err := decodeAllDynamic(t, container.BytesSource(clean), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)
	const block = 4096 // the encode's block size
	if ref.N <= n*block-block || ref.N > n*block {
		t.Fatalf("%d samples in %d frames, not %d-sample blocks", ref.N, n, block)
	}

	for _, c := range []struct {
		name  string
		holes []int
	}{{"one hole", []int{n / 3}}, {"two holes", []int{n / 3, 2 * n / 3}}} {
		holes := c.holes
		t.Run(c.name, func(t *testing.T) {
			raw := append([]byte(nil), clean...)
			for _, k := range holes {
				mid := (starts[k] + starts[k+1]) / 2
				for i := mid - 8; i < mid+8; i++ {
					raw[i] ^= 0xFF
				}
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "flac")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			ch := got.Fmt.Channels
			if got.N != ref.N {
				t.Fatalf("decoded %d samples, want the clean %d", got.N, ref.N)
			}
			// Each hole is silence where the clean file has its frame.
			inter := testutil.Interleave(got)
			joined := make([]int32, 0, len(inter))
			for i := 0; i < got.N; i++ {
				if slices.ContainsFunc(holes, func(k int) bool { return i >= k*block && i < (k+1)*block }) {
					if slices.ContainsFunc(inter[i*ch:i*ch+ch], func(v int32) bool { return v != 0 }) {
						t.Fatalf("sample %d, in a hole, is not silence", i)
					}
					continue
				}
				joined = append(joined, inter[i*ch:i*ch+ch]...)
			}
			// What follows each hole sits where the clean file has it.
			for i, k := range holes {
				from := (k + 1) * block
				to := ref.N
				if i+1 < len(holes) {
					to = holes[i+1] * block
				}
				if j := sameFrames(got, from, ref, from, to-from); j >= 0 {
					t.Fatalf("after hole %d: differs from the clean decode at sample %d", i, from+j)
				}
			}
			// ffmpeg before 8.0 drops the frames beside a damaged one too
			// (measured, 6.1.1: three per hole), so only a newer one is this
			// oracle.
			if !testutil.FFmpegAtLeast(t, 8, 0) {
				t.Log("ffmpeg before 8.0 loses more than the damaged frame; its comparison is skipped")
				return
			}
			path := filepath.Join(t.TempDir(), "damaged.flac")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd := exec.Command(ffmpeg, "-v", "error", "-err_detect", "crccheck+explode", "-i", path,
				"-f", "s32le", "-c:a", "pcm_s32le", "-")
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("ffmpeg: %v\n%s", err, stderr.Bytes())
			}
			want := make([]int32, len(out)/4)
			for i := range want {
				want[i] = int32(binary.LittleEndian.Uint32(out[4*i:]))
			}
			// Outside the holes it is ffmpeg's decode, which joins across one.
			if len(want) != len(joined) {
				t.Fatalf("ffmpeg decoded %d samples, want the clean %d less %d lost frames",
					len(want)/ch, ref.N, len(holes))
			}
			if i := testutil.DiffI32(joined, want); i >= 0 {
				t.Fatalf("outside the holes, differs from ffmpeg's decode at sample %d", i/ch)
			}
		})
	}
}
