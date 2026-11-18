package srcwin

import (
	"bytes"
	"testing"
)

// FuzzScanMatchesNaive holds Scan to a byte-at-a-time search of tile repeated
// to size, times 1 to 8: the same candidates, views and answer. A verdict byte
// answers a candidate; 0x80 has try read ahead, 0x04 past the window's end.
func FuzzScanMatchesNaive(f *testing.F) {
	f.Add([]byte{0, 0, 0xFF, 0}, []byte{0xFF}, uint16(8192), uint16(0), uint16(8192), uint16(4), uint8(0), []byte{2})
	f.Add([]byte("....wvpk.."), []byte("wvpk"), uint16(40000), uint16(7), uint16(39000), uint16(32), uint8(0), []byte{2, 0x82, 2})
	f.Add([]byte("MAC"), []byte("MAC"), uint16(3075), uint16(1000), uint16(65535), uint16(4), uint8(0), []byte{0xF2, 2, 2, 2, 0})
	f.Add([]byte("aaab"), []byte("aa"), uint16(1026), uint16(1020), uint16(1025), uint16(1200), uint8(0), []byte{2, 2, 1})
	// The data ends inside the view of a candidate at the limit.
	f.Add([]byte{0xFF}, []byte{0xFF}, uint16(52), uint16(0), uint16(50), uint16(4), uint8(0), []byte{2})
	f.Add([]byte{0xFF}, []byte{0xFF}, uint16(52), uint16(0), uint16(50), uint16(4), uint8(0), []byte{2, 2, 2, 0})
	// 400,000 bytes: past two span ends at the Chunk cap, with a read that
	// moves the window every thousandth candidate on the way.
	f.Add([]byte("....wvpk.."), []byte("wvpk"), uint16(50000), uint16(1), uint16(50000), uint16(32), uint8(7),
		append(bytes.Repeat([]byte{2}, 998), 0x26, 0x92))
	f.Fuzz(func(t *testing.T, tile, pat []byte, size, from, limit, need uint16, scale uint8, verdicts []byte) {
		if len(tile) == 0 || len(pat) == 0 {
			t.Skip()
		}
		mul := 1 + int64(scale)%8
		data := make([]byte, int64(size)*mul)
		for i := range data {
			data[i] = tile[i%len(tile)]
		}
		n := int64(len(data))
		lo, hi := int64(from)*mul, int64(limit)*mul
		ask := int(need) % 1500
		view := max(ask, len(pat)) // a view holds the pattern at least
		end := min(hi, n)

		src := &planted{data: data}
		w := New(src, n, "test: reading")
		// next is the naive search's cursor: the first offset not yet
		// compared against the pattern.
		next, offers := lo, 0
		var answer Verdict
		got, ok := w.Scan(lo, hi, pat, ask, func(off int64, hdr []byte) Verdict {
			if answer != Skip {
				t.Fatalf("offered %d after try answered %d", off, answer)
			}
			for next < end && !bytes.HasPrefix(data[next:], pat) {
				next++
			}
			if off >= end || off != next || !bytes.Equal(hdr, data[off:min(off+int64(view), n)]) {
				t.Fatalf("offered %d with %d bytes; the next occurrence is at %d and the range ends at %d", off, len(hdr), next, end)
			}
			next++
			if len(verdicts) == 0 {
				return Skip
			}
			v := verdicts[offers%len(verdicts)]
			offers++
			// A read past the window's end rebases it there and the next one
			// rebases again, so the bytes read bound how many an input gets.
			if v&0x04 != 0 && src.bytes < 32*n+4<<20 {
				far := min(w.winOff+int64(len(w.win))+1+int64(v&0x70)<<4, n)
				if b := w.Ahead(far, 32); !bytes.Equal(b, data[far:min(far+32, n)]) {
					t.Fatalf("Ahead(%d, 32) past the window's end served the wrong %d bytes", far, len(b))
				}
			}
			if v&0x80 != 0 {
				ahead := int(v&0x70) << 6
				if b := w.Ahead(off, ahead); !bytes.Equal(b, data[off:min(off+int64(ahead), n)]) {
					t.Fatalf("Ahead(%d, %d) served the wrong %d bytes", off, ahead, len(b))
				}
			}
			if !bytes.Equal(hdr, data[off:min(off+int64(view), n)]) {
				t.Fatalf("the view at %d lost its bytes to a read try made", off)
			}
			switch v & 3 {
			case 0:
				answer = Take
			case 1:
				answer = Stop
			}
			return answer
		})
		if w.Err() != nil {
			t.Fatalf("Err = %v", w.Err())
		}
		switch answer {
		case Take:
			if !ok || got != next-1 {
				t.Fatalf("Scan = %d, %v, want the candidate taken at %d", got, ok, next-1)
			}
		case Stop:
			if ok || got != 0 {
				t.Fatalf("Scan = %d, %v after a Stop", got, ok)
			}
		default:
			for next < end && !bytes.HasPrefix(data[next:], pat) {
				next++
			}
			if ok || got != 0 || next < end {
				t.Fatalf("Scan = %d, %v with the next occurrence at %d of %d", got, ok, next, end)
			}
		}
	})
}
