package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
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

// muxPCM encodes buf as PCM into m, header to trailer.
func muxPCM(t *testing.T, cfg pcm.Config, buf *audio.Buffer, m container.Muxer) {
	t.Helper()
	f := buf.Fmt
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(),
		Fmt: f, Samples: int64(buf.N), Default: true}
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

	var b strings.Builder
	b.WriteString("PERFORMER \"The Band\"\nTITLE \"The Album\"\nFILE \"album.wav\" WAVE\n")
	for i, f := range frameStarts {
		mm, ss, ff := f/75/60, (f/75)%60, f%75
		fmt.Fprintf(&b, "  TRACK %02d AUDIO\n    TITLE \"Track %d\"\n    INDEX 01 %02d:%02d:%02d\n",
			i+1, i+1, mm, ss, ff)
	}
	sheet = filepath.Join(dir, "album.cue")
	if err := os.WriteFile(sheet, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return wav, sheet, total
}

// TestSplitCueRejoinsGaplessly is M24a's end-to-end proof, and the reason
// internal/cue exists at all: a real CUE sheet drives a real split, and the
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
	}{{
		name:   "track 1 at frame 0",
		starts: []int{0, 100, 250},
		want: []piece{
			{from: 0, to: 100 * 588, title: "Track 1", number: 1},
			{from: 100 * 588, to: 250 * 588, title: "Track 2", number: 2},
			{from: 250 * 588, to: waxflow.ToEnd, title: "Track 3", number: 3},
		},
	}, {
		// The shape internal/cue's own sheetBasic fixture has.
		name:   "track 1 past frame 0",
		starts: []int{33, 412},
		want: []piece{
			// The lead-in: the disc's track 0, which no sheet line names, so
			// it takes no title and invents none.
			{from: 0, to: 33 * 588, title: "", number: 0},
			{from: 33 * 588, to: 412 * 588, title: "Track 1", number: 1},
			{from: 412 * 588, to: waxflow.ToEnd, title: "Track 2", number: 2},
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, sheet, _ := cueRip(t, t.TempDir(), tc.starts)
			got, err := cuePieces(sheet, 44100, total)
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
		})
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
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
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

// underDeclaredMKA writes a Matroska file that holds frames samples and says
// it holds fewer, which is not a corruption but the shape the format has: its
// Duration is advisory and lands on a millisecond, so a rip whose length is
// not a whole number of them declares the millisecond below and the demuxer
// leaves SamplesExact false to say so. Our own muxer writes no Duration at
// all (an unknown length declares nothing, and nothing cannot be wrong), so
// the element is spliced into its Info afterward.
//
// It is the source a split has to survive: the declaration is not what the
// file holds, which is why the CLI measures rather than trusts it.
func underDeclaredMKA(t *testing.T, rate, channels, frames int) []byte {
	t.Helper()
	cfg, f := pcm16Format(rate, channels)
	buf := testutil.Ramp(f, frames)
	defer audio.Put(buf)
	var b bytes.Buffer
	muxPCM(t, cfg, buf, mka.NewMuxer(&b, nil))

	// A millisecond short of the true length: the value a muxer that
	// truncates its Duration writes. What the demuxer makes of it is the
	// demuxer's arithmetic, so the caller probes for the declared length
	// rather than predicting it here.
	ms := math.Floor(float64(frames)/float64(rate)*1000) - 1
	return spliceMKADuration(t, b.Bytes(), ms)
}

// spliceMKADuration adds a Duration element (ticks of the 1 ms
// TimestampScale the muxer writes) to the Info element of raw.
func spliceMKADuration(t *testing.T, raw []byte, ticks float64) []byte {
	t.Helper()
	// The Segment header, and so Info, is at the head of the file, ahead of
	// any cluster: bounding the search keeps it off audio that happens to
	// spell the same four bytes.
	const headWindow = 512
	idInfo := []byte{0x15, 0x49, 0xA9, 0x66}
	at := bytes.Index(raw[:min(headWindow, len(raw))], idInfo)
	if at < 0 {
		t.Fatal("no Info element in the muxed header")
	}
	sizeAt := at + len(idInfo)
	// The muxer's Info is well under 127 bytes, so its size is a one-byte
	// vint (the 0x80 marker plus the length) and so is the grown one.
	size := raw[sizeAt]
	if size&0x80 == 0 {
		t.Fatalf("Info size %#x is not the one-byte vint this splice rewrites", size)
	}
	body := int(size &^ 0x80)

	dur := []byte{0x44, 0x89, 0x88} // Duration, an 8-byte payload
	dur = binary.BigEndian.AppendUint64(dur, math.Float64bits(ticks))
	if body+len(dur) > 0x7F {
		t.Fatalf("Info grows to %d bytes, past what a one-byte vint says", body+len(dur))
	}

	bodyAt := sizeAt + 1
	out := make([]byte, 0, len(raw)+len(dur))
	out = append(out, raw[:sizeAt]...)
	out = append(out, byte(0x80|(body+len(dur))))
	out = append(out, raw[bodyAt:bodyAt+body]...)
	out = append(out, dur...)
	return append(out, raw[bodyAt+body:]...)
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

// TestSplitMeasuresASourceWithNoDeclaredLength: a container is free to
// declare no length at all (our own Matroska muxer writes none: the length is
// not known when the header goes out, and an unknown length declared as a
// number would be a lie where silence is the truth). Every cut point is
// checked against the source's length, so trusting that non-answer refuses
// every cut in the list against a source of -1 samples.
func TestSplitMeasuresASourceWithNoDeclaredLength(t *testing.T) {
	dir := t.TempDir()
	const actual = 100_000
	cfg, f := pcm16Format(44100, 2)
	buf := testutil.Ramp(f, actual)
	defer audio.Put(buf)
	var b bytes.Buffer
	muxPCM(t, cfg, buf, mka.NewMuxer(&b, nil))
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
		// Progressive rather than the fragmented default only because the tag
		// library cannot read a moof back; the art rides the same options
		// field into both.
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
