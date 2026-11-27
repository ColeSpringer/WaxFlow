package mp4

import (
	"testing"

	"github.com/colespringer/waxflow/container"
)

// fxMovie builds a small movie for the metadata tests: one PCM sound track
// (8 kHz mono 16-bit, a ramp), any number of text tracks, and whatever tag
// and chapter stores a case needs. The moov leads, so a truncation or a
// mutation of the file still reaches it.
type fxMovie struct {
	frames int // audio frames; 16000 (two seconds) when 0

	// entry, unit and payload replace the sound track's sample entry, the
	// bytes of one sample and the samples themselves, for a track that is
	// not the default PCM.
	entry   []byte
	unit    int
	payload []byte

	chap           []uint32 // the sound track's tref chap ids
	text           []fxText // text tracks, in movie order behind the sound track
	udta           [][]byte // udta payloads, each its own box, in order
	meta           []byte   // a complete meta box directly under moov
	lead           [][]byte // boxes ahead of ftyp
	moov           [][]byte // further moov children, behind everything else
	mdia           [][]byte // further children of the sound track's mdia, behind its own
	trak           [][]byte // further traks, complete, behind the text tracks
	movieTimescale uint32   // 8000 when 0
}

// fxText is one text track of an fxMovie.
type fxText struct {
	id        uint32   // track id; 2 when 0
	handler   string   // "text" when empty
	timescale uint32   // 1000 when 0
	samples   [][]byte // each sample's bytes, the length prefix included
	deltas    []uint32 // the stts delta of each sample
	elst      []byte   // an elst payload, nil for no edit list
	chap      []uint32 // this track's own tref chap ids
	entry     []byte   // the sample entry; a QuickTime text one when nil
	stsd      []byte   // a whole stsd box in place of one around entry; empty for none
	pastEOF   bool     // the chunk offset points past the end of the file
	stsc      []byte   // a whole stsc box in place of the one-run one
	stz2      bool     // the sizes are in a compact stz2 box, sixteen bits each

	// again is a second copy of this track's tables with other deltas and
	// none of the damage above, appended as a whole stbl, minf or mdia
	// (againAt) behind the first.
	again   []uint32
	againAt string
}

// textSample is a chapter sample: a 16-bit length, then the title.
func textSample(title string) []byte {
	return append(u16(uint16(len(title))), title...)
}

// chapElst is the edit list a chapter track with a late first chapter
// carries: an empty edit of lead movie ticks, then one playing played ticks.
func chapElst(lead, played uint32) []byte {
	return joinBytes([]byte{0, 0, 0, 0}, u32(2),
		u32(lead), u32(0xFFFFFFFF), u32(0x00010000),
		u32(played), u32(0), u32(0x00010000))
}

func joinBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (m fxMovie) build() []byte {
	// Chunk offsets depend on the moov's size and nothing in the moov
	// depends on them, so the second pass has them right.
	head := m.head(0)
	return append(m.head(int64(len(head))+8), makeBox("mdat", m.mdat())...)
}

func (m fxMovie) audio() (entry []byte, unit, frames int, payload []byte) {
	entry, unit, frames = m.entry, m.unit, m.frames
	if frames == 0 {
		frames = 16000
	}
	if entry == nil {
		entry, unit = soundEntry("sowt", 1, 16, 8000), 2
	}
	payload = m.payload
	if payload == nil {
		payload = make([]byte, frames*unit)
		for i := range payload {
			payload[i] = byte(i)
		}
	}
	return entry, unit, frames, payload
}

func (m fxMovie) mdat() []byte {
	_, _, _, out := m.audio()
	out = append([]byte(nil), out...)
	for _, tt := range m.text {
		for _, s := range tt.samples {
			out = append(out, s...)
		}
	}
	return out
}

// head is everything ahead of the mdat, whose payload starts at base.
func (m fxMovie) head(base int64) []byte {
	entry, unit, frames, payload := m.audio()
	scale := m.movieTimescale
	if scale == 0 {
		scale = 8000
	}
	dur := uint32(uint64(frames) * uint64(scale) / 8000)
	moov := [][]byte{
		makeFullBox("mvhd", 0, 0, u32(0), u32(0), u32(scale), u32(dur),
			u32(0x00010000), u16(0x0100), u16(0), make([]byte, 8), unityMatrix(), make([]byte, 24), u32(9)),
		fxSoundTrak(1, entry, unit, frames, base, dur, m.chap, m.mdia),
	}
	off := base + int64(len(payload))
	for _, tt := range m.text {
		moov = append(moov, tt.trak(off, dur))
		for _, s := range tt.samples {
			off += int64(len(s))
		}
	}
	moov = append(moov, m.trak...)
	for _, u := range m.udta {
		moov = append(moov, makeBox("udta", u))
	}
	if m.meta != nil {
		moov = append(moov, m.meta)
	}
	moov = append(moov, m.moov...)
	return joinBytes(joinBytes(m.lead...),
		makeBox("ftyp", []byte("M4A "), u32(0), []byte("M4A "), []byte("isom")),
		makeBox("moov", moov...))
}

// fxSoundTrak renders a sound track of frames samples of unit bytes each,
// in one chunk at off, timed one tick a sample at 8 kHz.
func fxSoundTrak(id uint32, entry []byte, unit, frames int, off int64, movieDur uint32, chap []uint32, mdiaExtra [][]byte) []byte {
	stbl := makeBox("stbl",
		makeFullBox("stsd", 0, 0, u32(1), entry),
		makeFullBox("stts", 0, 0, u32(1), u32(uint32(frames)), u32(1)),
		makeFullBox("stsc", 0, 0, u32(1), u32(1), u32(uint32(frames)), u32(1)),
		makeFullBox("stsz", 0, 0, u32(uint32(unit)), u32(uint32(frames))),
		makeFullBox("stco", 0, 0, u32(1), u32(uint32(off))))
	mdia := append([][]byte{
		makeFullBox("mdhd", 0, 0, u32(0), u32(0), u32(8000), u32(uint32(frames)), u16(0x55C4), u16(0)),
		makeFullBox("hdlr", 0, 0, u32(0), []byte("soun"), make([]byte, 12), []byte{0}),
		makeBox("minf",
			makeFullBox("smhd", 0, 0, u16(0), u16(0)),
			makeBox("dinf", makeFullBox("dref", 0, 0, u32(1), makeFullBox("url ", 0, 1))),
			stbl)}, mdiaExtra...)
	trak := [][]byte{fxTkhd(id, movieDur)}
	if chap != nil {
		trak = append(trak, fxTref(chap))
	}
	return makeBox("trak", append(trak, makeBox("mdia", mdia...))...)
}

func fxTkhd(id, dur uint32) []byte {
	return makeFullBox("tkhd", 0, 7, u32(0), u32(0), u32(id), u32(0), u32(dur),
		make([]byte, 8), u16(0), u16(0), u16(0x0100), u16(0), unityMatrix(), u32(0), u32(0))
}

func fxTref(ids []uint32) []byte {
	var list []byte
	for _, id := range ids {
		list = append(list, u32(id)...)
	}
	return makeBox("tref", makeBox("chap", list))
}

// trak renders the text track, its samples in one chunk at off.
func (tt fxText) trak(off int64, movieDur uint32) []byte {
	id := tt.id
	if id == 0 {
		id = 2
	}
	stbl, minf, mdia := tt.boxes(off)
	if tt.again != nil {
		other := tt
		other.deltas, other.again = tt.again, nil
		other.stsd, other.stsc, other.stz2, other.pastEOF = nil, nil, false, false
		stbl2, minf2, mdia2 := other.boxes(off)
		dinf := makeBox("dinf", makeFullBox("dref", 0, 0, u32(1), makeFullBox("url ", 0, 1)))
		switch tt.againAt {
		case "stbl":
			minf = makeBox("minf", dinf, stbl, stbl2)
			mdia = tt.mdia(minf)
		case "minf":
			mdia = tt.mdia(minf, minf2)
		case "mdia":
			mdia = append(mdia, mdia2...)
		}
	}
	parts := [][]byte{fxTkhd(id, movieDur)}
	if tt.elst != nil {
		parts = append(parts, makeBox("edts", makeBox("elst", tt.elst)))
	}
	if tt.chap != nil {
		parts = append(parts, fxTref(tt.chap))
	}
	return makeBox("trak", append(parts, mdia)...)
}

// mdia wraps minf boxes in the track's mdia.
func (tt fxText) mdia(minf ...[]byte) []byte {
	handler, timescale := tt.handler, tt.timescale
	if handler == "" {
		handler = "text"
	}
	if timescale == 0 {
		timescale = 1000
	}
	var media uint32
	for _, d := range tt.deltas {
		media += d
	}
	return makeBox("mdia", append([][]byte{
		makeFullBox("mdhd", 0, 0, u32(0), u32(0), u32(timescale), u32(media), u16(0x55C4), u16(0)),
		makeFullBox("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), []byte{0})}, minf...)...)
}

// boxes renders the track's stbl, the minf around it and the mdia around
// that, its samples in one chunk at off.
func (tt fxText) boxes(off int64) (stbl, minf, mdia []byte) {
	n := uint32(len(tt.samples))
	stts, stsz := [][]byte{u32(n)}, [][]byte{u32(0), u32(n)}
	for i, s := range tt.samples {
		stts = append(stts, u32(1), u32(tt.deltas[i]))
		stsz = append(stsz, u32(uint32(len(s))))
	}
	entry := tt.entry
	if entry == nil {
		entry = textSampleEntry()
	}
	stsd := makeFullBox("stsd", 0, 0, u32(1), entry)
	if tt.stsd != nil {
		stsd = tt.stsd
	}
	if tt.pastEOF {
		off = 1 << 30
	}
	stsc := makeFullBox("stsc", 0, 0, u32(1), u32(1), u32(n), u32(1))
	if tt.stsc != nil {
		stsc = tt.stsc
	}
	sizes := makeFullBox("stsz", 0, 0, stsz...)
	if tt.stz2 {
		compact := [][]byte{{0, 0, 0, 16}, u32(n)}
		for _, s := range tt.samples {
			compact = append(compact, u16(uint16(len(s))))
		}
		sizes = makeFullBox("stz2", 0, 0, compact...)
	}
	stbl = makeBox("stbl",
		stsd,
		makeFullBox("stts", 0, 0, stts...),
		stsc,
		sizes,
		makeFullBox("stco", 0, 0, u32(1), u32(uint32(off))))
	minf = makeBox("minf", makeBox("dinf", makeFullBox("dref", 0, 0, u32(1), makeFullBox("url ", 0, 1))), stbl)
	return stbl, minf, tt.mdia(minf)
}

// fxChpl renders a Nero chapter list with raw title bytes, version 1.
func fxChpl(entries ...fxChplEntry) []byte {
	body := []byte{byte(len(entries))}
	for _, e := range entries {
		body = append(body, u64(uint64(e.start100ns))...)
		body = append(body, byte(len(e.title)))
		body = append(body, e.title...)
	}
	return makeFullBox("chpl", 1, 0, u32(0), body)
}

type fxChplEntry struct {
	start100ns int64
	title      string
}

// fxMeta renders an iTunes-style meta box around ilst items.
func fxMeta(items ...[]byte) []byte {
	return makeFullBox("meta", 0, 0,
		makeFullBox("hdlr", 0, 0, u32(0), []byte("mdir"), []byte("appl"), u32(0), u32(0), []byte{0}),
		makeBox("ilst", items...))
}

// fxKeysMeta renders a keys-store meta box: each name takes the next index,
// and each value is one UTF-8 data atom under it.
func fxKeysMeta(pairs ...[2]string) []byte {
	keys := [][]byte{u32(0), u32(uint32(len(pairs)))}
	var items [][]byte
	for i, p := range pairs {
		keys = append(keys, makeBox("mdta", []byte(p[0])))
		items = append(items, makeBox(string(u32(uint32(i+1))), dataAtom(1, []byte(p[1]))))
	}
	return makeBox("meta",
		makeFullBox("hdlr", 0, 0, u32(0), []byte("mdta"), make([]byte, 12), []byte{0}),
		makeBox("keys", keys...),
		makeBox("ilst", items...))
}

// openFx opens a built movie, failing the test when it does not open.
func openFx(t *testing.T, raw []byte) *Demuxer {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	return d
}
