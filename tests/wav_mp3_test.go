package waxflow_test

import (
	"os"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The two wrappers this stream added, against the elementary stream they
// wrap. Both fixtures hold the same 41-frame libmp3lame encode the MP4
// fixtures carry, so what is compared is one bitstream read through several
// containers rather than several encodes agreeing.
//
// container/riff/testdata/mp3.wav is ffmpeg's own remux. The AIFF-C is built
// here because nothing writes one: ffmpeg's aiff muxer refuses MP3 and its
// demuxer refuses the result, so there is no third party to make it or to
// score it, and waxlabel is the second reader instead (oracletest).
const (
	wavMP3Delay   = 1105  // LAME's 576 encoder plus 529 decoder samples
	wavMP3Raw     = 23616 // 41 frames of 576, which is what fact declares
	wavMP3Trimmed = 22050 // what the .mp3's LAME tag trims that to
)

func mp3SourceBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(repoPath("container", "mp4", "testdata", "mp3.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// aifcMP3 wraps the WAV fixture's own frames in an AIFF-C '.mp3' file, so
// the two wrappers carry the same bytes by construction.
func aifcMP3(t *testing.T) []byte {
	t.Helper()
	return testutil.AIFCFrames(".mp3", 22050, 1, 0, 41, testutil.WAVDataChunk(t, wavMP3Bytes(t)))
}

func wavMP3Bytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(repoPath("container", "riff", "testdata", "mp3.wav"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func wavMP3(t *testing.T) container.Source {
	t.Helper()
	return container.BytesSource(wavMP3Bytes(t))
}

// TestMP3InWAVEqualsTheElementaryStream is the differential that says the WAV
// path carries the same audio as the bare .mp3, and it is a decode of one
// bitstream through two demuxers rather than of two encodes.
//
// The trims are what separates them and they are aligned rather than
// asserted equal, because the WAV genuinely does not have them: `-c copy`
// strips the Xing frame, so the only length in the file is the fact chunk's
// coded capacity and nothing states where inside the frames the audio begins.
// Decoded, the WAV's sample i+1105 is the .mp3's sample i, which is the same
// relationship ffmpeg's own two decodes have.
func TestMP3InWAVEqualsTheElementaryStream(t *testing.T) {
	fromWAV := decodeAll(t, wavMP3(t), "wav")
	defer audio.Put(fromWAV)
	fromMP3 := decodeAll(t, container.BytesSource(mp3SourceBytes(t)), "mp3")
	defer audio.Put(fromMP3)

	if fromWAV.N != wavMP3Raw || fromMP3.N != wavMP3Trimmed {
		t.Fatalf("decoded %d samples from the WAV and %d from the .mp3, want %d and %d",
			fromWAV.N, fromMP3.N, wavMP3Raw, wavMP3Trimmed)
	}
	if fromWAV.Fmt != fromMP3.Fmt {
		t.Fatalf("formats differ: %v against %v", fromWAV.Fmt, fromMP3.Fmt)
	}
	// Bit-exact, with no tolerance: it is the same decoder over the same
	// frames, so any difference is a framing or trim bug and not rounding.
	for c := range fromMP3.Fmt.Channels {
		a, b := fromWAV.ChanF(c)[wavMP3Delay:], fromMP3.ChanF(c)
		for i := range fromMP3.N {
			if a[i] != b[i] {
				t.Fatalf("channel %d sample %d: WAV %v, .mp3 %v", c, i, a[i], b[i])
			}
		}
	}
}

// TestMP3InAIFCEqualsTheElementaryStream is the same comparison for the
// AIFF-C wrapper, where the length is unknown rather than advisory: COMM
// states a number nothing defines the units of, so the decode runs to the end
// of the payload and carries the encoder's delay and tail with it.
func TestMP3InAIFCEqualsTheElementaryStream(t *testing.T) {
	fromAIFC, err := decodeAllDynamic(t, container.BytesSource(aifcMP3(t)), "aiff")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(fromAIFC)
	fromMP3 := decodeAll(t, container.BytesSource(mp3SourceBytes(t)), "mp3")
	defer audio.Put(fromMP3)

	if fromAIFC.N != wavMP3Raw {
		t.Fatalf("decoded %d samples from the AIFF-C, want the payload's %d", fromAIFC.N, wavMP3Raw)
	}
	for c := range fromMP3.Fmt.Channels {
		a, b := fromAIFC.ChanF(c)[wavMP3Delay:], fromMP3.ChanF(c)
		for i := range fromMP3.N {
			if a[i] != b[i] {
				t.Fatalf("channel %d sample %d: AIFF-C %v, .mp3 %v", c, i, a[i], b[i])
			}
		}
	}
}

// TestMP3InWAVDifferential puts the WAV arm against a third party rather than
// against our own MP3 reader. Both matter: the test above says the two
// containers agree with each other, this one says they agree with ffmpeg
// about the same file.
//
// Bit-exactness is not the gate and could not be: ffmpeg's Layer III decoder
// is its own float implementation. The count is exact, though, and it is the
// interesting half, since the count is what the chunks decide.
func TestMP3InWAVDifferential(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	path := repoPath("container", "riff", "testdata", "mp3.wav")
	got := decodeAll(t, wavMP3(t), "wav")
	defer audio.Put(got)
	ref := testutil.FFmpegDecodeF32(t, path)
	if len(ref) != got.N*got.Fmt.Channels {
		t.Fatalf("we decode %d samples, ffmpeg %d: the chunks are read differently",
			got.N*got.Fmt.Channels, len(ref))
	}
	rms, off := alignedRMS(testutil.InterleaveF(got), ref, got.Fmt.Channels)
	t.Logf("mp3.wav: rms=%g offset=%d", rms, off)
	// The gate the MP4 differential uses, for the same reason: two float
	// Layer III decoders agree to about their own rounding, and a
	// chunk-reading defect moves this by orders of magnitude.
	if rms > 1e-5 {
		t.Errorf("RMS %g against ffmpeg exceeds the 1e-5 gate (offset %d)", rms, off)
	}
	if off != 0 {
		t.Errorf("aligned at offset %d, want 0: the head trim differs from ffmpeg's", off)
	}
}

// TestMP3InWAVSeeks holds a seek to the property the reservoir backoff exists
// to provide: not that the landing is exact, but that the samples AT it are
// the ones a continuous decode delivers. A backoff that is too shallow does
// not move the landing at all, since format.Media pre-rolls to the target;
// what it does is corrupt the audio there.
func TestMP3InWAVSeeks(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  func(t *testing.T) container.Source
		hint string
	}{
		{"wav", wavMP3, "wav"},
		{"aifc", func(t *testing.T) container.Source { return container.BytesSource(aifcMP3(t)) }, "aiff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := decodeAllDynamic(t, tc.src(t), tc.hint)
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(tc.src(t), tc.hint)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			if got := med.Info().Default().Codec; got != codec.MP3 {
				t.Fatalf("codec = %q, want mp3", got)
			}
			seekMatchesReference(t, med, ref, 60, 11)
		})
	}
}
