package waxflow_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
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
	// permuted marks a file whose channels the decoder reorders: its output
	// is not in the file's order and ffmpeg's decode is, so the two cannot be
	// compared sample for sample. TestPCMInMP4PermutedOrderMatchesTheCanonicalFile
	// scores such a file against its canonical twin instead.
	permuted bool
	// oracle is the ffmpeg release the differential needs, zero for any: 6.1
	// refuses to open a chnl box whose positions are the 110 degree pair
	// (measured on a build of 6.1.1), and 7.0 opens it.
	oracle [2]int
}{
	{name: "pcm-in24.mov"},
	{name: "pcm-in24-chunks.mov"},
	{name: "pcm-in24le.mov"},
	{name: "pcm-sowt.mov"},
	{name: "pcm-twos.mov"},
	{name: "pcm-in32.mov"},
	{name: "pcm-raw.mov"},
	{name: "pcm-fl32.mov", float: true},
	{name: "pcm-fl64.mov", float: true},
	{name: "pcm-lpcm.mov"},
	{name: "pcm-ipcm.mp4"},
	{name: "pcm-ipcm-96k.mp4"},
	{name: "pcm-fpcm.mp4", float: true},
	// The layout fixtures; see pcmLayoutFixtures for what each carries.
	{name: "pcm-51.mov"},
	{name: "pcm-51side.mov"},
	{name: "pcm-40.mov"},
	{name: "pcm-71.mov"},
	{name: "pcm-51-perm.mov", permuted: true},
	{name: "pcm-71-perm.mov", permuted: true},
	{name: "pcm-51.mp4"},
	{name: "pcm-51side.mp4", oracle: [2]int{7, 0}},
	{name: "pcm-71.mp4", oracle: [2]int{7, 0}},
	{name: "pcm-51-perm.mp4", permuted: true},
}

// The tone each speaker position carries in the layout fixtures, the
// spacing tests/aac_multichannel_test.go uses with 60 Hz on the LFE. The tone
// is keyed to the POSITION, not to the channel index, so a permuted file
// carries the same tone in the same speaker as its canonical twin and the
// identity assertion is one assertion for both.
var positionTones = map[audio.ChannelMask]float64{
	audio.FrontLeft: 300, audio.FrontRight: 550, audio.FrontCenter: 800, audio.LowFrequency: 60,
	audio.BackLeft: 1050, audio.BackRight: 1300, audio.SideLeft: 1550, audio.SideRight: 1800,
	audio.BackCenter: 2050,
}

// pcmLayoutFixtures is the layout each fixture must report and the tone each
// of OUR output channels must carry, in mask-bit order. Where a row's tones
// are not the positions' own, the file's Ls Rs pair was generated as
// ffmpeg's side pair and lands on WAVE's back pair here: that is decision 7,
// a lone 110 degree pair (a named tag's, or chnl positions 4 and 5) is the
// back pair in this tree and 5.1(side) to ffmpeg.
var pcmLayoutFixtures = []struct {
	name   string
	layout audio.ChannelMask
	tones  []float64
	// lonePair marks the two rows above, which the ffprobe agreement folds.
	lonePair bool
	// probe is the ffmpeg release whose ffprobe reads the box as 8.0.1 does,
	// zero for any. Measured on builds of 6.1.1, 7.0.2 and 7.1.1: a chnl
	// 110 degree pair is refused by 6.1, named UNK by 7.0 and placed by 7.1;
	// every chan shape and the other chnl positions are read by all three.
	probe [2]int
}{
	{name: "pcm-51.mov", layout: fiveOne, tones: []float64{300, 550, 800, 60, 1050, 1300}},
	{name: "pcm-51side.mov", layout: fiveOne, tones: []float64{300, 550, 800, 60, 1550, 1800}, lonePair: true},
	{name: "pcm-40.mov", layout: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackCenter,
		tones: []float64{300, 550, 800, 2050}},
	{name: "pcm-71.mov", layout: sevenOne, tones: []float64{300, 550, 800, 60, 1050, 1300, 1550, 1800}},
	{name: "pcm-51-perm.mov", layout: fiveOne, tones: []float64{300, 550, 800, 60, 1050, 1300}},
	{name: "pcm-71-perm.mov", layout: sevenOne, tones: []float64{300, 550, 800, 60, 1050, 1300, 1550, 1800}},
	{name: "pcm-51.mp4", layout: fiveOne, tones: []float64{300, 550, 800, 60, 1050, 1300}},
	{name: "pcm-51side.mp4", layout: fiveOne, tones: []float64{300, 550, 800, 60, 1550, 1800}, lonePair: true, probe: [2]int{7, 1}},
	{name: "pcm-71.mp4", layout: sevenOne, tones: []float64{300, 550, 800, 60, 1050, 1300, 1550, 1800}, probe: [2]int{7, 1}},
	{name: "pcm-51-perm.mp4", layout: fiveOne, tones: []float64{300, 550, 800, 60, 1050, 1300}},
}

const (
	fiveOne  = audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.LowFrequency | audio.BackLeft | audio.BackRight
	sevenOne = fiveOne | audio.SideLeft | audio.SideRight
)

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
		if f.permuted {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			if f.oracle != [2]int{} && !testutil.FFmpegAtLeast(t, f.oracle[0], f.oracle[1]) {
				t.Skipf("ffmpeg before %d.%d cannot open this file's layout box", f.oracle[0], f.oracle[1])
			}
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

// TestPCMInMP4ChannelIdentity is the layout gate the fixtures were built for,
// and it needs no oracle: every speaker position carries its own tone, so
// the channel this tree labels with position P must carry P's tone above
// every other tone in the file. A wrong mask, a wrong order, or an order the
// decoder failed to apply all fail here, at full length, with every channel
// present. The well-formed files also reach neither a Note nor a warning:
// a permuted file plays in the pipeline's order with nothing left to say.
func TestPCMInMP4ChannelIdentity(t *testing.T) {
	for _, f := range pcmLayoutFixtures {
		t.Run(f.name, func(t *testing.T) {
			info, err := waxflow.New().Probe(pcmInMP4(t, f.name), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Notes) != 0 || len(info.Warnings) != 0 {
				t.Errorf("notes %v, warnings %v; want none for a layout the file states", info.Notes, info.Warnings)
			}
			buf := decodeAll(t, pcmInMP4(t, f.name), "mp4")
			defer audio.Put(buf)
			if buf.Fmt.Layout != f.layout {
				t.Fatalf("layout = %v, want %v", buf.Fmt.Layout, f.layout)
			}
			if buf.Fmt.Channels != len(f.tones) {
				t.Fatalf("decoded %d channels, want %d", buf.Fmt.Channels, len(f.tones))
			}
			for c, tone := range f.tones {
				ch := chanFloat(buf, c)
				own := goertzel(ch, buf.Fmt.Rate, tone)
				if own < 0.02 {
					t.Errorf("channel %d has only %.4f at its own %g Hz, want a real tone", c, own, tone)
					continue
				}
				for _, other := range f.tones {
					if other == tone {
						continue
					}
					if got := goertzel(ch, buf.Fmt.Rate, other); got >= own {
						t.Errorf("channel %d carries %g Hz at %.4f, above its own %g Hz at %.4f: the layout is wrong",
							c, other, got, tone, own)
					}
				}
			}
		})
	}
}

// TestPCMInMP4PermutedOrderMatchesTheCanonicalFile is the reorder scored
// against an independent writer: each permuted file is its canonical twin
// through ffmpeg's channelmap filter, the same samples in another wire
// order, so the two must decode sample for sample equal once the order is
// applied. The channel identity test says each channel holds the right
// tone; this says it holds the right samples.
func TestPCMInMP4PermutedOrderMatchesTheCanonicalFile(t *testing.T) {
	for _, tc := range []struct{ permuted, canonical string }{
		{"pcm-51-perm.mov", "pcm-51.mov"},
		{"pcm-51-perm.mp4", "pcm-51.mov"},
		{"pcm-71-perm.mov", "pcm-71.mov"},
	} {
		t.Run(tc.permuted, func(t *testing.T) {
			got := decodeAll(t, pcmInMP4(t, tc.permuted), "mp4")
			defer audio.Put(got)
			want := decodeAll(t, pcmInMP4(t, tc.canonical), "mp4")
			defer audio.Put(want)
			if got.Fmt != want.Fmt || got.N != want.N {
				t.Fatalf("permuted decodes as %v / %d samples, canonical as %v / %d",
					got.Fmt, got.N, want.Fmt, want.N)
			}
			for c := 0; c < got.Fmt.Channels; c++ {
				if i := testutil.DiffI32(got.ChanI(c)[:got.N], want.ChanI(c)[:want.N]); i != -1 {
					t.Errorf("channel %d sample %d: permuted %d, canonical %d",
						c, i, got.ChanI(c)[i], want.ChanI(c)[i])
				}
			}
		})
	}
}

// TestPCMInMP4LayoutAgreesWithFFprobe compares the layout this tree reports
// against ffprobe's channel_layout, as masks: ffprobe names a custom order
// by the file's order and this tree by ascending mask bit, and the two are
// the same speakers.
//
// One divergence is folded rather than adopted, and only on the rows that
// carry it. ffmpeg reads a lone Ls Rs pair (a named tag such as MPEG_5_1_A,
// or chnl positions 4 and 5) as its side pair and reports 5.1(side); this
// tree reads it as the back pair, because every codec here lands 5.1 on
// the back-pair mask and so does the WAV mask container/riff writes.
// Apple's own header argues both ways (WAVE_5_1_B names the back-pair mask
// separately, which within that family makes the lone pair the side one),
// so this is a choice made for the pipeline, recorded in
// docs/quality-gates.md. Description labels 5 and 6 are not folded: ffprobe
// already reads those as the back pair, so pcm-51-perm.mov agrees as it is.
func TestPCMInMP4LayoutAgreesWithFFprobe(t *testing.T) {
	for _, f := range pcmLayoutFixtures {
		t.Run(f.name, func(t *testing.T) {
			if f.probe != [2]int{} && !testutil.FFmpegAtLeast(t, f.probe[0], f.probe[1]) {
				t.Skipf("ffprobe before %d.%d does not place this file's chnl positions", f.probe[0], f.probe[1])
			}
			info, err := waxflow.New().Probe(pcmInMP4(t, f.name), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			ours := info.Default().Fmt.Layout
			theirs := ffprobeLayout(t, testutil.FFprobeFile(t, repoPath("container", "mp4", "testdata", f.name)).ChannelLayout)
			if f.lonePair && theirs&(audio.BackLeft|audio.BackRight) == 0 {
				theirs = theirs&^(audio.SideLeft|audio.SideRight) | audio.BackLeft | audio.BackRight
			}
			if ours != theirs {
				t.Errorf("layout = %v, ffprobe says %v", ours, theirs)
			}
		})
	}
}

// ffprobeLayout reads ffprobe's channel_layout as a mask: a layout name from
// the small table the fixtures need, or the "N channels (A+B+...)" form
// ffmpeg 7 and later print for an order that has no name (6.1 prints the
// name of the mask instead, which reads the same way).
func ffprobeLayout(t *testing.T, s string) audio.ChannelMask {
	t.Helper()
	named := map[string]string{
		"4.0":       "FL+FR+FC+BC",
		"5.1":       "FL+FR+FC+LFE+BL+BR",
		"5.1(side)": "FL+FR+FC+LFE+SL+SR",
		"7.1":       "FL+FR+FC+LFE+BL+BR+SL+SR",
	}
	list, ok := named[s]
	if !ok {
		i := strings.Index(s, "(")
		if i < 0 || !strings.HasSuffix(s, ")") {
			t.Fatalf("ffprobe channel_layout %q is not one this test reads", s)
		}
		list = s[i+1 : len(s)-1]
	}
	var mask audio.ChannelMask
	for _, name := range strings.Split(list, "+") {
		bit := audio.ChannelMask(0)
		for i := 0; i < 32; i++ {
			if audio.ChannelMask(1<<i).String() == name {
				bit = 1 << i
			}
		}
		if bit == 0 {
			t.Fatalf("ffprobe channel_layout %q names %q, which is no WAVE position", s, name)
		}
		mask |= bit
	}
	return mask
}
