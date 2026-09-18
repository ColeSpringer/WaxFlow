package mpegframes

import (
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// DecoderDelay is the fixed Layer III decoder latency in samples (528 plus
// 1) that gapless trims add to the encoder delay signaled in the LAME tag;
// every mainstream decoder applies the same constant, so trimmed output
// lines up across implementations.
const DecoderDelay = 529

// Gapless reports the track length and the trims a metadata frame states,
// or (-1, 0, 0) for one that states nothing usable, which is also what a run
// with no metadata frame at all reports.
//
// The trims and the length are adopted together or not at all: the back trim
// is only expressible through a known length, and a front-trim-only stream
// would be neither raw nor gapless.
func (i VBRInfo) Gapless(spf int64) (samples, delay, padding int64) {
	switch {
	case i.Delay >= 0 && i.Frames > 0:
		// LAME gapless: the decoder's fixed latency joins the encoder
		// delay at the front and comes off the padding at the back.
		delay = i.Delay + DecoderDelay
		padding = max(i.Padding-DecoderDelay, 0)
		return max(i.Frames*spf-delay-padding, 0), delay, padding
	case i.Frames > 0:
		return i.Frames * spf, 0, 0
	}
	return -1, 0, 0
}

// Reader turns the walk into the packet stream a demuxer delivers: one
// packet per whole frame, at the sample timeline the frame index and the
// frame's own length give. Every container that carries these frames
// delivers them exactly this way, which is why the model is here and not
// copied into each one.
type Reader struct {
	w   *Walker
	cur int64 // next frame number ReadPacket delivers
}

// Reader returns the packet reader over a walk that has already begun. A walk
// that has not is unusable here, the way the zero Walker is: what one frame
// decodes to is the whole of this type's arithmetic and Begin is what settles
// it.
func (w *Walker) Reader() *Reader { return &Reader{w: w} }

// ReadPacket yields one whole frame. Packet data is reused across calls.
func (r *Reader) ReadPacket(pkt *container.Packet) error {
	f, err := r.w.Frame(r.cur)
	if err != nil {
		return err
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: f.Data,
			PTS:  r.cur * r.w.spf,
			Dur:  r.w.spf,
			Sync: f.Sync,
		},
	}
	r.cur++
	return nil
}

// SeekSample repositions to the frame a decode aiming at sample must begin
// at, and reports the sample that frame starts on. The landing precedes the
// target by the backoff Landing computes; the caller's pre-roll decodes and
// discards the rest, so the delivered position is sample-exact.
func (r *Reader) SeekSample(sample int64) (int64, error) {
	land, err := r.w.Landing(sample / r.w.spf)
	if err != nil {
		return 0, err
	}
	r.cur = land
	return land * r.w.spf, nil
}

// Walk implements the walking half of container.Walker for the owner: the
// index is finished, and every finding on the way is reported as reached.
func (r *Reader) Walk() error { return r.w.Complete() }

// Walked implements the other half: whether the index already reaches the
// end of the run.
func (r *Reader) Walked() bool { return r.w.Done() }

// SettleLength settles the owner's track against the walk: once the index
// reaches the end of the run, the run's frames are its length, and what the
// track reports is what a read of it delivers. Before that it is a no-op,
// and after it is idempotent, which is what lets the media wrappers call
// Walk as often as they like.
//
// The declared-count comparison runs here too, for the one path that
// finishes an index without walking to its end: a restore whose blob was
// adopted before this track was settled.
func (r *Reader) SettleLength(t *container.Track) error {
	if !r.w.Done() {
		return nil
	}
	if err := r.w.compare(); err != nil {
		return err
	}
	*t = container.SettleLength(*t, r.w.Frames()*r.w.spf)
	return nil
}

// Snapshot implements the serializing half of container.Indexer.
func (r *Reader) Snapshot() []byte { return r.w.Snapshot() }

// Restore implements the adopting half of container.Indexer. A walk that has
// already delivered frames declines, since a restored index would move them
// under the caller.
func (r *Reader) Restore(blob []byte) bool {
	if r.cur != 0 {
		return false
	}
	return r.w.Restore(blob)
}
