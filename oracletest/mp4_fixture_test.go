package oracletest

// A small MP4 builder for the cells that hold container/mp4's own reader
// against the tag library's on shapes no encoder on this box writes: a
// second tag store, a keys store, a chapter title in a legacy encoding. The
// movie is one PCM sound track (8 kHz mono 16-bit) with the moov in front,
// plus whatever text tracks and metadata boxes a cell asks for.

import (
	"context"
	"encoding/binary"
	"maps"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
)

func mp4Join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func mp4U16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func mp4U32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func mp4U64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func mp4Box(typ string, parts ...[]byte) []byte {
	body := mp4Join(parts...)
	return mp4Join(mp4U32(uint32(8+len(body))), []byte(typ), body)
}

func mp4FullBox(typ string, parts ...[]byte) []byte {
	return mp4Box(typ, append([][]byte{{0, 0, 0, 0}}, parts...)...)
}

// mp4Text is one text track of an mp4Movie.
type mp4Text struct {
	handler   string   // "text" when empty
	timescale uint32   // 1000 when 0
	samples   [][]byte // each sample's bytes, the length prefix included
	deltas    []uint32 // the stts delta of each sample
	elst      []byte   // an elst payload, nil for no edit list
	stsd      []byte   // a whole stsd box in place of the text one; empty for none
	pastEOF   bool     // the chunk offset points past the end of the file
	stsc      []byte   // a whole stsc box in place of the one-run one
	stz2      bool     // the sizes are in a compact stz2 box, sixteen bits each

	// again is a second, undamaged copy of this track's tables with other
	// deltas, appended as a whole stbl, minf or mdia (againAt).
	again   []uint32
	againAt string
}

// mp4Movie is the movie a cell builds. The sound track is track 1 and the
// text tracks follow from 2.
type mp4Movie struct {
	chap           []uint32 // the sound track's tref chap ids
	text           []mp4Text
	udta           [][]byte // udta payloads, each its own box
	meta           []byte   // a complete meta box directly under moov
	lead           []byte   // boxes ahead of ftyp
	movieTimescale uint32   // 8000 when 0
}

const mp4Frames = 16000 // two seconds

func mp4Title(title string) []byte { return mp4Join(mp4U16(uint16(len(title))), []byte(title)) }

// mp4LateElst is the edit list of a chapter track whose first chapter
// starts late: an empty edit of lead movie ticks, then played ticks of media.
func mp4LateElst(lead, played uint32) []byte {
	return mp4Join([]byte{0, 0, 0, 0}, mp4U32(2),
		mp4U32(lead), mp4U32(0xFFFFFFFF), mp4U32(0x00010000),
		mp4U32(played), mp4U32(0), mp4U32(0x00010000))
}

func (m mp4Movie) build() []byte {
	// Chunk offsets depend on the moov's size and nothing in it on them.
	head := m.head(0)
	payload := make([]byte, mp4Frames*2)
	for i := range payload {
		payload[i] = byte(i)
	}
	for _, tt := range m.text {
		payload = append(payload, mp4Join(tt.samples...)...)
	}
	return mp4Join(m.head(uint32(len(head))+8), mp4Box("mdat", payload))
}

var mp4Matrix = mp4Join(mp4U32(0x00010000), make([]byte, 12), mp4U32(0x00010000), make([]byte, 12), mp4U32(0x40000000))

func mp4Tkhd(id, dur uint32) []byte {
	return mp4Box("tkhd", []byte{0, 0, 0, 7}, mp4U32(0), mp4U32(0), mp4U32(id), mp4U32(0), mp4U32(dur),
		make([]byte, 8), mp4U16(0), mp4U16(0), mp4U16(0x0100), mp4U16(0), mp4Matrix, mp4U32(0), mp4U32(0))
}

func mp4Mdia(handler string, timescale, dur uint32, stsd []byte, stts [][]byte, stsc, sizes []byte, off uint32) []byte {
	return mp4MdiaAround(handler, timescale, dur, mp4Minf(mp4Stbl(stsd, stts, stsc, sizes, off)))
}

func mp4Stbl(stsd []byte, stts [][]byte, stsc, sizes []byte, off uint32) []byte {
	return mp4Box("stbl", stsd, mp4FullBox("stts", stts...), stsc, sizes, mp4FullBox("stco", mp4U32(1), mp4U32(off)))
}

func mp4Minf(stbl ...[]byte) []byte {
	return mp4Box("minf", append([][]byte{mp4Box("dinf", mp4FullBox("dref", mp4U32(1), mp4Box("url ", []byte{0, 0, 0, 1})))}, stbl...)...)
}

func mp4MdiaAround(handler string, timescale, dur uint32, minf ...[]byte) []byte {
	return mp4Box("mdia", append([][]byte{
		mp4FullBox("mdhd", mp4U32(0), mp4U32(0), mp4U32(timescale), mp4U32(dur), mp4U16(0x55C4), mp4U16(0)),
		mp4FullBox("hdlr", mp4U32(0), []byte(handler), make([]byte, 12), []byte{0})}, minf...)...)
}

// head is everything ahead of the mdat, whose payload starts at base.
func (m mp4Movie) head(base uint32) []byte {
	scale := m.movieTimescale
	if scale == 0 {
		scale = 8000
	}
	dur := uint32(uint64(mp4Frames) * uint64(scale) / 8000)
	// A version 0 sound entry: little-endian 16-bit PCM, mono, 8 kHz.
	sowt := mp4Box("sowt", make([]byte, 6), mp4U16(1), make([]byte, 8),
		mp4U16(1), mp4U16(16), mp4U16(0), mp4U16(0), mp4U32(8000<<16))
	sound := [][]byte{mp4Tkhd(1, dur)}
	if m.chap != nil {
		var ids []byte
		for _, id := range m.chap {
			ids = append(ids, mp4U32(id)...)
		}
		sound = append(sound, mp4Box("tref", mp4Box("chap", ids)))
	}
	sound = append(sound, mp4Mdia("soun", 8000, mp4Frames, mp4FullBox("stsd", mp4U32(1), sowt),
		[][]byte{mp4U32(1), mp4U32(mp4Frames), mp4U32(1)},
		mp4FullBox("stsc", mp4U32(1), mp4U32(1), mp4U32(mp4Frames), mp4U32(1)),
		mp4FullBox("stsz", mp4U32(2), mp4U32(mp4Frames)), base))
	moov := [][]byte{
		mp4FullBox("mvhd", mp4U32(0), mp4U32(0), mp4U32(scale), mp4U32(dur),
			mp4U32(0x00010000), mp4U16(0x0100), make([]byte, 10), mp4Matrix, make([]byte, 24), mp4U32(9)),
		mp4Box("trak", sound...),
	}
	off := base + mp4Frames*2
	for i, tt := range m.text {
		handler, timescale := tt.handler, tt.timescale
		if handler == "" {
			handler = "text"
		}
		if timescale == 0 {
			timescale = 1000
		}
		n := uint32(len(tt.samples))
		stts, stsz := [][]byte{mp4U32(n)}, [][]byte{mp4U32(0), mp4U32(n)}
		var media, size uint32
		for j, s := range tt.samples {
			stts = append(stts, mp4U32(1), mp4U32(tt.deltas[j]))
			stsz = append(stsz, mp4U32(uint32(len(s))))
			media += tt.deltas[j]
			size += uint32(len(s))
		}
		// A QuickTime text sample entry with every display field neutral.
		stsd := mp4FullBox("stsd", mp4U32(1), mp4Box("text", make([]byte, 6), mp4U16(1), make([]byte, 44)))
		if tt.stsd != nil {
			stsd = tt.stsd
		}
		at := off
		if tt.pastEOF {
			at = 1 << 30
		}
		trak := [][]byte{mp4Tkhd(uint32(i+2), dur)}
		if tt.elst != nil {
			trak = append(trak, mp4Box("edts", mp4Box("elst", tt.elst)))
		}
		stsc := mp4FullBox("stsc", mp4U32(1), mp4U32(1), mp4U32(n), mp4U32(1))
		if tt.stsc != nil {
			stsc = tt.stsc
		}
		sizes := mp4FullBox("stsz", stsz...)
		if tt.stz2 {
			compact := [][]byte{{0, 0, 0, 16}, mp4U32(n)}
			for _, s := range tt.samples {
				compact = append(compact, mp4U16(uint16(len(s))))
			}
			sizes = mp4FullBox("stz2", compact...)
		}
		mdia := [][]byte{mp4Mdia(handler, timescale, media, stsd, stts, stsc, sizes, at)}
		if tt.again != nil {
			stts2 := [][]byte{mp4U32(n)}
			for _, d := range tt.again {
				stts2 = append(stts2, mp4U32(1), mp4U32(d))
			}
			first := mp4Stbl(stsd, stts, stsc, sizes, at)
			second := mp4Stbl(mp4FullBox("stsd", mp4U32(1), mp4Box("text", make([]byte, 6), mp4U16(1), make([]byte, 44))), stts2,
				mp4FullBox("stsc", mp4U32(1), mp4U32(1), mp4U32(n), mp4U32(1)), mp4FullBox("stsz", stsz...), off)
			switch tt.againAt {
			case "stbl":
				mdia = [][]byte{mp4MdiaAround(handler, timescale, media, mp4Minf(first, second))}
			case "minf":
				mdia = [][]byte{mp4MdiaAround(handler, timescale, media, mp4Minf(first), mp4Minf(second))}
			case "mdia":
				mdia = append(mdia, mp4MdiaAround(handler, timescale, media, mp4Minf(second)))
			}
		}
		moov = append(moov, mp4Box("trak", append(trak, mdia...)...))
		off += size
	}
	for _, u := range m.udta {
		moov = append(moov, mp4Box("udta", u))
	}
	if m.meta != nil {
		moov = append(moov, m.meta)
	}
	return mp4Join(m.lead,
		mp4Box("ftyp", []byte("M4A "), mp4U32(0), []byte("M4A "), []byte("isom")),
		mp4Box("moov", moov...))
}

// mp4Chpl renders a Nero chapter list: starts in 100 ns units and raw title
// bytes, in pairs.
func mp4Chpl(entries ...any) []byte {
	body := []byte{byte(len(entries) / 2)}
	for i := 0; i < len(entries); i += 2 {
		title := entries[i+1].(string)
		body = append(body, mp4U64(uint64(entries[i].(int)))...)
		body = append(body, byte(len(title)))
		body = append(body, title...)
	}
	return mp4Box("chpl", []byte{1, 0, 0, 0}, mp4U32(0), body)
}

// mp4Data is a data atom of the given type.
func mp4Data(typ uint32, value []byte) []byte {
	return mp4Box("data", mp4U32(typ), mp4U32(0), value)
}

// mp4Item is a text item holding one UTF-8 value.
func mp4Item(atom, value string) []byte { return mp4Box(atom, mp4Data(1, []byte(value))) }

// mp4Freeform is a freeform item with one text value.
func mp4Freeform(mean, name, value string) []byte {
	return mp4Box("----", mp4FullBox("mean", []byte(mean)), mp4FullBox("name", []byte(name)), mp4Data(1, []byte(value)))
}

// mp4Meta is an iTunes-style meta box around items.
func mp4Meta(items ...[]byte) []byte {
	return mp4FullBox("meta",
		mp4FullBox("hdlr", mp4U32(0), []byte("mdir"), []byte("appl"), mp4U32(0), mp4U32(0), []byte{0}),
		mp4Box("ilst", items...))
}

// mp4KeysMeta is a keys-store meta box: name and value pairs, each value one
// UTF-8 data atom under the next index.
func mp4KeysMeta(pairs ...string) []byte {
	keys := [][]byte{mp4U32(0), mp4U32(uint32(len(pairs) / 2))}
	var items [][]byte
	for i := 0; i < len(pairs); i += 2 {
		keys = append(keys, mp4Box("mdta", []byte(pairs[i])))
		items = append(items, mp4Box(string(mp4U32(uint32(i/2+1))), mp4Data(1, []byte(pairs[i+1]))))
	}
	return mp4Box("meta",
		mp4FullBox("hdlr", mp4U32(0), []byte("mdta"), make([]byte, 12), []byte{0}),
		mp4Box("keys", keys...),
		mp4Box("ilst", items...))
}

// mp4Readers reads raw through container/mp4 alone, as an embedder with no
// mapper does, and through the tag library behind cli/label.
func mp4Readers(t *testing.T, raw []byte) (native *format.Info, mapped *meta.Info) {
	t.Helper()
	native, err := waxflow.New().Probe(context.Background(), container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatalf("the container does not open the movie: %v", err)
	}
	mapped, err = label.New().Read(t.Context(), container.BytesSource(raw), "m4a", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range mapped.Warnings {
		if len(w) > 15 && w[:15] == "metadata unread" {
			t.Fatalf("the tag library does not read the movie: %s", w)
		}
	}
	return native, mapped
}

// mp4ChaptersAgree requires both readers to give the same chapters, ends
// included, and returns them.
func mp4ChaptersAgree(t *testing.T, raw []byte) []container.Chapter {
	t.Helper()
	native, mapped := mp4Readers(t, raw)
	if !slices.Equal(native.Chapters, mapped.Chapters) {
		t.Errorf("the container read %+v, the tag library %+v", native.Chapters, mapped.Chapters)
	}
	return native.Chapters
}

// mp4TagsWithin requires every key the container reads to be one the tag
// library reads too, with the same values, and returns the container's tags.
func mp4TagsWithin(t *testing.T, raw []byte) map[string][]string {
	t.Helper()
	native, mapped := mp4Readers(t, raw)
	for _, key := range slices.Sorted(maps.Keys(native.Tags)) {
		if !slices.Equal(native.Tags[key], mapped.Tags[key]) {
			t.Errorf("%s: the container read %q, the tag library %q", key, native.Tags[key], mapped.Tags[key])
		}
	}
	return native.Tags
}
