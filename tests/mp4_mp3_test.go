package waxflow_test

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// mp3InMP4 reads one of container/mp4's MP3 fixtures. They live there because
// the boxes are what they exercise; this file is where the two containers can
// be compared, since container/mp4 cannot import container/mpa and should not
// want to.
func mp3InMP4(t *testing.T, name string) container.Source {
	t.Helper()
	raw, err := os.ReadFile(repoPath("container", "mp4", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return container.BytesSource(raw)
}

// TestMP3InMP4EqualsTheElementaryStream is the differential that says the MP4
// path carries the same audio as the bare .mp3, and it is a decode of one
// bitstream through two demuxers rather than of two encodes: the fixtures are
// one libmp3lame run, remuxed.
//
// It compares the prefix, not the whole track, and the reason is the finding
// this stage rests on. The .mp3 carries a LAME tag with both trims, so
// container/mpa delivers exactly the 22050 samples that went in. ffmpeg's MP4
// muxer writes only the front trim into the edit list, so the MP4 delivers
// 22511: the same audio plus the encoder's tail. Nothing in the file says
// those 461 samples are padding (ffmpeg's own decode of the same file keeps
// them too), so the demuxer must not invent a trim, and the assertion is that
// the samples they do share are identical rather than that the lengths match.
func TestMP3InMP4EqualsTheElementaryStream(t *testing.T) {
	fromMP4 := decodeAll(t, mp3InMP4(t, "mp3.mp4"), "mp4")
	defer audio.Put(fromMP4)
	fromMP3 := decodeAll(t, mp3InMP4(t, "mp3.mp3"), "mp3")
	defer audio.Put(fromMP3)

	if fromMP3.N == 0 {
		t.Fatal("the elementary stream decoded nothing")
	}
	if fromMP4.N <= fromMP3.N {
		t.Fatalf("the MP4 delivered %d samples and the .mp3 %d; the MP4 has no tail trim and must be the longer",
			fromMP4.N, fromMP3.N)
	}
	if fromMP4.Fmt != fromMP3.Fmt {
		t.Fatalf("formats differ: %v against %v", fromMP4.Fmt, fromMP3.Fmt)
	}
	// Bit-exact, with no tolerance: it is the same decoder over the same
	// frames, so any difference is a framing or trim bug and not rounding.
	for c := 0; c < fromMP3.Fmt.Channels; c++ {
		a, b := fromMP4.ChanF(c), fromMP3.ChanF(c)
		for i := 0; i < fromMP3.N; i++ {
			if a[i] != b[i] {
				t.Fatalf("channel %d sample %d: MP4 %v, .mp3 %v", c, i, a[i], b[i])
			}
		}
	}
}

// TestMP3InMP4Seeks holds a seek to the property the backoff exists to
// provide: not that the landing is exact, but that the samples AT it are the
// ones a continuous decode delivers.
//
// The distinction is the whole point. format.Media pre-rolls from the
// demuxer's landing to the target, so a backoff that is too shallow does not
// move the landing at all and every landing stays exact; what it does is
// corrupt the audio there, because Layer III's filterbank carries overlap
// history and its bit reservoir lets a frame's main data begin up to 511
// bytes earlier in the stream. Measured on this fixture: with the backoff the
// decode is bit-identical from every landing, and with none the error reaches
// 0.06 RMS against a signal of 0.084, which is most of the audio.
//
// "With none" is the whole backoff, and the wording is deliberate: at this
// fixture's 32 kbit/s either half suffices on its own, since six preroll
// frames already carry more main data than the reservoir's 575 bytes. That
// stops being true as the bit rate falls (six frames at 8 kbit/s carry about
// 340), which no fixture here has, so the walk's own contribution is pinned
// structurally instead, by TestMP3SeekBacksOffForTheReservoir.
//
// The targets straddle frame boundaries (576 samples at 22.05 kHz) and include
// the region inside the backoff distance, where the walk clamps at the top of
// the stream.
func TestMP3InMP4Seeks(t *testing.T) {
	whole := decodeAll(t, mp3InMP4(t, "mp3.mp4"), "mp4")
	defer audio.Put(whole)
	med, err := waxflow.New().OpenStream(mp3InMP4(t, "mp3.mp4"), "mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	track := med.Info().Default()
	if track.Codec != codec.MP3 {
		t.Fatalf("codec = %q, want mp3", track.Codec)
	}
	if int64(whole.N) != track.Samples {
		t.Fatalf("the continuous decode delivered %d samples, the track declares %d", whole.N, track.Samples)
	}
	const compare = 4096
	buf := audio.Get(track.Fmt, compare)
	defer audio.Put(buf)
	for _, target := range []int64{0, 1, 575, 576, 577, 5000, 11520, track.Samples - 1} {
		landed, err := med.SeekSample(target)
		if err != nil {
			t.Fatalf("seek %d: %v", target, err)
		}
		if landed != target {
			t.Fatalf("seek %d landed at %d; the pre-roll is meant to make it exact", target, landed)
		}
		buf.N = 0
		for buf.N < compare {
			tmp := audio.Get(track.Fmt, compare-buf.N)
			err := med.ReadChunk(tmp)
			if err == nil {
				for c := 0; c < track.Fmt.Channels; c++ {
					copy(buf.ChanF(c)[buf.N:buf.N+tmp.N], tmp.ChanF(c)[:tmp.N])
				}
				buf.N += tmp.N
			}
			audio.Put(tmp)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("read after seek %d: %v", target, err)
				}
				break
			}
		}
		// Bit-exact, with no tolerance. A resumed Layer III decode has no
		// free-running state once the reservoir is satisfied and the
		// filterbank has settled, so anything but equality is the backoff
		// being too shallow rather than rounding.
		var energy float64
		for c := 0; c < track.Fmt.Channels; c++ {
			got, want := buf.ChanF(c), whole.ChanF(c)[target:]
			for i := 0; i < buf.N && i < len(want); i++ {
				if got[i] != want[i] {
					t.Fatalf("seek %d channel %d sample %d: %v, want the continuous decode's %v",
						target, c, i, got[i], want[i])
				}
				energy += float64(want[i]) * float64(want[i])
			}
		}
		// A region of silence would compare equal however wrong the decode
		// was, and the last target is deliberately one sample from the end.
		if target < track.Samples-compare && energy == 0 {
			t.Fatalf("seek %d compared %d samples of silence; the assertion above proves nothing there",
				target, buf.N)
		}
	}
}

// TestMP3InMP4Transcodes runs the whole pipeline over one, since a track the
// demuxer describes correctly and the engine cannot open is not wired.
func TestMP3InMP4Transcodes(t *testing.T) {
	e := waxflow.New()
	ws := &memWS{}
	res, err := e.Transcode(t.Context(), mp3InMP4(t, "mp3.mov"), "", ws,
		waxflow.TranscodeOptions{Format: "flac", BitDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples <= 0 || len(ws.Buf) == 0 {
		t.Fatalf("transcode produced %d samples in %d bytes", res.Samples, len(ws.Buf))
	}
	// The hint is empty above on purpose: a .mov has to sniff as MP4 for a
	// caller who only has the bytes.
	info, err := e.Probe(mp3InMP4(t, "mp3.mov"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "mp4" {
		t.Errorf("container = %q, want mp4", info.Container)
	}
}

// TestMP3RemuxKeepsItsTrims is the transmux rung over MP3, and it is a
// round-trip assertion because the trims are what a remux can silently ruin:
// the packets are copied, so the only thing the muxer decides is the metadata
// frame, and the metadata frame is the gapless.
//
// Both source containers are here because they arrive with the same head trim
// stated two ways (a LAME tag and an edit list) and different tails: the .mp3
// carries the encoder's 461-sample padding and the MP4 does not, since
// ffmpeg's muxer writes only the front trim. Each has to come back as itself.
//
// The bug this pins was real and shipped: the LAME extension's fields hold
// the encoder's share of the delay and every reader adds the fixed 529-sample
// decoder latency back, so writing a demuxer's already-summed trim grew the
// head by 529 per generation and dropped any tail shorter than that. The
// .mp3-to-.mp3 case had it before MP3 in MP4 existed.
func TestMP3RemuxKeepsItsTrims(t *testing.T) {
	e := waxflow.New()
	for _, tc := range []struct{ name, hint string }{
		{"mp3.mp3", "mp3"},
		{"mp3.mp4", "mp4"},
		{"mp3.mov", "mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := mp3InMP4(t, tc.name)
			in, err := e.Probe(src, tc.hint, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := in.Default()
			plan, err := e.PlanRemux(want, waxflow.TranscodeOptions{Format: "mp3"})
			if err != nil {
				t.Fatal(err)
			}
			if plan == nil {
				t.Fatal("the remux rung declined an MP3 source for format=mp3; the packets cross unchanged")
			}
			ws := &memWS{}
			if _, err := e.Remux(t.Context(), src, tc.hint, ws, waxflow.TranscodeOptions{Format: "mp3"}); err != nil {
				t.Fatal(err)
			}
			out, err := e.Probe(container.BytesSource(ws.Buf), "mp3", nil)
			if err != nil {
				t.Fatal(err)
			}
			got := out.Default()
			if got.Delay != want.Delay || got.Padding != want.Padding || got.Samples != want.Samples {
				t.Errorf("remux read back delay=%d padding=%d samples=%d, want the source's %d/%d/%d",
					got.Delay, got.Padding, got.Samples, want.Delay, want.Padding, want.Samples)
			}
		})
	}
}

// TestMP3InMP4Differential puts the MP4 arm against a third party rather than
// against our own MP3 reader, which is what the equality test above compares
// with. Both matter: that one says the two containers agree with each other,
// this one says they agree with ffmpeg about the same file.
//
// Bit-exactness is not the gate here and could not be: ffmpeg's Layer III
// decoder is its own float implementation. The count is exact, though, and it
// is the interesting half, since the count is what the boxes decide.
func TestMP3InMP4Differential(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	for _, name := range []string{"mp3.mp4", "mp3.mov"} {
		t.Run(name, func(t *testing.T) {
			path := repoPath("container", "mp4", "testdata", name)
			got := decodeAll(t, mp3InMP4(t, name), "mp4")
			defer audio.Put(got)
			ref := testutil.FFmpegDecodeF32(t, path)
			if len(ref) != got.N*got.Fmt.Channels {
				t.Fatalf("we decode %d samples, ffmpeg %d: the boxes are read differently",
					got.N*got.Fmt.Channels, len(ref))
			}
			rms, off := alignedRMS(testutil.InterleaveF(got), ref, got.Fmt.Channels)
			t.Logf("%s: rms=%g offset=%d", name, rms, off)
			// Measured 8.0e-08 on both cells (ffmpeg 8.0.1, Ubuntu), which is
			// two float Layer III decoders agreeing to about their own
			// rounding. The gate is 1e-5, two orders of headroom over that
			// and still far under the 1/8192 the AAC differential allows: a
			// box-reading defect moves this by orders of magnitude, not by
			// rounding, so a loose gate would pass one.
			if rms > 1e-5 {
				t.Errorf("RMS %g against ffmpeg exceeds the 1e-5 gate (offset %d)", rms, off)
			}
			if off != 0 {
				t.Errorf("aligned at offset %d, want 0: the head trim differs from ffmpeg's", off)
			}
		})
	}
}
