package waxflow_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/format"
)

// TestMuxersDoNotRetainPacketData drives the contract container.Muxer states:
// WritePacket must not retain pkt.Data past the call.
//
// It holds by construction today (every muxer writes the payload through or
// copies it), and remux is what makes it load-bearing rather than incidental: a
// demuxer reuses pkt.Data across ReadPacket calls, and the remux loop hands
// those borrowed slices straight to a muxer. A muxer that kept one would corrupt
// the packet *before* it, silently, which is why no test over a single packet
// can catch it and why the contract needed a test rather than only a sentence.
//
// The method is a differential against the muxer itself: feed one run private
// copies, feed the other a single scratch buffer that is overwritten the moment
// WritePacket returns, and require identical bytes. A retained slice reads back
// as the scribble.
func TestMuxersDoNotRetainPacketData(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format string // the fixture to build and then re-mux
		hint   string // that fixture's container, for the demuxer
		build  func(dst io.Writer, t container.Track) container.Muxer
	}{
		{"riff", "wav", "wav", func(d io.Writer, _ container.Track) container.Muxer {
			return riff.NewMuxer(d, nil)
		}},
		{"aiff", "aiff", "aiff", func(d io.Writer, _ container.Track) container.Muxer {
			return aiff.NewMuxer(d)
		}},
		{"mka-pcm", "wav", "wav", func(d io.Writer, _ container.Track) container.Muxer {
			return mka.NewMuxer(d, nil)
		}},
		{"flacn", "flac", "flac", func(d io.Writer, _ container.Track) container.Muxer {
			// MD5 nil is the remux convention: the source's own STREAMINFO
			// signature stands, since the audio is unchanged.
			return flacn.NewMuxer(d, nil)
		}},
		{"mka-flac", "flac", "flac", func(d io.Writer, _ container.Track) container.Muxer {
			return mka.NewMuxer(d, nil)
		}},
		{"ogg-flac", "flac", "flac", func(d io.Writer, _ container.Track) container.Muxer {
			return ogg.NewMuxer(d, nil)
		}},
		{"ogg-opus", "opus", "opus", func(d io.Writer, _ container.Track) container.Muxer {
			return ogg.NewMuxer(d, nil)
		}},
		{"mka-opus", "opus", "opus", func(d io.Writer, _ container.Track) container.Muxer {
			return mka.NewMuxer(d, nil)
		}},
		{"ogg-vorbis", "vorbis", "ogg", func(d io.Writer, _ container.Track) container.Muxer {
			return ogg.NewMuxer(d, nil)
		}},
		{"mpa", "mp3", "mp3", func(d io.Writer, tr container.Track) container.Muxer {
			return mpa.NewMuxer(d, &mpa.MuxerOptions{Delay: int(tr.Delay)})
		}},
		{"adts", "aac", "m4a", func(d io.Writer, _ container.Track) container.Muxer {
			return adts.NewMuxer(d)
		}},
		{"mp4-fragmented", "aac", "m4a", func(d io.Writer, _ container.Track) container.Muxer {
			return mp4.NewMuxer(d, nil)
		}},
		{"mp4-progressive", "aac", "m4a", func(d io.Writer, _ container.Track) container.Muxer {
			return mp4.NewProgressiveMuxer(d, nil)
		}},
		{"mka-aac", "aac", "m4a", func(d io.Writer, _ container.Track) container.Muxer {
			return mka.NewMuxer(d, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := remuxFixture(t, waxflow.TranscodeOptions{Format: tc.format}, 24000)
			track, pkts := demuxAll(t, src, tc.hint)
			if len(pkts) < 3 {
				t.Fatalf("fixture yielded %d packets; need several for a retained one to be visible", len(pkts))
			}
			clean := runMuxer(t, tc.build, track, pkts, false)
			scribbled := runMuxer(t, tc.build, track, pkts, true)
			if !bytes.Equal(clean, scribbled) {
				t.Fatalf("%s retains pkt.Data past WritePacket: overwriting the caller's buffer "+
					"after the call changed the output (%d bytes vs %d)", tc.name, len(scribbled), len(clean))
			}
		})
	}
}

// runMuxer muxes pkts through a freshly built muxer. When scribble is set,
// every payload is handed over in one shared scratch buffer that is filled with
// 0xA5 as soon as WritePacket returns, exactly as a demuxer's reuse would
// overwrite it.
func runMuxer(t *testing.T, build func(io.Writer, container.Track) container.Muxer,
	track container.Track, pkts []codec.Packet, scribble bool) []byte {
	t.Helper()
	ws := &memWS{}
	m := build(ws, track)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	var scratch []byte
	var decoded int64
	for _, p := range pkts {
		decoded += p.Dur
		out := p
		if scribble {
			scratch = append(scratch[:0], p.Data...)
			out.Data = scratch
		}
		if err := m.WritePacket(container.Packet{Track: 0, Packet: out}); err != nil {
			t.Fatal(err)
		}
		if scribble {
			for i := range scratch {
				scratch[i] = 0xA5
			}
		}
	}
	// The same trailer the remux rung synthesizes, so the muxers see the shape
	// they see in production.
	trailer := codec.Trailer{Samples: track.Samples, Delay: track.Delay, Padding: track.Padding}
	if track.Delay > 0 && track.Samples >= 0 {
		trailer.Padding = max(0, decoded-track.Delay-track.Samples)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return ws.b
}

// demuxAll returns a container's default track and every one of its packets,
// with payloads copied (the demuxer reuses its buffer, which is the very hazard
// under test).
func demuxAll(t *testing.T, raw []byte, hint string) (container.Track, []codec.Packet) {
	t.Helper()
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	var out []codec.Packet
	var pkt container.Packet
	for {
		if err := demux.ReadPacket(&pkt); err != nil {
			break
		}
		if pkt.Track != track.ID {
			continue
		}
		p := pkt.Packet
		p.Data = bytes.Clone(pkt.Data)
		out = append(out, p)
	}
	return track, out
}
