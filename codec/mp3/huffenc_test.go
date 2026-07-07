package mp3

import "testing"

// TestHuffEncodeRoundTrip checks that every codeword the encoder derives
// from the decode trees decodes back to the same pair, and that the bit
// count writePair produces matches what pairBits predicts. This validates
// the tree-walk derivation against the decoder directly.
func TestHuffEncodeRoundTrip(t *testing.T) {
	for _, tbl := range bigCandidates {
		lim := tableLimit(tbl)
		// Exhaustively cover the no-escape range and the boundary at 15;
		// beyond that the linbits escape is linear, so a sampling up to the
		// table maximum exercises it without O(lim^2) blowup.
		vals := escapeValues(lim)
		for _, x := range vals {
			for _, y := range vals {
				for _, sx := range []int{1, -1} {
					for _, sy := range []int{1, -1} {
						vx, vy := x*sx, y*sy
						var w bitWriter
						w.writePair(tbl, vx, vy)
						want := pairBits(tbl, x, y)
						if want < 0 {
							t.Fatalf("table %d cannot code (%d,%d) but tableLimit says %d", tbl, x, y, lim)
						}
						if w.bitLen() != want {
							t.Fatalf("table %d pair (%d,%d): wrote %d bits, pairBits says %d", tbl, vx, vy, w.bitLen(), want)
						}
						w.align()
						r := bitReader{data: w.buf}
						gx, gy := decodePair(&r, tbl)
						if int(gx) != vx || int(gy) != vy {
							t.Fatalf("table %d pair (%d,%d) round-tripped to (%d,%d)", tbl, vx, vy, gx, gy)
						}
					}
				}
			}
		}
	}
}

// escapeValues returns the values to test for a table whose limit is lim:
// everything up to the escape boundary, then a sampling toward lim.
func escapeValues(lim int) []int {
	var v []int
	for i := 0; i <= min(lim, 16); i++ {
		v = append(v, i)
	}
	for _, x := range []int{31, 100, 1000, lim - 1, lim} {
		if x > 16 && x <= lim {
			v = append(v, x)
		}
	}
	return v
}

// TestHuffCount1RoundTrip checks the count1 quad tables the same way.
func TestHuffCount1RoundTrip(t *testing.T) {
	for sel := 0; sel < 2; sel++ {
		for i := 0; i < 16; i++ {
			// Expand the 4-bit magnitude pattern with every sign combo.
			mags := [4]int{i >> 3 & 1, i >> 2 & 1, i >> 1 & 1, i & 1}
			for signs := 0; signs < 16; signs++ {
				var vals [4]int
				for k := 0; k < 4; k++ {
					vals[k] = mags[k]
					if mags[k] != 0 && signs>>k&1 != 0 {
						vals[k] = -vals[k]
					}
				}
				var w bitWriter
				w.writeQuad(sel, vals[0], vals[1], vals[2], vals[3])
				if got := w.bitLen(); got != quadBits(sel, vals[0], vals[1], vals[2], vals[3]) {
					t.Fatalf("count1 table %d quad %v: wrote %d bits, quadBits disagrees", sel, vals, got)
				}
				w.align()
				r := bitReader{data: w.buf}
				gv, gw, gx, gy := decodeQuad(&r, sel == 1)
				if int(gv) != vals[0] || int(gw) != vals[1] || int(gx) != vals[2] || int(gy) != vals[3] {
					t.Fatalf("count1 table %d quad %v round-tripped to (%d,%d,%d,%d)", sel, vals, gv, gw, gx, gy)
				}
			}
		}
	}
}
