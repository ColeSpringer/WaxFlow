// Package meta defines WaxFlow's metadata model and the Mapper seam
// between it and the tag library. The types here are plain data over
// stdlib plus the module's own packages, so the public server package
// can consume them while the waxlabel-backed implementation stays in
// the label subpackage, outside the depcheck-enforced stdlib-only tree
// (the CLI injects it; see server.Config.Meta).
//
// The passthrough matrix this implements: live streams embed a minimal
// descriptive tag set via each muxer's stream form (plus gapless, which
// the engine owns); jobs and CLI file outputs get the full set (tags,
// pictures, chapters, synced lyrics) written onto the finished file by
// Mapper.Apply, except MP4 outputs, whose metadata the mp4 muxer embeds
// at Begin because the mapper cannot rewrite fragmented MP4.
package meta

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow/container"
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
	// Warnings are non-fatal notes from the metadata parse.
	Warnings []string
}

// Picture is one embedded image.
type Picture struct {
	MIME        string
	Description string
	Front       bool
	Data        []byte
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
// implementation lives in the label subpackage.
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
		for _, v := range vals {
			out = append(out, container.Tag{Key: key, Value: v})
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
		for _, v := range info.Tags[k] {
			out = append(out, container.Tag{Key: k, Value: v})
		}
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
