package cue

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// fr is the samples in one CD frame at 44100 and sec the samples in one
// second, spelled out rather than taken from Samples: an expectation computed
// by the code under test agrees with any conversion at all.
const (
	fr  = 588
	sec = 75 * fr
)

// pieceFile parses a sheet and returns the one file it cuts.
func pieceFile(t *testing.T, sheet string) *File {
	t.Helper()
	s, err := Parse([]byte(sheet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f, err := s.SingleFile()
	if err != nil {
		t.Fatalf("SingleFile: %v", err)
	}
	return f
}

func TestIsAudio(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"AUDIO", true},
		// CD+G carries its audio like any other track; the graphics ride the
		// subcode, which a decoded file no longer has.
		{"CDG", true},
		// A hand-written sheet may omit the datatype, and an unfamiliar one
		// is taken as audio rather than dropping a song.
		{"", true},
		{"WHATEVER", true},
		{"MODE1/2048", false},
		{"MODE1/2352", false},
		{"MODE2/2336", false},
		{"MODE2/2352", false},
		{"CDI/2336", false},
		{"CDI/2352", false},
		// Parse uppercases the token; a hand-built Track need not.
		{"mode1/2352", false},
	} {
		if got := (Track{Type: tc.typ}).IsAudio(); got != tc.want {
			t.Errorf("Track{Type: %q}.IsAudio() = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// TestPieces pins the division of a file into what a split writes and what it
// skips. The shapes are the ones real sheets have: a single-file image of a
// mixed-mode disc with its data track first (CDRWIN, Alcohol, and every
// PlayStation rip), an EAC image whose data track occupies none of the audio
// file, an Enhanced CD whose data track follows the audio, and a PC Engine
// disc whose data track sits between audio tracks.
func TestPieces(t *testing.T) {
	const end = -1
	for _, tc := range []struct {
		name  string
		in    string
		total int64
		want  []Piece
	}{
		{
			// The data track runs to the first audio INDEX 01, not to the
			// audio's INDEX 00: on a mixed-mode disc the mode change puts
			// data-mode sectors inside track 2's pregap.
			name: "a raw image's data track is skipped up to the first audio INDEX 01",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 00 00:10:00\n    INDEX 01 00:12:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:20:00\n",
			total: 30 * sec,
			want: []Piece{
				{From: 0, To: 12 * sec, Track: 0, Audio: false},
				{From: 12 * sec, To: 20 * sec, Track: 1, Audio: true},
				{From: 20 * sec, To: end, Track: 2, Audio: true},
			},
		},
		{
			// EAC rips the audio session alone, so its image starts at the
			// first audio track while the sheet still lists the data track
			// ahead of it, sharing frame 0. A data track may occupy nothing.
			name: "a data track occupying no samples is not a piece",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			total: 10 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 1, Audio: true},
				{From: 5 * sec, To: end, Track: 2, Audio: true},
			},
		},
		{
			// The pregap of a data track is in the data track's mode, so the
			// audio before its INDEX 01 is not a lead-in.
			name: "a first data track owns the file from sample 0",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:08:00\n",
			total: 10 * sec,
			want: []Piece{
				{From: 0, To: 4 * sec, Track: 0, Audio: false},
				{From: 4 * sec, To: 8 * sec, Track: 1, Audio: true},
				{From: 8 * sec, To: end, Track: 2, Audio: true},
			},
		},
		{
			// A sheet that lists the disc's data track without placing it
			// (real sheets do, in a FILE of its own) still says where the
			// audio starts.
			name: "a first data track needs no INDEX",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:08:00\n",
			total: 10 * sec,
			want: []Piece{
				{From: 0, To: 4 * sec, Track: 0, Audio: false},
				{From: 4 * sec, To: 8 * sec, Track: 1, Audio: true},
				{From: 8 * sec, To: end, Track: 2, Audio: true},
			},
		},
		{
			// An audio track's INDEX 00 audio belongs to the song before it;
			// a data track's INDEX 00 region does not, since it is in data
			// mode (or, on an Enhanced CD, the second session's lead-in).
			name: "the audio before a data track ends at its INDEX 00",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 00 00:09:00\n    INDEX 01 00:11:00\n",
			total: 20 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: 9 * sec, Track: 1, Audio: true},
				{From: 9 * sec, To: end, Track: 2, Audio: false},
			},
		},
		{
			name: "a data track with only an INDEX 01 ends the audio there",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 01 00:09:00\n",
			total: 20 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: 9 * sec, Track: 1, Audio: true},
				{From: 9 * sec, To: end, Track: 2, Audio: false},
			},
		},
		{
			// An Enhanced CD's data track lives in a second session, past
			// the audio the rip holds, and a sheet may address it there.
			name: "a data track at the file's end occupies nothing",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 00 00:09:00\n    INDEX 01 00:11:00\n",
			total: 9 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: end, Track: 1, Audio: true},
			},
		},
		{
			name: "a data track past the file's end occupies nothing",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 01 01:00:00\n",
			total: 9 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: end, Track: 1, Audio: true},
			},
		},
		{
			// With no length to check against, the sheet is taken at its word.
			name: "an unknown length keeps a trailing data track",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 01 01:00:00\n",
			total: -1,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: 60 * sec, Track: 1, Audio: true},
				{From: 60 * sec, To: end, Track: 2, Audio: false},
			},
		},
		{
			// PC Engine CD-ROM² discs open with an audio warning track, put
			// the data track second, and follow it with more audio. The
			// audio after it is cut at its INDEX 01, so the pregap between
			// them, which the disc fills with data-mode sectors, goes with
			// the data track.
			name: "a data track between audio tracks",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 MODE1/2352\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 00 00:09:00\n    INDEX 01 00:11:00\n",
			total: 20 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: 11 * sec, Track: 1, Audio: false},
				{From: 11 * sec, To: end, Track: 2, Audio: true},
			},
		},
		{
			// Nothing audio follows, so the cooked sectors spoil no frame
			// the split cuts by.
			name: "a cooked data track followed only by data is skipped",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 MODE1/2048\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE2/2352\n    INDEX 01 00:09:00\n",
			total: 20 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: 9 * sec, Track: 1, Audio: false},
				{From: 9 * sec, To: end, Track: 2, Audio: false},
			},
		},
		{
			// A cooked data track spoils the frame arithmetic only past
			// itself; last, it is skipped like any other.
			name: "a cooked data track last is skipped",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2048\n    INDEX 01 00:09:00\n",
			total: 20 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 0, Audio: true},
				{From: 5 * sec, To: 9 * sec, Track: 1, Audio: true},
				{From: 9 * sec, To: end, Track: 2, Audio: false},
			},
		},
		{
			name: "a cooked data track occupying nothing spoils nothing",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2048\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			total: 10 * sec,
			want: []Piece{
				{From: 0, To: 5 * sec, Track: 1, Audio: true},
				{From: 5 * sec, To: end, Track: 2, Audio: true},
			},
		},
		{
			// One audio track is not a division of the file, but one audio
			// track behind a data track is: the data comes off the front.
			name: "one audio track behind a data track is still a split",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n",
			total: 10 * sec,
			want: []Piece{
				{From: 0, To: 4 * sec, Track: 0, Audio: false},
				{From: 4 * sec, To: end, Track: 1, Audio: true},
			},
		},
		{
			// The existing contract: audio before an audio track 1 is a
			// piece of its own, the lead-in, which no track names.
			name: "a lead-in stays a piece when the first track is audio",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:33\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:37\n",
			total: 10 * sec,
			want: []Piece{
				{From: 0, To: 33 * fr, Track: -1, Audio: true},
				{From: 33 * fr, To: 412 * fr, Track: 0, Audio: true},
				{From: 412 * fr, To: end, Track: 1, Audio: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pieceFile(t, tc.in).Pieces(44100, tc.total)
			if err != nil {
				t.Fatalf("Pieces: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Pieces(44100, %d) =\n%+v\nwant\n%+v", tc.total, got, tc.want)
			}
		})
	}
}

// TestPiecesRefuses: what a split cannot do with a sheet, named rather than
// cut wrong.
func TestPiecesRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		total int64
		want  string
	}{
		{
			// Only a first data track can go unplaced: a later one's INDEX
			// is what says where the audio before it ends.
			name: "a data track after audio with no INDEX",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n",
			total: 20 * sec,
			want:  "where the audio before it ends",
		},
		{
			// A data track's INDEX 01 still orders the sheet: the audio after
			// it cannot start inside it.
			name: "an audio track starting inside the data track before it",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 MODE1/2352\n    INDEX 00 00:05:00\n    INDEX 01 00:09:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			total: 20 * sec,
			want:  "have to ascend",
		},
		{
			name: "an audio track starting inside a first data track",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:02:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:01:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			total: 20 * sec,
			want:  "have to ascend",
		},
		{
			// A lead-in on a file with no samples is an audio piece past the
			// end like any other, named as such rather than indexed as a
			// track.
			name: "a lead-in on an empty file",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:33\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:37\n",
			total: 0,
			want:  "does not describe this file",
		},
		{
			// A sheet naming one track is not describing a division of the
			// file, whatever its INDEX 01 says.
			name: "one audio track with a lead-in",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n",
			total: 10 * sec,
			want:  "nothing to cut",
		},
		{
			// Refused for what it lacks, not for the index a data track it
			// would never write is missing.
			name: "only data tracks, one of them unplaced",
			in: "FILE \"img.bin\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 MODE1/2352\n",
			total: 10 * sec,
			want:  "no audio track",
		},
		{
			// A 2048-byte sector is not 588 samples, so every frame the sheet
			// names past a cooked data track lands on the wrong sample.
			name: "a cooked data track occupying the file before audio",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2048\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:08:00\n",
			total: 10 * sec,
			want:  "2048",
		},
		{
			name: "no audio track at all",
			in: "FILE \"img.bin\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 MODE2/2352\n    INDEX 01 00:04:00\n",
			total: 10 * sec,
			want:  "no audio track",
		},
		{
			name:  "one audio track alone",
			in:    "FILE \"rip.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n",
			total: 10 * sec,
			want:  "nothing to cut",
		},
		{
			// A data track at the end of the file leaves one audio piece,
			// which is the whole file: still nothing to cut.
			name: "one audio track and a data track past the end",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 MODE1/2352\n    INDEX 01 01:00:00\n",
			total: 10 * sec,
			want:  "nothing to cut",
		},
		{
			name: "an audio track past the file's end",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n",
			total: 5 * sec,
			want:  "does not describe this file",
		},
		{
			// A data track's INDEX 00 on the audio's own start empties it.
			name: "a data track leaving the audio before it empty",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 02 MODE1/2352\n    INDEX 00 00:05:00\n    INDEX 01 00:07:00\n",
			total: 20 * sec,
			want:  "have to ascend",
		},
		{
			name: "a data track before the audio before it",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 01 00:03:00\n",
			total: 20 * sec,
			want:  "have to ascend",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pieceFile(t, tc.in).Pieces(44100, tc.total)
			if err == nil {
				t.Fatalf("Pieces = %+v, want a refusal mentioning %q", got, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Pieces error = %v, want it to mention %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeInvalidRequest {
				t.Errorf("code = %v, want CodeInvalidRequest", code)
			}
		})
	}
}

// TestStartsDataTrackBoundaries: Starts is the walk Pieces divides by, so a
// data track's entry is the sample the audio before it ends at, and two
// entries may coincide where a data track occupies nothing.
func TestStartsDataTrackBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []int64
	}{
		{
			name: "a data track's INDEX 00 is its boundary",
			in: "FILE \"rip.wav\" WAVE\n" +
				"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 00 00:09:00\n    INDEX 01 00:11:00\n",
			want: []int64{0, 5 * sec, 9 * sec},
		},
		{
			name: "a first data track starts at 0 whatever its INDEX says",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n",
			want: []int64{0, 4 * sec},
		},
		{
			name: "a data track may share its boundary with the audio after it",
			in: "FILE \"img.wav\" WAVE\n" +
				"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			want: []int64{0, 0, 5 * sec},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pieceFile(t, tc.in).Starts(44100)
			if err != nil {
				t.Fatalf("Starts: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Starts(44100) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCutsRefusesADataTrack: a cut list partitions the whole file, so it
// cannot say "and skip this piece"; a sheet whose data track occupies the
// file is refused by name rather than cut into a piece of noise, while one
// whose data track occupies nothing still cuts.
func TestCutsRefusesADataTrack(t *testing.T) {
	occupied := pieceFile(t, "FILE \"img.wav\" WAVE\n"+
		"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n"+
		"  TRACK 03 AUDIO\n    INDEX 01 00:08:00\n")
	cuts, err := occupied.Cuts(44100)
	if err == nil {
		t.Fatalf("Cuts = %v for a data track occupying the file, want a refusal", cuts)
	}
	if !strings.Contains(err.Error(), "data track") {
		t.Errorf("Cuts error = %v, want it to name the data track", err)
	}
	if code := waxerr.CodeOf(err); code != waxerr.CodeInvalidRequest {
		t.Errorf("code = %v, want CodeInvalidRequest", code)
	}

	// One occupying nothing is refused too: Cuts promises a caller one
	// piece per track (plus a lead-in), and a track with no piece breaks
	// the pairing a caller does by position.
	empty := pieceFile(t, "FILE \"img.wav\" WAVE\n"+
		"  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"+
		"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n")
	if cuts, err := empty.Cuts(44100); err == nil {
		t.Fatalf("Cuts = %v for a data track occupying nothing, want a refusal", cuts)
	} else if !strings.Contains(err.Error(), "data track") {
		t.Errorf("Cuts error = %v, want it to name the data track", err)
	}
}

// TestCutpoints: the list a split's job carries, derived from the pieces
// once for every caller.
func TestCutpoints(t *testing.T) {
	pieces := []Piece{
		{From: 0, To: 100, Track: 0, Audio: false},
		{From: 100, To: 250, Track: 1, Audio: true},
		{From: 250, To: 300, Track: 2, Audio: false},
		{From: 300, To: -1, Track: 3, Audio: true},
	}
	cuts, skip := Cutpoints(pieces)
	if want := []int64{100, 250, 300}; !slices.Equal(cuts, want) {
		t.Errorf("cuts = %v, want %v", cuts, want)
	}
	if want := []int{0, 2}; !slices.Equal(skip, want) {
		t.Errorf("skip = %v, want %v", skip, want)
	}
	if cuts, skip := Cutpoints(pieces[1:2]); len(cuts) != 0 || len(skip) != 0 {
		t.Errorf("Cutpoints(one audio piece) = %v, %v, want none", cuts, skip)
	}
}

// TestPiecesAfterADataFile: when the disc's data track has a FILE of its
// own ahead of the audio file, the audio before that file's first INDEX 01
// is the pregap after the data track, in the mode change's mixed sectors,
// and is skipped as it would be were the data track in this file. The same
// file with no data track ahead of it keeps the lead-in.
func TestPiecesAfterADataFile(t *testing.T) {
	const audio = "FILE \"disc.wav\" WAVE\n" +
		"  TRACK 02 AUDIO\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n" +
		"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n"
	after := pieceFile(t, "FILE \"disc.iso\" BINARY\n  TRACK 01 MODE1/2352\n"+audio)
	if !after.PrecededByData {
		t.Fatal("PrecededByData = false for the audio file after a data track's FILE")
	}
	got, err := after.Pieces(44100, 10*sec)
	if err != nil {
		t.Fatalf("Pieces: %v", err)
	}
	want := []Piece{
		{From: 0, To: 2 * sec, Track: -1, Audio: false},
		{From: 2 * sec, To: 5 * sec, Track: 0, Audio: true},
		{From: 5 * sec, To: -1, Track: 1, Audio: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Pieces = %+v, want %+v", got, want)
	}

	alone := pieceFile(t, audio)
	if alone.PrecededByData {
		t.Fatal("PrecededByData = true for a sheet's first file")
	}
	got, err = alone.Pieces(44100, 10*sec)
	if err != nil {
		t.Fatalf("Pieces: %v", err)
	}
	want[0].Audio = true
	if !slices.Equal(got, want) {
		t.Errorf("Pieces = %+v, want %+v", got, want)
	}

	// An audio FILE ahead of the data one is the sheet's first: its lead-in
	// is a lead-in.
	before := pieceFile(t, audio+"FILE \"disc.iso\" BINARY\n  TRACK 04 MODE1/2352\n    INDEX 01 00:00:00\n")
	if before.PrecededByData {
		t.Error("PrecededByData = true for an audio file ahead of the data one")
	}
}

// TestPiecesClipsTheDatatype: a refusal quotes the token at the package's
// operand bound, so a sheet cannot put a megabyte, or a terminal escape,
// into a 400 body.
func TestPiecesClipsTheDatatype(t *testing.T) {
	long := "MODE1/2048" + strings.Repeat("x", 4096)
	f := pieceFile(t, "FILE \"img.wav\" WAVE\n"+
		"  TRACK 01 "+long+"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 00:04:00\n"+
		"  TRACK 03 AUDIO\n    INDEX 01 00:08:00\n")
	_, err := f.Pieces(44100, 10*sec)
	if err == nil {
		t.Fatal("Pieces accepted a cooked data track ahead of audio")
	}
	if len(err.Error()) > 300 || !strings.Contains(err.Error(), "...") {
		t.Errorf("Pieces error is %d bytes and unclipped: %.120s", len(err.Error()), err)
	}
	if _, err := f.Cuts(44100); err == nil || len(err.Error()) > 300 {
		t.Errorf("Cuts error = %v, want a clipped refusal", err)
	}
}

// TestSingleFileFindsTheAudioFile: a sheet may give a disc's data track a
// FILE of its own beside the one file holding every audio track (EAC and
// XLD both write mixed-mode and Enhanced CD rips this way). That is one
// audio file, so it is the one to cut; a sheet whose FILEs each hold audio
// is a rip already split per track and is still refused.
func TestSingleFileFindsTheAudioFile(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			"a data FILE ahead of the audio",
			"FILE \"disc.iso\" BINARY\n  TRACK 01 MODE1/2352\n" +
				"FILE \"disc.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 03 AUDIO\n    INDEX 01 00:05:00\n",
			"disc.wav",
		},
		{
			"a data FILE behind the audio",
			"FILE \"album.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n" +
				"FILE \"track03.bin\" BINARY\n  TRACK 03 MODE1/2352\n    INDEX 01 00:00:00\n",
			"album.flac",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			f, err := s.SingleFile()
			if err != nil {
				t.Fatalf("SingleFile: %v", err)
			}
			if f.Name != tc.want {
				t.Errorf("SingleFile = %q, want %q", f.Name, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name, in, want string
	}{
		{
			"two audio FILEs",
			"FILE \"01.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
				"FILE \"02.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n",
			"indexes 2 files",
		},
		{
			"two audio FILEs and a data FILE",
			"FILE \"01.iso\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"FILE \"02.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n" +
				"FILE \"03.wav\" WAVE\n  TRACK 03 AUDIO\n    INDEX 01 00:00:00\n",
			"already separate",
		},
		{
			"no FILE holds audio",
			"FILE \"01.iso\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				"FILE \"02.iso\" BINARY\n  TRACK 02 MODE1/2352\n    INDEX 01 00:00:00\n",
			"audio",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			f, err := s.SingleFile()
			if err == nil {
				t.Fatalf("SingleFile = %q, want a refusal mentioning %q", f.Name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("SingleFile error = %v, want it to mention %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeInvalidRequest {
				t.Errorf("code = %v, want CodeInvalidRequest", code)
			}
		})
	}
}
