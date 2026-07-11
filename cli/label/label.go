// Package label implements meta.Mapper over the waxlabel tag library.
// It is the one place WaxFlow touches waxlabel, kept out of the public
// tree (depcheck) and injected by the CLI: the sanctioned second runtime
// dependency does metadata mapping and nothing else.
package label

import (
	"context"
	"sort"

	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// Mapper is the waxlabel-backed meta.Mapper.
type Mapper struct{}

var _ meta.Mapper = Mapper{}

// New returns the waxlabel-backed mapper.
func New() Mapper { return Mapper{} }

// Read parses src's metadata. Formats waxlabel cannot read (our own
// fragmented MP4, Ogg FLAC) yield an empty Info with a warning: metadata
// stays best-effort, the audio pipeline owns hard errors.
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
		return waxerr.Wrap(waxerr.CodeInternal, "meta: metadata plan", err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "meta: metadata write", err)
	}
	return nil
}
