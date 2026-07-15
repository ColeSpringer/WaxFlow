// Package cue parses CUE sheets, the sidecar index that pairs a
// single-file CD rip with the track boundaries the disc itself had.
//
// It is internal rather than public because ADR-0002 makes the public
// surface a promise: internal to public is a move we can make later,
// while the reverse breaks callers. It is not container/cue either, since
// that namespace means "things with a Demuxer" and a CUE sheet is an
// index alongside the audio rather than a wrapper around it; it must
// never enter format's magic-byte driver table.
//
// Positions are CD frames throughout, never time.Duration. Samples says
// why.
package cue

import (
	"fmt"
	"strconv"
	"strings"

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
// more (SONGWRITER, CDTEXTFILE, a ripper's REM keys), and the rest is
// skipped rather than stored: nothing in this program writes a sheet back
// out, so an unread field is not a round trip being preserved, it is a
// field. WaxLabel owns tags.
type Sheet struct {
	Catalog   string
	Title     string
	Performer string
	Files     []File
}

// File is one FILE statement and the tracks indexed against it. Positions
// inside it are relative to that file's own start, so a track-per-file
// sheet has each track at frame 0 of its own file.
type File struct {
	// Name is the referenced audio file, as the sheet spells it. It is a
	// sheet-relative name and is not resolved or validated here.
	Name string
	// Type is the FILE type token (WAVE, MP3, AIFF, BINARY, MOTOROLA).
	Type   string
	Tracks []Track
}

// Track is one TRACK statement.
type Track struct {
	// Number is the TRACK number as written. It is the disc's numbering,
	// not an index into Tracks: a sheet is free to start at 2 or skip.
	Number int
	// Type is the TRACK datatype token (AUDIO, MODE1/2352, and the other
	// data modes). It is the only thing in a sheet that says a track is not
	// audio, which a mixed-mode disc's first track routinely is not, and
	// cutting a data track as audio yields a piece of filesystem named after
	// a song. Nothing refuses that yet: see validate.
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
// The rate is the audio's, not the sheet's: a sheet has no rate. Its times
// are CD frames, and it is the file that says how many samples a frame is.
func (f *File) Starts(rate int) ([]int64, error) {
	if len(f.Tracks) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cue: file %q names no tracks", f.Name))
	}
	out := make([]int64, len(f.Tracks))
	for i, t := range f.Tracks {
		start, ok := t.Start()
		if !ok {
			// Parse already refuses this, so reaching it means a
			// hand-built Sheet. Refuse rather than address a track that
			// has no start.
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("cue: track %d has no INDEX 01, so it has no start", t.Number))
		}
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
			"cue: file %q names one track, so there is nothing to cut", f.Name))
	}
	if starts[0] == 0 {
		return starts[1:], nil
	}
	return starts, nil
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
// negative frame count, which then reaches Samples and validate as a
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
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cue: time %q is not MM:SS:FF", s))
	}
	n := make([]int, 3)
	for i, p := range parts {
		// Reject a signed or space-padded field rather than letting Atoi
		// take it: "-0" and "+1" parse fine and mean nothing here.
		if p == "" || strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("cue: time %q is not MM:SS:FF", s))
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return 0, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("cue: time %q: %v", s, err))
		}
		n[i] = v
	}
	switch {
	case n[0] > maxMinutes:
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cue: time %q has %d minutes; a sheet addresses at most %d", s, n[0], maxMinutes))
	case n[1] > 59:
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cue: time %q has %d seconds; a minute holds 60", s, n[1]))
	case n[2] > FramesPerSecond-1:
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cue: time %q has %d frames; a second holds %d", s, n[2], FramesPerSecond))
	}
	return (n[0]*60+n[1])*FramesPerSecond + n[2], nil
}

// Parse parses a CUE sheet.
//
// The text is decoded best effort: see decode. Unknown commands are
// skipped rather than refused, since sheets in the wild carry vendor
// extensions and a ripper's private REM keys; what is refused is a sheet
// whose structure or arithmetic cannot be trusted, because that is the
// part a split job would act on.
func Parse(b []byte) (*Sheet, error) {
	var sheet Sheet
	var file *File
	var track *Track

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

	for i, line := range strings.Split(decode(b), "\n") {
		lineNo := i + 1
		toks, err := fields(strings.TrimRight(line, "\r"))
		if err != nil {
			return nil, lineErr(lineNo, err)
		}
		if len(toks) == 0 {
			continue
		}
		cmd, args := strings.ToUpper(toks[0]), toks[1:]

		// A command that carries one string operand: which struct field it
		// lands in depends only on whether a track is open, so the sheet
		// and track levels share one arm rather than two parallel ones.
		if dst := stringTarget(cmd, &sheet, track); dst != nil {
			if len(args) < 1 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("cue: %s takes an operand", cmd)))
			}
			*dst = args[0]
			continue
		}

		switch cmd {
		case "FILE":
			if len(args) < 1 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					"cue: FILE takes a name and a type"))
			}
			commitFile()
			f := File{Name: args[0]}
			if len(args) > 1 {
				f.Type = strings.ToUpper(args[1])
			}
			file = &f

		case "TRACK":
			if file == nil {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					"cue: TRACK before any FILE; a track has to be indexed against something"))
			}
			if len(args) < 1 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					"cue: TRACK takes a number and a type"))
			}
			num, err := strconv.Atoi(args[0])
			if err != nil || num < 0 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("cue: TRACK number %q is not a number", args[0])))
			}
			commitTrack()
			t := Track{Number: num}
			if len(args) > 1 {
				t.Type = strings.ToUpper(args[1])
			}
			track = &t

		case "INDEX":
			if track == nil {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					"cue: INDEX outside a TRACK"))
			}
			if len(args) < 2 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					"cue: INDEX takes a number and a time"))
			}
			num, err := strconv.Atoi(args[0])
			if err != nil || num < 0 || num > 99 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("cue: INDEX number %q is not 00 to 99", args[0])))
			}
			frame, err := ParseTime(args[1])
			if err != nil {
				return nil, lineErr(lineNo, err)
			}
			track.Indexes = append(track.Indexes, Index{Number: num, Frame: frame})

		case "PREGAP", "POSTGAP":
			if track == nil {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("cue: %s outside a TRACK", cmd)))
			}
			if len(args) < 1 {
				return nil, lineErr(lineNo, waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("cue: %s takes a time", cmd)))
			}
			gap, err := ParseTime(args[0])
			if err != nil {
				return nil, lineErr(lineNo, err)
			}
			if cmd == "PREGAP" {
				track.Pregap = gap
			} else {
				track.Postgap = gap
			}

		case "FLAGS":
			if track != nil {
				track.Flags = append(track.Flags, args...)
			}

		case "REM":
			// A ripper's private key space. Skipped rather than captured:
			// nothing here reads it, and a REM that happens to spell FILE
			// is still a comment.

		default:
			// A vendor extension, or a command from a revision of the
			// format this does not know. Skipping is deliberate: a sheet
			// is metadata beside the audio, and refusing a whole rip over
			// an unread line would trade a working split for a purity
			// nobody asked for.
		}
	}
	commitFile()

	if err := validate(&sheet); err != nil {
		return nil, err
	}
	return &sheet, nil
}

// stringTarget returns the field a one-operand string command writes,
// which is the open track's when there is one and the sheet's otherwise.
// A nil return means cmd is not one of these, and the caller skips it.
//
// SONGWRITER is deliberately absent and so falls to the skip. A command
// nothing reads is better skipped than stored: storing it would also mean
// refusing a sheet whose SONGWRITER line is malformed, which is refusing a
// working rip over a line that could not have changed the split.
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

// validate holds a parsed sheet to the invariants a splitter depends on.
//
// These are checked here rather than left to the caller because the
// failure they prevent is silent: a track with no INDEX 01 has no start,
// and cut points that do not ascend produce an empty or negative-length
// piece. Both are arithmetic a split job would carry out rather than
// notice.
//
// Ascend means strictly. Two tracks sharing an INDEX 01 name a zero-sample
// piece, which jobs.SplitSpans refuses in its own right; accepting it here
// would let one sheet be answered two ways, parsing cleanly for the CLI
// (which would write an empty file) and failing at the daemon.
func validate(sheet *Sheet) error {
	for fi := range sheet.Files {
		f := &sheet.Files[fi]
		// The predecessor is carried rather than looked up by index, and a
		// first track is the nil case rather than a sentinel frame: there is
		// no position that reads as "before every legal start" without also
		// being a position, and a track legitimately at frame 0 has to keep
		// parsing.
		var prev *Track
		var prevStart int
		for ti := range f.Tracks {
			t := &f.Tracks[ti]
			start, ok := t.Start()
			if !ok {
				return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
					"cue: file %q track %d has no INDEX 01, so it has no start", f.Name, t.Number))
			}
			if prev != nil && start <= prevStart {
				return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
					"cue: file %q track %d starts at frame %d, at or before track %d at %d; a file's tracks have to ascend",
					f.Name, t.Number, start, prev.Number, prevStart))
			}
			prev, prevStart = t, start
		}
	}
	return nil
}

func lineErr(line int, err error) error {
	return waxerr.Wrap(waxerr.CodeOf(err), fmt.Sprintf("cue: line %d", line), err)
}

// fields splits a CUE line into tokens: whitespace separated, except that
// a double-quoted run is one token and may hold spaces.
//
// The format has no escape sequence, so a quote always opens or closes and
// never stands for itself. That is a real limit of CUE rather than a
// simplification here: a title containing a double quote cannot be written
// in a sheet at all.
func fields(line string) ([]string, error) {
	var out []string
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '"':
			end := strings.IndexByte(line[i+1:], '"')
			if end < 0 {
				return nil, waxerr.New(waxerr.CodeInvalidRequest, "cue: unterminated quoted string")
			}
			out = append(out, line[i+1:i+1+end])
			i += end + 2
		default:
			j := i
			for j < len(line) && line[j] != ' ' && line[j] != '\t' {
				j++
			}
			out = append(out, line[i:j])
			i = j
		}
	}
	return out, nil
}
