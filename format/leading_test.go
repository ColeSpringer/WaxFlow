package format

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// stream is one fixture of each driver that reads a raw stream a tagger can
// put an ID3v2 tag in front of.
type stream struct {
	name string // the driver's
	raw  []byte
	// gap is how stray bytes between the tags and the stream are reported:
	// by this package (a warning at their offset, refused under Strict), by
	// the demuxer in its own words, or not at all where the format's own
	// readers step over them unremarked.
	gap string
}

const (
	gapFormat  = "format"
	gapDemuxer = "demuxer"
	gapSilent  = "silent"
)

func streams(t *testing.T) []stream {
	t.Helper()
	return []stream{
		{"flac", readFixture(t, "testdata", "sine-s16.flac"), gapFormat},
		{"wavpack", readFixture(t, "testdata", "sine-s16.wv"), gapFormat},
		{"ogg", readFixture(t, "testdata", "sine-s16.oga"), gapFormat},
		{"ape", readFixture(t, "testdata", "sine-s16.ape"), gapSilent},
		{"musepack", readFixture(t, "testdata", "sine-s16.mpc"), gapSilent},
		{"adts", readFixture(t, "container", "adts", "testdata", "stereo.aac"), gapDemuxer},
		{"mp3", readFixture(t, "testdata", "sine-cbr128.mp3"), gapDemuxer},
	}
}

// samplesOf opens raw under hint and reads it from sample to the end.
func samplesOf(t *testing.T, raw []byte, hint string, sample int64) (*Info, []float64) {
	t.Helper()
	med, err := Open(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer med.Close()
	if sample > 0 {
		if landed, err := med.SeekSample(context.Background(), sample); err != nil || landed != sample {
			t.Fatalf("a seek to %d landed at %d: %v", sample, landed, err)
		}
	}
	f := med.Info().Default().Fmt
	dst := audio.Get(f, audio.StandardChunk)
	defer audio.Put(dst)
	var out []float64
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			return med.Info(), out
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		for i := range dst.N {
			if f.Type == audio.Float {
				out = append(out, float64(dst.ChanF(0)[i]))
			} else {
				out = append(out, float64(dst.ChanI(0)[i]))
			}
		}
	}
}

// TestStreamsBehindLeadingTags: a stream behind chained ID3v2 tags, or behind
// tags and bytes that are no tag, is identified by what it is, with no hint
// and against a wrong one, and reads as the stream alone does: the same
// length from a strict probe, the same samples from a read and behind a seek.
// Tags alone are no finding. Stray bytes are damage, in the words of whoever
// steps over them, except where the format's own readers say nothing.
func TestStreamsBehindLeadingTags(t *testing.T) {
	ctx := context.Background()
	junk := bytes.Repeat([]byte{0xAA}, 300)
	for _, s := range streams(t) {
		clean, err := Probe(ctx, container.BytesSource(s.raw), "", &Options{Strict: true})
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		_, want := samplesOf(t, s.raw, "", 0)
		mid := int64(len(want) / 2)
		for _, lead := range []struct {
			name string
			raw  []byte
			gap  int
		}{
			{"two tags", behind(nil, 60, 200), 0},
			{"eight tags", behind(nil, 60, 200, 11, 60, 60, 4096, 60, 60), 0},
			{"a tag and NULs", append(behind(nil, 60), make([]byte, 100)...), 100},
			{"two tags and junk", append(behind(nil, 60, 200), junk...), 300},
		} {
			for _, hint := range []string{"", "wav", "mp3", "flac"} {
				t.Run(s.name+" behind "+lead.name+" as "+hint, func(t *testing.T) {
					raw := slices.Concat(lead.raw, s.raw)
					tagged := int64(len(lead.raw) - lead.gap)
					info, got := samplesOf(t, raw, hint, 0)
					if info.Container != s.name {
						t.Fatalf("identified as %s", info.Container)
					}
					if !slices.Equal(got, want) {
						t.Errorf("a read delivered %d samples, other than the stream's own %d", len(got), len(want))
					}
					if _, tail := samplesOf(t, raw, hint, mid); !slices.Equal(tail, want[mid:]) {
						t.Errorf("a read behind a seek to %d differs from the stream's own", mid)
					}
					strict, err := Probe(ctx, container.BytesSource(raw), hint, &Options{Strict: true})
					switch {
					case lead.gap == 0 || s.gap == gapSilent:
						if len(info.Warnings) != 0 || info.Damage != 0 {
							t.Errorf("warnings %v, damage %d", info.Warnings, info.Damage)
						}
						if err != nil || strict.Default().Samples != clean.Default().Samples {
							t.Errorf("a strict probe = %v, want the stream's own %d samples", err, clean.Default().Samples)
						}
					case s.gap == gapFormat:
						msg := fmt.Sprintf("%d bytes in front of the stream belong to no tag (offset %d)", lead.gap, tagged)
						if !slices.Equal(info.Warnings, []string{msg}) || info.Damage != 1 {
							t.Errorf("warnings %v, damage %d; want %q", info.Warnings, info.Damage, msg)
						}
						if !errors.Is(err, waxerr.ErrMalformedInput) || !strings.Contains(err.Error(), fmt.Sprintf("at offset %d", tagged)) {
							t.Errorf("a strict probe = %v, want the stray bytes refused at %d", err, tagged)
						}
					default:
						msg := fmt.Sprintf("%d unparsable bytes before the first frame (offset %d)", lead.gap, tagged)
						if !slices.Equal(info.Warnings, []string{msg}) {
							t.Errorf("warnings %v, want the demuxer's own %q", info.Warnings, msg)
						}
						if !errors.Is(err, waxerr.ErrMalformedInput) {
							t.Errorf("a strict probe = %v, want the stray bytes refused", err)
						}
					}
				})
			}
		}
	}
}

// TestStreamStartScanIsBounded: a stream is looked for in the 64 KiB behind
// the tags and no further, and with no tag in front only a checksummed start
// is trusted at all, so stray bytes never become a format on a sync word's
// say-so. Past the bound, and in front of an unchecksummed stream with no
// tag, the hint is all there is.
func TestStreamStartScanIsBounded(t *testing.T) {
	ctx := context.Background()
	for _, s := range streams(t) {
		under := slices.Concat(leadingTag(60), bytes.Repeat([]byte{0xAA}, startScan-1), s.raw)
		if info, err := Probe(ctx, container.BytesSource(under), "", nil); err != nil || info.Container != s.name {
			t.Errorf("%s one byte inside the scan: %v, %v", s.name, info, err)
		}
		over := slices.Concat(leadingTag(60), bytes.Repeat([]byte{0xAA}, startScan), s.raw)
		if info, err := Probe(ctx, container.BytesSource(over), "", nil); !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Errorf("%s at the scan's bound: %v, %v; want unrecognized", s.name, info, err)
		}
		bare := slices.Concat(bytes.Repeat([]byte{0xAA}, 9000), s.raw)
		info, err := Probe(ctx, container.BytesSource(bare), "", nil)
		if s.name == "ogg" {
			if err != nil || info.Container != "ogg" || info.Damage != 1 {
				t.Errorf("ogg behind bytes and no tag: %v, %v; want the checksummed page found", info, err)
			}
		} else if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Errorf("%s behind bytes and no tag: %v, %v; want unrecognized", s.name, info, err)
		}
	}
}

// TestAConfirmedStartBeatsTheMP3Sniff: the mp3 row matches a frame pair
// behind junk, anywhere in its window. A stream another driver confirms in
// front of that pair is the stream; the pair is its payload.
func TestAConfirmedStartBeatsTheMP3Sniff(t *testing.T) {
	flac := readFixture(t, "testdata", "sine-s16.flac")
	// Two MPEG frames in a PADDING block right behind STREAMINFO.
	frames := testutil.SyntheticMP3Frames(2)
	pad := append([]byte{1, byte(len(frames) >> 16), byte(len(frames) >> 8), byte(len(frames))}, frames...)
	if flac[4]&0x80 != 0 {
		t.Fatal("the fixture's STREAMINFO is its last block")
	}
	raw := slices.Concat(leadingTag(60), make([]byte, 10), flac[:42], pad, flac[42:])
	_, want := samplesOf(t, flac, "", 0)
	for _, hint := range []string{"", "mp3"} {
		info, got := samplesOf(t, raw, hint, 0)
		if info.Container != "flac" || !slices.Equal(got, want) {
			t.Errorf("hint %q: identified as %s, %d samples; want the FLAC stream's own %d", hint, info.Container, len(got), len(want))
		}
	}
}

// TestALeadingTagPastTheEndEndsTheChain: a tag that declares more bytes than
// the file holds is not skipped, so the bytes behind its header are still
// what the sniff sees.
func TestALeadingTagPastTheEndEndsTheChain(t *testing.T) {
	flac := readFixture(t, "testdata", "sine-s16.flac")
	long := leadingTag(60)
	long[9] = 0x7F
	long[8] = 0x7F
	long[7] = 0x7F
	raw := slices.Concat(leadingTag(60), long[:10], flac)
	info, err := Probe(context.Background(), container.BytesSource(raw), "", nil)
	if err != nil || info.Container != "flac" || info.Damage != 1 {
		t.Errorf("probe = %v, %v; want the FLAC stream behind 10 stray bytes", info, err)
	}
}

// TestStrayBytesAreTheDemuxersFinding: bytes left in front of the stream are
// reported by the demuxer itself, in its warnings and its damage count, so
// every reader of a demuxer sees them: a caller that moves packets, which
// holds no Info of this package's making, as much as one that decodes.
func TestStrayBytesAreTheDemuxersFinding(t *testing.T) {
	for _, s := range streams(t) {
		if s.gap != gapFormat {
			continue
		}
		raw := slices.Concat(leadingTag(60), make([]byte, 100), s.raw)
		demux, info, err := OpenDemuxer(container.BytesSource(raw), "", nil)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		want := container.Warning{Offset: 60, Msg: "100 bytes in front of the stream belong to no tag", Kind: container.Damage}
		ws := demux.(container.Warner).Warnings()
		if len(ws) == 0 || ws[0] != want || container.DamageOf(demux) != 1 {
			t.Errorf("%s: the demuxer lists %v with damage %d, want %v first", s.name, ws, container.DamageOf(demux), want)
		}
		// A fresh Info refreshed from the demuxer alone says the same.
		var fresh Info
		RefreshWarnings(&fresh, demux)
		if !slices.Equal(fresh.Warnings, info.Warnings) || fresh.Damage != 1 {
			t.Errorf("%s: a fresh Info reads %v (damage %d), the open's %v", s.name, fresh.Warnings, fresh.Damage, info.Warnings)
		}
	}
}

// TestMagicsInTextAreNoStream: a tag, then text that holds another format's
// magic, then an MP3 or ADTS stream is that stream, named or not, with the
// text reported as the bytes in front of its first frame. A start is taken
// from a magic only where the structure behind it holds, and one taken in
// bytes that then do not open gives way to the next.
func TestMagicsInTextAreNoStream(t *testing.T) {
	ctx := context.Background()
	for _, s := range streams(t) {
		if s.name != "mp3" && s.name != "adts" {
			continue
		}
		hint := map[string]string{"mp3": "mp3", "adts": "aac"}[s.name]
		_, want := samplesOf(t, s.raw, hint, 0)
		for _, text := range []string{
			"Comment: transcoded from MP+7 (SV7) MPC v1.15 by Frank in 2004.",
			"Encoded from the fLaC\x00\x00\x00\x22 master of the album, then fLaC again.",
			"The source was a wvpk WavPack rip of the original disc: wvpk, v5.",
			"From MAC 3.99 (MAC and MACF), a Monkey's Audio rip, for a player.",
			"Not MPCKSH8 nor MPCKAP: those are Musepack packet names, not ours.",
			"An OggS\x00\x02 page starts that way, and OggS is all there is here.",
		} {
			raw := slices.Concat(leadingTag(60), []byte(text), s.raw)
			msg := fmt.Sprintf("%d unparsable bytes before the first frame (offset 60)", len(text))
			for _, h := range []string{hint, ""} {
				info, got := samplesOf(t, raw, h, 0)
				if info.Container != s.name || !slices.Equal(got, want) || !slices.Equal(info.Warnings, []string{msg}) {
					t.Errorf("%s behind %q as %q: identified as %s, %d samples, warnings %v; want the stream's own %d and %q",
						s.name, text[:12], h, info.Container, len(got), info.Warnings, len(want), msg)
				}
				if _, err := Probe(ctx, container.BytesSource(raw), h, &Options{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
					t.Errorf("%s behind %q as %q: a strict probe = %v, want the text refused", s.name, text[:12], h, err)
				}
			}
		}
	}
}

// TestAStartThatDoesNotOpenFallsBack: a start a driver confirms is where the
// open goes first, but it is confirmed from a window, not from the file.
// Where that open is refused and the sniff matched another row, the file
// opens as that row would have opened it.
func TestAStartThatDoesNotOpenFallsBack(t *testing.T) {
	flac := readFixture(t, "testdata", "sine-s16.flac")
	mp3 := readFixture(t, "testdata", "sine-cbr128.mp3")
	// The marker, STREAMINFO and the next block's header of a FLAC stream,
	// and no stream behind them.
	raw := slices.Concat(leadingTag(60), make([]byte, 10), flac[:46], mp3)
	_, want := samplesOf(t, mp3, "mp3", 0)
	for _, hint := range []string{"", "mp3"} {
		info, got := samplesOf(t, raw, hint, 0)
		if info.Container != "mp3" || !slices.Equal(got, want) {
			t.Errorf("hint %q: identified as %s, %d samples; want the MP3 stream's own %d", hint, info.Container, len(got), len(want))
		}
	}
	// With no other row to fall back on, the confirmed driver's refusal is
	// the answer.
	bare := slices.Concat(leadingTag(60), make([]byte, 10), flac[:46], make([]byte, 9000))
	if _, err := Probe(context.Background(), container.BytesSource(bare), "", nil); err == nil || !strings.HasPrefix(err.Error(), "flac: ") {
		t.Errorf("probe = %v, want the FLAC demuxer's refusal", err)
	}
}

// TestFindStartTakesNoDecoy: every driver's FindStart finds its own fixture
// behind stray bytes and nothing in zeros, ones, or text that spells the
// magics, each followed by more text.
func TestFindStartTakesNoDecoy(t *testing.T) {
	text := []byte(strings.Repeat("fLaC wvpk MAC  MACF MPCK MP+7 MP+\x07 OggS ID3 TAG APETAGEX the quick brown fox ", 64))
	decoys := map[string][]byte{"zeros": make([]byte, 8192), "ones": bytes.Repeat([]byte{0xFF}, 8192), "text": text}
	for _, s := range streams(t) {
		var d *driver
		for i := range drivers {
			if drivers[i].name == s.name {
				d = &drivers[i]
			}
		}
		if at, ok := d.start(slices.Concat(bytes.Repeat([]byte{0xAA}, 300), s.raw)); !ok || at != 300 {
			t.Errorf("%s: FindStart = %d, %v on its own stream behind 300 bytes", s.name, at, ok)
		}
		for name, window := range decoys {
			if at, ok := d.start(window); ok {
				t.Errorf("%s: FindStart = %d in %s", s.name, at, name)
			}
		}
	}
}

// TestACutOffHeadInFrontOfAStreamIsItsJunk: a tag, stray bytes, the cut-off
// head of some other stream and then an MP3 or ADTS stream is that stream.
// The head's start is confirmed by its own structure and lies in front of the
// frames, but a run of frames that long is no other stream's payload, so the
// head is the bytes in front of the first frame: reported as such, and
// refused under Strict in the frame stream's own words.
func TestACutOffHeadInFrontOfAStreamIsItsJunk(t *testing.T) {
	ctx := context.Background()
	heads := map[string][]byte{}
	for _, s := range streams(t) {
		if s.name != "mp3" && s.name != "adts" {
			heads[s.name] = s.raw
		}
	}
	for _, s := range streams(t) {
		if s.name != "mp3" && s.name != "adts" {
			continue
		}
		prefix := map[string]string{"mp3": "mp3: ", "adts": "adts: "}[s.name]
		_, want := samplesOf(t, s.raw, "", 0)
		for head, raw := range heads {
			for _, n := range []int{200, 600} {
				file := slices.Concat(leadingTag(60), bytes.Repeat([]byte{0x11}, 10), raw[:n], s.raw)
				msg := fmt.Sprintf("%d unparsable bytes before the first frame", 10+n)
				for _, hint := range []string{"", s.name} {
					info, got := samplesOf(t, file, hint, 0)
					if info.Container != s.name || !slices.Equal(got, want) || !slices.Equal(info.Warnings, []string{msg + " (offset 60)"}) {
						t.Errorf("%s behind %d bytes of %s as %q: identified as %s, %d samples, warnings %v; want the stream's own %d and %q",
							s.name, n, head, hint, info.Container, len(got), info.Warnings, len(want), msg)
					}
					_, err := Probe(ctx, container.BytesSource(file), hint, &Options{Strict: true})
					if err == nil || err.Error() != prefix+msg+" (at offset 60)" {
						t.Errorf("%s behind %d bytes of %s as %q: a strict probe = %v, want the stream's own refusal of the %d bytes", s.name, n, head, hint, err, 10+n)
					}
				}
			}
		}
	}
}

// TestStrictBlamesTheStreamThatIsThere: under Strict the bytes in front of a
// confirmed start are refused only once that start has opened. One that does
// not open is no stream, and the refusal is the next opening's own.
func TestStrictBlamesTheStreamThatIsThere(t *testing.T) {
	flac := readFixture(t, "testdata", "sine-s16.flac")
	mp3 := readFixture(t, "testdata", "sine-cbr128.mp3")
	// Four frames and then junk: too short a run to go first.
	short := append(mp3Frames(t, mp3, 0, 4), bytes.Repeat([]byte{0x11}, 64)...)
	raw := slices.Concat(leadingTag(60), make([]byte, 10), flac[:46], short)
	if info, err := Probe(context.Background(), container.BytesSource(raw), "", nil); err != nil || info.Container != "mp3" {
		t.Fatalf("probe = %v, %v; want the MP3 frames behind a FLAC head that does not open", info, err)
	}
	_, err := Probe(context.Background(), container.BytesSource(raw), "", &Options{Strict: true})
	if want := "mp3: 56 unparsable bytes before the first frame (at offset 60)"; err == nil || err.Error() != want {
		t.Errorf("a strict probe = %v, want %q", err, want)
	}
	// A FLAC stream that does open keeps its gap's refusal.
	whole := slices.Concat(leadingTag(60), make([]byte, 10), flac)
	_, err = Probe(context.Background(), container.BytesSource(whole), "", &Options{Strict: true})
	if want := "format: 10 bytes in front of the stream belong to no tag (at offset 60)"; err == nil || err.Error() != want {
		t.Errorf("a strict probe = %v, want %q", err, want)
	}
}

// leadShapes is every way this file puts bytes in front of a stream, each
// with the stream behind it and what the file is: tags, tags and stray
// bytes, text holding other formats' magics, the cut-off head of another
// stream, and two MPEG frames behind the stream's end.
func leadShapes(t *testing.T) (shapes []struct {
	name, is string
	raw      []byte
}) {
	add := func(name, is string, raw []byte) {
		shapes = append(shapes, struct {
			name, is string
			raw      []byte
		}{name, is, raw})
	}
	all := streams(t)
	mp3 := readFixture(t, "testdata", "sine-cbr128.mp3")
	tail := mp3Frames(t, mp3, 5, 7) // two audio frames
	for _, s := range all {
		add(s.name+" behind two tags", s.name, slices.Concat(behind(nil, 60, 200), s.raw))
		add(s.name+" behind a tag and NULs", s.name, slices.Concat(leadingTag(60), make([]byte, 100), s.raw))
		add(s.name+" behind tags and junk", s.name, slices.Concat(behind(nil, 60, 200), bytes.Repeat([]byte{0xAA}, 300), s.raw))
		if s.name != "mp3" && s.name != "adts" {
			add(s.name+" behind NULs, two MPEG frames behind it", s.name, slices.Concat(leadingTag(60), make([]byte, 10), s.raw, tail))
			continue
		}
		add(s.name+" behind text", s.name, slices.Concat(leadingTag(60), []byte("Encoded from the fLaC\x00\x00\x00\x22 master, a wvpk rip, via MAC 3.99 and MPCKSH, OggS\x00\x02."), s.raw))
		for _, head := range all {
			if head.name != "mp3" && head.name != "adts" {
				add(s.name+" behind the head of "+head.name, s.name, slices.Concat(leadingTag(60), bytes.Repeat([]byte{0x11}, 10), head.raw[:600], s.raw))
			}
		}
	}
	return shapes
}

// TestStrictNeverChoosesTheFormat states the rule for every leading shape and
// every hint: the format is the one a tolerant open picks, and a strict open
// either opens that driver or refuses on its behalf. Strict changes whether a
// file is accepted, never what it is.
func TestStrictNeverChoosesTheFormat(t *testing.T) {
	for _, sh := range leadShapes(t) {
		for _, hint := range []string{"", "wav", "mp3", "aac", "flac", "mpc", "ape", "wv", "ogg"} {
			src := container.BytesSource(sh.raw)
			_, tolerant, err := openSource(src, hint, false)
			if err != nil || tolerant.name != sh.is {
				name := "nothing"
				if tolerant != nil {
					name = tolerant.name
				}
				t.Errorf("%s as %q: a tolerant open chose %s (%v), want %s", sh.name, hint, name, err, sh.is)
				continue
			}
			if _, strict, err := openSource(src, hint, true); strict != tolerant {
				name := "nothing"
				if strict != nil {
					name = strict.name
				}
				t.Errorf("%s as %q: a strict open answered for %s (%v), a tolerant one chose %s", sh.name, hint, name, err, tolerant.name)
			}
		}
	}
}

// TestAGapIsRefusedForTheStreamBehindIt: under Strict the bytes in front of a
// stream are that stream's damage and the file is refused for them, even
// where another driver further in would have passed. A FLAC stream behind
// stray bytes with a Musepack stream behind it, whose driver steps over what
// leads it without a word, is the FLAC stream to both opens.
func TestAGapIsRefusedForTheStreamBehindIt(t *testing.T) {
	flac := readFixture(t, "testdata", "sine-s16.flac")
	mpc := readFixture(t, "testdata", "sine-s16.mpc")
	raw := slices.Concat(leadingTag(60), make([]byte, 10), flac, mpc)
	info, err := Probe(context.Background(), container.BytesSource(raw), "", nil)
	if err != nil || info.Container != "flac" {
		t.Fatalf("probe = %v, %v; want the FLAC stream", info, err)
	}
	_, d, err := openSource(container.BytesSource(raw), "", true)
	if want := "format: 10 bytes in front of the stream belong to no tag (at offset 60)"; err == nil || err.Error() != want || d == nil || d.name != "flac" {
		t.Errorf("a strict open = %v for %v, want %q for the FLAC stream", err, d, want)
	}
}

// TestAShortRunBehindAnotherStreamsHeadIsThatHead pins a limit as what it is:
// a run of MPEG frames goes ahead of a start another driver confirms only
// when it is long, since a short one is what a real stream's payload or tail
// can hold. So a real stream with a frame pair behind it stays itself, and a
// frame stream of a few frames behind a cut-off head that still opens is
// read as that head.
func TestAShortRunBehindAnotherStreamsHeadIsThatHead(t *testing.T) {
	ape := readFixture(t, "testdata", "sine-s16.ape")
	mp3 := readFixture(t, "testdata", "sine-cbr128.mp3")
	short := append(mp3Frames(t, mp3, 0, 4), bytes.Repeat([]byte{0x11}, 64)...)
	raw := slices.Concat(leadingTag(60), make([]byte, 10), ape[:600], short)
	for _, hint := range []string{"", "mp3"} {
		info, err := Probe(context.Background(), container.BytesSource(raw), hint, nil)
		if err != nil || info.Container != "ape" {
			t.Errorf("hint %q: probe = %v, %v; the limit is that this reads as the ape head", hint, info, err)
		}
	}
	// Eight frames is a stream.
	long := slices.Concat(leadingTag(60), make([]byte, 10), ape[:600], mp3Frames(t, mp3, 0, 8), bytes.Repeat([]byte{0x11}, 64))
	if info, err := Probe(context.Background(), container.BytesSource(long), "", nil); err != nil || info.Container != "mp3" {
		t.Errorf("eight frames behind the head: probe = %v, %v; want mp3", info, err)
	}
}

// TestAStrictOpenCostsOneOpen: where Strict has nothing to refuse, a strict
// open reads what a tolerant one reads. The tolerant open that settles the
// format is only paid behind a strict refusal.
func TestAStrictOpenCostsOneOpen(t *testing.T) {
	for _, s := range streams(t) {
		for name, raw := range map[string][]byte{"alone": s.raw, "behind two tags": slices.Concat(behind(nil, 60, 200), s.raw)} {
			var cost [2][2]int64
			for i, strict := range []bool{false, true} {
				src := &testutil.CountingSource{Src: container.BytesSource(raw)}
				if _, _, err := OpenDemuxer(src, "", &Options{Strict: strict}); err != nil {
					t.Fatalf("%s %s, strict %v: %v", s.name, name, strict, err)
				}
				cost[i] = [2]int64{int64(src.Reads), src.Bytes}
			}
			if cost[0] != cost[1] {
				t.Errorf("%s %s: a strict open cost %v (reads, bytes), a tolerant one %v", s.name, name, cost[1], cost[0])
			}
		}
	}
}

// mp3Frames returns frames [from, to) of an MP3 with nothing in front of its
// first frame, the metadata frame counted as frame 0.
func mp3Frames(t *testing.T, raw []byte, from, to int) []byte {
	t.Helper()
	offs := []int{0}
	for len(offs) <= to {
		at := offs[len(offs)-1]
		h, err := mp3.ParseHeader(raw[at:])
		if err != nil || h.Size() == 0 || at+h.Size() > len(raw) {
			t.Fatalf("the fixture holds %d frames, want %d", len(offs)-1, to)
		}
		offs = append(offs, at+h.Size())
	}
	return bytes.Clone(raw[offs[from]:offs[to]])
}

// TestAStrictRefusalIsTheStrictOpensOwn: a file with one way to open it is
// opened once under Strict, and the refusal is that open's: the first damage
// met, at its file offset, where a tolerant open of the same file would have
// gone on to refuse something else.
func TestAStrictRefusalIsTheStrictOpensOwn(t *testing.T) {
	// The last 100 bytes cut, which Strict refuses first, and the seek
	// table's first entry off by one, which no open accepts.
	ape := readFixture(t, "testdata", "sine-s16.ape")
	table := binary.LittleEndian.Uint32(ape[8:]) + binary.LittleEndian.Uint32(ape[12:])
	binary.LittleEndian.PutUint32(ape[table:], binary.LittleEndian.Uint32(ape[table:])+1)
	ape = ape[:len(ape)-100]
	for _, hint := range []string{"", "ape"} {
		_, err := Probe(context.Background(), container.BytesSource(ape), hint, &Options{Strict: true})
		if want := "ape: the descriptor counts 15020 bytes of frame data but 14920 are present (at offset 15044)"; err == nil || err.Error() != want {
			t.Errorf("hint %q: a strict probe = %v, want %q", hint, err, want)
		}
	}

	// A WavPack head and 4 MiB that are no block: a strict probe reads what
	// finding the driver and one strict open read.
	wv := slices.Concat(readFixture(t, "testdata", "sine-s16.wv")[:40], bytes.Repeat([]byte{0x11}, 4<<20))
	probe := &testutil.CountingSource{Src: container.BytesSource(wv)}
	_, probeErr := Probe(context.Background(), probe, "", &Options{Strict: true})
	found := &testutil.CountingSource{Src: container.BytesSource(wv)}
	tries, err := resolve(found, "")
	if err != nil || len(tries) != 1 {
		t.Fatalf("resolve = %d openings, %v; want one", len(tries), err)
	}
	once := &testutil.CountingSource{Src: container.BytesSource(wv)}
	_, onceErr := tries[0].d.open(once, openOptions{strict: true})
	if probeErr == nil || onceErr == nil || probeErr.Error() != onceErr.Error() {
		t.Errorf("a strict probe = %v, one strict open %v", probeErr, onceErr)
	}
	if probe.Reads != found.Reads+once.Reads || probe.Bytes != found.Bytes+once.Bytes {
		t.Errorf("a strict probe read %d bytes in %d reads; finding the driver and one strict open read %d in %d",
			probe.Bytes, probe.Reads, found.Bytes+once.Bytes, found.Reads+once.Reads)
	}
}
