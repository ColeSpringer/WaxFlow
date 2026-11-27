package mpa

import (
	"fmt"
	"slices"
	"strings"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Minimal ID3v2.4 writer for the muxer's leading tag. The mapped set is
// the descriptive core; everything else (pictures, chapters, freeform
// fields) belongs to the metadata post-pass a finished file gets, not to
// a live stream's headers.

// id3Text maps canonical tag keys onto ID3v2.4 text frames, in the fixed
// order the frames are written. Multi-valued keys join with "; ": the
// null-separated v2.4 multi-value form confuses enough real players that
// the single joined string is the safer wire shape for a stream.
var id3Text = []struct{ key, frame string }{
	{"TITLE", "TIT2"},
	{"ARTIST", "TPE1"},
	{"ALBUM", "TALB"},
	{"ALBUMARTIST", "TPE2"},
	{"COMPOSER", "TCOM"},
	{"GENRE", "TCON"},
	{"RECORDINGDATE", "TDRC"},
}

// maxID3Bytes bounds the whole tag, header included; the engine passes a
// minimal set, and a frame past the cap is refused rather than growing the
// pre-audio headers without limit.
const maxID3Bytes = 48 << 10

// id3HeaderLen is the tag header: "ID3", two version bytes, flags, and the
// syncsafe size. It counts against maxID3Bytes, which bounds the whole tag.
const id3HeaderLen = 10

// id3FrameHeaderLen is a frame header: four-byte ID, syncsafe size, two flags.
const id3FrameHeaderLen = 10

// TagsFit reports whether tags fit the ID3v2 tag the muxer writes: nil, or
// Begin's own refusal. A key the tag does not map takes no room.
func TagsFit(tags []container.Tag) error {
	_, err := buildID3(tags)
	return err
}

// DroppedTags names the keys of tags the muxer's ID3v2 tag has no frame for,
// each once.
func DroppedTags(tags []container.Tag) []string {
	var out []string
	for _, t := range tags {
		if !id3Maps(t.Key) && !slices.Contains(out, t.Key) {
			out = append(out, t.Key)
		}
	}
	return out
}

// id3Maps reports whether id3v2Tag writes a frame from key.
func id3Maps(key string) bool {
	switch key {
	case "TRACKNUMBER", "TRACKTOTAL", "DISCNUMBER", "DISCTOTAL":
		return true
	}
	for _, m := range id3Text {
		if m.key == key {
			return true
		}
	}
	return false
}

// buildID3 renders the muxer's ID3v2 tag, refusing a set it cannot hold.
func buildID3(tags []container.Tag) ([]byte, error) {
	b, err := id3v2Tag(tags)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "mp3", err)
	}
	return b, nil
}

// id3v2Tag renders tags as an ID3v2.4 tag block, nil when nothing maps.
func id3v2Tag(tags []container.Tag) ([]byte, error) {
	vals := make(map[string][]string, len(tags))
	for _, t := range tags {
		if t.Value != "" {
			vals[t.Key] = append(vals[t.Key], t.Value)
		}
	}
	var frames []byte
	// A frame that overflows is refused rather than skipped, for the reason
	// apev2.Build refuses one: a tag written without it is a file missing
	// metadata the caller asked to embed, reported as a success. add returns
	// the refusal at the point of overflow, as the other three writers do.
	add := func(id, key, text string) error {
		if text == "" {
			return nil
		}
		// The tag header counts too: maxID3Bytes bounds the whole tag, and a
		// check over the frames alone lets it run 10 bytes past. The size
		// reported is the whole tag's, since a frame can overflow by being
		// large or by being the one that crossed a running total.
		need := id3HeaderLen + len(frames) + id3FrameHeaderLen + 1 + len(text)
		if need > maxID3Bytes {
			return fmt.Errorf("%s does not fit: the ID3v2 tag would need %d bytes, over its %d-byte limit",
				key, need, maxID3Bytes)
		}
		frames = append(frames, id...)
		frames = appendSyncsafe(frames, uint32(1+len(text)))
		frames = append(frames, 0, 0) // frame flags
		frames = append(frames, 3)    // UTF-8 encoding
		frames = append(frames, text...)
		return nil
	}
	for _, m := range id3Text {
		if vs := vals[m.key]; len(vs) > 0 {
			if err := add(m.frame, m.key, strings.Join(vs, "; ")); err != nil {
				return nil, err
			}
		}
	}
	// TRCK and TPOS each pack a pair into one frame, so the refusal names the
	// pair rather than picking one of the two keys and blaming it for a length
	// the other may own.
	if err := add("TRCK", "TRACKNUMBER/TRACKTOTAL",
		numberPair(vals["TRACKNUMBER"], vals["TRACKTOTAL"])); err != nil {
		return nil, err
	}
	if err := add("TPOS", "DISCNUMBER/DISCTOTAL",
		numberPair(vals["DISCNUMBER"], vals["DISCTOTAL"])); err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, nil
	}
	tag := make([]byte, 0, id3HeaderLen+len(frames))
	tag = append(tag, "ID3"...)
	tag = append(tag, 4, 0, 0) // v2.4.0, no flags
	tag = appendSyncsafe(tag, uint32(len(frames)))
	return append(tag, frames...), nil
}

func appendSyncsafe(b []byte, v uint32) []byte {
	return append(b, byte(v>>21&0x7F), byte(v>>14&0x7F), byte(v>>7&0x7F), byte(v&0x7F))
}

// numberPair renders "n" or "n/total" from the first value of each list. A
// number that holds its own total ("3/12", "3 of 12") is split, and that
// total stands only where no total key gave one.
func numberPair(nums, totals []string) string {
	n, total := "", ""
	if len(nums) > 0 {
		n, total, _ = container.SplitNumberTotal(nums[0])
	}
	if len(totals) > 0 && totals[0] != "" {
		total = totals[0]
	}
	if total == "" {
		return n
	}
	if n == "" {
		n = "0"
	}
	return n + "/" + total
}
