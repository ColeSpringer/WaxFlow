package testutil

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// OpenDemuxer opens a demuxer over src with Strict off.
type OpenDemuxer func(src container.Source) (container.Demuxer, error)

// GoldenDamagedRead holds what a tolerant read and three seeks deliver from
// the fixture at file, and from each damaged copy of it, to a golden under
// testdata/damaged. file has slashes and starts at the package.
func GoldenDamagedRead(t *testing.T, file string, open OpenDemuxer, update bool, damages func(t *testing.T, raw []byte) []Damage) {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(file))
	if err != nil {
		t.Fatal(err)
	}
	// The two damages every format takes.
	fixed := []Damage{
		{Name: "zero-middle", What: fmt.Sprintf("64 bytes zeroed at %d", len(raw)/2), Raw: ZeroMiddle(raw, 64)},
		{Name: "cut-tail", What: "the last 100 bytes cut", Raw: CutTail(raw, 100)},
	}
	GoldenDamagedInput(t, path.Base(file), "", raw, open, update, func(t *testing.T, raw []byte) []Damage {
		return append(damages(t, raw), fixed...)
	})
}

// GoldenDamagedInput is GoldenDamagedRead for bytes a test makes from a
// fixture, under the damages it is given and no other: name names the golden
// and what says what the bytes are.
func GoldenDamagedInput(t *testing.T, name, what string, raw []byte, open OpenDemuxer, update bool, damages func(t *testing.T, raw []byte) []Damage) {
	t.Helper()
	golden := filepath.Join("testdata", "damaged", name+".txt")
	claimGolden(t, golden)
	t.Run(name, func(t *testing.T) {
		goldenDamagedRead(t, golden, what, raw, damages(t, raw), open, update)
	})
}

// goldenClaims is the goldens each running test has written or compared.
var goldenClaims = struct {
	sync.Mutex
	of map[testing.TB]map[string]bool
}{of: map[testing.TB]map[string]bool{}}

// claimGolden fails t when it holds two inputs to one golden, which the
// second would otherwise overwrite or be compared against.
func claimGolden(t testing.TB, golden string) {
	t.Helper()
	goldenClaims.Lock()
	defer goldenClaims.Unlock()
	if goldenClaims.of[t] == nil {
		goldenClaims.of[t] = map[string]bool{}
		t.Cleanup(func() {
			goldenClaims.Lock()
			defer goldenClaims.Unlock()
			delete(goldenClaims.of, t)
		})
	}
	if goldenClaims.of[t][golden] {
		t.Fatalf("two inputs of this test share the golden %s", golden)
	}
	goldenClaims.of[t][golden] = true
}

// goldenDamagedRead reads raw and each damage and holds the lines to the
// golden. A case has to count: it changes the read or is Silent, and a read
// reaches every range it inserted.
func goldenDamagedRead(t testing.TB, golden, what string, raw []byte, damages []Damage, open OpenDemuxer, update bool) {
	t.Helper()
	cases := append([]Damage{{Name: "clean", What: what, Raw: raw}}, damages...)

	var out strings.Builder
	out.WriteString("# <case> p<n> <pts> <dur> <crc-32 of the data>, with lost=<samples> and discont where the packet says so\n" +
		"# <case> seek <target> landed <position>, then the packet read next in the same fields\n")
	var clean []string
	var targets []int64
	for i, c := range cases {
		if slices.ContainsFunc(cases[:i], func(o Damage) bool { return o.Name == c.Name }) {
			t.Fatalf("two damages are named %q", c.Name)
		}
		src := &CountingSource{Src: container.BytesSource(c.Raw), Log: true}
		var lines []string
		d, err := open(src)
		if err != nil {
			if i == 0 {
				t.Fatalf("the undamaged file does not open: %v", err)
			}
			lines = []string{fmt.Sprintf("open refused %s: %v", waxerr.CodeOf(err), err)}
		} else {
			var samples int64
			lines, samples = readLines(t, d, len(c.Raw))
			if i == 0 {
				targets = []int64{samples / 4, samples / 2, samples / 4 * 3}
			}
			lines = append(lines, seekLines(d, targets)...)
		}
		switch same := slices.Equal(lines, clean); {
		case i == 0:
			clean = lines
		case same && !c.Silent:
			t.Errorf("%s reads as the undamaged file does, so it pins nothing", c.Name)
		case !same && c.Silent:
			t.Errorf("%s is marked Silent and does not read as the undamaged file does", c.Name)
		}
		for _, in := range c.Inserted {
			if !slices.ContainsFunc(src.Spans, func(s [2]int64) bool { return s[0] < int64(in[1]) && s[1] > int64(in[0]) }) {
				t.Errorf("%s: no read reached the %d bytes inserted at %d", c.Name, in[1]-in[0], in[0])
			}
		}
		fmt.Fprintf(&out, "%s file %d bytes", c.Name, len(c.Raw))
		if c.What != "" {
			fmt.Fprintf(&out, ": %s", c.What)
		}
		out.WriteByte('\n')
		for _, l := range lines {
			fmt.Fprintf(&out, "%s %s\n", c.Name, l)
		}
	}
	GoldenText(t, golden, []byte(out.String()), update,
		"a damaged read moved: the demuxer resyncs somewhere else, or delivers other packets or warnings past the damage.\n"+
			"If the change says it moves these lines, run `make goldens` and review the diff line by line.")
}

// readLines reads d to its end as golden lines, and counts the samples its
// packets hold. size is the file's, the bound on a read that never ends.
func readLines(t testing.TB, d container.Demuxer, size int) (lines []string, samples int64) {
	t.Helper()
	tr := d.Tracks()[0]
	lines = append(lines, fmt.Sprintf("open samples=%d exact=%t", tr.Samples, tr.SamplesExact))
	var packets []string
	var pkt container.Packet
	var end error
	for {
		if end = d.ReadPacket(&pkt); end != nil {
			break
		}
		// No format read here has an empty packet.
		if len(packets) > size {
			t.Fatalf("more than %d packets from %d bytes", size, size)
		}
		packets = append(packets, fmt.Sprintf("p%d %s", len(packets), packetFields(&pkt)))
		samples += pkt.Dur
	}
	lines = append(lines, fmt.Sprintf("packets %d", len(packets)))
	lines = append(lines, packets...)
	if end != io.EOF {
		lines = append(lines, fmt.Sprintf("read stopped %s: %v", waxerr.CodeOf(end), end))
	}
	return append(lines, warningLines("warnings", "w", warningsOf(d))...), samples
}

// seekLines seeks d, already read, to each target and reads one packet after
// each. The warnings the seeks added come last, apart from the read's.
func seekLines(d container.Demuxer, targets []int64) []string {
	seeker, ok := d.(container.Seeker)
	if !ok {
		return []string{"seeks none: not a seeker"}
	}
	read := len(warningsOf(d))
	var lines []string
	for _, target := range targets {
		landed, err := seeker.SeekSample(context.Background(), 0, target)
		if err != nil {
			lines = append(lines, fmt.Sprintf("seek %d refused %s: %v", target, waxerr.CodeOf(err), err))
			continue
		}
		var pkt container.Packet
		then := "the end"
		if err := d.ReadPacket(&pkt); err == nil {
			then = packetFields(&pkt)
		} else if err != io.EOF {
			then = fmt.Sprintf("stopped %s: %v", waxerr.CodeOf(err), err)
		}
		lines = append(lines, fmt.Sprintf("seek %d landed %d, then %s", target, landed, then))
	}
	all := warningsOf(d)
	return append(lines, warningLines("seek warnings", "sw", all[min(read, len(all)):])...)
}

// packetFields is a packet's position, duration and CRC-32, and the hole in
// front of it where it carries one.
func packetFields(pkt *container.Packet) string {
	l := fmt.Sprintf("%d %d %08x", pkt.PTS, pkt.Dur, crc32.ChecksumIEEE(pkt.Data))
	if pkt.Lost != 0 {
		l += fmt.Sprintf(" lost=%d", pkt.Lost)
	}
	if pkt.Discont {
		l += " discont"
	}
	return l
}

func warningsOf(d container.Demuxer) []container.Warning {
	if w, ok := d.(container.Warner); ok {
		return w.Warnings()
	}
	return nil
}

func warningLines(title, mark string, ws []container.Warning) []string {
	lines := []string{fmt.Sprintf("%s %d", title, len(ws))}
	for i, w := range ws {
		kind := "damage"
		if w.Kind == container.Note {
			kind = "note"
		}
		lines = append(lines, fmt.Sprintf("%s%d %s at %d: %s", mark, i, kind, w.Offset, w.Msg))
	}
	return lines
}

// Damage is one damaged copy of a file, as GoldenDamagedRead reads it.
type Damage struct {
	Name string // the case, and the prefix of its lines in the golden
	What string // the damage in words
	Raw  []byte
	// Silent marks damage the format reads past without a word, so the read
	// must be the undamaged file's. Every other damage must change the read.
	Silent bool
	// Inserted is the ranges of Raw the damage put there. A read must reach
	// each, or the case tests nothing the demuxer does.
	Inserted [][2]int
}

// Cut returns d without its last n bytes.
func (d Damage) Cut(n int) Damage {
	d.Raw = CutTail(d.Raw, n)
	d.What += fmt.Sprintf(", and the last %d bytes cut", n)
	return d
}

// JunkLen is the junk JunkAt, DecoyAt and IslandAt splice in.
const JunkLen = 8 << 10

// SpanJunkLen is junk three bytes short of 1 KiB. A scan that reads its range
// in spans growing from 1 KiB, as the MP3 frame walk does, ends its first
// span inside the header behind junk of this length.
const SpanJunkLen = 1<<10 - 3

// JunkAt is raw with JunkLen of SyncJunk spliced in at off.
func JunkAt(name string, raw []byte, off int, sync byte) Damage {
	return JunkOf(name, raw, off, JunkLen, sync)
}

// JunkOf is raw with n bytes of SyncJunk spliced in at off.
func JunkOf(name string, raw []byte, off, n int, sync byte) Damage {
	return Damage{
		Name:     name,
		What:     fmt.Sprintf("%d bytes of junk at %d, every second one %#02x", n, off, sync),
		Raw:      Splice(raw, off, SyncJunk(n, sync)),
		Inserted: [][2]int{{off, off + n}},
	}
}

// DecoyAt is JunkAt with a decoy a quarter of the way into the junk.
func DecoyAt(name string, raw []byte, off int, sync byte) Damage {
	return DecoyOf(name, raw, off, JunkLen, JunkLen/4, sync)
}

// DecoyOf is JunkOf with a decoy at bytes into the junk: the 32 bytes raw
// holds at off, the head of the frame the junk is in front of. A scan meets a
// header that parses with nothing of its frame behind it.
func DecoyOf(name string, raw []byte, off, n, at int, sync byte) Damage {
	const head = 32
	d := JunkOf(name, raw, off, n, sync)
	copy(d.Raw[off+at:], raw[off:off+head])
	d.What += fmt.Sprintf(", the %d bytes that follow it also at %d", head, off+at)
	return d
}

// IslandAt is JunkAt twice, in front of raw[from:to] and behind it: a whole
// frame with nothing a scan could confirm it against on either side.
func IslandAt(name string, raw []byte, from, to int, sync byte) Damage {
	d := JunkAt(name, Splice(raw, to, SyncJunk(JunkLen, sync)), from, sync)
	d.What = fmt.Sprintf("%d bytes of junk at %d and at %d, every second one %#02x, the %d bytes between them whole",
		JunkLen, from, to+JunkLen, sync, to-from)
	d.Inserted = append(d.Inserted, [2]int{to + JunkLen, to + 2*JunkLen})
	return d
}

// SyncJunk returns n bytes in which every second byte is sync, the last one
// too, and the bytes between step through all 256 values. A candidate in it
// reads sync, x, sync or sync, sync, x, which no header scanned for allows.
func SyncJunk(n int, sync byte) []byte {
	out := make([]byte, n)
	for i := range out {
		if (n-i)%2 == 1 {
			out[i] = sync
		} else {
			out[i] = byte(i/2*0x9D + 0x35)
		}
	}
	return out
}

// Splice returns a copy of raw with junk inserted at off.
func Splice(raw []byte, off int, junk []byte) []byte {
	out := make([]byte, 0, len(raw)+len(junk))
	out = append(out, raw[:off]...)
	out = append(out, junk...)
	return append(out, raw[off:]...)
}

// CutTail returns a copy of raw without its last n bytes.
func CutTail(raw []byte, n int) []byte {
	return append([]byte(nil), raw[:max(len(raw)-n, 0)]...)
}
