package mp4

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// sbrASC is an AudioSpecificConfig with explicit hierarchical signalling:
// AOT 5 (SBR), sfIdx 6 (24000 core), chanCfg 2, extSfIdx 3 (48000 output),
// AOT 2 (AAC-LC base). This is how an M4A's esds carries HE-AAC.
var sbrASC = []byte{0x2B, 0x11, 0x88}

// psASC is the same config wrapped as AOT 29 (PS): only the leading object
// type differs, plus the canonical trailing GASpecificConfig byte. PS still
// decodes the base layer only, so this is the config that keeps warning.
var psASC = []byte{0xEB, 0x11, 0x88, 0x00}

// muxSBRSignalled writes a minimal MP4 whose track declares the given SBR
// config, at the rate and AU length ParseASC resolves for it (the muxer
// cross-checks the track format against the ASC). The packet payloads are
// not real AAC: nothing here decodes them, and the demuxer reads the sample
// table and the esds rather than the bitstream.
func muxSBRSignalled(t *testing.T, asc []byte, id codec.ID, rate, dur int) []byte {
	t.Helper()
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	track := container.Track{
		Codec: id, CodecConfig: asc, Fmt: f,
		Samples: int64(4 * dur), Default: true,
	}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := range 4 {
		pkt := codec.Packet{Data: bytes.Repeat([]byte{byte(i + 1)}, 64),
			PTS: int64(i * dur), Dur: int64(dur), Sync: true}
		if err := m.WritePacket(container.Packet{Track: 0, Packet: pkt}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(codec.Trailer{Samples: int64(4 * dur)}); err != nil {
		t.Fatalf("End: %v", err)
	}
	return out.Bytes()
}

// TestStrictAcceptsExplicitHEAAC pins the decode-side identity of an
// explicitly signalled HE-AAC v1 file: the track is codec.HEAAC at the
// 48000 extension rate, and no band-limit warning appears in either mode
// (the high band is synthesized now). The test predates the SBR decoder as
// a regression test for warn-vs-note escalation under Strict; what it holds
// today is that a conformant HE-AAC file probes clean.
func TestStrictAcceptsExplicitHEAAC(t *testing.T) {
	data := muxSBRSignalled(t, sbrASC, codec.HEAAC, 48000, 2048)

	for _, strict := range []bool{false, true} {
		name := "tolerant"
		if strict {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				t.Fatalf("a valid HE-AAC file was rejected (strict=%v): %v", strict, err)
			}
			tracks := d.Tracks()
			if len(tracks) != 1 {
				t.Fatalf("tracks = %d, want 1", len(tracks))
			}
			if got := tracks[0].Codec; got != codec.HEAAC {
				t.Errorf("codec = %q, want %q", got, codec.HEAAC)
			}
			if got := tracks[0].Fmt.Rate; got != 48000 {
				t.Errorf("rate = %d, want the 48000 extension rate", got)
			}
			for _, w := range d.Warnings() {
				if strings.Contains(w.Msg, "high band") {
					t.Errorf("HE-AAC v1 decode still warns (strict=%v): %q", strict, w.Msg)
				}
			}
		})
	}
}

// TestStrictKeepsPSWarning is the note-vs-warn coverage the v1 decode did
// not retire: explicit PS (AOT 29) still decodes the base layer only, so
// its band-limit note must be recorded in both modes, name both rates, and
// never escalate to an error under Strict. This is the original regression
// this file was born for (warn() under Strict rejected a conformant file),
// now pinned on the config that still warns.
func TestStrictKeepsPSWarning(t *testing.T) {
	data := muxSBRSignalled(t, psASC, codec.AACLC, 24000, 1024)

	for _, strict := range []bool{false, true} {
		name := "tolerant"
		if strict {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				t.Fatalf("a conformant PS file was rejected (strict=%v): %v", strict, err)
			}
			tracks := d.Tracks()
			if len(tracks) != 1 {
				t.Fatalf("tracks = %d, want 1", len(tracks))
			}
			if got := tracks[0].Codec; got != codec.AACLC {
				t.Errorf("codec = %q, want %q (PS decodes its base layer)", got, codec.AACLC)
			}
			if got := tracks[0].Fmt.Rate; got != 24000 {
				t.Errorf("rate = %d, want the 24000 base rate", got)
			}
			var msgs []string
			for _, w := range d.Warnings() {
				msgs = append(msgs, w.Msg)
			}
			joined := strings.Join(msgs, " | ")
			if !strings.Contains(joined, "high band") {
				t.Errorf("no band-limit warning recorded (strict=%v); warnings: %q", strict, joined)
			}
			// It should name both rates: what the file would play at with a
			// full HE-AAC v2 decoder, and what we actually decode.
			if !strings.Contains(joined, "48000") || !strings.Contains(joined, "24000") {
				t.Errorf("warning should name the 48000 extension rate and the 24000 base rate; got %q", joined)
			}
		})
	}
}

// TestNonSelectedTrackDoesNotWarn pins the note to the track actually chosen.
// setMP4A runs for every mp4a sample entry, so emitting there would warn about
// an alternate audio track nobody decodes.
func TestNonSelectedTrackDoesNotWarn(t *testing.T) {
	// A plain AAC-LC file: no SBR anywhere, so no note may appear.
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	// 00010 0100 0010 0: AOT 2, sfIdx 4 (44100), chanCfg 2.
	lcASC := []byte{0x12, 0x10}
	track := container.Track{Codec: codec.AACLC, CodecConfig: lcASC, Fmt: f,
		Samples: 2048, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	pkt := codec.Packet{Data: bytes.Repeat([]byte{1}, 64), PTS: 0, Dur: 1024, Sync: true}
	if err := m.WritePacket(container.Packet{Track: 0, Packet: pkt}); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := m.End(codec.Trailer{Samples: 2048}); err != nil {
		t.Fatalf("End: %v", err)
	}

	d, err := NewDemuxer(container.BytesSource(out.Bytes()), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, "high band") {
			t.Errorf("plain AAC-LC produced a band-limit warning: %q", w.Msg)
		}
	}
}
