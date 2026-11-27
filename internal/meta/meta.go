// Package meta defines WaxFlow's metadata model and the Mapper seam
// between it and the tag library. The types here are plain data over
// stdlib plus the module's own packages, so the public server package
// can consume them while the waxlabel-backed implementation lives in
// the cli module (cli/label), outside the dependency-free root module
// (the CLI injects it; see server.Config.Meta).
//
// The passthrough matrix this implements: live streams embed a minimal
// descriptive tag set via each muxer's stream form (plus gapless, which
// the engine owns); jobs and CLI file outputs get the full set (tags,
// pictures, chapters, synced lyrics) written onto the finished file by
// Mapper.Apply, except MP4 outputs, whose metadata the mp4 muxer embeds
// at Begin because the mapper refuses to rewrite a fragmented MP4 (it
// reads one fine; the rewrite is what it will not do).
package meta

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/gain"
)

// Info is one source's extracted metadata.
type Info struct {
	// Tags holds canonical uppercase keys (TITLE, ARTIST,
	// REPLAYGAIN_TRACK_GAIN, ...) with their values in order.
	Tags map[string][]string
	// Chapters are the source's chapter markers, in playback order.
	Chapters []container.Chapter
	// HasPictures reports embedded art; Pictures carries the payloads
	// only when ReadOptions.Pictures asked for them.
	HasPictures bool
	Pictures    []Picture
	// Synced are the timed-lyrics sets (unsynced lyrics ride in Tags
	// under LYRICS).
	Synced []SyncedLyrics
	// CommentItems and LyricsItems say which description and language a
	// COMMENT or LYRICS value had in the source, matched to it by Text.
	// The values in Tags stay the authority: an item whose text is no
	// longer a value there describes nothing. Nil when the source told
	// none apart.
	CommentItems, LyricsItems []TextItem
	// Warnings are non-fatal notes bearing on this transfer: something the
	// source carried that may not survive onto the output, or metadata
	// that could not be read at all.
	Warnings []string
	// Notes describe the source itself and cost the transfer nothing (an
	// inherited encoder stamp, a legacy tag block beside the native one).
	// Split out because they fire on most acquired files, which is how a
	// real warning comes to be ignored. Debug-level.
	Notes []string
}

// Picture is one embedded image.
type Picture struct {
	MIME        string
	Description string
	// Type is the picture's role as ID3v2's APIC frame and FLAC's PICTURE
	// block number it (3 the front cover, 4 the back, 0 other). Front says
	// the same of the one role callers ask about.
	Type  uint8
	Front bool
	Data  []byte
}

// PictureFrontCover is the Picture.Type of a front cover.
const PictureFrontCover = 3

// TextItem is one COMMENT or LYRICS value with the description and language
// its format stored beside it (an ID3 COMM or USLT frame, a Matroska tag's
// language).
type TextItem struct {
	Text, Description, Language string
}

// WritableText returns s as every tag writer can store it: cut at its first
// NUL, where a C-string field ends, with each run of invalid UTF-8 replaced
// by U+FFFD, which is how a reader shows it. changed reports that s was not
// writable as given.
func WritableText(s string) (clean string, changed bool) {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s, changed = s[:i], true
	}
	if !utf8.ValidString(s) {
		s, changed = strings.ToValidUTF8(s, "\uFFFD"), true
	}
	return s, changed
}

// SyncedLyrics is one timed-lyrics set.
type SyncedLyrics struct {
	Language    string
	Description string
	Lines       []SyncedLine
}

// SyncedLine is one timed lyric line.
type SyncedLine struct {
	Time time.Duration
	Text string
}

// ReadOptions configures Mapper.Read.
type ReadOptions struct {
	// Pictures loads picture payloads (the /art endpoint); without it
	// only HasPictures is filled, keeping bulk reads cheap.
	Pictures bool
}

// Mapper reads source metadata and writes it onto finished outputs. The
// implementation lives in the cli module (cli/label).
type Mapper interface {
	// Read extracts metadata from a source. A source whose metadata
	// cannot be parsed yields an empty Info with warnings, not an error:
	// metadata is best-effort and audio must still flow.
	Read(ctx context.Context, src container.Source, hint string, opts ReadOptions) (*Info, error)
	// Apply writes info plus the extra tags onto the finished file at
	// path, rewriting it in place. The extra tags win over same-keyed
	// info tags (the analyzed ReplayGain values replace stale source
	// ones).
	Apply(ctx context.Context, path string, info *Info, extra []container.Tag) error
}

// Lyrics returns the source's unsynced lyrics text, or "".
func (i *Info) Lyrics() string {
	if i == nil || len(i.Tags["LYRICS"]) == 0 {
		return ""
	}
	return i.Tags["LYRICS"][0]
}

// HasLyrics reports whether /lyrics has anything to serve: an unsynced
// sheet or a synced set.
func (i *Info) HasLyrics() bool {
	return i != nil && (i.Lyrics() != "" || len(i.Synced) > 0)
}

// TagSummary is the tag map for probe-style summaries: LYRICS excluded
// (the sheet can be many KB and is served by /lyrics, reported as a
// boolean), copied so callers can never alias a shared cache entry. Nil
// when there are no tags.
func (i *Info) TagSummary() map[string][]string {
	if i == nil || len(i.Tags) == 0 {
		return nil
	}
	out := make(map[string][]string, len(i.Tags))
	for k, v := range i.Tags {
		if k != "LYRICS" {
			out[k] = v
		}
	}
	return out
}

// FrontPicture picks the cover to serve: the first front cover, else the
// first picture. Nil when none were loaded.
func (i *Info) FrontPicture() *Picture {
	if i == nil || len(i.Pictures) == 0 {
		return nil
	}
	for k := range i.Pictures {
		if i.Pictures[k].Front {
			return &i.Pictures[k]
		}
	}
	return &i.Pictures[0]
}

// SyncedLRC renders the first synced-lyrics set as LRC text, or "".
func (i *Info) SyncedLRC() string {
	if i == nil || len(i.Synced) == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range i.Synced[0].Lines {
		cs := l.Time / (10 * time.Millisecond)
		fmt.Fprintf(&b, "[%02d:%02d.%02d]%s\n", cs/6000, cs/100%60, cs%100, l.Text)
	}
	return b.String()
}

// minimalKeys is the descriptive core a live stream's headers carry, in
// the fixed order the muxers receive it. ReplayGain deliberately stays
// out: a stream that already applied gain= must not carry tags telling
// the player to apply it again.
var minimalKeys = []string{
	"TITLE", "ARTIST", "ALBUM", "ALBUMARTIST", "COMPOSER", "GENRE",
	"TRACKNUMBER", "TRACKTOTAL", "DISCNUMBER", "DISCTOTAL", "RECORDINGDATE",
}

// MinimalTags projects the live-stream tag set from a source's metadata.
// RECORDINGDATE falls back to the release then original date, so a file
// tagged with only one date still carries it.
func MinimalTags(info *Info) []container.Tag {
	if info == nil {
		return nil
	}
	var out []container.Tag
	for _, key := range minimalKeys {
		vals := info.Tags[key]
		if key == "RECORDINGDATE" && len(vals) == 0 {
			if vals = info.Tags["RELEASEDATE"]; len(vals) == 0 {
				vals = info.Tags["ORIGINALDATE"]
			}
		}
		out = appendWritable(out, key, vals)
	}
	return out
}

// appendWritable appends key's values as tags, each as a writer stores text
// (WritableText). A value the repair leaves empty is dropped: the muxers
// write a value as given, and a NUL inside one ends a C-string field early
// or, in an ID3 frame, splits it in two.
func appendWritable(out []container.Tag, key string, vals []string) []container.Tag {
	for _, v := range vals {
		if clean, _ := WritableText(v); clean != "" || v == "" {
			out = append(out, container.Tag{Key: key, Value: clean})
		}
	}
	return out
}

// FullTags flattens every tag for the muxer-embedded full set (MP4 job
// outputs), preserving each key's value order. Keys are emitted in
// sorted order for deterministic output.
func FullTags(info *Info) []container.Tag {
	if info == nil {
		return nil
	}
	keys := make([]string, 0, len(info.Tags))
	for k := range info.Tags {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var out []container.Tag
	for _, k := range keys {
		out = appendWritable(out, k, info.Tags[k])
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// WithoutReplayGain returns info minus the ReplayGain and R128 tags (a
// shallow copy; the map is rebuilt): an output whose loudness no longer
// matches the source (gain applied, fresh measurement pending) must not
// carry tags telling players to adjust again.
func WithoutReplayGain(info *Info) *Info {
	if info == nil {
		return nil
	}
	out := *info
	out.Tags = make(map[string][]string, len(info.Tags))
	for k, v := range info.Tags {
		if strings.HasPrefix(k, "REPLAYGAIN_") || strings.HasPrefix(k, "R128_") {
			continue
		}
		out.Tags[k] = v
	}
	return &out
}

// Writable returns info with all of its text as every tag writer stores it
// (WritableText), and a line naming each field it had to repair, in a fixed
// order. A tag value the repair leaves empty is dropped. Such text comes
// straight off a source (a Vorbis comment stores a NUL as readily as a
// letter), and written as given it is cut short by one reader and split in
// two by the next. info itself comes back, with no lines, when nothing
// needed repair.
func Writable(info *Info) (*Info, []string) {
	if info == nil {
		return nil, nil
	}
	var repaired []string
	text := func(what, s string) string {
		clean, changed := WritableText(s)
		if changed && !slices.Contains(repaired, what) {
			repaired = append(repaired, what)
		}
		return clean
	}
	out := *info
	keys := slices.Sorted(maps.Keys(info.Tags))
	if info.Tags != nil {
		out.Tags = make(map[string][]string, len(info.Tags))
	}
	for _, k := range keys {
		vals := make([]string, 0, len(info.Tags[k]))
		for _, v := range info.Tags[k] {
			if clean := text("tag "+k, v); clean != "" || v == "" {
				vals = append(vals, clean)
			}
		}
		if len(vals) > 0 || len(info.Tags[k]) == 0 {
			out.Tags[k] = vals
		}
	}
	items := func(key string, in []TextItem) []TextItem {
		res := slices.Clone(in)
		for i := range res {
			res[i].Text, _ = WritableText(res[i].Text) // the tag loop named it
			res[i].Description = text("tag "+key, res[i].Description)
			res[i].Language = text("tag "+key, res[i].Language)
		}
		return res
	}
	out.CommentItems, out.LyricsItems = items("COMMENT", info.CommentItems), items("LYRICS", info.LyricsItems)
	out.Chapters = writableChapters(info.Chapters, text)
	out.Pictures = slices.Clone(info.Pictures)
	for i := range out.Pictures {
		out.Pictures[i].Description = text("a picture description", out.Pictures[i].Description)
	}
	out.Synced = slices.Clone(info.Synced)
	for i := range out.Synced {
		sl := &out.Synced[i]
		sl.Language = text("synced lyrics", sl.Language)
		sl.Description = text("synced lyrics", sl.Description)
		sl.Lines = slices.Clone(sl.Lines)
		for j := range sl.Lines {
			sl.Lines[j].Text = text("synced lyrics", sl.Lines[j].Text)
		}
	}
	if len(repaired) == 0 {
		return info, nil
	}
	return &out, repaired
}

// WritableChapters returns chs with each title as a writer stores text
// (WritableText), and whether any had to be repaired; chs itself comes back
// when none did.
func WritableChapters(chs []container.Chapter) ([]container.Chapter, bool) {
	var changed bool
	out := writableChapters(chs, func(_, s string) string {
		clean, c := WritableText(s)
		changed = changed || c
		return clean
	})
	if !changed {
		return chs, false
	}
	return out, true
}

func writableChapters(chs []container.Chapter, text func(what, s string) string) []container.Chapter {
	out := slices.Clone(chs)
	for i := range out {
		out[i].Title = text("a chapter title", out[i].Title)
	}
	return out
}

// encodingKeys are stamps that would be false on another encoding of the
// same audio: the encoder that wrote the file, and mp3gain's record of the
// gain steps it applied to the frames (undoing it on another encode would
// shift a level that was never moved). What still holds stays: who encoded
// the source, its coding history, and its fingerprint, which names the
// recording and is exact wherever the samples are.
var encodingKeys = [...]string{"ENCODER", "MP3GAIN_UNDO", "MP3GAIN_MINMAX", "MP3GAIN_ALBUM_MINMAX"}

// WithoutEncoding returns info minus the stamps of the source's own encoding
// (a shallow copy; the map is rebuilt, and info itself comes back when it
// holds none). An output that was encoded again was written by another
// encoder, so the source's stamp on it would name one that did not.
func WithoutEncoding(info *Info) *Info {
	if info == nil || !slices.ContainsFunc(encodingKeys[:], func(k string) bool { _, ok := info.Tags[k]; return ok }) {
		return info
	}
	out := *info
	out.Tags = maps.Clone(info.Tags)
	for _, k := range encodingKeys {
		delete(out.Tags, k)
	}
	return &out
}

// WithContainerTags folds a container's own tags into a mapper's Info as
// the floor: the mapper's values win for every key it read, and the
// container fills in the keys it has none for (a shallow copy; the map is
// rebuilt). It is format.Info.Tags reaching the transfer, the same
// argument format.Info.Chapters already makes, and it matters most where
// no mapper spoke at all: see the Tagger doc in container/metadata.go for
// why that is now the case it exists for.
//
// A nil info with tags to carry becomes one. Nil means "no mapper spoke"
// (none wired, or a read that failed), which is exactly the case the
// fallback exists for, so nil is a floor to build on rather than a veto.
// Whether tags are wanted at all is the caller's gate, not this one:
// --no-tags skips the call, and TestNoTagsSuppressesEverySourceOfTags
// pins that. Nil in and nothing to say stays nil, so an untagged source
// through a mapperless daemon still produces no Info.
//
// Callers fold before dropping ReplayGain, never after: the container
// carries the four REPLAYGAIN_* keys too, so a later fold would put the
// source's stale values back onto an output whose gain just changed.
func WithContainerTags(info *Info, tags map[string][]string) *Info {
	if len(tags) == 0 {
		return info
	}
	out := Info{}
	if info != nil {
		out = *info
	}
	out.Tags = make(map[string][]string, len(out.Tags)+len(tags))
	maps.Copy(out.Tags, tags)
	if info != nil {
		maps.Copy(out.Tags, info.Tags)
	}
	return &out
}

// replayGainRef is the ReplayGain 2 reference loudness in LUFS.
const replayGainRef = -18

// TrackGainDB reads the source's track ReplayGain in dB: the RG2 tag,
// else the Opus R128 tag converted from its -23 LUFS reference (Q7.8
// fixed point) to RG2's -18.
func TrackGainDB(info *Info) (float64, bool) {
	return gainDB(info, "REPLAYGAIN_TRACK_GAIN", "R128_TRACK_GAIN")
}

// AlbumGainDB reads the album ReplayGain in dB, with the same R128
// fallback.
func AlbumGainDB(info *Info) (float64, bool) {
	return gainDB(info, "REPLAYGAIN_ALBUM_GAIN", "R128_ALBUM_GAIN")
}

func gainDB(info *Info, rgKey, r128Key string) (float64, bool) {
	if info == nil {
		return 0, false
	}
	if vs := info.Tags[rgKey]; len(vs) > 0 {
		if db, err := parseGainValue(vs[0]); err == nil {
			return db, true
		}
	}
	if vs := info.Tags[r128Key]; len(vs) > 0 {
		if q, err := strconv.ParseInt(strings.TrimSpace(vs[0]), 10, 32); err == nil {
			return float64(q)/256 + float64(replayGainRef-(-23)), true
		}
	}
	return 0, false
}

// parseGainValue parses "-3.10 dB" (the suffix is optional and
// case-insensitive).
func parseGainValue(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if s := strings.ToLower(v); strings.HasSuffix(s, "db") {
		v = strings.TrimSpace(v[:len(v)-2])
	}
	db, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(db) || math.IsInf(db, 0) {
		return 0, fmt.Errorf("meta: unparsable gain %q", v)
	}
	return db, nil
}

// ReplayGainGainDB converts a measured integrated loudness to the RG2
// track gain (the gain that brings the track to the -18 LUFS reference).
func ReplayGainGainDB(integratedLUFS float64) float64 {
	return replayGainRef - integratedLUFS
}

// ProjectLoudness projects an output's loudness and true peak from the source
// measurement plus the applied gain, for outputs that cannot be measured back
// (fragmented MP4, and Ogg, whose tags are written at Begin). limited says the
// encode chain's true-peak limiter is engaged; it runs for positive gain and
// for a downmix whose matrix can sum past unity, so a downmix must pass it even
// at negative gain (analyze measures the raw fold, so srcTruePeakDB can already
// exceed the ceiling).
//
// Both results are one-sided, in opposite directions. The loudness is an upper
// bound whenever the limiter engages, since its attenuation is not modelled
// (around 1 LU on 27 dB-crest content, less on real music). The peak is a real
// bound, and only because the limiter converges: it used to overshoot its own
// ceiling by up to 1.8 dB.
func ProjectLoudness(srcLUFS, srcTruePeakDB, gainDB float64, limited bool) (lufs, truePeakDB float64) {
	lufs = math.Inf(-1)
	if !math.IsInf(srcLUFS, -1) {
		lufs = srcLUFS + gainDB
	}
	truePeakDB = srcTruePeakDB + gainDB
	if limited {
		truePeakDB = min(truePeakDB, gain.DefaultCeilingDB)
	}
	return lufs, truePeakDB
}

// ReplayGainTags renders the RG2 track tags from a measurement: the gain
// against the -18 LUFS reference and the linear true peak.
func ReplayGainTags(integratedLUFS, truePeakDB float64) []container.Tag {
	peak := math.Pow(10, truePeakDB/20)
	if math.IsInf(integratedLUFS, -1) {
		// Digital silence: nothing to normalize, unity values.
		return []container.Tag{
			{Key: "REPLAYGAIN_TRACK_GAIN", Value: FormatGain(0)},
			{Key: "REPLAYGAIN_TRACK_PEAK", Value: FormatPeak(0)},
		}
	}
	return []container.Tag{
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: FormatGain(ReplayGainGainDB(integratedLUFS))},
		{Key: "REPLAYGAIN_TRACK_PEAK", Value: FormatPeak(peak)},
	}
}

// FormatGain renders an RG2 gain value. The width is fixed (sign, two
// integer digits, two decimals) so a placeholder written into an MP4
// header can be patched in place; the range covers any real measurement.
func FormatGain(db float64) string {
	db = min(max(db, -99.99), 99.99)
	return fmt.Sprintf("%+06.2f dB", db)
}

// FormatPeak renders an RG2 linear peak value at fixed width.
func FormatPeak(linear float64) string {
	linear = min(max(linear, 0), 9.999999)
	return fmt.Sprintf("%.6f", linear)
}

// maxEmbeddableTagBytes is the smallest tag block any embedding muxer writes
// (container/ogg's comment header). A tag set that fits this fits every one of
// them, so it is the budget a projected set is trimmed to.
const maxEmbeddableTagBytes = 48 << 10

// embedFixedOverhead and embedItemOverhead bound the framing the writers add
// around a tag set: the largest fixed preamble is APEv2's header plus footer,
// and the largest per-item cost is an ID3v2 frame header plus its encoding
// byte. Both are upper bounds, so a set that fits here fits each writer's own
// accounting.
const (
	embedFixedOverhead = 64
	embedItemOverhead  = 11
)

// EmbeddableTags trims a tag set projected from a source down to what every
// muxer that embeds tags can hold, returning the kept tags and the keys it
// dropped, in order: each tag is kept if it still fits. It is for the live
// stream path, whose bytes are cache-keyed and whose tags are the source's
// alone; a file writer fits its set to the output's own block instead
// (waxflow.TagsFit).
func EmbeddableTags(tags []container.Tag) (kept []container.Tag, dropped []string) {
	used := embedFixedOverhead
	for _, t := range tags {
		if used+embedCost(t) > maxEmbeddableTagBytes {
			dropped = append(dropped, t.Key)
			continue
		}
		used += embedCost(t)
		kept = append(kept, t)
	}
	return kept, dropped
}

func embedCost(t container.Tag) int { return embedItemOverhead + len(t.Key) + len(t.Value) }
