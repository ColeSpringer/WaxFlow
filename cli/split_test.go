package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// pcm16Format is the CD rip's own format at an arbitrary rate. A CUE rip is
// 44.1 kHz by definition, which writeWAV's fixed 48 kHz cannot stand in
// for: the whole point of a CD frame is that 44100 divides by 75.
func pcm16Format(rate, channels int) (pcm.Config, audio.Format) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	return cfg, cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
}

// rampWAVBytes renders a ramp as a WAV.
func rampWAVBytes(t *testing.T, rate, channels, frames int) []byte {
	t.Helper()
	cfg, f := pcm16Format(rate, channels)
	buf := testutil.Ramp(f, frames)
	defer audio.Put(buf)
	return wavBytes(t, cfg, buf)
}

// wavBytes encodes one buffer as a WAV.
func wavBytes(t *testing.T, cfg pcm.Config, buf *audio.Buffer) []byte {
	t.Helper()
	ws := &memWriteSeeker{}
	muxPCM(t, cfg, buf, riff.NewMuxer(ws, nil))
	return ws.b
}

// muxPCM encodes buf as PCM into m, header to trailer, declaring its length.
func muxPCM(t *testing.T, cfg pcm.Config, buf *audio.Buffer, m container.Muxer) {
	t.Helper()
	muxPCMSamples(t, cfg, buf, m, int64(buf.N))
}

// muxPCMSamples is muxPCM with the declared length spelled out, so a fixture
// can pass the -1 an engine uses for an unknown source length.
func muxPCMSamples(t *testing.T, cfg pcm.Config, buf *audio.Buffer, m container.Muxer, samples int64) {
	t.Helper()
	f := buf.Fmt
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(),
		Fmt: f, Samples: samples, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
}

// memWriteSeeker is an in-memory io.WriteSeeker, so the WAV muxer can
// back-patch its sizes without a temp file.
type memWriteSeeker struct {
	b   []byte
	pos int64
}

func (w *memWriteSeeker) Write(p []byte) (int, error) {
	if need := w.pos + int64(len(p)); need > int64(len(w.b)) {
		grown := make([]byte, need)
		copy(grown, w.b)
		w.b = grown
	}
	copy(w.b[w.pos:], p)
	w.pos += int64(len(p))
	return len(p), nil
}

func (w *memWriteSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 1:
		off += w.pos
	case 2:
		off += int64(len(w.b))
	}
	w.pos = off
	return off, nil
}

// cueRip writes a 44.1 kHz stereo WAV and a CUE sheet indexing it at the
// given CD-frame boundaries, which is the shape a real single-file rip has.
func cueRip(t *testing.T, dir string, frameStarts []int) (wav, sheet string, samples int64) {
	t.Helper()
	const rate, channels = 44100, 2
	// Long enough to hold the last track: the sheet's boundaries are CD
	// frames, so the file has to run past the last one.
	total := int64(frameStarts[len(frameStarts)-1])*588 + 44100
	raw := rampWAVBytes(t, rate, channels, int(total))
	wav = filepath.Join(dir, "album.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	sheet = writeSheet(t, filepath.Join(dir, "album.cue"), false, frameStarts)
	return wav, sheet, total
}

// writeSheet writes a sheet indexing album.wav at the given CD-frame starts,
// with a data track at frame 0 ahead of them when dataFirst is set and the
// audio then numbered from 2, as a mixed-mode disc numbers it.
func writeSheet(t *testing.T, path string, dataFirst bool, frameStarts []int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("PERFORMER \"The Band\"\nTITLE \"The Album\"\nFILE \"album.wav\" WAVE\n")
	first := 1
	if dataFirst {
		b.WriteString("  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		first = 2
	}
	for i, f := range frameStarts {
		mm, ss, ff := f/75/60, (f/75)%60, f%75
		fmt.Fprintf(&b, "  TRACK %02d AUDIO\n    TITLE \"Track %d\"\n    INDEX 01 %02d:%02d:%02d\n",
			i+first, i+first, mm, ss, ff)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// outputNames lists a split's output directory in name order.
func outputNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// pieceTags reads a written FLAC piece's tags.
func pieceTags(t *testing.T, path string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(context.Background(), container.BytesSource(raw), "flac", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return info.Tags
}

// TestSplitCueRejoinsGaplessly is M24a's end-to-end proof, and the reason
// the cue package exists at all: a real CUE sheet drives a real split, and the
// pieces rejoin into the original bit for bit.
//
// The boundaries are deliberately on CD frames that are not whole seconds
// (FF != 0), which is exactly where a seconds-based conversion rounds and
// drops or repeats a sample at every track join. FLAC at the source's own
// rate keeps it lossless, so "rejoins" means bit-exact rather than close.
func TestSplitCueRejoinsGaplessly(t *testing.T) {
	dir := t.TempDir()
	// Frame 0, then boundaries at FF = 37, 12 and 61: none of them a whole
	// second, none of them a round sample count.
	starts := []int{0, 5*75 + 37, 11*75 + 12, 18*75 + 61}
	wav, sheet, total := cueRip(t, dir, starts)
	out := filepath.Join(dir, "tracks")

	code, _, errOut := run(t, "split", wav, out, "--cue", sheet)
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(starts) {
		t.Fatalf("wrote %d pieces, want %d", len(entries), len(starts))
	}
	// The names carry the disc's numbering and the sheet's titles, zero
	// padded so a listing sorts the way the album plays.
	for i, e := range entries {
		want := fmt.Sprintf("%02d - Track %d.flac", i+1, i+1)
		if e.Name() != want {
			t.Errorf("piece %d is named %q, want %q", i, e.Name(), want)
		}
	}

	// Rejoin: concatenate the pieces and require the original back.
	e := waxflow.New()
	members := make([]waxflow.ConcatSource, len(entries))
	for i, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(out, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Probe(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		members[i] = waxflow.ConcatSource{
			Track: info.Default(),
			Open:  func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), "flac") },
		}

		// Each piece must be exactly as long as the sheet's own frame
		// arithmetic says, and this is the assertion the rejoin below
		// cannot make. Cut points are boundaries: shift every one of them
		// by a sample and the pieces still concatenate back into the
		// original perfectly, while every track begins a sample late. So
		// the round trip proves Slice and Concat are inverses, and only
		// this proves the cuts land where the sheet put them. It is what
		// fails if the CD-frame conversion is ever routed through
		// time.Duration.
		//
		// The expected boundary is spelled out here (588 samples per CD
		// frame at 44100) rather than taken from cue.Samples, which would
		// make the test move with the code it is checking and pass for any
		// conversion at all.
		wantEnd := total
		if i+1 < len(starts) {
			wantEnd = int64(starts[i+1]) * 588
		}
		if want := wantEnd - int64(starts[i])*588; info.Default().Samples != want {
			t.Fatalf("piece %d is %d samples, want %d: the cut is not on the frame the sheet names",
				i+1, info.Default().Samples, want)
		}
	}
	joined, err := waxflow.Concat(members, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer joined.Close()
	if got := joined.Info().Default().Samples; got != total {
		t.Fatalf("the rejoined album is %d samples, the rip was %d: a sample was lost or repeated at a cut",
			got, total)
	}

	orig, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	srcMed, err := e.OpenStream(container.BytesSource(orig), "wav")
	if err != nil {
		t.Fatal(err)
	}
	defer srcMed.Close()

	// Both are drained whole rather than compared chunk by chunk: a
	// timeline returns a short chunk at every seam by design (a chunk never
	// spans one), so chunk boundaries legitimately differ between the
	// rejoined album and the original. Only the samples have to agree.
	f := joined.Info().Default().Fmt
	got := drainAll(t, joined, int(total))
	defer audio.Put(got)
	want := drainAll(t, srcMed, int(total))
	defer audio.Put(want)

	if got.N != int(total) || want.N != int(total) {
		t.Fatalf("rejoined %d samples and the original %d, want %d each", got.N, want.N, total)
	}
	for ch := range f.Channels {
		ra, rb := got.ChanI(ch), want.ChanI(ch)
		for i := range got.N {
			if ra[i] != rb[i] {
				t.Fatalf("channel %d sample %d differs: rejoined %d, original %d. "+
					"The cut is not sample-exact.", ch, i, ra[i], rb[i])
			}
		}
	}
}

// drainAll reads a Media to end of stream into one buffer.
func drainAll(t *testing.T, med format.Media, capacity int) *audio.Buffer {
	t.Helper()
	f := med.Info().Default().Fmt
	out := audio.Get(f, capacity)
	out.N = 0
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.N+tmp.N > out.Cap() {
			t.Fatalf("delivered more than the %d frames promised", capacity)
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
}

// TestCuePieces pins both shapes a sheet comes in, against the funnel both
// this and the daemon's split job cut by.
//
// The shapes differ by one thing: whether TRACK 01's INDEX 01 is at frame 0.
// The overwhelmingly common sheet says it is, and its pieces are its tracks.
// A sheet that says otherwise has audio before track 1 (a pregap, or hidden
// track one audio, which on some discs is a whole song), and that audio is a
// piece of its own: folding it into track 1 would make track 1 start where
// the sheet says it does not, and dropping it would destroy a song. There is
// then a piece more than the sheet has tracks, which is the off-by-one every
// caller pairing titles by position has to survive.
//
// The boundaries are spelled out here (588 samples per CD frame at 44100)
// rather than taken from the code under test, which would agree with any
// conversion at all.
func TestCuePieces(t *testing.T) {
	const total = 400_000
	for _, tc := range []struct {
		name   string
		starts []int
		want   []piece
		tracks int
	}{{
		name:   "track 1 at frame 0",
		starts: []int{0, 100, 250},
		want: []piece{
			{from: 0, to: 100 * 588, title: "Track 1", number: 1},
			{from: 100 * 588, to: 250 * 588, title: "Track 2", number: 2},
			{from: 250 * 588, to: waxflow.ToEnd, title: "Track 3", number: 3},
		},
		tracks: 3,
	}, {
		// The shape the cue package's own sheetBasic fixture has.
		name:   "track 1 past frame 0",
		starts: []int{33, 412},
		want: []piece{
			// The lead-in: the disc's track 0, which no sheet line names, so
			// it takes no title and invents none.
			{from: 0, to: 33 * 588, title: "", number: 0},
			{from: 33 * 588, to: 412 * 588, title: "Track 1", number: 1},
			{from: 412 * 588, to: waxflow.ToEnd, title: "Track 2", number: 2},
		},
		// TRACKTOTAL counts the disc's tracks, which a lead-in is not.
		tracks: 2,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, sheet, _ := cueRip(t, t.TempDir(), tc.starts)
			got, tracks, err := cuePieces(sheet, 44100, total)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%d pieces, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("piece %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			if tracks != tc.tracks {
				t.Errorf("tracks = %d, want %d", tracks, tc.tracks)
			}
		})
	}
}

// mixedModeSheet is a single-file image of a mixed-mode disc: the data
// track first at frame 0, then the audio tracks at the given frame starts,
// numbered from 2 as the disc numbers them.
func mixedModeSheet(t *testing.T, dir string, audioStarts []int) string {
	t.Helper()
	return writeSheet(t, filepath.Join(dir, "mixed.cue"), true, audioStarts)
}

// TestCuePiecesSkipsADataTrack: a mixed-mode disc's data track is a piece
// the split names and does not write, and the disc's numbering survives:
// the audio is tracks 2 and 3 of a 3-track disc, not 1 and 2 of 2.
func TestCuePiecesSkipsADataTrack(t *testing.T) {
	dir := t.TempDir()
	sheet := mixedModeSheet(t, dir, []int{100, 250})
	got, tracks, err := cuePieces(sheet, 44100, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	want := []piece{
		{from: 0, to: 100 * 588, number: 1, skip: `TRACK 01 "MODE1/2352", a data track, not audio`},
		{from: 100 * 588, to: 250 * 588, title: "Track 2", number: 2},
		{from: 250 * 588, to: waxflow.ToEnd, title: "Track 3", number: 3},
	}
	if !slices.Equal(got, want) {
		t.Errorf("pieces = %+v, want %+v", got, want)
	}
	if tracks != 3 {
		t.Errorf("tracks = %d, want 3: the data track is one of the disc's", tracks)
	}

	// A data track the sheet gives a FILE of its own is not in this file
	// at all, and still counts as one of the disc's tracks; the audio
	// before this file's first INDEX 01 is its pregap, not a lead-in.
	beside := filepath.Join(dir, "beside.cue")
	if err := os.WriteFile(beside, []byte(
		"FILE \"album.iso\" BINARY\n  TRACK 01 MODE1/2352\n"+
			"FILE \"album.wav\" WAVE\n  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 00 00:00:00\n    INDEX 01 00:00:30\n"+
			"  TRACK 03 AUDIO\n    TITLE \"Three\"\n    INDEX 01 00:01:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, tracks, err = cuePieces(beside, 44100, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	want = []piece{
		{from: 0, to: 30 * 588, skip: "the pregap after the disc's data track, not audio"},
		{from: 30 * 588, to: 75 * 588, title: "Two", number: 2},
		{from: 75 * 588, to: waxflow.ToEnd, title: "Three", number: 3},
	}
	if !slices.Equal(got, want) {
		t.Errorf("pieces = %+v, want %+v", got, want)
	}
	if tracks != 3 {
		t.Errorf("tracks = %d, want 3: the data track is one of the disc's", tracks)
	}

	// An Enhanced CD's data track sits in a second session after the
	// audio, which a CD player never counts: the audio is 1 and 2 of 2.
	enhanced := filepath.Join(dir, "enhanced.cue")
	if err := os.WriteFile(enhanced, []byte(
		"FILE \"album.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"+
			"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:01:00\n"+
			"  TRACK 03 MODE1/2352\n    INDEX 01 05:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, tracks, err = cuePieces(enhanced, 44100, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	want = []piece{
		{from: 0, to: 75 * 588, title: "One", number: 1},
		{from: 75 * 588, to: waxflow.ToEnd, title: "Two", number: 2},
	}
	if !slices.Equal(got, want) {
		t.Errorf("pieces = %+v, want %+v", got, want)
	}
	if tracks != 2 {
		t.Errorf("tracks = %d, want 2: a data track after the last audio track is not one of the disc's", tracks)
	}
}

// TestCuePiecesTrackZeroIsATrack: a sheet may number its first track 00,
// and that track is a track, not the lead-in its number resembles.
func TestCuePiecesTrackZeroIsATrack(t *testing.T) {
	sheet := filepath.Join(t.TempDir(), "album.cue")
	if err := os.WriteFile(sheet, []byte("FILE \"album.wav\" WAVE\n"+
		"  TRACK 00 AUDIO\n    TITLE \"Zero\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:01:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := cuePieces(sheet, 44100, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	want := []piece{
		{from: 0, to: 44100, title: "Zero", number: 0},
		{from: 44100, to: waxflow.ToEnd, title: "One", number: 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("pieces = %+v, want %+v", got, want)
	}
}

// TestCuePiecesReadsWhatTheSheetMeans: a hand-written sheet names no FILE
// and skips the quotes, and splits by the whole title all the same.
func TestCuePiecesReadsWhatTheSheetMeans(t *testing.T) {
	sheet := filepath.Join(t.TempDir(), "album.cue")
	if err := os.WriteFile(sheet, []byte(
		"TRACK 01 AUDIO\n  TITLE Track One\n  INDEX 01 00:00:00\n"+
			"TRACK 02 AUDIO\n  TITLE \"Track Two\"\n  INDEX 01 00:01:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := cuePieces(sheet, 44100, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	want := []piece{
		{from: 0, to: 44100, title: "Track One", number: 1},
		{from: 44100, to: waxflow.ToEnd, title: "Track Two", number: 2},
	}
	if !slices.Equal(got, want) {
		t.Errorf("pieces = %+v, want %+v", got, want)
	}
}

// TestSplitCueTrackTotal: TRACKTOTAL counts the disc's tracks, so a lead-in
// piece is not one and a TRACK 00 is.
func TestSplitCueTrackTotal(t *testing.T) {
	dir := t.TempDir()
	wav, leadIn, _ := cueRip(t, dir, []int{33, 412})
	zero := filepath.Join(dir, "zero.cue")
	if err := os.WriteFile(zero, []byte("FILE \"album.wav\" WAVE\n"+
		"  TRACK 00 AUDIO\n    TITLE \"Zero\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:05:37\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sheet string
		names []string
	}{
		{leadIn, []string{"00.flac", "01 - Track 1.flac", "02 - Track 2.flac"}},
		{zero, []string{"00 - Zero.flac", "01 - One.flac"}},
	} {
		out := filepath.Join(dir, "out-"+filepath.Base(tc.sheet))
		if code, _, errOut := run(t, "split", wav, out, "--cue", tc.sheet); code != 0 {
			t.Fatalf("split exit = %d: %s", code, errOut)
		}
		for _, name := range tc.names {
			if got := pieceTags(t, filepath.Join(out, name))["TRACKTOTAL"]; len(got) != 1 || got[0] != "2" {
				t.Errorf("%s: %s carries TRACKTOTAL %v, want [2]", filepath.Base(tc.sheet), name, got)
			}
		}
	}
}

// TestSplitCueRefusesSharedNames: pieces that would share a name are refused
// before anything is written, since --force would let the later one replace
// the earlier. A lead-in and an untitled TRACK 00 are both 00.
func TestSplitCueRefusesSharedNames(t *testing.T) {
	dir := t.TempDir()
	wav, _, _ := cueRip(t, dir, []int{33, 412})
	sheet := filepath.Join(dir, "zero.cue")
	if err := os.WriteFile(sheet, []byte("FILE \"album.wav\" WAVE\n"+
		"  TRACK 00 AUDIO\n    INDEX 01 00:00:33\n  TRACK 01 AUDIO\n    INDEX 01 00:05:37\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "tracks")
	for _, flag := range []string{"--dry-run", "--force"} {
		code, _, errOut := run(t, "split", wav, out, "--cue", sheet, flag)
		if code == 0 || !strings.Contains(errOut, `pieces 1 and 2 would both be named "00.flac"`) {
			t.Errorf("split %s exit = %d, stderr %q; want the shared name refused", flag, code, errOut)
		}
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("wrote %d files before refusing", len(entries))
	}
}

// TestSanitizeFilenameMapsReservedCharacters: a title keeps its inner quotes
// now, and a quote, like ?:*<>|, is a character Windows and FAT refuse in a
// name, so a piece is named the same on every system.
func TestSanitizeFilenameMapsReservedCharacters(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{`The "Best" Of`, `The 'Best' Of`},
		{`Symphony No. 5: Allegro`, `Symphony No. 5- Allegro`},
		{`Why? <Live|Take*2>`, `Why- -Live-Take-2-`},
	} {
		if got := sanitizeFilename(tc.title); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

// TestSplitCueLeadInBecomesAPiece is the end of the same story: the lead-in
// audio reaches the disk, named and whole. A hidden track one is a song, and
// a split that quietly drops it loses the only copy the rip had.
func TestSplitCueLeadInBecomesAPiece(t *testing.T) {
	dir := t.TempDir()
	wav, sheet, total := cueRip(t, dir, []int{33, 412})
	out := filepath.Join(dir, "tracks")
	code, _, errOut := run(t, "split", wav, out, "--cue", sheet)
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	names := outputNames(t, out)
	// Track 0 sorts ahead of track 1, which is where it plays.
	want := []string{"00.flac", "01 - Track 1.flac", "02 - Track 2.flac"}
	if len(names) != len(want) {
		t.Fatalf("wrote %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("piece %d is named %q, want %q", i, names[i], want[i])
		}
	}

	e := waxflow.New()
	// Nothing discarded: the pieces' lengths add back up to the whole rip,
	// and the lead-in is the sheet's own frame 33.
	var sum int64
	for i, name := range names {
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Probe(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if got := info.Default().Samples; got != 33*588 {
				t.Errorf("the lead-in is %d samples, want the sheet's %d", got, 33*588)
			}
		}
		sum += info.Default().Samples
	}
	if sum != total {
		t.Errorf("the pieces hold %d samples, the rip was %d: the split lost audio", sum, total)
	}
}

// TestSplitAtSampleOffsets covers the other cut source: explicit sample
// offsets, which is what a caller with its own boundaries (a chapter list,
// a silence map) has.
func TestSplitAtSampleOffsets(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", wav, out, "--at", "30000,70000")
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	// Two interior cuts make three pieces: the offsets are boundaries, not
	// starts, so neither 0 nor the end is written by the caller.
	if len(entries) != 3 {
		t.Fatalf("wrote %d pieces, want 3 from two interior cuts", len(entries))
	}
}

// TestSplitDryRunWritesNothing holds --dry-run to its word. The output
// directory itself is the assertion: an empty ReadDir passes just as well
// against a run that created the tree and then printed, which is exactly
// what a dry run against a mistyped path must not do.
func TestSplitDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	wav, sheet, _ := cueRip(t, dir, []int{0, 30 * 75})
	out := filepath.Join(dir, "tracks")
	code, stdout, errOut := run(t, "split", wav, out, "--cue", sheet, "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	if !strings.Contains(stdout, "01 - Track 1.flac") || !strings.Contains(stdout, "[0, ") {
		t.Errorf("dry run output does not list the pieces and their ranges:\n%s", stdout)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("--dry-run created %s (stat err = %v), which it promised not to write", out, err)
	}
}

// TestSplitDryRunPrintsTheMeasuredEnd: the last piece is open-ended, and the
// print has to resolve that against the source rather than show its sentinel.
func TestSplitDryRunPrintsTheMeasuredEnd(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, errOut := run(t, "split", wav, filepath.Join(dir, "out"), "--at", "30000", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	if !strings.Contains(stdout, "[30000, 100000)") {
		t.Errorf("the last piece's range is not the source's own end:\n%s", stdout)
	}
}

// TestSplitDefaultsToTheEncoderDefaultLevel pins the --flac-level default.
//
// The flag's 0 has to mean level 0, so it maps to the FLACLevelFastest
// sentinel (a plain 0 in the options selects the encoder default). That remap
// only leaves the default reachable if the flag itself defaults to something
// other than 0: with a 0 default, a bare split writes the least compressed
// FLAC there is and level 5 cannot be asked for at all.
//
// The output bytes are the signal, compared against both levels written
// explicitly: the encoder is deterministic, so a bare piece is byte-identical
// to the level it actually used. A sine is the fixture because LPC is what
// the levels differ by, and a ramp is predicted exactly by the fixed
// predictors level 0 already has.
func TestSplitDefaultsToTheEncoderDefaultLevel(t *testing.T) {
	dir := t.TempDir()
	cfg, f := pcm16Format(44100, 2)
	buf := testutil.Sine(f, 100_000, 440, 0.5)
	defer audio.Put(buf)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, wavBytes(t, cfg, buf), 0o644); err != nil {
		t.Fatal(err)
	}

	piece := func(t *testing.T, name string, args ...string) []byte {
		t.Helper()
		out := filepath.Join(dir, name)
		code, _, errOut := run(t, append([]string{"split", wav, out, "--at", "50000"}, args...)...)
		if code != 0 {
			t.Fatalf("split exit = %d: %s", code, errOut)
		}
		raw, err := os.ReadFile(filepath.Join(out, "01.flac"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	bare := piece(t, "bare")
	level0 := piece(t, "level0", "--flac-level", "0")
	level5 := piece(t, "level5", "--flac-level", "5")

	if bytes.Equal(level0, level5) {
		t.Fatal("levels 0 and 5 wrote identical bytes: this fixture cannot tell the levels apart")
	}
	if bytes.Equal(bare, level0) {
		t.Errorf("a bare split wrote level 0 (%d bytes), the least compressed FLAC there is", len(bare))
	}
	if !bytes.Equal(bare, level5) {
		t.Errorf("a bare split wrote %d bytes, level 5 writes %d: the default is not the encoder's default",
			len(bare), len(level5))
	}
}

// TestSplitPieceExtension: the piece's name comes from the engine's output
// table, so it names the file that was actually written. --container writes a
// different kind of file and so a different extension, and a format whose row
// claims no extension of its own (alac) still has a name its output plays
// under. A piece called .alac, or Matroska bytes called .flac, is what this
// refuses.
func TestSplitPieceExtension(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"flac in matroska", []string{"--container", "mka"}, "01.mka"},
		{"alac", []string{"--format", "alac"}, "01.m4a"},
		{"wavpack", []string{"--format", "wavpack"}, "01.wv"},
		{"ape", []string{"--format", "ape"}, "01.ape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name)
			code, _, errOut := run(t, append([]string{"split", wav, out, "--at", "50000"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("split exit = %d: %s", code, errOut)
			}
			path := filepath.Join(out, tc.want)
			piece, err := os.ReadFile(path)
			if err != nil {
				entries, _ := os.ReadDir(out)
				var got []string
				for _, e := range entries {
					got = append(got, e.Name())
				}
				t.Fatalf("no piece named %s; the split wrote %v", tc.want, got)
			}
			// The extension has to be the one the bytes are, not just a
			// plausible string: probing by it is what a player does.
			if _, err := waxflow.New().Probe(container.BytesSource(piece), strings.TrimPrefix(filepath.Ext(path), "."), nil); err != nil {
				t.Errorf("%s does not hold what its name says: %v", tc.want, err)
			}
		})
	}
}

// TestSplitWavPackWritesItsOwnTags pins the mux half of the wavpack tag
// contract: the wv muxer writes the APEv2 block itself, so the pieces carry
// their tags with no post-pass. The stderr check can catch only a FAILING
// post-pass, and a wrongly-run one now succeeds silently (the tag library
// rewrites APEv2 fine), so the skip itself is pinned where it lives, by
// TestOutputEmbedsTags in the engine.
func TestSplitWavPackWritesItsOwnTags(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", wav, out, "--format", "wavpack", "--at", "50000")
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	if strings.Contains(errOut, "post-pass") {
		t.Errorf("a format whose muxer writes its own tags ran the post-pass anyway: %s", errOut)
	}
	piece, err := os.ReadFile(filepath.Join(out, "02.wv"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(container.BytesSource(piece), "wv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Tags["TRACKNUMBER"]; len(got) != 1 || got[0] != "2" {
		t.Errorf("piece 2 carries TRACKNUMBER %v, want [2]; the mux wrote no tags", got)
	}
}

func TestSplitRejects(t *testing.T) {
	dir := t.TempDir()
	wav, sheet, _ := cueRip(t, dir, []int{0, 30 * 75})

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no cut source", []string{"split", wav, filepath.Join(dir, "a")}, "--cue or --at"},
		{"both cut sources", []string{"split", wav, filepath.Join(dir, "b"), "--cue", sheet, "--at", "100"}, "exclusive"},
		{"cut past the end", []string{"split", wav, filepath.Join(dir, "c"), "--at", "99999999"}, "past the source's"},
		{"descending cuts", []string{"split", wav, filepath.Join(dir, "d"), "--at", "5000,1000"}, "ascend"},
		{"zero cut", []string{"split", wav, filepath.Join(dir, "e"), "--at", "0"}, "positive sample offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := run(t, tc.args...)
			if code == 0 {
				t.Fatalf("exit = 0, want a failure mentioning %q", tc.want)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", errOut, tc.want)
			}
		})
	}
}

// underDeclaredMKA writes a Matroska file that holds frames samples and says it
// holds fewer, which the format allows: a Duration is advisory, so a muxer that
// rounds it down declares less audio than it wrote. Our muxer does not round,
// so the value is rewritten afterward. It is the source a split has to survive.
func underDeclaredMKA(t *testing.T, rate, channels, frames int) []byte {
	t.Helper()
	cfg, f := pcm16Format(rate, channels)
	buf := testutil.Ramp(f, frames)
	defer audio.Put(buf)
	var b bytes.Buffer
	muxPCM(t, cfg, buf, mka.NewMuxer(&b, nil))

	// A millisecond short of the true length. What the demuxer makes of it is
	// the demuxer's arithmetic, so the caller probes for the declared length
	// rather than predicting it here.
	ms := math.Floor(float64(frames)/float64(rate)*1000) - 1
	return rewriteMKADuration(t, b.Bytes(), ms)
}

// rewriteMKADuration overwrites the Duration in Info. The element is a fixed
// eleven bytes, so this changes no length and needs no splice.
func rewriteMKADuration(t *testing.T, raw []byte, ticks float64) []byte {
	t.Helper()
	// Bound the search to the header, off audio that spells the same bytes.
	// The anchor is the Duration's ID plus its size vint, not Info's own ID,
	// which also appears earlier as a SeekHead SeekID payload.
	const headWindow = 512
	head := raw[:min(headWindow, len(raw))]
	dur := []byte{0x44, 0x89, 0x88}
	at := bytes.Index(head, dur)
	if at < 0 {
		t.Fatal("no Info > Duration in the muxed header to rewrite")
	}
	if bytes.Contains(head[at+len(dur):], dur) {
		t.Fatal("more than one Duration-shaped run in the header; the anchor is ambiguous")
	}
	out := append([]byte(nil), raw...)
	binary.BigEndian.PutUint64(out[at+len(dur):], math.Float64bits(ticks))
	return out
}

// TestSplitMeasuresAnUnderDeclaredSource is the last piece's whole problem: a
// cut list checked against a measured length, and pieces then held to the
// declared one, abort the split on the final piece with everything before it
// already on disk, naming the very length the CLI decided not to trust.
//
// The last piece's own length is the assertion, not just the exit code: it
// has to run to the source's true end. Held to the declaration it would be
// short by the samples the header forgot, which is a silent loss where the
// abort was at least loud.
func TestSplitMeasuresAnUnderDeclaredSource(t *testing.T) {
	dir := t.TempDir()
	const actual = 100_000
	raw := underDeclaredMKA(t, 44100, 2, actual)
	src := filepath.Join(dir, "rip.mka")
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The fixture only tests anything if the headers really do under-declare,
	// and the probe is what says whether they do.
	info, err := waxflow.New().Probe(container.BytesSource(raw), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	declared := info.Default().Samples
	if declared < 0 || declared >= actual || info.Default().SamplesExact {
		t.Fatalf("the fixture probes as %d samples (exact %v), want an advisory length short of %d",
			declared, info.Default().SamplesExact, actual)
	}

	const cut = 30_000
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", src, out, "--at", strconv.Itoa(cut))
	if code != 0 {
		t.Fatalf("split exit = %d (the source declares %d and holds %d): %s", code, declared, actual, errOut)
	}
	e := waxflow.New()
	var sum int64
	for i, name := range []string{"01.flac", "02.flac"} {
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Probe(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := info.Default().Samples
		if want := []int64{cut, actual - cut}[i]; got != want {
			t.Errorf("%s is %d samples, want %d: the piece ran to the declared %d, not the source's %d",
				name, got, want, declared, actual)
		}
		sum += got
	}
	// Nothing lost at the end: the audio the header forgot to declare is
	// audio, and the last piece is what holds it.
	if sum != actual {
		t.Errorf("the pieces hold %d samples, the source holds %d", sum, actual)
	}
}

// TestSplitMeasuresASourceWithNoDeclaredLength: a container is free to declare
// no length at all, which our Matroska muxer does when it has no projection and
// cannot seek back to fill one in. Every cut point is checked against the
// source's length, so trusting that non-answer refuses every cut in the list.
func TestSplitMeasuresASourceWithNoDeclaredLength(t *testing.T) {
	dir := t.TempDir()
	const actual = 100_000
	cfg, f := pcm16Format(44100, 2)
	buf := testutil.Ramp(f, actual)
	defer audio.Put(buf)
	var b bytes.Buffer
	muxPCMSamples(t, cfg, buf, mka.NewMuxer(&b, nil), -1)
	src := filepath.Join(dir, "rip.mka")
	if err := os.WriteFile(src, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(container.BytesSource(b.Bytes()), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Default().Samples; got >= 0 {
		t.Fatalf("the fixture declares %d samples, so it does not test an undeclared length", got)
	}

	const cut = 30_000
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", src, out, "--at", strconv.Itoa(cut))
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	e := waxflow.New()
	for i, name := range []string{"01.flac", "02.flac"} {
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		pieceInfo, err := e.Probe(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := []int64{cut, actual - cut}[i]; pieceInfo.Default().Samples != want {
			t.Errorf("%s is %d samples, want %d", name, pieceInfo.Default().Samples, want)
		}
	}
}

// TestSplitCarriesCoverArt: a piece of a rip is a track of the album and
// carries the album's cover, the same passthrough a transcode of the same
// source gives. Nothing about being cut makes the art less the source's.
//
// Both halves of that passthrough are here, because they are separate paths
// and only one runs per output: the MP4 muxers embed the art at mux time
// (from the options), and every other output gets it from the metadata
// post-pass on the finished file. A test on one alone passes with the other
// deleted.
func TestSplitCarriesCoverArt(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The source has to be a format that holds art, so the rip is a FLAC
	// (which is what a single-file rip is) rather than the WAV.
	src := filepath.Join(dir, "album.flac")
	if code, _, errOut := run(t, "transcode", wav, src); code != 0 {
		t.Fatalf("building the source: %s", errOut)
	}
	art := tinyPNG(t)
	ctx := context.Background()
	if err := label.New().Apply(ctx, src, &meta.Info{
		Tags:     map[string][]string{"ALBUM": {"The Album"}},
		Pictures: []meta.Picture{{MIME: "image/png", Front: true, Data: art}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		args   []string
		pieces []string
	}{
		{"flac, through the metadata post-pass", nil, []string{"01.flac", "02.flac"}},
		// Progressive spelled out rather than left to the default (which
		// already picks it for a file output), so the cell pins the flat form
		// it reads back. The art rides the same options field into both forms.
		{"alac, embedded by the muxer", []string{"--format", "alac", "--container", "progressive"},
			[]string{"01.m4a", "02.m4a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name)
			code, _, errOut := run(t, append([]string{"split", src, out, "--at", "50000"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("split exit = %d: %s", code, errOut)
			}
			for _, name := range tc.pieces {
				piece, err := os.ReadFile(filepath.Join(out, name))
				if err != nil {
					t.Fatal(err)
				}
				info, err := label.New().Read(ctx, container.BytesSource(piece),
					strings.TrimPrefix(filepath.Ext(name), "."), meta.ReadOptions{Pictures: true})
				if err != nil {
					t.Fatal(err)
				}
				p := info.FrontPicture()
				if p == nil {
					t.Errorf("%s carries no cover art (metadata notes: %v)", name, info.Warnings)
					continue
				}
				if !bytes.Equal(p.Data, art) {
					t.Errorf("%s carries %d bytes of art, want the source's %d", name, len(p.Data), len(art))
				}
				if got := info.Tags["ALBUM"]; len(got) != 1 || got[0] != "The Album" {
					t.Errorf("%s carries ALBUM %q, want the album's", name, got)
				}
			}
		})
	}
}

// TestSplitDropsTheAlbumTimeline: the metadata post-pass writes the source's
// whole Info onto each finished piece, and a rip's chapter list is a set of
// marks on the rip's timeline. Copied onto a piece unchanged, every mark the
// album has lands in every piece, at times the piece does not reach: a mark
// at two seconds inside a piece that is one second long. Tags describe the
// album and travel; a timeline describes the file and does not.
func TestSplitDropsTheAlbumTimeline(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000) // ~2.27s
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "album.flac")
	if code, _, errOut := run(t, "transcode", wav, src); code != 0 {
		t.Fatalf("building the source: %s", errOut)
	}
	ctx := context.Background()
	if err := label.New().Apply(ctx, src, &meta.Info{
		Tags: map[string][]string{"ALBUM": {"The Album"}},
		Chapters: []container.Chapter{
			{Start: 0, Title: "One"},
			{Start: 2 * time.Second, Title: "Two"},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// A cut at ~1.13s, so the album's second mark is past the first piece's
	// end and the first mark is past the second piece's start.
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", src, out, "--at", "50000")
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	for _, name := range []string{"01.flac", "02.flac"} {
		piece, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		info, err := label.New().Read(ctx, container.BytesSource(piece), "flac", meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Chapters) != 0 {
			t.Errorf("%s carries %d of the album's chapters (%+v): they are marks on the album's timeline, not this piece's",
				name, len(info.Chapters), info.Chapters)
		}
		// The tags are not the timeline and do ride along: this must not pass
		// by way of the piece carrying no metadata at all.
		if got := info.Tags["ALBUM"]; len(got) != 1 || got[0] != "The Album" {
			t.Errorf("%s carries ALBUM %q, want the album's", name, got)
		}
	}
}

// tinyPNG is a 1x1 PNG: the tag library reads the payload, so it has to be
// an image, and nothing here cares which.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(
		"89504e470d0a1a0a0000000d4948445200000001000000010806000000" +
			"1f15c4890000000a49444154789c63000100000500010d0a2db40000000049454e44ae426082")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSanitizeFilenameKeepsRunesWhole: the name is cut to a byte budget, and
// a byte cut through a multi-byte rune leaves a filename that is not text.
//
// Each fixture checks itself first, because most of the obvious ones prove
// nothing: 2-, 3- and 4-byte runes all divide the budget exactly, so a title
// made of one width alone is cut on a boundary however long it is, and passes
// a byte slice just as happily. The cut only lands inside a rune when
// something ahead of it does not divide, which is what the ASCII prefixes are
// doing here.
func TestSanitizeFilenameKeepsRunesWhole(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
	}{
		{"two bytes into a three-byte rune", "a" + strings.Repeat("あ", 40)},
		{"two bytes into a four-byte rune", "ab" + strings.Repeat("🎵", 30)},
		{"one byte into a four-byte rune", "abc" + strings.Repeat("🎵", 30)},
		// A seven-byte group, so the budget lands inside a rune rather than
		// on the boundary every width that divides 120 gives.
		{"mixed widths", strings.Repeat("🎵abc", 30)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.title) <= maxTitleBytes {
				t.Fatalf("the title is %d bytes, inside the %d budget: it is never cut", len(tc.title), maxTitleBytes)
			}
			if utf8.ValidString(tc.title[:maxTitleBytes]) {
				t.Fatalf("the budget falls on a rune boundary in this title, so a byte cut would pass it too")
			}
			got := sanitizeFilename(tc.title)
			if !utf8.ValidString(got) {
				t.Errorf("sanitizeFilename(%d bytes) = %q, which is not valid UTF-8", len(tc.title), got)
			}
			if len(got) > maxTitleBytes {
				t.Errorf("name is %d bytes, past the %d budget", len(got), maxTitleBytes)
			}
			// The cut costs the rune it landed in and nothing else, so what
			// survives is a prefix of the title's own runes.
			if !strings.HasPrefix(tc.title, got) {
				t.Errorf("name %q is not a prefix of the title", got)
			}
			if len(got) < maxTitleBytes-utf8.UTFMax {
				t.Errorf("name is %d bytes, want within one rune of the %d budget: the cut took more than the rune it landed in",
					len(got), maxTitleBytes)
			}
		})
	}
}

// TestTruncateTitle covers the budget itself, which the caller's fixed 120
// cannot reach: a cut that lands inside the first rune has nothing valid to
// fall back to and gives back nothing, which is the caller's "untitled" case
// rather than a name.
func TestTruncateTitle(t *testing.T) {
	for _, tc := range []struct {
		s    string
		max  int
		want string
	}{
		{"abc", 10, "abc"},   // inside the budget, untouched
		{"abcdef", 3, "abc"}, // ASCII cuts anywhere
		{"a🎵", 3, "a"},       // one byte into the rune
		{"🎵🎵", 6, "🎵"},       // on the boundary between them
		{"🎵🎵", 2, ""},        // the whole remainder is one rune, so nothing survives
		{"🎵", 0, ""},
	} {
		if got := truncateTitle(tc.s, tc.max); got != tc.want {
			t.Errorf("truncateTitle(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
		}
	}
	// The one long rune reaches the caller as a name, not as an empty one.
	if got := sanitizeFilename("🎵"); got != "🎵" {
		t.Errorf("sanitizeFilename(%q) = %q", "🎵", got)
	}
}

// TestSplitCueMultiFileRefused: a track-per-file sheet has nothing to cut,
// and splitting its first file would be a plausible wrong answer.
func TestSplitCueMultiFileRefused(t *testing.T) {
	dir := t.TempDir()
	wav, _, _ := cueRip(t, dir, []int{0, 30 * 75})
	sheet := filepath.Join(dir, "multi.cue")
	if err := os.WriteFile(sheet, []byte(
		"FILE \"01.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"+
			"FILE \"02.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "split", wav, filepath.Join(dir, "out"), "--cue", sheet)
	if code == 0 {
		t.Fatal("a track-per-file sheet was accepted")
	}
	if !strings.Contains(errOut, "already separate") {
		t.Errorf("stderr = %q", errOut)
	}
}

// TestSplitAPEWritesItsOwnTags is TestSplitWavPackWritesItsOwnTags for the
// other lossless row whose muxer owns its APEv2 block. The two are separate
// tests because the skip is keyed on the format name: a row added without an
// arm there reruns the tag rewrite over every piece the mux already tagged,
// which is the failure that looks like success. Each format's arm is pinned
// by TestOutputEmbedsTags in the engine.
func TestSplitAPEWritesItsOwnTags(t *testing.T) {
	dir := t.TempDir()
	raw := rampWAVBytes(t, 44100, 2, 100_000)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", wav, out, "--format", "ape", "--at", "50000")
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	if strings.Contains(errOut, "post-pass") {
		t.Errorf("a format whose muxer writes its own tags ran the post-pass anyway: %s", errOut)
	}
	piece, err := os.ReadFile(filepath.Join(out, "02.ape"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(container.BytesSource(piece), "ape", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Tags["TRACKNUMBER"]; len(got) != 1 || got[0] != "2" {
		t.Errorf("piece 2 carries TRACKNUMBER %v, want [2]; the mux wrote no tags", got)
	}
}

// measureFailMedia is a Media whose measuring seek fails with the error it
// was given. The other methods are explicit so a widened measureMedia fails
// the test instead of panicking on a nil embedded interface.
//
// Its Info carries one track with no declared length, which is what a Media
// always carries: all three of format's constructors refuse a trackless
// demuxer, so Info().Default() has something to return. The length sends
// measureMedia past its two cheap routes to the seek this is about, and the
// type deliberately does not implement format.Walker, so there is no walk to
// answer instead.
type measureFailMedia struct{ err error }

func (m *measureFailMedia) Info() *format.Info {
	return &format.Info{Tracks: []container.Track{{Samples: -1, Default: true}}}
}
func (m *measureFailMedia) Close() error { return nil }
func (m *measureFailMedia) ReadChunk(*audio.Buffer) error {
	return errors.New("measureFailMedia: read")
}
func (m *measureFailMedia) SeekSample(int64) (int64, error) { return 0, m.err }

// TestMeasureKeepsTheWalkCode pins that measuring reports the walk's own
// classification. A damaged file fails its exact-length walk as
// malformed-input, and re-coding that as source-unreadable told the user
// their disk was bad; the server's measure passes the walk's error through.
// An error with no code is a walk that failed to classify, which is a
// waxflow bug, and exit class internal is what says so.
func TestMeasureKeepsTheWalkCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want waxerr.Code
	}{
		{"malformed", waxerr.New(waxerr.CodeMalformedInput, "no page capture pattern"), waxerr.CodeMalformedInput},
		{"unreadable", waxerr.New(waxerr.CodeSourceUnreadable, "reading page header"), waxerr.CodeSourceUnreadable},
		{"canceled", context.Canceled, waxerr.CodeCanceled},
		{"unclassified", errors.New("a walk that forgot its code"), waxerr.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := measureMedia(&measureFailMedia{err: tc.err})
			if err == nil {
				t.Fatal("a failed measure returned a length")
			}
			if got := waxerr.CodeOf(err); got != tc.want {
				t.Errorf("code = %s, want %s", got, tc.want)
			}
			if !strings.Contains(err.Error(), "measuring the source") {
				t.Errorf("error = %v, want it to say what was being done", err)
			}
		})
	}
}

// TestSplitCueSkipsADataTrack is the end of the mixed-mode story: the rip's
// audio reaches the disk under the disc's own numbering, the data track's
// bytes reach nothing, and the split says which track it left out.
func TestSplitCueSkipsADataTrack(t *testing.T) {
	dir := t.TempDir()
	starts := []int{100, 250}
	wav, _, total := cueRip(t, dir, starts)
	sheet := mixedModeSheet(t, dir, starts)
	out := filepath.Join(dir, "tracks")
	code, _, errOut := run(t, "split", wav, out, "--cue", sheet)
	if code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "TRACK 01") || !strings.Contains(errOut, "data track") {
		t.Errorf("stderr = %q, want it to say the data track was skipped", errOut)
	}
	names := outputNames(t, out)
	if want := []string{"02 - Track 2.flac", "03 - Track 3.flac"}; !slices.Equal(names, want) {
		t.Fatalf("wrote %v, want %v", names, want)
	}
	e := waxflow.New()
	var sum int64
	for i, name := range names {
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Probe(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		sum += info.Default().Samples
		// The disc's numbering, and its count: the data track is track 1 of
		// 3 on the disc, so the audio is 2 and 3 of 3.
		tags := pieceTags(t, filepath.Join(out, name))
		if got := tags["TRACKNUMBER"]; len(got) != 1 || got[0] != strconv.Itoa(i+2) {
			t.Errorf("%s carries TRACKNUMBER %v, want [%d]", name, got, i+2)
		}
		if got := tags["TRACKTOTAL"]; len(got) != 1 || got[0] != "3" {
			t.Errorf("%s carries TRACKTOTAL %v, want [3]", name, got)
		}
	}
	// The pieces hold the audio and nothing else: the data track's span is
	// exactly what is missing from the rip.
	if want := total - int64(starts[0])*588; sum != want {
		t.Errorf("the pieces hold %d samples, want %d: the rip less its data track", sum, want)
	}
}

// TestSplitCueDryRunListsTheSkippedDataTrack: the dry run says what the split
// would leave out, not only what it would write.
func TestSplitCueDryRunListsTheSkippedDataTrack(t *testing.T) {
	dir := t.TempDir()
	starts := []int{100, 250}
	wav, _, _ := cueRip(t, dir, starts)
	sheet := mixedModeSheet(t, dir, starts)
	out := filepath.Join(dir, "tracks")
	code, stdout, errOut := run(t, "split", wav, out, "--cue", sheet, "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	if !strings.Contains(stdout, "MODE1/2352") || !strings.Contains(stdout, "[0, 58800)") {
		t.Errorf("dry run does not list the data track's span as skipped:\n%s", stdout)
	}
	if !strings.Contains(stdout, "02 - Track 2.flac") || !strings.Contains(stdout, "[58800, 147000)") {
		t.Errorf("dry run does not list the audio pieces:\n%s", stdout)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("--dry-run created %s", out)
	}
}

// TestSplitCueDataFileBesideTheRip: a sheet giving the data track a FILE of
// its own beside the one WAVE with every audio track (EAC's and XLD's
// mixed-mode layout) cuts that WAVE; it is not a rip already split.
func TestSplitCueDataFileBesideTheRip(t *testing.T) {
	dir := t.TempDir()
	wav, _, _ := cueRip(t, dir, []int{0, 75})
	sheet := filepath.Join(dir, "beside.cue")
	if err := os.WriteFile(sheet, []byte(
		"FILE \"album.iso\" BINARY\n  TRACK 01 MODE1/2352\n"+
			"FILE \"album.wav\" WAVE\n  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:00:00\n"+
			"  TRACK 03 AUDIO\n    TITLE \"Three\"\n    INDEX 01 00:01:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "tracks")
	if code, _, errOut := run(t, "split", wav, out, "--cue", sheet); code != 0 {
		t.Fatalf("split exit = %d: %s", code, errOut)
	}
	if names, want := outputNames(t, out), []string{"02 - Two.flac", "03 - Three.flac"}; !slices.Equal(names, want) {
		t.Errorf("wrote %v, want %v", names, want)
	}
}
