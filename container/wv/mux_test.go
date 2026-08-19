package wv_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/waxerr"
)

// A .wv file is the blocks the encoder produced and nothing else, so the muxer
// is thin: the tests here are about the two things it does add, which are the
// stream length nobody knew while encoding and the tag block after the audio.

// muxFormat is the shape the fixtures below are written in.
var muxFormat = audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
	Type: audio.Int, BitDepth: 16}

// muxBlocks encodes n frames of a ramp and returns the blocks plus the track
// they belong to, with Samples as the caller's projection.
func muxBlocks(t testing.TB, n int, projected int64) (container.Track, []container.Packet) {
	t.Helper()
	enc, err := wavpack.NewEncoder(muxFormat, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkts []container.Packet
	emit := func(p codec.Packet) error {
		pkts = append(pkts, container.Packet{Packet: codec.Packet{
			Data: append([]byte(nil), p.Data...), PTS: p.PTS, Dur: p.Dur, Sync: p.Sync,
		}})
		return nil
	}
	buf := audio.Get(muxFormat, wavpack.BlockSamples)
	defer audio.Put(buf)
	for off := 0; off < n; off += wavpack.BlockSamples {
		buf.N = min(wavpack.BlockSamples, n-off)
		for c := range muxFormat.Channels {
			s := buf.ChanI(c)
			for i := range buf.N {
				s[i] = int32((off+i)%2000 - 1000)
			}
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.WavPack, CodecConfig: enc.CodecConfig(),
		Fmt: muxFormat, Samples: projected, Default: true}
	return track, pkts
}

// writeAll drives one whole mux, returning the bytes.
func writeAll(t testing.TB, dst io.Writer, track container.Track, pkts []container.Packet, opts *wv.MuxerOptions) {
	t.Helper()
	m := wv.NewMuxer(dst, opts)
	if m.NeedsSeek() {
		t.Fatal("the wv muxer claims it needs a seekable destination")
	}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	samples := int64(0)
	for _, p := range pkts {
		if err := m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
		samples += p.Dur
	}
	if err := m.End(codec.Trailer{Samples: samples}); err != nil {
		t.Fatal(err)
	}
}

// seekBuf is an in-memory io.WriteSeeker, the file destination's stand-in.
type seekBuf struct {
	b   []byte
	pos int64
}

func (s *seekBuf) Write(p []byte) (int, error) {
	if grow := s.pos + int64(len(p)) - int64(len(s.b)); grow > 0 {
		s.b = append(s.b, make([]byte, grow)...)
	}
	copy(s.b[s.pos:], p)
	s.pos += int64(len(p))
	return len(p), nil
}

func (s *seekBuf) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += s.pos
	case io.SeekEnd:
		off += int64(len(s.b))
	}
	s.pos = off
	return off, nil
}

// firstTotal reads the total-samples field out of a written stream's first
// block, which is the copy every reader consults.
func firstTotal(t testing.TB, raw []byte) int64 {
	t.Helper()
	h, err := wavpack.ParseBlockHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	return h.TotalSamples
}

// TestMuxStatesTheProjectedLength pins the field the engine's projection fills
// in: the encoder cannot know it, so the muxer writes it on the way past.
func TestMuxStatesTheProjectedLength(t *testing.T) {
	const n = 20000
	track, pkts := muxBlocks(t, n, n)
	var out bytes.Buffer
	writeAll(t, &out, track, pkts, nil)
	if got := firstTotal(t, out.Bytes()); got != n {
		t.Errorf("first block states %d samples, want %d", got, n)
	}
	// The stream reads back as one contiguous track of that length.
	d, err := wv.NewDemuxer(container.BytesSource(out.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != n || tr.Fmt != muxFormat || tr.Codec != codec.WavPack {
		t.Errorf("read back as %+v", tr)
	}
	if blocks, samples := walk(t, d); samples != n || blocks != 3 {
		t.Errorf("walked %d blocks of %d samples", blocks, samples)
	}
}

// TestMuxPatchesAnUnknownLength pins the seekable path: a caller with no
// projection gets the escape while writing and the real count at End.
func TestMuxPatchesAnUnknownLength(t *testing.T) {
	const n = 12000
	track, pkts := muxBlocks(t, n, -1)

	var plain bytes.Buffer
	writeAll(t, &plain, track, pkts, nil)
	if got := firstTotal(t, plain.Bytes()); got != -1 {
		t.Errorf("unseekable output states %d samples; an unknown length must stay the escape", got)
	}
	// The length is still recoverable, by the demuxer's own tail scan.
	d, err := wv.NewDemuxer(container.BytesSource(plain.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != n {
		t.Errorf("scanned length %d, want %d", tr.Samples, n)
	}

	var file seekBuf
	writeAll(t, &file, track, pkts, nil)
	if got := firstTotal(t, file.b); got != n {
		t.Errorf("seekable output states %d samples, want the back-patched %d", got, n)
	}
	if int64(len(file.b)) != file.pos {
		t.Errorf("the muxer left the writer at %d of %d bytes; a patch must seek back to the end",
			file.pos, len(file.b))
	}
	if !bytes.Equal(plain.Bytes()[16:], file.b[16:]) {
		t.Error("the two destinations produced different audio; only the length field may differ")
	}
}

// TestMuxCorrectsAWrongProjection pins the case the back-patch exists for: the
// engine's projection is a projection, and a stream that came out shorter must
// not leave a header claiming otherwise.
func TestMuxCorrectsAWrongProjection(t *testing.T) {
	const n = 9000
	track, pkts := muxBlocks(t, n, 99999)

	var file seekBuf
	writeAll(t, &file, track, pkts, nil)
	if got := firstTotal(t, file.b); got != n {
		t.Errorf("first block states %d samples, want the corrected %d", got, n)
	}

	// Without a seek there is nowhere to correct it, so the mismatch is an
	// error rather than a header that lies about the audio behind it.
	m := wv.NewMuxer(io.Discard, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	for _, p := range pkts {
		if err := m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.End(codec.Trailer{Samples: n}); err == nil {
		t.Error("an unseekable stream that missed its promised length succeeded")
	}
}

// TestMuxWritesTags pins the APEv2 trailer, and that the demuxer reads back
// what was written without the tag reaching the audio.
func TestMuxWritesTags(t *testing.T) {
	const n = 6000
	track, pkts := muxBlocks(t, n, n)
	tags := []container.Tag{
		{Key: "TITLE", Value: "Round Trip"},
		{Key: "ARTIST", Value: "WaxFlow"},
		{Key: "TRACKNUMBER", Value: "3"},
		{Key: "bad=key", Value: "dropped"},
	}
	var out bytes.Buffer
	writeAll(t, &out, track, pkts, &wv.MuxerOptions{Tags: tags})
	untagged := new(bytes.Buffer)
	writeAll(t, untagged, track, pkts, nil)
	if out.Len() <= untagged.Len() {
		t.Fatal("tagging the stream added no bytes")
	}

	d, err := wv.NewDemuxer(container.BytesSource(out.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := d.Tags()
	for _, want := range tags[:3] {
		if v := got[want.Key]; len(v) != 1 || v[0] != want.Value {
			t.Errorf("tag %s read back as %v, want %q", want.Key, v, want.Value)
		}
	}
	if _, ok := got["BAD=KEY"]; ok {
		t.Error("a key no muxer's vocabulary holds was written anyway")
	}
	// The audio is untouched by the trailer: same blocks, same length.
	if tr := d.Tracks()[0]; tr.Samples != n {
		t.Errorf("tagged stream reports %d samples, want %d", tr.Samples, n)
	}
	if blocks, samples := walk(t, d); samples != n || blocks != 1 {
		t.Errorf("walked %d blocks of %d samples", blocks, samples)
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("a tag after the audio drew warnings: %v", d.Warnings())
	}
}

// TestTagThatSpellsID3v1 pins the trailer peel's order. "APETAGEX" spells TAG
// at bytes three to five, so an APEv2 tag of exactly 131 bytes puts that T
// where the ID3v1 probe looks: peeling by ID3v1 first read 128 bytes off the
// middle of the tag, left three bytes of it standing as trailing garbage, and
// lost the tags entirely. The tag is sized to that length deliberately, and
// the size is asserted, so a value edited later cannot quietly stop covering
// the case.
func TestTagThatSpellsID3v1(t *testing.T) {
	const n = 4000
	track, pkts := muxBlocks(t, n, n)
	// header + footer are 32 bytes each; the item is 8 bytes plus the key, a
	// NUL, and the value.
	tag := container.Tag{Key: "TITLE", Value: "01234567890123456789012345678901234567890123456789012"}
	if got := 64 + 9 + len(tag.Key) + len(tag.Value); got != 131 {
		t.Fatalf("the fixture tag renders to %d bytes; this test needs exactly 131", got)
	}
	var plain, tagged bytes.Buffer
	writeAll(t, &plain, track, pkts, nil)
	writeAll(t, &tagged, track, pkts, &wv.MuxerOptions{Tags: []container.Tag{tag}})
	if got := tagged.Len() - plain.Len(); got != 131 {
		t.Fatalf("the tag added %d bytes, want 131", got)
	}

	d, err := wv.NewDemuxer(container.BytesSource(tagged.Bytes()), &wv.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if v := d.Tags()[tag.Key]; len(v) != 1 || v[0] != tag.Value {
		t.Errorf("tag read back as %v, want %q", v, tag.Value)
	}
	if blocks, samples := walk(t, d); samples != n || blocks != 1 {
		t.Errorf("walked %d blocks of %d samples", blocks, samples)
	}
}

// TestMuxRefusesEmptyStream pins the one output shape WavPack has no way to
// write. A file is its blocks: there is no header that stands alone, so a
// stream with no samples has nowhere to put the format, and the reference
// encoder refuses to write one at all. The muxer used to return nil over an
// empty file, which reached the CLI as "wrote out.wv" and the server as a 200
// with an empty body that then got cached.
func TestMuxRefusesEmptyStream(t *testing.T) {
	track, _ := muxBlocks(t, 4000, 0)
	var buf bytes.Buffer
	m := wv.NewMuxer(&buf, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	err := m.End(codec.Trailer{Samples: 0})
	if err == nil {
		t.Fatalf("End accepted a stream with no blocks and wrote %d bytes", buf.Len())
	}
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Errorf("refusal code is %v, want %v", waxerr.CodeOf(err), waxerr.CodeUnsupportedFormat)
	}
}

func TestMuxRefuses(t *testing.T) {
	track, pkts := muxBlocks(t, 4000, 4000)
	cases := map[string]func(m *wv.Muxer) error{
		"no tracks": func(m *wv.Muxer) error { return m.Begin(nil) },
		"two tracks": func(m *wv.Muxer) error {
			return m.Begin([]container.Track{track, track})
		},
		"another codec": func(m *wv.Muxer) error {
			t2 := track
			t2.Codec = codec.FLAC
			return m.Begin([]container.Track{t2})
		},
		"a gapless trim": func(m *wv.Muxer) error {
			t2 := track
			t2.Delay = 1024
			return m.Begin([]container.Track{t2})
		},
		"a format its config disagrees with": func(m *wv.Muxer) error {
			t2 := track
			t2.Fmt.Rate = 48000
			return m.Begin([]container.Track{t2})
		},
		"a packet that is not a block": func(m *wv.Muxer) error {
			if err := m.Begin([]container.Track{track}); err != nil {
				return err
			}
			return m.WritePacket(container.Packet{Packet: codec.Packet{Data: []byte("nope"), Dur: 1}})
		},
		"a packet whose duration disagrees with its block": func(m *wv.Muxer) error {
			if err := m.Begin([]container.Track{track}); err != nil {
				return err
			}
			p := pkts[0]
			p.Dur++
			return m.WritePacket(p)
		},
		"a second track's packets": func(m *wv.Muxer) error {
			if err := m.Begin([]container.Track{track}); err != nil {
				return err
			}
			p := pkts[0]
			p.Track = 1
			return m.WritePacket(p)
		},
		"a packet before Begin": func(m *wv.Muxer) error { return m.WritePacket(pkts[0]) },
		"End before Begin":      func(m *wv.Muxer) error { return m.End(codec.Trailer{}) },
		"Begin twice": func(m *wv.Muxer) error {
			if err := m.Begin([]container.Track{track}); err != nil {
				return err
			}
			return m.Begin([]container.Track{track})
		},
		"a trailer that disagrees with the blocks": func(m *wv.Muxer) error {
			if err := m.Begin([]container.Track{track}); err != nil {
				return err
			}
			if err := m.WritePacket(pkts[0]); err != nil {
				return err
			}
			return m.End(codec.Trailer{Samples: 1})
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(wv.NewMuxer(io.Discard, nil)); err == nil {
				t.Fatal("the muxer accepted it")
			}
		})
	}
}
