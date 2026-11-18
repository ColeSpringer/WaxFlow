package blocks

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestReaderLandsInsideTheTrack: a track that stops inside its last unit
// seeks to that unit at most, and reads deliver whole units from there.
func TestReaderLandsInsideTheTrack(t *testing.T) {
	data := []byte("hdr0123456789ABCDE")
	r := NewReader(container.BytesSource(data), 3, 3, 2, 5, 9, "x: reading data")
	var pkt container.Packet
	if err := r.ReadPacket(&pkt); err != nil || pkt.PTS != 0 || pkt.Dur != 10 || string(pkt.Data) != "0123456789ABCDE" {
		t.Fatalf("first read: %v, pts %d dur %d data %q", err, pkt.PTS, pkt.Dur, pkt.Data)
	}
	for _, target := range []int64{9, 100} {
		if landed, err := r.SeekSample(context.Background(), target); err != nil || landed != 8 {
			t.Errorf("SeekSample(%d) = %d, %v; want the last unit, at 8", target, landed, err)
		}
	}
	if err := r.ReadPacket(&pkt); err != nil || pkt.PTS != 8 || pkt.Dur != 2 || string(pkt.Data) != "CDE" {
		t.Errorf("read after the seek: %v, pts %d dur %d data %q", err, pkt.PTS, pkt.Dur, pkt.Data)
	}
	if err := r.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
		t.Errorf("read past the end: %v, want io.EOF", err)
	}
}

// TestAShortPayloadReadCarriesTheOwnersText: a read the source cannot serve
// fails under the text the owner gave.
func TestAShortPayloadReadCarriesTheOwnersText(t *testing.T) {
	r := NewReader(container.BytesSource(make([]byte, 10)), 4, 4, 1, 2, 2, "x: reading data")
	var pkt container.Packet
	if err := r.ReadPacket(&pkt); err == nil || !strings.Contains(err.Error(), "x: reading data") {
		t.Errorf("ReadPacket = %v, want a failure under the owner's text", err)
	}
}
