package format

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// leadingTag is an ID3v2.4 tag of n bytes, its header included, with an
// empty body.
func leadingTag(n int) []byte {
	tag := make([]byte, n)
	copy(tag, "ID3")
	tag[3] = 4
	body := n - 10
	tag[6], tag[7], tag[8], tag[9] = byte(body>>21&0x7F), byte(body>>14&0x7F), byte(body>>7&0x7F), byte(body&0x7F)
	return tag
}

// behind is raw behind a leading tag of each size, in order.
func behind(raw []byte, sizes ...int) []byte {
	var out []byte
	for _, n := range sizes {
		out = append(out, leadingTag(n)...)
	}
	return append(out, raw...)
}

// directOpen opens a file with a container's own demuxer.
type directOpen func(container.Source) (container.Demuxer, error)

// damagedFile is a file with damage a tolerant read lists.
type damagedFile struct {
	name, hint, prefix string
	raw                []byte
	// at is where the damage is in raw, -1 where only a read of raw says.
	at int64
	// direct is the container's own open, for one that steps over a leading
	// tag itself; nil for the rest.
	direct directOpen
}

// readFixture reads a file by its path from the repository root.
func readFixture(t *testing.T, path ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{".."}, path...)...))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// damagedFiles is one damaged file per driver.
func damagedFiles(t *testing.T) []damagedFile {
	t.Helper()
	junk := bytes.Repeat([]byte{0x11}, 64)
	var files []damagedFile

	wav := readFixture(t, "testdata", "sine-s16.wav")
	at := bytes.Index(wav, []byte("data"))
	binary.LittleEndian.PutUint32(wav[at+4:], binary.LittleEndian.Uint32(wav[at+4:])+1000)
	files = append(files, damagedFile{"wav", "wav", "wav: ", wav, int64(at), nil})

	aif := readFixture(t, "testdata", "sine-s16.aiff")
	at = bytes.Index(aif, []byte("SSND"))
	binary.BigEndian.PutUint32(aif[at+4:], binary.BigEndian.Uint32(aif[at+4:])+1000)
	files = append(files, damagedFile{"aiff", "aiff", "aiff: ", aif, int64(at), nil})

	flacRaw, frames := testutil.FLACFixture(t)
	files = append(files, damagedFile{"flac", "flac", "flac: ", testutil.DamageFLACFrames(t, flacRaw, frames, 2), int64(frames[2].Off), nil})

	ogg := readFixture(t, "container", "ogg", "testdata", "golden-vorbis.ogg")
	pages := oggPageOffsets(t, ogg)
	at = pages[len(pages)/2]
	files = append(files, damagedFile{"ogg", "ogg", "ogg: ", testutil.Splice(ogg, at, junk), int64(at), nil})

	// Cut short, the second sample runs past the end of the file. It starts
	// where the first, at the chunk's start, ends.
	m4a := readFixture(t, "container", "mp4", "testdata", "alac-stereo.m4a")
	stco, stsz := bytes.Index(m4a, []byte("stco")), bytes.Index(m4a, []byte("stsz"))
	at = int(binary.BigEndian.Uint32(m4a[stco+12:]) + binary.BigEndian.Uint32(m4a[stsz+16:]))
	files = append(files, damagedFile{"mp4", "m4a", "mp4: ", m4a[:len(m4a)-100], int64(at), nil})

	mka := readFixture(t, "container", "mka", "testdata", "seed-flac.mka")
	blocks := mkaBlockOffsets(mka)
	at = blocks[len(blocks)-1]
	mka[at] = 0 // the block's track number, an invalid vint
	files = append(files, damagedFile{"mka", "mka", "mka: ", mka, int64(at), nil})

	aac := readFixture(t, "container", "adts", "testdata", "stereo.aac")
	at = 0
	for range 5 {
		at += int(aac[at+3]&3)<<11 | int(aac[at+4])<<3 | int(aac[at+5])>>5
	}
	files = append(files, damagedFile{"adts", "aac", "adts: ", testutil.Splice(aac, at, junk), int64(at),
		func(src container.Source) (container.Demuxer, error) { return adts.NewDemuxer(src, nil) }})

	// Cut short, the audio ends short of what the descriptor counts.
	ape := readFixture(t, "testdata", "sine-s16.ape")
	ape = ape[:len(ape)-100]
	files = append(files, damagedFile{"ape", "ape", "ape: ", ape, int64(len(ape)),
		func(src container.Source) (container.Demuxer, error) { return apen.NewDemuxer(src, nil) }})

	wv := readFixture(t, "testdata", "sine-s16.wv")
	var wvBlocks []int
	for off := 0; off+8 <= len(wv) && string(wv[off:off+4]) == "wvpk"; off += int(binary.LittleEndian.Uint32(wv[off+4:])) + 8 {
		wvBlocks = append(wvBlocks, off)
	}
	at = wvBlocks[len(wvBlocks)/2]
	files = append(files, damagedFile{"wavpack", "wv", "wavpack: ", testutil.Splice(wv, at, junk), int64(at), nil})

	// Cut short, the Data Object declares more than the file holds.
	wma := readFixture(t, "testdata", "sine-s16.wma")
	dataObject := []byte{0x36, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	at = bytes.Index(wma, dataObject) + 16
	files = append(files, damagedFile{"wma", "wma", "wma: ", wma[:len(wma)-1000], int64(at), nil})

	// Cut short, the frames end short of the header's count where the
	// stream does.
	mpcRaw := readFixture(t, "container", "mpc", "testdata", "gapless-sv7.mpc")
	mpcRaw = mpcRaw[:len(mpcRaw)-1000]
	files = append(files, damagedFile{"musepack", "mpc", "musepack: ", mpcRaw, int64(len(mpcRaw)),
		func(src container.Source) (container.Demuxer, error) { return mpc.NewDemuxer(src, nil) }})

	// Zeroed from the middle on, the first frame header in the zeros is where
	// the skip starts.
	mp3Raw := readFixture(t, "testdata", "sine-untagged.mp3")
	for at = 0; at < len(mp3Raw)/2; {
		h, err := mp3.ParseHeader(mp3Raw[at:])
		if err != nil || h.Size() == 0 {
			t.Fatalf("no frame at %d: %v", at, err)
		}
		at += h.Size()
	}
	files = append(files, damagedFile{"mp3", "mp3", "mp3: ", testutil.ZeroMiddle(mp3Raw, 2048), int64(at),
		func(src container.Source) (container.Demuxer, error) { return mpa.NewDemuxer(src, nil) }})
	return files
}

// oggPageOffsets is where each page of raw starts.
func oggPageOffsets(t *testing.T, raw []byte) []int {
	t.Helper()
	var offs []int
	off := 0
	for _, p := range testutil.OggPages(t, raw) {
		offs = append(offs, off)
		off += 27 + len(p.Lacing) + len(p.Body)
	}
	return offs
}

// mkaBlockOffsets is where each SimpleBlock or Block's data starts in file.
func mkaBlockOffsets(file []byte) []int {
	vint := func(b []byte, clear bool) (v uint64, n int, unknown bool) {
		if len(b) == 0 || b[0] == 0 {
			return 0, 0, false
		}
		for n = 1; b[0]&(0x80>>(n-1)) == 0; n++ {
		}
		if n > len(b) {
			return 0, 0, false
		}
		v = uint64(b[0])
		if clear {
			v &= 0xFF >> n
		}
		unknown = v == 0xFF>>n
		for _, c := range b[1:n] {
			v, unknown = v<<8|uint64(c), unknown && c == 0xFF
		}
		return v, n, unknown
	}
	var offs []int
	var walk func(off, end int)
	walk = func(off, end int) {
		for off < end {
			id, idLen, _ := vint(file[off:end], false)
			size, sizeLen, unknown := vint(file[off+idLen:end], true)
			if idLen == 0 || sizeLen == 0 {
				return
			}
			data, dataEnd := off+idLen+sizeLen, end
			if !unknown {
				dataEnd = data + int(size)
			}
			switch id {
			case 0x18538067, 0x1F43B675, 0xA0: // Segment, Cluster, BlockGroup
				walk(data, dataEnd)
			case 0xA3, 0xA1: // SimpleBlock, Block
				offs = append(offs, data)
			}
			off = dataEnd
		}
	}
	walk(0, len(file))
	return offs
}

// readToEnd reads d's packets to its end and returns what it listed, with
// the error that ended the read early.
func readToEnd(d container.Demuxer) ([]container.Warning, error) {
	var pkt container.Packet
	var err error
	for err == nil {
		err = d.ReadPacket(&pkt)
	}
	if err == io.EOF {
		err = nil
	}
	return d.(container.Warner).Warnings(), err
}

// readThrough opens raw through the driver table and reads it to its end.
func readThrough(raw []byte, hint string, strict bool) ([]container.Warning, error) {
	d, _, err := OpenDemuxer(container.BytesSource(raw), hint, &Options{Strict: strict})
	if err != nil {
		return nil, err
	}
	return readToEnd(d)
}

// shifted is ws with every localized offset moved on by n.
func shifted(ws []container.Warning, n int64) []container.Warning {
	out := slices.Clone(ws)
	for i := range out {
		if out[i].Offset >= 0 {
			out[i].Offset += n
		}
	}
	return out
}

// leads is the leading tags each file is read behind: one, and for a
// container that steps over a tag itself, two stacked.
func (f damagedFile) leads() [][]int {
	if f.direct != nil {
		return [][]int{{4096}, {4096, 512}}
	}
	return [][]int{{4096}}
}

func sum(ns []int) int64 {
	var n int64
	for _, x := range ns {
		n += int64(x)
	}
	return n
}

// TestWarningOffsetsAreFileOffsetsBehindALeadingTag: format hands a driver
// the bytes behind a leading ID3v2 tag, and what the driver lists is still
// where the damage is in the file, the same place its own open names.
func TestWarningOffsetsAreFileOffsetsBehindALeadingTag(t *testing.T) {
	for _, f := range damagedFiles(t) {
		t.Run(f.name, func(t *testing.T) {
			bare, err := readThrough(f.raw, f.hint, false)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(bare, func(w container.Warning) bool { return w.Offset >= 0 }) {
				t.Fatalf("listed %v, nothing at an offset to shift", bare)
			}
			if f.at >= 0 && !slices.ContainsFunc(bare, func(w container.Warning) bool { return w.Offset == f.at }) {
				t.Fatalf("listed %v, want damage at %d", bare, f.at)
			}
			for _, lead := range f.leads() {
				file := behind(f.raw, lead...)
				got, err := readThrough(file, f.hint, false)
				if err != nil {
					t.Fatal(err)
				}
				if want := shifted(bare, sum(lead)); !slices.Equal(got, want) {
					t.Errorf("behind tags %v: listed %v, want %v", lead, got, want)
				}
				if f.direct == nil {
					continue
				}
				d, err := f.direct(container.BytesSource(file))
				if err != nil {
					t.Fatal(err)
				}
				own, err := readToEnd(d)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, own) {
					t.Errorf("behind tags %v: listed %v, and the container's own open %v", lead, got, own)
				}
			}
		})
	}
}

// TestStrictRefusalNamesTheFileOffset: a strict read refuses the first
// damage it meets, named at its offset in the file, tag or no tag.
func TestStrictRefusalNamesTheFileOffset(t *testing.T) {
	for _, f := range damagedFiles(t) {
		t.Run(f.name, func(t *testing.T) {
			bare, err := readThrough(f.raw, f.hint, false)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(bare, func(w container.Warning) bool { return w.Kind == container.Damage })
			if i < 0 {
				t.Fatalf("listed %v, no damage", bare)
			}
			first := bare[i]
			for _, lead := range append([][]int{nil}, f.leads()...) {
				_, err := readThrough(behind(f.raw, lead...), f.hint, true)
				want := fmt.Sprintf("%s%s (at offset %d)", f.prefix, first.Msg, first.Offset+sum(lead))
				if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != want {
					t.Errorf("behind tags %v: strict read ended with %v, want %q", lead, err, want)
				}
			}
		})
	}
}

// TestErrorOffsetsAreFileOffsetsBehindALeadingTag: an offset a refusal prints
// in its own words is the file's too.
func TestErrorOffsetsAreFileOffsetsBehindALeadingTag(t *testing.T) {
	ogg := readFixture(t, "container", "ogg", "testdata", "golden-vorbis.ogg")
	headerPage := oggPageOffsets(t, ogg)[1]
	mp4 := readFixture(t, "container", "mp4", "testdata", "mp3.mp4")
	mp4 = mp4[:len(mp4)-100] // the moov, the last box, runs past the end
	mka := readFixture(t, "container", "mka", "testdata", "seed-flac.mka")
	mka = mka[:200] // the Segment runs past the end
	wv := readFixture(t, "testdata", "sine-s16.wv")
	binary.LittleEndian.PutUint32(wv[4:], uint32(2*len(wv))) // the first block runs past the end
	// The seek table's first entry disagrees with the header by one byte.
	ape := readFixture(t, "testdata", "sine-s16.ape")
	table := binary.LittleEndian.Uint32(ape[8:]) + binary.LittleEndian.Uint32(ape[12:])
	frames := int64(binary.LittleEndian.Uint32(ape[table:]))
	binary.LittleEndian.PutUint32(ape[table:], uint32(frames)+1)
	// Cut short, the first audio packet is not whole.
	mpcRaw := readFixture(t, "testdata", "sine-s16.mpc")
	mpcRaw = mpcRaw[:len(mpcRaw)-100]

	for _, c := range []struct {
		name, hint string
		raw        []byte
		format     string
		offs       []int64
		direct     directOpen
	}{
		{"ogg", "ogg", testutil.Splice(ogg, headerPage, bytes.Repeat([]byte{0x11}, 64)),
			"ogg: no page capture pattern at offset %d", []int64{int64(headerPage)}, nil},
		{"mp4", "mp4", mp4, `mp4: box "moov" at %d (size 874) runs past end %d`,
			[]int64{int64(bytes.Index(mp4, []byte("moov")) - 4), int64(len(mp4))}, nil},
		{"mka", "mka", mka, "mka: the file ends at %d, before the Segment's Tracks", []int64{int64(len(mka))}, nil},
		{"wavpack", "wv", wv, "wavpack: no parsable block at offset %d", []int64{0}, nil},
		{"ape", "ape", ape, "ape: seek table starts at %d but the header puts the frames at %d", []int64{frames + 1, frames},
			func(src container.Source) (container.Demuxer, error) { return apen.NewDemuxer(src, nil) }},
		{"musepack", "mpc", mpcRaw, "musepack: no whole packet at offset %d before the first audio packet", []int64{45},
			func(src container.Source) (container.Demuxer, error) { return mpc.NewDemuxer(src, nil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, lead := range [][]int{nil, {4096}} {
				file := behind(c.raw, lead...)
				args := make([]any, len(c.offs))
				for i, off := range c.offs {
					args[i] = off + sum(lead)
				}
				want := fmt.Sprintf(c.format, args...)
				if _, err := readThrough(file, c.hint, false); err == nil || err.Error() != want {
					t.Errorf("behind tags %v: open failed with %v, want %q", lead, err, want)
				}
				if c.direct == nil {
					continue
				}
				if _, err := c.direct(container.BytesSource(file)); err == nil || err.Error() != want {
					t.Errorf("behind tags %v: the container's own open failed with %v, want %q", lead, err, want)
				}
			}
		})
	}
}
