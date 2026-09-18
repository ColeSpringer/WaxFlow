package container_test

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// rawDemuxer delivers exactly raw raw samples of one 16-bit mono track,
// whatever its declared length claims: the source a settled length has to
// agree with. lastPadding rides on the final packet, the way a container that
// states its tail trim per packet signals it.
type rawDemuxer struct {
	track       container.Track
	raw         int64
	lastPadding int64
	pos         int64
}

func (d *rawDemuxer) Tracks() []container.Track { return []container.Track{d.track} }

func (d *rawDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.pos >= d.raw {
		return io.EOF
	}
	n := min(int64(1000), d.raw-d.pos)
	d.pos += n
	pad := int64(0)
	if d.pos >= d.raw {
		pad = d.lastPadding
	}
	*pkt = container.Packet{Track: 0, Padding: pad, Packet: codec.Packet{
		Data: make([]byte, n*2),
		PTS:  d.pos - n,
		Dur:  n,
		Sync: true,
	}}
	return nil
}

// delivered is how many samples a Media over track reads out of raw raw
// samples: the number a settled length is a claim about.
func delivered(t *testing.T, track container.Track, raw int64) int64 {
	t.Helper()
	return deliveredTrimmed(t, track, raw, 0)
}

// deliveredTrimmed is delivered with a per-packet trim on the last packet.
func deliveredTrimmed(t *testing.T, track container.Track, raw, lastPadding int64) int64 {
	t.Helper()
	med, err := format.FromDemuxer("synthetic", &rawDemuxer{track: track, raw: raw, lastPadding: lastPadding})
	if err != nil {
		t.Fatalf("FromDemuxer: %v", err)
	}
	defer med.Close()
	dst := audio.Get(track.Fmt, 512)
	defer audio.Put(dst)
	var got int64
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		got += int64(dst.N)
	}
	return got
}

func pcmTrack(samples, delay, padding int64, exact, advisory bool) container.Track {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		panic(err)
	}
	return container.Track{
		Codec: codec.PCM, CodecConfig: blob,
		Fmt:             cfg.PCMFormat(48000, 1, audio.DefaultLayout(1)),
		Samples:         samples,
		Delay:           delay,
		Padding:         padding,
		SamplesExact:    exact,
		SamplesAdvisory: advisory,
		Default:         true,
	}
}

// TestSettleLengthIsWhatAReadDelivers is the whole contract of SettleLength:
// the number it puts on a track is the number a read of the same raw samples
// hands out. Both directions of the disagreement, both trim states, since the
// raw-end cap engages for a trimmed track and not for a bare one.
func TestSettleLengthIsWhatAReadDelivers(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		samples, delay, padding int64
		exact, advisory         bool
		raw                     int64
		wantSamples             int64
		wantPadding             int64
	}{
		// No length at all: the raw run minus the trims the header stated.
		{"unknown, no trims", -1, 0, 0, false, false, 48000, 48000, 0},
		{"unknown, primed", -1, 1105, 0, false, false, 48000, 46895, 0},

		// An advisory total is replaced outright, in either direction.
		{"advisory over the run", 48100, 0, 0, false, true, 48000, 48000, 0},
		{"advisory under the run", 47900, 0, 0, false, true, 48000, 48000, 0},

		// A trimmed track: the declared length stands while the run can
		// supply it, and the raw tail the cap discards is the padding.
		{"trimmed, intact", 45744, 1105, 1151, false, false, 48000, 45744, 1151},
		{"trimmed, run comes up short", 45744, 1105, 1151, false, false, 40000, 38895, 0},
		{"trimmed, run overruns", 45744, 1105, 1151, false, false, 50000, 45744, 3151},
		{"trimmed, run shorter than the delay", 45744, 1105, 1151, false, false, 500, 0, 0},

		// A bare count with no trim is not capped, so a read hands out the
		// whole run and the count follows it.
		{"bare count, run comes up short", 48000, 0, 0, false, false, 47000, 47000, 0},
		{"bare count, run overruns", 48000, 0, 0, false, false, 49000, 49000, 0},
		{"exact count confirmed", 48000, 0, 0, true, false, 48000, 48000, 0},
		// The Ogg Vorbis shape: the final page granule is the playable length
		// and the run decodes past it, with no trim beside it to say so. An
		// authoritative length caps the decode, so the tail is padding rather
		// than audio the settled track grows.
		{"exact count, run decodes past it", 48000, 0, 0, true, false, 48960, 48000, 960},
		{"exact count, run comes up short", 48000, 0, 0, true, false, 47000, 47000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := pcmTrack(tc.samples, tc.delay, tc.padding, tc.exact, tc.advisory)
			got := container.SettleLength(in, tc.raw)
			if got.Samples != tc.wantSamples || got.Padding != tc.wantPadding {
				t.Errorf("settled to %d samples / %d padding, want %d / %d",
					got.Samples, got.Padding, tc.wantSamples, tc.wantPadding)
			}
			if !got.SamplesExact || got.SamplesAdvisory {
				t.Errorf("settled flags exact=%v advisory=%v, want exact", got.SamplesExact, got.SamplesAdvisory)
			}
			if got.Delay != in.Delay {
				t.Errorf("Delay moved to %d, want %d", got.Delay, in.Delay)
			}
			if n := delivered(t, got, tc.raw); n != got.Samples {
				t.Errorf("a read of %d raw samples delivered %d, the settled track says %d",
					tc.raw, n, got.Samples)
			}
		})
	}
}

// TestSettleLengthAgreesWithTheUnsettledRead is the other half: for the
// shapes a walk actually meets, settling does not change what a read of the
// same bytes hands out. It is what lets format.Media refresh its own track
// mid-read without moving the position or the end.
func TestSettleLengthAgreesWithTheUnsettledRead(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		samples, delay, padding int64
		advisory                bool
		raw                     int64
	}{
		{"untagged run", -1, 0, 0, false, 48000},
		{"advisory total, run longer", 47900, 0, 0, true, 48000},
		{"advisory total, run shorter", 48100, 0, 0, true, 48000},
		{"LAME trims, intact", 45744, 1105, 1151, false, 48000},
		{"LAME trims, truncated", 45744, 1105, 1151, false, 40000},
		{"bare count, truncated", 48000, 0, 0, false, 47000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := pcmTrack(tc.samples, tc.delay, tc.padding, false, tc.advisory)
			before := delivered(t, in, tc.raw)
			if after := container.SettleLength(in, tc.raw).Samples; after != before {
				t.Errorf("settled to %d, the unsettled track delivered %d", after, before)
			}
		})
	}
}

// TestPerPacketPaddingIsWhatTheReadDrops is the delivery half of a container
// that states its tail trim per packet rather than as a total (Matroska's
// DiscardPadding): the trim is dropped as the packet decodes, so the read is
// gapless before anything has counted the payload, and settling with the same
// trim folded in does not move it.
//
// The two arms are the before and after of a walk. Uncapped, because an
// advisory total vetoes the raw-end cap, so the per-packet trim is the only
// thing bounding the delivery; settled, because the cap then lands on the same
// sample, which is what lets a Walk refresh a Media's track mid-read.
func TestPerPacketPaddingIsWhatTheReadDrops(t *testing.T) {
	for _, tc := range []struct {
		name           string
		samples, delay int64
		advisory       bool
		raw, padding   int64
	}{
		{"advisory total, trimmed tail", 48100, 0, true, 48000, 648},
		{"advisory total, primed and trimmed", 48100, 312, true, 48000, 648},
		{"no total at all", -1, 312, false, 48000, 648},
		{"the whole last packet trimmed", 48100, 0, true, 48000, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := pcmTrack(tc.samples, tc.delay, 0, false, tc.advisory)
			want := tc.raw - tc.delay - tc.padding
			if got := deliveredTrimmed(t, in, tc.raw, tc.padding); got != want {
				t.Errorf("an unsettled read delivered %d, want the raw run less both trims (%d)", got, want)
			}
			// What a walk does: the trims it summed become the track's
			// Padding, then the run settles the length.
			folded := in
			folded.Padding = tc.padding
			settled := container.SettleLength(folded, tc.raw)
			if settled.Samples != want {
				t.Errorf("settled to %d, the read delivered %d", settled.Samples, want)
			}
			if got := deliveredTrimmed(t, settled, tc.raw, tc.padding); got != want {
				t.Errorf("a settled read delivered %d, want %d: the cap and the "+
					"per-packet trim must land on the same sample", got, want)
			}
		})
	}
}

// TestPerPacketPaddingCannotOutrunItsPacket is the bound on the trim: it drops
// what this packet emitted and no more, so a file stating a trim longer than
// the frames it rides on cannot reach back into audio already delivered.
// Matroska's own DiscardPadding never exceeds its block, so this is the
// hostile-input arm rather than a shape a real encoder writes.
func TestPerPacketPaddingCannotOutrunItsPacket(t *testing.T) {
	const raw, packet = 48000, 1000 // rawDemuxer's packet size
	in := pcmTrack(-1, 0, 0, false, false)
	if got, want := deliveredTrimmed(t, in, raw, 5*packet), int64(raw-packet); got != want {
		t.Errorf("a trim of five packets on the last one delivered %d, want %d", got, want)
	}
}
