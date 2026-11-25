package mp4

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestFragmentedEndTrimNeedsADelayOrALength: with no delay, the fragmented
// muxer states an end trim as its edit list's duration, so it writes one for
// a track whose length the list holds and refuses any other.
func TestFragmentedEndTrimNeedsADelayOrALength(t *testing.T) {
	const packets, pad = 4, 176
	kept := int64(packets*1024 - pad)
	for _, tc := range []struct {
		name     string
		begin    container.Track // Samples and Padding as Begin sees them
		seekable bool
		trailer  int64 // the trailer's Samples
		fits     bool  // TrackFits and Begin take the track
		ends     bool  // End takes the trailer
	}{
		{"the length at Begin, a pipe", container.Track{Samples: kept}, false, kept, true, true},
		{"the length and the trim at Begin", container.Track{Samples: kept, Padding: pad}, false, kept, true, true},
		{"another length at Begin, patched", container.Track{Samples: kept + 7}, true, kept, true, true},
		{"another length at Begin, a pipe", container.Track{Samples: kept + 7}, false, kept, true, false},
		{"no length at Begin", container.Track{Samples: -1}, true, kept, true, false},
		{"the trim and no length at Begin", container.Track{Samples: -1, Padding: pad}, true, kept, false, false},
		{"the trim and an empty track at Begin", container.Track{Padding: pad}, true, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track := aacTrackAt(t, 48000)
			track.Delay, track.Samples, track.Padding = 0, tc.begin.Samples, tc.begin.Padding
			if err := TrackFits(track); (err == nil) != tc.fits {
				t.Fatalf("TrackFits = %v, want fits=%v", err, tc.fits)
			}
			ws := &testutil.MemWriteSeeker{}
			var pipe bytes.Buffer
			var w io.Writer = &pipe
			if tc.seekable {
				w = ws
			}
			m := NewMuxer(w, nil)
			err := m.Begin([]container.Track{track})
			if (err == nil) != tc.fits {
				t.Fatalf("Begin = %v, want fits=%v", err, tc.fits)
			}
			if err != nil {
				if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat || pipe.Len()+len(ws.Buf) != 0 {
					t.Fatalf("Begin refused with %v after %d bytes", err, pipe.Len()+len(ws.Buf))
				}
				return
			}
			for i := range packets {
				pkt := codec.Packet{Data: []byte{byte(i), 1, 2, 3}, PTS: int64(i) * 1024, Dur: 1024, Sync: true}
				if err := m.WritePacket(container.Packet{Packet: pkt}); err != nil {
					t.Fatal(err)
				}
			}
			err = m.End(codec.Trailer{Samples: tc.trailer, Padding: pad})
			if (err == nil) != tc.ends {
				t.Fatalf("End = %v, want ends=%v", err, tc.ends)
			}
			if err != nil {
				if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
					t.Fatalf("End refused with code %v: %v", waxerr.CodeOf(err), err)
				}
				return
			}
			out := append(pipe.Bytes(), ws.Buf...)
			if dur, mediaTime := elstEntry(t, out); dur != kept || mediaTime != 0 {
				t.Errorf("the edit list states %d samples from %d, want %d from 0", dur, mediaTime, kept)
			}
			d, err := NewDemuxer(container.BytesSource(out), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Tracks()[0]; got.Delay != 0 || got.Samples != kept {
				t.Errorf("the stream reopens with Delay %d, Samples %d; want 0, %d", got.Delay, got.Samples, kept)
			}
		})
	}
}
