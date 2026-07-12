package mp4

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// seekBuf is a minimal in-memory io.WriteSeeker for the progressive muxer,
// which back-patches the mdat size.
type seekBuf struct {
	b   []byte
	pos int64
}

func (s *seekBuf) Write(p []byte) (int, error) {
	end := s.pos + int64(len(p))
	if end > int64(len(s.b)) {
		s.b = append(s.b, make([]byte, end-int64(len(s.b)))...)
	}
	copy(s.b[s.pos:], p)
	s.pos = end
	return len(p), nil
}

func (s *seekBuf) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = off
	case io.SeekCurrent:
		s.pos += off
	case io.SeekEnd:
		s.pos = int64(len(s.b)) + off
	}
	return s.pos, nil
}

// muxProgressive runs a track and packets through the progressive muxer.
func muxProgressive(t *testing.T, track container.Track, pkts []codec.Packet, trailer codec.Trailer) []byte {
	t.Helper()
	sb := &seekBuf{}
	m := NewProgressiveMuxer(sb, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, p := range pkts {
		if err := m.WritePacket(container.Packet{Track: 0, Packet: p}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(trailer); err != nil {
		t.Fatalf("End: %v", err)
	}
	return sb.b
}

// TestProgressiveRoundTrip pins the progressive muxer against the progressive
// demuxer: a flat moov/stbl movie the demuxer reads back with the same packets,
// format, and (for Opus) gapless delay.
func TestProgressiveRoundTrip(t *testing.T) {
	t.Run("opus gapless", func(t *testing.T) {
		const preSkip, padding = 312, 100
		track, pkts := opusTrackFor(preSkip, 8*960-preSkip-padding, 8)
		trailer := codec.Trailer{Samples: 8*960 - preSkip - padding, Delay: preSkip, Padding: padding}
		file := muxProgressive(t, track, pkts, trailer)

		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		if d.fragmented {
			t.Error("progressive output parsed as fragmented")
		}
		tr := d.Tracks()[0]
		if tr.Codec != codec.Opus || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
			t.Errorf("track = %+v", tr)
		}
		if tr.Delay != preSkip {
			t.Errorf("Delay = %d, want %d", tr.Delay, preSkip)
		}
		got, pts := readFrag(t, d)
		if len(got) != len(pkts) {
			t.Fatalf("read %d packets, wrote %d", len(got), len(pkts))
		}
		var wantPTS int64
		for i := range pkts {
			if !bytes.Equal(got[i], pkts[i].Data) {
				t.Errorf("packet %d payload mismatch", i)
			}
			if pts[i] != wantPTS {
				t.Errorf("packet %d PTS = %d, want %d", i, pts[i], wantPTS)
			}
			wantPTS += pkts[i].Dur
		}
	})

	t.Run("flac lossless", func(t *testing.T) {
		track, pkts := flacTrackFor(t, 5)
		file := muxProgressive(t, track, pkts, codec.Trailer{Samples: 5 * 4096})
		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		tr := d.Tracks()[0]
		if tr.Codec != codec.FLAC || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 {
			t.Errorf("track = %+v", tr)
		}
		got, _ := readFrag(t, d)
		if len(got) != len(pkts) {
			t.Fatalf("read %d packets, wrote %d", len(got), len(pkts))
		}
		for i := range pkts {
			if !bytes.Equal(got[i], pkts[i].Data) {
				t.Errorf("packet %d payload mismatch", i)
			}
		}
	})
}

// TestProgressiveNeedsSeek pins the seek requirement: a non-seekable writer is
// refused at Begin (the engine checks NeedsSeek up front, but the muxer guards
// too).
func TestProgressiveNeedsSeek(t *testing.T) {
	m := NewProgressiveMuxer(io.Discard, nil)
	if !m.NeedsSeek() {
		t.Error("NeedsSeek = false, want true")
	}
	track, _ := opusTrackFor(312, 960, 1)
	if err := m.Begin([]container.Track{track}); err == nil {
		t.Error("Begin on a non-seekable writer accepted; want rejection")
	}
}
