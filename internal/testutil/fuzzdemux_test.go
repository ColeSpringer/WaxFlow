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
	tracks       []container.Track
	strictTracks []container.Track // what a strict open states, when not tracks
	packets      []container.Packet
	end          error                             // what a read past the packets returns; nil is io.EOF
	land         func(target int64) (int64, error) // nil lands on the target
	after        *container.Packet                 // what a read after a seek delivers
	reads        int
	sought       bool
	strict       bool // what Open was told
	noSeek       bool // opened as a demuxer that cannot seek
	// settled is what Tracks states once a read reaches the end, nil for no
	// change; trunc is the count of container.Truncated, truncEnd its count
	// from then.
	settled         []container.Track
	trunc, truncEnd int64

	walks    []error // what each Walk answers, the last one repeated; none walks cleanly
	walkN    int     // the Walks so far
	walkedOK bool    // a Walk returned nil
	unwalked bool    // Walked stays false whatever Walk answered
	// walkEdit is what a walk that returns nil before the read does to its
	// packets.
	walkEdit func([]container.Packet) []container.Packet

	noIndex  bool                              // opened as a demuxer that keeps no index
	snapshot []byte                            // what IndexSnapshot gives
	refuses  bool                              // RestoreIndex declines every blob
	restored bool                              // RestoreIndex accepted a blob
	landBack func(target int64) (int64, error) // nil lands as land does once restored
}

// readOnly is a scriptDemux without its SeekSample.
type readOnly struct{ d *scriptDemux }

func (r readOnly) Tracks() []container.Track              { return r.d.Tracks() }
func (r readOnly) ReadPacket(pkt *container.Packet) error { return r.d.ReadPacket(pkt) }

// seekOnly is a scriptDemux without its index.
type seekOnly struct{ readOnly }

func (s seekOnly) SeekSample(ctx context.Context, track int, target int64) (int64, error) {
	return s.d.SeekSample(ctx, track, target)
}

func (d *scriptDemux) Tracks() []container.Track {
	if d.strict && d.strictTracks != nil {
		return d.strictTracks
	}
	return d.tracks
}

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
		if d.settled != nil {
			d.tracks, d.trunc = d.settled, d.truncEnd
		}
		return io.EOF
	}
	*pkt, d.packets = d.packets[0], d.packets[1:]
	return nil
}

func (d *scriptDemux) TruncatedSamples(int) int64 { return d.trunc }

func (d *scriptDemux) SeekSample(_ context.Context, _ int, target int64) (int64, error) {
	d.sought = true
	if d.restored && d.landBack != nil {
		return d.landBack(target)
	}
	if d.land == nil {
		return target, nil
	}
	return d.land(target)
}

func (d *scriptDemux) Walk(context.Context) error {
	var err error
	if len(d.walks) > 0 {
		err = d.walks[min(d.walkN, len(d.walks)-1)]
	}
	d.walkN++
	if err == nil {
		d.walkedOK = true
		if d.walkEdit != nil && d.reads == 0 {
			d.packets, d.walkEdit = d.walkEdit(d.packets), nil
		}
	}
	return err
}

func (d *scriptDemux) Walked() bool { return d.walkedOK && !d.unwalked }

func (d *scriptDemux) KeepsIndex() bool      { return d.snapshot != nil }
func (d *scriptDemux) IndexSnapshot() []byte { return d.snapshot }

func (d *scriptDemux) RestoreIndex([]byte) bool {
	d.restored = !d.refuses
	return d.restored
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

// eat is a walk that takes the first n packets from the read.
func eat(n int) func([]container.Packet) []container.Packet {
	return func(p []container.Packet) []container.Packet { return p[min(n, len(p)):] }
}

// stretch is a walk that lengthens packet i by a sample.
func stretch(i int) func([]container.Packet) []container.Packet {
	return func(p []container.Packet) []container.Packet {
		p[i].Dur++
		return p
	}
}

// settles is two packets of 10 samples on a track the open states exact at
// samples with trunc truncated, which a read to the end settles to after with
// truncEnd truncated.
func settles(samples, trunc, after, truncEnd int64) func() *scriptDemux {
	return func() *scriptDemux {
		return &scriptDemux{tracks: exactTrack(samples, 0, 0, 0), packets: packetsAt(0, 2, 10),
			settled: exactTrack(after, 0, 0, 0), trunc: trunc, truncEnd: truncEnd}
	}
}

// outrun is three packets of 10 samples on a track that says 20 exact. With
// retracts, a read to the end leaves the length and no longer calls it exact.
func outrun(retracts bool) func() *scriptDemux {
	return func() *scriptDemux {
		d := &scriptDemux{tracks: exactTrack(20, 0, 0, 0), packets: packetsAt(0, 3, 10)}
		if retracts {
			d.settled = exactTrack(20, 0, 0, 0)
			d.settled[0].SamplesExact = false
		}
		return d
	}
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
			switch {
			case d.noSeek:
				return readOnly{d}, nil
			case d.noIndex:
				return seekOnly{readOnly{d}}, nil
			}
			return d, nil
		},
		MaxPackets: func([]byte) int64 { return 3 },
	}, opened
}

func oneTrack() []container.Track { return []container.Track{{Fmt: goodFmt}} }

// exactTrack is one track that says samples exact, with the trims it states.
func exactTrack(samples, delay, padding, mid int64) []container.Track {
	return []container.Track{{Fmt: goodFmt, Samples: samples, SamplesExact: true, Delay: delay, Padding: padding, MidPadding: mid}}
}

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
		{name: "a demuxer revision in the spelling", mk: func() *scriptDemux {
			return &scriptDemux{tracks: []container.Track{{Fmt: goodFmt, DemuxerVersion: "mka-demux-12"}}, packets: packetsAt(0, 2, 10)}
		}},
		{name: "a demuxer revision in another spelling", mk: func() *scriptDemux {
			return &scriptDemux{tracks: []container.Track{{Fmt: goodFmt, DemuxerVersion: "mka demuxer v2"}}, packets: packetsAt(0, 2, 10)}
		}, want: "<name>-demux-<n>"},
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

		{name: "a walk that settles and leaves the read alone", mk: good,
			tune: func(f *DemuxFuzz) { f.Walk = true }},
		{name: "a walk that fails, and fails again", mk: func() *scriptDemux {
			d := good()
			d.walks, d.walkEdit = []error{errors.New("damage")}, eat(1)
			return d
		}, tune: func(f *DemuxFuzz) { f.Walk = true }},
		{name: "a walk on a demuxer that cannot walk", mk: func() *scriptDemux {
			d := good()
			d.noSeek = true
			return d
		}, tune: func(f *DemuxFuzz) { f.Walk = true }, want: "cannot walk"},
		{name: "a walk that returns nil and is not walked", mk: func() *scriptDemux {
			d := good()
			d.unwalked = true
			return d
		}, tune: func(f *DemuxFuzz) { f.Walk = true }, want: "not walked"},
		{name: "a second walk that answers otherwise", mk: func() *scriptDemux {
			d := good()
			d.walks = []error{errors.New("damage"), nil}
			return d
		}, tune: func(f *DemuxFuzz) { f.Walk = true }, want: "second walk"},
		{name: "a walk that moves the read", mk: func() *scriptDemux {
			d := good()
			d.walkEdit = eat(1)
			return d
		}, tune: func(f *DemuxFuzz) { f.Walk = true }, want: "packet 0: a read after a walk delivered"},
		{name: "a walk that ends the read early", mk: func() *scriptDemux {
			d := good()
			d.walkEdit = eat(2)
			return d
		}, tune: func(f *DemuxFuzz) { f.Walk = true }, want: "answered EOF"},

		{name: "a walk that moves a lone packet's duration", mk: func() *scriptDemux {
			return &scriptDemux{tracks: oneTrack(), packets: packetsAt(0, 1, 10), walkEdit: stretch(0)}
		}, tune: func(f *DemuxFuzz) { f.Walk = true }, want: "packet 0: a read after a walk delivered"},

		{name: "a restore that lands where its snapshot's demuxer does", mk: func() *scriptDemux {
			d := good()
			d.snapshot = []byte("index")
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.Restore = []int64{0, 100}, true }},
		{name: "a snapshot the fresh open refuses", mk: func() *scriptDemux {
			d := good()
			d.snapshot, d.refuses = []byte("index"), true
			d.landBack = func(int64) (int64, error) { return 0, errors.New("never asked") }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.Restore = []int64{0, 100}, true }},
		{name: "a demuxer that declines to give a snapshot", mk: func() *scriptDemux {
			d := good()
			d.landBack = func(int64) (int64, error) { return 0, errors.New("never asked") }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.Restore = []int64{0, 100}, true }},
		{name: "a restore that moves a landing", mk: func() *scriptDemux {
			d := good()
			d.snapshot = []byte("index")
			d.landBack = func(target int64) (int64, error) { return target / 2, nil }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.Restore = []int64{0, 100}, true }, want: "seek to 100: the restored open landed at 50"},
		{name: "a restore that fails a seek", mk: func() *scriptDemux {
			d := good()
			d.snapshot = []byte("index")
			d.landBack = func(int64) (int64, error) { return 0, errors.New("bad index") }
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.Restore = []int64{0, 100}, true }, want: "seek to 0: the restored open landed at 0 (bad index)"},
		{name: "a restore on a demuxer that keeps no index", mk: func() *scriptDemux {
			d := good()
			d.noIndex = true
			return d
		}, tune: func(f *DemuxFuzz) { f.Targets, f.Restore = []int64{0, 100}, true }, want: "keeps no index"},
		{name: "a restore with no seek targets", mk: func() *scriptDemux {
			d := good()
			d.snapshot = []byte("index")
			return d
		}, tune: func(f *DemuxFuzz) { f.Restore = true }, want: "no seek targets"},

		{name: "an exact length its packets hold", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(20, 0, 0, 0), packets: packetsAt(0, 2, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "an exact length its packets do not hold", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(30, 0, 0, 0), packets: packetsAt(0, 2, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }, want: "says 30 samples exact"},
		{name: "an exact length that counts what a hole took", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: exactTrack(25, 0, 0, 0), packets: packetsAt(0, 2, 10)}
			d.packets[1].PTS, d.packets[1].Lost = 15, 5
			return d
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "an exact length that leaves out what a hole took", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: exactTrack(20, 0, 0, 0), packets: packetsAt(0, 2, 10)}
			d.packets[1].PTS, d.packets[1].Lost = 15, 5
			return d
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }, want: "says 20 samples exact"},
		{name: "an exact length less the trims the track states", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(24, 3, 2, 1), packets: packetsAt(0, 3, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "an exact length with the trims left in", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(30, 3, 2, 1), packets: packetsAt(0, 3, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }, want: "says 30 samples exact"},
		{name: "an exact length of nothing when the trims outrun the packets", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(0, 40, 0, 0), packets: packetsAt(0, 2, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "a length not stated exact", mk: func() *scriptDemux {
			d := &scriptDemux{tracks: exactTrack(30, 0, 0, 0), packets: packetsAt(0, 2, 10)}
			d.tracks[0].SamplesExact = false
			return d
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "an exact length after a read that ended on an error", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(30, 0, 0, 0), packets: packetsAt(0, 2, 10), end: errors.New("damage")}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "an exact length after a read the cap stopped", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(30, 0, 0, 0), packets: packetsAt(0, 5, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength, f.CapNotFatal = true, true }},
		{name: "an exact length a strict open states", mk: func() *scriptDemux {
			return &scriptDemux{tracks: exactTrack(20, 0, 0, 0), strictTracks: exactTrack(30, 0, 0, 0), packets: packetsAt(0, 2, 10)}
		}, tune: func(f *DemuxFuzz) { f.ExactLength = true }},

		{name: "an exact length a read outruns", mk: outrun(false),
			tune: func(f *DemuxFuzz) { f.ExactLength = true }, want: "says 20 samples exact"},
		{name: "an exact length a read outruns and no longer calls exact", mk: outrun(true),
			tune: func(f *DemuxFuzz) { f.ExactLength = true }},
		{name: "a length the open called exact that a read outruns and takes back", mk: outrun(true),
			tune: func(f *DemuxFuzz) { f.OpenLength = true }, want: "the open called 20 samples exact and its packets hold 30"},
		{name: "a length the open called exact that a read may outrun", mk: outrun(true),
			tune: func(f *DemuxFuzz) {
				f.OpenLength, f.LongerWhen = true, func(container.Demuxer) bool { return true }
			}},
		{name: "a length the open called exact that a read may not outrun", mk: outrun(true),
			tune: func(f *DemuxFuzz) {
				f.OpenLength, f.LongerWhen = true, func(container.Demuxer) bool { return false }
			}, want: "the open called 20 samples exact and its packets hold 30"},
		{name: "a length the open called exact that a read falls short of and counts", mk: settles(30, 0, 20, 10),
			tune: func(f *DemuxFuzz) { f.OpenLength = true }},
		{name: "an exact length a read outruns on a track the checks are scoped to", mk: outrun(false),
			tune: func(f *DemuxFuzz) {
				f.ExactLength, f.LengthOn = true, func(tr container.Track) bool { return tr.Samples == 20 }
			}, want: "says 20 samples exact"},
		{name: "an exact length a read outruns on a track the checks are not scoped to", mk: outrun(false),
			tune: func(f *DemuxFuzz) {
				f.ExactLength, f.OpenLength, f.LengthOn = true, true, func(container.Track) bool { return false }
			}},
		{name: "a length the open called exact that a read outruns, on a scoped track", mk: outrun(true),
			tune: func(f *DemuxFuzz) {
				f.OpenLength, f.LengthOn = true, func(tr container.Track) bool { return tr.SamplesExact }
			}, want: "the open called 20 samples exact and its packets hold 30"},
		{name: "a length the open called exact that a read falls short of by more than it counts", mk: settles(30, 0, 25, 5),
			tune: func(f *DemuxFuzz) { f.OpenLength = true },
			want: "the open called 30 samples exact, its packets hold 20, and the read counted 5 truncated"},

		{name: "a read that settles an exact length shorter and counts what it took", mk: settles(30, 0, 20, 10)},
		{name: "a read that settles an exact length shorter and counts none of it", mk: settles(30, 0, 20, 0),
			want: "the open claimed 30 samples (30 there, 0 truncated), after the read 20 (20 there, 0 truncated)"},
		{name: "a read that settles a length the open did not state exact", mk: func() *scriptDemux {
			d := settles(30, 0, 20, 0)()
			d.tracks[0].SamplesExact = false
			return d
		}},
		{name: "a read that keeps the claim of an open that counted some truncated", mk: settles(30, 5, 20, 15)},
		{name: "a read that moves the claim of an open that counted some truncated", mk: settles(30, 5, 20, 10),
			want: "the open claimed 35 samples"},
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
