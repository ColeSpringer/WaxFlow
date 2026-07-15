package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/cue"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// piece is one output of a split: where it starts and ends on the source
// timeline, and what to call it.
//
// to is waxflow.ToEnd for the last piece, the shape jobs.SplitSpans hands
// the daemon's runner and for the same reason: the end is whatever the
// source turns out to hold. Holding the last piece to a number instead
// makes the split refuse a source whose headers merely under-declare, since
// the number is checked against the declaration the CLI chose not to trust.
type piece struct {
	from, to int64
	title    string
	number   int
}

func newSplitCmd(flavor Flavor) *cobra.Command {
	var cueFile string
	var atFlag []string
	var formatName, containerName string
	var flacLevel int
	var force bool
	var noTags bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "split <source> <output-dir>",
		Short: "Split one file into its tracks at CUE or sample cut points",
		Long: `Split a single-file rip into one output per track.

Cut points come from a CUE sheet (--cue) or as explicit source-sample
offsets (--at). They are sample offsets either way: a CUE sheet's MM:SS:FF
times are CD frames, 1/75 s, which every CD-family rate divides exactly
(44100/75 = 588), so a boundary converts to a sample with no rounding.
Seconds would not: 245.32 s at 44100 floors a sample short of the boundary
the sheet names, and that one sample is a click at every track join.

The cut is sample-exact and, to a lossless output at the source's own rate,
bit-exact: the pieces rejoin into the original with nothing lost, repeated,
or filtered at any seam.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case cueFile == "" && len(atFlag) == 0:
				return waxerr.New(waxerr.CodeInvalidRequest, "one of --cue or --at is required")
			case cueFile != "" && len(atFlag) > 0:
				return waxerr.New(waxerr.CodeInvalidRequest, "--cue and --at are exclusive: cut points come from one place")
			}
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			logger, err := newLogger(cmd.ErrOrStderr(), cfg) // CLI logs to stderr
			if err != nil {
				return err
			}
			// FLAC by default, and the default is load-bearing rather than a
			// taste: at the source's own rate a lossless output makes the cut
			// bit-exact, so the pieces rejoin into the original. A lossy
			// default would quietly cost a generation on every split.
			outFormat := formatName
			if outFormat == "" {
				outFormat = "flac"
			}

			src, srcHint, cleanup, err := openSourceRef(cmd, flavor, args[0], &cfg, logger)
			if err != nil {
				return err
			}
			defer cleanup()

			e := waxflow.New(waxflow.WithLogger(logger))
			info, err := e.Probe(src, srcHint, nil)
			if err != nil {
				return err
			}
			track := info.Default()
			// A cut list is meaningless against a length the headers only
			// guess at: every piece is checked against the total. Measure
			// rather than trust, the same call a timeline's mint makes for the
			// same reason.
			if track.Samples < 0 || !track.SamplesExact {
				measured, merr := measureSamples(e, src, srcHint)
				if merr != nil {
					return merr
				}
				// The walk read to the true end of stream, so the length is
				// authoritative now and the track says so rather than keeping
				// the declaration it replaced. Everything below reads the
				// track: a measured length parked in a local next to a stale
				// declared one is how a split comes to check its cut points
				// against the first and its pieces against the second.
				track.Samples, track.SamplesExact = measured, true
			}

			var pieces []piece
			if cueFile != "" {
				pieces, err = cuePieces(cueFile, track.Fmt.Rate, track.Samples)
			} else {
				pieces, err = atPieces(atFlag, track.Samples)
			}
			if err != nil {
				return err
			}

			// The engine's output table is the single source of truth for
			// extensions, so the CLI cannot drift from it: --container names a
			// different kind of file (mka, oga), and a format whose row claims
			// no extension of its own still has a name its output is playable
			// under.
			ext := waxflow.OutputExt(outFormat, containerName)

			// Before the output directory is created: a dry run against a
			// mistyped path prints and leaves nothing behind, which is what it
			// promises.
			if dryRun {
				for _, p := range pieces {
					// The last piece runs to the source's end, which is the
					// length just measured; printing ToEnd itself would show
					// the reader a -1 the split does not mean.
					to := p.to
					if to == waxflow.ToEnd {
						to = track.Samples
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t[%d, %d)\t%.3fs\n",
						pieceName(p, ext), p.from, to,
						float64(to-p.from)/float64(track.Fmt.Rate))
				}
				return nil
			}

			outDir := args[1]
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating the output directory", err)
			}

			// The options field cannot say "level 0" with a plain 0 (that
			// selects the encoder default), so the flag's 0 maps to the
			// sentinel. The flag's own default is the encoder default spelled
			// out, which is what leaves an explicit 0 a level and not a guess.
			optLevel := flacLevel
			if optLevel == 0 {
				optLevel = waxflow.FLACLevelFastest
			}
			// TRACKTOTAL counts the disc's own tracks, and a lead-in piece is
			// not one of them: it is the audio before track 1 and carries
			// track 0 (see cuePieces).
			ofN := len(pieces)
			if len(pieces) > 0 && pieces[0].number == 0 {
				ofN--
			}
			sp := splitter{
				e: e, src: src, hint: srcHint,
				outFormat: outFormat, container: containerName,
				flacLevel: optLevel, force: force, ofN: ofN,
				mapper: label.New(),
			}
			if !noTags {
				sp.readMeta(cmd)
			}

			for _, p := range pieces {
				name := pieceName(p, ext)
				if err := sp.writePiece(cmd, filepath.Join(outDir, name), p); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cueFile, "cue", "", "CUE sheet naming the track boundaries")
	cmd.Flags().StringSliceVar(&atFlag, "at", nil, "explicit cut points as source sample offsets (repeatable or comma-separated)")
	cmd.Flags().StringVar(&formatName, "format", "", "output format (default: flac, which keeps the split lossless)")
	cmd.Flags().StringVar(&containerName, "container", "", "container override where the format has one")
	cmd.Flags().IntVar(&flacLevel, "flac-level", 5, "FLAC compression level 0-8, size vs speed (flac output only)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing outputs")
	cmd.Flags().BoolVar(&noTags, "no-tags", false, "do not carry the source's metadata onto the pieces")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the pieces and their sample ranges without writing anything")
	return cmd
}

// measureCeiling is the past-any-stream seek target that forces a demuxer's
// exact-length walk (an IO-bound frame-index build, no decode): SeekSample
// lands at the true end of stream. It is the daemon's own ceiling, and it is
// well short of the int64 maximum on purpose, so that a demuxer converting
// the target to a timestamp or adding a seek margin to it cannot overflow on
// the way.
const measureCeiling = int64(1) << 61

// measureSamples reads src to its end and returns the sample count it
// actually holds.
func measureSamples(e *waxflow.Engine, src container.Source, hint string) (int64, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return 0, err
	}
	defer med.Close()
	total, err := med.SeekSample(measureCeiling)
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeSourceUnreadable, "measuring the source", err)
	}
	return total, nil
}

// pieceName is the output filename: the disc's own track number, zero
// padded so a directory listing sorts the way the album plays, plus the
// title when the sheet gave one.
func pieceName(p piece, ext string) string {
	if p.title == "" {
		return fmt.Sprintf("%02d.%s", p.number, ext)
	}
	return fmt.Sprintf("%02d - %s.%s", p.number, sanitizeFilename(p.title), ext)
}

// maxTitleBytes bounds the title in a piece's filename. It is bytes because
// the limit it stands off from is (255 on ext4 and APFS), with the number,
// the separator and the extension riding alongside.
const maxTitleBytes = 120

// sanitizeFilename strips what a path cannot hold. It is deliberately
// blunt: a title is arbitrary text from a sheet written by anyone, and a
// separator or a NUL in it is a path traversal rather than a typo.
func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '-'
		case r < 0x20:
			return -1
		}
		return r
	}, s)
	// Truncated before the trim, so that a cut landing on a space or a dot
	// leaves neither at the end of the name.
	s = truncateTitle(s, maxTitleBytes)
	s = strings.TrimSpace(strings.Trim(s, "."))
	if s == "" {
		return "untitled"
	}
	return s
}

// truncateTitle cuts s to at most max bytes without splitting a rune: a byte
// slice through a multi-byte rune leaves invalid UTF-8, and a filename is
// text the filesystem and every listing that renders it have to decode. The
// cut walks back to the last rune boundary at or before max, which costs the
// one rune it landed inside and nothing else.
func truncateTitle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// cuePieces turns a sheet into the pieces of one audio file.
//
// A sheet may index several files (a track-per-file rip), which has no
// cutting to do and is refused by name rather than silently splitting the
// wrong one. The file the sheet names is not resolved or checked against
// the source: a sheet and its rip are routinely renamed together, and
// refusing on a name mismatch would reject working input for a spelling.
func cuePieces(path string, rate int, total int64) ([]piece, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalidRequest, "reading the CUE sheet", err)
	}
	sheet, err := cue.Parse(raw)
	if err != nil {
		return nil, err
	}
	file, err := sheet.SingleFile()
	if err != nil {
		return nil, err
	}
	// The same funnel the daemon's split job cuts by, so a sheet split here
	// and the same sheet POSTed there cut at the same samples. Only the
	// product differs: the daemon wants spans, this wants pieces with names.
	// Every rule about what a sheet's cut list may say lives there, including
	// the sheet that names one track and so has nothing to cut.
	cuts, err := file.Cuts(rate)
	if err != nil {
		return nil, err
	}

	// Cuts keeps a nonzero first start instead of folding the audio before
	// track 1 into it, so with one there is a piece more than the sheet has
	// tracks and it is the first: the lead-in, which no track names. It is a
	// pregap, or it is hidden track one audio, which on some discs is a whole
	// song. It gets track 0, the disc's own address for the audio ahead of
	// track 1, which is also where a listing sorts it. It gets no title: the
	// sheet gave it none, and a piece with no title carries no TITLE tag
	// rather than an invented one.
	leadIn := len(cuts) == len(file.Tracks)

	out := make([]piece, 0, len(cuts)+1)
	from := int64(0)
	for i := 0; i <= len(cuts); i++ {
		p := piece{from: from, to: waxflow.ToEnd}
		if i < len(cuts) {
			p.to = cuts[i]
		}
		ti := i
		if leadIn {
			ti--
		}
		if ti >= 0 {
			p.title, p.number = file.Tracks[ti].Title, file.Tracks[ti].Number
		}
		if from >= total {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"track %d starts at sample %d, past the source's %d: this sheet does not describe this file",
				p.number, from, total))
		}
		out = append(out, p)
		from = p.to
	}
	return out, nil
}

// atPieces turns explicit sample offsets into pieces. The offsets are the
// boundaries between pieces, so N of them make N+1 pieces.
func atPieces(at []string, total int64) ([]piece, error) {
	cuts := make([]int64, 0, len(at)+1)
	cuts = append(cuts, 0)
	for _, s := range at {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || v <= 0 {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("--at %q: want a positive sample offset", s))
		}
		if v >= total {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"--at %d is past the source's %d samples", v, total))
		}
		if v <= cuts[len(cuts)-1] {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"--at %d does not come after %d; cut points ascend", v, cuts[len(cuts)-1]))
		}
		cuts = append(cuts, v)
	}
	out := make([]piece, len(cuts))
	for i, c := range cuts {
		to := int64(waxflow.ToEnd)
		if i+1 < len(cuts) {
			to = cuts[i+1]
		}
		out[i] = piece{from: c, to: to, number: i + 1}
	}
	return out, nil
}

// splitter is what every piece of one split is written with: the things a
// piece does not vary, resolved once, so that writing one takes the piece
// and its path rather than a dozen arguments in a row.
type splitter struct {
	e         *waxflow.Engine
	src       container.Source
	hint      string
	outFormat string
	container string
	flacLevel int
	force     bool
	ofN       int

	// The metadata half: albumTags and art ride onto every piece at mux
	// time, tagInfo carries what a tag list cannot (art again, for the
	// formats whose muxer does not embed it) through the post-pass. All
	// three are empty under --no-tags.
	mapper    label.Mapper
	tagInfo   *meta.Info
	albumTags []container.Tag
	art       *container.Picture
}

// readMeta reads the album's metadata off the source, once for the whole
// split. It is the transcode command's file-output passthrough matrix: full
// tags and art flow onto every piece, with the per-piece title and number
// from the cut list on top of them.
//
// Metadata is best-effort, so an unreadable one leaves the pieces untagged
// rather than failing a split whose audio is fine.
func (s *splitter) readMeta(cmd *cobra.Command) {
	m, err := s.mapper.Read(cmd.Context(), s.src, s.hint, meta.ReadOptions{Pictures: true})
	if err != nil {
		return
	}
	for _, warn := range m.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "metadata: %s\n", warn)
	}
	// The album's ReplayGain measures the whole rip, and a piece is not it:
	// the tags would tell a player to adjust a piece by a number nothing
	// measured. A fresh measurement is what --loudness is for, on the piece.
	s.tagInfo = withoutTimeline(meta.WithoutReplayGain(m))
	s.albumTags = meta.FullTags(s.tagInfo)
	if p := s.tagInfo.FrontPicture(); p != nil {
		s.art = &container.Picture{MIME: p.MIME, Data: p.Data}
	}
}

// withoutTimeline drops the source's chapters and synced lyrics (a shallow
// copy, the transfer WithoutReplayGain makes for the same kind of reason).
// Both are marks on the source's own timeline, and a piece starts elsewhere
// on it: stamped onto a piece unchanged, a rip's chapter list would put
// every mark it has into each piece, minutes past the end of a four-minute
// file. Rebasing them per piece is a thing a split could do (Slice does it
// for the audio); dropping them is what it does until it does.
func withoutTimeline(info *meta.Info) *meta.Info {
	if info == nil {
		return nil
	}
	out := *info
	out.Chapters, out.Synced = nil, nil
	return &out
}

// embedsTags reports whether the piece's muxer writes the tags itself, which
// is what the metadata post-pass has to skip so it does not write them a
// second time (or a conflicting one). It is the transcode command's set, for
// the reasons kept there: the MP4 muxers embed an ilst in moov, and the Ogg
// muxer embeds the comment header at Begin.
func (s *splitter) embedsTags() bool {
	isMP4 := s.outFormat == "alac" ||
		(s.outFormat == "aac" && (s.container == "" || s.container == "progressive"))
	return isMP4 || s.container == "ogg"
}

// writePiece transcodes one span of the source to its own file.
func (s *splitter) writePiece(cmd *cobra.Command, path string, p piece) error {
	writePath := path
	if s.force {
		writePath = fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	}
	out, err := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if !s.force && os.IsExist(err) {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "output exists (use --force to overwrite)", err)
		}
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating output", err)
	}
	fail := func(err error) error {
		out.Close()
		os.Remove(writePath)
		return err
	}

	// Each piece opens the source afresh. A single Media cannot serve them
	// all, because Slice owns what it wraps and closes it, and because a
	// piece is its own stream from its own start rather than a continuation.
	med, err := s.e.OpenStream(s.src, s.hint)
	if err != nil {
		return fail(err)
	}
	sl, err := waxflow.Slice(med, p.from, p.to)
	if err != nil {
		med.Close()
		return fail(err)
	}
	defer sl.Close()

	tags := append([]container.Tag(nil), s.albumTags...)
	tags = replaceTag(tags, "TRACKNUMBER", strconv.Itoa(p.number))
	tags = replaceTag(tags, "TRACKTOTAL", strconv.Itoa(s.ofN))
	if p.title != "" {
		tags = replaceTag(tags, "TITLE", p.title)
	}

	if _, err := s.e.TranscodeMedia(cmd.Context(), sl, out, waxflow.TranscodeOptions{
		Format:    s.outFormat,
		Container: s.container,
		FLACLevel: s.flacLevel,
		Tags:      tags,
		Art:       s.art,
	}); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(writePath)
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "closing output", err)
	}

	// Post-pass on the finished piece, for the outputs whose muxer did not
	// already write the metadata: it is what carries the art onto them, and
	// everything else a Tag list cannot hold. The piece's own tags go as the
	// extras, which win over the album's same-keyed ones, so the title and
	// number the mux wrote survive the rewrite.
	if s.tagInfo != nil && !s.embedsTags() {
		if err := s.mapper.Apply(cmd.Context(), writePath, s.tagInfo, tags); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "metadata: post-pass failed: %v\n", err)
		}
	}
	if s.force && writePath != path {
		if err := os.Rename(writePath, path); err != nil {
			os.Remove(writePath)
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "renaming output into place", err)
		}
	}
	return nil
}

// replaceTag sets key to value, dropping whatever the album carried: a
// piece's own title and number are the sheet's, not the whole rip's.
func replaceTag(tags []container.Tag, key, value string) []container.Tag {
	out := tags[:0]
	for _, t := range tags {
		if !strings.EqualFold(t.Key, key) {
			out = append(out, t)
		}
	}
	return append(out, container.Tag{Key: key, Value: value})
}
