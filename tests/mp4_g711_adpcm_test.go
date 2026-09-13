package waxflow_test

import (
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/internal/testutil"
)

// G.711 and QuickTime ima4 in a .mov, against ffmpeg. The fixtures are
// container/mp4's, whose provenance comment holds the command lines; this is
// where they are scored against a third party, and the gate is EQUALITY:
// every one of these codecs is a fixed integer computation, so a difference
// of any size is a defect rather than a rounding disagreement.
//
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_mulaw -f mov g711-ulaw.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_alaw -f mov g711-alaw.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=1" \
//	    -ac 2 -c:a adpcm_ima_qt -f mov ima4.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=1" \
//	    -ac 2 -c:a adpcm_ima_qt -movflags empty_moov -frag_duration 500000 \
//	    -f mov ima4-frag.mov
//
// (ffmpeg 8.0.1, Ubuntu.) The ima4 file is a full second because it is the
// one whose seek cost is linear in the target: a shorter file would not tell
// a decode-from-the-start apart from a decode-from-the-block.
var mp4CodecFixtures = []struct {
	name  string
	codec codec.ID
	// tailPadded marks the track whose last block decodes past the length the
	// sample table times, so ffmpeg emits more samples than we do.
	tailPadded bool
}{
	{"g711-ulaw.mov", codec.MuLaw, false},
	{"g711-alaw.mov", codec.ALaw, false},
	{"ima4.mov", codec.IMAADPCM, true},
}

// TestCompressedInMP4MatchesFFmpeg is the differential. It pins the sample
// count too, which is the half the boxes decide: a length read off the wrong
// table shows up here before any sample is compared.
func TestCompressedInMP4MatchesFFmpeg(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	for _, f := range mp4CodecFixtures {
		t.Run(f.name, func(t *testing.T) {
			path := repoPath("container", "mp4", "testdata", f.name)
			got := decodeAll(t, pcmInMP4(t, f.name), "mp4")
			defer audio.Put(got)
			ours := testutil.Interleave(got)
			ref := testutil.FFmpegDecodeS32(t, path)
			switch excess := len(ref) - len(ours); {
			case !f.tailPadded && excess != 0:
				t.Fatalf("we decode %d samples, ffmpeg %d", len(ours), len(ref))
			case f.tailPadded && excess <= 0:
				t.Fatalf("we decode %d samples and ffmpeg %d; a tail-padded track must give ffmpeg more",
					len(ours), len(ref))
			case f.tailPadded:
				// Under one block, which is what "the timeline ends inside the
				// last block" means. 64 frames is ima4's fixed geometry.
				if block := 64 * got.Fmt.Channels; excess >= block {
					t.Fatalf("ffmpeg decoded %d samples past our %d, a whole block of %d or more",
						excess, len(ours), block)
				}
				ref = ref[:len(ours)]
			}
			if i := testutil.DiffI32(ours, ref); i != -1 {
				t.Errorf("sample %d differs from ffmpeg", i)
			}
		})
	}
}

// TestCompressedInMP4Seeks holds every fixture to a seek whose output is
// bit-identical to its own linear decode.
//
// For ima4 that is the whole point rather than a formality. Its decoder
// carries a predictor across blocks and a block header restates only the top
// nine bits of it, so a decode begun at a block sits up to 127 LSB from the
// linear decode and never rejoins; the demuxer therefore lands at the file's
// start and lets the engine's pre-roll walk up to the target. Comparing
// against ffmpeg's own `-ss` output would compare against a decode that
// starts fresh at a block, which is the answer this deliberately does not
// produce.
func TestCompressedInMP4Seeks(t *testing.T) {
	for _, f := range mp4CodecFixtures {
		t.Run(f.name, func(t *testing.T) {
			ref := decodeAll(t, pcmInMP4(t, f.name), "mp4")
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(pcmInMP4(t, f.name), "mp4")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			if got := med.Info().Default().Codec; got != f.codec {
				t.Fatalf("codec = %q, want %q", got, f.codec)
			}
			seekMatchesReference(t, med, ref, 24, 1)
		})
	}
}

// TestCompressedInMP4Probe pins what probe says about these tracks: the
// codec, the decoded depth, and the source depth beside it, which is the
// number ffprobe reports and the only place the 8- and 4-bit storage is
// visible.
func TestCompressedInMP4Probe(t *testing.T) {
	for _, tt := range []struct {
		name     string
		codec    codec.ID
		rate     int
		channels int
		samples  int64
		srcDepth int
	}{
		{"g711-ulaw.mov", codec.MuLaw, 8000, 1, 800, 8},
		{"g711-alaw.mov", codec.ALaw, 8000, 1, 800, 8},
		{"ima4.mov", codec.IMAADPCM, 44100, 2, 44100, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info, err := waxflow.New().Probe(pcmInMP4(t, tt.name), "mp4", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings on a clean fixture: %v", info.Warnings)
			}
			d := info.Default()
			if d.Codec != tt.codec {
				t.Errorf("codec = %q, want %q", d.Codec, tt.codec)
			}
			if d.Fmt.Rate != tt.rate || d.Fmt.Channels != tt.channels {
				t.Errorf("format = %v, want %d Hz %d ch", d.Fmt, tt.rate, tt.channels)
			}
			if d.Fmt.Type != audio.Int || d.Fmt.BitDepth != 16 {
				t.Errorf("decoded format = %v%d, want int16", d.Fmt.Type, d.Fmt.BitDepth)
			}
			if d.Samples != tt.samples {
				t.Errorf("samples = %d, want %d", d.Samples, tt.samples)
			}
			if d.SourceBitDepth != tt.srcDepth {
				t.Errorf("source bit depth = %d, want %d", d.SourceBitDepth, tt.srcDepth)
			}
			ref := testutil.FFprobeFile(t, repoPath("container", "mp4", "testdata", tt.name))
			if ref.BitsPerSample != 0 && ref.BitsPerSample != tt.srcDepth {
				t.Errorf("ffprobe says %d bits per sample, we report %d", ref.BitsPerSample, tt.srcDepth)
			}
		})
	}
}

// TestFragmentedIMA4MatchesFFmpeg is the fragmented half, and it exists
// because the rule that excludes byte-linear audio from a fragmented movie
// used to exclude these too.
//
// That rule is PCM's: a trun for uncompressed audio carries one entry per
// FRAME, tens of thousands a second, and nothing writes one. An ADPCM sample
// is a block of 64 frames, so its trun is an ordinary size and ffmpeg writes
// exactly this file for `-movflags empty_moov`. The movie has no sample table
// to state a length, so the comparison is against ffmpeg's whole decode.
//
// The seek is the other half: this codec carries its predictor across blocks,
// so the landing has to be the stream's head here as it is in a progressive
// movie, and the fragmented walk has its own restart to reach it.
func TestFragmentedIMA4MatchesFFmpeg(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	const name = "ima4-frag.mov"
	// decodeAllDynamic, not decodeAll: a fragmented movie with no edit-list
	// segment duration states no length, which is the normal shape and the
	// reason this fixture cannot be sized up front.
	got, err := decodeAllDynamic(t, pcmInMP4(t, name), "mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(got)
	ref := testutil.FFmpegDecodeS32(t, repoPath("container", "mp4", "testdata", name))
	ours := testutil.Interleave(got)
	if len(ours) != len(ref) {
		t.Fatalf("we decode %d samples, ffmpeg %d", len(ours), len(ref))
	}
	if i := testutil.DiffI32(ours, ref); i != -1 {
		t.Errorf("sample %d differs from ffmpeg", i)
	}

	med, err := waxflow.New().OpenStream(pcmInMP4(t, name), "mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if c := med.Info().Default().Codec; c != codec.IMAADPCM {
		t.Fatalf("codec = %q, want %q", c, codec.IMAADPCM)
	}
	seekMatchesReference(t, med, got, 16, 5)
}
