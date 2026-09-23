// Package cue parses CUE sheets, the sidecar index that pairs a
// single-file CD rip with the track boundaries the disc itself had.
//
// Public since 2026-09, having been internal first: ADR-0002 makes the
// public surface a promise, and internal to public is a move we can make
// later while the reverse breaks callers. It is not container/cue, since
// that namespace means "things with a Demuxer" and a CUE sheet is an
// index alongside the audio rather than a wrapper around it; it must
// never enter format's magic-byte driver table.
//
// Parse refuses a sheet at the first line it cannot read, since a skipped
// line is a silently wrong cut; ParseTolerant reads past it, for a reader.
// Strict is the default here, unlike container, as a sheet's damage moves a cut.
//
// A sheet that parses may still not cut: File.Starts holds the splitting
// invariants and Cuts calls it, so a reader is not held to them.
//
// Positions are CD frames throughout, never time.Duration. Samples says
// why.
package cue

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/waxerr"
)

// FramesPerSecond is the CD frame rate. A frame is 1/75 s: the disc's own
// addressing quantum, and the unit every MM:SS:FF in a sheet is written
// in.
const FramesPerSecond = 75

// Sheet is a parsed CUE sheet: the disc-level metadata and the files it
// indexes.
//
// The metadata kept here is the metadata something reads. A sheet carries
// more (SONGWRITER, CDTEXTFILE), and the rest is skipped rather than
// stored: nothing in this program writes a sheet back out, so an unread
// field is not a round trip being preserved, it is a field. WaxLabel owns
// tags.
type Sheet struct {
	Catalog   string
	Title     string
	Performer string
	// Rems are the REM lines read while no track was open, in the order the
	// sheet gave them; Rem looks one up. That is every line before the first
	// TRACK, which is where a ripper writes disc metadata, and also any line
	// between one FILE and the next file's first TRACK. The two are not
	// distinguished, so on a multi-FILE sheet a per-file REM is answered as
	// the disc's. First-match ordering makes that harmless where the disc
	// wrote the key too, since its lines come first; a sheet that writes a
	// key only per file is the case to know about.
	Rems  []Rem
	Files []File
	// Warnings are the lines ParseTolerant could not read, in line order,
	// capped at maxWarnings. Nil after Parse, and after ParseTolerant of a
	// sheet Parse accepts.
	Warnings []Warning
}

// Warning is one line ParseTolerant could not read.
type Warning struct {
	// Line is 1-based over the decoded text, which has the bytes' lines.
	Line int
	// Msg is the finding as Parse's error spells it after "cue: line N: ".
	Msg string
}

// maxWarnings caps Sheet.Warnings, as the containers cap theirs: one
// finding per unread line has no other bound.
const maxWarnings = 64

// Rem is one REM line: its first token and the rest of the line.
//
// REM is the format's comment command and its extension point at once.
// Rippers put disc metadata there that the format gives no command of its
// own (GENRE and DATE are the two every ripper writes, ReplayGain the next
// most common), beside lines that really are comments, and nothing in a
// sheet distinguishes the two uses. So the lines are kept as read rather
// than mapped onto fields, and a reader decides which keys it trusts.
type Rem struct {
	// Key is the first token after REM, as the sheet spelled it. Rem
	// compares it case-insensitively, since rippers disagree about case.
	Key string
	// Value is the rest of the line after the key, trimmed, and a quoted
	// value without its quotes. A REM with nothing after its key has the
	// empty string, which is why Rem reports presence separately.
	Value string
}

// File is one FILE statement and the tracks indexed against it. Positions
// inside it are relative to that file's own start, so a track-per-file
// sheet has each track at frame 0 of its own file.
type File struct {
	// Name is the referenced audio file, as the sheet spells it. It is a
	// sheet-relative name and is not resolved or validated here. It is empty
	// for the implied file a TRACK before any FILE opens.
	Name string
	// Type is the FILE type token (WAVE, MP3, AIFF, BINARY, MOTOROLA).
	Type   string
	Tracks []Track
}

// Track is one TRACK statement.
type Track struct {
	// Number is the TRACK number as written. It is the disc's numbering,
	// not an index into Tracks: a sheet is free to start at 2 or skip.
	// ParseTolerant writes -1, which no sheet can, for one it cannot read.
	Number int
	// Type is the TRACK datatype token (AUDIO, MODE1/2352, and the other
	// data modes). It is the only thing in a sheet that says a track is not
	// audio, which a mixed-mode disc's first track routinely is not, and
	// cutting a data track as audio yields a piece of filesystem named after
	// a song. Nothing refuses that yet.
	Type      string
	Title     string
	Performer string
	ISRC      string
	Flags     []string
	// Pregap and Postgap are PREGAP/POSTGAP durations in frames. They
	// describe silence a burner would generate, not audio present in the
	// file, so a splitter reads Indexes rather than these.
	Pregap  int
	Postgap int
	// Indexes are the track's INDEX points in frames, in the order the
	// sheet gave them.
	Indexes []Index
	// Rems are the REM lines read while this track was the open one. Rem
	// looks one up.
	Rems []Rem
}

// Index is one INDEX point: a number and a position in CD frames from the
// start of the enclosing file.
type Index struct {
	Number int
	Frame  int
}

// Start returns the track's audio start in frames, which is INDEX 01.
//
// INDEX 00, when present, is the pregap start: it addresses audio that
// belongs to the previous track's tail, so splitting there would move the
// gap to the wrong side of the boundary. Every splitting tool cuts at
// INDEX 01, and so does this.
func (t Track) Start() (int, bool) {
	for _, ix := range t.Indexes {
		if ix.Number == 1 {
			return ix.Frame, true
		}
	}
	return 0, false
}

// Rem returns the value of the first of the sheet's REM lines whose key
// matches, compared case-insensitively. Rems says which lines those are.
//
// First match rather than every one: a sheet with two REM DATE lines is
// either saying the same thing twice or contradicting itself, and a second
// return value for the difference is surface nothing would read. Rems
// carries them all in order for a caller that wants the rest.
//
// The bool separates a key that is absent from one whose value is empty:
// REM DATE with nothing after it is a ripper writing a field it did not
// fill, not a date of "".
func (s *Sheet) Rem(key string) (string, bool) { return remValue(s.Rems, key) }

// Rem returns the value of the first of this track's REM lines whose key
// matches, compared case-insensitively. Sheet.Rem says why first match.
//
// Track and sheet REMs are separate on purpose: per-track ReplayGain is
// written as a track REM and the album's as a sheet REM, under keys that
// differ only in that word, so folding the two levels together would let
// one be read as the other.
func (t Track) Rem(key string) (string, bool) { return remValue(t.Rems, key) }

func remValue(rems []Rem, key string) (string, bool) {
	for _, r := range rems {
		if strings.EqualFold(r.Key, key) {
			return r.Value, true
		}
	}
	return "", false
}

// SingleFile returns the one file this sheet indexes.
//
// A sheet indexing several files describes a rip whose tracks are already
// separate, so there is nothing to cut, and picking its first file would
// be a plausible wrong answer rather than an error. Refusing by name is
// the point.
func (s *Sheet) SingleFile() (*File, error) {
	switch len(s.Files) {
	case 1:
		return &s.Files[0], nil
	case 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "cue: the sheet indexes no files")
	default:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"cue: the sheet indexes %d files, so its tracks are already separate; there is nothing to cut",
			len(s.Files)))
	}
}

// Starts returns every track's audio start in samples at rate, in order:
// one offset per track, in the sheet's own order.
//
// This is the arithmetic, not the split. Cuts is what a caller dividing the
// file wants, and is built on this. Reach for Starts directly only to pair
// a track with its own start (naming a file by its title), and never to
// re-derive cut points: that derivation is Cuts, once, because the two
// callers each had a copy of it and the copies did not agree.
//
// It is also where the invariants a splitter depends on are checked, rather
// than in Parse, so that reading a sheet and dividing a file by one are
// held to different standards. Both invariants prevent a silent failure: a
// track with no INDEX 01 has no start, and starts that do not ascend
// produce an empty or negative-length piece, which is arithmetic a split
// job carries out rather than notices. Ascend means strictly, since two
// tracks sharing an INDEX 01 name a zero-sample piece that jobs.SplitSpans
// refuses in its own right; accepting it here would let one sheet be
// answered two ways, cleanly for the CLI (which would write an empty file)
// and not at the daemon.
//
// A sheet a splitter cannot use is still a sheet, and Parse returns it: a
// caller reading titles or a REM key has no business being refused over a
// track that would not cut, and WaxBin filters on Track.Start instead.
//
// A track numbered -1 is refused too: only ParseTolerant makes one, and a
// split names and tags its pieces by number.
//
// The rate is the audio's, not the sheet's: a sheet has no rate. Its times
// are CD frames, and it is the file that says how many samples a frame is.
func (f *File) Starts(rate int) ([]int64, error) {
	if len(f.Tracks) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cue: %s names no tracks", f.label()))
	}
	out := make([]int64, len(f.Tracks))
	// The predecessor is carried rather than looked up by index, and a
	// first track is the nil case rather than a sentinel frame: there is no
	// position that reads as "before every legal start" without also being
	// a position, and a track legitimately at frame 0 has to keep going.
	var prev *Track
	var prevStart int
	for i := range f.Tracks {
		t := &f.Tracks[i]
		if t.Number < 0 {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"cue: %s track %d has no readable number, so a split cannot number its piece", f.label(), t.Number))
		}
		start, ok := t.Start()
		if !ok {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"cue: %s track %d has no INDEX 01, so it has no start", f.label(), t.Number))
		}
		if prev != nil && start <= prevStart {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"cue: %s track %d starts at frame %d, at or before track %d at %d; a file's tracks have to ascend",
				f.label(), t.Number, start, prev.Number, prevStart))
		}
		prev, prevStart = t, start
		out[i] = Samples(start, rate)
	}
	return out, nil
}

// Cuts returns the interior cut points a split of this file uses, in
// samples at rate.
//
// This is the funnel. The daemon and the CLI both divide a file by this
// list, so a sheet POSTed to one and handed to the other cuts at the same
// samples; they derived it separately before, and a sheet whose track 1 did
// not start at frame 0 came out two different rips.
//
// The pieces a caller forms are [0,c0), [c0,c1), ..., [cn,end): the whole
// file, nothing discarded. So the returned length is the piece count minus
// one, which is not always the track count, and a caller sizing its output
// counts pieces rather than tracks.
//
// A first start of 0 is dropped, since a cut at 0 opens an empty piece and
// track 1 already owns the file's first piece. That is the overwhelmingly
// common sheet. A first start past 0 is kept, and this is the part worth
// being deliberate about: the audio before track 1's INDEX 01 is real. It
// is a pregap, or it is hidden track one audio, which on some discs is an
// entire song. Keeping the cut makes it a piece of its own instead of
// folding it into track 1 or dropping it on the floor, and both callers
// then agree by construction rather than by comment.
//
// The cost of keeping it is that pieces and tracks stop lining up: with a
// nonzero first start there is one more piece than there are tracks, and
// the extra is the lead-in, which has no track and so no title. A caller
// pairing pieces with titles has to account for that offset; the CLI is
// that caller, and pairs by position.
func (f *File) Cuts(rate int) ([]int64, error) {
	starts, err := f.Starts(rate)
	if err != nil {
		return nil, err
	}
	// A sheet naming one track is not describing a division of the file,
	// whatever its INDEX 01 says: there is one track, and the file already
	// is it. This lives here rather than in a caller so that both refuse it,
	// with one message, at the same point.
	if len(starts) < 2 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"cue: %s names one track, so there is nothing to cut", f.label()))
	}
	if starts[0] == 0 {
		return starts[1:], nil
	}
	return starts, nil
}

// label names the file in a message. A file with no name is the implied
// one, and quoting its empty name would say nothing.
func (f *File) label() string {
	if f.Name == "" {
		return "the implied file"
	}
	return fmt.Sprintf("file %q", clip(f.Name, nameBytes))
}

// Samples converts a CD frame count to a sample offset at rate.
//
// The integer arithmetic here is the single most load-bearing fact in the
// package. A frame is 1/75 s, which is 13333333.33... ns, and that is not
// representable in time.Duration: a sheet routed through a Duration lands
// its cut points up to a sample off, which is a gapless failure at every
// track boundary and exactly what a CUE split exists to avoid. Every
// CD-family rate divides by 75 exactly (44100/75 = 588, 48000/75 = 640),
// so frames convert to samples directly, and exactly.
//
// A rate that does not divide by 75 truncates, because no exact answer
// exists; 32000 is the one in common use. A CUE sheet describes a CD rip,
// so that is a caller handing this a source it does not describe rather
// than a rounding policy worth having. TestCueFrameExactness pins the
// rates that matter.
func Samples(frames, rate int) int64 {
	return int64(frames) * int64(rate) / FramesPerSecond
}

// maxMinutes bounds MM, which is what keeps ParseTime's arithmetic from
// wrapping.
//
// MM has to be bounded by something: it arrives from wire bytes, Atoi only
// refuses what an int cannot hold, and (MM*60)*75 overflows an int64 well
// before Atoi objects. MM = 2049638230412173 parses, wraps, and yields a
// negative frame count, which then reaches Samples and Starts as a
// position.
//
// 100 hours is where the bound sits because a sheet is not always a CD. A
// disc tops out near 80 minutes, but sheets pair with single-file sources
// that were never discs: an audiobook or a DJ set can run 40 hours and
// carry a sheet that indexes it honestly. 100 hours clears the longest of
// those by better than twice while staying far from the arithmetic's
// limits, and a sheet addressing past it is not describing a rip.
//
// Below it every product here is exact. The largest frame count the format
// can then spell is (6000*60+59)*75+74 = 27004499, which fits an int even
// where an int is 32 bits, and Samples' int64 product stays under 1.1e13 at
// every rate in use: about six orders of magnitude below where an int64
// wraps.
const maxMinutes = 100 * 60

// ParseTime parses a CUE MM:SS:FF timestamp into CD frames.
//
// MM runs to maxMinutes (a sheet may address well past 99 minutes), SS is 0
// to 59, and FF is 0 to 74, which is what makes a frame 1/75 s rather than
// a unit of the caller's choosing.
func ParseTime(s string) (int, error) {
	frames, err := parseTime(s)
	if err != nil {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "cue: "+err.Error())
	}
	return frames, nil
}

// parseTime is ParseTime with unprefixed findings, which the parser locates
// by line.
func parseTime(s string) (int, error) {
	q := clip(s, operandBytes)
	parts := strings.SplitN(s, ":", 4) // bounded whatever the token holds
	if len(parts) != 3 {
		return 0, fmt.Errorf("time %q is not MM:SS:FF", q)
	}
	var n [3]int
	for i, p := range parts {
		v, ok := digits(p)
		if !ok {
			return 0, fmt.Errorf("time %q is not MM:SS:FF", q)
		}
		n[i] = v
	}
	switch {
	case n[0] > maxMinutes:
		return 0, fmt.Errorf("time %q has %d minutes; a sheet addresses at most %d", q, n[0], maxMinutes)
	case n[1] > 59:
		return 0, fmt.Errorf("time %q has %d seconds; a minute holds 60", q, n[1])
	case n[2] > FramesPerSecond-1:
		return 0, fmt.Errorf("time %q has %d frames; a second holds %d", q, n[2], FramesPerSecond)
	}
	return (n[0]*60+n[1])*FramesPerSecond + n[2], nil
}

// digits reads a field of ASCII digits and nothing else: no sign, and no
// value past what an int holds.
func digits(s string) (int, bool) {
	n, err := strconv.ParseUint(s, 10, strconv.IntSize-1)
	return int(n), err == nil
}

// Bounds on what a message quotes, so a 400 body or a diagnostic never
// carries a megabyte: an operand, and a file's name at a filename's length.
const (
	operandBytes = 32
	nameBytes    = 255
)

// clip cuts s to n bytes and "...", on a rune boundary when one is within a
// rune's length of the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for j := n; j > n-utf8.UTFMax; j-- {
		if utf8.RuneStart(s[j]) {
			i = j
			break
		}
	}
	return s[:i] + "..."
}

// Parse parses a CUE sheet and refuses it at the first line it cannot read,
// as "cue: line N: " and the finding. Whether a splitter can use the sheet
// is File.Starts' question.
func Parse(b []byte) (*Sheet, error) {
	s := parse(b, true)
	if len(s.Warnings) > 0 {
		w := s.Warnings[0]
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("cue: line %d: %s", w.Line, w.Msg))
	}
	return s, nil
}

// ParseTolerant never fails: each line Parse would refuse is a Warning and
// is dropped, though a FILE or TRACK line still opens its file or track (-1
// when unreadable). It is for a reader, since a dropped line can move a cut.
func ParseTolerant(b []byte) *Sheet { return parse(b, false) }

// parse reads the sheet, recording every finding; strict stops at the first.
func parse(b []byte, strict bool) *Sheet {
	var sheet Sheet
	var file *File
	var track *Track
	// culprit is the first TRACK-shaped line skipped since the last FILE,
	// TRACK or accepted INDEX line, which an ordering or outside-a-TRACK
	// finding names.
	var culprit skippedLine

	// commit folds the track and file under construction into the sheet.
	// Both are built in place and appended on close, so the pointers above
	// never alias a slice that may have been reallocated by a later
	// append.
	commitTrack := func() {
		if track != nil {
			file.Tracks = append(file.Tracks, *track)
			track = nil
		}
	}
	commitFile := func() {
		commitTrack()
		if file != nil {
			sheet.Files = append(sheet.Files, *file)
			file = nil
		}
	}
	// warn records a line this could not read. The arm then drops the
	// line, or keeps it best effort where dropping would orphan the
	// lines under it (a FILE without a name, a TRACK without a number).
	warn := func(line int, msg string) {
		if len(sheet.Warnings) < maxWarnings {
			sheet.Warnings = append(sheet.Warnings, Warning{Line: line, Msg: msg})
		}
	}

	text := decode(b)
	sep := "\n"
	if !strings.Contains(text, "\n") {
		sep = "\r" // classic Mac OS ends a line with CR alone
	}
	for i, line := range strings.Split(text, sep) {
		if strict && sheet.Warnings != nil {
			break
		}
		lineNo := i + 1
		line = strings.TrimRight(line, "\r")
		if strings.Trim(line, blanks) == "" {
			continue
		}
		kw, rest, closed := cutToken(line)
		if !closed {
			warn(lineNo, fmt.Sprintf("%q is missing its closing quote", clip(kw, operandBytes)))
			continue
		}
		cmd := strings.ToUpper(kw)

		// A command that carries one string operand: which struct field it
		// lands in depends only on whether a track is open, so the sheet
		// and track levels share one arm rather than two parallel ones.
		if dst := stringTarget(cmd, &sheet, track); dst != nil {
			*dst = stringOperand(rest)
			continue
		}

		switch cmd {
		case "FILE":
			name, typ, msg := fileOperands(rest)
			if msg != "" {
				warn(lineNo, msg)
			}
			commitFile()
			file = &File{Name: name, Type: typ}
			culprit = skippedLine{}

		case "TRACK":
			if file == nil {
				file = &File{} // the implied file
			}
			commitTrack()
			numTok, more, _ := cutToken(rest)
			typ, _, _ := cutToken(more)
			if typ == "" && isDatatype(numTok) {
				numTok, typ = "", numTok // TRACK AUDIO: it is the number that is missing
			}
			t := Track{Number: -1, Type: strings.ToUpper(typ)}
			num, ok := digits(numTok)
			if ok {
				t.Number = num
			}
			switch _, closed := tokens(rest); {
			case numTok == "":
				warn(lineNo, "TRACK takes a number")
			case !ok:
				warn(lineNo, fmt.Sprintf("TRACK number %q is not a number", clip(numTok, operandBytes)))
			case !closed:
				warn(lineNo, "TRACK is missing a closing quote")
			}
			track = &t
			culprit = skippedLine{}

		case "INDEX":
			if track == nil {
				warn(lineNo, outside(cmd, culprit))
				continue
			}
			numTok, more, _ := cutToken(rest)
			timeTok, _, _ := cutToken(more)
			if _, closed := tokens(rest); !closed {
				warn(lineNo, trackf(track.Number, "INDEX is missing a closing quote"))
				continue
			}
			if timeTok == "" {
				warn(lineNo, trackf(track.Number, "INDEX takes a number and a time"))
				continue
			}
			num, ok := digits(numTok)
			if !ok || num > 99 {
				warn(lineNo, trackf(track.Number, "INDEX number %q is not 00 to 99", clip(numTok, operandBytes)))
				continue
			}
			frame, err := parseTime(timeTok)
			if err != nil {
				warn(lineNo, trackf(track.Number, "%v", err))
				continue
			}
			// Indexes ascend, so one that does not is most likely a TRACK line
			// this never read, whose track would otherwise merge into this one.
			if n := len(track.Indexes); n > 0 && num <= track.Indexes[n-1].Number {
				warn(lineNo, disorder(track.Number, num, track.Indexes[n-1].Number, culprit))
				continue
			}
			track.Indexes = append(track.Indexes, Index{Number: num, Frame: frame})
			culprit = skippedLine{}

		case "PREGAP", "POSTGAP":
			if track == nil {
				warn(lineNo, outside(cmd, culprit))
				continue
			}
			tok, _, _ := cutToken(rest)
			if _, closed := tokens(rest); !closed {
				warn(lineNo, trackf(track.Number, "%s is missing a closing quote", cmd))
				continue
			}
			if tok == "" {
				warn(lineNo, trackf(track.Number, "%s takes a time", cmd))
				continue
			}
			gap, err := parseTime(tok)
			if err != nil {
				warn(lineNo, trackf(track.Number, "%v", err))
				continue
			}
			if cmd == "PREGAP" {
				track.Pregap = gap
			} else {
				track.Postgap = gap
			}

		case "FLAGS":
			if track == nil {
				continue
			}
			if _, closed := tokens(rest); !closed {
				warn(lineNo, trackf(track.Number, "FLAGS is missing a closing quote"))
				continue
			}
			for rest = strings.Trim(rest, blanks); rest != ""; {
				var flag string
				flag, rest, _ = cutToken(rest)
				track.Flags = append(track.Flags, flag)
			}

		case "REM":
			// A ripper's private key space, kept as read. A REM that
			// happens to spell FILE is still a comment, so this arm never
			// looks at the key: Rem is where a reader decides which keys
			// mean anything.
			//
			// An empty REM is a blank comment line, which carries nothing
			// and is not worth a keyless entry.
			key, value, _ := cutToken(rest)
			if key == "" {
				continue
			}
			r := Rem{Key: key, Value: stringOperand(value)}
			if track != nil {
				track.Rems = append(track.Rems, r)
			} else {
				sheet.Rems = append(sheet.Rems, r)
			}

		case "SONGWRITER", "CDTEXTFILE", "CATALOG", "ISRC":
			// The format's own commands where nothing here reads them
			// (CATALOG inside a track, ISRC outside one): skipped, and never
			// named as a misspelled TRACK.

		default:
			// A vendor extension, or a command from a revision of the
			// format this does not know. Skipping is deliberate: a sheet
			// is metadata beside the audio, and refusing a whole rip over
			// an unread line would trade a working split for a purity
			// nobody asked for. One shaped like a TRACK may be a misspelled one.
			if culprit.line == 0 && trackish(kw, rest) {
				culprit = skippedLine{line: lineNo, kw: clip(kw, operandBytes)}
			}
		}
	}
	commitFile()
	return &sheet
}

// skippedLine is a line whose command this does not read; the zero value is
// none.
type skippedLine struct {
	line int
	kw   string
}

// unread names the line in a finding. %+q escapes letters outside ASCII, so
// a lookalike cannot read as TRACK.
func (s skippedLine) unread() string {
	if s.kw == "" {
		return fmt.Sprintf("line %d is not a command this reads", s.line)
	}
	return fmt.Sprintf("line %d %+q is not a command this reads", s.line, s.kw)
}

// outside explains cmd arriving with no track open.
func outside(cmd string, culprit skippedLine) string {
	if culprit.line == 0 {
		return cmd + " outside a TRACK"
	}
	return cmd + " outside a TRACK; " + culprit.unread()
}

// disorder explains INDEX num arriving after INDEX prev in track n.
func disorder(n, num, prev int, culprit skippedLine) string {
	what := fmt.Sprintf("INDEX %02d repeats", num)
	if num < prev {
		what = fmt.Sprintf("INDEX %02d follows INDEX %02d", num, prev)
	}
	if culprit.line == 0 {
		return trackf(n, "%s; the TRACK line between the two is missing or misspelled", what)
	}
	return trackf(n, "%s; %s", what, culprit.unread())
}

// trackf prefixes a finding with the track its line belongs to.
func trackf(n int, format string, args ...any) string {
	return fmt.Sprintf("track %d: ", n) + fmt.Sprintf(format, args...)
}

// trackish reports whether a skipped line looks like a TRACK line: it says
// TRACK, or its first operand is a track number.
func trackish(kw, rest string) bool {
	if strings.Contains(strings.ToUpper(kw+rest), "TRACK") {
		return true
	}
	tok, _, _ := cutToken(rest)
	n, ok := digits(tok)
	return ok && n <= 99
}

// isDatatype reports whether tok is a TRACK datatype rather than a number.
func isDatatype(tok string) bool {
	t := strings.ToUpper(tok)
	return t == "AUDIO" || t == "CDG" || strings.HasPrefix(t, "MODE") || strings.HasPrefix(t, "CDI")
}

// stringTarget returns the field a one-operand string command writes,
// which is the open track's when there is one and the sheet's otherwise.
// A nil return means cmd is not one of these, and the caller skips it.
//
// SONGWRITER is deliberately absent and so falls to the skip: a command
// nothing reads is better skipped than stored.
func stringTarget(cmd string, sheet *Sheet, track *Track) *string {
	if track != nil {
		switch cmd {
		case "TITLE":
			return &track.Title
		case "PERFORMER":
			return &track.Performer
		case "ISRC":
			return &track.ISRC
		}
		return nil
	}
	switch cmd {
	case "TITLE":
		return &sheet.Title
	case "PERFORMER":
		return &sheet.Performer
	case "CATALOG":
		return &sheet.Catalog
	}
	return nil
}

// blanks separate tokens; the format has no others.
const blanks = " \t"

// cutToken splits the first token off s: blanks skipped, then a quoted run
// without its quotes, or a run to the next blank. closed is false when a
// quote never closes; the token then runs to the end.
func cutToken(s string) (tok, rest string, closed bool) {
	s = strings.TrimLeft(s, blanks)
	switch {
	case s == "":
		return "", "", true
	case s[0] == '"':
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return s[1:], "", false
		}
		return s[1 : 1+end], s[2+end:], true
	}
	if end := strings.IndexAny(s, blanks); end >= 0 {
		return s[:end], s[end:], true
	}
	return s, "", true
}

// tokens counts the tokens in s and reports whether every quote among them
// closes.
func tokens(s string) (n int, closed bool) {
	closed = true
	for s = strings.TrimLeft(s, blanks); s != ""; s = strings.TrimLeft(s, blanks) {
		var ok bool
		_, s, ok = cutToken(s)
		n, closed = n+1, closed && ok
	}
	return n, closed
}

// stringOperand reads a one-string command's operand, the rest of the line.
// The format has no escape, so quotes strip only as the pair around it: "The
// "Best" Of" keeps its inner quotes, and 12" Remix reads itself.
func stringOperand(s string) string {
	s = strings.Trim(s, blanks)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	if s != "" && s[0] == '"' {
		tok, _, _ := cutToken(s) // to its closing quote, or the end
		return tok
	}
	return s
}

// fileOperands reads a FILE line's name and type. A quoted name closes at its
// first quote unless that splits a name with quotes of its own ("12"
// Single.flac" WAVE); an unquoted one ends before a last word without a dot.
func fileOperands(s string) (name, typ, msg string) {
	s = strings.Trim(s, blanks)
	if s == "" {
		return "", "", "FILE takes a name"
	}
	if s[0] != '"' {
		cut := strings.LastIndexAny(s, blanks)
		if cut < 0 || strings.Contains(s[cut+1:], ".") {
			return s, "", ""
		}
		return strings.TrimRight(s[:cut], blanks), strings.ToUpper(s[cut+1:]), ""
	}
	name, tail, closed := cutToken(s)
	if !closed {
		return name, "", "FILE name is missing its closing quote"
	}
	if n, _ := tokens(tail); n > 1 {
		if last := strings.LastIndexByte(s, '"'); last > len(s)-len(tail)-1 {
			if t, _, _ := cutToken(s[last+1:]); t != "" {
				name, tail = s[1:last], s[last+1:]
			}
		}
	}
	typ, _, _ = cutToken(tail)
	if _, closed := tokens(tail); !closed {
		msg = "FILE is missing a closing quote"
	}
	return name, strings.ToUpper(typ), msg
}
