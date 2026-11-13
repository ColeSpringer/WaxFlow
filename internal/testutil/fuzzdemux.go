package testutil

import (
	"context"
	"regexp"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// DemuxFuzz says what one demuxer is held to on arbitrary bytes. The zero
// value of every relaxation is the strict form; one is set with its reason
// beside it in the package's fuzz_test.go.
//
// Run opens the bytes tolerant and then strict. Each open that is accepted
// states one track of a valid format, reads to its end or to the packet cap,
// and is then sought to each target. A refused open, a read that errors and a
// seek that errors all pass: the bytes are arbitrary.
type DemuxFuzz struct {
	Open func(src container.Source, strict bool) (container.Demuxer, error)
	// MaxPackets bounds the packets the bytes can hold. It is asked once the
	// open is accepted, after Opened.
	MaxPackets func(raw []byte) int64
	// Targets are the samples sought once the read is over.
	Targets []int64

	// Relaxations, each one a fact about a format.
	CapNotFatal bool // the read stops at the cap and is not failed for reaching it
	EmptyData   bool // a packet may carry no bytes
	ZeroDur     bool // a packet may carry no samples
	LateLanding bool // a seek may land past its target, on the first packet of a stream that starts late

	// SeekPTS holds the packet read after a seek to the landing: its PTS,
	// less the samples it reports lost, is where the seek said it landed.
	SeekPTS bool

	// What one format adds, each nil or run in this order: Opened on an
	// accepted open, Packet on every packet read, Read when the read is over
	// (end is io.EOF, the error that stopped it, or nil when the cap did),
	// Landed on every seek that did not error, After last.
	Opened func(t testing.TB, d container.Demuxer, strict bool)
	Packet func(t testing.TB, pkt *container.Packet)
	Read   func(t testing.TB, d container.Demuxer, strict bool, end error)
	Landed func(t testing.TB, d container.Demuxer, target, landed int64)
	After  func(t testing.TB, raw []byte, d container.Demuxer, strict bool)
}

// demuxerVersionSpelling is how a track names its demuxer's revision, the
// cache-key term of ADR-0004.
var demuxerVersionSpelling = regexp.MustCompile(`^[a-z0-9]+-demux-[1-9][0-9]*$`)

// Run holds the demuxer f opens over raw to f.
func (f DemuxFuzz) Run(t testing.TB, raw []byte) {
	t.Helper()
	for _, strict := range []bool{false, true} {
		d, err := f.Open(container.BytesSource(raw), strict)
		if err != nil {
			continue
		}
		f.hold(t, raw, d, strict)
	}
}

func (f DemuxFuzz) hold(t testing.TB, raw []byte, d container.Demuxer, strict bool) {
	t.Helper()
	tracks := d.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("accepted input with %d tracks", len(tracks))
	}
	if err := tracks[0].Fmt.Valid(); err != nil {
		t.Fatalf("accepted track with invalid format: %v", err)
	}
	if v := tracks[0].DemuxerVersion; v != "" && !demuxerVersionSpelling.MatchString(v) {
		t.Fatalf("track names demuxer revision %q, want <name>-demux-<n>", v)
	}
	if f.Opened != nil {
		f.Opened(t, d, strict)
	}

	maxPackets := f.MaxPackets(raw)
	var pkt container.Packet
	var end error
	// The first packet read, when one was: a stream may start before zero,
	// so no position stands for "none yet".
	var read bool
	var firstPTS int64
	for i := int64(0); ; i++ {
		if f.CapNotFatal {
			if i >= maxPackets {
				break
			}
		} else if i > maxPackets {
			t.Fatalf("more than %d packets from %d bytes", maxPackets, len(raw))
		}
		if end = d.ReadPacket(&pkt); end != nil {
			break
		}
		// A packet that holds nothing is an EOF or an error.
		if pkt.Dur < 0 || (pkt.Dur == 0 && !f.ZeroDur) {
			t.Fatalf("packet %d has duration %d", i, pkt.Dur)
		}
		if len(pkt.Data) == 0 && !f.EmptyData {
			t.Fatalf("packet %d carries no bytes", i)
		}
		if f.Packet != nil {
			f.Packet(t, &pkt)
		}
		if !read {
			read, firstPTS = true, pkt.PTS
		}
	}
	if f.Read != nil {
		f.Read(t, d, strict, end)
	}

	if len(f.Targets) > 0 {
		seeker, ok := d.(container.Seeker)
		if !ok {
			t.Fatalf("%T is given seek targets and cannot seek", d)
		}
		for _, target := range f.Targets {
			landed, err := seeker.SeekSample(context.Background(), 0, target)
			if err != nil {
				continue
			}
			if f.Landed != nil {
				f.Landed(t, d, target, landed)
			}
			if f.SeekPTS {
				// A landing in a hole is owed what the hole took.
				if err := d.ReadPacket(&pkt); err == nil && pkt.PTS-pkt.Lost != landed {
					t.Fatalf("seek to %d landed at %d, then read a packet at %d owing %d", target, landed, pkt.PTS, pkt.Lost)
				}
			}
			switch {
			case !f.LateLanding:
				if landed > target {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
			case !read:
				// Any landing is fine when no packet was ever readable: reads
				// after the seek just hit EOF.
			case landed > max(target, firstPTS):
				t.Fatalf("seek to %d landed at %d (the stream starts at %d)", target, landed, firstPTS)
			}
		}
	}
	if f.After != nil {
		f.After(t, raw, d, strict)
	}
}
