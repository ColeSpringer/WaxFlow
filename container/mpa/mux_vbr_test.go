package mpa

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
)

// encodeVBR encodes half silence, half dense broadband so the VBR encoder
// exercises several bit-rate indexes.
func encodeVBR(t *testing.T, rate, channels, anchor, n int) ([][]byte, codec.Trailer, int64) {
	t.Helper()
	f := audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Float, BitDepth: 32}
	e, err := mp3.NewEncoder(f, &mp3.EncoderOptions{Bitrate: anchor, VBR: true})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	var pkts [][]byte
	emit := func(p codec.Packet) error {
		b := make([]byte, len(p.Data))
		copy(b, p.Data)
		pkts = append(pkts, b)
		return nil
	}
	for off := 0; off < n; off += 1152 {
		m := min(1152, n-off)
		buf := audio.Get(f, m)
		buf.N = m
		for ch := 0; ch < channels; ch++ {
			for i := 0; i < m; i++ {
				v := 0.0
				if off+i >= n/2 {
					x := float64(off + i)
					for j, fq := range []float64{220, 700, 1900, 4300, 8100} {
						v += 0.12 * math.Sin(2*math.Pi*fq*x/float64(rate)+float64(j))
					}
				}
				buf.ChanF(ch)[i] = float32(v)
			}
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatalf("Encode: %v", err)
		}
		audio.Put(buf)
	}
	tr, err := e.Finish(emit)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return pkts, tr, int64(n)
}

// muxVBR muxes pkts with the VBR (Xing) metadata form.
func muxVBR(t *testing.T, w io.Writer, pkts [][]byte, tr codec.Trailer, projected int64, rate, channels int) {
	t.Helper()
	mux := NewMuxer(w, &MuxerOptions{Delay: mp3.EncoderDelay, VBR: true})
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

// TestMuxXingVBR checks the VBR metadata frame both ways: the streaming
// form carries the Xing magic, projected frame count, and the neutral
// linear TOC; the seekable form back-patches the exact frame count, the
// measured byte count, and a monotone measured TOC. Both round-trip the
// gapless invariant through the demuxer.
func TestMuxXingVBR(t *testing.T) {
	const rate, channels, n = 44100, 2, 80000
	pkts, tr, samples := encodeVBR(t, rate, channels, 128000, n)

	for _, seekable := range []bool{false, true} {
		name := "streaming"
		if seekable {
			name = "seekable"
		}
		t.Run(name, func(t *testing.T) {
			var out []byte
			if seekable {
				ws := &memWS{}
				muxVBR(t, ws, pkts, tr, samples, rate, channels)
				out = ws.buf
			} else {
				var b bytes.Buffer
				muxVBR(t, &b, pkts, tr, samples, rate, channels)
				out = b.Bytes()
			}

			h, err := mp3.ParseHeader(out)
			if err != nil {
				t.Fatalf("first frame header: %v", err)
			}
			off := mp3.HeaderLen + h.SideInfoLen()
			if got := string(out[off : off+4]); got != "Xing" {
				t.Fatalf("first frame tag %q, want Xing", got)
			}
			flags := binary.BigEndian.Uint32(out[off+4:])
			if flags != xingFlagFrames|xingFlagBytes|xingFlagTOC {
				t.Fatalf("flags %#x, want frames|bytes|toc", flags)
			}
			frames := binary.BigEndian.Uint32(out[off+8:])
			if int(frames) != len(pkts) {
				t.Errorf("frame count %d, want %d", frames, len(pkts))
			}
			bytesField := binary.BigEndian.Uint32(out[off+12:])
			toc := out[off+16 : off+116]
			for i := 1; i < 100; i++ {
				if toc[i] < toc[i-1] {
					t.Fatalf("TOC not monotone at %d: %d < %d", i, toc[i], toc[i-1])
				}
			}
			if seekable {
				if int(bytesField) != len(out) {
					t.Errorf("bytes field %d, want %d", bytesField, len(out))
				}
				// Half the stream is silence (tiny frames), so the measured
				// TOC's midpoint must sit far below the linear guess.
				if toc[50] > 100 {
					t.Errorf("measured TOC midpoint %d does not reflect the silent first half", toc[50])
				}
			} else {
				if bytesField != 0 {
					t.Errorf("streaming bytes field %d, want 0 (unknown)", bytesField)
				}
				if toc[50] != byte(50*256/100) {
					t.Errorf("streaming TOC midpoint %d, want the linear %d", toc[50], 50*256/100)
				}
			}

			// The demuxer reads the tag back: gapless trims and, on the
			// seekable form, the exact frame count.
			d, err := NewDemuxer(container.BytesSource(out), nil)
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			track := d.Tracks()[0]
			if seekable && track.Samples != samples {
				t.Errorf("trimmed length %d, want %d", track.Samples, samples)
			}
			if track.Delay != int64(mp3.EncoderDelay+529) {
				t.Errorf("Track.Delay=%d, want %d", track.Delay, mp3.EncoderDelay+529)
			}
		})
	}
}

// TestMuxXingVBRLongStream drives the TOC sampler through its stride
// doubling (more frames than tocSampleCap) and checks the table stays
// monotone and complete.
func TestMuxXingVBRLongStream(t *testing.T) {
	// Synthesize packets directly (a real encode of this length would be
	// slow): tocSampleCap*3 identical small frames.
	f := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	e, err := mp3.NewEncoder(f, &mp3.EncoderOptions{Bitrate: 64000, VBR: true})
	if err != nil {
		t.Fatal(err)
	}
	var one []byte
	emit := func(p codec.Packet) error {
		if one == nil {
			one = append([]byte(nil), p.Data...)
		}
		return nil
	}
	buf := audio.Get(f, 1152)
	buf.N = 1152
	if err := e.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	audio.Put(buf)
	if _, err := e.Finish(emit); err != nil {
		t.Fatal(err)
	}

	const frames = tocSampleCap*3 + 17
	ws := &memWS{}
	mux := NewMuxer(ws, &MuxerOptions{VBR: true})
	track := container.Track{Codec: codec.MP3, Fmt: f, Samples: -1}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < frames; i++ {
		if err := mux.WritePacket(container.Packet{Packet: codec.Packet{Data: one, Dur: 1152, Sync: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mux.End(codec.Trailer{Samples: int64(frames) * 1152}); err != nil {
		t.Fatal(err)
	}
	if mux.stride < 2 {
		t.Fatalf("stride %d: the sampler never compacted", mux.stride)
	}

	out := ws.buf
	h, err := mp3.ParseHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	off := mp3.HeaderLen + h.SideInfoLen()
	toc := out[off+16 : off+116]
	for i := 1; i < 100; i++ {
		if toc[i] < toc[i-1] {
			t.Fatalf("TOC not monotone at %d", i)
		}
	}
	// Identical frames: the measured TOC converges on the linear ramp.
	for i, v := range toc {
		want := i * 256 / 100
		if d := int(v) - want; d < -6 || d > 6 {
			t.Fatalf("TOC[%d]=%d, want ~%d (uniform frames)", i, v, want)
		}
	}
}
