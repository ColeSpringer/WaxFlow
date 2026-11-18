package asf_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
)

// corpusEntry is the input of one committed FuzzDemux corpus file.
func corpusEntry(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fuzz", "FuzzDemux", name))
	if err != nil {
		t.Fatal(err)
	}
	_, line, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "[]byte("), ")")
	s, err := strconv.Unquote(line)
	if err != nil {
		t.Fatalf("corpus file %s: %v", name, err)
	}
	return []byte(s)
}

// loneV2 is a clean file of one WMA v2 object with no bit reservoir: one
// 2048-sample frame, under a play duration that rounds to 2029 samples.
func loneV2() []byte {
	b := newBuilder()
	b.playHNS = b.prerollMS*10_000 + 46*10_000
	data := make([]byte, 400)
	data[0] = 1
	b.packet(0, 1, 0, uint32(len(data)), uint32(b.prerollMS), data)
	return b.build()
}

// loneReservoir is a clean file of the first object Windows' encoder wrote to
// the committed fixture, under that file's codec configuration, its
// superframe header restated to the given frames (the encoder's is seven).
// The reservoir's last frame never completes.
func loneReservoir(t testing.TB, frames byte) []byte {
	t.Helper()
	src, err := asf.NewDemuxer(container.BytesSource(fixture(t, windowsFixture)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := src.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.Data[0]&0x0f != 7 {
		t.Fatalf("the fixture's first object states %d frames, want 7", pkt.Data[0]&0x0f)
	}
	data := append([]byte(nil), pkt.Data...)
	data[0] = data[0]&0xf0 | frames
	b := newBuilder()
	b.wfx = append([]byte(nil), src.Tracks()[0].CodecConfig...)
	b.packetLen = len(data) + 64
	b.playHNS = b.prerollMS*10_000 + 300*10_000
	b.packet(0, 1, 0, uint32(len(data)), uint32(b.prerollMS), data)
	return b.build()
}

// TestLoneObjectLastsItsCountedFrames: the only packet of a one-object WMA
// v1/v2 stream lasts what its own frames deliver, nothing included, whether a
// walk or a seek's count ran before the read or not, Strict and tolerant; and
// it is the whole of the length the count settles.
func TestLoneObjectLastsItsCountedFrames(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want int64
		// declared is the length the play duration states, which the count
		// must differ from for the case to tell the two apart.
		declared int64
		// refused marks a file Strict refuses at the open.
		refused bool
	}{
		{name: "truncated to one object", raw: corpusEntry(t, "9a216c7adf11b5c9"), want: 6 * 2048, declared: 266188, refused: true},
		{name: "one frame, no reservoir", raw: loneV2(), want: 2048, declared: 2029},
		{name: "seven frames in the reservoir", raw: loneReservoir(t, 7), want: 6 * 2048, declared: 13230},
		// The reservoir's one frame never completes, so the object delivers
		// nothing, and neither does a header that states no frame.
		{name: "one frame in the reservoir", raw: loneReservoir(t, 1), want: 0, declared: 13230},
		{name: "no frames", raw: loneReservoir(t, 0), want: 0, declared: 13230},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, strict := range []bool{false, true} {
				for _, before := range []string{"nothing", "a walk", "a seek"} {
					d, err := asf.NewDemuxer(container.BytesSource(tc.raw), &container.DemuxerOptions{Strict: strict})
					if strict && tc.refused {
						if err == nil {
							t.Fatal("Strict opened a file whose Data Object overstates its packets")
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if got := d.Tracks()[0].Samples; got != tc.declared {
						t.Fatalf("the open declares %d samples, the case was built for %d", got, tc.declared)
					}
					switch before {
					case "a walk":
						if err := d.Walk(context.Background()); err != nil {
							t.Fatal(err)
						}
					case "a seek":
						if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
							t.Fatal(err)
						}
					}
					var pkt container.Packet
					if err := d.ReadPacket(&pkt); err != nil {
						t.Fatal(err)
					}
					if pkt.Dur != tc.want {
						t.Errorf("strict %v, %s before the read: the lone packet lasts %d, want its counted %d", strict, before, pkt.Dur, tc.want)
					}
					if err := d.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
						t.Fatalf("strict %v, %s before the read: a second read answered %v, want the end", strict, before, err)
					}
					if tr := d.Tracks()[0]; !tr.SamplesExact || tr.Samples != tc.want {
						t.Errorf("strict %v, %s before the read: the track settled %d (exact %v), want the packet's %d", strict, before, tr.Samples, tr.SamplesExact, tc.want)
					}
				}
			}
		})
	}
}

// TestTrackNamesTheDemuxerRevision: a WMA v1/v2 track carries
// DemuxerVersion, which is what puts this demuxer's revision in a cache key,
// and keeps it across the walk that settles its length. A Pro, Lossless or
// Voice track reads as it did and names none, so its keys stay.
func TestTrackNamesTheDemuxerRevision(t *testing.T) {
	for _, name := range []string{windowsFixture, "sine-wmav2.wma", "sine-wmav1.wma", "pro-s16.wma", "lossless-s16.wma", "voice-mono.wma"} {
		d := open(t, fixture(t, name))
		want := ""
		if d.Tracks()[0].Codec == codec.WMA {
			want = "asf-demux-2"
		}
		if got := d.Tracks()[0].DemuxerVersion; got != want || (want != "" && want != asf.DemuxerVersion) {
			t.Fatalf("%s: the %s track names demuxer revision %q, want %q", name, d.Tracks()[0].Codec, got, want)
		}
		if err := d.Walk(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := d.Tracks()[0].DemuxerVersion; got != want {
			t.Errorf("%s: after the walk the track names %q, want %q", name, got, want)
		}
	}
}

// TestManyObjectDurationsDoNotMoveWithAWalk pins a stream of several objects:
// each lasts to its successor's stated time and the last as long as the one
// before it, walked or not, Strict and tolerant.
func TestManyObjectDurationsDoNotMoveWithAWalk(t *testing.T) {
	for _, tc := range []struct {
		name          string
		packets       int
		first, last   int64
		durs, settled int64
	}{
		{"sine-wmav2.wma", 44, 2029, 2029, 90053, 90112},
		{"sine-wmav1.wma", 11, 2029, 2028, 22490, 22528},
		{"frag.wma", 11, 2029, 2028, 22490, 22528},
		{windowsFixture, 20, 10231, 14332, 270288, 268288},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fixture(t, tc.name)
			for _, strict := range []bool{false, true} {
				for _, walked := range []bool{false, true} {
					d, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: strict})
					if err != nil {
						t.Fatal(err)
					}
					if walked {
						if err := d.Walk(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					var pkt container.Packet
					var n int
					var first, last, prev, durs int64
					for ; ; n++ {
						err := d.ReadPacket(&pkt)
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if n == 0 {
							first = pkt.Dur
						}
						prev, last = last, pkt.Dur
						durs += pkt.Dur
					}
					if n != tc.packets || first != tc.first || last != tc.last || last != prev || durs != tc.durs {
						t.Errorf("strict %v walked %v: %d packets, the first %d, the last two %d and %d, %d in all; want %d, %d, %d twice, %d",
							strict, walked, n, first, prev, last, durs, tc.packets, tc.first, tc.last, tc.durs)
					}
					if tr := d.Tracks()[0]; !tr.SamplesExact || tr.Samples != tc.settled {
						t.Errorf("strict %v walked %v: the track settled %d (exact %v), want %d", strict, walked, tr.Samples, tr.SamplesExact, tc.settled)
					}
				}
			}
		})
	}
}
