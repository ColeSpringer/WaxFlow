package flacn_test

import (
	"context"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// holed is a native FLAC with the named frames damaged, opened as a Media,
// and where each of those frames sat in the clean file.
func holed(t *testing.T, seconds int, pick func(frames int) []int) (format.Media, [][2]int64) {
	t.Helper()
	raw := unseekableTwin(t, seconds)
	frames := testutil.FLACFrames(t, raw)
	ks := pick(len(frames))
	holes := make([][2]int64, len(ks))
	for i, k := range ks {
		holes[i] = [2]int64{frames[k].Pos, frames[k].Pos + frames[k].Dur}
	}
	med, err := format.Open(container.BytesSource(testutil.DamageFLACFrames(t, raw, frames, ks...)), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { med.Close() })
	return med, holes
}

// readSamples reads up to n samples of med, all of what is left when n is
// negative, and returns how many it delivered.
func readSamples(t *testing.T, med format.Media, n int64) int64 {
	t.Helper()
	f := med.Info().Default().Fmt
	var got int64
	for n < 0 || got < n {
		want := int64(audio.StandardChunk)
		if n >= 0 {
			want = min(want, n-got)
		}
		buf := audio.Get(f, int(want))
		err := med.ReadChunk(buf)
		got += int64(buf.N)
		audio.Put(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return got
}

// TestReadCountsTheSilenceItDelivers: Info.Lost is the silence a read
// delivered where damage took the audio, every hole's, and none for a file
// with no hole.
func TestReadCountsTheSilenceItDelivers(t *testing.T) {
	med, holes := holed(t, 4, func(n int) []int { return []int{n / 3, 2 * n / 3} })
	if got := med.Info().Lost; got != 0 {
		t.Fatalf("Lost is %d before any read", got)
	}
	readSamples(t, med, -1)
	want := holes[0][1] - holes[0][0] + holes[1][1] - holes[1][0]
	if got := med.Info().Lost; got != want {
		t.Fatalf("Lost is %d after the read, want the two holes' %d", got, want)
	}

	whole, _ := holed(t, 4, func(int) []int { return nil })
	readSamples(t, whole, -1)
	if got := whole.Info().Lost; got != 0 {
		t.Fatalf("Lost is %d for a file with no hole", got)
	}
}

// TestLostIsAsOfTheLastRead: a read that stops inside a hole has delivered
// that much of it and no more.
func TestLostIsAsOfTheLastRead(t *testing.T) {
	med, holes := holed(t, 4, func(n int) []int { return []int{n / 2} })
	const into = 100
	readSamples(t, med, holes[0][0]+into)
	if got := med.Info().Lost; got != into {
		t.Fatalf("Lost is %d, want the %d samples of the hole delivered so far", got, into)
	}
	readSamples(t, med, -1)
	if got, want := med.Info().Lost, holes[0][1]-holes[0][0]; got != want {
		t.Fatalf("Lost is %d at the end, want the hole's %d", got, want)
	}
}

// TestSeekPrerollIsNotLost: silence a seek decodes and discards on the way to
// its target was never delivered. A seek into a hole lands before it, so what
// counts is the part of the hole from the target on.
func TestSeekPrerollIsNotLost(t *testing.T) {
	med, holes := holed(t, 4, func(n int) []int { return []int{n / 2} })
	lo, hi := holes[0][0], holes[0][1]
	const into = 1000
	if _, err := med.SeekSample(context.Background(), lo+into); err != nil {
		t.Fatal(err)
	}
	if got := med.Info().Lost; got != 0 {
		t.Fatalf("Lost is %d after a seek that delivered nothing", got)
	}
	readSamples(t, med, -1)
	if got, want := med.Info().Lost, hi-lo-into; got != want {
		t.Fatalf("Lost is %d, want the %d samples of the hole past the target", got, want)
	}

	past, holes := holed(t, 4, func(n int) []int { return []int{n / 2} })
	if _, err := past.SeekSample(context.Background(), holes[0][1]+17); err != nil {
		t.Fatal(err)
	}
	readSamples(t, past, -1)
	if got := past.Info().Lost; got != 0 {
		t.Fatalf("Lost is %d for a read that began past the hole", got)
	}
}

// TestLostCountsADeliveryNotAPosition: a hole read twice was delivered twice.
func TestLostCountsADeliveryNotAPosition(t *testing.T) {
	med, holes := holed(t, 4, func(n int) []int { return []int{n / 2} })
	readSamples(t, med, -1)
	if _, err := med.SeekSample(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	readSamples(t, med, -1)
	if got, want := med.Info().Lost, 2*(holes[0][1]-holes[0][0]); got != want {
		t.Fatalf("Lost is %d after two reads of the hole, want %d", got, want)
	}
}

// failOnceDemuxer fails one ReadPacket and reads on after it, as a source
// read that fails once does.
type failOnceDemuxer struct {
	*flacn.Demuxer
	reads, failAt int
}

func (d *failOnceDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.reads++; d.reads == d.failAt {
		return waxerr.New(waxerr.CodeSourceUnreadable, "a read that fails once")
	}
	return d.Demuxer.ReadPacket(pkt)
}

// TestLostStaysOnTheTimelineAFailedReadLeaves: a read that fails drops the
// frames it had decoded and stamps the next chunk where the failed one would
// have been. The count follows the chunks, so a hole after the failure is
// counted with the chunk that carries its silence out, not a chunk later by
// however many frames the failure dropped.
func TestLostStaysOnTheTimelineAFailedReadLeaves(t *testing.T) {
	raw := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, raw)
	k := len(frames) / 2
	lo, hi := frames[k].Pos, frames[k].Pos+frames[k].Dur
	d, err := flacn.NewDemuxer(container.BytesSource(testutil.DamageFLACFrames(t, raw, frames, k)), nil)
	if err != nil {
		t.Fatal(err)
	}
	med, err := format.FromDemuxer("flac", &failOnceDemuxer{Demuxer: d, failAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	// One frame decodes into the chunk, the read of the second fails, and the
	// first is gone with the chunk.
	dropped := frames[0].Dur
	buf := audio.Get(med.Info().Default().Fmt, int(dropped)+1000)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("the read returned %v, want the failure", err)
	}
	// The stream reads on from its second frame, stamped from 0. Once the
	// chunks delivered reach the end of the hole's silence, all of it has
	// been delivered.
	got := readSamples(t, med, hi-dropped)
	if got != hi-dropped {
		t.Fatalf("read %d samples, want %d", got, hi-dropped)
	}
	if lost := med.Info().Lost; lost != hi-lo {
		t.Fatalf("Lost is %d once the hole's silence has been delivered, want %d", lost, hi-lo)
	}
	readSamples(t, med, -1)
	if lost := med.Info().Lost; lost != hi-lo {
		t.Fatalf("Lost is %d at the end, want %d", lost, hi-lo)
	}
}
