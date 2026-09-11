package asf_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/format"
)

// A dropped media object is a hole, and WMA Voice is the first codec in this
// container that cannot tell: it carries the head of a superframe from one
// object into the next, and the spillover bits at the top of the object after
// a hole finish a superframe whose head went with it. Read past in non-strict
// mode, the hole has to reach the decoder as a discontinuity on the next
// packet, so the format layer restarts it there and tells it where the
// packet lands, and the decode goes on as a resume rather than as a splice.
//
// The file is the 16 kHz 16 kbit/s corpus cell's own packets, laid out again by
// the synthetic builder with one object's size declared as zero, which the
// demuxer drops with a warning.

const voiceGapCell = "voice-16000-16k"

// voiceFile lays the cell's packets into a synthetic file, with object hole's
// size declared as zero when hole is not negative.
func voiceFile(t *testing.T, hole int) (raw []byte, rate int) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "codec", "wmavoice", "testdata", "corpus", voiceGapCell+".wma"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := asf.NewDemuxer(container.BytesSource(src), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	track := d.Tracks()[0]
	rate = track.Fmt.Rate
	b := newBuilder()
	b.wfx = append([]byte(nil), track.CodecConfig...)
	b.packetLen = 700
	for i := 0; ; i++ {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ms := uint32(pkt.PTS * 1000 / int64(rate))
		size := uint32(len(pkt.Data))
		if i == hole {
			size = 0
		}
		b.packet(ms, byte(i), 0, size, uint32(b.prerollMS)+ms, append([]byte(nil), pkt.Data...))
	}
	return b.build(), rate
}

func decodeThrough(t *testing.T, raw []byte) ([]float32, error) {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "", nil)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	var out []float32
	for {
		err := med.ReadChunk(buf)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, buf.ChanF(0)[:buf.N]...)
	}
}

func TestADroppedObjectIsADiscontinuity(t *testing.T) {
	const hole = 5
	raw, _ := voiceFile(t, hole)

	t.Run("strict refuses it", func(t *testing.T) {
		d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
		if err == nil {
			err = readToErr(d)
		}
		if err == nil || !strings.Contains(err.Error(), "media object of 0 bytes") {
			t.Fatalf("error %v, want the empty object named", err)
		}
	})

	t.Run("the packet after the hole says so", func(t *testing.T) {
		d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []container.Packet
		for {
			var pkt container.Packet
			err := d.ReadPacket(&pkt)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, pkt)
		}
		if len(got) != 13 {
			t.Fatalf("%d packets, want the cell's 14 less the hole", len(got))
		}
		for i, p := range got {
			if want := i == hole; p.Discont != want {
				t.Errorf("packet %d: Discont %v, want %v", i, p.Discont, want)
			}
		}
		warned := false
		for _, w := range d.Warnings() {
			warned = warned || strings.Contains(w.Msg, "media object of 0 bytes")
		}
		if !warned {
			t.Errorf("the drop was not warned about: %v", d.Warnings())
		}
	})
}

func TestADecodeReadsPastAHoleAsAResume(t *testing.T) {
	const hole = 5
	clean, _ := voiceFile(t, -1)
	ref, err := decodeThrough(t, clean)
	if err != nil {
		t.Fatalf("the undamaged file: %v", err)
	}
	// What the hole costs: the superframes decoded while the dropped object
	// is consumed (the one it finishes and its own body) and the one it
	// carried into the next object, whose spillover is skipped unread.
	per := superframesPerObject(t, clean)
	lost := per[hole] + 1

	damaged, _ := voiceFile(t, hole)
	got, err := decodeThrough(t, damaged)
	if err != nil {
		t.Fatalf("the damaged file: %v", err)
	}
	const superframe = 480
	if want := len(ref) - lost*superframe; len(got) != want {
		t.Fatalf("decoded %d samples, want the linear %d less the hole's %d superframes (%d)", len(got), len(ref), lost, want)
	}
	before := 0
	for _, n := range per[:hole] {
		before += n
	}
	at := before * superframe
	g, r := got[at:], ref[at+lost*superframe:]
	last := -1
	for i := range min(len(g), len(r)) {
		if g[i] != r[i] {
			last = i
		}
	}
	if last >= min(len(g), len(r))-1 {
		t.Fatalf("the decode after the hole never matches the linear one")
	}
	t.Logf("after the hole the decode is exact from superframe %d on", last/superframe+1)
}

// superframesPerObject counts, per object, the superframes the undamaged
// file's decoder emits while consuming it, through the demuxer and the codec
// directly: the format layer buffers and would blur the object boundary.
func superframesPerObject(t *testing.T, raw []byte) []int {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	track := d.Tracks()[0]
	cfg, err := wmavoice.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmavoice.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	var per []int
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return per
		}
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if err := dec.Decode(pkt.Data, func(*audio.Buffer) error { n++; return nil }); err != nil {
			t.Fatalf("object %d: %v", len(per), err)
		}
		per = append(per, n)
	}
}
