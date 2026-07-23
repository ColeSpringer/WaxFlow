package server

import (
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// stubMedia is a Media whose declared total and true audio length can
// disagree, which no honest in-tree fixture does: WAV, FLAC, and MP3 all
// declare what they measure, so the advisory-gap behavior sliceMeasured
// exists for (Matroska's total can sit up to a millisecond under the audio)
// is only reachable by construction. Only Info and Close carry weight here;
// the disease is at open, where Slice re-derives its bound from the
// declaration, so nothing needs to read samples.
type stubMedia struct {
	info   *format.Info
	closed bool
}

func (m *stubMedia) Info() *format.Info                { return m.info }
func (m *stubMedia) ReadChunk(*audio.Buffer) error     { return io.EOF }
func (m *stubMedia) SeekSample(t int64) (int64, error) { return t, nil }
func (m *stubMedia) Close() error                      { m.closed = true; return nil }

func advisoryMedia(declared int64) *stubMedia {
	return &stubMedia{info: &format.Info{
		Container: "test",
		Tracks: []container.Track{{
			Fmt: audio.Format{Rate: 48000, Channels: 2,
				Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16},
			Samples: declared,
			Default: true,
		}},
	}}
}

// TestSliceMeasuredReanchorsAdvisoryDeclarations pins the property that keeps
// a windowed member servable on an advisory container: the mint and the plan
// validate a window against the measured total, and the run must slice
// against the same number, not the freshly opened header's shorter advisory
// one. Without the re-anchor, a window edge between the advisory declaration
// and the measure refuses at every open, so a timeline the mint accepted
// mints its playlist and then fails every segment, permanently.
func TestSliceMeasuredReanchorsAdvisoryDeclarations(t *testing.T) {
	const declared, measured = 99000, 100000

	// First, the disease, so this test fails honestly if Slice ever learns
	// the measure on its own: a bare Slice refuses the window the plan
	// accepted, because it can only see the advisory declaration.
	bare := advisoryMedia(declared)
	if _, err := waxflow.Slice(bare, 98000, 99500); err == nil {
		t.Fatal("a bare Slice accepted a window past the advisory declaration; " +
			"if Slice now validates against a measure, sliceMeasured may be redundant")
	}

	t.Run("a bounded window edge inside the advisory gap opens", func(t *testing.T) {
		med := advisoryMedia(declared)
		sl, err := sliceMeasured(med, span{from: 98000, to: 99500}, measured)
		if err != nil {
			t.Fatalf("sliceMeasured refused a window the plan validated: %v", err)
		}
		defer sl.Close()
		if got := sl.Info().Default().Samples; got != 1500 {
			t.Fatalf("the slice declares %d samples, want the window's 1500", got)
		}
	})

	t.Run("an open end starting inside the advisory gap opens", func(t *testing.T) {
		med := advisoryMedia(declared)
		sl, err := sliceMeasured(med, span{from: 99200}, measured)
		if err != nil {
			t.Fatalf("sliceMeasured refused an open window the plan validated: %v", err)
		}
		defer sl.Close()
		// measured - from, not declared - from: the open end inherits the
		// measure, which is what keeps the slice's declaration equal to the
		// recorded member track a concat holds delivery to.
		if got := sl.Info().Default().Samples; got != 800 {
			t.Fatalf("the open slice declares %d samples, want the measure's 800", got)
		}
	})

	t.Run("the bound still holds against the measure", func(t *testing.T) {
		med := advisoryMedia(declared)
		_, err := sliceMeasured(med, span{from: 0, to: measured + 1}, measured)
		if err == nil || !strings.Contains(err.Error(), "past the source's") {
			t.Fatalf("a window past the measure was not refused: %v", err)
		}
		if !med.closed {
			t.Fatal("sliceMeasured leaked the media on a refused window; " +
				"Slice took nothing, so the caller's close duty stands")
		}
	})

	t.Run("a matching declaration passes through", func(t *testing.T) {
		med := advisoryMedia(measured)
		sl, err := sliceMeasured(med, span{from: 98000, to: 99500}, measured)
		if err != nil {
			t.Fatalf("sliceMeasured refused an already-honest declaration: %v", err)
		}
		defer sl.Close()
		if got := sl.Info().Default().Samples; got != 1500 {
			t.Fatalf("the slice declares %d samples, want 1500", got)
		}
	})

	t.Run("an unknown measure leaves the declaration alone", func(t *testing.T) {
		med := advisoryMedia(declared)
		sl, err := sliceMeasured(med, span{from: 1000}, -1)
		if err != nil {
			t.Fatalf("sliceMeasured with no measure failed: %v", err)
		}
		defer sl.Close()
		if got := sl.Info().Default().Samples; got != declared-1000 {
			t.Fatalf("the slice declares %d samples, want the declaration's %d", got, declared-1000)
		}
	})
}
