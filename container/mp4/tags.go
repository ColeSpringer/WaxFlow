package mp4

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
)

// The tag reader. A movie keeps tags in up to three stores: the item list of
// moov.udta.meta, classic QuickTime text atoms directly under moov.udta, and
// the item list of a meta directly under moov, as Apple's recorders write
// it. An item list is keyed by four-character atom names, or through a keys
// index when the meta's handler is mdta. Each store is read on its own and a
// key takes its values from the first store that holds it, so a value two
// stores repeat is read once. Of duplicate udta, meta and ilst boxes the
// first is the one read.
//
// Tolerance is the parseUdta contract: an item that does not parse is
// skipped, never an error, since none of it gates playback. An empty value
// is no value, and is left out.

// The data atom types a tag value takes.
const (
	dataImplicit = 0
	dataUTF8     = 1
	dataInt      = 21
	dataUint     = 22
)

const (
	// itunesMean is the namespace of the freeform items read as tags.
	itunesMean = "com.apple.iTunes"
	// mdtaHandler marks a meta whose items are keyed through a keys index.
	mdtaHandler = "mdta"
	// maxKeysEntries bounds a keys index.
	maxKeysEntries = 4096
	// maxTagItems bounds the items walked in one list, and maxItemValues the
	// data atoms of one item. A real list holds a few dozen items of a value
	// or two each; the caps keep a crafted moov from turning into far more
	// memory than its own bytes.
	maxTagItems   = 1 << 14
	maxItemValues = 1 << 10
)

// errTagItems stops an item walk at its cap.
var errTagItems = malformed("too many metadata items")

// tagValue is one value an item holds of a canonical key.
type tagValue struct{ key, value string }

// itemID is the slot an item fills: its atom name, with a freeform item's
// namespace and name, or a keys-store item's key name alone. Items of one
// slot are one multi-valued field; items of different slots that map to one
// key are a disagreement, and the first decides.
type itemID struct{ atom, mean, name string }

// tagItem is one decoded item of a list.
type tagItem struct {
	id   itemID
	vals []tagValue
}

// readMovieTags reads the movie's tags from its first udta and the first
// meta directly under moov, and the gapless descriptor of that meta when no
// udta stated one.
func (d *Demuxer) readMovieTags(udta, meta []byte) {
	if !d.smpbOK && meta != nil {
		d.scanGapless(meta, 1)
	}
	var udtaItems, moovItems []tagItem
	var udtaList, moovList bool
	if m := firstChild(udta, "meta"); m != nil {
		udtaItems, udtaList = readMetaItems(m)
	}
	if meta != nil {
		moovItems, moovList = readMetaItems(meta)
	}
	var vals []tagValue
	held := map[string]bool{}
	// promote adds the values of the keys nothing read so far holds. Merging
	// a later store into a key would fold a disagreement into a multi-value.
	promote := func(from []tagValue) {
		for _, v := range from {
			if !held[v.key] {
				vals = append(vals, v)
			}
		}
		for _, v := range from {
			held[v.key] = true
		}
	}
	switch {
	case udtaList:
		promote(readItems(udtaItems))
		promote(udtaTexts(udta))
		promote(readItems(moovItems))
	case moovList:
		promote(readItems(moovItems))
		promote(udtaTexts(udta))
	default:
		promote(udtaTexts(udta))
	}
	// The NULs some writers terminate a value with are not part of it, and
	// an empty value is none. Both are settled here, on what is stored: every
	// rule above that splits or compares values saw them as the file has
	// them, which is how the tag library sees them.
	for _, v := range vals {
		value := strings.TrimRight(v.value, "\x00")
		if value == "" {
			continue
		}
		if d.tags == nil {
			d.tags = make(map[string][]string)
		}
		d.tags[v.key] = append(d.tags[v.key], value)
	}
}

// firstChild returns the payload of the first child box of the type, nil
// when there is none.
func firstChild(body []byte, typ string) []byte {
	var out []byte
	_ = walkBoxes(body, func(t string, payload []byte) error {
		if t == typ && out == nil {
			out = payload[:len(payload):len(payload)]
		}
		return nil
	})
	return out
}

// readMetaItems reads a meta box's item list. The box is a FullBox in ISO
// files and a plain box in some QuickTime ones: four zero bytes ahead of the
// children are the version and flags. list is false for a meta with no ilst.
func readMetaItems(payload []byte) (items []tagItem, list bool) {
	body := payload
	if len(body) >= 4 && body[0]|body[1]|body[2]|body[3] == 0 {
		body = body[4:]
	}
	var handler string
	if h := firstChild(body, "hdlr"); len(h) >= 12 {
		handler = string(h[8:12])
	}
	var keys []string
	if k := firstChild(body, "keys"); k != nil {
		keys = parseKeys(k)
	}
	ilst := firstChild(body, "ilst")
	if ilst == nil {
		return nil, false
	}
	walked := 0
	_ = walkBoxes(ilst, func(typ string, p []byte) error {
		if walked++; walked > maxTagItems {
			return errTagItems
		}
		// In a keys store the item's name is a 1-based index into the keys
		// box. One the index does not cover is read by its atom name.
		if i := be32([]byte(typ)); handler == mdtaHandler && i != 0 && uint64(i) <= uint64(len(keys)) {
			name := keys[i-1]
			if vals, ok := decodeMdtaItem(name, p); ok {
				items = append(items, tagItem{id: itemID{name: name}, vals: vals})
			}
			return nil
		}
		if id, vals, ok := decodeItem(typ, p); ok {
			items = append(items, tagItem{id: id, vals: vals})
		}
		return nil
	})
	return items, true
}

// parseKeys reads a keys box into its name index, nil for a malformed box:
// a half-read index would resolve items against the wrong names.
func parseKeys(p []byte) []string {
	if len(p) < 8 {
		return nil
	}
	count := int(be32(p[4:]))
	if count <= 0 || count > maxKeysEntries {
		return nil
	}
	p = p[8:]
	out := make([]string, 0, count)
	for range count {
		if len(p) < 8 {
			return nil
		}
		size := int64(be32(p))
		if size < 8 || size > int64(len(p)) {
			return nil
		}
		// The namespace ahead of the name is "mdta" in every file seen.
		name := p[8:size]
		if !utf8.Valid(name) {
			return nil
		}
		out = append(out, string(name))
		p = p[size:]
	}
	return out
}

// dataAtomValue is one data atom of an item: its type and value bytes.
type dataAtomValue struct {
	typ   uint32
	value []byte
}

// parseDataAtoms reads an item payload that is nothing but data atoms, ok
// false for any other shape, so a malformed item is not half read.
func parseDataAtoms(p []byte) ([]dataAtomValue, bool) {
	var out []dataAtomValue
	for len(p) >= 8 {
		size := int64(be32(p))
		if size < 16 || size > int64(len(p)) || string(p[4:8]) != "data" || len(out) >= maxItemValues {
			return nil, false
		}
		// type set (1 byte), type (3), locale (4), then the value.
		out = append(out, dataAtomValue{typ: be32(p[8:]) & 0xFFFFFF, value: p[16:size]})
		p = p[size:]
	}
	if len(p) != 0 || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// textValues reads data atoms as text, one value each: UTF-8 or the
// implicit type, and valid UTF-8, or the item is not text.
func textValues(atoms []dataAtomValue) ([]string, bool) {
	out := make([]string, 0, len(atoms))
	for _, a := range atoms {
		if a.typ != dataUTF8 && a.typ != dataImplicit || !utf8.Valid(a.value) {
			return nil, false
		}
		out = append(out, string(a.value))
	}
	return out, true
}

// decodeItem decodes one item of an iTunes list. ok is false for an atom
// this reader has no key for, cover art included, and for one that does not
// parse as its shape.
func decodeItem(atom string, payload []byte) (id itemID, vals []tagValue, ok bool) {
	id = itemID{atom: atom}
	switch atom {
	case "----":
		return decodeFreeform(payload)
	case "trkn":
		vals, ok = decodePair(payload, "TRACKNUMBER", "TRACKTOTAL")
	case "disk":
		vals, ok = decodePair(payload, "DISCNUMBER", "DISCTOTAL")
	case "gnre":
		vals, ok = decodeGnre(payload)
	case "cpil":
		vals, ok = decodeBool(payload, "COMPILATION")
	case "pgap":
		vals, ok = decodeBool(payload, "ITUNESGAPLESS")
	case "shwm":
		vals, ok = decodeBool(payload, "SHOWMOVEMENT")
	case "stik":
		vals, ok = decodeInt(payload, "MEDIATYPE")
	case "rtng":
		vals, ok = decodeInt(payload, "ITUNESADVISORY")
	case "tmpo":
		vals, ok = decodeInt(payload, "BPM")
	case "\xa9mvi":
		vals, ok = decodeInt(payload, "MOVEMENT")
	case "\xa9mvc":
		vals, ok = decodeInt(payload, "MOVEMENTTOTAL")
	case "tves":
		vals, ok = decodeInt(payload, "EPISODENUMBER")
	case "tvsn":
		vals, ok = decodeInt(payload, "SEASONNUMBER")
	default:
		key, known := ilstTextKeys[atom]
		if !known {
			return id, nil, false
		}
		vals, ok = decodeText(payload, key)
	}
	return id, vals, ok
}

// decodeText reads a text item, one value per data atom.
func decodeText(payload []byte, key string) ([]tagValue, bool) {
	atoms, ok := parseDataAtoms(payload)
	if !ok {
		return nil, false
	}
	texts, ok := textValues(atoms)
	if !ok {
		return nil, false
	}
	out := make([]tagValue, len(texts))
	for i, s := range texts {
		out[i] = tagValue{key, s}
	}
	return out, true
}

// decodePair reads a trkn or disk item: two reserved bytes, a 16-bit number,
// then a 16-bit total where the atom has room for one. A zero is absent.
//
// A text-typed atom is refused. A writer that stored "3/12" as UTF-8 is not
// hypothetical, and read as big-endian words those bytes are a track number
// in the thousands.
func decodePair(payload []byte, numKey, totalKey string) ([]tagValue, bool) {
	atoms, ok := parseDataAtoms(payload)
	if !ok {
		return nil, false
	}
	var out []tagValue
	for _, a := range atoms {
		if !integerType(a.typ) || len(a.value) < 4 {
			return nil, false
		}
		if n := be16(a.value[2:]); n != 0 {
			out = append(out, tagValue{numKey, strconv.Itoa(int(n))})
		}
		if len(a.value) >= 6 {
			if t := be16(a.value[4:]); t != 0 {
				out = append(out, tagValue{totalKey, strconv.Itoa(int(t))})
			}
		}
	}
	return out, true
}

// integerType reports whether a data atom's type holds an integer: either
// integer type, or the implicit one iTunes gives its fixed-layout atoms.
func integerType(typ uint32) bool {
	return typ == dataInt || typ == dataUint || typ == dataImplicit
}

// oneInteger returns the value bytes of an item that is a single integer
// atom.
func oneInteger(payload []byte) ([]byte, bool) {
	atoms, ok := parseDataAtoms(payload)
	if !ok || len(atoms) != 1 || !integerType(atoms[0].typ) {
		return nil, false
	}
	return atoms[0].value, true
}

// decodeInt reads an integer atom of one to four bytes as its decimal.
func decodeInt(payload []byte, key string) ([]tagValue, bool) {
	b, ok := oneInteger(payload)
	if !ok || len(b) == 0 || len(b) > 4 {
		return nil, false
	}
	var n uint64
	for _, x := range b {
		n = n<<8 | uint64(x)
	}
	return []tagValue{{key, strconv.FormatUint(n, 10)}}, true
}

// decodeBool reads a one-byte boolean atom as "1" or "0".
func decodeBool(payload []byte, key string) ([]tagValue, bool) {
	b, ok := oneInteger(payload)
	if !ok || len(b) != 1 {
		return nil, false
	}
	if b[0] != 0 {
		return []tagValue{{key, "1"}}, true
	}
	return []tagValue{{key, "0"}}, true
}

// decodeGnre reads the legacy numeric genre atom, a 1-based ID3v1 index,
// as the genre's name.
func decodeGnre(payload []byte) ([]tagValue, bool) {
	atoms, ok := parseDataAtoms(payload)
	if !ok {
		return nil, false
	}
	var out []tagValue
	for _, a := range atoms {
		if len(a.value) != 2 {
			return nil, false
		}
		n := int(be16(a.value)) - 1
		if n < 0 || n >= len(id3v1Genres) {
			return nil, false
		}
		out = append(out, tagValue{"GENRE", id3v1Genres[n]})
	}
	return out, true
}

// decodeFreeform reads a "----" item: a mean box, a name box, then data
// atoms. Only the iTunes namespace is read, and only text.
func decodeFreeform(payload []byte) (id itemID, vals []tagValue, ok bool) {
	mean, rest, ok := labelBox(payload, "mean")
	if !ok {
		return id, nil, false
	}
	name, rest, ok := labelBox(rest, "name")
	if !ok || mean != itunesMean {
		return id, nil, false
	}
	id = itemID{atom: "----", mean: mean, name: name}
	key, ok := freeformKey(name)
	if !ok {
		return id, nil, false
	}
	atoms, ok := parseDataAtoms(rest)
	if !ok {
		return id, nil, false
	}
	texts, ok := textValues(atoms)
	if !ok {
		return id, nil, false
	}
	for _, s := range texts {
		vals = appendSplit(vals, key, s)
	}
	return id, vals, true
}

// labelBox reads the mean or name box a freeform item opens with: a FullBox
// holding text. It returns the text and what follows the box.
func labelBox(p []byte, typ string) (text string, rest []byte, ok bool) {
	if len(p) < 12 {
		return "", nil, false
	}
	size := int64(be32(p))
	if size < 12 || size > int64(len(p)) || string(p[4:8]) != typ {
		return "", nil, false
	}
	return string(p[12:size]), p[size:], true
}

// decodeMdtaItem reads an item of a keys store under the key its name maps
// to. Cover art is the one value there that is not text.
func decodeMdtaItem(name string, payload []byte) ([]tagValue, bool) {
	if name == mdtaPrefix+"artwork" || name == "covr" {
		return nil, false
	}
	key, ok := mdtaKey(name)
	if !ok {
		return nil, false
	}
	atoms, ok := parseDataAtoms(payload)
	if !ok {
		return nil, false
	}
	texts, ok := textValues(atoms)
	if !ok {
		return nil, false
	}
	var vals []tagValue
	for _, s := range texts {
		vals = appendSplit(vals, key, s)
	}
	return vals, true
}

// appendSplit appends a text value under key. A track or disc number that
// holds its total too ("3/12") is two values of two keys, as trkn's are.
func appendSplit(dst []tagValue, key, value string) []tagValue {
	total := ""
	switch key {
	case "TRACKNUMBER":
		total = "TRACKTOTAL"
	case "DISCNUMBER":
		total = "DISCTOTAL"
	default:
		return append(dst, tagValue{key, value})
	}
	num, tot, split := container.SplitNumberTotal(value)
	if !split {
		return append(dst, tagValue{key, value})
	}
	if num != "" {
		dst = append(dst, tagValue{key, num})
	}
	if tot != "" {
		dst = append(dst, tagValue{total, tot})
	}
	return dst
}

// dateKeys are the keys that hold a partial date.
var dateKeys = map[string]bool{
	"RECORDINGDATE": true, "RELEASEDATE": true, "ORIGINALDATE": true, "ACQUISITION_DATE": true,
}

// readItems settles which items of one list are read for each key and
// returns their values in item order: the first slot to hold a key, and the
// other items of that slot with it. A date is the one exception to first:
// an item holding only its year gives way to a later one holding the date,
// as a tagger that writes an originalyear ahead of originaldate needs.
func readItems(items []tagItem) []tagValue {
	primary := map[string]itemID{}
	dates := map[string][]string{}
	for _, it := range items {
		seen := map[string]bool{}
		for _, v := range it.vals {
			if seen[v.key] {
				continue
			}
			seen[v.key] = true
			first, held := primary[v.key]
			if !dateKeys[v.key] {
				if !held {
					primary[v.key] = it.id
				}
				continue
			}
			vals := valuesOf(it, v.key)
			switch {
			case !held, first != it.id && echoesOf(dates[v.key], vals):
				primary[v.key], dates[v.key] = it.id, vals
			case first == it.id:
				dates[v.key] = append(dates[v.key], vals...)
			}
		}
	}
	var out []tagValue
	for _, it := range items {
		for _, v := range it.vals {
			if primary[v.key] == it.id {
				out = append(out, v)
			}
		}
	}
	return out
}

// valuesOf returns the values an item holds of key.
func valuesOf(it tagItem, key string) []string {
	var out []string
	for _, v := range it.vals {
		if v.key == key {
			out = append(out, v.value)
		}
	}
	return out
}

// echoesOf reports whether every one of years is a bare year that opens one
// of dates.
func echoesOf(years, dates []string) bool {
	if len(years) == 0 {
		return true
	}
	opens := make(map[string]bool, len(dates))
	for _, d := range dates {
		if len(d) > 4 && d[4] == '-' {
			opens[d[:4]] = true
		}
	}
	for _, y := range years {
		if !opens[y] {
			return false
		}
	}
	return true
}

// QuickTime language codes that mean no particular language: Macintosh
// English, which a writer that does not care emits, and packed "und" and
// "eng".
const (
	langMacEnglish = 0
	langUnd        = 0x55C4
	langEng        = 0x15C7
)

// maxQTTextEntries bounds the per-language entries of one udta text atom.
const maxQTTextEntries = 4096

// udtaTexts reads the classic QuickTime text atoms directly under a udta,
// one value per atom name: of several atoms of a name, the first holding a
// value.
func udtaTexts(udta []byte) []tagValue {
	var out []tagValue
	at := map[string]int{}
	_ = walkBoxes(udta, func(typ string, payload []byte) error {
		key, ok := ilstTextKeys[typ]
		if !ok {
			if key, ok = udtaTextKeys[typ]; !ok {
				return nil
			}
		}
		text, ok := udtaText(payload)
		if !ok {
			return nil
		}
		if i, seen := at[typ]; !seen {
			at[typ] = len(out)
			out = append(out, tagValue{key, text})
		} else if out[i].value == "" {
			out[i].value = text
		}
		return nil
	})
	// An atom with no text holds no value, and so keeps no later store's.
	held := out[:0]
	for _, v := range out {
		if v.value != "" {
			held = append(held, v)
		}
	}
	return held
}

// udtaText reads one udta text atom: back-to-back entries of a 16-bit text
// length, a 16-bit language and the text, or the data atoms an item list
// holds. The value is the first entry in no particular language or English,
// else the first.
func udtaText(p []byte) (string, bool) {
	if text, ok := qtText(p); ok {
		return text, true
	}
	atoms, ok := parseDataAtoms(p)
	if !ok {
		return "", false
	}
	texts, ok := textValues(atoms)
	if !ok {
		return "", false
	}
	return texts[0], true
}

func qtText(p []byte) (string, bool) {
	if len(p) < 4 {
		return "", false
	}
	var first, picked string
	n, have := 0, false
	for len(p) >= 4 {
		size, lang := int(be16(p)), be16(p[2:])
		if 4+size > len(p) || n >= maxQTTextEntries {
			return "", false
		}
		text := p[4 : 4+size]
		if !utf8.Valid(text) {
			return "", false
		}
		if n == 0 {
			first = string(text)
		}
		if !have && (lang == langMacEnglish || lang == langUnd || lang == langEng) {
			picked, have = string(text), true
		}
		n++
		p = p[4+size:]
	}
	if len(p) != 0 || n == 0 {
		return "", false
	}
	if have {
		return picked, true
	}
	return first, true
}
