package opus

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestConformanceVectors runs the RFC 6716 section 6 quality metric
// (opus_compare ported into testutil) over the 12 official test vectors at
// both decode rates (48 kHz stereo and 48 kHz mono), asserting every vector
// passes (Q >= 0). It mirrors the official run_vectors.sh procedure exactly:
// every reference file is stereo-layout; a mono decode is compared against
// the 0.5*(L+R) downmix of the reference (what opus_compare does without -s),
// and each decode passes if it matches EITHER testvectorNN.dec or the
// testvectorNNm.dec alternate (the mono-decoder reference: a mono decode of a
// stereo SILK stream is the mid channel, not the L/R average, so the two
// legitimately differ).
//
// The pinned tarball is the RFC 8251 one: identical bitstreams to the
// original 2012 RFC 6716 tarball, but with the reference decodes regenerated
// after RFC 8251's normative decoder changes, plus the mono-decoder
// references. The 2012 references are stale for the hybrid and transition
// paths: current libopus (verified with 1.6.1) fails 05/06/12 against them
// at weighted error 0.380/0.434/0.393 vs the 0.277 bar, so no spec-compliant
// modern decoder can pass those three against the old tarball. Against the
// RFC 8251 references, libopus 1.6.1 scores 99.5-100% stereo and 96.2-99.6%
// mono; this decoder is expected to land in the same band on all 12.
func TestConformanceVectors(t *testing.T) {
	if raceEnabled {
		// Single-goroutine numeric decode: the race detector adds a ~10x
		// slowdown (an hour and up) for zero concurrency coverage. CI runs
		// this test in a dedicated non-race step of the differential job.
		t.Skip("skipped under -race; run without the race detector")
	}
	tarPath := testutil.VectorPath(t, "opus/opus_testvectors-rfc8251.tar.gz")
	members, err := readTarGz(tarPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	for n := 1; n <= 12; n++ {
		name := fmt.Sprintf("testvector%02d", n)
		bit := members["opus_newvectors/"+name+".bit"]
		refS := members["opus_newvectors/"+name+".dec"]
		refM := members["opus_newvectors/"+name+"m.dec"]
		if bit == nil || refS == nil || refM == nil {
			t.Fatalf("vector %s missing from archive", name)
		}
		for _, channels := range []int{2, 1} {
			t.Run(fmt.Sprintf("%s/%dch", name, channels), func(t *testing.T) {
				got := decodeVector(t, bit, channels)
				test := make([]float32, len(got))
				for i, v := range got {
					s := math.Round(float64(v) * 32768)
					if s > 32767 {
						s = 32767
					} else if s < -32768 {
						s = -32768
					}
					test[i] = float32(s)
				}
				qS := compareRef(t, refS, test, channels)
				qM := compareRef(t, refM, test, channels)
				q := max(qS, qM)
				t.Logf("%s %dch: opus_compare Q = %.2f%% (vs .dec %.2f%%, vs m.dec %.2f%%)", name, channels, q, qS, qM)
				if q < 0 {
					t.Errorf("%s %dch FAILS conformance: Q = %.2f%% (want >= 0 vs either reference)", name, channels, q)
				}
			})
		}
	}
}

// compareRef scores a decode against one stereo-layout reference file,
// downmixing the reference for a mono decode exactly as opus_compare does.
func compareRef(t *testing.T, dec []byte, test []float32, channels int) float64 {
	t.Helper()
	pairs := len(dec) / 4
	ref := make([]float32, pairs*channels)
	for i := range pairs {
		l := float32(int16(binary.LittleEndian.Uint16(dec[4*i:])))
		r := float32(int16(binary.LittleEndian.Uint16(dec[4*i+2:])))
		if channels == 2 {
			ref[2*i], ref[2*i+1] = l, r
		} else {
			ref[i] = 0.5 * (l + r)
		}
	}
	if len(test) != len(ref) {
		t.Fatalf("sample count %d != reference %d (%dch)", len(test), len(ref), channels)
	}
	return testutil.OpusCompare(ref, test, channels)
}

// decodeVector decodes an opus_demo-format bitstream at 48 kHz with the given
// API channel count, returning interleaved float32 samples.
func decodeVector(t testing.TB, bit []byte, channels int) []float32 {
	cfg := Config{Channels: channels, Family: 1}
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	var out []float32
	off := 0
	for off+8 <= len(bit) {
		ln := int(binary.BigEndian.Uint32(bit[off:]))
		off += 8 // length + enc_final_range
		if ln < 0 || off+ln > len(bit) {
			break
		}
		pkt := bit[off : off+ln]
		off += ln
		if err := d.Decode(pkt, func(b *audio.Buffer) error {
			if channels == 1 {
				out = append(out, b.ChanF(0)...)
				return nil
			}
			l := b.ChanF(0)
			r := b.ChanF(1)
			for i := range l {
				out = append(out, l[i], r[i])
			}
			return nil
		}); err != nil {
			t.Fatalf("decode packet: %v", err)
		}
	}
	return out
}

func readTarGz(path string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	members := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		members[hdr.Name] = data
	}
	return members, nil
}
