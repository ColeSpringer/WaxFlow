package waxflow_test

// WavPack conformance against the official WavPack test suite (wavpack.com,
// "Decennial update 2.0"), the acceptance gate for the decoder: bit-exact
// decodes of everything in scope, and a named refusal for everything outside
// it. The suite is SHA-256-pinned and fetched by `make verify-vectors`; tests
// self-skip until then, and WAXFLOW_REQUIRE_VECTORS=1 escalates skips to
// failures.
//
// The split below is the scope statement made executable. Every .wv in the
// archive appears in exactly one of the tables, enforced by
// TestWavPackSuiteIsFullyClassified rather than promised in a comment, so a
// decoder that quietly started accepting hybrid streams, or quietly stopped
// decoding 20-bit ones, fails here rather than in a user's library. A suite
// update that adds a file fails the same way, unclassified.

import (
	"archive/zip"
	"errors"
	"io"
	pathpkg "path"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// wavpackSuite is the pinned archive's vector name.
const wavpackSuite = "wavpack/test_suite.zip"

// wavpackSupported are the suite members inside this decoder's scope; each
// must decode bit-exactly. The parenthetical says what the file is for, since
// the suite's own readmes are not fetched with the vectors.
var wavpackSupported = []string{
	"bit_depths/8bit.wv",
	"bit_depths/12bit.wv",     // 4 stripped LSBs: the shift path
	"bit_depths/16bit.wv",     //
	"bit_depths/20bit.wv",     // 4 stripped LSBs in a 24-bit container
	"bit_depths/24bit.wv",     //
	"bit_depths/32bit_int.wv", // the extended-integer path and its wvx stream
	"num_channels/mono-1.wv",
	"num_channels/stereo-2.wv",
	"special_cases/custom_srate.wv",   // 36 kHz, which rides in a metadata block
	"special_cases/stereo_mixing.wv",  // joint, true, and false stereo in one file
	"special_cases/redundant_bits.wv", // redundant LSBs of all three kinds
	"speed_modes/fast.wv",
	"speed_modes/default.wv",
	"speed_modes/high.wv",
	"speed_modes/vhigh.wv",
	"legacy/vers-40.wv",  // the oldest stream version still in scope
	"legacy/vers-480.wv", // cover art in the APEv2 tag
}

// wavpackRefused are the suite members outside this decoder's scope. Each must
// be refused with a message naming the situation, not a parse failure: these
// are all perfectly good WavPack files.
var wavpackRefused = map[string]string{
	"bit_depths/1bit_dsd.wv":       "dsd",
	"bit_depths/32bit_float.wv":    "float",
	"corruption/dsd_corrupt.wv":    "dsd",
	"corruption/hybrid_corrupt.wv": "hybrid",
	"hybrid_bitrates/24kbps.wv":    "hybrid",
	"hybrid_bitrates/32kbps.wv":    "hybrid",
	"hybrid_bitrates/48kbps.wv":    "hybrid",
	"hybrid_bitrates/64kbps.wv":    "hybrid",
	"hybrid_bitrates/128kbps.wv":   "hybrid",
	"hybrid_bitrates/160kbps.wv":   "hybrid",
	"hybrid_bitrates/256kbps.wv":   "hybrid",
	"hybrid_bitrates/320kbps.wv":   "hybrid",
	"hybrid_bitrates/384kbps.wv":   "hybrid",
	"hybrid_bitrates/512kbps.wv":   "hybrid",
	"hybrid_bitrates/1024kbps.wv":  "hybrid",
	"special_cases/hybrid_test.wv": "hybrid",
	"special_cases/cue_sheet.wv":   "hybrid",
	"legacy/vers-40-hybrid.wv":     "hybrid",
	"legacy/vers-480-hybrid.wv":    "hybrid",
	// The 5.1 file is lossy as well as multichannel, and the block flags say
	// so first; the channel refusal has its own cell over a lossless
	// multichannel fixture in TestWavPackRefusalsNamed.
	"num_channels/multichannel-6.wv": "hybrid",
}

// wavpackLegacyWAV are the suite's pre-4.0 members. They are a different
// bitstream and out of scope, and they never reach this decoder to be refused:
// WavPack before 4.0 prepended the source WAV header, so the sniff table
// resolves them as RIFF, exactly as the suite readme warns every WAV-based
// player does. Pinning that keeps the split honest about where they land.
var wavpackLegacyWAV = []string{
	"legacy/vers-10.wv",
	"legacy/vers-20.wv",
	"legacy/vers-20-lossy.wv",
	"legacy/vers-30.wv",
	"legacy/vers-30-fast.wv",
	"legacy/vers-30-lossy.wv",
	"legacy/vers-397.wv",
	"legacy/vers-397-hybrid.wv",
}

// wavpackUnreadable are the suite members no driver resolves at all. The
// self-extracting sample is a Windows executable with a WavPack stream buried
// in it, which the sniff table is right to decline.
var wavpackUnreadable = []string{
	"legacy/vers-480-sfx.wv",
}

// wavpackDamaged are the two deliberately corrupted members, each with its own
// expected behavior; TestWavPackSuiteCorruption spells them out.
var wavpackDamaged = []string{
	"corruption/lossless_corrupt.wv",
	"corruption/bad_checksums.wv",
}

// suiteFile reads one member of the pinned suite archive.
func suiteFile(t *testing.T, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(testutil.VectorPath(t, wavpackSuite))
	if err != nil {
		t.Fatalf("opening the WavPack test suite: %v", err)
	}
	defer zr.Close()
	full := pathpkg.Join("test_suite", name)
	for _, f := range zr.File {
		if f.Name != full {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", name, err)
		}
		defer rc.Close()
		raw, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return raw
	}
	t.Fatalf("the WavPack test suite has no member %s", full)
	return nil
}

// TestWavPackSuiteBitExact decodes every in-scope suite member and compares it
// sample-for-sample with ffmpeg.
func TestWavPackSuiteBitExact(t *testing.T) {
	for _, name := range wavpackSupported {
		t.Run(name, func(t *testing.T) {
			raw := suiteFile(t, name)
			// ffmpeg reads from a file, so the member is spilled once and
			// both decoders read the same bytes.
			path := writeTemp(t, pathpkg.Base(name), raw)
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "wv")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			ref := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(got), ref); idx != -1 {
				t.Fatalf("decode differs from ffmpeg at interleaved sample %d (got %d, ref %d)",
					idx, got.N*got.Fmt.Channels, len(ref))
			}
		})
	}
}

// TestWavPackSuiteSourceBitDepth pins what probe reports for a stream whose
// coded depth is narrower than the words it decodes into. The samples come
// back in the container width, as every other WavPack decoder presents them,
// so Fmt.BitDepth is that width; the source's own depth rides in
// SourceBitDepth, which is what probe prints.
func TestWavPackSuiteSourceBitDepth(t *testing.T) {
	for _, tc := range []struct {
		name              string
		width, sourceBits int
	}{
		{"bit_depths/12bit.wv", 16, 12},
		{"bit_depths/20bit.wv", 24, 20},
		{"bit_depths/16bit.wv", 16, 0}, // the two agree, so nothing to record
		{"bit_depths/24bit.wv", 24, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := waxflow.New().Probe(container.BytesSource(suiteFile(t, tc.name)), "wv", nil)
			if err != nil {
				t.Fatal(err)
			}
			tr := info.Default()
			if tr.Fmt.BitDepth != tc.width {
				t.Errorf("Fmt.BitDepth = %d, want the %d-bit container width", tr.Fmt.BitDepth, tc.width)
			}
			if tr.SourceBitDepth != tc.sourceBits {
				t.Errorf("SourceBitDepth = %d, want %d", tr.SourceBitDepth, tc.sourceBits)
			}
		})
	}
}

// TestWavPackSuiteRefusals checks that every out-of-scope suite member is
// refused, and that the refusal names the situation.
func TestWavPackSuiteRefusals(t *testing.T) {
	for name, want := range wavpackRefused {
		t.Run(name, func(t *testing.T) {
			raw := suiteFile(t, name)
			_, err := decodeAllDynamic(t, container.BytesSource(raw), "wv")
			if err == nil {
				t.Fatalf("decoded an out-of-scope file (%s) that must be refused", want)
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("error code = %v, want unsupported-format", code)
			}
			if !strings.Contains(strings.ToLower(err.Error()), want) {
				t.Errorf("refusal %q does not name the situation (%q)", err, want)
			}
		})
	}
}

// TestWavPackSuiteIsFullyClassified holds the tables above to their claim:
// every .wv the archive carries is named in exactly one of them. Without this
// the split is a comment, and the eleven hybrid-bitrate members it originally
// omitted were exactly the kind of thing that hides in one.
func TestWavPackSuiteIsFullyClassified(t *testing.T) {
	zr, err := zip.OpenReader(testutil.VectorPath(t, wavpackSuite))
	if err != nil {
		t.Fatalf("opening the WavPack test suite: %v", err)
	}
	defer zr.Close()

	seen := map[string]int{}
	for _, name := range wavpackSupported {
		seen[name]++
	}
	for name := range wavpackRefused {
		seen[name]++
	}
	for _, group := range [][]string{wavpackLegacyWAV, wavpackUnreadable, wavpackDamaged} {
		for _, name := range group {
			seen[name]++
		}
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%s appears in %d tables, want exactly one", name, n)
		}
	}

	members := 0
	for _, f := range zr.File {
		name, ok := strings.CutPrefix(f.Name, "test_suite/")
		if !ok || !strings.HasSuffix(name, ".wv") {
			continue // readmes, and the .wvc correction files a refusal covers
		}
		members++
		if seen[name] == 0 {
			t.Errorf("suite member %s is in no table: classify it as supported, refused, "+
				"unreachable, or damaged", name)
		}
		delete(seen, name)
	}
	for name := range seen {
		t.Errorf("table entry %s names no suite member", name)
	}
	if members == 0 {
		t.Fatal("the archive holds no .wv members; the prefix or layout changed")
	}
	t.Logf("%d suite members, all classified", members)
}

// TestWavPackSuiteUnreadable pins the members no driver claims: the
// self-extracting executable, whose WavPack stream sits behind a PE header.
func TestWavPackSuiteUnreadable(t *testing.T) {
	for _, name := range wavpackUnreadable {
		t.Run(name, func(t *testing.T) {
			raw := suiteFile(t, name)
			if info, err := waxflow.New().Probe(container.BytesSource(raw), "wv", nil); err == nil {
				t.Fatalf("resolved as %q; a stream behind an executable header is not a .wv",
					info.Container)
			}
		})
	}
}

// TestWavPackSuiteLegacyResolvesAsWAV pins where the pre-4.0 members land: the
// wav driver, because they carry the original WAV header. They are out of
// scope either way, but the reason matters, since it is why they never appear
// in the refusal table.
func TestWavPackSuiteLegacyResolvesAsWAV(t *testing.T) {
	for _, name := range wavpackLegacyWAV {
		t.Run(name, func(t *testing.T) {
			raw := suiteFile(t, name)
			info, err := waxflow.New().Probe(container.BytesSource(raw), "wv", nil)
			if err != nil {
				t.Skipf("pre-4.0 sample does not probe at all: %v", err)
			}
			if info.Container == "wavpack" {
				t.Fatalf("a pre-4.0 stream resolved to the wavpack driver, which cannot decode it")
			}
			if info.Container != "wav" {
				t.Errorf("container = %q, want wav (the prepended source header)", info.Container)
			}
		})
	}
}

// TestWavPackSuiteCorruption pins how the two damaged files behave.
//
// lossless_corrupt.wv has random bytes sprayed through the audio, so a block
// stops checksumming and the decode fails cleanly: no panic, no runaway, an
// error rather than silent garbage.
//
// bad_checksums.wv is the other half of the pair: only the blocks own
// checksums are damaged, the audio under them is intact. Our integrity check
// is the header CRC over the decoded samples, not the version-5 block
// checksum, so the file decodes correctly rather than dropping out. That is
// the same behavior the suite readme describes for a version-4 decoder, and it
// is the right one here: the samples are provably right, and refusing them
// would lose good audio to a damaged checksum of a checksum.
func TestWavPackSuiteCorruption(t *testing.T) {
	t.Run(wavpackDamaged[0], func(t *testing.T) {
		raw := suiteFile(t, wavpackDamaged[0])
		got, err := decodeAllDynamic(t, container.BytesSource(raw), "wv")
		if err == nil {
			audio.Put(got)
			t.Fatal("a file with corrupt audio decoded without error")
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
			t.Errorf("error code = %v, want unsupported-format", code)
		}
		if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "wavpack: ") {
			t.Errorf("error %q does not carry the wavpack prefix", err)
		}
	})
	t.Run(wavpackDamaged[1], func(t *testing.T) {
		raw := suiteFile(t, wavpackDamaged[1])
		got, err := decodeAllDynamic(t, container.BytesSource(raw), "wv")
		if err != nil {
			t.Fatalf("intact audio under damaged block checksums must still decode: %v", err)
		}
		defer audio.Put(got)
		if got.N == 0 {
			t.Fatal("decoded no samples")
		}
	})
}

// TestWavPackSuiteBlockChecksums recomputes the reference encoder's own block
// checksum over the reference encoder's own blocks. Our encoder agreeing with
// our own fold proves nothing about whether libwavpack agrees, on the
// arithmetic or on which bytes it covers, and `wvunpack -v` refuses a block
// whose stored value does not match.
//
// Both widths the reference writes are here. The narrow two-byte form is not
// hypothetical: fifteen members of this suite carry it, and it is what the
// remux rung would have to restate when a hybrid or multichannel source's
// blocks pass through the muxer's length patch.
func TestWavPackSuiteBlockChecksums(t *testing.T) {
	for _, name := range []string{
		"bit_depths/12bit.wv",            // the four-byte form
		"hybrid_bitrates/128kbps.wv",     // the two-byte form
		"num_channels/multichannel-6.wv", // the two-byte form, several blocks a frame
	} {
		t.Run(name, func(t *testing.T) {
			raw := suiteFile(t, name)
			blocks := 0
			// The walk stops at the tag block every suite member ends with,
			// which is not a WavPack block and not this test's business.
			for off := int64(0); wavpack.Match(raw[off:]); blocks++ {
				h, err := wavpack.ParseBlockHeader(raw[off:])
				if err != nil {
					t.Fatalf("block %d at byte %d: %v", blocks, off, err)
				}
				if off+h.Size > int64(len(raw)) {
					t.Fatalf("block %d declares %d bytes, %d remain", blocks, h.Size, int64(len(raw))-off)
				}
				ok, present := wavpack.VerifyBlockChecksum(raw[off : off+h.Size])
				if !present {
					t.Fatalf("block %d carries no checksum; this member can no longer check the fold", blocks)
				}
				if !ok {
					t.Errorf("block %d at byte %d: our fold disagrees with the stored checksum", blocks, off)
				}
				off += h.Size
			}
			if blocks == 0 {
				t.Fatal("no blocks walked")
			}
			t.Logf("%d blocks checked", blocks)
		})
	}
}
