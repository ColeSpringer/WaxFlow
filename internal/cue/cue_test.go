package cue

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// TestCueFrameExactness pins the conversion the whole package is shaped
// around: a CD frame is 1/75 s, and at every CD-family rate that is a whole
// number of samples, so a cut point derived from a sheet is exact rather
// than nearly exact.
//
// This is the test that catches someone routing the conversion back
// through time.Duration, which is the obvious simplification and is wrong
// at the very first frame. See TestCueFrameDurationRouteIsLossy for the
// arithmetic.
func TestCueFrameExactness(t *testing.T) {
	// Every CD-family rate divides by 75. The per-frame sample counts are
	// spelled out rather than computed so the test states the fact instead
	// of restating the implementation.
	rates := map[int]int64{
		11025: 147,
		22050: 294,
		44100: 588,
		48000: 640,
		88200: 1176,
		96000: 1280,
	}
	// A grid over the wrap points: FF=74 is the last frame of a second and
	// SS=59 the last second of a minute, which is where an off-by-one in
	// the base arithmetic surfaces.
	for _, mm := range []int{0, 1, 59, 99, 137} {
		for _, ss := range []int{0, 1, 30, 59} {
			for _, ff := range []int{0, 1, 37, 74} {
				stamp := fmt.Sprintf("%02d:%02d:%02d", mm, ss, ff)
				frames, err := ParseTime(stamp)
				if err != nil {
					t.Fatalf("ParseTime(%q): %v", stamp, err)
				}
				if want := (mm*60+ss)*75 + ff; frames != want {
					t.Fatalf("ParseTime(%q) = %d frames, want %d", stamp, frames, want)
				}
				for rate, perFrame := range rates {
					got := Samples(frames, rate)
					want := int64(frames) * perFrame
					if got != want {
						t.Errorf("Samples(%d, %d) = %d, want %d (%s)", frames, rate, got, want, stamp)
					}
					// The property that makes it exact rather than merely
					// correct here: the conversion never leaves a remainder,
					// so no cut point is ever rounded.
					if int64(frames)*int64(rate)%FramesPerSecond != 0 {
						t.Errorf("%s at %d Hz leaves a remainder; %d is not a CD-family rate", stamp, rate, rate)
					}
				}
			}
		}
	}
}

// TestCueFrameDurationRouteIsLossy records why Samples does integer math on
// frames instead of the obvious thing.
//
// It asserts against a conversion this package deliberately does not have,
// which is unusual for a test and is the point: the Duration route looks
// correct, is what a reader would reach for while tidying, and is wrong at
// frame 1 of a plain 44.1 kHz rip. Pinning the divergence here means the
// next person to propose it reads the arithmetic rather than rediscovering
// it as a one-sample gap at every track boundary of a gapless album.
func TestCueFrameDurationRouteIsLossy(t *testing.T) {
	const rate = 44100
	// A frame is 13333333.33... ns, and time.Duration is an integer
	// nanosecond count, so it cannot hold one.
	frame := time.Second / FramesPerSecond
	if frame != 13333333 {
		t.Fatalf("time.Second/75 = %d ns; the premise of this test moved", frame)
	}
	viaDuration := int64(frame) * rate / int64(time.Second)
	if viaDuration != 587 {
		t.Fatalf("the Duration route yields %d samples for frame 1, expected the lossy 587", viaDuration)
	}
	if got := Samples(1, rate); got != 588 {
		t.Fatalf("Samples(1, %d) = %d, want the exact 588", rate, got)
	}
}

// TestParseTimeMinutesBound pins the refusal that keeps a wire-supplied MM
// from wrapping the frame arithmetic.
//
// The stamp below is not arbitrary: (2049638230412173*60)*75 is 2693 past
// MaxInt64, so before the bound existed ParseTime returned a large negative
// frame count and a nil error, and that negative reached Samples and
// validate as though it were a position in the file. Atoi cannot catch it,
// because the value fits an int fine; only the multiply is out of range.
//
// The wanted frame count is written out rather than computed, since
// computing it here would reproduce the overflow the test is about.
func TestParseTimeMinutesBound(t *testing.T) {
	const overflows = "2049638230412173:00:00"
	frames, err := ParseTime(overflows)
	if err == nil {
		t.Fatalf("ParseTime(%q) = %d frames, no error; MM this large cannot be a position", overflows, frames)
	}
	if frames != 0 {
		t.Errorf("ParseTime(%q) = %d alongside its error, want 0", overflows, frames)
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("ParseTime(%q) code = %v, want CodeInvalidRequest", overflows, got)
	}
	if !strings.Contains(err.Error(), "addresses at most") {
		t.Errorf("ParseTime(%q) error = %v, want it to name the bound", overflows, err)
	}

	// The bound is only defensible if it still admits the long single-file
	// sources that legitimately carry a sheet. A 40-hour audiobook rip is
	// the shape it must not refuse.
	const audiobook = "2400:00:00"
	if got, err := ParseTime(audiobook); err != nil {
		t.Errorf("ParseTime(%q): %v; a 40-hour source with a sheet is real", audiobook, err)
	} else if got != 2400*60*75 {
		t.Errorf("ParseTime(%q) = %d frames, want %d", audiobook, got, 2400*60*75)
	}
}

// TestParseTimeOverflowSheetRefused drives the bound through Parse, since
// the damage was never in ParseTime's return: a negative frame count became
// a negative sample offset and a cut point addressing before the file.
//
// The wrapped stamp is track 1's, and that is the whole design of the test.
// A negative start on any later track is caught by the ascending rule,
// which would let this pass on the strength of an unrelated fix while the
// overflow went unrefused. Track 1 has no predecessor to be measured
// against, so nothing stands between the wrap and a cut point except the
// bound this is here to pin.
func TestParseTimeOverflowSheetRefused(t *testing.T) {
	in := "FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n    INDEX 01 2049638230412173:00:00\n" +
		"  TRACK 02 AUDIO\n    INDEX 01 00:02:00\n"
	sheet, err := Parse([]byte(in))
	if err == nil {
		start, _ := sheet.Files[0].Tracks[0].Start()
		t.Fatalf("Parse accepted a wrapped timestamp; track 1 starts at frame %d (%d samples)",
			start, Samples(start, 44100))
	}
	if sheet != nil {
		t.Errorf("Parse returned both a sheet and an error")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("code = %v, want CodeInvalidRequest", got)
	}
}

// TestParseStartsMustAscend pins strict ascent, and the frame-0 case that
// the sentinel it replaced only ever handled by luck.
//
// Equal starts are the interesting half: they parsed here and then failed
// at jobs.SplitSpans, which refuses a zero-sample piece, so one sheet got
// two answers depending on which caller read it. The CLI's answer was an
// empty file written without complaint.
func TestParseStartsMustAscend(t *testing.T) {
	equal := "FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n    INDEX 01 01:00:00\n" +
		"  TRACK 02 AUDIO\n    INDEX 01 01:00:00\n"
	if _, err := Parse([]byte(equal)); err == nil {
		t.Fatalf("Parse accepted two tracks sharing an INDEX 01; that names a zero-sample piece")
	} else if !strings.Contains(err.Error(), "have to ascend") {
		t.Errorf("Parse error = %v, want it to name the ascending rule", err)
	}

	// A first track at frame 0 is the overwhelmingly common sheet. It has no
	// predecessor to ascend from, so it has to keep parsing.
	zero := "FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n    INDEX 01 01:00:00\n"
	sheet, err := Parse([]byte(zero))
	if err != nil {
		t.Fatalf("Parse(track 1 at frame 0): %v", err)
	}
	if start, ok := sheet.Files[0].Tracks[0].Start(); !ok || start != 0 {
		t.Errorf("track 1 start = %d (%v), want frame 0", start, ok)
	}
}

// TestValidateNeverIndexesBeforeTheFirstTrack holds the ascent loop to
// reporting a predecessor it actually has.
//
// The loop used to name the offender as f.Tracks[ti-1], which at ti == 0 is
// f.Tracks[-1] and a panic. ParseTime's bound removes the only way to reach
// it from wire bytes today, so this drives validate through a hand-built
// Sheet: the loop must not be able to index -1 whatever Start returns, and
// a bound in another function is not that guarantee.
func TestValidateNeverIndexesBeforeTheFirstTrack(t *testing.T) {
	sheet := &Sheet{Files: []File{{
		Name: "a.flac",
		Tracks: []Track{
			{Number: 1, Indexes: []Index{{Number: 1, Frame: -9223372036854773116}}},
			{Number: 2, Indexes: []Index{{Number: 1, Frame: 4500}}},
		},
	}}}
	// A negative first start is what a wrapped ParseTime produced. Whatever
	// validate makes of it, it must return rather than panic.
	if err := validate(sheet); err != nil && waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("validate code = %v, want CodeInvalidRequest", waxerr.CodeOf(err))
	}
}

const sheetBasic = `REM GENRE "Rock"
REM DATE 1997
PERFORMER "The Performer"
TITLE "The Album"
FILE "album.flac" WAVE
  TRACK 01 AUDIO
    TITLE "First"
    PERFORMER "Guest"
    INDEX 00 00:00:00
    INDEX 01 00:00:33
  TRACK 02 AUDIO
    TITLE "Second"
    ISRC ABCDE1234567
    INDEX 01 05:50:65
  TRACK 03 AUDIO
    TITLE "Third"
    INDEX 01 09:12:00
`

func TestParseBasic(t *testing.T) {
	sheet, err := Parse([]byte(sheetBasic))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sheet.Title != "The Album" || sheet.Performer != "The Performer" {
		t.Errorf("disc metadata: title %q performer %q", sheet.Title, sheet.Performer)
	}
	if len(sheet.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(sheet.Files))
	}
	f := sheet.Files[0]
	if f.Name != "album.flac" || f.Type != "WAVE" {
		t.Errorf("file = %q %q", f.Name, f.Type)
	}
	if len(f.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(f.Tracks))
	}
	// A track-level TITLE must land on the track and leave the disc's
	// alone, which is the one thing the shared string arm could get wrong.
	if f.Tracks[0].Title != "First" || sheet.Title != "The Album" {
		t.Errorf("track title %q leaked into or from the disc title %q", f.Tracks[0].Title, sheet.Title)
	}
	if f.Tracks[0].Performer != "Guest" {
		t.Errorf("track performer = %q, want Guest", f.Tracks[0].Performer)
	}
	if f.Tracks[1].ISRC != "ABCDE1234567" {
		t.Errorf("ISRC = %q", f.Tracks[1].ISRC)
	}
	// INDEX 00 is the pregap and must not be taken as the start.
	start, ok := f.Tracks[0].Start()
	if !ok || start != 33 {
		t.Errorf("track 1 start = %d (%v), want frame 33 (INDEX 01, not INDEX 00)", start, ok)
	}
	if start, _ := f.Tracks[1].Start(); start != (5*60+50)*75+65 {
		t.Errorf("track 2 start = %d", start)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"track before file", "TRACK 01 AUDIO\n", "before any FILE"},
		{"index outside track", "FILE \"a.flac\" WAVE\nINDEX 01 00:00:00\n", "outside a TRACK"},
		{"no index 01", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 00 00:00:00\n", "no INDEX 01"},
		{"bad frame count", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 00:00:75\n", "a second holds 75"},
		{"bad seconds", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 00:60:00\n", "a minute holds 60"},
		{"not a timestamp", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 nope\n", "not MM:SS:FF"},
		{"unterminated quote", "TITLE \"unclosed\nFILE \"a.flac\" WAVE\n", "unterminated"},
		{"descending tracks", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 05:00:00\nTRACK 02 AUDIO\nINDEX 01 01:00:00\n", "have to ascend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error mentioning %q", tc.in, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestParseSkipsUnknown pins the best-effort half of the policy: a sheet is
// metadata beside the audio, so an unread line must not cost the caller a
// working split.
func TestParseSkipsUnknown(t *testing.T) {
	in := "CATALOG 1234567890123\n" +
		"CDTEXTFILE \"disc.cdt\"\n" +
		"REM COMMENT \"ExactAudioCopy v1.3\"\n" +
		"VENDOR_EXTENSION whatever\n" +
		"FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n" +
		"    FLAGS DCP\n" +
		"    PREGAP 00:00:32\n" +
		"    INDEX 01 00:00:00\n"
	sheet, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sheet.Catalog != "1234567890123" {
		t.Errorf("catalog = %q", sheet.Catalog)
	}
	tr := sheet.Files[0].Tracks[0]
	if tr.Pregap != 32 {
		t.Errorf("pregap = %d frames, want 32", tr.Pregap)
	}
	if len(tr.Flags) != 1 || tr.Flags[0] != "DCP" {
		t.Errorf("flags = %v", tr.Flags)
	}
}

// TestParseMultiFile covers the track-per-file rip, where each file holds
// one track at its own frame 0. The ascending check is per file, so this
// must not trip it.
func TestParseMultiFile(t *testing.T) {
	in := "FILE \"01.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
		"FILE \"02.flac\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"
	sheet, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(sheet.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(sheet.Files))
	}
	for i, f := range sheet.Files {
		if len(f.Tracks) != 1 {
			t.Fatalf("file %d has %d tracks, want 1", i, len(f.Tracks))
		}
	}
}

// TestCuts pins the split product, which is where the daemon and the CLI
// each kept a copy of the same derivation and the copies disagreed.
//
// The wanted lists are literal sample offsets rather than anything routed
// back through Starts or Samples: an expectation computed by the code under
// test agrees with it by construction, including when both are wrong. At
// 44100 a CD frame is 588 samples, so 00:02:00 is 150 frames and 88200
// samples, and 00:05:00 is 375 frames and 220500.
func TestCuts(t *testing.T) {
	for _, tc := range []struct {
		name, in   string
		want       []int64
		wantPieces int
	}{
		{
			// The common sheet. Track 1 opens the file, so its start is the
			// implied 0 and a cut there would open an empty piece.
			name: "first start at 0 is dropped",
			in: "FILE \"a.flac\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:02:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			want:       []int64{88200, 220500},
			wantPieces: 3,
		},
		{
			// Hidden track one audio: the two seconds before track 1's INDEX
			// 01 are a piece, not a rounding error. The daemon used to fold
			// them into track 1 and the CLI used to discard them.
			name: "first start past 0 is kept as the lead-in piece",
			in: "FILE \"a.flac\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:02:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:09:00\n",
			want: []int64{88200, 220500, 396900},
			// One more piece than there are tracks, and the extra has no
			// title. This is the count a caller pairing titles must expect.
			wantPieces: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sheet, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			f, err := sheet.SingleFile()
			if err != nil {
				t.Fatalf("SingleFile: %v", err)
			}
			got, err := f.Cuts(44100)
			if err != nil {
				t.Fatalf("Cuts: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Cuts(44100) = %v, want %v", got, tc.want)
			}
			// The documented contract callers size their output by: the cut
			// list is one short of the pieces it opens, whichever shape the
			// sheet has.
			if len(got)+1 != tc.wantPieces {
				t.Errorf("Cuts(44100) = %v opens %d pieces, want %d", got, len(got)+1, tc.wantPieces)
			}
			// Nothing discarded: the pieces run from 0 to the end, so the
			// first cut is never 0 and the cuts strictly ascend, which is
			// the rule jobs.SplitSpans holds a cut list to.
			prev := int64(0)
			for i, c := range got {
				if c <= prev {
					t.Fatalf("cut %d at sample %d does not advance past %d", i, c, prev)
				}
				prev = c
			}
		})
	}
}

// TestCutsRefusesOneTrack keeps the refusal in the funnel rather than in
// each caller, so a sheet that is not describing a division of its file is
// answered the same way whoever asked.
func TestCutsRefusesOneTrack(t *testing.T) {
	sheet, err := Parse([]byte("FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f, err := sheet.SingleFile()
	if err != nil {
		t.Fatalf("SingleFile: %v", err)
	}
	cuts, err := f.Cuts(44100)
	if err == nil {
		t.Fatalf("Cuts = %v for a one-track sheet, want a refusal", cuts)
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("code = %v, want CodeInvalidRequest", got)
	}
	if !strings.Contains(err.Error(), "nothing to cut") {
		t.Errorf("Cuts error = %v, want it to say there is nothing to cut", err)
	}
}

func TestDecodeEncodings(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		in         []byte
	}{
		{
			name: "utf8 passes through",
			in:   []byte("TITLE \"Café\""),
			want: "TITLE \"Café\"",
		},
		{
			name: "bom is stripped",
			in:   append([]byte(utf8BOM), []byte("TITLE \"x\"")...),
			want: "TITLE \"x\"",
		},
		{
			// The case the whole fallback exists for: 0x93/0x94 are curly
			// quotes in CP1252 and C1 control characters in latin-1.
			name: "cp1252 smart quotes",
			in:   []byte{'T', 'I', 'T', 'L', 'E', ' ', '"', 0x93, 'H', 'i', 0x94, '"'},
			want: "TITLE \"“Hi”\"",
		},
		{
			name: "cp1252 dash and ellipsis",
			in:   []byte{0x96, 0x97, 0x85},
			want: "–—…",
		},
		{
			// 0xE9 is é in both CP1252 and latin-1, and is not valid UTF-8
			// on its own, so it takes the fallback.
			name: "cp1252 high range",
			in:   []byte{0xE9},
			want: "é",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decode(tc.in); got != tc.want {
				t.Errorf("decode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseCP1252Sheet drives the fallback through the parser, since the
// payoff is a readable title rather than a readable byte.
func TestParseCP1252Sheet(t *testing.T) {
	in := []byte("FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"\x93Quoted\x94 \x96 Caf\xe9\"\n    INDEX 01 00:00:00\n")
	sheet, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := sheet.Files[0].Tracks[0].Title, "“Quoted” – Café"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func FuzzParseCue(f *testing.F) {
	f.Add([]byte(sheetBasic))
	f.Add([]byte("FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 00:00:00\n"))
	f.Add([]byte("TITLE \"unterminated\n"))
	f.Add([]byte("INDEX 01 99:59:74\n"))
	f.Add([]byte{0x93, 0x94, 0xe9, 0xff})
	f.Add([]byte(utf8BOM + "REM\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		sheet, err := Parse(b)
		if err != nil {
			if sheet != nil {
				t.Fatal("Parse returned both a sheet and an error")
			}
			return
		}
		// A sheet that parsed must be one a splitter can act on: every
		// track has a start, starts ascend strictly within a file, and no
		// start converts to a negative sample offset. Those are validate's
		// own postconditions, checked here against arbitrary input rather
		// than the fixtures.
		for fi := range sheet.Files {
			file := &sheet.Files[fi]
			first, prevStart := true, 0
			for _, tr := range file.Tracks {
				start, ok := tr.Start()
				if !ok {
					t.Fatalf("track %d parsed with no INDEX 01", tr.Number)
				}
				if !first && start <= prevStart {
					t.Fatalf("track %d starts at %d, which does not advance past %d", tr.Number, start, prevStart)
				}
				first, prevStart = false, start
				if Samples(start, 44100) < 0 {
					t.Fatalf("track %d start %d frames converts to a negative sample offset", tr.Number, start)
				}
			}
			// Cuts is the list a caller divides the file by, so hold it to
			// the rule jobs.SplitSpans holds a cut list to: refuse, or
			// return offsets that strictly ascend from past 0. Anything
			// this accepts and SplitSpans then rejects is the divergence
			// the funnel exists to prevent.
			cuts, err := file.Cuts(44100)
			if err != nil {
				continue
			}
			prev := int64(0)
			for i, c := range cuts {
				if c <= prev {
					t.Fatalf("cut %d at sample %d does not advance past %d", i, c, prev)
				}
				prev = c
			}
		}
	})
}
