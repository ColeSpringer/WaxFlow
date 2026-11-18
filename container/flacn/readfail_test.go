package flacn_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAFailedReadIsTheErrorOfItsCall: wherever a source goes down, the open,
// read or seek that met the failed read returns its error, and records no
// warning a source that reads does not give.
func TestAFailedReadIsTheErrorOfItsCall(t *testing.T) {
	const n, at = 1 << 18, 20
	clean := noiseStream(t, n)
	spans := frameSpans(t, clean)
	// A whole first frame, the header behind it gone, then frames whose
	// headers stand and whose bytes do not check, for more than a window.
	hole := damageFrames(t, clean, spans, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	clear(hole[spans[1][0] : spans[1][0]+8])
	// A whole frame, the header behind it gone, the next header standing and
	// junk from there to an end placed so a seek's first probe starts a byte
	// in front of the whole frame: the bisection lands on it from elsewhere.
	tail := append([]byte(nil), clean[:spans[at+2][0]+12]...)
	clear(tail[spans[at+1][0] : spans[at+1][0]+8])
	tail = append(tail, bytes.Repeat([]byte{0x11}, 2*(spans[at][0]-1-spans[0][0])+spans[0][0]-len(tail))...)

	for _, tc := range []struct {
		name    string
		raw     []byte
		damaged bool
	}{{"a clean stream", clean, false}, {"a hole behind the first frame", hole, true}, {"junk behind a whole frame", tail, true}} {
		// run opens src, reads two packets, and seeks twice with a read
		// behind each. It stops at the call that met a failed read, names
		// its kind, and says what that call got wrong.
		run := func(src container.Source, failed func() int, whole []container.Warning) (warnings []container.Warning, met, wrong string) {
			d, err := flacn.NewDemuxer(src, nil)
			if err != nil {
				if failed() == 0 || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					wrong = fmt.Sprintf("the open returned %v after %d failed reads", err, failed())
				}
				return nil, "open", wrong
			}
			if failed() > 0 {
				return nil, "open", fmt.Sprintf("the open returned nil after %d failed reads", failed())
			}
			var pkt container.Packet
			read := func() error { return d.ReadPacket(&pkt) }
			seek := func(target int64) func() error {
				return func() error { _, err := d.SeekSample(context.Background(), 0, target); return err }
			}
			for i, call := range []func() error{read, read, seek(at * 4096), read, seek(5 * 4096), read} {
				before, held := failed(), len(d.Warnings())
				err := call()
				if failed() == before {
					continue
				}
				met = []string{"read", "read", "seek", "read", "seek", "read"}[i]
				if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					wrong = fmt.Sprintf("call %d returned %v", i, err)
				}
				for _, w := range d.Warnings()[held:] {
					if !slices.Contains(whole, w) {
						wrong = fmt.Sprintf("call %d returned %v and warned %v, which a source that reads does not", i, err, w)
					}
				}
				break
			}
			return d.Warnings(), met, wrong
		}
		counted := &testutil.CountingSource{Src: container.BytesSource(tc.raw)}
		whole, _, _ := run(counted, func() int { return 0 }, nil)
		calls := map[string]int{}
		for k := 1; k < counted.Reads; k++ {
			src := &testutil.FailingSource{Src: container.BytesSource(tc.raw), From: int64(len(tc.raw)), After: k}
			_, met, wrong := run(src, func() int { return src.Failed }, whole)
			if wrong != "" {
				t.Errorf("%s, every read behind the first %d failing, the first of them %v: %s", tc.name, k, src.FailedAt, wrong)
			}
			calls[met]++
		}
		// The sweep holds something only where each kind of call met a failed
		// read, and where the damaged streams warn on a source that reads.
		if calls["open"] == 0 || calls["read"] == 0 || calls["seek"] == 0 || tc.damaged != (len(whole) > 0) {
			t.Errorf("%s: failed reads met by %v and warnings %v on a source that reads: this holds nothing", tc.name, calls, whole)
		}
	}
}
