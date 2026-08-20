package mpa

import (
	"bytes"
	"io"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// memWS aliases the shared in-memory io.WriteSeeker.
type memWS = testutil.MemWriteSeeker

// encodeTone runs a tone through the encoder and returns its packets, the
// gapless trailer, and the real input sample count.
func encodeTone(t *testing.T, rate, channels, bitrate, n int) ([][]byte, codec.Trailer, int64) {
	t.Helper()
	f := audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Float, BitDepth: 32}
	e, err := mp3.NewEncoder(f, &mp3.EncoderOptions{Bitrate: bitrate})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	var pkts [][]byte
	emit := func(p codec.Packet) error {
		b := make([]byte, len(p.Data))
		copy(b, p.Data)
		pkts = append(pkts, b)
		return nil
	}
	for off := 0; off < n; off += 1152 {
		m := min(1152, n-off)
		buf := audio.Get(f, m)
		buf.N = m
		for ch := 0; ch < channels; ch++ {
			for i := 0; i < m; i++ {
				x := float64(off + i)
				buf.ChanF(ch)[i] = float32(0.3 * math.Sin(2*math.Pi*440*x/float64(rate)))
			}
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatalf("Encode: %v", err)
		}
		audio.Put(buf)
	}
	tr, err := e.Finish(emit)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return pkts, tr, int64(n)
}

// muxPackets muxes pkts to w with the given projected sample count.
func muxPackets(t *testing.T, w io.Writer, pkts [][]byte, tr codec.Trailer, projected int64, rate, channels int) {
	t.Helper()
	mux := NewMuxer(w, &MuxerOptions{Delay: mp3.EncoderDelay})
	track := container.Track{
		Codec:   codec.MP3,
		Fmt:     audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Float, BitDepth: 32},
		Samples: projected,
	}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, p := range pkts {
		if err := mux.WritePacket(container.Packet{Packet: codec.Packet{Data: p, Dur: 1152, Sync: true}}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := mux.End(tr); err != nil {
		t.Fatalf("End: %v", err)
	}
}

// TestMuxGaplessTag muxes an encoded stream and checks the demuxer reads the
// gapless delay/padding back through the WaxFlow LAME-format tag, for both the
// streaming (projected) and seekable (back-patched) writers.
func TestMuxGaplessTag(t *testing.T) {
	const rate, channels, n = 44100, 2, 40000
	pkts, tr, samples := encodeTone(t, rate, channels, 128000, n)

	for _, seekable := range []bool{false, true} {
		name := "streaming"
		if seekable {
			name = "seekable"
		}
		t.Run(name, func(t *testing.T) {
			var out []byte
			if seekable {
				ws := &memWS{}
				muxPackets(t, ws, pkts, tr, samples, rate, channels)
				out = ws.Buf
			} else {
				var b bytes.Buffer
				muxPackets(t, &b, pkts, tr, samples, rate, channels)
				out = b.Bytes()
			}

			d, err := NewDemuxer(container.BytesSource(out), nil)
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			track := d.Tracks()[0]
			// The demuxer adds the 529-sample decoder delay to the tag value.
			wantDelay := int64(mp3.EncoderDelay + 529)
			if track.Delay != wantDelay {
				t.Errorf("Track.Delay=%d, want %d (tag delay %d + 529)", track.Delay, wantDelay, mp3.EncoderDelay)
			}
			// The demuxer reports Samples as the already-trimmed length
			// (raw frame samples minus delay and padding), so it must equal
			// the real input sample count for the gapless invariant to hold.
			if track.Samples != samples {
				t.Errorf("trimmed length %d (delay %d, padding %d), want %d",
					track.Samples, track.Delay, track.Padding, samples)
			}
			t.Logf("%s: track samples=%d delay=%d padding=%d", name, track.Samples, track.Delay, track.Padding)
		})
	}
}

// TestMuxFramesValid checks that every muxed frame parses in sequence, the
// first is the Info metadata frame carrying the encoder tag, and the total
// frame count is exactly the audio frames plus that one metadata frame.
func TestMuxFramesValid(t *testing.T) {
	pkts, tr, samples := encodeTone(t, 44100, 2, 128000, 30000)
	var b bytes.Buffer
	muxPackets(t, &b, pkts, tr, samples, 44100, 2)
	out := b.Bytes()

	h, err := mp3.ParseHeader(out)
	if err != nil {
		t.Fatalf("first frame header: %v", err)
	}
	off := mp3.HeaderLen + h.SideInfoLen()
	if got := string(out[off : off+4]); got != "Info" {
		t.Errorf("first frame tag %q, want Info", got)
	}
	if got := string(out[off+12 : off+16]); got != "WaxF" {
		t.Errorf("encoder tag %q, want WaxF prefix", got)
	}

	// Walk the whole stream frame by frame: every frame must parse and its
	// size must land the walk exactly on the next frame boundary.
	frames := 0
	for pos := 0; pos < len(out); {
		fh, err := mp3.ParseHeader(out[pos:])
		if err != nil {
			t.Fatalf("frame %d at offset %d does not parse: %v", frames, pos, err)
		}
		sz := fh.Size()
		if sz == 0 || pos+sz > len(out) {
			t.Fatalf("frame %d size %d at offset %d overruns %d bytes", frames, sz, pos, len(out))
		}
		pos += sz
		frames++
	}
	if want := len(pkts) + 1; frames != want {
		t.Errorf("walked %d frames, want %d (%d audio + 1 metadata)", frames, want, len(pkts))
	}
}

// TestMuxLowBitrateTag verifies that even when the LAME gapless extension does
// not fit the frame (very low bit rates), the metadata frame still carries the
// Info marker so the demuxer skips it as metadata rather than decoding a frame
// of silence, and never panics on the tiny frames.
func TestMuxLowBitrateTag(t *testing.T) {
	// MPEG-2.5 stereo at 8 kbit/s / 11.025 kHz: Size ~52, the Xing header fits
	// but the 36-byte LAME extension does not.
	pkts, tr, samples := encodeTone(t, 11025, 2, 8000, 8000)
	var b bytes.Buffer
	muxPackets(t, &b, pkts, tr, samples, 11025, 2)
	out := b.Bytes()

	h, err := mp3.ParseHeader(out)
	if err != nil {
		t.Fatalf("first frame header: %v", err)
	}
	off := mp3.HeaderLen + h.SideInfoLen()
	if got := string(out[off : off+4]); got != "Info" {
		t.Errorf("low-bitrate first frame tag %q, want Info (must still be skippable metadata)", got)
	}
	// The demuxer must recognize the metadata frame and not count it as audio.
	d, err := NewDemuxer(container.BytesSource(out), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if d.Tracks()[0].Codec != codec.MP3 {
		t.Errorf("track codec %q, want mp3", d.Tracks()[0].Codec)
	}
}

// TestMuxAtANonZeroStart writes into a destination the caller had already
// written to. The Info frame is patched at the offset the ID3 header ends,
// which is an offset into the stream, not into the file.
func TestMuxAtANonZeroStart(t *testing.T) {
	const rate, channels, n = 44100, 2, 40000
	pkts, tr, samples := encodeTone(t, rate, channels, 128000, n)
	raw := testutil.MuxAtOffset(t, 97, func(w io.Writer) {
		muxPackets(t, w, pkts, tr, samples, rate, channels)
	})
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	// The demuxer adds the 529-sample decoder delay to the tag value.
	if track, want := d.Tracks()[0], int64(mp3.EncoderDelay+529); track.Delay != want || track.Samples != samples {
		t.Errorf("read back delay %d samples %d, want %d and %d", track.Delay, track.Samples, want, samples)
	}
}

// TestMuxPipeKeepsTheProjection: seekability is probed, not inferred. *os.File
// has Seek for a pipe too, and a muxer that trusted the method set would fail
// at End over a patch the projected Info frame did not need.
func TestMuxPipeKeepsTheProjection(t *testing.T) {
	const rate, channels, n = 44100, 2, 20000
	pkts, tr, samples := encodeTone(t, rate, channels, 128000, n)
	w := &testutil.PipeWriteSeeker{}
	muxPackets(t, w, pkts, tr, samples, rate, channels)
	d, err := NewDemuxer(container.BytesSource(w.Buf), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	// The projected length is the other half of the tag, and on this path
	// nothing downstream corrects it.
	if track, want := d.Tracks()[0], int64(mp3.EncoderDelay+529); track.Delay != want || track.Samples != samples {
		t.Errorf("read back delay %d samples %d, want %d and %d", track.Delay, track.Samples, want, samples)
	}
}
