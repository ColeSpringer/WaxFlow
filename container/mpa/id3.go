package mpa

import "github.com/colespringer/waxflow/container"

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

// maxID3Bytes bounds the whole tag; the engine passes a minimal set, and
// anything past the cap is dropped rather than growing the pre-audio
// headers without limit.
const maxID3Bytes = 48 << 10

// id3v2Tag renders tags as an ID3v2.4 tag block, nil when nothing maps.
func id3v2Tag(tags []container.Tag) []byte {
	vals := make(map[string][]string, len(tags))
	for _, t := range tags {
		if t.Value != "" {
			vals[t.Key] = append(vals[t.Key], t.Value)
		}
	}
	var frames []byte
	add := func(id, text string) {
		if text == "" || len(frames)+10+1+len(text) > maxID3Bytes {
			return
		}
		frames = append(frames, id...)
		frames = appendSyncsafe(frames, uint32(1+len(text)))
		frames = append(frames, 0, 0) // frame flags
		frames = append(frames, 3)    // UTF-8 encoding
		frames = append(frames, text...)
	}
	for _, m := range id3Text {
		if vs := vals[m.key]; len(vs) > 0 {
			add(m.frame, joinValues(vs))
		}
	}
	add("TRCK", numberPair(vals["TRACKNUMBER"], vals["TRACKTOTAL"]))
	add("TPOS", numberPair(vals["DISCNUMBER"], vals["DISCTOTAL"]))
	if len(frames) == 0 {
		return nil
	}
	tag := make([]byte, 0, 10+len(frames))
	tag = append(tag, "ID3"...)
	tag = append(tag, 4, 0, 0) // v2.4.0, no flags
	tag = appendSyncsafe(tag, uint32(len(frames)))
	return append(tag, frames...)
}

func appendSyncsafe(b []byte, v uint32) []byte {
	return append(b, byte(v>>21&0x7F), byte(v>>14&0x7F), byte(v>>7&0x7F), byte(v&0x7F))
}

func joinValues(vs []string) string {
	out := vs[0]
	for _, v := range vs[1:] {
		out += "; " + v
	}
	return out
}

// numberPair renders "n" or "n/total" from the first value of each list.
func numberPair(nums, totals []string) string {
	n := ""
	if len(nums) > 0 {
		n = nums[0]
	}
	if len(totals) > 0 && totals[0] != "" {
		if n == "" {
			n = "0"
		}
		return n + "/" + totals[0]
	}
	return n
}
