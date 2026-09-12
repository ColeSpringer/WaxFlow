// Package mp4 demuxes ISO base media files (ISO/IEC 14496-12) and their
// QuickTime kin: the .m4a/.m4b/.mp4/.mov family carrying AAC-LC, HE-AAC,
// ALAC, Opus, FLAC, MP3, or uncompressed PCM. It parses the moov box tree
// into per-track sample tables, selects the audio track, and hands
// format.Media codec packets read straight from mdat, with sample-exact
// seeking over the sample-to-chunk mapping.
//
// PCM arrives in both families' spellings (QuickTime's sowt/twos/NONE/raw
// fourccs, its in24/in32/fl32/fl64 set with an enda byte order, its version 2
// lpcm flags word, its "ms" entries carrying WAVE format tag 1 or 3, and
// ISO/IEC 23003-5's ipcm/fpcm with a pcmC box) and takes a
// second sample-table representation, because a PCM sample is one FRAME: a
// three-minute 48 kHz stereo track is 8.6 million of them, so the per-sample
// offset and size arrays every other codec gets would be a hundred megabytes
// of index. A byte-linear track is instead indexed per CHUNK, which is what
// the file itself is sized by, and one packet carries a working chunk of
// frames rather than one. The same shape makes its seeks exact with no
// pre-roll. PCM in a FRAGMENTED movie is named and skipped rather than read:
// that path is per-sample by construction and nothing writes one, so the track
// leaves the candidate set and a movie carrying a codec we do decode beside it
// still opens on that one. QuickTime's 'chan' channel layout is not read, so a
// multichannel track plays in audio.DefaultLayout's order, and says so.
//
// Gapless trims come from the iTunes iTunSMPB tag or an edit list, mapped
// onto Track.Delay/Padding so format.Media delivers the trimmed timeline.
// Nero (chpl) and QuickTime text chapter tracks are parsed and surfaced
// through Chapters for probe.
//
// The box parser is the service's widest attack surface, so it holds
// to the hostile-input invariants strictly: bounded nesting depth, every
// size validated against remaining input before any allocation, table and
// sample-count caps, and a progress guarantee on every parse loop. The
// whole moov is read into a bounded buffer and parsed from memory; mdat
// stays on disk and only sampled bytes are read.
package mp4

import (
	"github.com/colespringer/waxflow/waxerr"
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxDepth bounds box nesting. Real files nest moov>trak>mdia>minf>
	// stbl>stsd>wave, seven deep; the cap leaves headroom without letting
	// a crafted file recurse without bound.
	maxDepth = 32
	// maxMoovBytes bounds the in-memory moov buffer. A metadata box larger
	// than this is refused rather than read: sample tables for even a
	// day-long file are a few MiB, and cover art lives here too.
	maxMoovBytes = 64 << 20
	// maxTracks bounds the track count parsed from one movie.
	maxTracks = 1 << 10
	// maxSamples bounds the flattened sample count across a track, the
	// backstop behind the per-table payload bounds. A ten-hour audiobook
	// runs to a few million samples; this leaves two orders of headroom.
	maxSamples = 1 << 26
	// maxChapters bounds parsed chapter entries.
	maxChapters = 1 << 16
	// maxReserveChunks bounds what buildChunkIndex reserves up front. The
	// chunk count it would otherwise size by is the file's own number, and
	// append grows to the count the table really yields, so this costs a real
	// file one reservation and costs a crafted one nothing.
	maxReserveChunks = 1 << 16
	// maxDescriptorLen bounds one esds descriptor payload.
	maxDescriptorLen = 1 << 16
)

// brands that mark an ISO-BMFF or QuickTime file in the ftyp box. Match
// keys on the ftyp box itself rather than a fixed brand set, since the
// brand zoo (M4A, mp42, isom, qt, M4B, dash, ...) is open-ended; a track
// with an audio handler and a codec we know is the real gate, applied at
// parse time.

// Match reports whether head is an ISO base media file: a leading box
// whose type is ftyp. It is the format sniff-table entry. Some muxers
// emit a leading styp or a free/skip box before ftyp; the sniffer only
// needs the common case, and the ext hint covers the rest.
func Match(head []byte) bool {
	if len(head) < 8 {
		return false
	}
	// A plausible box: size at least 8 and no larger than a sane ftyp, then
	// the ftyp type. Guarding the size keeps random data with "ftyp" at
	// bytes 4..8 from matching.
	size := be32(head)
	if size < 8 || size > 1024 {
		return false
	}
	return string(head[4:8]) == "ftyp"
}

// MatchNeed is how many leading bytes Match inspects.
const MatchNeed = 8

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("mp4: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not, which is a thing a caller can act on. See
// [waxerr.Malformed] for the rule.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("mp4: ", format, args...)
}

func be16(b []byte) uint16 {
	return uint16(b[0])<<8 | uint16(b[1])
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func be64(b []byte) uint64 {
	return uint64(be32(b))<<32 | uint64(be32(b[4:]))
}
