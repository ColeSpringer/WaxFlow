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

// muxHEAAC writes a minimal fragmented MP4 whose track declares the SBR config
// above. The packet payloads are not real AAC: nothing here decodes them, and
// the demuxer reads the sample table and the esds rather than the bitstream.
func muxHEAAC(t *testing.T) []byte {
	t.Helper()
	// The core rate the base layer codes at, which is what ParseASC reports and
	// what the muxer cross-checks the track format against.
	f := audio.Format{Rate: 24000, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	track := container.Track{
		Codec: codec.AACLC, CodecConfig: sbrASC, Fmt: f,
		Samples: 4096, Default: true,
	}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := range 4 {
		pkt := codec.Packet{Data: bytes.Repeat([]byte{byte(i + 1)}, 64),
			PTS: int64(i * 1024), Dur: 1024, Sync: true}
		if err := m.WritePacket(container.Packet{Track: 0, Packet: pkt}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(codec.Trailer{Samples: 4096}); err != nil {
		t.Fatalf("End: %v", err)
	}
	return out.Bytes()
}

// TestStrictAcceptsExplicitHEAAC is a regression test for a real defect: the
// HE-AAC band-limit note was first routed through warn(), which escalates to a
// hard error under Strict. That made `probe --strict` reject a perfectly valid
// HE-AAC file as malformed.
//
// Strict exists to turn real-world mess into errors for conformance runs. An
// HE-AAC file is not mess: it is conformant, and the limitation is ours. So the
// note must be recorded without escalating.
func TestStrictAcceptsExplicitHEAAC(t *testing.T) {
	data := muxHEAAC(t)

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
			if got := tracks[0].Fmt.Rate; got != 24000 {
				t.Errorf("rate = %d, want the 24000 core rate", got)
			}
			// The note must survive in both modes: silently dropping it under
			// strict would be the opposite failure, since strict callers are
			// the ones most interested in it.
			var msgs []string
			for _, w := range d.Warnings() {
				msgs = append(msgs, w.Msg)
			}
			joined := strings.Join(msgs, " | ")
			if !strings.Contains(joined, "high band not synthesized") {
				t.Errorf("no band-limit warning recorded (strict=%v); warnings: %q", strict, joined)
			}
			// It should name both rates: what the file would play at with a
			// full HE-AAC decoder, and what we actually decode.
			if !strings.Contains(joined, "48000") || !strings.Contains(joined, "24000") {
				t.Errorf("warning should name the 48000 extension rate and the 24000 core rate; got %q", joined)
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
