package apen_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// The muxer's own work is the bookkeeping a frame cannot do for itself: the
// seek table that is the format's only index, the totals the descriptor states
// once the audio has gone out, and the MD5 over all of it. The engine-level
// gates in tests/ape_encode_test.go check that a real file comes out right;
// these check the edges that a healthy transcode never reaches.

// muxFormat is the shape the fixtures below are written in.
var muxFormat = audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
	Type: audio.Int, BitDepth: 16}

// muxFrames encodes n blocks of a ramp and returns the packets plus the track
// they belong to, with Samples as the caller's projection.
func muxFrames(t testing.TB, n int, projected int64) (container.Track, []container.Packet) {
	t.Helper()
	enc, err := ape.NewEncoder(muxFormat, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkts []container.Packet
	emit := func(p codec.Packet) error {
		pkts = append(pkts, container.Packet{Packet: codec.Packet{
			Data: append([]byte(nil), p.Data...), PTS: p.PTS, Dur: p.Dur, Sync: p.Sync,
		}})
		return nil
	}
	buf := audio.Get(muxFormat, ape.BlocksPerFrame)
	defer audio.Put(buf)
	for off := 0; off < n; off += ape.BlocksPerFrame {
		buf.N = min(ape.BlocksPerFrame, n-off)
		for c := range muxFormat.Channels {
			s := buf.ChanI(c)
			for i := range buf.N {
				s[i] = int32((off+i+c)%2000 - 1000)
			}
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.APE, CodecConfig: enc.CodecConfig(),
		Fmt: muxFormat, Samples: projected, Default: true}
	return track, pkts
}

// writeAll drives one whole mux into an in-memory seekable destination.
func writeAll(t testing.TB, track container.Track, pkts []container.Packet, opts *apen.MuxerOptions) []byte {
	t.Helper()
	dst := &testutil.MemWriteSeeker{}
	m := apen.NewMuxer(dst, opts)
	if !m.NeedsSeek() {
		t.Fatal("the ape muxer claims a plain writer will do")
	}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	samples := int64(0)
	for _, p := range pkts {
		if err := m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
		samples += p.Dur
	}
	if err := m.End(codec.Trailer{Samples: samples}); err != nil {
		t.Fatal(err)
	}
	return dst.Buf
}

// TestMuxSeekTableIndexesTheFrames pins the muxer's central job. The format has
// no other index: a frame states neither its length nor its position, so an
// entry that is off by a byte makes the frame it points at undecodable rather
// than approximate.
//
// It runs over several lengths for one reason: the coded run's remainder mod
// four decides how many bytes the region ends up holding, and a single fixture
// pins whichever remainder it happens to produce. The three below cover 0, 1
// and 2, and the check at the end refuses to pass on zero alone -- the first
// version of this test asserted a formula that was wrong for every non-zero
// remainder and was green because the one fixture it had landed on zero.
func TestMuxSeekTableIndexesTheFrames(t *testing.T) {
	remainders := map[int64]bool{}
	for _, n := range []int{2*ape.BlocksPerFrame + 4321, 5000, ape.BlocksPerFrame + 1234} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			track, pkts := muxFrames(t, n, int64(n))
			raw := writeAll(t, track, pkts, nil)

			h, err := ape.ParseHeader(raw)
			if err != nil {
				t.Fatal(err)
			}
			if h.TotalFrames != len(pkts) {
				t.Fatalf("header says %d frames, %d were written", h.TotalFrames, len(pkts))
			}
			if h.Samples() != int64(n) {
				t.Errorf("header states %d samples, %d were written", h.Samples(), n)
			}
			off := h.FrameDataOffset
			for i, p := range pkts {
				_, _, bytes, _, err := ape.ParseFrameHeader(p.Data)
				if err != nil {
					t.Fatal(err)
				}
				got := int64(binary.LittleEndian.Uint32(raw[h.SeekTableOffset+int64(i)*4:]))
				if got != off {
					t.Fatalf("frame %d is indexed at %d but was written at %d", i, got, off)
				}
				off += int64(bytes)
			}
			run := off - h.FrameDataOffset
			remainders[run%4] = true
			// The frames are laid down end to end, sharing the word at each
			// boundary, so the region is the run's WHOLE words plus the
			// trailing one that carries whatever is left over. Rounding the
			// run up and then adding a word would count four bytes nobody
			// wrote, and would agree only when the run is already word-sized.
			if want := run&^3 + 4; h.FrameDataBytes != want {
				t.Errorf("a %d-byte run: the descriptor counts %d bytes of frame data, %d were written",
					run, h.FrameDataBytes, want)
			}
			if int64(len(raw)) != h.FrameDataOffset+h.FrameDataBytes {
				t.Errorf("the file is %d bytes for a %d-byte frame region starting at %d",
					len(raw), h.FrameDataBytes, h.FrameDataOffset)
			}
		})
	}
	if len(remainders) < 2 || !remainders[0] {
		t.Errorf("the lengths above produced run remainders %v; the formula is only pinned "+
			"when at least one is non-zero", remainders)
	}
}

// TestMuxReservesForTheProjectedLength pins the size the table is reserved at.
// It sits ahead of the audio, so it cannot grow: the projection plus a frame
// of slack is what the engine's rounded estimate needs, and a stream that
// outgrows it is refused rather than silently truncated.
func TestMuxReservesForTheProjectedLength(t *testing.T) {
	const n = 2*ape.BlocksPerFrame + 4321
	track, pkts := muxFrames(t, n, n)
	raw := writeAll(t, track, pkts, nil)
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Three frames' worth of audio, projected: three entries plus one of
	// slack.
	if h.SeekTableEntries != 4 {
		t.Errorf("reserved %d seek entries for a 3-frame projection, want 4", h.SeekTableEntries)
	}

	// A projection short by more than the slack is a refusal at the packet
	// that overflows, not at End.
	short := track
	short.Samples = 1
	m := apen.NewMuxer(&testutil.MemWriteSeeker{}, nil)
	if err := m.Begin([]container.Track{short}); err != nil {
		t.Fatal(err)
	}
	var err2 error
	for _, p := range pkts {
		if err2 = m.WritePacket(p); err2 != nil {
			break
		}
	}
	if err2 == nil {
		t.Fatal("three frames fit a table reserved for one")
	}
	if code := waxerr.CodeOf(err2); code != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v", code, waxerr.CodeUnsupportedFormat)
	}
}

// TestMuxUnknownLengthReservesTheMaximum pins the answer to a source that will
// not say how long it is. The table cannot grow once the first frame is
// written, so the reservation covers the largest block count the header's own
// 32-bit field can state: the file carries a quarter of a megabyte of zeros
// and is a file, rather than an error.
func TestMuxUnknownLengthReservesTheMaximum(t *testing.T) {
	const n = 5000
	track, pkts := muxFrames(t, n, -1)
	raw := writeAll(t, track, pkts, nil)
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if int64(h.SeekTableEntries)*int64(h.BlocksPerFrame) < 1<<32-1 {
		t.Errorf("reserved %d seek entries of %d blocks for an unknown length; that covers %d blocks "+
			"and the header can count %d, so a stream could outgrow it",
			h.SeekTableEntries, h.BlocksPerFrame,
			int64(h.SeekTableEntries)*int64(h.BlocksPerFrame), int64(1)<<32-1)
	}
	if h.TotalFrames != 1 || h.Samples() != n {
		t.Errorf("header says %d frames / %d samples, want 1 / %d", h.TotalFrames, h.Samples(), n)
	}
	// The unused entries are zeros and the file still reads back.
	d, err := apen.NewDemuxer(container.BytesSource(raw), &apen.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("strict demux of an unknown-length file: %v", err)
	}
	if got := d.Tracks()[0].Samples; got != n {
		t.Errorf("demuxer reads %d samples, want %d", got, n)
	}
}

// TestMuxRefusals pins the shapes the muxer will not write. Each of them makes
// a file no reader could use, and the muxer is the last place that can say so.
func TestMuxRefusals(t *testing.T) {
	const n = ape.BlocksPerFrame + 100
	track, pkts := muxFrames(t, n, n)

	t.Run("a destination that cannot seek", func(t *testing.T) {
		m := apen.NewMuxer(io.Discard, nil)
		err := m.Begin([]container.Track{track})
		if err == nil {
			t.Fatal("Begin accepted a plain writer")
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeInvalidRequest {
			t.Errorf("code = %v, want %v", code, waxerr.CodeInvalidRequest)
		}
	})

	t.Run("a stream with no frames", func(t *testing.T) {
		m := apen.NewMuxer(&testutil.MemWriteSeeker{}, nil)
		if err := m.Begin([]container.Track{track}); err != nil {
			t.Fatal(err)
		}
		// A .ape states its length as (frames-1)*blocksPerFrame plus the last
		// frame's blocks, so a file with no frames can only claim a length it
		// does not have.
		if err := m.End(codec.Trailer{Samples: 0}); err == nil {
			t.Fatal("End wrote a file with no frames")
		}
	})

	t.Run("a short frame that is not the last", func(t *testing.T) {
		m := apen.NewMuxer(&testutil.MemWriteSeeker{}, nil)
		if err := m.Begin([]container.Track{track}); err != nil {
			t.Fatal(err)
		}
		// pkts[1] is the short final frame; writing it first and then a full
		// one is the shape every reader would misread, since the block count
		// of every frame but the last comes from the header.
		if err := m.WritePacket(pkts[1]); err != nil {
			t.Fatal(err)
		}
		if err := m.WritePacket(pkts[0]); err == nil {
			t.Fatal("a full frame was accepted behind a short one")
		}
	})

	t.Run("a frame that overruns its packet", func(t *testing.T) {
		m := apen.NewMuxer(&testutil.MemWriteSeeker{}, nil)
		if err := m.Begin([]container.Track{track}); err != nil {
			t.Fatal(err)
		}
		bad := container.Packet{Packet: codec.Packet{
			Data: append([]byte(nil), pkts[0].Data...), Dur: pkts[0].Dur,
		}}
		blocks, _, n, data, err := ape.ParseFrameHeader(bad.Data)
		if err != nil {
			t.Fatal(err)
		}
		// A frame claiming more bytes than its packet holds: the one shape
		// that cannot be laid down, since the bytes are not there.
		ape.PutFrameHeader(bad.Data, blocks, 0, len(data)+1)
		if err := m.WritePacket(bad); err == nil {
			t.Fatal("a frame longer than its packet was accepted")
		}
		_ = n
	})

	t.Run("a codec it does not hold", func(t *testing.T) {
		wrong := track
		wrong.Codec = codec.FLAC
		m := apen.NewMuxer(&testutil.MemWriteSeeker{}, nil)
		if err := m.Begin([]container.Track{wrong}); err == nil {
			t.Fatal("Begin accepted a FLAC track")
		}
	})

	t.Run("gapless trims it cannot signal", func(t *testing.T) {
		trimmed := track
		trimmed.Padding = 100
		m := apen.NewMuxer(&testutil.MemWriteSeeker{}, nil)
		if err := m.Begin([]container.Track{trimmed}); err == nil {
			t.Fatal("Begin accepted a track with trailing padding")
		}
	})
}

// TestMuxTagsRideBehindTheAudio pins the trailer seam: the tag block goes
// after the frame data and must leave the descriptor's byte count describing
// the audio alone, or a reader that trusts the count reads the tag as frames.
func TestMuxTagsRideBehindTheAudio(t *testing.T) {
	const n = 5000
	track, pkts := muxFrames(t, n, n)
	bare := writeAll(t, track, pkts, nil)
	tagged := writeAll(t, track, pkts, &apen.MuxerOptions{
		Tags: []container.Tag{{Key: "ARTIST", Value: "Wax Test"}, {Key: "TITLE", Value: "Stage Seven"}},
	})
	if len(tagged) <= len(bare) {
		t.Fatalf("the tagged file is %d bytes against the bare one's %d", len(tagged), len(bare))
	}
	hb, err := ape.ParseHeader(bare)
	if err != nil {
		t.Fatal(err)
	}
	ht, err := ape.ParseHeader(tagged)
	if err != nil {
		t.Fatal(err)
	}
	if hb.FrameDataBytes != ht.FrameDataBytes {
		t.Errorf("the tag changed the frame-data count from %d to %d", hb.FrameDataBytes, ht.FrameDataBytes)
	}
	// Strict mode is the check that matters: the demuxer cross-checks the
	// descriptor's count against the bytes it finds, and a tag it failed to
	// peel would show up there.
	d, err := apen.NewDemuxer(container.BytesSource(tagged), &apen.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("strict demux of a tagged file: %v", err)
	}
	if v := d.Tags()["ARTIST"]; len(v) != 1 || v[0] != "Wax Test" {
		t.Errorf("ARTIST read back as %v", v)
	}
}

// TestMuxPacksAFrameWhoseLastWordIsShort is the hostile-input edge of the word
// packer. A frame's logical byte sits up to three bytes further into the
// packet than its own index, so a frame whose last word is not all there
// reaches past the payload. The decoder's reader answers zero there; the
// packer has to answer the same, or a file that decodes would either be
// refused or read out of bounds.
func TestMuxPacksAFrameWhoseLastWordIsShort(t *testing.T) {
	const n = 5000
	track, pkts := muxFrames(t, n, n)
	_, _, coded, data, err := ape.ParseFrameHeader(pkts[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	// Trim the payload to the frame's own length, which leaves its final word
	// incomplete whenever the frame is not a whole number of words.
	if coded%4 == 0 {
		t.Skipf("this frame is %d bytes, a whole number of words: nothing to trim", coded)
	}
	short := container.Packet{Packet: codec.Packet{
		Data: append(append([]byte(nil), pkts[0].Data[:ape.FrameHeaderLen]...), data[:coded]...),
		Dur:  pkts[0].Dur,
	}}
	raw := writeAll(t, track, []container.Packet{short}, nil)

	// The result decodes, and to the same samples: the bytes the packer read
	// as zero are bytes the decoder would have read as zero too.
	whole := writeAll(t, track, pkts[:1], nil)
	if len(raw) != len(whole) {
		t.Fatalf("the trimmed packet wrote %d bytes against the whole one's %d", len(raw), len(whole))
	}
	d, err := apen.NewDemuxer(container.BytesSource(raw), &apen.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("strict demux: %v", err)
	}
	if got := d.Tracks()[0].Samples; got != int64(n) {
		t.Errorf("read back %d samples, want %d", got, n)
	}
}

// TestTruncationAtAFrameBoundaryIsRefused pins the shape a .ape cut exactly at
// a frame's first byte takes. Every earlier boundary refuses because the next
// table entry then runs past the audio; only the last frame can tie with the
// synthetic end entry, and a zero-length frame is impossible (a frame opens
// with a CRC word and closes with the coder's four-byte flush).
//
// Before this was refused the demuxer emitted a packet its own decoder could
// not parse, and the message read like an internal inconsistency rather than
// "this file is short". The muxer inherited it too, since a transmux parses
// the same header, so a truncated source died mid-write.
func TestTruncationAtAFrameBoundaryIsRefused(t *testing.T) {
	const n = 2*ape.BlocksPerFrame + 4321
	track, pkts := muxFrames(t, n, n)
	raw := writeAll(t, track, pkts, nil)
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range h.TotalFrames {
		at := int64(binary.LittleEndian.Uint32(raw[h.SeekTableOffset+int64(i)*4:]))
		d, err := apen.NewDemuxer(container.BytesSource(raw[:at]), nil)
		if err == nil {
			// It must not have opened; if it somehow did, every packet it
			// hands out still has to be one the decoder accepts.
			var pkt container.Packet
			for d.ReadPacket(&pkt) == nil {
				if _, _, _, _, perr := ape.ParseFrameHeader(pkt.Data); perr != nil {
					t.Fatalf("cut at frame %d's first byte: emitted a packet the decoder rejects: %v", i, perr)
				}
			}
			t.Fatalf("cut at frame %d's first byte was accepted", i)
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
			t.Errorf("cut at frame %d: code = %v, want %v", i, code, waxerr.CodeUnsupportedFormat)
		}
	}
	// One byte further in is a different file: the last frame is short rather
	// than absent, so it opens and dies at that frame's own CRC.
	last := int64(binary.LittleEndian.Uint32(raw[h.SeekTableOffset+int64(h.TotalFrames-1)*4:]))
	if _, err := apen.NewDemuxer(container.BytesSource(raw[:last+1]), nil); err != nil {
		t.Fatalf("a file with one byte of its last frame should still open: %v", err)
	}
}

// TestReserveTracksTheStreamsOwnFrameLength pins what finding the mismatch
// cost: the reservation has to be derived from the frame length the muxer was
// handed, not from this encoder's. On the transmux rung the length is the
// SOURCE file's, and the format lets it be anything from one block to
// 73728*16, so a constant computed from the encoder's own 73728 describes a
// table the stream may not have.
func TestReserveTracksTheStreamsOwnFrameLength(t *testing.T) {
	// The two deep levels multiply the frame length, and a remux of one of
	// their files carries that through; the shallow end is a stream that
	// packs far more frames into the same block count.
	for _, bpf := range []int{ape.BlocksPerFrame, ape.BlocksPerFrame * 16, 1024, 1} {
		got := apen.ReserveEntriesForTest(-1, bpf)
		if int64(got)*int64(bpf) < 1<<32-1 && got != apen.MaxSeekEntriesForTest {
			t.Errorf("a %d-block frame length reserved %d entries: that covers %d of the %d blocks "+
				"the header can count, and it is not the table cap either",
				bpf, got, int64(got)*int64(bpf), int64(1)<<32-1)
		}
		if got > apen.MaxSeekEntriesForTest {
			t.Errorf("a %d-block frame length reserved %d entries, past the %d-entry cap",
				bpf, got, apen.MaxSeekEntriesForTest)
		}
	}
	// A known length still reserves what it needs and no more, whatever the
	// frame length: the count plus one frame of slack.
	for _, tc := range []struct{ samples, bpf, want int }{
		{5000, ape.BlocksPerFrame, 2},
		{2 * ape.BlocksPerFrame, ape.BlocksPerFrame, 3},
		{5000, 1024, 6},
	} {
		if got := apen.ReserveEntriesForTest(int64(tc.samples), tc.bpf); got != tc.want {
			t.Errorf("%d samples at %d blocks a frame reserved %d entries, want %d",
				tc.samples, tc.bpf, got, tc.want)
		}
	}
}
