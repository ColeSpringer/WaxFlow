package adts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestStrictOpenSurvivesUndecodableFirstFrame pins that the SBR probe
// never escalates under Strict: framing-clean files whose first payload
// does not decode kept strict-probing (and remuxing) before implicit
// detection existed, and must keep doing so. The failure stays visible
// as a warning.
func TestStrictOpenSurvivesUndecodableFirstFrame(t *testing.T) {
	// Valid 24000 mono LC framing; the payload's leading CCE element tag
	// fails the decode.
	frame := []byte{0xFF, 0xF1, 0x58, 0x40, 0x01, 0x3F, 0xFC, 0x40, 0x00}
	raw := append(append([]byte(nil), frame...), frame...)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("strict open must survive a payload the probe cannot decode: %v", err)
	}
	if got := d.Tracks()[0].Codec; got != codec.AACLC {
		t.Errorf("codec = %q, want the aac-lc fallback", got)
	}
	found := false
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, "SBR probe failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("the probe failure must stay visible as a warning, got %v", d.Warnings())
	}
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDemuxFixtures walks the committed ADTS fixtures: one AAC track, an
// AAC-LC config synthesized from the header, and uniform 1024-sample frames.
func TestDemuxFixtures(t *testing.T) {
	for _, name := range []string{"stereo.aac", "mono.aac"} {
		t.Run(name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(fixture(t, name)), nil)
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			tr := d.Tracks()[0]
			if tr.Codec != codec.AACLC {
				t.Errorf("codec = %q, want aac-lc", tr.Codec)
			}
			if tr.Fmt.Rate != 44100 {
				t.Errorf("rate = %d, want 44100", tr.Fmt.Rate)
			}
			if err := tr.Fmt.Valid(); err != nil {
				t.Errorf("format invalid: %v", err)
			}
			if tr.Samples != -1 {
				t.Errorf("samples = %d, want -1 (ADTS declares no length)", tr.Samples)
			}
			var count int64
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("ReadPacket: %v", err)
				}
				if pkt.Dur != 1024 || len(pkt.Data) == 0 || !pkt.Sync {
					t.Fatalf("frame %d: dur=%d len=%d sync=%v", count, pkt.Dur, len(pkt.Data), pkt.Sync)
				}
				if pkt.PTS != count*1024 {
					t.Fatalf("frame %d pts=%d, want %d", count, pkt.PTS, count*1024)
				}
				count++
			}
			if count == 0 {
				t.Fatal("no frames decoded")
			}
		})
	}
}

// TestSeekBacksOff checks SeekSample lands one frame before the target (the
// IMDCT overlap pre-roll) and never overshoots.
func TestSeekBacksOff(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixture(t, "stereo.aac")), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 1024, 2048, 4096, 1 << 30} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target {
			t.Errorf("seek to %d overshot to %d", target, landed)
		}
		if landed%1024 != 0 {
			t.Errorf("seek to %d landed at %d, not a frame boundary", target, landed)
		}
	}
}

// TestLeadingID3Skipped checks a leading ID3v2 tag is skipped.
func TestLeadingID3Skipped(t *testing.T) {
	clean := fixture(t, "stereo.aac")
	tag := make([]byte, 10+20)
	copy(tag, "ID3\x04\x00\x00")
	tag[9] = 20 // 20-byte tag body (syncsafe)
	mangled := append(tag, clean...)
	d, err := NewDemuxer(container.BytesSource(mangled), nil)
	if err != nil {
		t.Fatalf("NewDemuxer with ID3: %v", err)
	}
	if d.firstFrame != int64(len(tag)) {
		t.Errorf("first frame at %d, want %d (after the ID3 tag)", d.firstFrame, len(tag))
	}
}

// TestTruncatedTailDropped checks a final frame cut short by EOF is dropped
// with a warning in tolerant mode (not a hard read error) and rejected in
// strict mode.
func TestTruncatedTailDropped(t *testing.T) {
	full := fixture(t, "stereo.aac")
	d0, err := NewDemuxer(container.BytesSource(full), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	complete := 0
	for d0.ReadPacket(&pkt) == nil {
		complete++
	}
	lastOff := d0.idx[len(d0.idx)-1]
	h, ok := parseHeader(d0.w.BytesAt(lastOff, 9))
	if !ok || lastOff+int64(h.frameLen) != int64(len(full)) || h.frameLen < 16 {
		t.Skip("fixture's last frame is not flush with EOF")
	}
	trunc := full[:len(full)-5] // cut into the final frame's payload

	d, err := NewDemuxer(container.BytesSource(trunc), nil)
	if err != nil {
		t.Fatalf("NewDemuxer(truncated): %v", err)
	}
	got := 0
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tolerant mode errored on a truncated tail: %v", err)
		}
		got++
	}
	if got != complete-1 {
		t.Errorf("delivered %d frames, want %d (all but the truncated tail)", got, complete-1)
	}
	if len(d.Warnings()) == 0 {
		t.Error("truncated tail produced no warning")
	}

	ds, err := NewDemuxer(container.BytesSource(trunc), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer(strict): %v", err)
	}
	var serr error
	for {
		if serr = ds.ReadPacket(&pkt); serr != nil {
			break
		}
	}
	if errors.Is(serr, io.EOF) {
		t.Error("strict mode tolerated a truncated tail")
	}

	// The walk publishes what a read delivers. It used to index the
	// truncated frame and report it exact, so a strict probe passed a file
	// a strict read failed.
	dw, err := NewDemuxer(container.BytesSource(trunc), nil)
	if err != nil {
		t.Fatalf("NewDemuxer(walk): %v", err)
	}
	if err := dw.Walk(); err != nil {
		t.Fatalf("tolerant Walk on a truncated tail: %v", err)
	}
	tr := dw.Tracks()[0]
	if want := int64(complete-1) * dw.spf; tr.Samples != want || !tr.SamplesExact {
		t.Errorf("walked length = %d (exact %v), want %d exact", tr.Samples, tr.SamplesExact, want)
	}
	if tr.Samples != int64(got)*dw.spf {
		t.Errorf("walk counted %d samples, a read delivered %d", tr.Samples, int64(got)*dw.spf)
	}
	if !slices.ContainsFunc(dw.Warnings(), func(w container.Warning) bool {
		return w.Msg == "truncated final frame dropped"
	}) {
		t.Errorf("warnings after Walk = %v, want the dropped tail", dw.Warnings())
	}

	sw, err := NewDemuxer(container.BytesSource(trunc), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer(strict walk): %v", err)
	}
	if err := sw.Walk(); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict Walk = %v, want malformed", err)
	}
}

// TestOnlyFrameTruncated is the same rule with nothing left over: a stream
// cut off inside its one and only frame indexes nothing, so the walk
// measures zero rather than publishing a frame no read can deliver.
func TestOnlyFrameTruncated(t *testing.T) {
	full := fixture(t, "stereo.aac")
	probe, err := NewDemuxer(container.BytesSource(full), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := probe.firstFrame
	h, ok := parseHeader(probe.w.BytesAt(first, 9))
	if !ok || h.frameLen < 16 {
		t.Skip("fixture's first frame is not usable here")
	}
	trunc := full[:first+int64(h.frameLen)-5]

	d, err := NewDemuxer(container.BytesSource(trunc), nil)
	if err != nil {
		t.Fatalf("NewDemuxer(truncated): %v", err)
	}
	if len(d.idx) != 0 {
		t.Errorf("index holds %d entries, want none: the only frame does not fit", len(d.idx))
	}
	want := container.Warning{Offset: first, Msg: "the only frame is truncated, dropped", Kind: container.Damage}
	if !slices.Contains(d.Warnings(), want) {
		t.Errorf("warnings = %v, want %v", d.Warnings(), want)
	}
	if err := d.Walk(); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if tr := d.Tracks()[0]; tr.Samples != 0 || !tr.SamplesExact {
		t.Errorf("walked length = %d (exact %v), want 0 exact", tr.Samples, tr.SamplesExact)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
		t.Errorf("ReadPacket = %v, want EOF", err)
	}
	if landed, err := d.SeekSample(0, 1<<20); err != nil || landed != 0 {
		t.Errorf("SeekSample = %d, %v; want 0, nil", landed, err)
	}
	if _, err := NewDemuxer(container.BytesSource(trunc), &DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict open = %v, want malformed", err)
	}
}
