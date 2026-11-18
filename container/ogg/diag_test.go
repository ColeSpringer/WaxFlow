package ogg

import (
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestOggLookAheadIsNotStrict: the look ahead behind the first audio page
// runs with Strict off, so the damage it crosses is the read's to refuse. A
// strict open takes the file, noting the start it cannot confirm, and the
// read that reaches the damage refuses it.
func TestOggLookAheadIsNotStrict(t *testing.T) {
	opus, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	shifted := testutil.ShiftOggGranules(t, opus, 2, 480000)
	first := testutil.OggFirstAudioPage(t, shifted, 2)
	raw := testutil.DamageOggPage(t, shifted, first+1)
	d, err := NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict open refused damage only its look ahead met: %v", err)
	}
	if ws := d.Warnings(); len(ws) != 1 || ws[0].Kind != container.Note || !strings.Contains(ws[0].Msg, "damage after the first audio page") {
		t.Errorf("warnings %v, want one note that damage leaves the start unconfirmed", ws)
	}
	var pkt container.Packet
	for err == nil {
		err = d.ReadPacket(&pkt)
	}
	if waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Errorf("the strict read ended with %v, want the damage refused", err)
	}

	// Past the damage, a packet longer than the look ahead's reach: a look
	// ahead that steps over the damage reaches its cap, and one that stopped
	// at it would say damage left the start unconfirmed.
	const serial = 4321
	pkt3 := []byte{31<<3 | 0x4, 0, 0}
	stream := buildPage(flagBOS, 0, serial, 0, muxOpusHead(312))
	stream = append(stream, buildPage(0, 0, serial, 1, append([]byte("OpusTags"), make([]byte, 8)...))...)
	stream = append(stream, buildPage(0, 480000+4*960, serial, 2, pkt3, pkt3, pkt3, pkt3)...)
	damaged := buildPage(0, 480000+5*960, serial, 3, pkt3)
	damaged[len(damaged)-1] ^= 0xFF
	stream = append(stream, damaged...)
	long := make([]byte, 3*255*255)
	copy(long, pkt3)
	full := make([]byte, 255)
	for i := range full {
		full[i] = 255
	}
	for i := range 3 {
		flags := byte(0)
		if i > 0 {
			flags = flagContinued
		}
		stream = append(stream, buildPageRaw(flags, -1, serial, uint32(4+i), full, long[i*255*255:(i+1)*255*255])...)
	}
	stream = append(stream, buildPageRaw(flagContinued|flagEOS, 480000+6*960, serial, 7, []byte{0}, nil)...)
	d, err = NewDemuxer(container.BytesSource(stream), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict open refused damage only its look ahead met: %v", err)
	}
	if ws := d.Warnings(); len(ws) != 1 || !strings.Contains(ws[0].Msg, "no later page within reach") {
		t.Errorf("warnings %v, want one note that no page within reach backs the start", ws)
	}
}
