package jobs

import (
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/waxerr"
)

// TestSplitSpans pins the cut arithmetic at its single funnel. The server
// validates cuts at creation and the runner cuts by them at run, both through
// here, so these are the rules both are held to: what a cut list may say, and
// which samples piece 3 is.
func TestSplitSpans(t *testing.T) {
	const total = 1000

	t.Run("resolved", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			cuts  []int64
			total int64
			want  [][2]int64
		}{
			// 0 is implied before the first cut and the source's end after the
			// last, so N cuts make N+1 pieces and every cut opens one.
			{"one cut makes two pieces", []int64{100}, total,
				[][2]int64{{0, 100}, {100, waxflow.ToEnd}}},
			{"each cut closes a piece and opens the next", []int64{100, 250, 900}, total,
				[][2]int64{{0, 100}, {100, 250}, {250, 900}, {900, waxflow.ToEnd}}},
			// The last piece inherits whatever the source holds rather than
			// being held to a declaration, so an undeclared length bounds only
			// the overshoot check, which cannot run.
			{"an undeclared length bounds nothing", []int64{100, 5_000_000}, -1,
				[][2]int64{{0, 100}, {100, 5_000_000}, {5_000_000, waxflow.ToEnd}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := (Request{Type: TypeSplit, Cuts: tc.cuts}).SplitSpans(tc.total)
				if err != nil {
					t.Fatalf("SplitSpans(%v, total %d): %v", tc.cuts, tc.total, err)
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("SplitSpans(%v, total %d) = %v, want %v", tc.cuts, tc.total, got, tc.want)
				}
			})
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			cuts  []int64
			total int64
		}{
			{"no cuts at all", nil, total},
			// Every cut opens a piece, so a cut that does not advance is
			// asking for an empty one, whichever way it fails to.
			{"a leading 0 asks for an empty first piece", []int64{0, 100}, total},
			{"a repeated cut asks for an empty piece", []int64{100, 100}, total},
			{"descending cuts", []int64{250, 100}, total},
			{"a negative cut", []int64{-1}, total},
			// Refused rather than clamped: a list that overshoots does not
			// describe this source.
			{"a cut at the end asks for an empty last piece", []int64{100, total}, total},
			{"a cut past the end", []int64{100, total + 1}, total},
		} {
			t.Run(tc.name, func(t *testing.T) {
				spans, err := (Request{Type: TypeSplit, Cuts: tc.cuts}).SplitSpans(tc.total)
				if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
					t.Errorf("SplitSpans(%v, total %d) error = %v, want %s",
						tc.cuts, tc.total, err, waxerr.CodeInvalidRequest)
				}
				if spans != nil {
					t.Errorf("SplitSpans(%v, total %d) returned %v beside its error", tc.cuts, tc.total, spans)
				}
			})
		}
	})
}
