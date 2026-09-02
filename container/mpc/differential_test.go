package mpc_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The ffprobe differentials: ffmpeg's two Musepack demuxers are the
// independent readers of the same framing. Its SV8 packets are our blocks,
// its SV7 packets our frames, and its stream duration is the untrimmed frame
// count, so the comparisons are shaped to what it reports.

func differentialFixtures(t testing.TB) []string {
	dir := repoPath("container", "mpc", "testdata")
	names, err := filepath.Glob(filepath.Join(dir, "*.mpc"))
	if err != nil {
		t.Fatal(err)
	}
	codecDir := repoPath("codec", "musepack", "testdata")
	more, err := filepath.Glob(filepath.Join(codecDir, "*.mpc"))
	if err != nil {
		t.Fatal(err)
	}
	return append(names, more...)
}

// TestDemuxMatchesFFprobePackets compares packet counts and, for SV8, payload
// sizes against ffprobe. SV7 gets a one-packet allowance for the decay frame
// ffmpeg never emits, and its sizes are not compared: ffmpeg's SV7 packets
// carry whole words where ours carry the frame's bits.
func TestDemuxMatchesFFprobePackets(t *testing.T) {
	testutil.FFprobe(t)
	for _, path := range differentialFixtures(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := open(t, raw, true)
			cfg, _ := musepack.ParseConfig(d.Tracks()[0].CodecConfig)
			ours := readAll(t, d)
			ref := testutil.FFprobePackets(t, path)
			if cfg.StreamVersion == 8 {
				if len(ours) != len(ref) {
					t.Fatalf("%d blocks, ffprobe sees %d packets", len(ours), len(ref))
				}
				for i := range ours {
					_, _, payload, err := musepack.ParsePacketHeader(ours[i].Data, cfg)
					if err != nil {
						t.Fatal(err)
					}
					if len(payload) != ref[i].Size {
						t.Errorf("block %d is %d bytes, ffprobe's packet %d", i, len(payload), ref[i].Size)
					}
				}
				return
			}
			if len(ours) != len(ref) && len(ours) != len(ref)+1 {
				t.Fatalf("%d frames, ffprobe sees %d packets", len(ours), len(ref))
			}
			// ffmpeg's SV7 time base is one frame.
			for i := range ref {
				if ours[i].PTS/musepack.FrameLength != ref[i].PTS {
					t.Errorf("frame %d PTS %d, ffprobe frame %d", i, ours[i].PTS, ref[i].PTS)
					break
				}
			}
		})
	}
}

// TestTrackLengthMatchesFFprobe compares the rate and channel count exactly
// and the length in ffprobe's own units. ffmpeg's Musepack time base is one
// frame (SV7) or one block (SV8), untrimmed: its SV7 duration is the header's
// frame count (it never reads a decay frame), and its SV8 duration_ts is the
// last block's index.
func TestTrackLengthMatchesFFprobe(t *testing.T) {
	testutil.FFprobe(t)
	for _, path := range differentialFixtures(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := open(t, raw, true)
			track := d.Tracks()[0]
			ref := testutil.FFprobeFile(t, path)
			if track.Fmt.Rate != ref.SampleRate || track.Fmt.Channels != ref.Channels {
				t.Errorf("%d Hz %d ch, ffprobe says %d Hz %d ch", track.Fmt.Rate, track.Fmt.Channels, ref.SampleRate, ref.Channels)
			}
			if ref.Samples < 0 {
				t.Skip("ffprobe reports no duration")
			}
			cfg, _ := musepack.ParseConfig(track.CodecConfig)
			pkts := readAll(t, d)
			total := int64(len(pkts))
			switch {
			case cfg.StreamVersion == 8 && ref.Samples != total-1:
				t.Errorf("%d blocks, ffprobe's last timestamp is block %d", total, ref.Samples)
			case cfg.StreamVersion == 7 && ref.Samples != total && ref.Samples != total-1:
				t.Errorf("%d frames (a decay frame included when there is one), ffprobe's duration is %d frames", total, ref.Samples)
			}
		})
	}
}
