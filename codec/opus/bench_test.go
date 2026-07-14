package opus

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The decode floor is 150x realtime per core (docs/quality-gates.md). These
// benches report the factor for the nightly trend rather than asserting the
// floor: a shared runner measures its own scheduling noise as much as the
// decoder, so the floor is triaged from the nightly numbers instead. The
// conformance vectors cover the three engines: 04 is SILK (speech), 11 is
// CELT (music), 06 is hybrid.

func benchDecodeVector(b *testing.B, n int) {
	tarPath := testutil.VectorPath(b, "opus/opus_testvectors-rfc8251.tar.gz")
	members, err := readTarGz(tarPath)
	if err != nil {
		b.Fatal(err)
	}
	bit := members[fmt.Sprintf("opus_newvectors/testvector%02d.bit", n)]
	if bit == nil {
		b.Fatalf("testvector%02d missing from archive", n)
	}
	var packets [][]byte
	off := 0
	for off+8 <= len(bit) {
		ln := int(binary.BigEndian.Uint32(bit[off:]))
		off += 8
		if ln < 0 || off+ln > len(bit) {
			break
		}
		packets = append(packets, bit[off:off+ln])
		off += ln
	}
	cfg := Config{Channels: 2, Family: 1}
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		b.Fatal(err)
	}
	defer d.Release()

	var samples int64
	emit := func(buf *audio.Buffer) error {
		samples += int64(buf.N)
		return nil
	}
	b.ResetTimer()
	for b.Loop() {
		for _, p := range packets {
			if err := d.Decode(p, emit); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	seconds := float64(samples) / SampleRate
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkDecodeSILKVector04(b *testing.B)   { benchDecodeVector(b, 4) }
func BenchmarkDecodeHybridVector06(b *testing.B) { benchDecodeVector(b, 6) }
func BenchmarkDecodeCELTVector11(b *testing.B)   { benchDecodeVector(b, 11) }
