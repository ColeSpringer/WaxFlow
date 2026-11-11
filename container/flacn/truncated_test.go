package flacn_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// declaring returns raw, a native FLAC, with its STREAMINFO total set.
func declaring(t *testing.T, raw []byte, total int64) []byte {
	t.Helper()
	const siAt = 8 // "fLaC" and the block header
	out := append([]byte(nil), raw...)
	si, err := flac.ParseStreamInfo(out[siAt : siAt+flac.StreamInfoLen])
	if err != nil {
		t.Fatal(err)
	}
	si.Samples = total
	b, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	copy(out[siAt:], b)
	return out
}

// TestTruncatedFLACCountsWhatItsHeaderStillClaims: a file cut short keeps the
// total its encoder wrote, and the samples between that and where the frames
// end are counted, on the demuxer, a probe and a Media alike. A header that
// claims nothing, or less than the frames hold, counts none.
func TestTruncatedFLACCountsWhatItsHeaderStillClaims(t *testing.T) {
	raw := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, raw)
	last := frames[len(frames)-1]
	total := last.Pos + last.Dur
	k := len(frames) / 2
	kept := frames[k].Pos + frames[k].Dur // frames 0..k survive
	for _, c := range []struct {
		name string
		raw  []byte
		want int64
	}{
		{"whole", declaring(t, raw, total), 0},
		{"cut after a frame", declaring(t, raw, total)[:frames[k].End], total - kept},
		{"cut inside a frame", declaring(t, raw, total)[:frames[k+1].Off+20], total - kept},
		{"cut with no total", declaring(t, raw, 0)[:frames[k].End], 0},
		{"a total under the frames", declaring(t, raw, total-5000), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := container.BytesSource(c.raw)
			d, err := flacn.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := container.TruncatedOf(d, 0); got != c.want {
				t.Errorf("the demuxer counts %d samples truncated, want %d", got, c.want)
			}
			info, err := format.Probe(context.Background(), src, "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			if info.Truncated != c.want {
				t.Errorf("a probe counts %d samples truncated, want %d", info.Truncated, c.want)
			}
			if c.want > 0 && info.Default().Samples != kept {
				t.Errorf("a probe reports %d samples, want the %d the frames hold", info.Default().Samples, kept)
			}
			med, err := format.Open(src, "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			if got := med.Info().Truncated; got != c.want {
				t.Errorf("a Media counts %d samples truncated, want %d", got, c.want)
			}
			readSamples(t, med, -1)
			if got := med.Info().Truncated; got != c.want {
				t.Errorf("a Media counts %d samples truncated after its read, want %d", got, c.want)
			}
		})
	}
}

// TestProbeCountsItsDamage: a probe's Info counts the damage its open met, as
// a Media's does.
func TestProbeCountsItsDamage(t *testing.T) {
	raw := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, raw)
	info, err := format.Probe(context.Background(), container.BytesSource(raw[:frames[len(frames)/2].End]), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Warnings) == 0 {
		t.Fatal("a truncated file probed with no warning")
	}
	if info.Damage != len(info.Warnings) {
		t.Fatalf("Damage is %d beside %d warnings: %q", info.Damage, len(info.Warnings), info.Warnings)
	}
}
