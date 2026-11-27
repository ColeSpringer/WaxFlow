// Package label implements meta.Mapper over the waxlabel tag library.
// It is the one place WaxFlow touches waxlabel, kept out of the public
// tree (depcheck) and injected by the CLI: the sanctioned second runtime
// dependency does metadata mapping and nothing else.
package label

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
	wlerr "github.com/colespringer/waxlabel/waxerr"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// sourceLint names the warnings that describe the source file rather than
// this transfer: WaxFlow writes a fresh output, so an inherited encoder stamp
// costs it nothing, and neither does a legacy tag block, whose values Read
// carries (see legacyFloor) and whose other content it warns of by itself.
// They become meta.Info.Notes, logged at Debug.
//
// Kept small on purpose. Anything that might mean a value did not survive as
// the source held it stays a Warning, WarnNumericGenre included (it fires
// when "(17)" resolved to a name, so the output says GENRE=Rock). waxlabel's
// own LintSeverity is not used: it is calibrated for a tagger editing in
// place, and ranks four of these as warnings. An unlisted code stays a
// Warning, which is the safe direction. That rule is also why the tempting
// geometry codes stay out: trailing-bytes covers a WAV or AIFF ID3v1 trailer
// whose tags the library preserves but does not read, unknown-chunk-size
// means every chunk after the sentinel-sized one went unread, and
// trailing-lyrics3 is a block of lyrics no reader projects, so each can mean
// content this transfer will not carry.
//
// WarnFragmented clears the bar because a fragmented MP4's tags read exactly:
// what degrades is the duration and the essence digest, which Read does not
// consume, and the refused write is one WaxFlow never asks for.
// WarnMultiplexedStream clears it the same way: the audio stream's tags read
// exactly, and what else an Ogg file multiplexes (a Skeleton index, a video
// stream) is the demuxer's track choice, as in any container of several
// tracks. A chained stream stays a warning, since its later links play and
// their tags go unread.
var sourceLint = map[waxlabel.WarningCode]bool{
	waxlabel.WarnInheritedEncoder:  true,
	waxlabel.WarnStrayLeadingID3:   true,
	waxlabel.WarnTrailingID3v1:     true,
	waxlabel.WarnLegacyAPE:         true,
	waxlabel.WarnFragmented:        true,
	waxlabel.WarnMultiplexedStream: true,
}

// legacyOpaque is the warning for a legacy block holding what legacyFloor
// cannot carry. The library has no code for it outside its linter.
const legacyOpaque = "a legacy tag block holds more than tag values (a picture, chapters, a binary item, " +
	"a Lyrics3 block or bytes that did not read); the output does not carry it"

// machineKeyLint spots an invalid-tag-key warning about an iTunes machine tag
// (iTunSMPB, iTunNORM, iTunMOVI, iTunes_CDDB_*). Only MP4 freeforms draw the
// warning for these: that store keeps their mixed-case spelling, which fails
// the canonical key syntax, while every other route (a TXXX description, a
// Vorbis or APE item) uppercases the same name into a valid custom key and
// carries it without a word. As a warning it would fire on most MP4 sources
// (iTunes rips and WaxFlow's own seekable AAC outputs all carry iTunSMPB)
// about names that travel silently everywhere else, and the two that matter,
// gapless and SoundCheck state, are values WaxFlow re-derives rather than
// inherits. Any other invalid-tag-key stays a warning: that value really does
// not survive onto the output.
//
// The offending native name lives only in the message prose (the code is
// keyless by design), so this reads it off the first ": ". The library builds
// every one of these warnings from one fixed phrase carrying exactly one, and
// the name is the whole remainder, quoted since v1.9.0, so cutting there
// takes a name containing ": " intact where cutting at the last would slice
// through it. If the wording changes upstream these fall back to warnings,
// which is noisy but not lossy.
func machineKeyLint(w waxlabel.Warning) bool {
	if w.Code != waxlabel.WarnInvalidTagKey {
		return false
	}
	_, name, ok := strings.Cut(w.Message, ": ")
	return ok && strings.HasPrefix(strings.TrimPrefix(name, `"`), "iTun")
}

// Mapper is the waxlabel-backed meta.Mapper.
type Mapper struct {
	log *slog.Logger
}

var _ meta.Mapper = Mapper{}

// New returns the waxlabel-backed mapper. Its demoted notes (see NewLogged)
// go to slog.Default.
func New() Mapper { return Mapper{} }

// NewLogged returns the mapper with log receiving the notes Apply demotes
// rather than fails on: a field the output has no store for, text it had to
// repair, and a write whose bytes landed but whose post-commit step then
// failed.
func NewLogged(log *slog.Logger) Mapper { return Mapper{log: log} }

// Read parses src's metadata. Every format WaxFlow decodes is one waxlabel
// reads, so a parse failure means damaged metadata, not a format gap; such a
// source yields an empty Info with a warning: metadata stays best-effort, the
// audio pipeline owns hard errors.
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
	legacyFloor(info.Tags, doc.Families())
	filler := doc.Capabilities().TextDefaultLanguage()
	info.CommentItems = textItems(doc.Comments(), filler)
	info.LyricsItems = textItems(doc.Lyrics(), filler)
	for _, ch := range doc.Chapters() {
		// Start, End, Title survive; per-chapter language and flags are
		// a Matroska nicety no output of ours can carry.
		info.Chapters = append(info.Chapters, container.Chapter{Start: ch.Start, End: ch.End, Title: ch.Title})
	}
	// The library returns a list as its file stores it; a Chapterer promises
	// start order, and these chapters take a demuxer's place.
	container.SortChapters(info.Chapters)
	for _, w := range doc.Warnings() {
		if sourceLint[w.Code] || machineKeyLint(w) {
			info.Notes = append(info.Notes, w.String())
			continue
		}
		info.Warnings = append(info.Warnings, w.String())
	}
	if doc.HasOpaqueLegacyContent() {
		info.Warnings = append(info.Warnings, legacyOpaque)
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
				Type:        uint8(p.Type),
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

// legacyFloor adds to tags the values only a legacy block holds: an MP3 whose
// one tag is ID3v1, an APEv2 tag behind a FLAC stream, a stray ID3v2 tag ahead
// of one. The library's canonical view leaves them out, since a tagger editing
// in place keeps the block, but a transcode writes a fresh file without it. A
// key the canonical view holds wins. The library lists a block's values one
// entry each, so the first kind of block to hold a key gives every value it
// has of it, a repeat once. Totals are floored after the numbers, so each is
// held to the number it would sit beside (totalAgrees).
func legacyFloor(tags map[string][]string, fams []waxlabel.FamilyValue) {
	canonical := maps.Clone(tags)
	from := map[string]waxlabel.Family{}
	for _, totals := range [2]bool{false, true} {
		for _, f := range fams {
			if !f.Legacy || f.Targeted || len(f.Values) == 0 || (numberOf(f.Key) != "") != totals {
				continue
			}
			k := string(f.Key)
			if fam, floored := from[k]; floored {
				if fam != f.Family {
					continue
				}
			} else {
				if _, held := canonical[k]; held || !totalAgrees(tags, from, fams, f) {
					continue
				}
				from[k] = f.Family
			}
			for _, v := range f.Values {
				if !slices.Contains(tags[k], v) {
					tags[k] = append(tags[k], v)
				}
			}
		}
	}
}

// numberOf is the number key a track or disc total counts, "" for any other
// key.
func numberOf(total tag.Key) tag.Key {
	switch total {
	case tag.TrackTotal:
		return tag.TrackNumber
	case tag.DiscTotal:
		return tag.DiscNumber
	}
	return ""
}

// totalAgrees reports whether a legacy block's track or disc total may sit
// beside the number tags holds. A block that states a number of its own
// counts that numbering, so one of them must be the number held. A block
// that states none may stand beside the canonical number, but not beside
// one floored from another legacy block (from), whose numbering nothing
// ties it to. Any other key agrees.
func totalAgrees(tags map[string][]string, from map[string]waxlabel.Family, fams []waxlabel.FamilyValue, f waxlabel.FamilyValue) bool {
	number := numberOf(f.Key)
	held, ok := tags[string(number)]
	if number == "" || !ok {
		return true
	}
	stated := false
	for _, g := range fams {
		if !g.Legacy || g.Family != f.Family || g.Key != number {
			continue
		}
		stated = true
		for _, v := range g.Values {
			if slices.ContainsFunc(held, func(h string) bool { return sameNumber(h, v) }) {
				return true
			}
		}
	}
	_, floored := from[string(number)]
	return !stated && !floored
}

// sameNumber compares two track or disc numbers as numbers, "03" being 3.
func sameNumber(a, b string) bool {
	x, errA := strconv.Atoi(strings.TrimSpace(a))
	y, errB := strconv.Atoi(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return a == b
	}
	return x == y
}

// textItems carries a source's COMMENT or LYRICS items, nil when none has a
// description or a language to tell. filler is the language the source's
// format writes for a text given none (ID3's "eng"): read back, it says
// nothing about the text, and carried on it would make a WAV or AIFF output
// keep the text in an ID3 chunk alone, for the language's sake.
func textItems[T ~struct{ Text, Description, Language string }](items []T, filler string) []meta.TextItem {
	var qualified bool
	out := make([]meta.TextItem, len(items))
	for i, it := range items {
		out[i] = meta.TextItem(it)
		if filler != "" && strings.EqualFold(out[i].Language, filler) {
			out[i].Language = ""
		}
		qualified = qualified || out[i].Description != "" || out[i].Language != ""
	}
	if !qualified {
		return nil
	}
	return out
}

// Apply rewrites the finished file at path with info's metadata plus the
// extra tags (which win over same-keyed info tags). Values or fields the
// output format cannot hold are waxlabel plan warnings, not errors: the
// transfer is preservation-first and best-effort by design.
func (m Mapper) Apply(ctx context.Context, path string, info *meta.Info, extra []container.Tag) error {
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		if canceled(err) {
			return waxerr.Wrap(waxerr.CodeCanceled, "meta: metadata read canceled", err)
		}
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "meta: output not taggable", err)
	}
	// WithAllowUnsupportedDrop makes a field the output has no store for (chapters
	// or synced lyrics on WavPack, a second cover of one role on APE) a plan
	// warning instead of a hard error, which is the best-effort contract above:
	// a finished transcode should not fail because the source carried a field the
	// destination cannot hold. It is the same choice a cross-format copy makes,
	// and what waxlabel's own CLI passes for set. The refusals that survive it
	// still mean the output cannot take the metadata, so the buckets below stand.
	//
	// WithUnrecognizedPictures is the same contract for a cover whose bytes no
	// sniffer names. Since waxlabel v1.7.0 an added picture is validated at
	// Prepare, which is right for a tagger authoring an edit and wrong here:
	// every picture set below came off the source, so refusing one would fail a
	// finished transcode over art the input already carried. waxlabel's own
	// transfer engine opts out for the same reason.
	opts := []waxlabel.WriteOption{waxlabel.WithAllowUnsupportedDrop(), waxlabel.WithUnrecognizedPictures()}
	caps := doc.Capabilities(opts...)
	a := applier{m: m, path: path}
	ed := doc.Edit()
	// vals is what each key ends up holding: info's values, then the extras',
	// which replace info's of the same key, a key they repeat being
	// multi-valued as container.Tag says.
	vals := map[tag.Key][]string{}
	if info != nil {
		names := make([]string, 0, len(info.Tags))
		for k := range info.Tags {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			key, err := tag.ParseKey(k)
			if err != nil {
				continue // a key the vocabulary rejects is not worth failing a finished transcode
			}
			vals[key] = a.values(k, info.Tags[k])
		}
	}
	replaced := map[tag.Key]bool{}
	for _, t := range extra {
		key, err := tag.ParseKey(t.Key)
		if err != nil {
			continue
		}
		v := a.values(t.Key, []string{t.Value})
		if len(v) == 0 {
			continue
		}
		if !replaced[key] {
			vals[key], replaced[key] = nil, true
		}
		vals[key] = append(vals[key], v...)
	}
	keys := make([]tag.Key, 0, len(vals))
	for key := range vals {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		switch {
		case key == tag.Comment && info != nil && len(info.CommentItems) > 0:
			ed.SetComments(qualified[waxlabel.Comment](vals[key], info.CommentItems)...)
		case key == tag.Lyrics && info != nil && len(info.LyricsItems) > 0:
			ed.SetLyrics(qualified[waxlabel.Lyrics](vals[key], info.LyricsItems)...)
		default:
			ed.Set(key, vals[key]...)
		}
	}
	if info != nil {
		for _, p := range a.pictures(info.Pictures) {
			ed.AddPicture(p)
		}
		if chs := a.chapters(info.Chapters, caps.Chapters.MaxItems); len(chs) > 0 {
			ed.SetChapters(chs...)
		}
		if sets := a.synced(info.Synced, caps.SyncedLyrics.MaxItems); len(sets) > 0 {
			ed.SetSyncedLyrics(sets...)
		}
	}
	plan, err := ed.Prepare(opts...)
	if err != nil {
		// Two unrelated failures land here. The write refusals (a fragmented
		// MP4, an iloc or saio the codec cannot patch, a picture or a tag block
		// past the format's limit) mean this output cannot hold the metadata,
		// the same answer ParseFile's refusal gives above. Everything else is
		// the metadata itself being unwritable anywhere, and calling that an
		// unsupported output blames the wrong file.
		//
		// No cancellation check: Prepare takes no context and does no I/O.
		if errors.Is(err, wlerr.ErrFragmented) ||
			errors.Is(err, wlerr.ErrUnsupportedFormat) ||
			errors.Is(err, wlerr.ErrUnsupportedTag) ||
			errors.Is(err, wlerr.ErrPictureTooLarge) ||
			errors.Is(err, wlerr.ErrContentTooLarge) {
			return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "meta: output cannot take the metadata", err)
		}
		return waxerr.Wrap(waxerr.CodeInvalidRequest, "meta: metadata is not writable", err)
	}
	// What the plan leaves out is a loss the caller cannot see, since Apply
	// reports only an error. A whole item dropped is logged as one; a value
	// stored in reduced form is the ordinary cost of crossing formats.
	for _, w := range plan.Report().Warnings {
		if waxlabel.IsDiscardWarning(w.Code) {
			m.logger().Warn("meta: metadata dropped, the output cannot hold it",
				"path", path, "warning", w.String())
		} else {
			m.logger().Debug("meta: metadata reduced", "path", path, "warning", w.String())
		}
	}
	if _, res, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		// Committed with an error means the bytes landed and only a step after
		// the rename failed (the directory fsync; waxlabel v1.4.1 documents the
		// quadrant and spends the plan). The tags are on the file, so failing
		// here would report a write that happened as one that did not; the
		// durability caveat is worth a log line but not an error.
		//
		// Checked before canceled for the same reason canceled tests the error
		// and not ctx.Err(): the real outcome wins over the racing signal, and
		// here the real outcome is a landed write.
		if res.Committed {
			m.logger().Warn("meta: metadata write committed, a post-commit step failed", "path", path, "err", err)
			return nil
		}
		if canceled(err) {
			return waxerr.Wrap(waxerr.CodeCanceled, "meta: metadata write canceled", err)
		}
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "meta: metadata write", err)
	}
	return nil
}

// applier shapes an Info into what Prepare takes whole. Prepare refuses an
// edit over any one item it cannot write, which for a tagger is the right
// answer to a mistyped flag and here would cost a finished transcode all of
// its metadata over one value. So each item is made writable or left out, and
// logged, the way the library's own copy drops an item and writes the rest.
type applier struct {
	m    Mapper
	path string
}

// text is s as a writer stores it (meta.WritableText), logged when repaired.
func (a applier) text(field, s string) string {
	clean, changed := meta.WritableText(s)
	if changed {
		a.m.logger().Warn("meta: metadata text repaired, it held a NUL or invalid UTF-8",
			"path", a.path, "field", field)
	}
	return clean
}

// values is key's values made writable. One a repair leaves empty is dropped:
// an empty value is a statement of its own, and the source made none.
func (a applier) values(key string, vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if clean := a.text(key, v); clean != "" || v == "" {
			out = append(out, clean)
		}
	}
	return out
}

// pictures maps each picture to its role. The two icon roles hold one picture
// each, and Prepare refuses a second, so a second goes in as Other.
func (a applier) pictures(pics []meta.Picture) []waxlabel.Picture {
	out := make([]waxlabel.Picture, 0, len(pics))
	var icon, otherIcon bool
	for _, p := range pics {
		typ := waxlabel.PicOther
		switch t := waxlabel.PictureType(p.Type); {
		case p.Front:
			typ = waxlabel.PicFrontCover
		case t == waxlabel.PicFileIcon && !icon:
			typ, icon = t, true
		case t == waxlabel.PicOtherFileIcon && !otherIcon:
			typ, otherIcon = t, true
		case t > waxlabel.PicOtherFileIcon && t <= waxlabel.PicPublisherLogo:
			typ = t
		}
		out = append(out, waxlabel.Picture{
			Type: typ, MIME: p.MIME, Description: a.text("picture description", p.Description), Data: p.Data,
		})
	}
	return out
}

// chapters is the list a writer takes, nil when it is longer than the output
// stores: a format with a one-byte chapter count refuses the whole edit, and
// a cut list would misplace every chapter after the cut. A start before zero
// and an end before its start are times no writer stores, so they become the
// start of the file and the open end.
func (a applier) chapters(chs []container.Chapter, limit int) []waxlabel.Chapter {
	if limit > 0 && len(chs) > limit {
		a.m.logger().Warn("meta: metadata dropped, the output cannot hold it",
			"path", a.path, "chapters", len(chs), "limit", limit)
		return nil
	}
	out := make([]waxlabel.Chapter, 0, len(chs))
	for _, ch := range chs {
		c := waxlabel.Chapter{Start: max(ch.Start, 0), End: ch.End, Title: a.text("chapter title", ch.Title)}
		if c.End != 0 && c.End < c.Start {
			c.End = 0
		}
		if c.Start != ch.Start || c.End != ch.End {
			a.m.logger().Warn("meta: chapter time adjusted, no writer stores it as given",
				"path", a.path, "start", ch.Start, "end", ch.End)
		}
		out = append(out, c)
	}
	return out
}

// synced is the sets a writer takes, nil when there are more than the output
// stores, for the reason chapters gives.
func (a applier) synced(sets []meta.SyncedLyrics, limit int) []waxlabel.SyncedLyrics {
	if limit > 0 && len(sets) > limit {
		a.m.logger().Warn("meta: metadata dropped, the output cannot hold it",
			"path", a.path, "syncedLyricsSets", len(sets), "limit", limit)
		return nil
	}
	out := make([]waxlabel.SyncedLyrics, 0, len(sets))
	for _, sl := range sets {
		lines := make([]waxlabel.SyncedLine, 0, len(sl.Lines))
		for _, l := range sl.Lines {
			lines = append(lines, waxlabel.SyncedLine{Time: l.Time, Text: a.text("synced lyrics line", l.Text)})
		}
		out = append(out, waxlabel.SyncedLyrics{
			Language:    a.text("synced lyrics language", sl.Language),
			Description: a.text("synced lyrics description", sl.Description),
			Lines:       lines,
		})
	}
	return out
}

// qualified is one item per value, in order, each with the description and
// language of the source item that held the same text, taken once. A value
// no item held goes in bare: the items describe the source's texts, and a
// text a caller replaced has no description to inherit.
func qualified[T ~struct{ Text, Description, Language string }](vals []string, items []meta.TextItem) []T {
	// The items holding each text, first to last: a file may hold a hundred
	// thousand comments, so the pairing is one pass over each list.
	holding := make(map[string][]int, len(items))
	for j, src := range items {
		text, _ := meta.WritableText(src.Text)
		holding[text] = append(holding[text], j)
	}
	out := make([]T, len(vals))
	for i, v := range vals {
		it := meta.TextItem{Text: v}
		if q := holding[v]; len(q) > 0 {
			it.Description, _ = meta.WritableText(items[q[0]].Description)
			it.Language, _ = meta.WritableText(items[q[0]].Language)
			holding[v] = q[1:]
		}
		out[i] = T(it)
	}
	return out
}

// logger resolves the mapper's log, defaulting to slog.Default.
func (m Mapper) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default()
	}
	return m.log
}

// canceled reports whether err is the context giving out rather than the work
// failing. It tests the error instead of ctx.Err() so that a real failure
// racing a Ctrl-C keeps its own code and its own explanation: a disk-full
// SaveBack is output-unwritable even if the signal lands first.
func canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
