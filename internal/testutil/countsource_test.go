package testutil

import (
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestReadsOnceInOrderFailsAReadThatRepeatsOrSkips: a read of [2, 10) in
// three-byte steps passes, and the same read with each step read twice, or
// with a byte skipped, is reported.
func TestReadsOnceInOrderFailsAReadThatRepeatsOrSkips(t *testing.T) {
	data := container.BytesSource([]byte("0123456789"))
	reader := func(src *CountingSource, times int, gap int64) func(*container.Packet) error {
		off := int64(2)
		return func(pkt *container.Packet) error {
			if off >= 10 {
				return io.EOF
			}
			p := make([]byte, min(3, 10-off))
			for range times {
				if _, err := src.ReadAt(p, off); err != nil {
					return err
				}
			}
			off += int64(len(p)) + gap
			pkt.Data = p
			return nil
		}
	}
	for _, tc := range []struct {
		name       string
		times      int
		gap        int64
		n          int
		wantReport string
	}{
		{"once, in order", 1, 0, 3, ""},
		{"each step twice", 2, 0, 3, "read 16 bytes in 6 reads, want 8 in 3"},
		{"a byte skipped", 1, 1, 3, "read 1 begins at 6, the one before it ended at 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &CountingSource{Src: data}
			src.ReadAt(make([]byte, 2), 0) // what an open would read
			rec := &errTB{TB: t}
			ReadsOnceInOrder(rec, src, reader(src, tc.times, tc.gap), 2, 10, tc.n)
			switch {
			case tc.wantReport == "" && len(rec.errs) != 0:
				t.Errorf("reported %q for a read of the bytes once, in order", rec.errs)
			case tc.wantReport != "" && !strings.Contains(strings.Join(rec.errs, "\n"), tc.wantReport):
				t.Errorf("reported %q, want a report holding %q", rec.errs, tc.wantReport)
			}
		})
	}
}
