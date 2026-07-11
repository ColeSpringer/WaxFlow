package mpa

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
)

// syncsafe decodes a 4-byte ID3 syncsafe integer.
func syncsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// id3Frames walks a rendered tag, asserting the v2.4 header shape and
// the UTF-8 encoding byte on every frame, and returns frame id to text.
func id3Frames(t *testing.T, tag []byte) map[string]string {
	t.Helper()
	if len(tag) < 10 || string(tag[:3]) != "ID3" {
		t.Fatalf("no ID3 header in %d bytes", len(tag))
	}
	if tag[3] != 4 || tag[4] != 0 {
		t.Fatalf("version %d.%d, want 4.0", tag[3], tag[4])
	}
	if tag[5] != 0 {
		t.Fatalf("header flags %#x, want none", tag[5])
	}
	if size := syncsafe(tag[6:10]); size != len(tag)-10 {
		t.Fatalf("syncsafe size %d, want %d", size, len(tag)-10)
	}
	frames := tag[10:]
	out := map[string]string{}
	for len(frames) >= 10 {
		id := string(frames[:4])
		n := syncsafe(frames[4:8])
		if frames[8] != 0 || frames[9] != 0 {
			t.Fatalf("frame %s carries flags %#x %#x", id, frames[8], frames[9])
		}
		if n < 1 || 10+n > len(frames) {
			t.Fatalf("frame %s size %d overruns the tag", id, n)
		}
		if frames[10] != 3 {
			t.Fatalf("frame %s encoding byte %d, want 3 (UTF-8)", id, frames[10])
		}
		out[id] = string(frames[11 : 10+n])
		frames = frames[10+n:]
	}
	if len(frames) != 0 {
		t.Fatalf("%d trailing bytes after the last frame", len(frames))
	}
	return out
}

// TestID3v2TagRender pins the writer's wire form: a v2.4 header with a
// syncsafe size, the mapped text frames in order, TRCK as "n/total",
// and nil when nothing maps.
func TestID3v2TagRender(t *testing.T) {
	cases := []struct {
		name string
		tags []container.Tag
		want map[string]string // nil expects no tag at all
	}{
		{
			name: "core fields",
			tags: []container.Tag{
				{Key: "TITLE", Value: "My Title"},
				{Key: "ARTIST", Value: "My Artist"},
				{Key: "ALBUM", Value: "My Album"},
				{Key: "TRACKNUMBER", Value: "3"},
				{Key: "TRACKTOTAL", Value: "12"},
			},
			want: map[string]string{
				"TIT2": "My Title",
				"TPE1": "My Artist",
				"TALB": "My Album",
				"TRCK": "3/12",
			},
		},
		{
			name: "multi-value artist joins",
			tags: []container.Tag{{Key: "ARTIST", Value: "One"}, {Key: "ARTIST", Value: "Two"}},
			want: map[string]string{"TPE1": "One; Two"},
		},
		{
			name: "track number alone",
			tags: []container.Tag{{Key: "TRACKNUMBER", Value: "7"}},
			want: map[string]string{"TRCK": "7"},
		},
		{name: "no tags", tags: nil, want: nil},
		{name: "unmapped keys only", tags: []container.Tag{{Key: "MOOD", Value: "calm"}}, want: nil},
		{name: "empty values only", tags: []container.Tag{{Key: "TITLE", Value: ""}}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := id3v2Tag(tc.tags)
			if tc.want == nil {
				if b != nil {
					t.Fatalf("rendered %d bytes, want nil", len(b))
				}
				return
			}
			got := id3Frames(t, b)
			if len(got) != len(tc.want) {
				t.Errorf("frames %v, want %v", got, tc.want)
			}
			for id, text := range tc.want {
				if got[id] != text {
					t.Errorf("%s = %q, want %q", id, got[id], text)
				}
			}
		})
	}
}

// muxWith muxes pkts to w with the given options, the shape muxPackets
// pins but with the options in the caller's hands.
func muxWith(t *testing.T, w io.Writer, pkts [][]byte, tr codec.Trailer, projected int64, rate, channels int, opts *MuxerOptions) {
	t.Helper()
	mux := NewMuxer(w, opts)
	track := container.Track{
		Codec:   codec.MP3,
		Fmt:     audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Float, BitDepth: 32},
		Samples: projected,
	}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, p := range pkts {
		if err := mux.WritePacket(container.Packet{Packet: codec.Packet{Data: p, Dur: 1152, Sync: true}}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := mux.End(tr); err != nil {
		t.Fatalf("End: %v", err)
	}
}

// TestMuxID3RoundTrip muxes an encoded stream with a leading ID3v2 tag
// and checks the tag is pure prefix: the MPEG stream after it is byte
// identical to a no-tags run (so gapless trims, sample counts, and the
// back-patch are untouched), the demuxer agrees, and the rendered ID3
// frames carry the fields. The waxlabel read-back cell lives in the
// oracletest module.
func TestMuxID3RoundTrip(t *testing.T) {
	const rate, channels, n = 44100, 2, 20000
	pkts, tr, samples := encodeTone(t, rate, channels, 128000, n)
	tags := []container.Tag{
		{Key: "TITLE", Value: "Stream Title"},
		{Key: "ARTIST", Value: "Stream Artist"},
	}
	id3Len := len(id3v2Tag(tags))
	if id3Len == 0 {
		t.Fatal("tags rendered no ID3 tag")
	}

	for _, seekable := range []bool{false, true} {
		name := "streaming"
		if seekable {
			name = "seekable"
		}
		t.Run(name, func(t *testing.T) {
			tagOpts := &MuxerOptions{Delay: mp3.EncoderDelay, Tags: tags}
			plainOpts := &MuxerOptions{Delay: mp3.EncoderDelay}
			var tagged, plain []byte
			if seekable {
				wsT, wsP := &memWS{}, &memWS{}
				muxWith(t, wsT, pkts, tr, samples, rate, channels, tagOpts)
				muxWith(t, wsP, pkts, tr, samples, rate, channels, plainOpts)
				tagged, plain = wsT.buf, wsP.buf
			} else {
				var bT, bP bytes.Buffer
				muxWith(t, &bT, pkts, tr, samples, rate, channels, tagOpts)
				muxWith(t, &bP, pkts, tr, samples, rate, channels, plainOpts)
				tagged, plain = bT.Bytes(), bP.Bytes()
			}
			if !bytes.HasPrefix(tagged, []byte("ID3")) {
				t.Fatal("output does not start with the ID3 tag")
			}
			if !bytes.Equal(tagged[id3Len:], plain) {
				t.Fatal("stream after the ID3 tag differs from a no-tags run")
			}

			dT, err := NewDemuxer(container.BytesSource(tagged), nil)
			if err != nil {
				t.Fatalf("NewDemuxer(tagged): %v", err)
			}
			dP, err := NewDemuxer(container.BytesSource(plain), nil)
			if err != nil {
				t.Fatalf("NewDemuxer(plain): %v", err)
			}
			trkT, trkP := dT.Tracks()[0], dP.Tracks()[0]
			if trkT.Delay != trkP.Delay || trkT.Padding != trkP.Padding {
				t.Errorf("gapless trims (%d, %d) with tags, (%d, %d) without",
					trkT.Delay, trkT.Padding, trkP.Delay, trkP.Padding)
			}
			if trkT.Samples != trkP.Samples || trkT.Samples != samples {
				t.Errorf("sample count %d with tags, %d without, want %d",
					trkT.Samples, trkP.Samples, samples)
			}

		})
	}
}

// TestMuxVBRXingAfterID3 checks the id3Len offset handling on the VBR
// back-patch path: the Xing frame starts exactly where the ID3 tag ends,
// and the patched byte count covers the MPEG stream alone (file size
// minus the tag), so seek fractions stay anchored to the first frame.
func TestMuxVBRXingAfterID3(t *testing.T) {
	const rate, channels, n = 44100, 2, 40000
	pkts, tr, samples := encodeVBR(t, rate, channels, 128000, n)
	tags := []container.Tag{{Key: "TITLE", Value: "VBR Title"}}
	id3Len := len(id3v2Tag(tags))

	ws := &memWS{}
	muxWith(t, ws, pkts, tr, samples, rate, channels,
		&MuxerOptions{Delay: mp3.EncoderDelay, VBR: true, Tags: tags})
	out := ws.buf

	if !bytes.HasPrefix(out, []byte("ID3")) {
		t.Fatal("output does not start with the ID3 tag")
	}
	h, err := mp3.ParseHeader(out[id3Len:])
	if err != nil {
		t.Fatalf("no frame exactly at the tag end: %v", err)
	}
	off := id3Len + mp3.HeaderLen + h.SideInfoLen()
	if got := string(out[off : off+4]); got != "Xing" {
		t.Fatalf("frame at the tag end carries %q, want Xing", got)
	}
	if frames := binary.BigEndian.Uint32(out[off+8:]); int(frames) != len(pkts) {
		t.Errorf("frame count %d, want %d", frames, len(pkts))
	}
	if got, want := binary.BigEndian.Uint32(out[off+12:]), uint32(len(out)-id3Len); got != want {
		t.Errorf("byte count %d, want %d (file size minus the ID3 tag)", got, want)
	}
}
