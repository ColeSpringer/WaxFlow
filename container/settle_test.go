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
// agree with.
type rawDemuxer struct {
	track container.Track
	raw   int64
	pos   int64
}

func (d *rawDemuxer) Tracks() []container.Track { return []container.Track{d.track} }

func (d *rawDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.pos >= d.raw {
		return io.EOF
	}
	n := min(int64(1000), d.raw-d.pos)
	*pkt = container.Packet{Track: 0, Packet: codec.Packet{
		Data: make([]byte, n*2),
		PTS:  d.pos,
		Dur:  n,
		Sync: true,
	}}
	d.pos += n
	return nil
}

// delivered is how many samples a Media over track reads out of raw raw
// samples: the number a settled length is a claim about.
func delivered(t *testing.T, track container.Track, raw int64) int64 {
	t.Helper()
	med, err := format.FromDemuxer("synthetic", &rawDemuxer{track: track, raw: raw})
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
