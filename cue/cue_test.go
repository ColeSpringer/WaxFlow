package cue

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
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
// Starts as though it were a position in the file. Atoi cannot catch it,
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
	// On a 32-bit build that field does not fit an int and is refused as not
	// MM:SS:FF before the bound, so the bound's message is pinned on a value
	// that reaches it at either word size.
	const pastTheBound = "1000000:00:00"
	if _, err := ParseTime(pastTheBound); err == nil {
		t.Errorf("ParseTime(%q) was accepted; MM past maxMinutes cannot be a position", pastTheBound)
	} else if !strings.Contains(err.Error(), "addresses at most") {
		t.Errorf("ParseTime(%q) error = %v, want it to name the bound", pastTheBound, err)
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
//
// The bound stays in Parse rather than moving to Starts with the splitting
// invariants: a stamp the arithmetic cannot hold is not a position at all,
// so no reader of the sheet should see it, splitting or not.
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

// TestStartsMustAscend pins strict ascent, and the frame-0 case that the
// sentinel it replaced only ever handled by luck.
//
// Equal starts are the interesting half: they got past this package and
// then failed at jobs.SplitSpans, which refuses a zero-sample piece, so one
// sheet got two answers depending on which caller read it. The CLI's answer
// was an empty file written without complaint.
//
// The refusal is Starts' now rather than Parse's, and the parse in between
// is asserted rather than skipped: the sheet is readable and the arithmetic
// is what fails, which is the whole point of the split.
func TestStartsMustAscend(t *testing.T) {
	equal := "FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n    INDEX 01 01:00:00\n" +
		"  TRACK 02 AUDIO\n    INDEX 01 01:00:00\n"
	sheet, err := Parse([]byte(equal))
	if err != nil {
		t.Fatalf("Parse(two tracks sharing an INDEX 01): %v; the sheet is readable", err)
	}
	if _, err := sheet.Files[0].Starts(44100); err == nil {
		t.Fatalf("Starts accepted two tracks sharing an INDEX 01; that names a zero-sample piece")
	} else if !strings.Contains(err.Error(), "have to ascend") {
		t.Errorf("Starts error = %v, want it to name the ascending rule", err)
	} else if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("Starts code = %v, want CodeInvalidRequest", got)
	}
	// Cuts is the funnel both splitting callers use, so it has to refuse
	// what Starts refuses rather than only what it checks itself.
	if _, err := sheet.Files[0].Cuts(44100); err == nil {
		t.Errorf("Cuts accepted a file whose Starts refused")
	}

	// A first track at frame 0 is the overwhelmingly common sheet. It has no
	// predecessor to ascend from, so it has to keep parsing.
	zero := "FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n    INDEX 01 01:00:00\n"
	sheet, err = Parse([]byte(zero))
	if err != nil {
		t.Fatalf("Parse(track 1 at frame 0): %v", err)
	}
	if start, ok := sheet.Files[0].Tracks[0].Start(); !ok || start != 0 {
		t.Errorf("track 1 start = %d (%v), want frame 0", start, ok)
	}
	if _, err := sheet.Files[0].Starts(44100); err != nil {
		t.Errorf("Starts(track 1 at frame 0): %v", err)
	}
}

// TestStartsRefusesAnUnnumberedTrack: ParseTolerant opens track -1 for a
// TRACK line whose number it could not read. A split names and tags its
// pieces by number, so the funnel refuses the sheet rather than cutting it.
func TestStartsRefusesAnUnnumberedTrack(t *testing.T) {
	sheet := ParseTolerant([]byte("TRACK x AUDIO\nINDEX 01 00:00:00\nTRACK 02 AUDIO\nINDEX 01 01:00:00\n"))
	tracks := tracksOf(t, sheet)
	if len(tracks) != 2 || tracks[0].Number != -1 {
		t.Fatalf("tracks = %+v, want track -1 then 2", tracks)
	}
	f := &sheet.Files[0]
	if _, err := f.Starts(44100); err == nil || !strings.Contains(err.Error(), "track -1") {
		t.Errorf("Starts error = %v, want it to name track -1", err)
	} else if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("Starts code = %v, want CodeInvalidRequest", got)
	}
	if cuts, err := f.Cuts(44100); err == nil {
		t.Errorf("Cuts = %v, want the refusal Starts makes", cuts)
	}
	// Mid-file too, where the ascending rule has a predecessor to check.
	mid := ParseTolerant([]byte("TRACK 01 AUDIO\nINDEX 01 00:00:00\nTRACK x AUDIO\nINDEX 01 01:00:00\n" +
		"TRACK 03 AUDIO\nINDEX 01 02:00:00\n"))
	if _, err := mid.Files[0].Starts(44100); err == nil || !strings.Contains(err.Error(), "track -1") {
		t.Errorf("Starts error = %v, want it to name track -1", err)
	}
}

// TestStartsNeverIndexesBeforeTheFirstTrack holds the ascent loop to
// reporting a predecessor it actually has.
//
// The loop used to name the offender as f.Tracks[ti-1], which at ti == 0 is
// f.Tracks[-1] and a panic. ParseTime's bound removes the only way to reach
// it from wire bytes today, so this drives Starts through a hand-built
// Sheet: the loop must not be able to index -1 whatever Start returns, and
// a bound in another function is not that guarantee.
func TestStartsNeverIndexesBeforeTheFirstTrack(t *testing.T) {
	f := &File{
		Name: "a.flac",
		Tracks: []Track{
			// Near the bottom of the int range, which is what a wrapped
			// ParseTime produces. Written relative to MinInt so it is a value
			// an int holds on a 32-bit build too.
			{Number: 1, Indexes: []Index{{Number: 1, Frame: math.MinInt + 2492}}},
			{Number: 2, Indexes: []Index{{Number: 1, Frame: 4500}}},
		},
	}
	// A negative first start is what a wrapped ParseTime produced. Whatever
	// Starts makes of it, it must return rather than panic.
	if _, err := f.Starts(44100); err != nil && waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("Starts code = %v, want CodeInvalidRequest", waxerr.CodeOf(err))
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

// wantRefusal holds both parses to one finding at one line: Parse refuses
// with it, prefixed once, and it is ParseTolerant's first warning.
func wantRefusal(t *testing.T, in string, line int, msg string) *Sheet {
	t.Helper()
	sheet, err := Parse([]byte(in))
	if want := fmt.Sprintf("cue: line %d: %s", line, msg); err == nil || err.Error() != want {
		t.Errorf("Parse(%q) error = %v, want %q", in, err, want)
	}
	if sheet != nil {
		t.Errorf("Parse returned both a sheet and an error")
	}
	if err != nil && waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("code = %v, want CodeInvalidRequest", waxerr.CodeOf(err))
	}
	tol := ParseTolerant([]byte(in))
	if len(tol.Warnings) == 0 || tol.Warnings[0] != (Warning{Line: line, Msg: msg}) {
		t.Errorf("ParseTolerant warnings = %v, want line %d %q first", tol.Warnings, line, msg)
	}
	return tol
}

// tracksOf returns the tracks of the sheet's one file.
func tracksOf(t *testing.T, s *Sheet) []Track {
	t.Helper()
	if len(s.Files) != 1 {
		t.Fatalf("files = %+v, want 1", s.Files)
	}
	return s.Files[0].Tracks
}

// TestParseRejects covers what Parse itself refuses: a line it cannot read.
// What a splitter cannot use is TestStartsRejects.
func TestParseRejects(t *testing.T) {
	const file = "FILE \"a.flac\" WAVE\n"
	const track = file + "TRACK 01 AUDIO\n"
	for _, tc := range []struct {
		name, in string
		line     int
		msg      string
	}{
		{"index outside track", file + "INDEX 01 00:00:00\n", 2, "INDEX outside a TRACK"},
		{"bad frame count", track + "INDEX 01 00:00:75\n", 3, `track 1: time "00:00:75" has 75 frames; a second holds 75`},
		{"bad seconds", track + "INDEX 01 00:60:00\n", 3, `track 1: time "00:60:00" has 60 seconds; a minute holds 60`},
		{"minutes past the bound", track + "INDEX 01 6001:00:00\n", 3,
			`track 1: time "6001:00:00" has 6001 minutes; a sheet addresses at most 6000`},
		{"not a timestamp", track + "INDEX 01 nope\n", 3, `track 1: time "nope" is not MM:SS:FF`},
		// Past what an int holds, in the format's words rather than strconv's.
		{"20-digit minutes", track + "INDEX 01 99999999999999999999:00:00\n", 3,
			`track 1: time "99999999999999999999:00:00" is not MM:SS:FF`},
		{"index without a time", track + "INDEX 01\n", 3, "track 1: INDEX takes a number and a time"},
		{"index number past 99", track + "INDEX 100 00:00:00\n", 3, `track 1: INDEX number "100" is not 00 to 99`},
		{"pregap outside track", file + "PREGAP 00:00:32\n", 2, "PREGAP outside a TRACK"},
		{"pregap without a time", track + "PREGAP\n", 3, "track 1: PREGAP takes a time"},
		{"track without a number", file + "TRACK\n", 2, "TRACK takes a number"},
		{"file without a name", "FILE\n", 1, "FILE takes a name"},
		// A quote that never closes hides where a token ends, as it did before
		// any operand could run to the end of its line.
		{"quote before the command", track + "\"INDEX 01 00:00:00\n", 3, `"INDEX 01 00:00:00" is missing its closing quote`},
		{"quote in a TRACK line", file + "TRACK 01 \"AUDIO\n", 2, "TRACK is missing a closing quote"},
		{"quote in an INDEX line", track + "INDEX 01 00:00:00 \"\n", 3, "track 1: INDEX is missing a closing quote"},
		{"quote in a PREGAP line", track + "PREGAP \"00:00:32\n", 3, "track 1: PREGAP is missing a closing quote"},
		{"quote in a FLAGS line", track + "FLAGS \"DCP\n", 3, "track 1: FLAGS is missing a closing quote"},
	} {
		t.Run(tc.name, func(t *testing.T) { wantRefusal(t, tc.in, tc.line, tc.msg) })
	}
}

// TestParseOperands pins the operand grammar: a one-operand command reads
// the rest of its line, and a quote strips only as a pair around it.
func TestParseOperands(t *testing.T) {
	disc := func(line string) string {
		return line + "\nFILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"
	}
	inTrack := func(line string) string {
		return "FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    " + line + "\n    INDEX 01 00:00:00\n"
	}
	title := func(s *Sheet) string { return s.Title }
	// rem reads a key's value, or says it is absent.
	rem := func(key string) func(*Sheet) string {
		return func(s *Sheet) string {
			if v, ok := s.Rem(key); ok {
				return v
			}
			return "<absent>"
		}
	}
	for _, tc := range []struct {
		name, in string
		get      func(*Sheet) string
		want     string
	}{
		{"unquoted words", disc("TITLE Jazz Album"), title, "Jazz Album"},
		{"inner spacing", disc("TITLE Jazz   Album"), title, "Jazz   Album"},
		{"quoted", disc(`TITLE "Jazz Album"`), title, "Jazz Album"},
		{"inner quotes", disc(`TITLE "The "Best" Of"`), title, `The "Best" Of`},
		{"unterminated quote", disc(`TITLE "Unclosed`), title, "Unclosed"},
		{"token after the quote", disc(`TITLE "Foo" bar`), title, "Foo"},
		{"bare", disc("TITLE"), title, ""},
		{"empty quotes", disc(`TITLE ""`), title, ""},
		{"lone quote", disc(`TITLE "`), title, ""},
		{"quoted, trailing blanks", disc("TITLE \"Foo\"  \t"), title, "Foo"},
		{"unquoted, trailing blanks", disc("TITLE Foo  "), title, "Foo"},
		{"inch mark", disc(`TITLE 12" Remix`), title, `12" Remix`},
		{"tab separated", disc("TITLE\tTabbed Title"), title, "Tabbed Title"},
		{"crlf", disc("TITLE \"Foo\"\r"), title, "Foo"},
		{"track performer", inTrack("PERFORMER Guest Artist"),
			func(s *Sheet) string { return s.Files[0].Tracks[0].Performer }, "Guest Artist"},
		{"track isrc", inTrack("ISRC ABCDE1234567"),
			func(s *Sheet) string { return s.Files[0].Tracks[0].ISRC }, "ABCDE1234567"},
		{"catalog", disc("CATALOG 1234567890123"), func(s *Sheet) string { return s.Catalog }, "1234567890123"},
		{"rem value", disc("REM COMMENT Ripped with EAC"), rem("COMMENT"), "Ripped with EAC"},
		{"rem quoted", disc(`REM GENRE "Rock"`), rem("GENRE"), "Rock"},
		{"rem unterminated", disc(`REM COMMENT "Ripped`), rem("COMMENT"), "Ripped"},
		{"rem key alone", disc("REM DATE"), rem("DATE"), ""},
		{"bare rem", disc("REM"), func(s *Sheet) string { return fmt.Sprint(len(s.Rems)) }, "0"},
		// A command this skips is not read, so its quotes cannot refuse.
		{"skipped command", disc(`SONGWRITER "Unclosed`), title, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sheet, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got := tc.get(sheet); got != tc.want {
				t.Errorf("Parse(%q) read %q, want %q", tc.in, got, tc.want)
			}
			if sheet.Warnings != nil {
				t.Errorf("Warnings = %v, want nil", sheet.Warnings)
			}
			if tol := ParseTolerant([]byte(tc.in)); !reflect.DeepEqual(tol, sheet) {
				t.Errorf("ParseTolerant = %+v, Parse = %+v", tol, sheet)
			}
		})
	}
}

// TestParseFile pins the FILE operands. A quoted name closes at its first
// quote unless that splits a name with quotes of its own; an unquoted one may
// hold spaces; the type is optional.
func TestParseFile(t *testing.T) {
	for _, tc := range []struct{ line, name, typ, msg string }{
		{`FILE "a.flac" WAVE`, "a.flac", "WAVE", ""},
		{`FILE "a.flac"`, "a.flac", "", ""},
		{`FILE "a.flac" wave`, "a.flac", "WAVE", ""},
		{`FILE "a.flac" WAVE extra`, "a.flac", "WAVE", ""},
		{`FILE a.flac WAVE`, "a.flac", "WAVE", ""},
		{`FILE my album.flac WAVE`, "my album.flac", "WAVE", ""},
		{`FILE my album.flac`, "my album.flac", "", ""},
		{`FILE a.flac`, "a.flac", "", ""},
		// WaxBin's prepend: an empty quoted name is a name, not a missing one.
		{`FILE "" WAVE`, "", "WAVE", ""},
		{"FILE\t\"a.flac\"\tWAVE", "a.flac", "WAVE", ""},
		{`FILE`, "", "", "FILE takes a name"},
		{`FILE "unclosed.flac WAVE`, "unclosed.flac WAVE", "", "FILE name is missing its closing quote"},
		// Quotes inside the name.
		{`FILE "The "Best" Of.flac" WAVE`, `The "Best" Of.flac`, "WAVE", ""},
		{`FILE "12" Single.flac" WAVE`, `12" Single.flac`, "WAVE", ""},
		{`FILE "Album ""Live"".flac" WAVE`, `Album ""Live"".flac`, "WAVE", ""},
		// Quotes after the name.
		{`FILE "a.wav" "WAVE"`, "a.wav", "WAVE", ""},
		{`FILE "a.wav" WAVE "comment"`, "a.wav", "WAVE", ""},
		{`FILE "a.wav" "WAVE`, "a.wav", "WAVE", "FILE is missing a closing quote"},
		{`FILE "a.wav" WAVE "`, "a.wav", "WAVE", "FILE is missing a closing quote"},
		{`FILE "a" "`, "a", "", "FILE is missing a closing quote"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			in := tc.line + "\n"
			var tol *Sheet
			if tc.msg != "" {
				tol = wantRefusal(t, in, 1, tc.msg)
			} else {
				if _, err := Parse([]byte(in)); err != nil {
					t.Fatalf("Parse(%q): %v", in, err)
				}
				tol = ParseTolerant([]byte(in))
				if tol.Warnings != nil {
					t.Errorf("Warnings = %v, want nil", tol.Warnings)
				}
			}
			// A FILE line read best effort still opens a file, so the tracks
			// under it keep their own positions.
			if len(tol.Files) != 1 {
				t.Fatalf("ParseTolerant(%q) files = %d, want 1", in, len(tol.Files))
			}
			if f := tol.Files[0]; f.Name != tc.name || f.Type != tc.typ {
				t.Errorf("file = %q %q, want %q %q", f.Name, f.Type, tc.name, tc.typ)
			}
		})
	}
}

// sheetNoFile is a chapter sheet's shape: tracks and no FILE line, which a
// sidecar named after its one rip routinely omits.
const sheetNoFile = "TITLE \"No File Line\"\n" +
	"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
	"  TRACK 02 AUDIO\n    INDEX 01 01:00:00\n"

// TestParseImpliedFile: a TRACK before any FILE opens a file with no name,
// which a splitter cuts like any other.
func TestParseImpliedFile(t *testing.T) {
	// With a BOM too, which is where a prepended FILE line used to land.
	for _, in := range []string{sheetNoFile, utf8BOM + sheetNoFile} {
		sheet, err := Parse([]byte(in))
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if sheet.Title != "No File Line" {
			t.Errorf("title = %q", sheet.Title)
		}
		tracks := tracksOf(t, sheet)
		if f := sheet.Files[0]; f.Name != "" || f.Type != "" {
			t.Errorf("file = %q %q, want the implied file's empty name and type", f.Name, f.Type)
		}
		if len(tracks) != 2 || tracks[0].Number != 1 || tracks[1].Number != 2 {
			t.Fatalf("tracks = %+v, want 1 and 2", tracks)
		}
		single, err := sheet.SingleFile()
		if err != nil {
			t.Fatalf("SingleFile: %v", err)
		}
		// 01:00:00 is 4500 frames, 588 samples each at 44100.
		if cuts, err := single.Cuts(44100); err != nil || !slices.Equal(cuts, []int64{2646000}) {
			t.Errorf("Cuts(44100) = %v, %v; want [2646000]", cuts, err)
		}
	}

	// A FILE line closes the implied file like any other.
	late := "TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
		"FILE \"b.flac\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"
	sheet, err := Parse([]byte(late))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(sheet.Files) != 2 || sheet.Files[0].Name != "" || sheet.Files[1].Name != "b.flac" {
		t.Fatalf("files = %+v, want the implied file then b.flac", sheet.Files)
	}
	if _, err := sheet.SingleFile(); err == nil || !strings.Contains(err.Error(), "indexes 2 files") {
		t.Errorf("SingleFile error = %v, want it to count 2 files", err)
	}

	// A refusal naming the file says which one.
	for _, tc := range []struct{ in, want string }{
		{"TRACK 01 AUDIO\n  INDEX 01 00:00:00\n", "the implied file names one track"},
		{"FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\n  INDEX 01 00:00:00\n", `file "a.flac" names one track`},
	} {
		sheet, err := Parse([]byte(tc.in))
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if _, err := sheet.Files[0].Cuts(44100); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Cuts error = %v, want it to mention %q", err, tc.want)
		}
	}
}

// TestParseClassicMacLineEnds: a sheet saved on classic Mac OS ends its lines
// with CR alone, and still reads line by line.
func TestParseClassicMacLineEnds(t *testing.T) {
	sheet, err := Parse([]byte(strings.ReplaceAll(sheetNoFile, "\n", "\r")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sheet.Title != "No File Line" {
		t.Errorf("title = %q", sheet.Title)
	}
	if tracks := tracksOf(t, sheet); len(tracks) != 2 {
		t.Errorf("tracks = %+v, want 2", tracks)
	}
	wantRefusal(t, "FILE \"a.flac\" WAVE\rTRACK 01 AUDIO\rINDEX 01 nope\r", 3, `track 1: time "nope" is not MM:SS:FF`)
}

// TestParseTrackLine: a TRACK number is digits and nothing else. Read
// tolerantly, a line with no readable number still opens a track, numbered
// -1, so the lines under it are not read as the previous track's.
func TestParseTrackLine(t *testing.T) {
	tol := wantRefusal(t, "FILE \"a.flac\" WAVE\n  TRACK AUDIO\n    TITLE \"Orphan\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 01:00:00\n", 2, "TRACK takes a number")
	if len(tol.Warnings) != 1 {
		t.Errorf("warnings = %v, want one", tol.Warnings)
	}
	tracks := tracksOf(t, tol)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %+v, want 2", tracks)
	}
	// A lone datatype is the type, since it is what marks a data track.
	if want := (Track{Number: -1, Type: "AUDIO", Title: "Orphan", Indexes: []Index{{Number: 1, Frame: 0}}}); !reflect.DeepEqual(tracks[0], want) {
		t.Errorf("track 0 = %+v, want %+v", tracks[0], want)
	}
	if tracks[1].Number != 2 {
		t.Errorf("track 1 number = %d, want 2", tracks[1].Number)
	}
	if tr := tracksOf(t, wantRefusal(t, "FILE \"a.bin\" BINARY\nTRACK MODE1/2352\n", 2, "TRACK takes a number")); len(tr) != 1 || tr[0].Type != "MODE1/2352" {
		t.Errorf("tracks = %+v, want one typed MODE1/2352", tr)
	}

	// Atoi alone would take a sign.
	for _, tc := range []struct{ line, msg string }{
		{"TRACK 1A AUDIO", `TRACK number "1A" is not a number`},
		{"TRACK -1 AUDIO", `TRACK number "-1" is not a number`},
		{"TRACK +1 AUDIO", `TRACK number "+1" is not a number`},
		{"TRACK 99999999999999999999 AUDIO", `TRACK number "99999999999999999999" is not a number`},
		{"TRACK", "TRACK takes a number"},
	} {
		wantRefusal(t, "FILE \"a.flac\" WAVE\n"+tc.line+"\n", 2, tc.msg)
	}

	for _, tc := range []struct {
		line string
		num  int
		typ  string
	}{
		// TRACK 00 is a number a sheet can write.
		{"TRACK 00 AUDIO", 0, "AUDIO"},
		{"TRACK 01", 1, ""},
	} {
		sheet, err := Parse([]byte("FILE \"a.flac\" WAVE\n" + tc.line + "\n"))
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.line, err)
			continue
		}
		if tr := sheet.Files[0].Tracks[0]; tr.Number != tc.num || tr.Type != tc.typ {
			t.Errorf("Parse(%q) track = %d %q, want %d %q", tc.line, tr.Number, tr.Type, tc.num, tc.typ)
		}
	}
}

// sheetMisspelledTrack loses track 2 to a typo: TRCK is skipped as an
// unknown command, so its TITLE and INDEX land on track 1.
const sheetMisspelledTrack = "FILE \"a.flac\" WAVE\n" +
	"  TRACK 01 AUDIO\n    TITLE \"A\"\n    INDEX 01 00:00:00\n" +
	"  TRCK 02 AUDIO\n    TITLE \"B\"\n    INDEX 01 03:00:00\n" +
	"  TRACK 03 AUDIO\n    TITLE \"C\"\n    INDEX 01 06:00:00\n"

// TestParseIndexOrder: INDEX numbers ascend within a track, so one that does
// not is the trace of a TRACK line this never read, which would otherwise
// merge two tracks. The finding names a skipped line shaped like a TRACK.
func TestParseIndexOrder(t *testing.T) {
	const head = "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\n"
	const missing = "the TRACK line between the two is missing or misspelled"
	for _, tc := range []struct {
		name, in string
		line     int
		msg      string
	}{
		{"misspelled TRACK", sheetMisspelledTrack, 7,
			`track 1: INDEX 01 repeats; line 5 "TRCK" is not a command this reads`},
		// The swallowed track's INDEX 00 is the first to break the order.
		{"pregap of a swallowed track", head + "INDEX 01 00:00:00\ntrck 02 AUDIO\nTITLE \"B\"\n" +
			"SONGWRITER \"S\"\nINDEX 00 02:58:00\nINDEX 01 03:00:00\n", 7,
			`track 1: INDEX 00 follows INDEX 01; line 4 "trck" is not a command this reads`},
		// SONGWRITER and CATALOG are the format's own commands, never a
		// misspelled TRACK, wherever a sheet puts them.
		{"songwriter after the INDEX lines", head + "INDEX 01 00:00:00\nSONGWRITER \"S\"\n" +
			"TRCK 02 AUDIO\nINDEX 01 03:00:00\n", 6,
			`track 1: INDEX 01 repeats; line 5 "TRCK" is not a command this reads`},
		{"catalog in a track", head + "INDEX 01 00:00:00\nCATALOG 12\nINDEX 01 03:00:00\n", 5,
			"track 1: INDEX 01 repeats; " + missing},
		// A line that does not look like a TRACK is not named.
		{"vendor line", head + "INDEX 01 00:00:00\nX-VENDOR foo\nINDEX 01 03:00:00\n", 5,
			"track 1: INDEX 01 repeats; " + missing},
		{"vendor line then TRCK", head + "INDEX 01 00:00:00\nX-VENDOR foo\nTRCK 02 AUDIO\nINDEX 01 03:00:00\n", 6,
			`track 1: INDEX 01 repeats; line 5 "TRCK" is not a command this reads`},
		{"empty keyword", head + "INDEX 01 00:00:00\n\"\" TRACK 02 AUDIO\nINDEX 01 03:00:00\n", 5,
			"track 1: INDEX 01 repeats; line 4 is not a command this reads"},
		// A lookalike letter is escaped, so the message cannot read TRACK.
		{"lookalike keyword", head + "INDEX 01 00:00:00\nTR\u0410CK 02 AUDIO\nINDEX 01 03:00:00\n", 5,
			`track 1: INDEX 01 repeats; line 4 "TR\u0410CK" is not a command this reads`},
		{"nothing between", head + "INDEX 01 00:00:00\nINDEX 01 00:01:00\n", 4,
			"track 1: INDEX 01 repeats; " + missing},
		// An accepted INDEX starts the search again.
		{"skipped line before both", head + "TRCK 02 AUDIO\nINDEX 01 00:00:00\nINDEX 01 00:01:00\n", 5,
			"track 1: INDEX 01 repeats; " + missing},
	} {
		t.Run(tc.name, func(t *testing.T) { wantRefusal(t, tc.in, tc.line, tc.msg) })
	}

	if _, err := Parse([]byte(head + "INDEX 00 00:00:00\nINDEX 01 00:02:00\nINDEX 02 00:04:00\n")); err != nil {
		t.Errorf("Parse(ascending indexes): %v", err)
	}

	// Read tolerantly, the first INDEX 01 stands and the overwrite the
	// warning exists to expose is visible: track 1 carries track 2's title.
	tracks := tracksOf(t, ParseTolerant([]byte(sheetMisspelledTrack)))
	if len(tracks) != 2 || tracks[0].Number != 1 || tracks[1].Number != 3 {
		t.Fatalf("tracks = %+v, want 1 and 3", tracks)
	}
	if tracks[0].Title != "B" {
		t.Errorf("track 1 title = %q, want B", tracks[0].Title)
	}
	if want := []Index{{Number: 1, Frame: 0}}; !reflect.DeepEqual(tracks[0].Indexes, want) {
		t.Errorf("track 1 indexes = %v, want %v", tracks[0].Indexes, want)
	}
}

// TestParseOutsideATrack: a line that needs a track and has none names the
// TRACK-shaped line skipped since the last FILE line.
func TestParseOutsideATrack(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		line     int
		msg      string
	}{
		{"misspelled first TRACK", "FILE \"a.wav\" WAVE\nTRCK 01 AUDIO\nINDEX 01 00:00:00\n", 3,
			`INDEX outside a TRACK; line 2 "TRCK" is not a command this reads`},
		// A FILE line closes the open track, so the search starts again.
		{"after a later FILE line", "FILE \"a.wav\" WAVE\nTRACK 01 AUDIO\nINDEX 01 00:00:00\n" +
			"FILE \"b.wav\" WAVE\nTRCK 02 AUDIO\nPREGAP 00:02:00\n", 6,
			`PREGAP outside a TRACK; line 5 "TRCK" is not a command this reads`},
		{"skipped before the FILE line", "TRCK 01 AUDIO\nFILE \"a.wav\" WAVE\nINDEX 01 00:00:00\n", 3,
			"INDEX outside a TRACK"},
		{"vendor line", "FILE \"a.wav\" WAVE\nX-VENDOR foo\nINDEX 01 00:00:00\n", 3, "INDEX outside a TRACK"},
	} {
		t.Run(tc.name, func(t *testing.T) { wantRefusal(t, tc.in, tc.line, tc.msg) })
	}
}

// TestParseTolerantKeepsTheRest: one bad timestamp costs its line, not the
// sheet.
func TestParseTolerantKeepsTheRest(t *testing.T) {
	tol := wantRefusal(t, "TITLE \"X\"\nFILE \"album.mp3\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"A\"\n    INDEX 01 00:99:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"B\"\n    INDEX 01 00:00:10\n",
		5, `track 1: time "00:99:00" has 99 seconds; a minute holds 60`)
	if len(tol.Warnings) != 1 {
		t.Errorf("warnings = %v, want one", tol.Warnings)
	}
	if tol.Title != "X" {
		t.Errorf("title = %q, want X", tol.Title)
	}
	tracks := tracksOf(t, tol)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %+v, want 2", tracks)
	}
	if tracks[0].Title != "A" {
		t.Errorf("track 1 title = %q, want A", tracks[0].Title)
	}
	if start, ok := tracks[0].Start(); ok {
		t.Errorf("track 1 start = %d, want none: its INDEX 01 was the line dropped", start)
	}
	if start, ok := tracks[1].Start(); !ok || start != 10 {
		t.Errorf("track 2 start = %d (%v), want frame 10", start, ok)
	}
}

// TestParseStrictStopsAtTheFirstFinding: Parse names the first line it
// cannot read and reads no further; ParseTolerant lists every one.
func TestParseStrictStopsAtTheFirstFinding(t *testing.T) {
	tol := wantRefusal(t, "FILE \"a.flac\" WAVE\n  PREGAP\n  TRACK 01 AUDIO\n    INDEX 01 nope\n"+
		"    INDEX 01 00:00:00\n    INDEX 100 00:00:01\n", 2, "PREGAP outside a TRACK")
	want := []Warning{
		{Line: 2, Msg: "PREGAP outside a TRACK"},
		{Line: 4, Msg: `track 1: time "nope" is not MM:SS:FF`},
		{Line: 6, Msg: `track 1: INDEX number "100" is not 00 to 99`},
	}
	if !reflect.DeepEqual(tol.Warnings, want) {
		t.Errorf("Warnings = %v, want %v", tol.Warnings, want)
	}
	if tracks := tracksOf(t, tol); len(tracks) != 1 || !reflect.DeepEqual(tracks[0].Indexes, []Index{{Number: 1, Frame: 0}}) {
		t.Errorf("tracks = %+v, want one with INDEX 01 at 0", tracks)
	}

	// A thousand tracks after a first line it cannot read cost Parse nothing.
	bad := []byte("INDEX 01 00:00:00\n" + strings.Repeat("TRACK 01 AUDIO\nINDEX 01 00:00:00\n", 1000))
	if allocs := testing.AllocsPerRun(5, func() { _, _ = Parse(bad) }); allocs > 50 {
		t.Errorf("Parse allocated %.0f times over a sheet refused at line 1, want it to stop there", allocs)
	}

	// A clean sheet reads the same both ways.
	strict, err := Parse([]byte(sheetBasic))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tolerant := ParseTolerant([]byte(sheetBasic))
	if strict.Warnings != nil || tolerant.Warnings != nil {
		t.Errorf("Warnings = %v and %v, want nil", strict.Warnings, tolerant.Warnings)
	}
	if !reflect.DeepEqual(strict, tolerant) {
		t.Errorf("ParseTolerant = %+v, Parse = %+v", tolerant, strict)
	}
}

// TestParseClipsOperands: an operand quoted into a message is clipped on a
// rune boundary, so a hostile token cannot make a message a megabyte.
func TestParseClipsOperands(t *testing.T) {
	// A 3-byte rune straddles the 32-byte clip.
	token := strings.Repeat("x", 31) + "€" + strings.Repeat("y", 1<<20)
	tol := ParseTolerant([]byte("FILE \"a.flac\" WAVE\nTRACK " + token + " AUDIO\n"))
	want := []Warning{{Line: 2, Msg: `TRACK number "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx..." is not a number`}}
	if !reflect.DeepEqual(tol.Warnings, want) {
		got := fmt.Sprint(tol.Warnings)
		if len(got) > 200 {
			got = got[:200] + "..."
		}
		t.Errorf("Warnings = %s, want %v", got, want)
	}

	// Public ParseTime takes any string. Bytes that are not UTF-8 have no
	// rune boundary to back up to, so they are cut where the bound falls.
	_, err := ParseTime(strings.Repeat("\x80", 40))
	if want := `cue: time "` + strings.Repeat(`\x80`, 32) + `..." is not MM:SS:FF`; err == nil || err.Error() != want {
		t.Errorf("ParseTime error = %v, want %q", err, want)
	}

	// A file's name is clipped at a filename's own length.
	sheet, err := Parse([]byte("FILE \"" + strings.Repeat("n", 1<<20) + "\" WAVE\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = sheet.Files[0].Starts(44100)
	if want := `cue: file "` + strings.Repeat("n", 255) + `..." names no tracks`; err == nil || err.Error() != want {
		t.Errorf("Starts error is %d bytes, want the %d of %q", len(fmt.Sprint(err)), len(want), want[:40]+"...")
	}
}

// TestParseWarningsCap: the list stops at maxWarnings, and the reading does
// not.
func TestParseWarningsCap(t *testing.T) {
	in := []byte(strings.Repeat("INDEX 01 00:00:00\n", 100) + "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 00:00:30\n")
	tol := ParseTolerant(in)
	if len(tol.Warnings) != 64 {
		t.Fatalf("%d warnings, want 64", len(tol.Warnings))
	}
	if tol.Warnings[0].Line != 1 || tol.Warnings[63].Line != 64 {
		t.Errorf("warnings run from line %d to %d, want 1 to 64", tol.Warnings[0].Line, tol.Warnings[63].Line)
	}
	if tracks := tracksOf(t, tol); len(tracks) != 1 || tracks[0].Indexes[0].Frame != 30 {
		t.Errorf("tracks = %+v, want the track after the cap read", tracks)
	}
	if _, err := Parse(in); err == nil || !strings.HasPrefix(err.Error(), "cue: line 1: ") {
		t.Errorf("Parse error = %v, want line 1's", err)
	}
}

// TestStartsRejects covers the other half of the split: a sheet Parse reads
// and a splitter cannot use.
//
// Each cell asserts both halves, because either one alone would pass while
// the change that moved the check was half made: a refusal at Parse would
// satisfy a Starts-only assertion by never getting there, and a check
// deleted outright would satisfy a Parse-only one.
func TestStartsRejects(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no index 01", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 00 00:00:00\n", "no INDEX 01"},
		{"descending tracks", "FILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nINDEX 01 05:00:00\nTRACK 02 AUDIO\nINDEX 01 01:00:00\n", "have to ascend"},
		{"no tracks", "FILE \"a.flac\" WAVE\n", "names no tracks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sheet, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse(%q) = %v; this is a sheet, just not a splittable one", tc.in, err)
			}
			_, err = sheet.Files[0].Starts(44100)
			if err == nil {
				t.Fatalf("Starts succeeded, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Starts error = %v, want it to mention %q", err, tc.want)
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
				t.Errorf("Starts code = %v, want CodeInvalidRequest", got)
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
		"    FLAGS DCP  4CH\n" +
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
	if !slices.Equal(tr.Flags, []string{"DCP", "4CH"}) {
		t.Errorf("flags = %v, want [DCP 4CH]", tr.Flags)
	}
}

// TestRem covers the keys WaxBin reads out of a sheet: REM is the format's
// comment command and its only extension point, so the disc metadata CUE
// has no command for (GENRE, DATE) is written there and nowhere else.
func TestRem(t *testing.T) {
	sheet, err := Parse([]byte(sheetBasic))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, tc := range []struct{ key, want string }{
		{"GENRE", "Rock"},
		{"DATE", "1997"},
		// Rippers disagree about case, and a reader asking for a key it
		// knows should not have to guess which one wrote the sheet.
		{"genre", "Rock"},
		{"Date", "1997"},
	} {
		got, ok := sheet.Rem(tc.key)
		if !ok || got != tc.want {
			t.Errorf("sheet.Rem(%q) = %q, %v; want %q, true", tc.key, got, ok, tc.want)
		}
	}
	if got, ok := sheet.Rem("REPLAYGAIN_ALBUM_GAIN"); ok {
		t.Errorf("sheet.Rem of an absent key = %q, true", got)
	}
	// The quotes are the sheet's syntax, not part of the value, and a
	// multi-token value keeps its spacing: both are what a reader compares
	// against.
	if got, _ := sheet.Rem("GENRE"); strings.Contains(got, `"`) {
		t.Errorf("sheet.Rem(GENRE) = %q, want the quotes stripped", got)
	}
	if len(sheet.Rems) != 2 {
		t.Errorf("sheet.Rems = %v, want the two lines before the first TRACK", sheet.Rems)
	}
}

// TestRemLevels pins which level a REM lands on, which decides whether an
// album ReplayGain value can be read as a track's.
//
// The keys below differ by one word and are both real: every ripper that
// writes ReplayGain writes both, the album value at the sheet level and the
// track value inside the track, so folding the levels together would let
// one be answered with the other.
func TestRemLevels(t *testing.T) {
	in := "REM GENRE \"Rock\"\n" +
		"REM REPLAYGAIN_ALBUM_GAIN -7.11 dB\n" +
		"FILE \"a.flac\" WAVE\n" +
		"  TRACK 01 AUDIO\n" +
		"    REM REPLAYGAIN_TRACK_GAIN -8.24 dB\n" +
		"    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n" +
		"    REM\n" +
		"    INDEX 01 01:00:00\n"
	sheet, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// A REM after a FILE but before its first TRACK still has no track
	// open, so the sheet level is where the pre-track lines go and nowhere
	// else: the album gain must not be visible as track 1's.
	if got, ok := sheet.Rem("REPLAYGAIN_ALBUM_GAIN"); !ok || got != "-7.11 dB" {
		t.Errorf("sheet.Rem(album gain) = %q, %v; want %q", got, ok, "-7.11 dB")
	}
	if got, ok := sheet.Rem("REPLAYGAIN_TRACK_GAIN"); ok {
		t.Errorf("sheet.Rem(track gain) = %q, true; a track REM is not the sheet's", got)
	}
	tr := sheet.Files[0].Tracks[0]
	if got, ok := tr.Rem("REPLAYGAIN_TRACK_GAIN"); !ok || got != "-8.24 dB" {
		t.Errorf("track 1 Rem(track gain) = %q, %v; want %q", got, ok, "-8.24 dB")
	}
	if got, ok := tr.Rem("GENRE"); ok {
		t.Errorf("track 1 Rem(GENRE) = %q, true; a sheet REM is not a track's", got)
	}
	// A REM between one FILE and the next file's first TRACK also has no
	// track open, so it lands on the sheet. Pinned because the doc says so
	// rather than because it is desirable: on a multi-FILE sheet a per-file
	// key is answered as the disc's, and first-match ordering is what keeps
	// that from overriding a real disc-level line.
	multi := "REM DATE 1997\n" +
		"FILE \"01.flac\" WAVE\n" +
		"  REM DATE 2003\n" +
		"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"
	ms, err := Parse([]byte(multi))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, _ := ms.Rem("DATE"); got != "1997" {
		t.Errorf("sheet.Rem(DATE) = %q, want the disc's 1997 ahead of the file's", got)
	}
	if len(ms.Rems) != 2 {
		t.Errorf("sheet.Rems = %v, want both lines read with no track open", ms.Rems)
	}
	if got := ms.Files[0].Tracks[0].Rems; len(got) != 0 {
		t.Errorf("track 1 Rems = %v; a line before TRACK is not the track's", got)
	}

	// A bare REM is a blank comment line. It carries no key, so it is
	// dropped rather than stored as one with an empty name that Rem("")
	// would then answer.
	if got := sheet.Files[0].Tracks[1].Rems; len(got) != 0 {
		t.Errorf("track 2 Rems = %v, want none from a bare REM", got)
	}
	if got, ok := sheet.Files[0].Tracks[1].Rem(""); ok {
		t.Errorf("track 2 Rem(\"\") = %q, true", got)
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
	f.Add([]byte(sheetNoFile))
	f.Add([]byte(sheetMisspelledTrack))
	f.Add([]byte(strings.Repeat("INDEX 01 00:00:00\n", 100)))
	f.Add([]byte("FILE\n"))
	f.Add([]byte("TRACK AUDIO\nTITLE x\n"))
	f.Add([]byte("TITLE Jazz Album\n"))
	f.Add([]byte("TRACK cue: AUDIO\n"))
	f.Add([]byte(strings.ReplaceAll(sheetNoFile, "\n", "\r")))
	f.Add([]byte("FILE \"a\" \"\nTRACK 01 \"AUDIO\n\"INDEX 01 00:00:00\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		strict, err := Parse(b)
		sheet := ParseTolerant(b)
		if sheet == nil {
			t.Fatal("ParseTolerant returned nil")
		}
		// One grammar, two policies: Parse refuses exactly when ParseTolerant
		// warns, with its first warning. Compared whole, since an operand in
		// the message may say cue: itself.
		if (err != nil) != (len(sheet.Warnings) > 0) {
			t.Fatalf("Parse error %v, ParseTolerant warnings %v", err, sheet.Warnings)
		}
		if err != nil {
			if strict != nil {
				t.Fatal("Parse returned both a sheet and an error")
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
				t.Fatalf("code = %v, want CodeInvalidRequest", got)
			}
			w := sheet.Warnings[0]
			if want := fmt.Sprintf("cue: line %d: %s", w.Line, w.Msg); err.Error() != want {
				t.Fatalf("Parse error = %q, want %q", err, want)
			}
		} else {
			if strict.Warnings != nil {
				t.Fatalf("Parse returned warnings %v", strict.Warnings)
			}
			if !reflect.DeepEqual(strict, sheet) {
				t.Fatalf("ParseTolerant = %+v, Parse = %+v", sheet, strict)
			}
		}
		if len(sheet.Warnings) > maxWarnings {
			t.Fatalf("%d warnings, past the cap of %d", len(sheet.Warnings), maxWarnings)
		}
		// Decoding never adds or removes a line end, so a warning's line is
		// one the input has, and one line says at most one thing. A sheet with
		// no LF ends its lines with CR.
		ends := bytes.Count(b, []byte("\n"))
		if ends == 0 {
			ends = bytes.Count(b, []byte("\r"))
		}
		prev := 0
		for _, w := range sheet.Warnings {
			if w.Line <= prev || w.Line > ends+1 {
				t.Fatalf("warning at line %d after line %d, in %d lines", w.Line, prev, ends+1)
			}
			// A finding never starts with an operand, so this sees a doubled
			// prefix that the whole-message comparison above cannot.
			if w.Msg == "" || strings.HasPrefix(w.Msg, "cue:") {
				t.Fatalf("warning at line %d says %q", w.Line, w.Msg)
			}
			prev = w.Line
		}
		for _, file := range sheet.Files {
			for _, tr := range file.Tracks {
				if tr.Number < -1 || (tr.Number == -1 && len(sheet.Warnings) == 0) {
					t.Fatalf("track numbered %d, with warnings %v", tr.Number, sheet.Warnings)
				}
			}
		}

		// Starts' postcondition is an either-or: it refuses a file or returns
		// offsets a splitter can act on. Checked on the tolerant read, which
		// reaches every shape Parse does and more.
		//
		// A start that converts to a negative sample offset is the shape the
		// ParseTime bound exists to stop, and it stays asserted here on the
		// accepted path, since a negative that ascends from a more negative
		// one would satisfy the ordering rule alone.
		for fi := range sheet.Files {
			file := &sheet.Files[fi]
			starts, err := file.Starts(44100)
			if err == nil {
				first, prevStart := true, int64(0)
				for i, start := range starts {
					if !first && start <= prevStart {
						t.Fatalf("track %d starts at sample %d, which does not advance past %d",
							file.Tracks[i].Number, start, prevStart)
					}
					first, prevStart = false, start
					if start < 0 {
						t.Fatalf("track %d converts to sample offset %d", file.Tracks[i].Number, start)
					}
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
