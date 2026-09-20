package waxflow

// A cut of a source that trims samples inside its run (a Matroska
// DiscardPadding on a block before the last). The cut's arithmetic runs on the
// raw decode timeline, where the packet grid holds; the trims are what map the
// track's own timeline onto it.

import (
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// trimmedOpusTrack is a walked Opus track over 100 packets of 960: a 312
// pre-skip, a 648 tail trim, packet 8 trimmed by 480 at its tail and packet
// 50 trimmed whole. Raw 96000, audio 96000-312-648-1440 = 93600.
func trimmedOpusTrack() container.Track {
	t := opusTrack(312, 93600)
	t.Padding = 648
	t.SamplesExact = true
	t.MidPadding = 1440
	t.MidTrims = []container.PacketTrim{{Pos: 8160, Samples: 480}, {Pos: 48000, Samples: 960}}
	return t
}

// TestSpliceTrimsSubsumesATrimInThePreroll: a source trim on a packet the
// spliced pre-roll discards whole is not an additional trim. Counting it
// again over-states MidPadding and, worse, leaves MidTrims out of ascending
// order, which plannedTrim binary-searches: the walk would then read the
// wrong trim for a packet, or none.
func TestSpliceTrimsSubsumesATrimInThePreroll(t *testing.T) {
	track := trimmedOpusTrack()
	// Span 1's splice point is snapUp(50000+312+960) = 51840, and the
	// pre-roll reaches back to 51840-3840 = 48000, which is where the
	// whole-packet trim at raw 48000 sits.
	spans := []Span{{0, 20000}, {50000, 60000}}
	res, err := computeCut(track, TranscodeOptions{SpliceTrims: true}, spans, 960)
	if err != nil {
		t.Fatal(err)
	}
	cut := res.track
	if !cut.MidTrimsComplete() {
		t.Errorf("MidPadding %d does not match MidTrims %v", cut.MidPadding, cut.MidTrims)
	}
	if !slices.IsSortedFunc(cut.MidTrims, func(a, b container.PacketTrim) int {
		return int(a.Pos - b.Pos)
	}) {
		t.Errorf("MidTrims is not ascending: %v", cut.MidTrims)
	}
	var seen int64
	for _, tr := range cut.MidTrims {
		if tr.Pos < seen {
			t.Errorf("trim %v overlaps the one before it", tr)
		}
		if tr.Samples > 960 {
			t.Errorf("trim %v is longer than a packet", tr)
		}
		seen = tr.Pos + tr.Samples
	}
}

// TestCutTrackMapsSpansThroughInnerTrims pins the arithmetic: a span bound is
// carried past the delay and past every trim before it, the window snaps on
// the raw grid there, and the trims the kept packets carry come off the
// synthesized length exactly as the reader will drop them.
func TestCutTrackMapsSpansThroughInnerTrims(t *testing.T) {
	track := trimmedOpusTrack()
	for _, tc := range []struct {
		name   string
		spans  []Span
		delay  int64
		pad    int64
		length int64
		mid    int64
		trims  []container.PacketTrim
		landed []Span
		win    []cutWindow
	}{
		{
			// df = 9600+312+480 = 10392, sd = snapDown(10392-3840) = 5760; the
			// head slop [5760, 10392) holds the 480 trim, so the delay is the
			// slop less the trim. dt = 28800+312+480 = 29592, su = 29760.
			name:  "the trim inside the head slop",
			spans: []Span{{9600, 28800}},
			delay: 10392 - 5760 - 480, pad: 29760 - 29592, length: 19200,
			mid: 480, trims: []container.PacketTrim{{Pos: 8160 - 5760, Samples: 480}},
			landed: []Span{{9600, 28800}}, win: []cutWindow{{5760, 29760}},
		},
		{
			// Both trims lie before the span: df = 50000+312+480+960 = 51752,
			// sd = snapDown(47912) = 47040, and the whole-packet trim at 48000
			// sits in the head slop.
			name:  "a whole packet trimmed in the head slop",
			spans: []Span{{50000, 60000}},
			delay: 51752 - 47040 - 960, pad: 62400 - 61752, length: 10000,
			mid: 960, trims: []container.PacketTrim{{Pos: 48000 - 47040, Samples: 960}},
			landed: []Span{{50000, 60000}}, win: []cutWindow{{47040, 62400}},
		},
		{
			// The first trim falls in the gap between the spans and the second
			// past them, so the output carries none: every destination serves.
			// Both interior edges snap inward: the first span's tail from
			// 5312 down to 4800 and the second span's head from 20792 up to
			// 21120, neither reaching into the gap the request removed.
			// Landed[1].From is packet 22's start on the track's timeline:
			// 21120 - 312 - 480.
			name:  "the trim in a gap",
			spans: []Span{{0, 5000}, {20000, 30000}},
			delay: 312, pad: 31680 - 30792, length: 4800 + (31680 - 21120) - 312 - 888,
			mid: 0, trims: nil,
			landed: []Span{{0, 4488}, {20328, 30000}}, win: []cutWindow{{0, 4800}, {21120, 31680}},
		},
		{
			// A span ending where packet 8's audio ends: the end bound stays
			// before the trim (a trim at the bound is past it), the window ends
			// at the packet's end, and the trim is the tail slop.
			name:  "a span ending at a trimmed packet's audio",
			spans: []Span{{0, 7848}},
			delay: 312, pad: 480, length: 7848,
			mid: 0, trims: nil,
			landed: []Span{{0, 7848}}, win: []cutWindow{{0, 8640}},
		},
		{
			// A span starting where packet 9's audio begins: the start bound
			// steps over the trim, and the pre-roll keeps packet 8 with its
			// trim in the head slop.
			name:  "a span starting after a trimmed packet",
			spans: []Span{{7848, 20000}},
			delay: 8640 - 4800 - 480, pad: 21120 - 20792, length: 20000 - 7848,
			mid: 480, trims: []container.PacketTrim{{Pos: 8160 - 4800, Samples: 480}},
			landed: []Span{{7848, 20000}}, win: []cutWindow{{4800, 21120}},
		},
		{
			// ToEnd: the decode ends at 312+93600+648+1440 = 96000 and the
			// audio 648 before it.
			name:  "to the end",
			spans: []Span{{50000, ToEnd}},
			delay: 51752 - 47040 - 960, pad: 648, length: 93600 - 50000,
			mid: 960, trims: []container.PacketTrim{{Pos: 48000 - 47040, Samples: 960}},
			landed: []Span{{50000, 93600}}, win: []cutWindow{{47040, -1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := computeCut(track, TranscodeOptions{}, tc.spans, 960)
			if err != nil {
				t.Fatal(err)
			}
			cut := res.track
			if cut.Delay != tc.delay || cut.Padding != tc.pad || cut.Samples != tc.length {
				t.Errorf("cut = {Delay %d Padding %d Samples %d}, want {%d %d %d}",
					cut.Delay, cut.Padding, cut.Samples, tc.delay, tc.pad, tc.length)
			}
			if cut.MidPadding != tc.mid {
				t.Errorf("MidPadding = %d, want %d", cut.MidPadding, tc.mid)
			}
			if !slices.Equal(cut.MidTrims, tc.trims) {
				t.Errorf("MidTrims = %v, want %v", cut.MidTrims, tc.trims)
			}
			if !cut.MidTrimsComplete() {
				t.Error("the cut track's trims do not add up to its MidPadding")
			}
			if len(res.landed) != len(tc.landed) {
				t.Fatalf("Landed = %v, want %v", res.landed, tc.landed)
			}
			for i := range tc.landed {
				if res.landed[i] != tc.landed[i] {
					t.Errorf("Landed[%d] = %v, want %v", i, res.landed[i], tc.landed[i])
				}
			}
			if len(res.windows) != len(tc.win) {
				t.Fatalf("windows = %v, want %v", res.windows, tc.win)
			}
			for i := range tc.win {
				if res.windows[i] != tc.win[i] {
					t.Errorf("window %d = %v, want %v", i, res.windows[i], tc.win[i])
				}
			}
			// The keystone, restated for a trimmed run: the trailer the muxer
			// is handed resolves to the cut's own trims. The run the copy
			// counts excludes the inner trims it forwarded and adds back the
			// final packet's.
			var raw int64
			for _, w := range res.windows {
				end := w.to
				if end < 0 {
					end = 96000
				}
				raw += end - w.from
			}
			run := copiedRun{samples: raw - cut.MidPadding, lastPadding: cut.Padding, trimmed: cut.Padding > 0}
			tr := remuxTrailer(cut, run)
			if tr.Padding != cut.Padding || tr.Samples != cut.Samples || tr.Delay != cut.Delay {
				t.Errorf("remuxTrailer = %+v, want {Samples %d Delay %d Padding %d}", tr, cut.Samples, cut.Delay, cut.Padding)
			}
		})
	}
}

// TestCutTrackDeclinesTrimsItCannotPlace: a walk that ran out of room for
// positions leaves MidPadding larger than its list, and a track nothing
// walked has a sum and no list. Neither can be cut around.
func TestCutTrackDeclinesTrimsItCannotPlace(t *testing.T) {
	short := trimmedOpusTrack()
	short.MidTrims = short.MidTrims[:1]
	unwalked := trimmedOpusTrack()
	unwalked.MidTrims = nil
	unsorted := trimmedOpusTrack()
	unsorted.MidTrims[0], unsorted.MidTrims[1] = unsorted.MidTrims[1], unsorted.MidTrims[0]
	for _, tc := range []struct {
		name  string
		track container.Track
	}{
		{"an incomplete list", short},
		{"no list", unwalked},
		{"a list out of order", unsorted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CutTrack(tc.track, TranscodeOptions{}, []Span{{60000, 70000}}, 960)
			if err == nil {
				t.Fatal("CutTrack planned around trims it could not place")
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
				t.Errorf("code = %v, want %v (a decline: the transcode rung serves it)", got, waxerr.CodeUnsupportedFormat)
			}
		})
	}
}

// trimDemuxer yields n packets of dur with the given tails trimmed, and seeks
// on the timeline a demuxer speaks, which excludes every trim before the
// landing (see container.Packet.Padding).
type trimDemuxer struct {
	n, i int
	dur  int64
	pads map[int]int64
}

func (d *trimDemuxer) Tracks() []container.Track { return nil }

// start is packet i's position on the delivered timeline.
func (d *trimDemuxer) start(i int) int64 {
	pos := int64(i) * d.dur
	for k, p := range d.pads {
		if k < i {
			pos -= min(p, d.dur)
		}
	}
	return pos
}

func (d *trimDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.i == d.n {
		return io.EOF
	}
	*pkt = container.Packet{Track: 0, Padding: d.pads[d.i], Packet: codec.Packet{
		Data: []byte{byte(d.i)},
		PTS:  d.start(d.i),
		Dur:  d.dur,
		Sync: true,
	}}
	d.i++
	return nil
}

func (d *trimDemuxer) SeekSample(track int, target int64) (int64, error) {
	i := 0
	for i+1 < d.n && d.start(i+1) <= target {
		i++
	}
	d.i = i
	return d.start(i), nil
}

// TestCutViewForwardsTheTrimsThePlanKnew: the view keeps a trimmed packet's
// trim on it, stamps the packets after it on the delivered timeline, and
// still refuses a trim the plan did not account for, wherever it lies before
// the last packet the cut needs.
func TestCutViewForwardsTheTrimsThePlanKnew(t *testing.T) {
	track := trimmedOpusTrack()
	pads := map[int]int64{8: 480, 50: 960}
	view, err := Cut(&trimDemuxer{n: 100, dur: 960, pads: pads}, track, TranscodeOptions{}, []Span{{9600, 28800}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var out int64
	for i := 6; ; i++ {
		err := view.ReadPacket(&pkt)
		if err == io.EOF {
			if i != 31 {
				t.Fatalf("the view ended before packet %d; want packets 6..30", i)
			}
			break
		}
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if int(pkt.Data[0]) != i {
			t.Fatalf("got source packet %d, want %d", pkt.Data[0], i)
		}
		if pkt.Padding != pads[i] {
			t.Errorf("packet %d: Padding = %d, want %d forwarded", i, pkt.Padding, pads[i])
		}
		if pkt.PTS != out {
			t.Errorf("packet %d: PTS = %d, want %d (the delivered timeline)", i, pkt.PTS, out)
		}
		out += pkt.Dur - min(pkt.Padding, pkt.Dur)
	}

	// The same span over a source whose packets carry a trim the plan's
	// track does not list: refused on the packet after it, as before.
	blind := trimmedOpusTrack()
	blind.MidPadding, blind.MidTrims = 0, nil
	view, err = Cut(&trimDemuxer{n: 100, dur: 960, pads: pads}, blind, TranscodeOptions{}, []Span{{9600, 28800}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		err := view.ReadPacket(&pkt)
		if err == nil {
			continue
		}
		if err == io.EOF {
			t.Fatal("the view finished over a trim the plan never saw")
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
			t.Fatalf("code = %v, want %v: %v", code, waxerr.CodeUnsupportedFormat, err)
		}
		break
	}

	// A trim the plan placed elsewhere is the same blindness: the packets
	// disagree with the track the windows were computed from.
	moved := trimmedOpusTrack()
	moved.MidTrims = []container.PacketTrim{{Pos: 7200, Samples: 480}, {Pos: 48000, Samples: 960}}
	view, err = Cut(&trimDemuxer{n: 100, dur: 960, pads: pads}, moved, TranscodeOptions{}, []Span{{9600, 28800}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	for {
		err := view.ReadPacket(&pkt)
		if err == nil {
			continue
		}
		if err == io.EOF {
			t.Fatal("the view finished over a trim that is not where the plan put it")
		}
		break
	}
}

// TestCutViewChecksOnlyThePacketsItNeeds: the backstop checks a packet once
// something follows it, so the last kept packet is covered (a trim there the
// plan never saw puts the requested end inside samples that are not audio),
// while the packet that ends the walk and everything past it are not.
func TestCutViewChecksOnlyThePacketsItNeeds(t *testing.T) {
	// Windows [5760, 29760): packets 6..30 are kept, 31 ends the walk.
	for _, tc := range []struct {
		name    string
		pads    map[int]int64
		refuses bool
	}{
		{"a trim on the packet that ends the walk", map[int]int64{31: 480}, false},
		{"a trim past every window", map[int]int64{40: 480}, false},
		{"a trim on the last kept packet", map[int]int64{30: 480}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := Cut(&trimDemuxer{n: 100, dur: 960, pads: tc.pads}, opusTrack(312, 93600), TranscodeOptions{}, []Span{{9600, 28800}}, 960)
			if err != nil {
				t.Fatal(err)
			}
			var pkt container.Packet
			n := 0
			for {
				err := view.ReadPacket(&pkt)
				if err == io.EOF {
					break
				}
				if err != nil {
					if !tc.refuses {
						t.Fatalf("after %d packets: %v", n, err)
					}
					if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
						t.Fatalf("code = %v, want %v: %v", code, waxerr.CodeUnsupportedFormat, err)
					}
					return
				}
				n++
			}
			if tc.refuses {
				t.Fatal("the view finished over a trim on its last kept packet that the plan never saw")
			}
			if n != 25 {
				t.Errorf("the view delivered %d packets, want 25", n)
			}
		})
	}
}

// partialTrimTrack is trimmedOpusTrack without the whole-packet trim: 100
// packets of 960, a 312 pre-skip, a 648 tail trim, and packet 8 trimmed by
// 480 at its tail. Audio 96000-312-648-480 = 94560.
func partialTrimTrack() container.Track {
	t := opusTrack(312, 94560)
	t.Padding = 648
	t.SamplesExact = true
	t.MidPadding = 480
	t.MidTrims = []container.PacketTrim{{Pos: 8160, Samples: 480}}
	return t
}

// TestCutSeekViewConvertsThroughTrims: the seekable view names a source
// position on the raw timeline, and the demuxer it seeks speaks the delivered
// one, so a trim before the window shifts the target it asks for and the
// landing it is told.
func TestCutSeekViewConvertsThroughTrims(t *testing.T) {
	track := partialTrimTrack()
	pads := map[int]int64{8: 480}
	// [12000, 24400): df = 12792, sd = snapDown(8952) = 8640 (packet 9); the
	// trim on packet 8 lies before the window and shifts the delivered
	// timeline under it. dt = 25192, su = 25920.
	demux := &trimDemuxer{n: 100, dur: 960, pads: pads}
	view, err := cutSeekable(demux, track, TranscodeOptions{}, []Span{{12000, 24400}}, 960)
	if err != nil {
		t.Fatal(err)
	}
	sk := view.(container.Seeker)
	// Output sample 9600 is ten packets into the window: source packet 19,
	// raw 18240, which the demuxer knows as 18240 - 480.
	landed, err := sk.SeekSample(0, 9600)
	if err != nil {
		t.Fatal(err)
	}
	if landed != 9600 {
		t.Fatalf("landed at %d, want 9600", landed)
	}
	var pkt container.Packet
	if err := view.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if int(pkt.Data[0]) != 19 || pkt.PTS != 9600 {
		t.Errorf("after the seek: source packet %d at PTS %d, want packet 19 at 9600", pkt.Data[0], pkt.PTS)
	}
}

// TestSeekableCutDeclinesAWholePacketTrim: a trim that empties a packet puts
// two packets at one position on the delivered timeline, so a seek landing
// there cannot say which one the demuxer delivers first. The segmented cut
// declines at plan time and the seekable view refuses; the progressive cut,
// which never seeks, is unaffected.
func TestSeekableCutDeclinesAWholePacketTrim(t *testing.T) {
	track := trimmedOpusTrack() // packet 50 is trimmed whole
	spans := []Span{{60000, 70000}}
	if _, err := cutSeekable(&trimDemuxer{n: 100, dur: 960, pads: map[int]int64{8: 480, 50: 960}}, track, TranscodeOptions{}, spans, 960); err == nil {
		t.Error("cutSeekable accepted a source with a whole-packet trim")
	} else if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
		t.Errorf("cutSeekable code = %v, want %v", code, waxerr.CodeUnsupportedFormat)
	}
	plan, err := New().PlanCutSegments(track, TranscodeOptions{Format: "opus"}, spans, 960, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Error("PlanCutSegments planned a segmented cut of a source with a whole-packet trim")
	}
	if _, _, err := CutTrack(track, TranscodeOptions{}, spans, 960); err != nil {
		t.Errorf("the progressive cut declined the same source: %v", err)
	}
}

// TestCutTrackDeclinesTrimsOffTheGrid: the arithmetic rests on every trim
// ending on a packet boundary inside one packet, and on the trim sum being
// bounded like the other addends; a list saying otherwise declines, as does
// one with no sum to account for.
func TestCutTrackDeclinesTrimsOffTheGrid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trims []container.PacketTrim
		sum   int64
	}{
		{"a trim ending mid-packet", []container.PacketTrim{{Pos: 8000, Samples: 480}}, 480},
		{"a trim larger than a packet", []container.PacketTrim{{Pos: 7680, Samples: 1920}}, 1920},
		{"a list with no sum", []container.PacketTrim{{Pos: 8160, Samples: 480}}, 0},
		{"a sum past the timeline", []container.PacketTrim{{Pos: 8160, Samples: maxCutSample}}, maxCutSample},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track := opusTrack(312, 93600)
			track.MidTrims, track.MidPadding = tc.trims, tc.sum
			_, _, err := CutTrack(track, TranscodeOptions{}, []Span{{20000, 30000}}, 960)
			if err == nil {
				t.Fatal("CutTrack planned around trims it cannot place")
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
				t.Errorf("code = %v, want %v", got, waxerr.CodeUnsupportedFormat)
			}
		})
	}
}
