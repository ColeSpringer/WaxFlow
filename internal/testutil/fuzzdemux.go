package testutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
// seek that errors all pass: the bytes are arbitrary. An open that states its
// length exact keeps the claim through the read: that length and the samples
// it counts truncated (container.Truncated) sum the same after it.
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
	// Walk holds the walk (container.Walker), after the read and the seeks
	// and on a fresh open: Walked holds after a nil Walk, a second Walk
	// answers the same, and an open walked to nil reads as one never walked.
	Walk bool
	// Restore holds the index snapshot (container.Indexer) taken once the
	// seeks and any walk are done: restored into a fresh open, it lands every
	// target where the demuxer it came from does. A nil snapshot passes.
	Restore bool
	// ExactLength holds a track that says SamplesExact once a tolerant read
	// reaches io.EOF to its packets: their durations and what holes took,
	// less the trims it states and never below 0. A read that found
	// otherwise stops saying so.
	ExactLength bool
	// OpenLength holds the length the open called exact to the same packets,
	// whatever the track says after the read: a shortfall is what the read
	// counts truncated, and more passes only where LongerWhen answers true.
	OpenLength bool
	// LongerWhen names the one way a read may deliver more than the open
	// called exact; it is asked of the demuxer after the read.
	LongerWhen func(d container.Demuxer) bool
	// LengthOn scopes ExactLength and OpenLength to the tracks it answers
	// true for, as the open states them; nil holds every track.
	LengthOn func(tr container.Track) bool

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

	// What an exact open claims: the samples there and those it counts
	// truncated past them. A read may settle some of the first into the
	// second, and the claim holds through it.
	opened := d.Tracks()[0]
	claim := opened.Samples + container.TruncatedOf(d, opened.ID)

	maxPackets := f.MaxPackets(raw)
	var pkt container.Packet
	var end error
	// The first packet read, when one was: a stream may start before zero,
	// so no position stands for "none yet".
	var read bool
	var firstPTS int64
	// What the packets hold: their durations and the samples holes took.
	var durs, lost int64
	damage := container.DamageOf(d)
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
		durs, lost = durs+pkt.Dur, lost+pkt.Lost
	}
	if tr := d.Tracks()[0]; opened.SamplesExact {
		if trunc := container.TruncatedOf(d, tr.ID); tr.Samples+trunc != claim {
			t.Fatalf("the open claimed %d samples (%d there, %d truncated), after the read %d (%d there, %d truncated)",
				claim, opened.Samples, claim-opened.Samples, tr.Samples+trunc, tr.Samples, trunc)
		}
	}
	lengths := f.LengthOn == nil || f.LengthOn(opened)
	if tr := d.Tracks()[0]; f.ExactLength && lengths && !strict && end == io.EOF && tr.SamplesExact {
		if want := max(durs+lost-tr.Delay-tr.Padding-tr.MidPadding, 0); tr.Samples != want {
			t.Fatalf("the track says %d samples exact, its packets hold %d (durations %d, lost %d, less delay %d, padding %d, mid padding %d, damage %d in the read)",
				tr.Samples, want, durs, lost, tr.Delay, tr.Padding, tr.MidPadding, container.DamageOf(d)-damage)
		}
	}
	if f.OpenLength && lengths && !strict && end == io.EOF && opened.SamplesExact {
		held := max(durs+lost-opened.Delay-opened.Padding-opened.MidPadding, 0)
		counted := container.TruncatedOf(d, opened.ID) - (claim - opened.Samples)
		switch {
		case held < opened.Samples && opened.Samples-held != counted:
			t.Fatalf("the open called %d samples exact, its packets hold %d, and the read counted %d truncated", opened.Samples, held, counted)
		case held > opened.Samples && (f.LongerWhen == nil || !f.LongerWhen(d)):
			t.Fatalf("the open called %d samples exact and its packets hold %d (durations %d, lost %d)", opened.Samples, held, durs, lost)
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
	if f.Walk {
		walked(t, d)
		w, plain := f.reopen(t, raw, strict), f.reopen(t, raw, strict)
		if walked(t, w) == nil {
			sameReads(t, w, plain, maxPackets)
		}
	}
	if f.Restore {
		f.restored(t, raw, d, strict)
	}
	if f.After != nil {
		f.After(t, raw, d, strict)
	}
}

// reopen opens raw again as hold's demuxer was opened.
func (f DemuxFuzz) reopen(t testing.TB, raw []byte, strict bool) container.Demuxer {
	t.Helper()
	d, err := f.Open(container.BytesSource(raw), strict)
	if err != nil {
		t.Fatalf("a second open of the same bytes failed: %v", err)
	}
	return d
}

// restored restores d's index snapshot into a fresh open of raw and holds
// every target to land there as it lands on d, error or not.
func (f DemuxFuzz) restored(t testing.TB, raw []byte, d container.Demuxer, strict bool) {
	t.Helper()
	ix, ok := d.(container.Indexer)
	if !ok {
		t.Fatalf("%T is told to restore and keeps no index", d)
	}
	if len(f.Targets) == 0 {
		t.Fatal("told to restore and given no seek targets to land")
	}
	blob := ix.IndexSnapshot()
	if blob == nil {
		return
	}
	e := f.reopen(t, raw, strict)
	e.(container.Indexer).RestoreIndex(blob) // accepted or refused, it lands the same
	for _, target := range f.Targets {
		want, errD := d.(container.Seeker).SeekSample(context.Background(), 0, target)
		got, errE := e.(container.Seeker).SeekSample(context.Background(), 0, target)
		if (errD == nil) != (errE == nil) || got != want {
			t.Fatalf("seek to %d: the restored open landed at %d (%v), the one its snapshot came from at %d (%v)",
				target, got, errE, want, errD)
		}
	}
}

// walked walks d twice and returns the first answer: a nil one leaves d
// walked, and the second answers the same.
func walked(t testing.TB, d container.Demuxer) error {
	t.Helper()
	w, ok := d.(container.Walker)
	if !ok {
		t.Fatalf("%T is told to walk and cannot walk", d)
	}
	err := w.Walk(context.Background())
	if err == nil && !w.Walked() {
		t.Fatal("a walk that returned nil is not walked")
	}
	if again := w.Walk(context.Background()); (again == nil) != (err == nil) || (err != nil && again.Error() != err.Error()) {
		t.Fatalf("a second walk answered %v, the first %v", again, err)
	}
	return err
}

// sameReads reads walked and plain in step, to their ends or one packet
// past the cap, and holds every packet and the end to be the same.
func sameReads(t testing.TB, walked, plain container.Demuxer, maxPackets int64) {
	t.Helper()
	var a, b container.Packet
	for i := int64(0); i <= maxPackets; i++ {
		errA, errB := walked.ReadPacket(&a), plain.ReadPacket(&b)
		if (errA == nil) != (errB == nil) || (errA == io.EOF) != (errB == io.EOF) {
			t.Fatalf("packet %d: a read after a walk answered %v, one without %v", i, errA, errB)
		}
		if errA != nil {
			return
		}
		if a.Track != b.Track || a.Padding != b.Padding || a.Lost != b.Lost || a.PTS != b.PTS || a.Dur != b.Dur ||
			a.Sync != b.Sync || a.Discont != b.Discont || !bytes.Equal(a.Data, b.Data) {
			t.Fatalf("packet %d: a read after a walk delivered %s, one without %s", i, describe(a), describe(b))
		}
	}
}

func describe(p container.Packet) string {
	return fmt.Sprintf("{track %d PTS %d dur %d lost %d padding %d sync %v discont %v, %d bytes}",
		p.Track, p.PTS, p.Dur, p.Lost, p.Padding, p.Sync, p.Discont, len(p.Data))
}
