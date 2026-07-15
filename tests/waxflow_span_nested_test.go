package waxflow_test

import (
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// TestSliceNestedHeadroom pins that a span of a span reports and delivers
// the headroom it really has.
//
// Nothing nests spans today, and the property still has to hold rather than
// happen to: headroom means "how far back can I be positioned", and an
// inner span can be positioned back through its own window and its own
// headroom both. Under-reporting it does not fail loudly. It primes a chain
// with less than it asked for and leaves a transient nobody attributes to
// this.
func TestSliceNestedHeadroom(t *testing.T) {
	const frames = 40000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 1717)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	// The inner span starts at 10000, so it has 10000 samples of headroom.
	// The outer starts 5000 into the inner, so it can reach back 15000 on
	// the file: its own 5000 plus the inner's 10000.
	inner, err := waxflow.Slice(med, 10_000, 35_000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	outer, err := waxflow.Slice(inner, 5_000, 20_000)
	if err != nil {
		inner.Close()
		t.Fatal(err)
	}
	defer outer.Close()

	h, ok := outer.(waxflow.Headroomer)
	if !ok {
		t.Fatal("a slice of a slice does not implement Headroomer")
	}
	if got := h.Headroom(); got != 15_000 {
		t.Fatalf("Headroom() = %d, want 15000: its own 5000 plus the inner span's 10000", got)
	}

	// And what it advertises, it can deliver: the advertised edge is
	// reachable, and the sample there is the file's sample 0.
	landed, err := outer.SeekSample(-15_000)
	if err != nil {
		t.Fatalf("a span refused a seek to the headroom it advertises: %v", err)
	}
	if landed != -15_000 {
		t.Fatalf("seek to -15000 landed at %d", landed)
	}
	buf := audio.Get(f, 64)
	defer audio.Put(buf)
	if err := outer.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	src := whole.ChanI(0)
	for i := range buf.N {
		if got := buf.ChanI(0)[i]; got != src[i] {
			t.Fatalf("pre-roll sample %d = %d, want the file's sample %d (%d): "+
				"the nested headroom does not address what it claims", i, got, i, src[i])
		}
	}
	// One past the advertised edge is before the file's start, and refused.
	if _, err := outer.SeekSample(-15_001); err == nil {
		t.Error("a span allowed a seek before the file's own start")
	}
}

// TestSliceShortSourcePositionIsHonest pins the floor in ensureStart, which
// looks like the one SeekSample deliberately does without.
//
// The two answer different questions. A negative seek target is a caller
// reaching into the headroom on purpose, so a negative position is the
// truth. Positioning at the window's own start is not: a format.Media lands
// on a sync point at or before the ask and then decodes forward to the ask
// exactly, so it cannot answer below the target except by running out of
// stream. When that happens the span is at its end, not somewhere in its
// own headroom, and saying so is what makes the error legible.
func TestSliceShortSourcePositionIsHonest(t *testing.T) {
	const frames = 20000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 6)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	// A media whose headers promise the full 20000 and whose audio stops at
	// 5000: a span starting past that is a cut list describing a different
	// file. Its seek answers below the target, which is the one way a
	// format.Media can, and is what ensureStart's floor is there for.
	sl, err := waxflow.Slice(&shortMedia{Media: med, at: 5_000}, 12_000, 18_000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()

	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	err = sl.ReadChunk(buf)
	if err == nil {
		t.Fatal("a span whose source ended before its window delivered samples")
	}
	// The error must count from the window's start, not from a negative
	// position: "ended 0 samples into a span that declared 6000" is legible,
	// "ended -7000 samples into" is not.
	if !strings.Contains(err.Error(), "ended 0 samples into a span that declared 6000") {
		t.Fatalf("error = %v, want it to report an honest position within the span", err)
	}
}

// shortMedia is a Media whose headers promise more audio than it holds: its
// stream really ends at at, so a seek past that lands there and reads stop.
// It is the lying-header case, which is the only way a format.Media seek
// answers below the target it was given.
type shortMedia struct {
	format.Media
	at  int64
	pos int64
}

func (m *shortMedia) SeekSample(target int64) (int64, error) {
	landed, err := m.Media.SeekSample(min(target, m.at))
	if err != nil {
		return 0, err
	}
	m.pos = landed
	return landed, nil
}

func (m *shortMedia) ReadChunk(dst *audio.Buffer) error {
	if m.pos >= m.at {
		return io.EOF
	}
	if err := m.Media.ReadChunk(dst); err != nil {
		return err
	}
	if left := m.at - m.pos; int64(dst.N) > left {
		dst.N = int(left)
	}
	m.pos += int64(dst.N)
	return nil
}
