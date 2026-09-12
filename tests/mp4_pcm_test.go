package waxflow_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// PCM in MP4/MOV against ffmpeg. The fixtures are container/mp4's, whose
// provenance comment holds the command lines; this is where they can be scored
// against a third party, and the gate is EQUALITY: uncompressed audio has no
// rounding for two readers to disagree by, so every sample must match and any
// difference at all is a width, byte order, or offset defect.
//
// The two directions are compared differently only because the pipeline
// carries them differently. Integer tracks decode to int32 words that ffmpeg's
// s32le output left-justifies the same way testutil.Interleave does, so they
// compare as integers; float tracks compare as float32, including the 64-bit
// source, where both sides narrow the same way.
var pcmInMP4Fixtures = []struct {
	name  string
	float bool
}{
	{"pcm-in24.mov", false},
	{"pcm-in24-chunks.mov", false},
	{"pcm-in24le.mov", false},
	{"pcm-sowt.mov", false},
	{"pcm-twos.mov", false},
	{"pcm-in32.mov", false},
	{"pcm-raw.mov", false},
	{"pcm-fl32.mov", true},
	{"pcm-fl64.mov", true},
	{"pcm-lpcm.mov", false},
	{"pcm-ipcm.mp4", false},
	{"pcm-ipcm-96k.mp4", false},
	{"pcm-fpcm.mp4", true},
}

func pcmInMP4(t *testing.T, name string) container.Source {
	t.Helper()
	return mp3InMP4(t, name)
}

// TestPCMInMP4MatchesFFmpeg is the differential. It also pins the sample
// count, which is the half the boxes decide: a PCM track's length is its
// stsz count and nothing else, so a chunk index that dropped or repeated a
// run shows up here before any sample is compared.
func TestPCMInMP4MatchesFFmpeg(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	for _, f := range pcmInMP4Fixtures {
		t.Run(f.name, func(t *testing.T) {
			path := repoPath("container", "mp4", "testdata", f.name)
			got := decodeAll(t, pcmInMP4(t, f.name), "mp4")
			defer audio.Put(got)
			if f.float {
				ref := testutil.FFmpegDecodeF32(t, path)
				if len(ref) != got.N*got.Fmt.Channels {
					t.Fatalf("we decode %d samples, ffmpeg %d", got.N*got.Fmt.Channels, len(ref))
				}
				if d := testutil.CompareF32(testutil.InterleaveF(got), ref); d.MaxAbs != 0 {
					t.Errorf("float samples differ from ffmpeg: %v", d)
				}
				return
			}
			ref := testutil.FFmpegDecodeS32(t, path)
			if len(ref) != got.N*got.Fmt.Channels {
				t.Fatalf("we decode %d samples, ffmpeg %d", got.N*got.Fmt.Channels, len(ref))
			}
			if i := testutil.DiffI32(testutil.Interleave(got), ref); i != -1 {
				t.Errorf("sample %d differs from ffmpeg", i)
			}
		})
	}
}

// TestPCMInMP4Seeks holds every fixture to a sample-exact seek whose output is
// bit-identical to the linear decode. Uncompressed audio carries no
// inter-frame state, so there is nothing for a pre-roll to converge and
// nothing a landing can approximate: anything but equality is a chunk index
// addressing the wrong frame.
func TestPCMInMP4Seeks(t *testing.T) {
	for _, f := range pcmInMP4Fixtures {
		t.Run(f.name, func(t *testing.T) {
			ref := decodeAll(t, pcmInMP4(t, f.name), "mp4")
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(pcmInMP4(t, f.name), "mp4")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			if got := med.Info().Default().Codec; got != codec.PCM {
				t.Fatalf("codec = %q, want %q", got, codec.PCM)
			}
			seekMatchesReference(t, med, ref, 24, 1)
		})
	}
}

// TestPCMInMP4ByteOrdersAgree is what the in24 pair is for: one sine stored
// big-endian and the other little-endian, so the samples they decode to must
// be identical. A byte order read backwards is still plausible audio at the
// right length, which is why neither the count nor the ffmpeg differential
// alone would catch a fourcc wired to the wrong end.
//
// The prefix, since the big-endian fixture is half a second and its
// little-endian sibling a tenth: the generator is deterministic, so the
// shorter file is the longer one's opening.
func TestPCMInMP4ByteOrdersAgree(t *testing.T) {
	be := decodeAll(t, pcmInMP4(t, "pcm-in24.mov"), "mp4")
	defer audio.Put(be)
	le := decodeAll(t, pcmInMP4(t, "pcm-in24le.mov"), "mp4")
	defer audio.Put(le)
	if le.N == 0 || be.N < le.N {
		t.Fatalf("the big-endian fixture decoded %d samples and the little-endian %d", be.N, le.N)
	}
	if be.Fmt != le.Fmt {
		t.Fatalf("formats differ: %v against %v", be.Fmt, le.Fmt)
	}
	for c := 0; c < le.Fmt.Channels; c++ {
		if i := testutil.DiffI32(be.ChanI(c)[:le.N], le.ChanI(c)[:le.N]); i != -1 {
			t.Fatalf("channel %d sample %d: big-endian %d, little-endian %d",
				c, i, be.ChanI(c)[i], le.ChanI(c)[i])
		}
	}
}

// TestPCMInMP4Transcodes runs the whole pipeline over one, since a track the
// demuxer describes correctly and the engine cannot open is not wired. WAV is
// the output that keeps the samples, so the round trip is checkable: the
// written file must read back as the same 24-bit audio at the same length.
func TestPCMInMP4Transcodes(t *testing.T) {
	e := waxflow.New()
	ws := &memWS{}
	res, err := e.Transcode(t.Context(), pcmInMP4(t, "pcm-in24.mov"), "", ws,
		waxflow.TranscodeOptions{Format: "wav", BitDepth: 24})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 4000 {
		t.Fatalf("transcode produced %d samples, want the source's 4000", res.Samples)
	}
	// The hint is empty above on purpose: a .mov has to sniff as MP4 for a
	// caller who only has the bytes.
	info, err := e.Probe(pcmInMP4(t, "pcm-in24.mov"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "mp4" {
		t.Errorf("container = %q, want mp4", info.Container)
	}

	src := decodeAll(t, pcmInMP4(t, "pcm-in24.mov"), "mp4")
	defer audio.Put(src)
	out := decodeAll(t, container.BytesSource(ws.Buf), "wav")
	defer audio.Put(out)
	if out.Fmt != src.Fmt || out.N != src.N {
		t.Fatalf("the WAV reads back as %v / %d samples, the source is %v / %d",
			out.Fmt, out.N, src.Fmt, src.N)
	}
	for c := 0; c < src.Fmt.Channels; c++ {
		if i := testutil.DiffI32(out.ChanI(c)[:out.N], src.ChanI(c)[:src.N]); i != -1 {
			t.Fatalf("channel %d sample %d survived the round trip as %d, was %d",
				c, i, out.ChanI(c)[i], src.ChanI(c)[i])
		}
	}
}

// TestPCMInMP4EndaOverridesTheFourcc is the byte-order rule measured against
// the reference reader rather than asserted from the spec.
//
// 'twos' names big-endian storage and an 'enda' box overrides it, which is not
// a reading anyone has to take on trust: ffmpeg answers pcm_s16le for this
// exact entry and pcm_s16be for the same entry with enda 0. Reading the box as
// inapplicable because the fourcc already named an order turns a 440 Hz sine
// into full-scale noise, at the right length, with no warning anywhere.
//
// The file is built here rather than committed: it is the committed
// pcm-twos.mov with a wave{frma,enda} spliced into its sample entry, and
// building it keeps the recipe next to the assertion.
func TestPCMInMP4EndaOverridesTheFourcc(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	raw, err := os.ReadFile(repoPath("container", "mp4", "testdata", "pcm-twos.mov"))
	if err != nil {
		t.Fatal(err)
	}
	swapped := spliceEnda(t, raw, 1)
	path := filepath.Join(t.TempDir(), "twos-enda.mov")
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatal(err)
	}

	got := decodeAll(t, container.BytesSource(swapped), "mp4")
	defer audio.Put(got)
	ref := testutil.FFmpegDecodeS32(t, path)
	if len(ref) != got.N*got.Fmt.Channels {
		t.Fatalf("we decode %d samples, ffmpeg %d", got.N*got.Fmt.Channels, len(ref))
	}
	if i := testutil.DiffI32(testutil.Interleave(got), ref); i != -1 {
		t.Fatalf("sample %d differs from ffmpeg: the enda box was not applied", i)
	}
	// And it is the byte swap of the unmodified file, not a coincidence: the
	// same bytes read the other way round are different audio.
	plain := decodeAll(t, pcmInMP4(t, "pcm-twos.mov"), "mp4")
	defer audio.Put(plain)
	if i := testutil.DiffI32(got.ChanI(0)[:got.N], plain.ChanI(0)[:plain.N]); i == -1 {
		t.Error("the two byte orders decoded identically; the fixture is not the trap it claims to be")
	}
}

// spliceEnda inserts a wave{frma,enda} pair into the first sample entry of a
// QuickTime movie, growing every box that contains it. The chunk offsets need
// no fixing because ffmpeg writes mdat before moov in these files, so the
// samples do not move.
func spliceEnda(t *testing.T, raw []byte, little uint16) []byte {
	t.Helper()
	box := func(typ string, payload []byte) []byte {
		b := make([]byte, 8, 8+len(payload))
		binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
		copy(b[4:], typ)
		return append(b, payload...)
	}
	wave := box("wave", append(append(
		box("frma", []byte("twos")),
		box("enda", []byte{byte(little >> 8), byte(little)})...),
		box("\x00\x00\x00\x00", nil)...))

	at := bytes.Index(raw, []byte("twos")) - 4
	if at < 0 {
		t.Fatal("no twos entry in the fixture")
	}
	size := int(binary.BigEndian.Uint32(raw[at:]))
	out := make([]byte, 0, len(raw)+len(wave))
	out = append(out, raw[:at+size]...)
	out = append(out, wave...)
	out = append(out, raw[at+size:]...)
	// The entry and every box enclosing it grew.
	for _, typ := range []string{"twos", "stsd", "stbl", "minf", "mdia", "trak", "moov"} {
		i := bytes.Index(out, []byte(typ)) - 4
		binary.BigEndian.PutUint32(out[i:], binary.BigEndian.Uint32(out[i:])+uint32(len(wave)))
	}
	if at < bytes.Index(raw, []byte("mdat")) {
		t.Fatal("mdat follows moov in this fixture; the chunk offsets would need fixing")
	}
	return out
}
