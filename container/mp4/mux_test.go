package mp4

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/container"
)

// muxALAC encodes src to a fragmented ALAC MP4 in memory using the given
// fragment target, returning the bytes.
func muxALAC(t *testing.T, src *audio.Buffer, fragSamples int) []byte {
	t.Helper()
	enc, err := alac.NewEncoder(src.Fmt, nil)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	var buf bytes.Buffer
	m := NewMuxer(&buf, &MuxerOptions{FragmentSamples: fragSamples})
	track := container.Track{
		Codec:       codec.ALAC,
		CodecConfig: enc.CodecConfig(),
		Fmt:         src.Fmt,
		Samples:     int64(src.N),
		Default:     true,
	}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	frame := audio.Get(src.Fmt, alac.FrameSize)
	defer audio.Put(frame)
	for off := 0; off < src.N; off += alac.FrameSize {
		n := min(alac.FrameSize, src.N-off)
		frame.N = n
		for c := 0; c < src.Fmt.Channels; c++ {
			copy(frame.ChanI(c), src.I[c*src.Stride+off:c*src.Stride+off+n])
		}
		if err := enc.Encode(frame, emit); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatalf("End: %v", err)
	}
	return buf.Bytes()
}

// fmp4Sample is one demuxed packet with its declared duration.
type fmp4Sample struct {
	data []byte
	dur  uint32
}

// parseFMP4 is a minimal fragmented-MP4 reader for the tests: it extracts
// the ALAC cookie from moov/stsd and the samples from each moof+mdat pair,
// validating that every trun data offset lands on its mdat payload. It is
// deliberately independent of the production demuxer (which reads only
// non-fragmented movies), so it is an honest second implementation.
func parseFMP4(t *testing.T, data []byte) (cookie []byte, samples []fmp4Sample) {
	t.Helper()
	src := container.BytesSource(data)
	size := int64(len(data))
	off := int64(0)
	var expectBase int64 // running decode time; each fragment's tfdt must equal it
	for off < size {
		b, err := readBox(src, off, size)
		if err != nil {
			t.Fatalf("readBox at %d: %v", off, err)
		}
		payload := data[b.payloadOff() : b.off+b.size]
		switch b.typ {
		case "moov":
			cookie = findCookie(t, payload)
		case "moof":
			frag := parseMoof(t, data, b.off, b.size, payload, expectBase)
			for _, s := range frag {
				expectBase += int64(s.dur)
			}
			samples = append(samples, frag...)
		}
		off = b.off + b.size
	}
	if cookie == nil {
		t.Fatal("no ALAC cookie in moov")
	}
	return cookie, samples
}

func findCookie(t *testing.T, moov []byte) []byte {
	var cookie []byte
	descend(moov, []string{"trak", "mdia", "minf", "stbl", "stsd"}, func(stsd []byte) {
		_, _, rest, ok := fullBox(stsd)
		if !ok || len(rest) < 4 {
			t.Fatal("stsd truncated")
		}
		_ = walkBoxes(rest[4:], func(typ string, entry []byte) error {
			if typ != "alac" || len(entry) < 28 {
				return nil
			}
			inner := findChild(entry[28:], "alac")
			if len(inner) >= 28 {
				cookie = inner[4:]
			}
			return nil
		})
	})
	return cookie
}

// descend walks a single-track box path and calls fn with the innermost
// box's payload.
func descend(body []byte, path []string, fn func([]byte)) {
	if len(path) == 0 {
		fn(body)
		return
	}
	_ = walkBoxes(body, func(typ string, payload []byte) error {
		if typ == path[0] {
			descend(payload, path[1:], fn)
		}
		return nil
	})
}

func parseMoof(t *testing.T, file []byte, moofOff, moofSize int64, moof []byte, expectBase int64) []fmp4Sample {
	// tfdt must anchor the fragment at the cumulative decode time; a wrong
	// baseTime accumulation would break seeking/sync in a real player while
	// leaving the concatenated samples (which ignore tfdt) intact.
	descend(moof, []string{"traf", "tfdt"}, func(tfdt []byte) {
		version, _, rest, ok := fullBox(tfdt)
		if !ok {
			t.Fatal("tfdt truncated")
		}
		var base int64
		if version == 1 {
			base = int64(be64(rest))
		} else {
			base = int64(be32(rest))
		}
		if base != expectBase {
			t.Fatalf("fragment tfdt baseMediaDecodeTime = %d, want %d", base, expectBase)
		}
	})

	var durs, sizes []uint32
	var dataOffset uint32
	descend(moof, []string{"traf", "trun"}, func(trun []byte) {
		version, flags, rest, ok := fullBox(trun)
		if !ok {
			t.Fatal("trun truncated")
		}
		_ = version
		count := be32(rest)
		rest = rest[4:]
		if flags&0x000001 != 0 {
			dataOffset = be32(rest)
			rest = rest[4:]
		}
		for i := uint32(0); i < count; i++ {
			var dur, sz uint32
			if flags&0x000100 != 0 {
				dur = be32(rest)
				rest = rest[4:]
			}
			if flags&0x000200 != 0 {
				sz = be32(rest)
				rest = rest[4:]
			}
			durs = append(durs, dur)
			sizes = append(sizes, sz)
		}
	})
	// data_offset is relative to the moof start (default-base-is-moof); it
	// must land on the following mdat's payload.
	base := moofOff + int64(dataOffset)
	mdatHdr := moofOff + moofSize
	if base != mdatHdr+8 {
		t.Fatalf("trun data_offset points at %d, mdat payload starts at %d", base, mdatHdr+8)
	}
	if string(file[mdatHdr+4:mdatHdr+8]) != "mdat" {
		t.Fatalf("box after moof is %q, want mdat", file[mdatHdr+4:mdatHdr+8])
	}
	out := make([]fmp4Sample, len(sizes))
	p := base
	for i, sz := range sizes {
		out[i] = fmp4Sample{data: file[p : p+int64(sz)], dur: durs[i]}
		p += int64(sz)
	}
	return out
}

// decodeFMP4 muxes and re-decodes src through parseFMP4, returning the
// reconstructed buffer.
func decodeFMP4(t *testing.T, src *audio.Buffer, fragSamples int) *audio.Buffer {
	t.Helper()
	data := muxALAC(t, src, fragSamples)
	cookie, samples := parseFMP4(t, data)
	cfg, err := alac.ParseMagicCookie(cookie)
	if err != nil {
		t.Fatalf("cookie: %v", err)
	}
	dec, err := alac.NewDecoder(cfg, src.Fmt)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Release()

	got := audio.Get(src.Fmt, src.N)
	pos := 0
	for _, s := range samples {
		err := dec.Decode(s.data, func(b *audio.Buffer) error {
			if uint32(b.N) != s.dur {
				t.Fatalf("sample dur %d, decoded %d", s.dur, b.N)
			}
			for c := 0; c < src.Fmt.Channels; c++ {
				copy(got.I[c*got.Stride+pos:c*got.Stride+pos+b.N], b.ChanI(c))
			}
			pos += b.N
			return nil
		})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	if pos != src.N {
		t.Fatalf("reconstructed %d samples, want %d", pos, src.N)
	}
	return got
}

// TestMuxRoundTrip proves the muxer writes valid fragments whose samples
// reconstruct the source, across depths, channels, lengths, and fragment
// sizes, without ffmpeg.
func TestMuxRoundTrip(t *testing.T) {
	for _, depth := range []int{16, 20, 24, 32} {
		for _, ch := range []int{1, 2} {
			for _, n := range []int{alac.FrameSize*3 + 100, 500, alac.FrameSize} {
				for _, frag := range []int{0, alac.FrameSize, 44100 * 2} {
					f := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: depth}
					src := audio.Get(f, n)
					src.N = n
					fillTone(src)
					got := decodeFMP4(t, src, frag)
					for c := 0; c < ch; c++ {
						w := src.I[c*src.Stride : c*src.Stride+n]
						g := got.I[c*got.Stride : c*got.Stride+n]
						for i := range w {
							if w[i] != g[i] {
								t.Fatalf("depth=%d ch=%d n=%d frag=%d: ch%d[%d]=%d want %d", depth, ch, n, frag, c, i, g[i], w[i])
							}
						}
					}
					audio.Put(src)
					audio.Put(got)
				}
			}
		}
	}
}

// TestMuxRejects covers the muxer's guard rails.
func TestMuxRejects(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	enc, _ := alac.NewEncoder(f, nil)
	cookie := enc.CodecConfig()

	// Non-ALAC codec.
	m := NewMuxer(&bytes.Buffer{}, nil)
	if err := m.Begin([]container.Track{{Codec: codec.FLAC, CodecConfig: cookie, Fmt: f}}); err == nil {
		t.Error("Begin accepted a non-ALAC track")
	}
	// Gapless trims (ALAC has none).
	m = NewMuxer(&bytes.Buffer{}, nil)
	if err := m.Begin([]container.Track{{Codec: codec.ALAC, CodecConfig: cookie, Fmt: f, Delay: 10}}); err == nil {
		t.Error("Begin accepted a nonzero Delay")
	}
	// Two tracks.
	m = NewMuxer(&bytes.Buffer{}, nil)
	if err := m.Begin([]container.Track{{Codec: codec.ALAC, CodecConfig: cookie, Fmt: f}, {}}); err == nil {
		t.Error("Begin accepted two tracks")
	}
}

// TestMuxLayoutTolerated confirms the muxer accepts a track whose channel
// layout differs from the ALAC cookie's default (a non-canonical WAV
// EXTENSIBLE mask, or an unknown layout), since the cookie has no layout
// field and ALAC codes channels in bitstream order. The samples still
// reconstruct when decoded through the cookie's (default-layout) format.
func TestMuxLayoutTolerated(t *testing.T) {
	for _, layout := range []audio.ChannelMask{0, audio.FrontLeft | audio.BackRight} {
		f := audio.Format{Rate: 44100, Channels: 2, Layout: layout, Type: audio.Int, BitDepth: 16}
		src := audio.Get(f, alac.FrameSize+321)
		src.N = alac.FrameSize + 321
		fillTone(src)
		data := muxALAC(t, src, 0) // muxALAC fails the test if Begin rejects the layout

		cookie, samples := parseFMP4(t, data)
		cfg, err := alac.ParseMagicCookie(cookie)
		if err != nil {
			t.Fatalf("cookie: %v", err)
		}
		dec, err := alac.NewDecoder(cfg, cfg.Format()) // decode through the cookie's default layout
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		got := audio.Get(cfg.Format(), src.N)
		pos := 0
		for _, s := range samples {
			if err := dec.Decode(s.data, func(b *audio.Buffer) error {
				for c := 0; c < 2; c++ {
					copy(got.I[c*got.Stride+pos:c*got.Stride+pos+b.N], b.ChanI(c))
				}
				pos += b.N
				return nil
			}); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		for c := 0; c < 2; c++ { // channel order is layout-agnostic
			w := src.I[c*src.Stride : c*src.Stride+src.N]
			g := got.I[c*got.Stride : c*got.Stride+src.N]
			for i := range w {
				if w[i] != g[i] {
					t.Fatalf("layout %v ch%d[%d]=%d want %d", layout, c, i, g[i], w[i])
				}
			}
		}
		dec.Release()
		audio.Put(src)
		audio.Put(got)
	}
}

// TestMuxHighRateClampsSampleEntry pins the 16.16 samplerate clamp: a 96 kHz
// stream round-trips through the (authoritative) cookie, and the legacy
// AudioSampleEntry samplerate field saturates at 65535 rather than wrapping
// its integer part into garbage.
func TestMuxHighRateClampsSampleEntry(t *testing.T) {
	f := audio.Format{Rate: 96000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}
	src := audio.Get(f, alac.FrameSize+100)
	defer audio.Put(src)
	src.N = alac.FrameSize + 100
	fillTone(src)
	got := decodeFMP4(t, src, 0) // cookie carries 96000, decode is exact
	defer audio.Put(got)
	for c := 0; c < 2; c++ {
		w := src.I[c*src.Stride : c*src.Stride+src.N]
		g := got.I[c*got.Stride : c*got.Stride+src.N]
		for i := range w {
			if w[i] != g[i] {
				t.Fatalf("96k ch%d[%d]=%d want %d", c, i, g[i], w[i])
			}
		}
	}

	// The sample-entry samplerate integer part must be the clamp (65535), not
	// the wrapped low bits of 96000<<16.
	data := muxALAC(t, src, 0)
	var entry []byte
	descend(findChild(data, "moov"), []string{"trak", "mdia", "minf", "stbl", "stsd"}, func(stsd []byte) {
		entry = findChild(stsd[8:], "alac")
	})
	if len(entry) < 28 {
		t.Fatal("no alac sample entry")
	}
	if rate := be32(entry[24:28]) >> 16; rate != 0xFFFF {
		t.Errorf("sample-entry samplerate integer part = %d, want 65535 (clamped)", rate)
	}
}

// fillTone writes a smooth signal (compressible) with a deterministic ramp.
func fillTone(b *audio.Buffer) {
	amp := int32(1)<<(b.Fmt.BitDepth-2) - 1
	for c := 0; c < b.Fmt.Channels; c++ {
		s := b.ChanI(c)
		acc := int32(c * 137)
		for i := range s {
			acc += 200 + int32(c*13)
			v := acc % (2 * amp)
			if v > amp {
				v = 2*amp - v
			}
			s[i] = v - amp/2
		}
	}
}
