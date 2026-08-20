package mp4

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// seekBuf aliases the shared in-memory io.WriteSeeker.
type seekBuf = testutil.MemWriteSeeker

// muxProgressive runs a track and packets through the progressive muxer.
func muxProgressive(t *testing.T, track container.Track, pkts []codec.Packet, trailer codec.Trailer) []byte {
	t.Helper()
	sb := &seekBuf{}
	muxProgressiveTo(t, sb, track, pkts, trailer)
	return sb.Buf
}

// muxProgressiveTo is muxProgressive on a caller's destination.
func muxProgressiveTo(t *testing.T, w io.Writer, track container.Track, pkts []codec.Packet, trailer codec.Trailer) {
	t.Helper()
	m := NewProgressiveMuxer(w, nil)
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

// TestProgressiveMuxAtANonZeroStart writes into a destination the caller had
// already written to. The mdat largesize is patched at the offset the header
// left it, which is a file offset only when the movie starts the file.
func TestProgressiveMuxAtANonZeroStart(t *testing.T) {
	const preSkip, padding = 312, 100
	track, pkts := opusTrackFor(preSkip, 8*960-preSkip-padding, 8)
	trailer := codec.Trailer{Samples: 8*960 - preSkip - padding, Delay: preSkip, Padding: padding}
	raw := testutil.MuxAtOffset(t, 97, func(w io.Writer) {
		muxProgressiveTo(t, w, track, pkts, trailer)
	})
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if tr := d.Tracks()[0]; tr.Codec != codec.Opus || tr.Delay != preSkip {
		t.Errorf("read back as %+v", tr)
	}
}

// TestProgressiveMuxRefusesAPipe: seekability is probed, not inferred.
// *os.File has Seek for a pipe too, and the progressive layout puts the moov
// after the samples, so trusting the method set buffers a whole movie before
// the mdat patch fails.
func TestProgressiveMuxRefusesAPipe(t *testing.T) {
	track, _ := opusTrackFor(312, 8*960-312, 8)
	w := &testutil.PipeWriteSeeker{}
	m := NewProgressiveMuxer(w, nil)
	if err := m.Begin([]container.Track{track}); err == nil {
		t.Fatal("a writer whose Seek fails was accepted")
	}
	if len(w.Buf) != 0 {
		t.Errorf("%d bytes were written to a destination that cannot be finished", len(w.Buf))
	}
}
