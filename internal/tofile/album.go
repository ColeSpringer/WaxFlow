package tofile

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// Album is what every piece of one split carries: the source's tags
// without ReplayGain, chapters and synced lyrics, and its front cover.
type Album struct {
	Info *meta.Info
	Tags []container.Tag
	Art  *container.Picture

	// mapper runs the post-pass, and postPasses says whether one rewrites
	// each piece with Info's full set, so a tag the muxer was not handed is
	// restored rather than lost.
	mapper     meta.Mapper
	postPasses bool
	// formatName and containerName are the form the pieces are written in.
	formatName, containerName string
	// note and dropped report each album tag left out for good, once per
	// split.
	note    func(Note)
	dropped map[string]bool
}

// ReadAlbum reads the source's metadata once for a whole split, with Run's
// read and fold, for pieces written in formatName and containerName: a
// failed read is a note and the container's tags still fold. A canceled read
// reports nothing and returns an empty Album; the first piece's write then
// fails on ctx.
func ReadAlbum(ctx context.Context, mapper meta.Mapper, src container.Source, hint string, containerTags map[string][]string,
	formatName, containerName string, note func(Note)) Album {
	r := &run{ctx: ctx, req: Request{
		Source: src, Hint: hint, Mapper: mapper, Note: note,
		Probe: &format.Info{Tags: containerTags},
	}}
	info, err := r.readMetadata()
	if err != nil {
		return Album{}
	}
	// The rip's ReplayGain measures the whole rip, and its chapters and
	// synced lyrics are marks on the rip's timeline: none describes a piece.
	info = withoutTimeline(meta.WithoutReplayGain(info))
	a := Album{
		Info: info, Tags: meta.FullTags(info), mapper: mapper, note: note, dropped: map[string]bool{},
		formatName: formatName, containerName: containerName,
		postPasses: info != nil && postPasses(mapper, formatName, containerName),
	}
	if p := info.FrontPicture(); p != nil {
		a.Art = &container.Picture{MIME: p.MIME, Data: p.Data}
	}
	return a
}

// withoutTimeline is a shallow copy of info without its chapters and synced
// lyrics.
func withoutTimeline(info *meta.Info) *meta.Info {
	if info == nil {
		return nil
	}
	out := *info
	out.Chapters, out.Synced = nil, nil
	return &out
}

// PieceTags is the album's tags with the piece's own after them: TRACKNUMBER,
// TRACKTOTAL and, unless title is empty, TITLE replace the album's. The set is
// fitted to the pieces' muxer (fitTags), and an album tag left out is
// reported once per split unless the post-pass restores it.
func (a Album) PieceTags(number, total int, title string) []container.Tag {
	own := ownTags(number, total, title)
	album := slices.DeleteFunc(slices.Clone(a.Tags), func(t container.Tag) bool {
		return slices.ContainsFunc(own, func(o container.Tag) bool { return strings.EqualFold(t.Key, o.Key) })
	})
	tags, dropped := fitTags(a.formatName, a.containerName, album, own)
	a.noteDropped(dropped)
	if !a.postPasses && a.note != nil {
		noteLeftOut(a.formatName, a.containerName, tags, a.dropped, a.note)
	}
	return tags
}

// CheckPieceTags refuses a piece whose own tags the muxer of the output form
// cannot embed even on their own: a caller named them, so they are refused,
// never dropped. Every writer refuses a set only on its size, so a refusal
// is worded as the title's.
func CheckPieceTags(formatName, containerName string, number, total int, title string) error {
	if waxflow.TagsFit(formatName, containerName, ownTags(number, total, title)) == nil {
		return nil
	}
	return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
		"a %d-byte title is too large to embed in a piece's tags", len(title)))
}

// ownTags is what a piece is called, in the order it is embedded.
func ownTags(number, total int, title string) []container.Tag {
	own := []container.Tag{
		{Key: "TRACKNUMBER", Value: strconv.Itoa(number)},
		{Key: "TRACKTOTAL", Value: strconv.Itoa(total)},
	}
	if title != "" {
		own = append(own, container.Tag{Key: "TITLE", Value: title})
	}
	return own
}

// noteDropped reports the album keys left out for good that this split has
// not reported yet. Where the post-pass runs, none is.
func (a Album) noteDropped(keys []string) {
	if a.postPasses {
		return
	}
	for _, key := range keys {
		if a.dropped == nil || a.dropped[key] {
			continue
		}
		a.dropped[key] = true
		if a.note != nil {
			a.note(Note{Kind: NoteTagTooLarge, Key: key})
		}
	}
}

// PostPass writes the album and the piece's tags onto the finished piece at
// path when its muxer is not the final tag writer: it is what carries the
// art and what a tag list cannot hold. The piece's tags win over the album's.
// A failure goes to failed, which must not be nil; a cancel, "split
// canceled", is returned, and the piece must not be published.
func (a Album) PostPass(ctx context.Context, path string, tags []container.Tag, failed func(error)) error {
	var apply func() error
	if a.postPasses {
		apply = func() error { return a.mapper.Apply(ctx, path, a.Info, tags) }
	}
	return postPass(ctx, "split", apply, failed)
}
