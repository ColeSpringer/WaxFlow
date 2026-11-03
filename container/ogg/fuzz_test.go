package ogg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production,
// and sane seek landings.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-s16.oga")
	f.Add(full)
	f.Add(full[:100])
	f.Add([]byte("OggS\x00\x02" + "\x00\x00\x00\x00\x00\x00\x00\x00" + "\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x33"))
	f.Add(fixture(f, "noise-s24.oga"))
	// Opus starting 1 s in, with a first page short of its own packets, and
	// with a first page whose granule the next page disowns.
	opus, _, _ := buildSyntheticOpus(312, 3, 4, 0)
	f.Add(testutil.ShiftOggGranules(f, opus, 2, 48000))
	f.Add(testutil.ShiftOggGranules(f, opus, 2, -1000))
	f.Add(testutil.SetOggGranule(f, opus, 2, 0))
	// A lone first page, no end of stream after it, short of its packets.
	f.Add(testutil.SetOggGranule(f, testutil.TruncateOggAfter(f, opus, 2), 2, 0))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			// Every packet consumes at least one lacing byte, so packet
			// production is bounded by the input length.
			maxPackets := int64(len(data)) + 8
			var pkt container.Packet
			firstPTS := int64(-1)
			for i := int64(0); ; i++ {
				if i > maxPackets {
					t.Fatalf("more than %d packets from %d bytes", maxPackets, len(data))
				}
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
				if pkt.Dur <= 0 || len(pkt.Data) == 0 {
					t.Fatal("empty packet must be EOF or error")
				}
				if pkt.PTS < 0 {
					t.Fatalf("packet %d at %d: positions start at 0", i, pkt.PTS)
				}
				if firstPTS < 0 {
					firstPTS = pkt.PTS
				}
			}
			for _, target := range []int64{0, 1000, 1 << 40} {
				landed, err := d.SeekSample(context.Background(), 0, target)
				if err != nil {
					continue
				}
				if landed < 0 {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
				if firstPTS < 0 {
					continue
				}
				if landed > max(target, firstPTS) {
					t.Fatalf("seek to %d landed at %d (stream starts at %d)", target, landed, firstPTS)
				}
			}
		}
	})
}

// FuzzMuxerPages: any run of packet sizes, from one byte to several pages,
// muxes to pages that parse, flag their continuations, state the granule of
// the last packet they complete or -1, and read back as the packets written.
// Each two bytes of plan size a packet, and a set top bit makes it span pages.
func FuzzMuxerPages(f *testing.F) {
	f.Add([]byte{0x00, 0xF0})
	f.Add([]byte{0x80, 0x00, 0x00, 0x10, 0xFF, 0xFF})
	f.Add([]byte{0xFE, 0x00, 0x80, 0x01, 0x00, 0x05, 0x81, 0x00})
	f.Fuzz(func(t *testing.T, plan []byte) {
		if len(plan) < 2 || len(plan) > 64 {
			return
		}
		var out bytes.Buffer
		m := NewMuxer(&out, nil)
		if err := m.Begin([]container.Track{{Codec: codec.Opus, CodecConfig: muxOpusHead(0)}}); err != nil {
			t.Fatal(err)
		}
		var want [][]byte
		var durs []int64
		for i := 0; i+1 < len(plan); i += 2 {
			n := 1 + (int(plan[i]&0x7F)<<8 | int(plan[i+1]))
			if plan[i]&0x80 != 0 {
				n = 1 + (int(plan[i]&0x7F)<<11 | int(plan[i+1])<<3) // up to about 260000 bytes
			}
			data := make([]byte, n)
			data[0] = byte(31<<3 | 0x4) // TOC: CELT FB 20 ms, stereo, code 0
			for j := 1; j < n; j++ {
				data[j] = byte(i + j)
			}
			want, durs = append(want, data), append(durs, 960)
			if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: data, Dur: 960, Sync: true}}); err != nil {
				t.Fatal(err)
			}
		}
		total := int64(len(want)) * 960
		if err := m.End(codec.Trailer{Samples: total}); err != nil {
			t.Fatal(err)
		}
		requirePageFlow(t, readPages(t, out.Bytes()), durs)
		d, err := NewDemuxer(container.BytesSource(out.Bytes()), &DemuxerOptions{Strict: true})
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tr.Samples != total {
			t.Fatalf("the track says %d samples, want %d", tr.Samples, total)
		}
		var pkt container.Packet
		for i := 0; ; i++ {
			err := d.ReadPacket(&pkt)
			if errors.Is(err, io.EOF) {
				if i != len(want) {
					t.Fatalf("read %d packets back, want %d", i, len(want))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if i >= len(want) || !bytes.Equal(pkt.Data, want[i]) {
				t.Fatalf("packet %d does not read back as written", i)
			}
		}
	})
}
