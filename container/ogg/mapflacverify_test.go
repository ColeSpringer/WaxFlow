package ogg

// The Ogg-FLAC declared-total verification: every branch of mapflac's
// finalizeTrack, driven off the committed ffmpeg fixtures.
//
// Those fixtures all declare total 0, which is only one of the five outcomes,
// so the other four are produced by rewriting the field in place. That is what
// patchOggFLACTotal is for, and it is what lets these pins hold with no
// reference encoder installed: the field is five bytes of the BOS page and the
// page CRC covers them, so a rewritten fixture is a file any reader accepts.

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// pageAt returns the header length and total size of the Ogg page at off.
func pageAt(t testing.TB, raw []byte, off int) (hdr, size int) {
	t.Helper()
	if off+27 > len(raw) || string(raw[off:off+4]) != "OggS" {
		t.Fatalf("no Ogg page at %d", off)
	}
	nsegs := int(raw[off+26])
	hdr = 27 + nsegs
	if off+hdr > len(raw) {
		t.Fatalf("page at %d is truncated", off)
	}
	size = hdr
	for _, n := range raw[off+27 : off+hdr] {
		size += int(n)
	}
	return hdr, size
}

// pageStarts lists every page offset in raw, plus the end of the last page.
func pageStarts(t testing.TB, raw []byte) []int {
	t.Helper()
	var out []int
	for off := 0; off+27 <= len(raw); {
		_, size := pageAt(t, raw, off)
		out = append(out, off)
		off += size
		if off > len(raw) {
			t.Fatalf("the last page runs past the file")
		}
		if off == len(raw) {
			out = append(out, off)
			break
		}
	}
	return out
}

// patchOggFLACTotal rewrites the 36-bit total-samples field in the BOS page's
// STREAMINFO and recomputes the page's CRC.
//
// The field is the last 36 bits of the 64-bit word at STREAMINFO offset 10, so
// it spans bytes 13 through 17 and the top nibble of byte 13 belongs to the bit
// depth; that nibble is preserved. The BOS packet begins right after the page
// header and STREAMINFO 17 bytes into it (the 0x7F FLAC signature, the version
// and header count, the fLaC marker, and the metadata block header).
func patchOggFLACTotal(t *testing.T, raw []byte, total int64) []byte {
	return patchOggFLACTotalB(t, raw, total)
}

func patchOggFLACTotalB(t testing.TB, raw []byte, total int64) []byte {
	t.Helper()
	if total < 0 || total >= 1<<36 {
		t.Fatalf("total %d does not fit FLAC's 36-bit field", total)
	}
	out := append([]byte(nil), raw...)
	hdr, size := pageAt(t, out, 0)
	si := hdr + 17
	if si+18 > size {
		t.Fatalf("the BOS page holds no STREAMINFO (page of %d bytes)", size)
	}
	f := si + 13
	out[f] = out[f]&0xF0 | byte(total>>32)&0x0F
	binary.BigEndian.PutUint32(out[f+1:f+5], uint32(total))

	// The page CRC is computed over the whole page with its own field zeroed.
	for i := 22; i < 26; i++ {
		out[i] = 0
	}
	binary.LittleEndian.PutUint32(out[22:26], crc32(0, out[:size]))
	return out
}

// dropLastPages truncates raw before its last n pages, which removes whole
// pages of audio and leaves every remaining page intact. What the file then
// declares is more than it holds.
func dropLastPages(t *testing.T, raw []byte, n int) []byte {
	t.Helper()
	starts := pageStarts(t, raw)
	if len(starts) < n+2 {
		t.Fatalf("the fixture has %d pages; cannot drop %d", len(starts)-1, n)
	}
	return raw[:starts[len(starts)-1-n]]
}

// probeTotal opens raw and returns its track and warnings.
func probeTotal(t *testing.T, raw []byte) (container.Track, []container.Warning) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d.Tracks()[0], d.Warnings()
}

// countKinds splits a warning list.
func countKinds(ws []container.Warning) (damage, notes int) {
	for _, w := range ws {
		if w.Kind == container.Damage {
			damage++
		} else {
			notes++
		}
	}
	return damage, notes
}

// TestOggFLACDeclaredTotalIsVerified: an Ogg-FLAC's STREAMINFO total is
// checked against the final page granule in both directions, exactly as the
// native FLAC and WavPack opens check theirs against the payload.
//
// Five outcomes, and the flag is the point of four of them: a length reported
// exact is one format.Media caps a decode at, so promoting one nothing checked
// is what would turn a file's own inconsistency into a truncation.
func TestOggFLACDeclaredTotalIsVerified(t *testing.T) {
	for _, name := range []string{"sine-s16.oga", "noise-s24.oga"} {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)

			// As committed: ffmpeg writes total 0, which is not a claim at all.
			// The granule answers, and the answer is exact now where it used to
			// be a length nothing had promised anything about.
			base, ws := probeTotal(t, raw)
			if base.Samples <= 0 || !base.SamplesExact {
				t.Fatalf("total 0: %d samples (exact %v), want the granule, exact",
					base.Samples, base.SamplesExact)
			}
			if len(ws) != 0 {
				t.Errorf("total 0: %v, want nothing to disagree with", ws)
			}
			granule := base.Samples

			t.Run("declared equal", func(t *testing.T) {
				tr, ws := probeTotal(t, patchOggFLACTotal(t, raw, granule))
				if tr.Samples != granule || !tr.SamplesExact {
					t.Errorf("%d samples (exact %v), want %d exact", tr.Samples, tr.SamplesExact, granule)
				}
				if len(ws) != 0 {
					t.Errorf("a total the granule confirms must report nothing: %v", ws)
				}
			})

			t.Run("declared low", func(t *testing.T) {
				tr, ws := probeTotal(t, patchOggFLACTotal(t, raw, granule-100))
				if tr.Samples != granule || !tr.SamplesExact {
					t.Errorf("%d samples (exact %v), want the granule %d exact",
						tr.Samples, tr.SamplesExact, granule)
				}
				damage, notes := countKinds(ws)
				if damage != 0 || notes != 1 {
					t.Errorf("%d damage, %d notes, want one note: a stream running past its own "+
						"total is inconsistent, not damaged: %v", damage, notes, ws)
				}
				// And a strict open still succeeds, which is what separates a
				// Note from a warning.
				if _, err := NewDemuxer(container.BytesSource(patchOggFLACTotal(t, raw, granule-100)),
					&DemuxerOptions{Strict: true}); err != nil {
					t.Errorf("a strict open refused an over-running stream: %v", err)
				}
			})

			t.Run("declared high", func(t *testing.T) {
				// The committed fixtures hold all their audio in one page, so
				// the truncation runs on a repaginated copy (one page per
				// frame) of the same stream. The cut is by whole pages, so
				// every page left is intact and the only thing wrong with the
				// file is that it declares more than it holds.
				paged, _ := repaginate(t, name, false)
				whole, _ := probeTotal(t, patchOggFLACTotal(t, paged, granule))
				if whole.Samples != granule {
					t.Fatalf("the repaginated stream holds %d samples, the fixture %d",
						whole.Samples, granule)
				}
				cut := dropLastPages(t, patchOggFLACTotal(t, paged, granule), 2)
				tr, ws := probeTotal(t, cut)
				if tr.Samples >= granule || !tr.SamplesExact {
					t.Errorf("%d samples (exact %v), want fewer than the declared %d, exact",
						tr.Samples, tr.SamplesExact, granule)
				}
				if got := readDelivers(t, cut); got != tr.Samples {
					t.Errorf("the open reports %d samples, a read delivers %d", tr.Samples, got)
				}
				damage, notes := countKinds(ws)
				if damage != 1 || notes != 0 {
					t.Errorf("%d damage, %d notes, want one damage warning: %v", damage, notes, ws)
				}
				if _, err := NewDemuxer(container.BytesSource(cut), &DemuxerOptions{Strict: true}); err == nil {
					t.Error("a strict open accepted a stream that declares more than it holds")
				}
			})
		})
	}
}

// readDelivers is the sample count a read of the stream hands out, from the
// packets' own timing.
func readDelivers(t *testing.T, raw []byte) int64 {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var end int64
	for {
		err := d.ReadPacket(&pkt)
		if err != nil {
			return end
		}
		end = pkt.PTS + pkt.Dur
	}
}

// TestReferenceOggFLACFixture is the real-producer cross-check for the branch
// above: `flac --ogg` is the only Ogg-FLAC producer available here that writes
// a nonzero STREAMINFO total (ffmpeg writes 0, and so did this tree until the
// muxer learned to stamp its projection), so the committed fixture is what
// says the "declared equal" row describes a file somebody actually writes
// rather than one this test patched into existence.
//
// Fixture recipe, from `testutil`'s sine (44.1 kHz stereo 16-bit, half a
// second) written to a WAV with `testutil.WriteWAV`:
//
//	flac --ogg -s -f -o container/ogg/testdata/ref-flac.oga in.wav
//
// checked with `flac -t`. The same assertions run against a fresh `flac --ogg`
// in tests/ogg_flac_reference_test.go, so the pin holds for the installed
// reference and not only for the committed bytes.
func TestReferenceOggFLACFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ref-flac.oga"))
	if err != nil {
		t.Fatal(err)
	}
	declared := declaredOggFLACTotal(t, raw)
	if declared == 0 {
		t.Fatal("the committed fixture declares total 0; it must come from a producer that states one")
	}
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.lastGranule(); got != declared {
		t.Errorf("the final page granule is %d, STREAMINFO declares %d", got, declared)
	}
	tr := d.Tracks()[0]
	if tr.Samples != declared || !tr.SamplesExact {
		t.Errorf("%d samples (exact %v), want the declared %d exact", tr.Samples, tr.SamplesExact, declared)
	}
	if ws := d.Warnings(); len(ws) != 0 {
		t.Errorf("the reference's own file disagrees with itself: %v", ws)
	}
}

// declaredOggFLACTotal reads the 36-bit total-samples field out of the BOS
// page's STREAMINFO, the inverse of patchOggFLACTotal.
func declaredOggFLACTotal(t testing.TB, raw []byte) int64 {
	t.Helper()
	hdr, size := pageAt(t, raw, 0)
	si := hdr + 17
	if si+18 > size {
		t.Fatal("the BOS page holds no STREAMINFO")
	}
	f := raw[si+13 : si+18]
	return int64(f[0]&0x0F)<<32 | int64(f[1])<<24 | int64(f[2])<<16 | int64(f[3])<<8 | int64(f[4])
}

// TestOggFLACUnknownLengthStaysUnknown: a stream that declares no total and
// offers no granule to find is unknown, not empty.
//
// The two are different claims and the wrong one is worse: -1 tells a caller to
// measure, while 0 tells it there is nothing to play. The Opus and Vorbis
// mappings report -1 in the same position, and so did this one before it
// verified anything.
func TestOggFLACUnknownLengthStaysUnknown(t *testing.T) {
	// A tail with no page of this serial: lastGranule answers -1, which is the
	// shape a stream whose pages complete no packet leaves behind.
	m := &flacMapping{}
	if _, err := m.parseID(flacBOSPacket()); err != nil {
		t.Fatal(err)
	}
	tr, err := m.finalizeTrack(func() int64 { return -1 }, nopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Samples != -1 || tr.SamplesExact {
		t.Errorf("samples = %d (exact %v), want -1 unknown", tr.Samples, tr.SamplesExact)
	}
	// And a declared total with no granule keeps the declaration, unflagged:
	// nothing was found to check it against.
	m2 := &flacMapping{}
	if _, err := m2.parseID(flacBOSPacket()); err != nil {
		t.Fatal(err)
	}
	m2.si.Samples = 4096
	tr2, err := m2.finalizeTrack(func() int64 { return -1 }, nopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Samples != 4096 || tr2.SamplesExact {
		t.Errorf("samples = %d (exact %v), want the declared 4096, unflagged", tr2.Samples, tr2.SamplesExact)
	}
}

// nopReporter drops what a mapping reports, for a cell driving finalizeTrack
// directly rather than through a demuxer.
type nopReporter struct{}

func (nopReporter) warn(int64, string, ...any) error { return nil }
func (nopReporter) note(int64, string, ...any)       {}

// TestOggFLACMuxAcceptsWhatItCanDemux: the muxer must not refuse a STREAMINFO
// this tree opens and decodes. Parsing is looser than marshalling (block bounds
// and the rate's upper end are checked only on the way out), so a round-trip
// through flac.StreamInfo would turn a readable source into a refusal at Begin;
// the mapping rewrites the one field it needs in the block's own bytes instead.
func TestOggFLACMuxAcceptsWhatItCanDemux(t *testing.T) {
	// MinBlock below 16 parses (only MaxBlock is floored) and does not marshal.
	cfg := flacBOSPacketBlocks(8, 4096)[17:]
	if len(cfg) != flacStreamInfoLen {
		t.Fatalf("the crafted STREAMINFO is %d bytes", len(cfg))
	}
	si, err := flac.ParseStreamInfo(cfg)
	if err != nil {
		t.Fatalf("this cell needs a STREAMINFO the parser accepts: %v", err)
	}
	if _, err := si.MarshalBinary(); err == nil {
		t.Skip("MarshalBinary now accepts this shape; the asymmetry this pins is gone")
	}
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	track := container.Track{Codec: codec.FLAC, CodecConfig: cfg, Fmt: si.PCMFormat(), Samples: 4096}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin refused a STREAMINFO the demuxer accepts: %v", err)
	}
	if err := m.End(codec.Trailer{Samples: 4096}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if got := declaredOggFLACTotal(t, out.Bytes()); got != 4096 {
		t.Errorf("the stamped total is %d, want 4096", got)
	}
}

// TestOggMuxerRefusesASecondEnd: End patches the BOS page now, so a second call
// would emit another EOS page and rewrite a header the first call had settled.
func TestOggMuxerRefusesASecondEnd(t *testing.T) {
	ws := &testutil.MemWriteSeeker{}
	m := NewMuxer(ws, nil)
	si := flacBOSPacketBlocks(4096, 4096)[17:]
	parsed, err := flac.ParseStreamInfo(si)
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.FLAC, CodecConfig: si, Fmt: parsed.PCMFormat(), Samples: 4096}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	if err := m.End(codec.Trailer{Samples: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := m.End(codec.Trailer{Samples: 4096}); err == nil {
		t.Error("a second End was accepted")
	}
	if err := m.Begin([]container.Track{track}); err == nil {
		t.Error("a second Begin was accepted")
	}
	if err := m.WritePacket(container.Packet{}); err == nil {
		t.Error("WritePacket after End was accepted")
	}
}

// TestOggFLACUnknownTrailerLengthDoesNotFail: codec.Trailer states -1 for a
// length its producer does not know, and a header a run cannot confirm is not a
// failure after the whole file is written. A seekable destination clears the
// claim to 0, FLAC's own "unknown", which every reader handles.
func TestOggFLACUnknownTrailerLengthDoesNotFail(t *testing.T) {
	si := flacBOSPacketBlocks(4096, 4096)[17:]
	parsed, err := flac.ParseStreamInfo(si)
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.FLAC, CodecConfig: si, Fmt: parsed.PCMFormat(), Samples: 4096}
	for _, tc := range []struct{ name string }{{"seekable"}, {"pipe"}} {
		t.Run(tc.name, func(t *testing.T) {
			ws := &testutil.MemWriteSeeker{}
			var buf bytes.Buffer
			var w io.Writer = &buf
			if tc.name == "seekable" {
				w = ws
			}
			m := NewMuxer(w, nil)
			if err := m.Begin([]container.Track{track}); err != nil {
				t.Fatal(err)
			}
			if err := m.End(codec.Trailer{Samples: -1}); err != nil {
				t.Fatalf("End with an unknown length: %v", err)
			}
			body := buf.Bytes()
			want := int64(4096)
			if tc.name == "seekable" {
				body, want = ws.Buf, 0
			}
			if got := declaredOggFLACTotal(t, body); got != want {
				t.Errorf("STREAMINFO declares %d samples, want %d", got, want)
			}
		})
	}
}
