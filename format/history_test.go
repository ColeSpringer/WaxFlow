package format

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

const historyFrame = 64

// historyDemux is a seekable run of packets, packet i carrying the byte i,
// that lands a seek one packet before the packet holding the target, as a
// counted WMA landing does. A packet listed in holes follows something the
// container dropped: it carries Discont and, like an early Windows stamp
// clamped at zero, PTS 0.
type historyDemux struct {
	n, cursor int
	holes     map[int]bool
	lands     []int
}

func (d *historyDemux) Tracks() []container.Track { return []container.Track{historyTrack} }

func (d *historyDemux) ReadPacket(p *container.Packet) error {
	if d.cursor >= d.n {
		return io.EOF
	}
	pts := int64(d.cursor * historyFrame)
	if d.holes[d.cursor] {
		pts = 0
	}
	*p = container.Packet{Packet: codec.Packet{Data: []byte{byte(d.cursor)},
		PTS: pts, Dur: historyFrame, Sync: true, Discont: d.holes[d.cursor]}}
	d.cursor++
	return nil
}

func (d *historyDemux) SeekSample(ctx context.Context, _ int, sample int64) (int64, error) {
	p := max(int(min(sample/historyFrame, int64(d.n-1)))-1, 0)
	d.lands = append(d.lands, p)
	d.cursor = p
	return int64(p * historyFrame), nil
}

var historyTrack = container.Track{
	Fmt:     audio.Format{Rate: 48000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32},
	Samples: -1, Default: true,
}

// historyDecoder emits a frame of packet i's value for packet i. A packet
// listed in needs reuses state the listed earlier packet sent: resumed past
// it, the decode asks for history, and from the start of the stream that is
// damage. A packet listed in tails opens with the end of the packet before's
// frame, which a resumed decode passes over and a decode from the start that
// never saw that packet refuses.
type historyDecoder struct {
	needs     map[int]int
	tails     map[int]bool
	seen      map[int]bool
	fromStart bool
	out       *audio.Buffer
}

func (d *historyDecoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	p := int(pkt[0])
	if d.tails[p] && d.fromStart && !d.seen[p-1] {
		return waxerr.New(waxerr.CodeMalformedInput, "a frame tail with nothing before it")
	}
	d.seen[p] = true
	if src, ok := d.needs[p]; ok && !d.seen[src] {
		if d.fromStart {
			return waxerr.New(waxerr.CodeMalformedInput, "state never sent")
		}
		return waxerr.Wrap(waxerr.CodeMalformedInput, "state sent before the landing", codec.ErrNeedsHistory)
	}
	if d.out == nil {
		d.out = audio.Get(historyTrack.Fmt, historyFrame)
	}
	d.out.N = historyFrame
	for i := range historyFrame {
		d.out.ChanF(0)[i] = float32(p)
	}
	return emit(d.out)
}

func (d *historyDecoder) Drain(func(*audio.Buffer) error) error { return nil }
func (d *historyDecoder) Reset()                                { d.seen, d.fromStart = map[int]bool{}, false }
func (d *historyDecoder) Restart()                              { d.seen, d.fromStart = map[int]bool{}, true }

// SetPosition takes the landing and nothing more: where a decode resumes
// says nothing about whether it resumed.
func (d *historyDecoder) SetPosition(int64) {}

func historyMedia(needs map[int]int, holes, tails map[int]bool) (*media, *historyDemux) {
	dm := &historyDemux{n: 24, holes: holes}
	m := &media{info: &Info{Tracks: []container.Track{historyTrack}}, demux: dm, seeker: dm,
		track: historyTrack, decoder: &historyDecoder{needs: needs, tails: tails, seen: map[int]bool{}, fromStart: true}, rawEnd: -1}
	m.stashFn = m.stash
	return m, dm
}

// readAll reads m to its end in chunks of n and returns the samples, checking
// that each chunk sits where the one before it ended.
func readAll(t *testing.T, m *media, n int) ([]float32, error) {
	t.Helper()
	buf := audio.Get(historyTrack.Fmt, n)
	defer audio.Put(buf)
	var got []float32
	next := int64(-1)
	for {
		err := m.ReadChunk(buf)
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		if next >= 0 && buf.Pos != next {
			t.Fatalf("a chunk at %d follows one that ended at %d", buf.Pos, next)
		}
		next = buf.Pos + int64(buf.N)
		got = append(got, buf.ChanF(0)[:buf.N]...)
	}
}

// TestASeekLandsEarlierWhenTheDecodeNeedsHistory: a landing can be too late
// for a decoder whose stream reuses state from further back than the
// demuxer's run-up, and a decode resumed there says so rather than play
// something else. Met before the target, the pre-roll lands earlier and
// decodes again; met after it, mid-read, the read does the same from the
// next sample it owes, so either way a seek delivers what a linear decode
// delivers. A reuse the start of the stream cannot satisfy is the damage it
// is, and one whose state lies behind a hole in the stream is not chased past
// the hole.
func TestASeekLandsEarlierWhenTheDecodeNeedsHistory(t *testing.T) {
	buf := audio.Get(historyTrack.Fmt, historyFrame)
	defer audio.Put(buf)

	m, dm := historyMedia(map[int]int{10: 5}, nil, nil)
	target := int64(10*historyFrame + historyFrame/2)
	landed, err := m.SeekSample(context.Background(), target)
	if err != nil {
		t.Fatalf("seek to %d: %v", target, err)
	}
	if landed != target {
		t.Fatalf("seek to %d landed at %d", target, landed)
	}
	if err := m.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if c := buf.ChanF(0); buf.Pos != target || c[0] != 10 || c[historyFrame/2-1] != 10 || c[historyFrame/2] != 11 {
		t.Errorf("first chunk at %d holds %v, %v, %v, want packet 10's tail at %d and then packet 11",
			buf.Pos, c[0], c[historyFrame/2-1], c[historyFrame/2], target)
	}
	if len(dm.lands) < 2 {
		t.Errorf("the demuxer was asked to land %d times; the run-up was never extended", len(dm.lands))
	}

	// The reuse after the target, read in chunks that end mid-packet so the
	// one that meets it is already part filled.
	m, dm = historyMedia(map[int]int{15: 5}, nil, nil)
	target = 12*historyFrame + 10
	if _, err := m.SeekSample(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	got, err := readAll(t, m, 150)
	if err != nil {
		t.Fatalf("a reuse met after the target failed the read: %v", err)
	}
	if want := 24*historyFrame - int(target); len(got) != want {
		t.Fatalf("the read delivered %d samples, a linear decode holds %d past %d", len(got), want, target)
	}
	for i, v := range got {
		if p := (int(target) + i) / historyFrame; v != float32(p) {
			t.Fatalf("sample %d past the target is packet %v's, want packet %d's", i, v, p)
		}
	}
	if len(dm.lands) < 2 {
		t.Errorf("the demuxer was asked to land %d times; the read never landed again", len(dm.lands))
	}

	m, _ = historyMedia(map[int]int{3: 20}, nil, nil)
	if _, err := m.SeekSample(context.Background(), 3*historyFrame+1); err == nil || errors.Is(err, codec.ErrNeedsHistory) {
		t.Errorf("state the stream never sent gave %v, want the damage the start of the stream reports", err)
	}

	// Behind a hole: every landing early enough passes the hole, which
	// restarts the decode, so no landing carries the state.
	m, dm = historyMedia(map[int]int{15: 5}, map[int]bool{10: true}, nil)
	if _, err := m.SeekSample(context.Background(), 12*historyFrame); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, m, historyFrame); !errors.Is(err, codec.ErrNeedsHistory) {
		t.Errorf("a reuse whose state lies behind a hole read on as %v, want the decoder's refusal", err)
	}
	if low := minOf(dm.lands); low < 5 {
		t.Errorf("the read landed as early as packet %d chasing state a hole at 10 had already lost", low)
	}
	// The same behind a hole the pre-roll itself passes.
	m, dm = historyMedia(map[int]int{15: 5}, map[int]bool{13: true}, nil)
	if _, err := m.SeekSample(context.Background(), 15*historyFrame+10); !errors.Is(err, codec.ErrNeedsHistory) {
		t.Errorf("a seek whose state lies behind a hole gave %v, want the decoder's refusal", err)
	}
	if low := minOf(dm.lands); low < 5 {
		t.Errorf("the pre-roll landed as early as packet %d chasing state a hole at 13 had already lost", low)
	}

	// A decoder with no Restart is resumed at a landing on the first packet
	// too, and a read that meets a reuse there has nothing earlier to try.
	m, _ = historyMedia(map[int]int{3: 20}, nil, nil)
	m.decoder = resetOnly{m.decoder.(*historyDecoder)}
	if _, err := m.SeekSample(context.Background(), historyFrame+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, m, historyFrame); !errors.Is(err, codec.ErrNeedsHistory) {
		t.Errorf("a reuse after a landing on the first packet read on as %v, want the decoder's refusal", err)
	}
}

// resetOnly is a historyDecoder without Restart.
type resetOnly struct{ d *historyDecoder }

func (r resetOnly) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	return r.d.Decode(pkt, emit)
}
func (r resetOnly) Drain(emit func(*audio.Buffer) error) error { return r.d.Drain(emit) }
func (r resetOnly) Reset()                                     { r.d.Reset() }

func minOf(s []int) int {
	low := s[0]
	for _, v := range s {
		low = min(low, v)
	}
	return low
}

// TestAHoleStampedZeroIsNotTheStart: a packet after a hole restarts the
// decoder, and the stamp it carries says where, not whether the decode is the
// stream's own from its start. An early Windows stamp clamps to 0 well into a
// file, so a decode told it resumes at 0 after a hole has to stay a resumed
// one, which reads a frame tail from the packet it lost as a landing does.
func TestAHoleStampedZeroIsNotTheStart(t *testing.T) {
	m, _ := historyMedia(nil, map[int]bool{3: true}, map[int]bool{3: true})
	got, err := readAll(t, m, historyFrame)
	if err != nil {
		t.Fatalf("a hole stamped 0 failed the read: %v", err)
	}
	if len(got) != 24*historyFrame {
		t.Errorf("the read delivered %d samples, want %d", len(got), 24*historyFrame)
	}
}
