package mp4

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/container"
)

// metaTone returns a short deterministic buffer for the metadata tests;
// the caller releases it with audio.Put.
func metaTone(n int) *audio.Buffer {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	src := audio.Get(f, n)
	src.N = n
	fillTone(src)
	return src
}

// muxALACMeta encodes src as ALAC through a muxer built with opts, so the
// metadata tests control MuxerOptions directly (muxALAC pins its own).
func muxALACMeta(t *testing.T, w io.Writer, src *audio.Buffer, opts *MuxerOptions) {
	t.Helper()
	enc, err := alac.NewEncoder(src.Fmt, nil)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	m := NewMuxer(w, opts)
	track := container.Track{
		Codec:       codec.ALAC,
		CodecConfig: enc.CodecConfig(),
		Fmt:         src.Fmt,
		Samples:     int64(src.N),
		Default:     true,
	}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	frame := audio.Get(src.Fmt, alac.FrameSize)
	defer audio.Put(frame)
	for off := 0; off < src.N; off += alac.FrameSize {
		n := min(alac.FrameSize, src.N-off)
		frame.N = n
		for c := 0; c < src.Fmt.Channels; c++ {
			copy(frame.ChanI(c), src.I[c*src.Stride+off:c*src.Stride+off+n])
		}
		if err := enc.Encode(frame, emit); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatalf("End: %v", err)
	}
}

// muxAACMeta encodes n samples of a deterministic tone as AAC through a
// muxer built with opts (the iTunSMPB path needs a codec with a nonzero
// delay), returning the encoder trailer.
func muxAACMeta(t *testing.T, w io.Writer, n int, declaredLen int64, opts *MuxerOptions) codec.Trailer {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	enc, err := aac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMuxer(w, opts)
	track := container.Track{Codec: codec.AACLC, CodecConfig: enc.CodecConfig(),
		Fmt: f, Samples: declaredLen, Delay: int64(enc.Delay()), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	buf := audio.Get(f, 1024)
	defer audio.Put(buf)
	for off := 0; off < n; off += 1024 {
		end := min(off+1024, n)
		buf.N = end - off
		for c := 0; c < 2; c++ {
			s := buf.ChanF(c)
			for i := 0; i < buf.N; i++ {
				ti := float64(off+i) / 44100
				s[i] = float32(0.3 * math.Sin(2*math.Pi*440*ti))
			}
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return trailer
}

// TestMuxTagsIlstAtoms checks the ilst text atoms land byte-exact: each
// mapped key becomes its iTunes atom wrapping a UTF-8 data atom, a
// repeated key joins with "; ", and TRACKNUMBER/TRACKTOTAL render as the
// trkn binary pair. The expected bytes come from the package's own
// builders, so the assertion is the whole atom, not substring luck.
func TestMuxTagsIlstAtoms(t *testing.T) {
	tags := []container.Tag{
		{Key: "TITLE", Value: "Title Value"},
		{Key: "ARTIST", Value: "Artist One"},
		{Key: "ARTIST", Value: "Artist Two"},
		{Key: "ALBUM", Value: "Album Value"},
		{Key: "TRACKNUMBER", Value: "3"},
		{Key: "TRACKTOTAL", Value: "12"},
		{Key: "RECORDINGDATE", Value: "2026-07-11"},
	}
	src := metaTone(alac.FrameSize + 50)
	defer audio.Put(src)
	var out bytes.Buffer
	muxALACMeta(t, &out, src, &MuxerOptions{Tags: tags})
	data := out.Bytes()

	cases := []struct {
		name string
		want []byte
	}{
		{"nam", itemAtom("\xa9nam", dataAtom(1, []byte("Title Value")))},
		{"ART", itemAtom("\xa9ART", dataAtom(1, []byte("Artist One; Artist Two")))},
		{"alb", itemAtom("\xa9alb", dataAtom(1, []byte("Album Value")))},
		{"day", itemAtom("\xa9day", dataAtom(1, []byte("2026-07-11")))},
		{"trkn", itemAtom("trkn", dataAtom(0, []byte{0, 0, 0, 3, 0, 12, 0, 0}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Contains(data, tc.want) {
				t.Errorf("output lacks the %s atom (% x)", tc.name, tc.want)
			}
		})
	}
}

// TestMuxChplChapters checks the muxed movie carries the exact chpl box
// the chapter list renders to.
func TestMuxChplChapters(t *testing.T) {
	chapters := []container.Chapter{
		{Start: 0, Title: "Intro"},
		{Start: 90 * time.Second, Title: "Middle"},
		{Start: 3 * time.Minute, Title: "Outro"},
	}
	src := metaTone(alac.FrameSize)
	defer audio.Put(src)
	var out bytes.Buffer
	muxALACMeta(t, &out, src, &MuxerOptions{Chapters: chapters})
	if !bytes.Contains(out.Bytes(), chplBox(chapters)) {
		t.Error("output lacks the exact chpl box")
	}
}

// TestChplBoxLimits pins the one-byte caps: the chapter count saturates
// at 255 entries and an overlong title truncates to at most 255 bytes on
// a rune boundary.
func TestChplBoxLimits(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		chapters := make([]container.Chapter, 300)
		for i := range chapters {
			chapters[i].Start = time.Duration(i) * time.Second
		}
		b := chplBox(chapters)
		// Layout: 8-byte box header, version/flags, the reserved word,
		// then the count byte at offset 16 and 9 bytes per untitled entry.
		if b[16] != 255 {
			t.Fatalf("chapter count byte %d, want 255", b[16])
		}
		if want := 17 + 255*9; len(b) != want {
			t.Fatalf("chpl of %d bytes, want %d (255 entries)", len(b), want)
		}
	})
	t.Run("title rune boundary", func(t *testing.T) {
		title := strings.Repeat("é", 130) // 260 bytes of 2-byte runes
		b := chplBox([]container.Chapter{{Title: title}})
		n := int(b[25]) // title length byte after the count and the 8-byte start
		if n != 254 {
			t.Fatalf("title length %d, want 254 (a 255-byte cut would split a rune)", n)
		}
		got := string(b[26 : 26+n])
		if !utf8.ValidString(got) {
			t.Fatal("truncated title is not valid UTF-8")
		}
		if got != strings.Repeat("é", 127) {
			t.Fatal("truncated title content wrong")
		}
	})
}

// TestIlstCovrFlags checks the covr data atom's type flag follows the
// art MIME: 14 for PNG, 13 for JPEG (the default for anything else).
func TestIlstCovrFlags(t *testing.T) {
	cases := []struct {
		name string
		mime string
		flag uint32
	}{
		{"png", "image/png", 14},
		{"jpeg", "image/jpeg", 13},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := &container.Picture{MIME: tc.mime, Data: []byte{1, 2, 3, 4}}
			got := ilstBox(nil, art, nil)
			want := itemAtom("covr", dataAtom(tc.flag, art.Data))
			if !bytes.Equal(got, want) {
				t.Errorf("covr bytes\n got % x\nwant % x", got, want)
			}
		})
	}
}

// TestMuxITunSMPB checks the gapless atom End completes on a seekable
// writer: field 2 carries the delay, field 3 the back-patched padding,
// and field 4 the back-patched 64-bit length, all fixed-width hex. A
// pure stream gets no atom at all, since its zeros could never be
// corrected.
func TestMuxITunSMPB(t *testing.T) {
	const n = 44100/2 + 37 // not a frame multiple, so the trailer pads
	t.Run("seekable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "smpb.m4a")
		fh, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		tr := muxAACMeta(t, fh, n, -1, nil)
		if err := fh.Close(); err != nil {
			t.Fatal(err)
		}
		if tr.Delay != aac.EncoderDelay || tr.Padding <= 0 {
			t.Fatalf("trailer delay %d padding %d; the test needs the AAC delay and positive padding", tr.Delay, tr.Padding)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		i := bytes.Index(data, []byte("iTunSMPB"))
		if i < 0 {
			t.Fatal("no iTunSMPB atom in the file")
		}
		da := data[i+len("iTunSMPB"):] // the data atom follows the freeform name
		if string(da[4:8]) != "data" {
			t.Fatalf("box after the freeform name is %q, want data", da[4:8])
		}
		payload := string(da[16:be32(da)])
		fields := strings.Fields(payload)
		if len(fields) != 12 {
			t.Fatalf("%d payload fields, want 12", len(fields))
		}
		if fields[0] != "00000000" {
			t.Errorf("field 1 is %q, want zeros", fields[0])
		}
		if want := fmt.Sprintf("%08X", uint32(tr.Delay)); fields[1] != want {
			t.Errorf("delay field %q, want %q", fields[1], want)
		}
		if want := fmt.Sprintf("%08X", uint32(tr.Padding)); fields[2] != want {
			t.Errorf("padding field %q, want %q", fields[2], want)
		}
		if want := fmt.Sprintf("%016X", uint64(tr.Samples)); fields[3] != want {
			t.Errorf("length field %q, want %q", fields[3], want)
		}
	})
	t.Run("streaming", func(t *testing.T) {
		var out bytes.Buffer
		muxAACMeta(t, &out, n, -1, nil)
		if bytes.Contains(out.Bytes(), []byte("iTunSMPB")) {
			t.Error("iTunSMPB written on a pure stream (its zeros could never be patched)")
		}
	})
}

// TestPatchFreeform covers the in-place ReplayGain patch: a same-width
// value swaps cleanly, while a width mismatch and an absent key both
// error without touching the file.
func TestPatchFreeform(t *testing.T) {
	const placeholder = "+00.00 dB"
	src := metaTone(alac.FrameSize)
	defer audio.Put(src)
	path := filepath.Join(t.TempDir(), "rg.m4a")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	muxALACMeta(t, fh, src, &MuxerOptions{Tags: []container.Tag{
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: placeholder},
	}})
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := PatchFreeform(f, "REPLAYGAIN_TRACK_GAIN", placeholder, "-03.10 dB"); err != nil {
		t.Fatalf("PatchFreeform: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, freeformAtom("REPLAYGAIN_TRACK_GAIN", "-03.10 dB")) {
		t.Error("patched freeform atom missing")
	}
	if bytes.Contains(data, []byte(placeholder)) {
		t.Error("placeholder still present after the patch")
	}
	if err := PatchFreeform(f, "REPLAYGAIN_TRACK_GAIN", "-03.10 dB", "-3.1 dB"); err == nil {
		t.Error("width-mismatched value accepted")
	}
	if err := PatchFreeform(f, "REPLAYGAIN_ALBUM_GAIN", placeholder, "-03.10 dB"); err == nil {
		t.Error("absent key accepted")
	}
}

// TestMuxMetadataDeterministic checks two identical runs with tags,
// chapters, and art produce byte-identical output: the atom builders
// walk fixed tables, never map iteration order.
func TestMuxMetadataDeterministic(t *testing.T) {
	opts := &MuxerOptions{
		Tags: []container.Tag{
			{Key: "TITLE", Value: "Same"},
			{Key: "ARTIST", Value: "Every"},
			{Key: "ARTIST", Value: "Time"},
			{Key: "TRACKNUMBER", Value: "3"},
			{Key: "REPLAYGAIN_TRACK_GAIN", Value: "+00.00 dB"},
		},
		Chapters: []container.Chapter{
			{Start: 0, Title: "One"},
			{Start: 45 * time.Second, Title: "Two"},
		},
		Art: &container.Picture{MIME: "image/jpeg", Data: []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2}},
	}
	src := metaTone(alac.FrameSize + 100)
	defer audio.Put(src)
	var a, b bytes.Buffer
	muxALACMeta(t, &a, src, opts)
	muxALACMeta(t, &b, src, opts)
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two identical runs differ")
	}
}

// TestMuxNoMetadataNoUdta pins the regression guard: nil options must
// leave the moov free of any udta box.
func TestMuxNoMetadataNoUdta(t *testing.T) {
	src := metaTone(alac.FrameSize)
	defer audio.Put(src)
	var out bytes.Buffer
	muxALACMeta(t, &out, src, nil)
	moov := findChild(out.Bytes(), "moov")
	if moov == nil {
		t.Fatal("no moov box")
	}
	if bytes.Contains(moov, []byte("udta")) {
		t.Error("udta present with no metadata options")
	}
}

// TestPatchFreeformPastFixedWindow pins the patch scan extent: a huge
// text atom (multi-MB lyrics) ahead of the ReplayGain freeform atoms
// must not push them out of PatchFreeform's reach, because the scan
// resolves the real ftyp+moov extent from the box headers rather than
// assuming a fixed window.
func TestPatchFreeformPastFixedWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.m4a")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := "+00.00 dB"
	tags := []container.Tag{
		{Key: "LYRICS", Value: strings.Repeat("la ", 2<<20)}, // ~6 MiB, written before the freeform atoms
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: placeholder},
	}
	muxAACMeta(t, fh, 4096, -1, &MuxerOptions{Tags: tags})
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	fh, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := PatchFreeform(fh, "REPLAYGAIN_TRACK_GAIN", placeholder, "-12.30 dB"); err != nil {
		t.Fatalf("PatchFreeform: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("-12.30 dB")) || bytes.Contains(raw, []byte(placeholder)) {
		t.Fatal("patched value did not land past the old fixed window")
	}
}
