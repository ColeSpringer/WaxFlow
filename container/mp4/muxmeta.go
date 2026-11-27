package mp4

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Muxer-side movie metadata: the moov's udta box holding iTunes-style
// ilst tags (with optional cover art), Nero chpl chapters, and the
// iTunSMPB gapless atom. Everything here is built once at Begin, so the
// output stays deterministic; the only later writes are the fixed-width
// iTunSMPB back-patches at End on a seekable destination.

// ilstTextOrder is the order the text atoms are written in: a fixed one, so
// the output does not depend on the order keys were handed over. The atom of
// each key comes from the vocabulary in tagkeys.go.
var ilstTextOrder = []string{
	"TITLE", "ARTIST", "ALBUM", "ALBUMARTIST", "COMPOSER", "GENRE", "RECORDINGDATE",
	"COMMENT", "LYRICS", "COPYRIGHT", "GROUPING", "DESCRIPTION", "LONGDESCRIPTION",
	"TITLESORT", "ARTISTSORT", "ALBUMSORT", "ALBUMARTISTSORT", "COMPOSERSORT",
	"WORK", "MOVEMENTNAME", "SHOW", "EPISODEID", "NETWORK", "SHOWSORT",
}

// ilstIntegers are the keys an integer atom stores, in the order written.
// width is the atom's bytes; 0 marks a one-byte boolean.
var ilstIntegers = []struct {
	key, atom string
	width     int
}{
	{"COMPILATION", "cpil", 0},
	{"ITUNESGAPLESS", "pgap", 0},
	{"SHOWMOVEMENT", "shwm", 0},
	{"MEDIATYPE", "stik", 1},
	{"ITUNESADVISORY", "rtng", 1},
	{"MOVEMENT", "\xa9mvi", 2},
	{"MOVEMENTTOTAL", "\xa9mvc", 2},
}

// unwrittenKeys are the keys the muxer leaves out: the encoder stamps. An
// output is stamped by whatever encoded it, and whether a source's stamp
// travels is decided before the tags reach a muxer.
var unwrittenKeys = map[string]bool{"ENCODER": true, "ENCODEDBY": true}

// smpb is the iTunSMPB payload template: twelve space-prefixed fixed-width
// hex fields (the iTunes spelling). Field 2 is the encoder delay, field 3
// the trailing padding, field 4 the 64-bit trimmed sample count; the rest
// are always zero. The widths are fixed, so End can patch values into a
// written header without moving a byte.
const (
	smpbDelayOff   = 10 // " 00000000 " then eight delay digits
	smpbPaddingOff = 19
	smpbLengthOff  = 28
)

func smpbPayload(delay, length int64) string {
	return fmt.Sprintf(" 00000000 %08X %08X %016X"+
		" 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000",
		uint32(delay), uint32(0), uint64(length))
}

// TagsFit reports whether tags fit the ilst the muxer writes. It always does:
// an ilst has no block cap, so the muxer refuses no tag set for its size.
func TagsFit([]container.Tag) error { return nil }

// udtaBox assembles the moov user-data box: meta/ilst when there are tags,
// art, or an iTunSMPB payload, plus chpl when there are chapters. smpb is
// the pre-built iTunSMPB freeform atom (nil for none). Returns nil when
// there is nothing to say.
func udtaBox(tags []container.Tag, chapters []container.Chapter, art *container.Picture, smpb []byte) []byte {
	ilst := ilstBox(tags, art, smpb)
	chpl := chplBox(chapters)
	if ilst == nil && chpl == nil {
		return nil
	}
	var parts [][]byte
	if ilst != nil {
		hdlr := makeFullBox("hdlr", 0, 0,
			u32(0),
			[]byte("mdir"), []byte("appl"),
			u32(0), u32(0),
			[]byte{0})
		parts = append(parts, makeFullBox("meta", 0, 0, hdlr, makeBox("ilst", ilst)))
	}
	if chpl != nil {
		parts = append(parts, chpl)
	}
	return makeBox("udta", parts...)
}

// ilstBox builds the ilst payload (concatenated item atoms), nil when empty.
func ilstBox(tags []container.Tag, art *container.Picture, smpb []byte) []byte {
	out, _ := tagItems(tags)
	if art != nil && len(art.Data) > 0 {
		out = append(out, itemAtom("covr", dataAtom(covrType(art.MIME, art.Data), art.Data))...)
	}
	out = append(out, smpb...)
	return out
}

// DroppedTags returns the keys that have no form in the muxer's output, each
// once: a key that is not legal, an encoder stamp, and a custom key whose
// freeform would read back as another key. It names keys, not values: an
// alias spelling beside its canonical key, and a repeated value of a key
// one atom stores, are folded away without being named here.
func DroppedTags(tags []container.Tag) []string {
	_, dropped := tagItems(tags)
	return dropped
}

// tagItems renders tags as ilst items and names the keys it left out.
//
// Every key is written where waxlabel writes it, so the file reads back
// there, and through this package's reader, under the key it was handed: a
// text atom, the trkn or disk pair, an integer atom, or else a
// com.apple.iTunes freeform. A multi-valued key is one data atom per value.
//
// An integer, boolean or pair atom holds one number. Values of its key that
// the atom would store alike ("3" and "03", "1" and "true") are one value,
// as are a number with and without its total ("3" and "3/12"), so a key two
// taggers wrote still gets its atom. A key whose values
// really differ, or whose one value the atom cannot hold, goes whole to the
// freeform of its own name instead, which reads back as the same key. That
// costs the atom, and every player that reads only it, for an input that
// contradicts itself; the other choice is to pick one value and lose the
// rest without a word.
func tagItems(tags []container.Tag) (out []byte, dropped []string) {
	vals, dropped := foldTags(tags)
	// take hands a key's values to the one item that stores them, and free
	// collects what goes to a freeform instead.
	take := func(key string) []string {
		vs := vals[key]
		delete(vals, key)
		return vs
	}
	free := map[string][]string{}
	for _, key := range ilstTextOrder {
		if vs := take(key); len(vs) > 0 {
			out = append(out, itemAtom(textAtomOf[key], textData(vs)...)...)
		}
	}
	for _, p := range [...]struct {
		atom, num, total string
		size             int
	}{{"trkn", "TRACKNUMBER", "TRACKTOTAL", 8}, {"disk", "DISCNUMBER", "DISCTOTAL", 6}} {
		item, numRest, totalRest := numberPairAtom(p.atom, take(p.num), take(p.total), p.size)
		out = append(out, item...)
		free[p.num], free[p.total] = numRest, totalRest
	}
	// one renders a key that an atom of one value stores.
	one := func(key string, render func(value string) []byte) {
		vs := distinctBy(take(key), func(v string) string { return string(render(v)) })
		if len(vs) == 1 {
			if item := render(vs[0]); item != nil {
				out = append(out, item...)
				return
			}
		}
		free[key] = vs
	}
	for _, m := range ilstIntegers {
		one(m.key, func(v string) []byte { return integerAtom(m.atom, m.width, v) })
	}
	one("BPM", tempoAtom)
	one("EPISODENUMBER", func(v string) []byte { return tvNumberAtom("tves", v) })
	one("SEASONNUMBER", func(v string) []byte { return tvNumberAtom("tvsn", v) })

	maps.Copy(free, vals)
	for _, key := range slices.Sorted(maps.Keys(free)) {
		if len(free[key]) == 0 {
			continue
		}
		name := freeformName(key)
		if read, ok := freeformKey(name); !ok || read != key {
			dropped = append(dropped, key)
			continue
		}
		out = append(out, freeformAtom(name, free[key]...)...)
	}
	return out, dropped
}

// distinctBy returns vs less the values that repeat an earlier one: those
// that are the same text, or that stored gives the same non-empty form for.
func distinctBy(vs []string, stored func(v string) string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range vs {
		id := "text:" + v
		if form := stored(v); form != "" {
			id = "form:" + form
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, v)
		}
	}
	return out
}

// foldTags groups tag values under the canonical keys they are written by,
// and names the keys that are not written at all. Where a key is handed
// over in its canonical spelling, that spelling's values are the key's and
// any alias or other case of it is ignored. Otherwise its other spellings
// are folded in the order of their names, each distinct value once, so the
// result does not depend on the order tags were handed over.
func foldTags(tags []container.Tag) (vals map[string][]string, dropped []string) {
	type spellings struct {
		exact []string
		other map[string][]string
	}
	groups := map[string]*spellings{}
	for _, t := range tags {
		if t.Value == "" {
			continue
		}
		key, ok := muxKey(t.Key)
		if !ok {
			if !slices.Contains(dropped, t.Key) {
				dropped = append(dropped, t.Key)
			}
			continue
		}
		g := groups[key]
		if g == nil {
			g = &spellings{other: map[string][]string{}}
			groups[key] = g
		}
		if t.Key == key {
			g.exact = append(g.exact, t.Value)
		} else {
			g.other[t.Key] = append(g.other[t.Key], t.Value)
		}
	}
	vals = make(map[string][]string, len(groups))
	for key, g := range groups {
		if g.exact != nil {
			vals[key] = g.exact
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(g.other)) {
			for _, v := range g.other[name] {
				if !slices.Contains(vals[key], v) {
					vals[key] = append(vals[key], v)
				}
			}
		}
	}
	return vals, dropped
}

// muxKey is the canonical key a caller's key is written under: uppercased,
// an alias spelling folded onto its key. ok is false for a key that is not
// legal or that the muxer does not write.
func muxKey(key string) (string, bool) {
	key = normalizeName(key)
	if k, ok := aliasKey(key); ok {
		key = k
	}
	return key, container.ValidTagKey(key) && !unwrittenKeys[key]
}

// freeformName is the freeform name a key is written under: the tagger
// spelling where there is one, else the key itself.
func freeformName(key string) string {
	if name, ok := freeformNameOf[key]; ok {
		return name
	}
	return key
}

// textData renders values as UTF-8 data atoms, one each.
func textData(vals []string) [][]byte {
	out := make([][]byte, len(vals))
	for i, v := range vals {
		out[i] = dataAtom(dataUTF8, []byte(v))
	}
	return out
}

// covrType maps a cover onto the ilst well-known type the covr data atom
// carries. Only four image types have one; anything else (a HEIF, AVIF, JXL or
// WebP cover, or bytes no sniff recognized) goes out as 0, raw binary, which is
// what TagLib writes for a type it cannot name. Typing an arbitrary image as
// JPEG would make every reader parse it as one.
//
// The MIME decides when it names one of the four, and the bytes decide when it
// does not: container.Picture carries no MIME on the paths that read art out of
// a container that stores none, and a JPEG handed over unlabelled must not go
// out as raw binary just because nobody filled the field in.
func covrType(mime string, data []byte) uint32 {
	base, _, _ := strings.Cut(mime, ";")
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "image/gif":
		return 12
	case "image/jpeg", "image/jpg":
		return 13
	case "image/png":
		return 14
	case "image/bmp", "image/x-ms-bmp":
		return 27
	}
	return sniffCovrType(data)
}

// sniffCovrType reads the magic of the four types ilst can name, and returns 0
// for everything else. Deliberately not a general image sniffer: a type ilst
// has no number for is 0 whatever it is, so recognizing more would change no
// answer.
func sniffCovrType(data []byte) uint32 {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return 13 // JPEG SOI + the first marker
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return 14
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return 12
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return 27
	}
	return 0
}

// itemAtom wraps data atoms in their ilst item box.
func itemAtom(atom string, data ...[]byte) []byte {
	return makeBox(atom, data...)
}

// dataAtom builds the ilst value carrier: type flag (1 UTF-8 text, 0 raw
// binary, 21 integer, 12 GIF, 13 JPEG, 14 PNG, 27 BMP), a zero locale, then
// the payload.
func dataAtom(flag uint32, payload []byte) []byte {
	return makeBox("data", u32(flag), u32(0), payload)
}

// numberPairAtom renders trkn/disk: 16-bit index and total inside a raw
// binary data atom (trkn carries a trailing reserved pair, disk does not).
// A number that holds its total too ("3/12") fills both fields, and a total
// given on its own key wins. nil when neither field has a number.
//
// numRest and totalRest are what the fields cannot hold, each bound for the
// freeform of its key: several values of one key, text, and a number that
// is zero or past 16 bits, since a zero field reads as absent.
func numberPairAtom(atom string, nums, totals []string, size int) (item []byte, numRest, totalRest []string) {
	// A value given twice is one value, compared as the fields would hold it.
	nums = distinctBy(nums, func(v string) string {
		if n, t, ok := pairForm(v); ok {
			return fmt.Sprint(n, "/", t)
		}
		return ""
	})
	totals = distinctBy(totals, func(v string) string {
		if t, held := pairSlot(strings.TrimSpace(v)); held {
			return fmt.Sprint(t)
		}
		return ""
	})
	nums = mergePairValues(nums, totals)
	var n, t uint16
	total := ""
	switch {
	case len(nums) > 1:
		numRest = nums
	case len(nums) == 1:
		num, split, ok := container.SplitNumberTotal(nums[0])
		if !ok {
			num, split = strings.TrimSpace(nums[0]), ""
		}
		total = split
		if slot, held := pairSlot(num); held {
			n = slot
		} else if !ok {
			numRest = nums // not a pair: the value as handed over
		} else if num != "" {
			numRest = []string{num}
		}
	}
	switch {
	case len(totals) > 1:
		totalRest, total = totals, ""
	case len(totals) == 1:
		total = strings.TrimSpace(totals[0])
	}
	if slot, held := pairSlot(total); held {
		t = slot
	} else if len(totals) == 1 {
		totalRest = totals
	} else if total != "" {
		totalRest = []string{total}
	}
	if n == 0 && t == 0 {
		return nil, numRest, totalRest
	}
	payload := make([]byte, size)
	payload[2], payload[3] = byte(n>>8), byte(n)
	payload[4], payload[5] = byte(t>>8), byte(t)
	return itemAtom(atom, dataAtom(dataImplicit, payload)), numRest, totalRest
}

// pairForm reads a number value as the pair's two fields would hold it: the
// number, and its total where the value carries one, 0 where it does not.
// ok is false for a value either field cannot hold.
func pairForm(v string) (n, t uint16, ok bool) {
	num, total, split := container.SplitNumberTotal(v)
	if !split {
		num, total = strings.TrimSpace(v), ""
	}
	n, ok = pairSlot(num)
	if total != "" && ok {
		t, ok = pairSlot(total)
	}
	return n, t, ok
}

// mergePairValues folds values of one number that differ only in whether
// they state the total, as two taggers leave them ("3" beside "3/12"), into
// the one that states it. It leaves them alone where their numbers or
// totals differ, or where a total handed over on its own key is not that
// total: those are values that disagree.
func mergePairValues(nums, totals []string) []string {
	if len(nums) < 2 || len(totals) > 1 {
		return nums
	}
	var num, total uint16
	keep := 0
	for i, v := range nums {
		n, t, ok := pairForm(v)
		if !ok || i > 0 && n != num || t != 0 && total != 0 && t != total {
			return nums
		}
		if num = n; t != 0 && total == 0 {
			total, keep = t, i
		}
	}
	if len(totals) == 1 {
		if own, held := pairSlot(strings.TrimSpace(totals[0])); !held || total != 0 && own != total {
			return nums
		}
	}
	return nums[keep : keep+1]
}

// pairSlot is one side of a number pair as its 16-bit field holds it: a
// number from 1 to 65535.
func pairSlot(v string) (uint16, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > math.MaxUint16 {
		return 0, false
	}
	return uint16(n), true
}

// integerAtom renders an integer atom of width bytes from a decimal value,
// nil for a value that is no number or does not fit. Width 0 is a one-byte
// boolean: 1, true or yes in any case, or 0, false or no.
func integerAtom(atom string, width int, value string) []byte {
	v := strings.TrimSpace(value)
	if width == 0 {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			return itemAtom(atom, dataAtom(dataInt, []byte{1}))
		case "0", "false", "no":
			return itemAtom(atom, dataAtom(dataInt, []byte{0}))
		}
		return nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n >= 1<<(8*width) {
		return nil
	}
	payload := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		payload[i] = byte(n)
		n >>= 8
	}
	return itemAtom(atom, dataAtom(dataInt, payload))
}

// tempoAtom renders BPM as the two-byte tmpo atom, a fraction rounded to
// the nearest whole number as the tag library rounds it: the atom is where
// players read a tempo, and the fraction is the format's to lose. Only an
// unsigned decimal up to 65535 is a tempo; a sign, an exponent or any other
// text yields nil.
func tempoAtom(value string) []byte {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	dots := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '.':
			dots++
		case c < '0' || c > '9':
			return nil
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if dots > 1 || err != nil || f > math.MaxUint16 {
		return nil
	}
	return itemAtom("tmpo", dataAtom(dataInt, u16(uint16(math.Round(f)))))
}

// tvNumberAtom renders a season or episode number as its four-byte atom,
// nil unless it is written as a plain decimal that fits 31 bits.
func tvNumberAtom(atom, value string) []byte {
	n, err := strconv.ParseUint(value, 10, 31)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return nil
	}
	return itemAtom(atom, dataAtom(dataInt, u32(uint32(n))))
}

// freeformAtom builds an iTunes freeform (----) item: mean, name, then a
// UTF-8 data atom per value.
func freeformAtom(name string, values ...string) []byte {
	parts := [][]byte{
		makeFullBox("mean", 0, 0, []byte(itunesMean)),
		makeFullBox("name", 0, 0, []byte(name)),
	}
	return makeBox("----", append(parts, textData(values)...)...)
}

// chplBox renders a Nero chapter list (version 1 with the reserved word,
// the form shipping encoders write and both our demuxer and ffmpeg read).
// Times are 100-nanosecond units; the one-byte count caps the list at 255
// entries and the one-byte title length caps each title at 255 bytes,
// truncated on a rune boundary.
func chplBox(chapters []container.Chapter) []byte {
	if len(chapters) == 0 {
		return nil
	}
	if len(chapters) > 255 {
		chapters = chapters[:255]
	}
	body := []byte{byte(len(chapters))}
	for _, ch := range chapters {
		start := max(ch.Start.Nanoseconds()/100, 0)
		title := truncateRunes(ch.Title, 255)
		body = append(body, u64(uint64(start))...)
		body = append(body, byte(len(title)))
		body = append(body, title...)
	}
	return makeFullBox("chpl", 1, 0, u32(0), body)
}

// truncateRunes cuts s to at most max bytes without splitting a rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// FreeformPatcher is the file access PatchFreeform needs (os.File).
type FreeformPatcher interface {
	io.ReaderAt
	io.WriterAt
}

// maxPatchScanBytes caps the header scan for PatchFreeform: a moov
// bigger than this is not something the muxer writes (metadata values
// are bounded upstream), so the cap only guards a corrupt size field.
const maxPatchScanBytes = 64 << 20

// patchScanLen resolves how much of the file PatchFreeform must scan:
// the muxer writes ftyp then moov at the head, so their two size fields
// name the exact extent holding every ilst atom, however large the
// metadata grew. A layout this muxer did not write falls back to the
// cap.
func patchScanLen(f FreeformPatcher) int {
	var hdr [8]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil || string(hdr[4:]) != "ftyp" {
		return maxPatchScanBytes
	}
	ftypLen := int64(be32(hdr[:]))
	if _, err := f.ReadAt(hdr[:], ftypLen); err != nil || string(hdr[4:]) != "moov" {
		return maxPatchScanBytes
	}
	n := ftypLen + int64(be32(hdr[:]))
	if n <= 8 || n > maxPatchScanBytes {
		return maxPatchScanBytes
	}
	return int(n)
}

// PatchFreeform replaces the value of the freeform ilst tag key in a
// finished file, in place. placeholder must be the exact value the muxer
// wrote at Begin and value must be the same length, so no byte moves:
// this is how measured ReplayGain values land in a fragmented MP4 after
// the encode, which no tag rewriter can restructure. The written atom
// bytes are matched whole, so a payload that happens to spell the
// placeholder can never redirect the patch.
func PatchFreeform(f FreeformPatcher, key, placeholder, value string) error {
	if len(value) != len(placeholder) {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("mp4: freeform patch %q: value %q does not match placeholder width %d", key, value, len(placeholder)))
	}
	atom := freeformAtom(freeformName(key), placeholder)
	head := make([]byte, patchScanLen(f))
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "mp4: freeform patch read", err)
	}
	i := bytes.Index(head[:n], atom)
	if i < 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: freeform tag %q not found for patch", key))
	}
	off := int64(i + len(atom) - len(placeholder))
	if _, err := f.WriteAt([]byte(value), off); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: freeform patch write", err)
	}
	return nil
}
