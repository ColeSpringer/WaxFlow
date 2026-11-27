package mka_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
)

// TestDamagedFilesDeliverTheirRun is what format.Media makes of a file cut
// short and of one with junk between clusters: every sample of the whole
// blocks the demuxer reads arrives once, in order, from a read and from a
// seek, and a walk settles the length to that run. The fixture's samples
// each hold their own index.
func TestDamagedFilesDeliverTheirRun(t *testing.T) {
	sized := mka.IndexPCM(t, true)
	streamed := mka.IndexPCM(t, false)
	clusterID := []byte{0x1F, 0x43, 0xB6, 0x75}
	third := 0
	for i := 0; i < 3; i++ {
		third += bytes.Index(streamed[third+1:], clusterID) + 1
	}
	junked := append(append(append([]byte(nil), streamed[:third]...), make([]byte, 300)...), streamed[third:]...)

	for _, tc := range []struct {
		name string
		file []byte
		// whole is whether every block survives; otherwise the run ends on
		// a block boundary short of the full 160000 samples.
		whole bool
	}{
		{"cut short", sized[:len(sized)/2], false},
		{"streamed and cut short", streamed[:len(streamed)/2], false},
		{"junk between clusters", junked, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open := func() format.Media {
				med, err := format.Open(container.BytesSource(tc.file), "", nil)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				return med
			}
			med := open()
			got := mustReadInts(t, med)
			med.Close()
			n := int64(len(got))
			if n == 0 || n%800 != 0 || (tc.whole && n != 160000) || (!tc.whole && n >= 160000) {
				t.Fatalf("the read delivered %d samples", n)
			}
			for i, v := range got {
				if int64(v) != int64(i) {
					t.Fatalf("sample %d holds %d", i, v)
				}
			}

			walked := open()
			defer walked.Close()
			if err := walked.(format.Walker).Walk(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tr := walked.Info().Default(); tr.Samples != n || !tr.SamplesExact {
				t.Errorf("the walk settled %d samples (exact %v), the read delivered %d", tr.Samples, tr.SamplesExact, n)
			}
			for _, target := range []int64{n / 3, n - 1} {
				seeked := open()
				landed, err := seeked.SeekSample(context.Background(), target)
				if err != nil || landed != target {
					seeked.Close()
					t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
				}
				after := mustReadInts(t, seeked)
				seeked.Close()
				if int64(len(after)) != n-target || int64(after[0]) != target {
					t.Errorf("after a seek to %d: %d samples from %d, want %d from %d", target, len(after), after[0], n-target, target)
				}
			}
		})
	}
}
