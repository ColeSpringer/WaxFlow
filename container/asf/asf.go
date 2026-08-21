// Package asf demuxes the Advanced Systems Format container, the .wma/.asf
// file Windows Media Audio ships in: a flat tree of GUID-tagged objects
// (header, data, index) whose Data Object holds a run of fixed-size packets,
// each carrying one or more payload fragments that reassemble into media
// objects. One media object is one codec packet, which for WMA is one
// super-frame.
//
// ASF names positions in milliseconds, not samples, so unlike the formats
// whose frames can be counted this container cannot state an exact sample
// position for anything: Track.Samples comes from the File Properties Object's
// play duration and SamplesExact stays false, and a packet's PTS is its
// millisecond presentation time converted at the track rate. That is also why
// seeking lands on a packet boundary at or before the target and backs up far
// enough to cover a whole media object: WMA's bit reservoir carries state
// across super-frames, so the landing owes the decoder run-up, and
// format.Media's pre-roll takes it from there.
//
// The package is named asf rather than wma because ASF is the container and
// WMA the codec inside it (the flacn and wv precedent). The public container
// name and error prefix are "wma" either way, since that is the driver name
// the registry advertises.
//
// Hostile-input invariants (ADR-0005): the object tree is walked, never
// recursed on attacker-chosen depth -- the Header Object's children are
// dispatched to named parsers and every unrecognized object, the Header
// Extension included, is treated as a leaf and skipped by its own declared
// size. Every size is validated against the bytes remaining before any slice
// or allocation, the header is read once into a bounded buffer, table and tag
// growth is capped, and each parse loop consumes input so none can spin.
package asf

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/waxerr"
)

// Hostile-input caps.
const (
	// maxHeaderBytes bounds the in-memory Header Object. Cover art rides in
	// the Extended Content Description, so this has to clear a few pictures;
	// past it the file is refused rather than parsed.
	maxHeaderBytes = 16 << 20
	// maxPacketBytes bounds one data packet. Real files use 3200 to 65536;
	// the field is 32-bit, so it needs a ceiling of its own.
	maxPacketBytes = 1 << 20
	// maxObjectBytes bounds one reassembled media object. A WMA super-frame
	// is a couple of kilobytes; this clears anything real and stops a crafted
	// media-object size from sizing an allocation.
	maxObjectBytes = 1 << 22
	// maxObjectSamples caps a packet's declared duration, for the tail object
	// whose length is arithmetic on a container-declared total.
	maxObjectSamples = 1 << 20
	// maxPayloads bounds the payload list built from one data packet. The
	// payload count field is six bits, so a packet holds at most 63 of them;
	// the ceiling is here for the compressed form, whose sub-objects are
	// bounded only by the packet length and so grow with it.
	maxPayloads = 1 << 12
	// maxStreams bounds the Stream Properties objects parsed from one header.
	// The stream number is seven bits, so 127 is the format's own ceiling.
	maxStreams = 128
	// maxTagBytes bounds one tag value, and maxTags the whole tag map.
	maxTagBytes = 1 << 16
	maxTags     = 1 << 10
	// maxIndexEntries bounds the Simple Index Object. One entry per second
	// clears days of audio; past it the index is dropped and seeking falls
	// back to bisection, which needs no table at all.
	maxIndexEntries = 1 << 20
	// maxWarnings caps the tolerated-damage list. Seeking re-walks spans a
	// linear read already crossed, so a damaged file would otherwise
	// accumulate the same warning without bound.
	maxWarnings = 64
	// maxPrerollMS bounds the File Properties pre-roll, which shifts every
	// presentation time in the file. Writers use a few seconds; a crafted one
	// past this is ignored rather than allowed to push the whole timeline.
	maxPrerollMS = 60_000
	// packetOverhead is what a data packet spends before its first payload's
	// bytes: the parsing information at its widest (error correction, packet
	// length, sequence, padding length, send time, duration, payload flags)
	// plus one payload header. Sizing a media object's packet span against
	// the packet length alone would put every object between this and the
	// full length in a gap where the span reads as one packet and is two.
	packetOverhead = 64
	// maxSeekPrerollPackets caps the run-up a seek backs over, so a stream
	// declaring an absurd media-object size cannot turn one seek into a walk
	// of the whole file.
	maxSeekPrerollPackets = 64
)

// Fixed on-wire sizes. Every ASF object opens with a 16-byte GUID and an
// 8-byte size; the Header and Data objects carry more before their bodies.
const (
	objectHeaderLen = 24
	headerObjectLen = 30 // + object count (4), reserved (1, 1)
	dataObjectLen   = 50 // + file ID (16), total packets (8), reserved (2)
)

// hundredNS is the tick ASF states durations in; preroll is in milliseconds.
const hundredNS = 10_000_000

// MatchNeed is how many leading bytes Match inspects.
const MatchNeed = 16

// Match reports whether head begins with the ASF Header Object GUID. It is the
// format sniff-table entry.
func Match(head []byte) bool {
	return len(head) >= MatchNeed && guidAt(head) == guidHeader
}

// malformed builds a parse error under the public container name. These
// messages reach users verbatim.
func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "wma: "+fmt.Sprintf(format, args...))
}

// msToSamples converts a millisecond position to a sample count at rate,
// rounded to nearest. It splits into whole seconds plus a remainder so a
// crafted timestamp cannot overflow the ms*rate product, the same shape mka's
// nsToSamples uses.
func msToSamples(ms int64, rate int) int64 {
	if ms <= 0 || rate <= 0 {
		return 0
	}
	sec := ms / 1000
	rem := ms % 1000
	return sec*int64(rate) + (rem*int64(rate)+500)/1000
}

// hnsToSamples is msToSamples for a 100-nanosecond duration, which is what the
// File Properties Object states play and send durations in. It reports false
// when the product would overflow, which needs a crafted duration or a sample
// rate no audio has: WAVEFORMATEX states the rate in 32 bits and
// audio.Format only requires it to be positive, so the two together reach
// past int64 and a wrapped total would be a negative length that is not the
// unknown-length sentinel.
func hnsToSamples(hns int64, rate int) (int64, bool) {
	if hns <= 0 || rate <= 0 {
		return 0, true
	}
	sec := hns / hundredNS
	rem := hns % hundredNS
	if sec > (math.MaxInt64-int64(rate))/int64(rate) {
		return 0, false
	}
	return sec*int64(rate) + (rem*int64(rate)+hundredNS/2)/hundredNS, true
}
