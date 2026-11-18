package testutil

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// recordDemux reads a format made for these tests, eight-byte records of ten
// samples read one at a time: zeros are a hole, question marks cannot be
// read, and a tail short of a record is noted from the size and never read.
type recordDemux struct {
	src      container.Source
	next     int64
	lost     int64
	warnings container.Warnings
}

func openRecords(src container.Source) (container.Demuxer, error) {
	if src.Size() < 8 {
		return nil, waxerr.Malformed("records: ", "no whole record")
	}
	d := &recordDemux{src: src}
	if tail := src.Size() % 8; tail != 0 {
		d.warnings.Note(src.Size()-tail, fmt.Sprintf("%d trailing bytes dropped", tail))
	}
	return d, nil
}

func (d *recordDemux) Tracks() []container.Track {
	return []container.Track{{Samples: d.src.Size() / 8 * 10, SamplesExact: true}}
}

func (d *recordDemux) Warnings() []container.Warning { return d.warnings.List() }

func (d *recordDemux) record(off int64) []byte {
	rec := make([]byte, 8)
	if err := container.ReadFull(d.src, rec, off); err != nil {
		panic(err)
	}
	return rec
}

func (d *recordDemux) ReadPacket(pkt *container.Packet) error {
	for ; d.next+8 <= d.src.Size(); d.next += 8 {
		rec := d.record(d.next)
		switch {
		case bytes.Equal(rec, make([]byte, 8)):
			d.warnings.Damage(d.next, "a record of zeros, skipped")
			d.lost += 10
			continue
		case string(rec) == "????????":
			return waxerr.Malformed("records: ", "a record that cannot be read")
		}
		*pkt = container.Packet{Lost: d.lost, Packet: codec.Packet{
			Data: rec, PTS: d.next / 8 * 10, Dur: 10, Discont: d.lost > 0}}
		d.next, d.lost = d.next+8, 0
		return nil
	}
	return io.EOF
}

func (d *recordDemux) SeekSample(_ context.Context, _ int, sample int64) (int64, error) {
	d.next, d.lost = min(sample/10, d.src.Size()/8-1)*8, 0
	if bytes.Equal(d.record(d.next), make([]byte, 8)) {
		d.warnings.Note(d.next, "a seek landed in a hole")
	}
	return d.next / 8 * 10, nil
}

// recordsNoSeek is a recordDemux that cannot seek.
type recordsNoSeek struct{ d *recordDemux }

func (r recordsNoSeek) Tracks() []container.Track              { return r.d.Tracks() }
func (r recordsNoSeek) ReadPacket(pkt *container.Packet) error { return r.d.ReadPacket(pkt) }

// fatalOf runs fn and returns what it reported through Fatalf, "" for nothing.
func fatalOf(t testing.TB, fn func(tb testing.TB)) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			s, ok := r.(string)
			if !ok {
				panic(r)
			}
			msg = s
		}
	}()
	fn(&fatalTB{TB: t})
	return ""
}

// TestGoldenDamagedReadWritesEachRead pins every kind of line the golden
// holds, for a read, for the seeks behind it and for a refused open, and that
// a moved read names its own case's lines and no other.
func TestGoldenDamagedReadWritesEachRead(t *testing.T) {
	raw := []byte("AAAAAAAABBBBBBBBCCCCCCCCDDDDDDDD")
	hole := append([]byte(nil), raw...)
	clear(hole[8:16])
	broken := append([]byte(nil), raw...)
	copy(broken[16:], "????????")
	crc := func(s string) uint32 { return crc32.ChecksumIEEE([]byte(s)) }
	want := fmt.Sprintf(`# <case> p<n> <pts> <dur> <crc-32 of the data>, with lost=<samples> and discont where the packet says so
# <case> seek <target> landed <position>, then the packet read next in the same fields
clean file 32 bytes: four records
clean open samples=40 exact=true
clean packets 4
clean p0 0 10 %08[1]x
clean p1 10 10 %08[2]x
clean p2 20 10 %08[3]x
clean p3 30 10 %08[4]x
clean warnings 0
clean seek 10 landed 10, then 10 10 %08[2]x
clean seek 20 landed 20, then 20 10 %08[3]x
clean seek 30 landed 30, then 30 10 %08[4]x
clean seek warnings 0
hole file 32 bytes: the second record zeroed
hole open samples=40 exact=true
hole packets 3
hole p0 0 10 %08[1]x
hole p1 20 10 %08[3]x lost=10 discont
hole p2 30 10 %08[4]x
hole warnings 1
hole w0 damage at 8: a record of zeros, skipped
hole seek 10 landed 10, then 20 10 %08[3]x lost=10 discont
hole seek 20 landed 20, then 20 10 %08[3]x
hole seek 30 landed 30, then 30 10 %08[4]x
hole seek warnings 1
hole sw0 note at 8: a seek landed in a hole
broken file 32 bytes: the third record unreadable
broken open samples=40 exact=true
broken packets 2
broken p0 0 10 %08[1]x
broken p1 10 10 %08[2]x
broken read stopped malformed-input: records: a record that cannot be read
broken warnings 0
broken seek 10 landed 10, then 10 10 %08[2]x
broken seek 20 landed 20, then stopped malformed-input: records: a record that cannot be read
broken seek 30 landed 30, then 30 10 %08[4]x
broken seek warnings 0
zero-middle file 32 bytes: 64 bytes zeroed at 16
zero-middle open samples=40 exact=true
zero-middle packets 2
zero-middle p0 0 10 %08[1]x
zero-middle p1 10 10 %08[2]x
zero-middle warnings 2
zero-middle w0 damage at 16: a record of zeros, skipped
zero-middle w1 damage at 24: a record of zeros, skipped
zero-middle seek 10 landed 10, then 10 10 %08[2]x
zero-middle seek 20 landed 20, then the end
zero-middle seek 30 landed 30, then the end
zero-middle seek warnings 2
zero-middle sw0 note at 16: a seek landed in a hole
zero-middle sw1 note at 24: a seek landed in a hole
cut-tail file 0 bytes: the last 100 bytes cut
cut-tail open refused malformed-input: records: no whole record
`, crc("AAAAAAAA"), crc("BBBBBBBB"), crc("CCCCCCCC"), crc("DDDDDDDD"))

	golden := filepath.Join(t.TempDir(), "damaged", "records.txt")
	damages := []Damage{
		{Name: "hole", What: "the second record zeroed", Raw: hole},
		{Name: "broken", What: "the third record unreadable", Raw: broken},
		{Name: "zero-middle", What: "64 bytes zeroed at 16", Raw: ZeroMiddle(raw, 64)},
		{Name: "cut-tail", What: "the last 100 bytes cut", Raw: CutTail(raw, 100)},
	}
	goldenDamagedRead(t, golden, "four records", raw, damages, openRecords, true)
	got, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("the golden reads:\n%s\nwant:\n%s", got, want)
	}

	same := &errTB{TB: t}
	goldenDamagedRead(same, golden, "four records", raw, damages, openRecords, false)
	if len(same.errs) != 0 {
		t.Errorf("an unchanged read reported %q", same.errs)
	}

	// A resync that lands a record late moves that case's lines and no other.
	late := func(src container.Source) (container.Demuxer, error) {
		d, err := openRecords(src)
		if rd, ok := d.(*recordDemux); ok && bytes.Equal(rd.record(8), make([]byte, 8)) {
			rd.next = 16
		}
		return d, err
	}
	moved := &errTB{TB: t}
	goldenDamagedRead(moved, golden, "four records", raw, damages, late, false)
	if len(moved.errs) != 1 {
		t.Fatalf("a moved read reported %d errors, want 1: %q", len(moved.errs), moved.errs)
	}
	for _, line := range []string{"- hole packets 3\n", "- hole p0 0 10 ", "+ hole packets 2\n", "+ hole p0 20 10 "} {
		if !strings.Contains(moved.errs[0], line) {
			t.Errorf("the report lacks %q:\n%s", line, moved.errs[0])
		}
	}
	if strings.Contains(moved.errs[0], "clean ") || strings.Contains(moved.errs[0], "broken ") {
		t.Errorf("the report names a case that held:\n%s", moved.errs[0])
	}

	// A demuxer that cannot seek says so in one line.
	noSeek := filepath.Join(t.TempDir(), "records.txt")
	goldenDamagedRead(t, noSeek, "", raw, nil, func(src container.Source) (container.Demuxer, error) {
		d, err := openRecords(src)
		if err != nil {
			return nil, err
		}
		return recordsNoSeek{d.(*recordDemux)}, nil
	}, true)
	if got, err := os.ReadFile(noSeek); err != nil || !strings.HasSuffix(string(got), "clean warnings 0\nclean seeks none: not a seeker\n") {
		t.Errorf("a demuxer that cannot seek wrote (%v):\n%s", err, got)
	}
}

// TestGoldenDamagedReadRefusesDamageThatPinsNothing holds each damage to
// changing the read, a Silent one to leaving it as it was, and every one to a
// read that reaches the bytes it inserted.
func TestGoldenDamagedReadRefusesDamageThatPinsNothing(t *testing.T) {
	raw := bytes.Repeat([]byte("AAAAAAAA"), 32)
	// Three bytes behind the last record: noted from the size, never read.
	ragged := append(append([]byte(nil), raw...), "EEE"...)
	quiet := func(src container.Source) (container.Demuxer, error) {
		d, err := openRecords(src)
		if rd, ok := d.(*recordDemux); ok {
			rd.warnings = container.Warnings{}
		}
		return d, err
	}
	for _, tt := range []struct {
		name   string
		damage Damage
		want   string // in the one error, "" for none
	}{
		{"noticed", Damage{Name: "hole", Raw: ZeroMiddle(raw, 8)}, ""},
		{"unnoticed", Damage{Name: "pad", Raw: ragged}, "pad reads as the undamaged file does"},
		{"silent", Damage{Name: "pad", Raw: ragged, Silent: true}, ""},
		{"silent and noticed", Damage{Name: "hole", Raw: ZeroMiddle(raw, 8), Silent: true}, "hole is marked Silent"},
		{"inserted and read", JunkOf("junk", raw, 8, 8, 'A'), ""},
		{"inserted and never read", Damage{Name: "pad", Raw: ragged, Silent: true, Inserted: [][2]int{{256, 259}}},
			"pad: no read reached the 3 bytes inserted at 256"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tb := &errTB{TB: t}
			goldenDamagedRead(tb, filepath.Join(t.TempDir(), "records.txt"), "", raw, []Damage{tt.damage}, quiet, true)
			switch {
			case tt.want == "" && len(tb.errs) != 0:
				t.Errorf("reported %q, want nothing", tb.errs)
			case tt.want != "" && (len(tb.errs) != 1 || !strings.Contains(tb.errs[0], tt.want)):
				t.Errorf("reported %q, want one error holding %q", tb.errs, tt.want)
			}
		})
	}
}

// TestGoldenDamagedInputRefusesAGoldenUsedTwice holds a test to one input a
// golden, and lets the next test use the same goldens again.
func TestGoldenDamagedInputRefusesAGoldenUsedTwice(t *testing.T) {
	claims := func() int {
		goldenClaims.Lock()
		defer goldenClaims.Unlock()
		return len(goldenClaims.of)
	}
	held := claims()
	defer func() {
		if n := claims(); n != held {
			t.Errorf("%d claims outlive their tests", n-held)
		}
	}()
	for range 2 {
		t.Run("", func(t *testing.T) {
			msg := fatalOf(t, func(tb testing.TB) {
				claimGolden(tb, "testdata/damaged/a.txt")
				claimGolden(tb, "testdata/damaged/b.txt")
			})
			if msg != "" {
				t.Fatalf("two goldens claimed once each reported %q", msg)
			}
			msg = fatalOf(t, func(tb testing.TB) {
				claimGolden(tb, "testdata/damaged/a.txt")
				claimGolden(tb, "testdata/damaged/a.txt")
			})
			if !strings.Contains(msg, "share the golden testdata/damaged/a.txt") {
				t.Errorf("a golden claimed twice reported %q", msg)
			}
		})
	}
}

// TestSyncJunkIsDenseWithFalseStarts pins the junk's shape: every second byte
// the sync byte and the last one too, every value between them, the same on
// every call, and never the start of a magic longer than a byte.
func TestSyncJunkIsDenseWithFalseStarts(t *testing.T) {
	for _, n := range []int{JunkLen, JunkLen - 1} {
		junk := SyncJunk(n, 0xFF)
		if len(junk) != n || junk[n-1] != 0xFF {
			t.Fatalf("%d bytes of junk: %d bytes ending in %#02x, want the sync byte last", n, len(junk), junk[n-1])
		}
		seen := map[byte]bool{}
		for i := n - 1; i >= 0; i -= 2 {
			if junk[i] != 0xFF {
				t.Fatalf("byte %d of %d is %#02x, want the sync byte", i, n, junk[i])
			}
			if i > 0 {
				seen[junk[i-1]] = true
			}
		}
		if len(seen) != 256 {
			t.Errorf("the bytes between take %d values, want all 256", len(seen))
		}
		if !bytes.Equal(junk, SyncJunk(n, 0xFF)) {
			t.Error("two calls returned different junk")
		}
	}
	for _, magic := range []string{"OggS", "wvpk", "MAC ", "MACF", "MPCK", "MP+"} {
		if junk := SyncJunk(1<<20+JunkLen, magic[0]); bytes.Contains(junk, []byte(magic[:3])) {
			t.Errorf("the junk for %q spells its first three bytes", magic)
		}
	}
	raw := bytes.Repeat([]byte("0123456789abcdef"), 8)
	// A decoy is the frame's own head, planted in the junk in front of it.
	d := DecoyAt("decoy", raw, 16, 'O')
	if at := 16 + JunkLen/4; !bytes.Equal(d.Raw[at:at+32], raw[16:48]) || !bytes.Equal(d.Raw[16+JunkLen:], raw[16:]) {
		t.Error("the decoy is not the 32 bytes behind the junk, a quarter of the way into it")
	}
	// An island keeps its bytes whole between two runs of junk, and names both.
	is := IslandAt("island", raw, 16, 48, 'O')
	junk := SyncJunk(JunkLen, 'O')
	want := append(append(append(append(append([]byte(nil), raw[:16]...), junk...), raw[16:48]...), junk...), raw[48:]...)
	if !bytes.Equal(is.Raw, want) || fmt.Sprint(is.Inserted) != fmt.Sprint([][2]int{{16, 16 + JunkLen}, {48 + JunkLen, 48 + 2*JunkLen}}) {
		t.Errorf("the island is not junk, the 32 bytes, junk: inserted %v", is.Inserted)
	}
	if cut := is.Cut(100); len(cut.Raw) != len(is.Raw)-100 || !strings.HasSuffix(cut.What, ", and the last 100 bytes cut") {
		t.Errorf("Cut left %d of %d bytes and says %q", len(cut.Raw), len(is.Raw), cut.What)
	}
}
