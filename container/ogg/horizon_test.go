package ogg

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAReadPastTheResyncHorizonEndsTheStream: damage a resync cannot see
// past ends a tolerant read there, with the damage and the bytes dropped
// listed, as every other demuxer ends one. Strict refuses it, and an open
// that finds no page in reach still refuses the file.
func TestAReadPastTheResyncHorizonEndsTheStream(t *testing.T) {
	const bs = 4096
	for name, raw := range map[string][]byte{
		"flac":   oggFLACFrames(t, 12*bs, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
		"vorbis": fixtureLocal(t, "golden-vorbis.ogg"),
		"opus":   fixtureLocal(t, "golden-opus.opus"),
	} {
		whole, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		all := len(placedBy(t, whole))
		// Zeros past the horizon in front of the stream's last page.
		at := bytes.LastIndex(raw, []byte("OggS"))
		damaged := slices.Concat(raw[:at], make([]byte, resyncHorizon+2), raw[at:])
		d, err := NewDemuxer(container.BytesSource(damaged), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var pkt container.Packet
		n := 0
		for err = d.ReadPacket(&pkt); err == nil; err = d.ReadPacket(&pkt) {
			n++
		}
		if err != io.EOF || n == 0 || n >= all {
			t.Errorf("%s: the read delivered %d of %d packets and ended %v, want the packets in front of the damage and the end", name, n, all, err)
		}
		if msgs := damageList(d); !strings.Contains(msgs, "no page within 1048576 bytes behind the damage") {
			t.Errorf("%s: findings %q, want the bytes dropped named", name, msgs)
		}
		s, err := NewDemuxer(container.BytesSource(damaged), &container.DemuxerOptions{Strict: true})
		for err == nil {
			err = s.ReadPacket(&pkt)
		}
		if waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
			t.Errorf("%s: Strict ended %v, want the damage refused", name, err)
		}
	}
	// A seek whose probe finds no page in reach looks in front of the
	// damage, where the read ends; Strict refuses the seek as it did, a
	// target in front of the damage too.
	raw := fixtureLocal(t, "golden-opus.opus")
	at := bytes.LastIndex(raw, []byte("OggS"))
	damaged := slices.Concat(raw[:at], make([]byte, 3<<20), raw[at:])
	for _, strict := range []bool{false, true} {
		d, err := NewDemuxer(container.BytesSource(damaged), &container.DemuxerOptions{Strict: strict})
		if err != nil {
			t.Fatal(err)
		}
		target := d.Tracks()[0].Samples - 1
		if strict {
			target = 1000
		}
		landed, err := d.SeekSample(t.Context(), 0, target)
		switch {
		case strict && waxerr.CodeOf(err) != waxerr.CodeMalformedInput:
			t.Errorf("Strict: a seek across the damage landed %d, %v; want it refused", landed, err)
		case !strict && err != nil:
			t.Errorf("a seek across the damage landed %d, %v; want a landing in front of it", landed, err)
		case !strict:
			var pkt container.Packet
			n := 0
			for err = d.ReadPacket(&pkt); err == nil; err = d.ReadPacket(&pkt) {
				n++
			}
			if err != io.EOF || n == 0 {
				t.Errorf("behind the seek the read delivered %d packets and ended %v, want those in front of the damage", n, err)
			}
		}
	}
	// With the zeros in front of the first audio page the open refuses, as
	// it did: there is no stream to end.
	raw = oggFLACFrames(t, 2*bs, 0, 1)
	at = 0
	for range 2 {
		at += bytes.Index(raw[at+1:], []byte("OggS")) + 1
	}
	if _, err := NewDemuxer(container.BytesSource(slices.Concat(raw[:at], make([]byte, resyncHorizon+2), raw[at:])), nil); waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Errorf("zeros past the horizon in front of the audio: the open returned %v, want the file refused", err)
	}
}

// damageList is the damage d lists, joined.
func damageList(d *Demuxer) string {
	var out []string
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			out = append(out, w.Msg)
		}
	}
	return strings.Join(out, "; ")
}
