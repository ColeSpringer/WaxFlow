// Package label implements meta.Mapper over the waxlabel tag library.
// It is the one place WaxFlow touches waxlabel, kept out of the public
// tree (depcheck) and injected by the CLI: the sanctioned second runtime
// dependency does metadata mapping and nothing else.
package label

import (
	"context"
	"errors"
	"sort"

	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
	wlerr "github.com/colespringer/waxlabel/waxerr"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// sourceLint names the warnings that describe the source file rather than
// this transfer: WaxFlow reads a merged tag view and writes a fresh output,
// so a stray legacy block or an inherited encoder stamp costs it nothing.
// They become meta.Info.Notes, logged at Debug.
//
// Kept small on purpose. Anything that might mean a value did not survive as
// the source held it stays a Warning, WarnNumericGenre included (it fires
// when "(17)" resolved to a name, so the output says GENRE=Rock). waxlabel's
// own LintSeverity is not used: it is calibrated for a tagger editing in
// place, and ranks four of these as warnings. An unlisted code stays a
// Warning, which is the safe direction.
//
// WarnFragmented clears the bar because a fragmented MP4's tags read exactly:
// what degrades is the duration and the essence digest, which Read does not
// consume, and the refused write is one WaxFlow never asks for.
var sourceLint = map[waxlabel.WarningCode]bool{
	waxlabel.WarnInheritedEncoder: true,
	waxlabel.WarnStrayLeadingID3:  true,
	waxlabel.WarnTrailingID3v1:    true,
	waxlabel.WarnLegacyAPE:        true,
	waxlabel.WarnFragmented:       true,
}

// Mapper is the waxlabel-backed meta.Mapper.
type Mapper struct{}

var _ meta.Mapper = Mapper{}

// New returns the waxlabel-backed mapper.
func New() Mapper { return Mapper{} }

// Read parses src's metadata. Formats waxlabel cannot read (Ogg FLAC) yield
// an empty Info with a warning: metadata stays best-effort, the audio
// pipeline owns hard errors.
func (Mapper) Read(ctx context.Context, src container.Source, hint string, opts meta.ReadOptions) (*meta.Info, error) {
	doc, err := waxlabel.Parse(ctx, src)
	if err != nil {
		if ctx.Err() != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "meta: read canceled", ctx.Err())
		}
		return &meta.Info{Warnings: []string{"metadata unread: " + err.Error()}}, nil
	}
	info := &meta.Info{Tags: map[string][]string{}}
	for k, vs := range doc.Tags().All() {
		info.Tags[string(k)] = vs
	}
	for _, ch := range doc.Chapters() {
		// Start, End, Title survive; per-chapter language and flags are
		// a Matroska nicety no output of ours can carry.
		info.Chapters = append(info.Chapters, container.Chapter{Start: ch.Start, End: ch.End, Title: ch.Title})
	}
	for _, w := range doc.Warnings() {
		if sourceLint[w.Code] {
			info.Notes = append(info.Notes, w.String())
			continue
		}
		info.Warnings = append(info.Warnings, w.String())
	}
	for _, sl := range doc.SyncedLyrics() {
		out := meta.SyncedLyrics{Language: sl.Language, Description: sl.Description}
		for _, l := range sl.Lines {
			out.Lines = append(out.Lines, meta.SyncedLine{Time: l.Time, Text: l.Text})
		}
		info.Synced = append(info.Synced, out)
	}
	if opts.Pictures {
		for _, p := range doc.Pictures() {
			info.Pictures = append(info.Pictures, meta.Picture{
				MIME:        p.MIME,
				Description: p.Description,
				Front:       p.Type == waxlabel.PicFrontCover,
				Data:        p.Data,
			})
		}
		info.HasPictures = len(info.Pictures) > 0
	} else {
		info.HasPictures = doc.Inspect().PictureCount > 0
	}
	return info, nil
}

// Apply rewrites the finished file at path with info's metadata plus the
// extra tags (which win over same-keyed info tags). Values or fields the
// output format cannot hold are waxlabel plan warnings, not errors: the
// transfer is preservation-first and best-effort by design.
func (Mapper) Apply(ctx context.Context, path string, info *meta.Info, extra []container.Tag) error {
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		if canceled(err) {
			return waxerr.Wrap(waxerr.CodeCanceled, "meta: metadata read canceled", err)
		}
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "meta: output not taggable", err)
	}
	ed := doc.Edit()
	if info != nil {
		keys := make([]string, 0, len(info.Tags))
		for k := range info.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key, err := tag.ParseKey(k)
			if err != nil {
				continue // a key the vocabulary rejects is not worth failing a finished transcode
			}
			ed.Set(key, info.Tags[k]...)
		}
		for _, p := range info.Pictures {
			typ := waxlabel.PicOther
			if p.Front {
				typ = waxlabel.PicFrontCover
			}
			ed.AddPicture(waxlabel.Picture{Type: typ, MIME: p.MIME, Description: p.Description, Data: p.Data})
		}
		if len(info.Chapters) > 0 {
			chs := make([]waxlabel.Chapter, 0, len(info.Chapters))
			for _, ch := range info.Chapters {
				chs = append(chs, waxlabel.Chapter{Start: ch.Start, End: ch.End, Title: ch.Title})
			}
			ed.SetChapters(chs...)
		}
		if len(info.Synced) > 0 {
			sets := make([]waxlabel.SyncedLyrics, 0, len(info.Synced))
			for _, sl := range info.Synced {
				lines := make([]waxlabel.SyncedLine, 0, len(sl.Lines))
				for _, l := range sl.Lines {
					lines = append(lines, waxlabel.SyncedLine{Time: l.Time, Text: l.Text})
				}
				sets = append(sets, waxlabel.SyncedLyrics{Language: sl.Language, Description: sl.Description, Lines: lines})
			}
			ed.SetSyncedLyrics(sets...)
		}
	}
	for _, t := range extra {
		if key, err := tag.ParseKey(t.Key); err == nil {
			ed.Set(key, t.Value)
		}
	}
	plan, err := ed.Prepare()
	if err != nil {
		// Two unrelated failures land here. The write refusals (a fragmented
		// MP4, an iloc or saio the codec cannot patch, a chapter count past the
		// format's limit) mean this output cannot hold the metadata, the same
		// answer ParseFile's refusal gives above. Everything else is the
		// metadata itself being unwritable anywhere (a NUL byte or invalid
		// UTF-8 in a value, usually straight off the source), and calling that
		// an unsupported output blames the wrong file.
		//
		// No cancellation check: Prepare takes no context and does no I/O.
		if errors.Is(err, wlerr.ErrFragmented) ||
			errors.Is(err, wlerr.ErrUnsupportedFormat) ||
			errors.Is(err, wlerr.ErrUnsupportedTag) {
			return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "meta: output cannot take the metadata", err)
		}
		return waxerr.Wrap(waxerr.CodeInvalidRequest, "meta: metadata is not writable", err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		if canceled(err) {
			return waxerr.Wrap(waxerr.CodeCanceled, "meta: metadata write canceled", err)
		}
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "meta: metadata write", err)
	}
	return nil
}

// canceled reports whether err is the context giving out rather than the work
// failing. It tests the error instead of ctx.Err() so that a real failure
// racing a Ctrl-C keeps its own code and its own explanation: a disk-full
// SaveBack is output-unwritable even if the signal lands first.
func canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
