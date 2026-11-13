package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

// scriptDemux is a demuxer that does what a test tells it to.
type scriptDemux struct {
	tracks  []container.Track
	packets []container.Packet
	end     error                             // what a read past the packets returns; nil is io.EOF
	land    func(target int64) (int64, error) // nil lands on the target
	after   *container.Packet                 // what a read after a seek delivers
	reads   int
	sought  bool
	strict  bool // what Open was told
	noSeek  bool // opened as a demuxer that cannot seek
}

// readOnly is a scriptDemux without its SeekSample.
type readOnly struct{ d *scriptDemux }

func (r readOnly) Tracks() []container.Track              { return r.d.Tracks() }
func (r readOnly) ReadPacket(pkt *container.Packet) error { return r.d.ReadPacket(pkt) }

func (d *scriptDemux) Tracks() []container.Track { return d.tracks }

func (d *scriptDemux) ReadPacket(pkt *container.Packet) error {
	d.reads++
	if d.sought {
		if d.after == nil {
			return io.EOF
		}
		*pkt = *d.after
		return nil
	}
	if len(d.packets) == 0 {
		if d.end != nil {
			return d.end
		}
		return io.EOF
	}
	*pkt, d.packets = d.packets[0], d.packets[1:]
	return nil
}

func (d *scriptDemux) SeekSample(_ context.Context, _ int, target int64) (int64, error) {
	d.sought = true
	if d.land == nil {
		return target, nil
	}
	return d.land(target)
}

var goodFmt = audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}

// packetsAt builds n one-byte packets of dur samples, the first at pts.
func packetsAt(pts int64, n int, dur int64) []container.Packet {
	out := make([]container.Packet, n)
	for i := range out {
		out[i].Data, out[i].Dur, out[i].PTS = []byte{1}, dur, pts+int64(i)*dur
	}
	return out
}

// bare strips the packets' bytes.
func bare(pkts []container.Packet) []container.Packet {
	for i := range pkts {
		pkts[i].Data = nil
	}
	return pkts
}

// scripted is a DemuxFuzz over scriptDemuxes built by mk, one per open. It
// returns the demuxers opened so far through opened.
func scripted(mk func() *scriptDemux) (f DemuxFuzz, opened *[]*scriptDemux) {
	opened = new([]*scriptDemux)
	return DemuxFuzz{
		Open: func(_ container.Source, strict bool) (container.Demuxer, error) {
			d := mk()
			d.strict = strict
			*opened = append(*opened, d)
			if d.noSeek {
				return readOnly{d}, nil
			}
			return d, nil
		},
		MaxPackets: func([]byte) int64 { return 3 },
	}, opened
}

func oneTrack() []container.Track { return []container.Track{{Fmt: goodFmt}} }

// failure runs f and returns what it failed with, "" when it passed.
func failure(t *testing.T, f DemuxFuzz) (msg string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	f.Run(&fatalTB{TB: t}, []byte("raw"))
	return ""
}

func TestDemuxFuzzHoldsEveryOpenToTheStrictForm(t *testing.T) {
	good := func() *scriptDemux { return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 2, 10)} }
	for _, tc := range []struct {
		name string
		mk   func() *scriptDemux
		tune func(*DemuxFuzz)
		want string // a word of the failure, "" for a pass
	}{
		{name: "a well-formed stream", mk: good},
		{name: "no track", mk: func() *scriptDemux { return &scriptDemux{} }, want: "0 tracks"},
		{name: "two tracks", mk: func() *scriptDemux {
			return &scriptDemux{tracks: append(oneTrack(), oneTrack()...)}
		}, want: "2 tracks"},
		{name: "an invalid format", mk: func() *scriptDemux {
			return &scriptDemux{tracks: []container.Track{{}}}
		}, want: "invalid format"},

		{name: "as many packets as the cap", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 3, 10)}
		}},
		{name: "one packet past the cap", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 4, 10)}
		}, want: "more than 3 packets"},
		{name: "past a cap that is not fatal", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 50, 10)}
		}, tune: func(f *DemuxFuzz) { f.CapNotFatal = true }},

		{name: "an empty packet", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: bare(packetsAt(0, 1, 10))}
		}, want: "no bytes"},
		{name: "an empty packet where one is allowed", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: bare(packetsAt(0, 1, 10))}
		}, tune: func(f *DemuxFuzz) { f.EmptyData = true }},
		{name: "a packet of no samples", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 1, 0)}
		}, want: "duration 0"},
		{name: "a packet of no samples where one is allowed", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 1, 0)}
		}, tune: func(f *DemuxFuzz) { f.ZeroDur = true }},
		{name: "a negative duration, allowed nowhere", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 1, -1)}
		}, tune: func(f *DemuxFuzz) { f.ZeroDur = true }, want: "duration -1"},

		{name: "seek targets on a demuxer that cannot seek", mk: func() *scriptDemux {
			d := good()
			d.noSeek = true
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets = []int64{100} }, want: "cannot seek"},
		{name: "a seek that errors", mk: func() *scriptDemux {
			d := good()
			d.land = func(int64) (int64, error) { return 0, errors.New("no") }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets = []int64{100} }},
		{name: "a seek past its target", mk: func() *scriptDemux {
			d := good()
			d.land = func(target int64) (int64, error) { return target + 1, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets = []int64{100} }, want: "landed at 101"},
		{name: "a late stream's seek, landing on its first packet", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: oneTrack(), packets: packetsAt(500, 2, 10)}
			d.land = func(int64) (int64, error) { return 500, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.LateLanding = []int64{100}, true }},
		{name: "a late stream's seek, landing past its first packet", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: oneTrack(), packets: packetsAt(500, 2, 10)}
			d.land = func(int64) (int64, error) { return 501, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.LateLanding = []int64{100}, true }, want: "landed at 501"},
		{name: "a late landing where none is allowed", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: oneTrack(), packets: packetsAt(500, 2, 10)}
			d.land = func(int64) (int64, error) { return 500, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets = []int64{100} }, want: "landed at 500"},
		// A packet before zero is still a packet read: the stream starts
		// there, and a seek is held to its target.
		{name: "a seek past its target on a stream that starts before zero", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: oneTrack(), packets: packetsAt(-2048, 2, 1024)}
			d.land = func(target int64) (int64, error) { return target + 1, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.LateLanding = []int64{100}, true }, want: "landed at 101"},
		{name: "any landing on a stream no packet was read from", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: oneTrack()}
			d.land = func(int64) (int64, error) { return 1 << 30, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.LateLanding = []int64{100}, true }},

		{name: "the packet after a seek, at the landing less what was lost", mk: func() *scriptDemux {
			d := good()
			d.after = &container.Packet{Packet: packetsAt(140, 1, 10)[0].Packet, Lost: 40}
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.SeekPTS = []int64{100}, true }},
		{name: "the packet after a seek, somewhere else", mk: func() *scriptDemux {
			d := good()
			d.after = &packetsAt(90, 1, 10)[0]
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.SeekPTS = []int64{100}, true }, want: "read a packet at 90"},
		{name: "no packet after a seek", mk: good,
			tune: func(f *DemuxFuzz) { f.Targets, f.SeekPTS = []int64{100}, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := scripted(tc.mk)
			if tc.tune != nil {
				tc.tune(&f)
			}
			got := failure(t, f)
			switch {
			case tc.want == "" && got != "":
				t.Errorf("failed with %q, want a pass", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("failed with %q, want a failure naming %q", got, tc.want)
			}
		})
	}
}

func TestDemuxFuzzStopsAtACapThatIsNotFatal(t *testing.T) {
	f, opened := scripted(func() *scriptDemux {
		return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 50, 10)}
	})
	f.CapNotFatal = true
	if msg := failure(t, f); msg != "" {
		t.Fatal(msg)
	}
	for _, d := range *opened {
		if d.reads != 3 {
			t.Errorf("read %d packets under a cap of 3", d.reads)
		}
	}
}

func TestDemuxFuzzRunsBothModesAndEveryHookInOrder(t *testing.T) {
	stop := errors.New("damage")
	var calls []string
	f, opened := scripted(func() *scriptDemux {
		return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 2, 10), end: stop}
	})
	f.Targets = []int64{0, 7}
	note := func(format string, args ...any) { calls = append(calls, fmt.Sprintf(format, args...)) }
	f.Opened = func(_ testing.TB, d container.Demuxer, strict bool) {
		last := (*opened)[len(*opened)-1]
		note("opened strict=%v same=%v told=%v", strict, d == container.Demuxer(last), last.strict)
	}
	// The cap is asked once an open is accepted, after Opened: a target may
	// size it from what Opened learned.
	f.MaxPackets = func(raw []byte) int64 {
		note("cap for %s", raw)
		return 3
	}
	f.Packet = func(_ testing.TB, pkt *container.Packet) { note("packet %d", pkt.PTS) }
	f.Read = func(_ testing.TB, _ container.Demuxer, strict bool, end error) {
		note("read strict=%v end=%v", strict, end)
	}
	f.Landed = func(_ testing.TB, _ container.Demuxer, target, landed int64) { note("landed %d at %d", target, landed) }
	f.After = func(_ testing.TB, raw []byte, _ container.Demuxer, strict bool) {
		note("after %s strict=%v", raw, strict)
	}
	if msg := failure(t, f); msg != "" {
		t.Fatal(msg)
	}
	var want []string
	for _, strict := range []bool{false, true} {
		want = append(want,
			fmt.Sprintf("opened strict=%v same=true told=%v", strict, strict),
			"cap for raw",
			"packet 0", "packet 10",
			fmt.Sprintf("read strict=%v end=damage", strict),
			"landed 0 at 0", "landed 7 at 7",
			fmt.Sprintf("after raw strict=%v", strict))
	}
	if !slices.Equal(calls, want) {
		t.Errorf("hooks ran as\n%q\nwant\n%q", calls, want)
	}
}

// A seek that errors is passed over: no Landed for it, and the targets after
// it are still sought.
func TestDemuxFuzzSeeksOnPastATargetThatErrors(t *testing.T) {
	var landings []int64
	f, _ := scripted(func() *scriptDemux {
		d := &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 2, 10)}
		d.land = func(target int64) (int64, error) {
			if target == 5 {
				return 0, errors.New("no")
			}
			return target, nil
		}
		return d
	})
	f.Targets = []int64{5, 7}
	f.Landed = func(_ testing.TB, _ container.Demuxer, target, _ int64) { landings = append(landings, target) }
	if msg := failure(t, f); msg != "" {
		t.Fatal(msg)
	}
	if !slices.Equal(landings, []int64{7, 7}) {
		t.Errorf("landed on targets %v, want 7 in each mode and never 5", landings)
	}
}

func TestDemuxFuzzPassesARefusedOpen(t *testing.T) {
	opens := 0
	f := DemuxFuzz{
		Open: func(container.Source, bool) (container.Demuxer, error) {
			opens++
			return nil, errors.New("not this format")
		},
		MaxPackets: func([]byte) int64 { return 3 },
	}
	if msg := failure(t, f); msg != "" {
		t.Fatal(msg)
	}
	if opens != 2 {
		t.Errorf("opened %d times, want once tolerant and once strict", opens)
	}
}

func TestDemuxFuzzReportsACleanEnd(t *testing.T) {
	var ends []error
	f, _ := scripted(func() *scriptDemux { return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 1, 10)} })
	f.Read = func(_ testing.TB, _ container.Demuxer, _ bool, end error) { ends = append(ends, end) }
	if msg := failure(t, f); msg != "" {
		t.Fatal(msg)
	}
	if len(ends) != 2 || ends[0] != io.EOF || ends[1] != io.EOF {
		t.Errorf("a stream read to its end reported %v, want io.EOF twice", ends)
	}

	// A read the cap stopped did not end: nothing says the stream is whole.
	ends = nil
	f, _ = scripted(func() *scriptDemux { return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 50, 10)} })
	f.CapNotFatal = true
	f.Read = func(_ testing.TB, _ container.Demuxer, _ bool, end error) { ends = append(ends, end) }
	if msg := failure(t, f); msg != "" {
		t.Fatal(msg)
	}
	if len(ends) != 2 || ends[0] != nil || ends[1] != nil {
		t.Errorf("a read the cap stopped reported %v, want nil twice", ends)
	}
}
