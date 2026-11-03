package asf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
)

func openFixture(t *testing.T, name string) *Demuxer {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// halfFrames is a counter whose frames are half as long, which a count taken
// at the real length must not be restored against.
type halfFrames struct{ frameCounter }

func (h halfFrames) FrameLen() int { return h.frameCounter.FrameLen() / 2 }

// TestARestoreRefusesAForeignCount. A sidecar is keyed by the source's
// identity, but the blob is checked against the file anyway, and one taken
// against another geometry, another version of the format, or cut short is
// refused: the open then counts from the data, and lands where it would have
// with no sidecar at all.
func TestARestoreRefusesAForeignCount(t *testing.T) {
	const name = "ms-v2-44100-2ch-128k.wma"
	walked := func() *Demuxer {
		d := openFixture(t, name)
		if err := d.Walk(context.Background()); err != nil {
			t.Fatal(err)
		}
		return d
	}
	good := walked()
	blob := good.IndexSnapshot()
	if blob == nil {
		t.Fatal("no snapshot to vary")
	}
	target := good.track.Samples - 3000
	want, err := good.SeekSample(context.Background(), 0, target)
	if err != nil {
		t.Fatal(err)
	}

	foreign := map[string][]byte{}
	{
		d := walked()
		d.packets++
		foreign["another packet count"] = d.IndexSnapshot()
	}
	{
		d := walked()
		d.packetLen++
		foreign["another packet length"] = d.IndexSnapshot()
	}
	{
		d := walked()
		d.counter = halfFrames{d.counter}
		foreign["another frame length"] = d.IndexSnapshot()
	}
	{
		d := walked()
		d.idx.frames++
		foreign["a frame more at the end"] = d.IndexSnapshot()
	}
	{
		d := walked()
		for i := range d.idx.entries[1:] {
			d.idx.entries[1+i].frames++
		}
		foreign["a frame more in the first packet"] = d.IndexSnapshot()
	}
	version := bytes.Clone(blob)
	version[len(idxMagic)] ^= 0xff
	foreign["another version"] = version
	foreign["cut short"] = blob[:len(blob)-1]
	foreign["a byte past the end"] = append(bytes.Clone(blob), 0)

	for what, b := range foreign {
		d := openFixture(t, name)
		if d.RestoreIndex(b) {
			t.Errorf("%s: restored", what)
			continue
		}
		if d.Walked() || len(d.idx.entries) != 0 {
			t.Errorf("%s: a refused restore left %d entries (walked %v)", what, len(d.idx.entries), d.Walked())
		}
		if got, err := d.SeekSample(context.Background(), 0, target); err != nil || got != want {
			t.Errorf("%s: seek to %d landed at %d (%v), %d from a count of the data", what, target, got, err, want)
		}
	}
	if !openFixture(t, name).RestoreIndex(blob) {
		t.Error("the snapshot itself was refused")
	}
}

// TestALandingThatDisagreesWithItsCountStartsOver. A restored count passes
// checks on a sample of its entries, so an unsampled one can still name a
// packet no object begins in. The landing finds that out when the first object
// it reads began elsewhere, and the start is the one landing that is always
// right; the count is dropped, and the next seek takes it from the data.
func TestALandingThatDisagreesWithItsCountStartsOver(t *testing.T) {
	d := openFixture(t, "frag.wma")
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	fl := int64(d.counter.FrameLen())
	// An entry whose packet is followed by one no object begins in, far
	// enough in that the landing's run-up does not reach the start.
	k := -1
	for i := 2; i+1 < len(d.idx.entries); i++ {
		if d.idx.entries[i+1].packet > d.idx.entries[i].packet+1 {
			k = i
			break
		}
	}
	if k < 0 {
		t.Fatal("every packet begins an object; nothing to point an entry at")
	}
	e := d.idx.entries[k]
	target := e.frames * fl
	want, err := d.SeekSample(context.Background(), 0, target)
	if err != nil || want == 0 {
		t.Fatalf("a seek to entry %d's object landed at %d (%v); the fallback below would be invisible", k, want, err)
	}
	d.idx.entries[k].packet++
	landed, err := d.SeekSample(context.Background(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if landed != 0 || pkt.PTS != 0 {
		t.Errorf("a landing on a packet no object begins in returned %d and read the object at %d, want the start", landed, pkt.PTS)
	}
	if got, err := d.SeekSample(context.Background(), 0, target); err != nil || got != want {
		t.Errorf("the next seek landed at %d (%v), want %d from a fresh count", got, err, want)
	}
}

// TestARestoreRefusesACorruptedSnapshot. The sidecar outlives the run that
// wrote it, and a restored count becomes the exact length a walk settles: a
// frame step one off at an entry the probes do not sample used to restore
// clean, cap the decode a frame short, and stay that way, since loading a
// sidecar refreshes it. No single changed byte of a stored snapshot may
// restore.
func TestARestoreRefusesACorruptedSnapshot(t *testing.T) {
	const name = "ms-v2-44100-2ch-128k.wma"
	d := openFixture(t, name)
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	blob := d.IndexSnapshot()
	if blob == nil {
		t.Fatal("no snapshot to corrupt")
	}
	for i := range blob {
		for _, delta := range []byte{1, 0xff} {
			b := bytes.Clone(blob)
			b[i] += delta
			if openFixture(t, name).RestoreIndex(b) {
				t.Errorf("byte %d of %d changed by %+d restored", i, len(blob), int8(delta))
			}
		}
	}
}

// TestACountPastItsCapStillLandsAndSettles. The count keeps an entry per
// packet an object begins in, and a crafted file of tiny packets would grow
// it with the file. Past the cap it keeps counting without keeping entries:
// the length it settles and every landing are the uncapped ones, a landing
// past the last entry steps there object by object, and a capped count is
// not worth a sidecar.
func TestACountPastItsCapStillLandsAndSettles(t *testing.T) {
	const name = "ms-v2-44100-2ch-128k.wma"
	full := openFixture(t, name)
	if err := full.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func(n int) { maxCountEntries = n }(maxCountEntries)
	maxCountEntries = 4
	d := openFixture(t, name)
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.idx.entries) > 4 {
		t.Fatalf("the count kept %d entries past a cap of 4", len(d.idx.entries))
	}
	if got, want := d.track.Samples, full.track.Samples; got != want || !d.track.SamplesExact {
		t.Errorf("the capped walk settled %d (exact %v), the full one %d", got, d.track.SamplesExact, want)
	}
	for _, target := range []int64{0, 10000, 100000, 200000, full.track.Samples - 1} {
		want, err := full.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := d.SeekSample(context.Background(), 0, target); err != nil || got != want {
			t.Errorf("seek to %d landed at %d (%v) capped, %d uncapped", target, got, err, want)
		}
	}
	if d.IndexSnapshot() != nil {
		t.Error("a capped count asked to be saved")
	}
}

// TestAWalkDoesNotCopyEveryObject: a walk reads every object to count its
// frames, and taking a fresh buffer for each one made it allocate about the
// file's size again in garbage.
func TestAWalkDoesNotCopyEveryObject(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sine-wmav2.wma"))
	if err != nil {
		t.Fatal(err)
	}
	open := func() *Demuxer {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	walk := testing.AllocsPerRun(5, func() {
		if err := open().Walk(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	bare := testing.AllocsPerRun(5, func() { open() })
	d := open()
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if objects := float64(d.idx.objects); walk-bare >= objects/2 {
		t.Errorf("a walk of %v objects allocated %v times", objects, walk-bare)
	}
}

// TestARestoreProbeLeavesNoWarning: a restore's probes read packets the reader
// has not reached. A probe that meets damage refuses the blob, and the walk
// that replaces it reports the damage where it lies; the probe itself
// reporting it would put a finding in Warnings that no read made.
func TestARestoreProbeLeavesNoWarning(t *testing.T) {
	const name = "ms-v2-44100-2ch-128k.wma"
	d := openFixture(t, name)
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	blob := d.IndexSnapshot()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	// Opaque error correction on packet 2, the probe at the third entry.
	raw[d.packetOff(2)] = 0x90
	damaged, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if damaged.RestoreIndex(blob) {
		t.Fatal("a blob whose probe meets a damaged packet was restored")
	}
	if w := damaged.Warnings(); len(w) != 0 {
		t.Errorf("the refused restore left %v", w)
	}
}

// TestAReadCountsWithoutCopying: the count a read feeds asks the codec about
// each object while the object and the one after it are both in hand, so
// playback copies nothing into the count to ask about later.
func TestAReadCountsWithoutCopying(t *testing.T) {
	d := openFixture(t, "ms-v2-44100-2ch-128k.wma")
	var pkt container.Packet
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
	}
	if !d.Walked() {
		t.Fatal("a read to the end left the count unwalked")
	}
	if n := len(d.idx.prev.data); n != 0 {
		t.Errorf("the count holds a %d-byte copy of an object the read delivered", n)
	}
}
