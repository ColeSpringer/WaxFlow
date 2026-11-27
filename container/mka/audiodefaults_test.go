package mka

import (
	"errors"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// audioChild returns where the first track's Audio child id lies in file.
func audioChild(t *testing.T, file []byte, id uint32) (start, end int) {
	t.Helper()
	_, segData, _ := segment(t, file)
	_, pos, ok := segChild(t, file, idTracks)
	if !ok {
		t.Fatal("no Tracks")
	}
	find := func(from, to int, want uint32) (int, int, int) {
		for off := from; off < to; {
			got, body, end := readElemAt(t, file, off)
			if got == want {
				return off, body, end
			}
			off = end
		}
		t.Fatalf("no element %#x in [%d, %d)", want, from, to)
		return 0, 0, 0
	}
	_, tracks, tracksEnd := readElemAt(t, file, segData+pos)
	_, entry, entryEnd := find(tracks, tracksEnd, idTrackEntry)
	_, a, aEnd := find(entry, entryEnd, idAudio)
	start, _, end = find(a, aEnd, id)
	return start, end
}

// voided is file with the Audio child id overwritten by a Void of its length,
// which is how a muxer that leaves the element out reads.
func voided(t *testing.T, file []byte, ids ...uint32) []byte {
	t.Helper()
	out := append([]byte(nil), file...)
	for _, id := range ids {
		start, end := audioChild(t, out, id)
		n := end - start
		if n < 2 || n-2 > 126 {
			t.Fatalf("element %#x is %d bytes; a one-byte-size Void cannot stand in", id, n)
		}
		v := append([]byte{byte(idVoid), 0x80 | byte(n-2)}, make([]byte, n-2)...)
		copy(out[start:], v)
	}
	return out
}

// restated is file with the Audio child id given the body value instead and
// a Void filling what is left. An empty body's size is written in two bytes,
// so the element keeps a Channels element's three.
func restated(t *testing.T, file []byte, id uint32, value []byte) []byte {
	t.Helper()
	out := append([]byte(nil), file...)
	start, end := audioChild(t, out, id)
	el := append(appendID(nil, id), 0x80|byte(len(value)))
	if len(value) == 0 {
		el = append(appendID(nil, id), 0x40, 0)
	}
	el = append(el, value...)
	left := end - start - len(el)
	if left != 0 && (left < 2 || left-2 > 126) {
		t.Fatalf("restating %#x leaves %d bytes, which no one-byte-size Void fills", id, left)
	}
	if left > 0 {
		el = append(el, byte(idVoid), 0x80|byte(left-2))
		el = append(el, make([]byte, left-2)...)
	}
	copy(out[start:], el)
	return out
}

// pcmMKA is a short 44.1 kHz stereo 16-bit PCM file.
func pcmMKA(t *testing.T) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	pkts := []codec.Packet{{Data: make([]byte, 4410*4), PTS: 0, Dur: 4410}}
	return muxToSeekable(t, container.Track{Codec: codec.PCM, Fmt: f, Samples: 4410, Default: true}, nil, pkts, codec.Trailer{Samples: 4410})
}

// TestAudioDefaults: a PCM track that leaves Channels or SamplingFrequency
// out has the format's defaults, one channel and 8000 Hz, as does one that
// writes either with no octets. One that states 0 is refused. mkvmerge leaves
// Channels out of every mono file.
func TestAudioDefaults(t *testing.T) {
	file := pcmMKA(t)
	for _, tc := range []struct {
		name           string
		file           []byte
		rate, channels int
	}{
		{"as written", file, 44100, 2},
		{"no Channels", voided(t, file, idChannels), 44100, 1},
		{"no SamplingFrequency", voided(t, file, idSamplingFreq), 8000, 2},
		{"neither", voided(t, file, idChannels, idSamplingFreq), 8000, 1},
		{"empty Channels", restated(t, file, idChannels, nil), 44100, 1},
		{"empty SamplingFrequency", restated(t, file, idSamplingFreq, nil), 8000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(tc.file), &container.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			if f := d.Tracks()[0].Fmt; f.Rate != tc.rate || f.Channels != tc.channels {
				t.Errorf("track reads %d Hz, %d channels; want %d, %d", f.Rate, f.Channels, tc.rate, tc.channels)
			}
		})
	}
	for _, tc := range []struct {
		name string
		file []byte
	}{
		{"Channels 0", restated(t, file, idChannels, []byte{0})},
		{"SamplingFrequency 0", restated(t, file, idSamplingFreq, make([]byte, 8))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDemuxer(container.BytesSource(tc.file), nil); !errors.Is(err, waxerr.ErrMalformedInput) && !errors.Is(err, waxerr.ErrUnsupportedFormat) {
				t.Errorf("a track stating %s opened with %v, want a refusal", tc.name, err)
			}
		})
	}
}

// TestAudioDefaultsYieldToCodecPrivate: a codec whose private data states the
// geometry keeps it when the Audio element is left out, so the defaults reach
// only a codec with nothing better to go on.
func TestAudioDefaultsYieldToCodecPrivate(t *testing.T) {
	heaac := func() []byte {
		f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
		track := container.Track{Codec: codec.HEAAC, CodecConfig: []byte{0x2B, 0x11, 0x88}, Fmt: f, Samples: 4096, Default: true}
		pkts := []codec.Packet{{Data: []byte{1, 2, 3}, Dur: 2048, Sync: true}, {Data: []byte{4, 5, 6}, PTS: 2048, Dur: 2048, Sync: true}}
		return voided(t, muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: 4096}), idChannels, idSamplingFreq, idOutputFreq)
	}
	for _, tc := range []struct {
		name           string
		file           []byte
		rate, channels int
	}{
		{"FLAC", voided(t, muxFLACFrom(t, 0), idChannels, idSamplingFreq), 44100, 2},
		{"Vorbis", voided(t, vorbisMKA(t, nil), idChannels, idSamplingFreq), 44100, 1},
		{"Opus", voided(t, fixture(t, "seed-opus.webm"), idChannels, idSamplingFreq), 48000, 2},
		{"HE-AAC", heaac(), 48000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(tc.file), nil)
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			if f := d.Tracks()[0].Fmt; f.Rate != tc.rate || f.Channels != tc.channels {
				t.Errorf("%s with no Channels or SamplingFrequency reads %d Hz, %d channels; want %d, %d",
					tc.name, f.Rate, f.Channels, tc.rate, tc.channels)
			}
		})
	}
}

// TestAudioDefaultsFillAnInBandPCE: an AAC config of channel configuration 0
// carries its layout in the stream, so the Audio element supplies the count,
// up to stereo. Left out, it is the format's default, one channel, as for the
// mono file mkvmerge writes; past stereo it is refused by name.
func TestAudioDefaultsFillAnInBandPCE(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	asc := []byte{0x12, 0x00} // AAC-LC, 44.1 kHz, channel configuration 0
	pkts := []codec.Packet{{Data: []byte{1, 2, 3}, Dur: 1024, Sync: true}}
	file := muxToSeekable(t, container.Track{Codec: codec.AACLC, CodecConfig: asc, Fmt: f, Samples: 1024, Default: true},
		nil, pkts, codec.Trailer{Samples: 1024})
	for _, tc := range []struct {
		name     string
		file     []byte
		channels int // 0 for a refusal
	}{
		{"stated 2", file, 2},
		{"left out", voided(t, file, idChannels), 1},
		{"stated 6", restated(t, file, idChannels, []byte{6}), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(tc.file), nil)
			if tc.channels == 0 {
				if err == nil {
					t.Errorf("opened with %d channels, want a refusal", d.Tracks()[0].Fmt.Channels)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Tracks()[0].Fmt.Channels; got != tc.channels {
				t.Errorf("track reads %d channels, want %d", got, tc.channels)
			}
		})
	}
}
