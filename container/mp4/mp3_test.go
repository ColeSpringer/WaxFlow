package mp4

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
)

// The MP3 fixtures. One encode, remuxed three ways, so the pair the
// differential in tests/ compares is the same bitstream by construction
// rather than by two runs of an encoder agreeing:
//
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=22050:duration=1" \
//	    -ac 1 -c:a libmp3lame -b:a 32k mp3.mp3
//	ffmpeg -i mp3.mp3 -c copy -movie_timescale 22050 mp3.mp4
//	ffmpeg -i mp3.mp3 -c copy mp3-ms.mp4
//	ffmpeg -i mp3.mp3 -c copy mp3.mov
//
// (ffmpeg 8.0.1, Ubuntu.) mp3.mp4 sets the movie timescale to the codec rate
// so its edit list is exact; mp3-ms.mp4 takes ffmpeg's default 1000 so the
// same edit is millisecond-granular; mp3.mov carries the QuickTime '.mp3'
// fourcc in a version 1 entry with no esds instead of an mp4a.
const (
	mp3Frames  = 41    // 41 frames of 576 samples: 23616 raw
	mp3Raw     = 23616 // the sample table's own total
	mp3Delay   = 1105  // LAME's 576 encoder plus 529 decoder samples
	mp3Samples = mp3Raw - mp3Delay
)

// TestDemuxMP3 reads the three MP4-family spellings of one MP3 stream.
//
// All three must come out the same track, because they are the same track:
// the mp4a/esds form and QuickTime's '.mp3' fourcc differ only in where the
// object type is written, and the millisecond edit differs only in how
// precisely it can state a length it does not get to state (see below).
func TestDemuxMP3(t *testing.T) {
	for _, name := range []string{"mp3.mp4", "mp3-ms.mp4", "mp3.mov"} {
		t.Run(name, func(t *testing.T) {
			d := open(t, fixture(t, name))
			tr := d.Tracks()[0]
			if tr.Codec != codec.MP3 {
				t.Fatalf("codec = %q, want mp3", tr.Codec)
			}
			// The decoder's output format, not the entry's: Layer III
			// reconstruction is floating point, and every one of these
			// entries writes a conventional 16 in samplesize.
			want := audio.Format{Rate: 22050, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
			if tr.Fmt != want {
				t.Errorf("format = %v, want %v", tr.Fmt, want)
			}
			if len(tr.CodecConfig) != 0 {
				t.Errorf("codec config = %d bytes, want none: an MPEG frame states its own header", len(tr.CodecConfig))
			}
			if tr.Delay != mp3Delay {
				t.Errorf("delay = %d, want the LAME priming %d", tr.Delay, mp3Delay)
			}
			if tr.Samples != mp3Samples {
				t.Errorf("samples = %d, want %d", tr.Samples, mp3Samples)
			}

			var total, count int64
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("ReadPacket: %v", err)
				}
				// One MP4 sample is one whole MPEG frame, which is what
				// codec/mp3 takes: the packet must begin with a
				// frame header whose own size covers it.
				h, err := mp3.ParseHeader(pkt.Data)
				if err != nil {
					t.Fatalf("packet %d does not begin with an MPEG frame header: %v", count, err)
				}
				if h.Size() != len(pkt.Data) {
					t.Fatalf("packet %d is %d bytes, its header says %d", count, len(pkt.Data), h.Size())
				}
				if h.Rate != 22050 || h.Channels != 1 {
					t.Fatalf("packet %d frame is %d Hz %dch", count, h.Rate, h.Channels)
				}
				if pkt.PTS != total {
					t.Fatalf("packet %d pts = %d, want %d", count, pkt.PTS, total)
				}
				total += pkt.Dur
				count++
			}
			if count != mp3Frames {
				t.Errorf("packets = %d, want %d", count, mp3Frames)
			}
			if total != mp3Raw {
				t.Errorf("packet durations sum to %d, want the raw total %d", total, mp3Raw)
			}
		})
	}
}

// TestMP3MillisecondEditCannotOvershoot pins what the second fixture is for.
//
// The played length comes from the edit list's segment_duration, which is in
// the MOVIE timescale: ffmpeg's default 1000 makes it millisecond-granular, so
// this file's 1021 ms rescales to 22513 samples where the exact fixture's edit
// says 22511. Two samples more than the sample table holds after the delay is
// not a longer stream, it is a rounded number, and the length falls back to
// the count the table actually has.
//
// Both files are asserted against absolute values, not against each other.
// Equality alone was not the test it claimed to be: an implementation that
// ignored the edit list entirely would report Delay 0 and the table's whole
// 23616 samples for both files, agreeing with itself and losing every real
// trim, and would have passed.
func TestMP3MillisecondEditCannotOvershoot(t *testing.T) {
	for _, name := range []string{"mp3.mp4", "mp3-ms.mp4"} {
		tr := open(t, fixture(t, name)).Tracks()[0]
		// The delay is the edit list's media_time, in media (codec-rate)
		// units in both files, so it is exact in both.
		if tr.Delay != mp3Delay {
			t.Errorf("%s: delay = %d, want the edit list's %d", name, tr.Delay, mp3Delay)
		}
		// The length is the edit's segment_duration, in MOVIE units: exact
		// where the movie timescale is the codec rate, and millisecond-
		// granular at ffmpeg's default 1000, where this file's 1021 ms
		// rescales to 22513 samples. Two more than the table holds after
		// the delay is not a longer stream, it is a rounded number, so the
		// length falls back to the count the table actually has and both
		// files answer 22511.
		if tr.Samples != mp3Samples {
			t.Errorf("%s: samples = %d, want %d", name, tr.Samples, mp3Samples)
		}
		if tr.Padding != 0 {
			t.Errorf("%s: padding = %d; an edit that overshoots the table cannot leave a negative tail",
				name, tr.Padding)
		}
		if got := tr.Delay + tr.Samples + tr.Padding; got != mp3Raw {
			t.Errorf("%s: the trims account for %d samples, the table holds %d", name, got, mp3Raw)
		}
	}
}

// TestMP3FrameOverhead pins the per-frame overhead the reservoir walk credits
// against, and the low-bitrate case that a fixed worst-case number broke.
//
// MPEG-1 stereo's 32-byte side info is the largest of the four shapes, so
// charging every frame for it credits nothing at all to a stream whose frames
// are smaller than 38 bytes, and the walk then runs to the top of the file
// instead of covering the reservoir's 575 bytes. An 8 kbit/s frame is 24
// bytes, so the case is reachable rather than hypothetical.
func TestMP3FrameOverhead(t *testing.T) {
	for _, tc := range []struct {
		rate, channels int
		want           int64
	}{
		{44100, 2, 4 + 2 + 32}, // MPEG-1 stereo, the largest
		{44100, 1, 4 + 2 + 17}, // MPEG-1 mono
		{22050, 2, 4 + 2 + 17}, // MPEG-2 stereo
		{22050, 1, 4 + 2 + 9},  // MPEG-2 mono
		{8000, 1, 4 + 2 + 9},   // MPEG-2.5 mono, the smallest
	} {
		f := audio.Format{Rate: tc.rate, Channels: tc.channels}
		if got := mp3FrameOverhead(f); got != tc.want {
			t.Errorf("mp3FrameOverhead(%d Hz %dch) = %d, want %d", tc.rate, tc.channels, got, tc.want)
		}
	}

	// The walk itself, over a table of 24-byte frames: with the overhead a
	// mono MPEG-2.5 stream actually has, each credits 9 bytes and the
	// reservoir is covered in 64 frames. With the fixed 38 each credited
	// nothing and the landing was 0 whatever the target.
	st := &sampleTable{sizes: make([]uint32, 400)}
	for i := range st.sizes {
		st.sizes[i] = 24
	}
	oh := mp3FrameOverhead(audio.Format{Rate: 8000, Channels: 1})
	land := st.mp3Backoff(300, oh)
	if land == 0 {
		t.Fatalf("the walk reached the top of the stream from sample 300; it has to be bounded")
	}
	if want := int64(300 - (mp3ReservoirBytes+8)/9); land > want {
		t.Errorf("landed at %d, want no earlier than %d for 24-byte frames", land, want)
	}

	// And it is bounded even when no frame can ever cover the reservoir,
	// which is a crafted table rather than a stream.
	tiny := &sampleTable{sizes: make([]uint32, 4000)}
	for i := range tiny.sizes {
		tiny.sizes[i] = 1
	}
	if land := tiny.mp3Backoff(3000, oh); land != 3000-mp3BackoffFrames {
		t.Errorf("one-byte samples walked to %d, want the %d-frame cap", land, mp3BackoffFrames)
	}
}

// TestMP3SeekBacksOffForTheReservoir holds the seek to both halves of Layer
// III's inter-frame state.
//
// The sample table carries no stss, so every frame reads as a sync point and
// the generic path would land exactly on the target frame. That is wrong for
// MP3 twice over: the filterbank keeps overlap history, and a frame's main
// data can begin up to 511 bytes earlier in the stream. At this fixture's 32
// kbit/s a frame is about 96 bytes, so the reservoir alone is worth six
// frames, and the landing has to precede the target by more than the fixed
// preroll accounts for.
func TestMP3SeekBacksOffForTheReservoir(t *testing.T) {
	d := open(t, fixture(t, "mp3.mp4"))
	const spf = 576
	target := int64(30 * spf)
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatal(err)
	}
	if landed > target {
		t.Fatalf("seek to %d landed at %d, past the target", target, landed)
	}
	if landed%spf != 0 {
		t.Errorf("landing %d is not a frame boundary", landed)
	}
	// mp3SeekPreroll is the filterbank's share. Anything at or above that
	// landing means the reservoir walk contributed nothing, which at this bit
	// rate it must.
	if want := target - mp3SeekPreroll; landed >= want {
		t.Errorf("seek to %d landed at %d; the fixed preroll alone reaches %d, so the reservoir walk did nothing",
			target, landed, want)
	}
	// And it must not run away: the whole backoff is a bounded number of
	// frames, not a rewind to the top of a long file.
	if landed == 0 {
		t.Errorf("seek to %d landed at 0; the backoff is bounded, not a rewind", target)
	}

	// A target inside the backoff distance clamps at the stream's top rather
	// than going negative.
	if landed, err := d.SeekSample(0, spf); err != nil || landed != 0 {
		t.Errorf("seek to %d = %d, %v; want 0", spf, landed, err)
	}
}

// TestMP3EntryDisagreeingWithTheFrames pins which side wins when the sample
// entry and the frames it describes do not agree.
//
// An MP3 track is the one place here with nothing to cross-check the entry
// against: its esds carries no DecoderSpecificInfo and QuickTime's own entry
// carries nothing at all. So the first frame's header is read at open and
// wins, exactly as an AAC track's ASC beats its entry's channelcount. Without
// that the entry is believed, the file probes plausibly, and codec/mp3 refuses
// the first packet for disagreeing with a track built from a lie.
//
// The fixture is edited rather than generated: the entry's 16.16 samplerate is
// halved in place, which is a field no MP3 decoder reads and every reader of
// this container does.
func TestMP3EntryDisagreeingWithTheFrames(t *testing.T) {
	raw := fixture(t, "mp3.mp4")
	at := bytes.Index(raw, []byte("mp4a"))
	if at < 0 {
		t.Fatal("no mp4a entry in the fixture")
	}
	// The entry body starts after the 4-byte fourcc; samplerate sits at
	// offset 24 within it, as a 16.16 fixed-point value.
	rateOff := at + 4 + 24
	lying := bytes.Clone(raw)
	if got := be16(lying[rateOff : rateOff+2]); got != 22050 {
		t.Fatalf("entry rate reads %d at the computed offset, want 22050", got)
	}
	const half = 11025
	lying[rateOff] = byte(half >> 8)
	lying[rateOff+1] = byte(half & 0xFF)

	d := open(t, lying)
	tr := d.Tracks()[0]
	if tr.Fmt.Rate != 22050 {
		t.Errorf("rate = %d, want the frames' 22050 over the entry's 11025", tr.Fmt.Rate)
	}
	// And it says so, at a kind --strict escalates: an entry that disagrees
	// with its own frames is a file deviating from its format.
	var said bool
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage && strings.Contains(w.Msg, "the frame wins") {
			said = true
		}
	}
	if !said {
		t.Errorf("warnings = %v, want one naming the disagreement", d.Warnings())
	}
	if _, err := NewDemuxer(container.BytesSource(lying), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict accepted an entry that disagrees with its frames")
	}

	// Every packet must then decode against the adopted format, which is the
	// failure the adoption exists to prevent.
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		h, err := mp3.ParseHeader(pkt.Data)
		if err != nil {
			t.Fatal(err)
		}
		if h.PCMFormat() != tr.Fmt {
			t.Fatalf("frame format %v is not the track's %v", h.PCMFormat(), tr.Fmt)
		}
	}
}
