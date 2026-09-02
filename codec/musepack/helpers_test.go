package musepack_test

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
)

// repoPath resolves a path relative to the repository root.
func repoPath(rel ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, rel...)...)
}

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func intFormat(rate, channels int) audio.Format {
	return audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Int, BitDepth: 16}
}

// interleave flattens a planar int buffer and returns it to the pool.
func interleave(b *audio.Buffer) []int32 {
	defer audio.Put(b)
	ch := b.Fmt.Channels
	out := make([]int32, b.N*ch)
	for c := range ch {
		for i, v := range b.ChanI(c) {
			out[i*ch+c] = v
		}
	}
	return out
}

// scaled returns a signal at a fraction of full scale: every fixture stays
// below -3 dBFS so no clip ever fires in an encoder.
func scaled(samps []int32, amp float64) []int32 {
	for i, v := range samps {
		samps[i] = int32(math.Round(float64(v) * amp))
	}
	return samps
}

// demuxAll returns a stream's config, track, and decoder packets, from the
// demuxer that builds them in production (the realignment of SV7 frames is a
// container rule the codec tests must not restate).
func demuxAll(t testing.TB, raw []byte) (musepack.Config, container.Track, [][]byte) {
	t.Helper()
	d, err := mpc.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	track := d.Tracks()[0]
	cfg, err := musepack.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return cfg, track, out
		}
		if err != nil {
			t.Fatalf("packet %d: %v", len(out), err)
		}
		out = append(out, append([]byte(nil), pkt.Data...))
	}
}

// decodePackets decodes packets with a fresh decoder and returns every sample
// the decoder emitted, interleaved: the raw timeline, before the container's
// delay and tail trims.
func decodePackets(t testing.TB, cfg musepack.Config, pkts [][]byte) []float32 {
	t.Helper()
	dec, err := musepack.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, testutil.InterleaveF(b)...)
		return nil
	}
	for i, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

// decodeFile is demuxAll then decodePackets.
func decodeFile(t testing.TB, raw []byte) (musepack.Config, container.Track, []float32) {
	t.Helper()
	cfg, track, pkts := demuxAll(t, raw)
	return cfg, track, decodePackets(t, cfg, pkts)
}

// trimmed returns the delivered timeline of a raw decode: the container's
// Delay dropped from the front, Samples kept.
func trimmed(t testing.TB, track container.Track, raw []float32) []float32 {
	t.Helper()
	ch := track.Fmt.Channels
	from := int(track.Delay) * ch
	to := from + int(track.Samples)*ch
	if track.Samples < 0 || to > len(raw) {
		t.Fatalf("trim [%d:%d) does not fit a %d-sample decode", from, to, len(raw))
	}
	return raw[from:to]
}

// diffStats summarizes a float comparison in full-scale units: the largest
// difference, its RMS, and how many samples agree exactly.
type diffStats struct {
	n     int
	max   float64
	maxAt int
	rms   float64
	exact float64
}

func compare(a, b []float32) diffStats {
	d := diffStats{n: min(len(a), len(b))}
	var sum float64
	same := 0
	for i := 0; i < d.n; i++ {
		diff := math.Abs(float64(a[i]) - float64(b[i]))
		sum += diff * diff
		if diff > d.max {
			d.max, d.maxAt = diff, i
		}
		if a[i] == b[i] {
			same++
		}
	}
	if d.n > 0 {
		d.rms = math.Sqrt(sum / float64(d.n))
		d.exact = float64(same) / float64(d.n)
	}
	return d
}
