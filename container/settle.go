package container

// SettleLength returns t with its length settled against raw, the raw
// samples a completed walk counted or a read produced: the number of samples
// the track's own codec emits before any gapless trim. The settled track is
// what format.Media delivers from those samples, and it is exact.
//
// One rule, three shapes, and the split between them is exactly the one
// format.Media's raw-end cap makes (see rawEndFor): a capped decode delivers
// its declared length, an uncapped one delivers everything it decodes.
//
//   - No length, or an advisory one: no cap, and nothing to keep. The raw run
//     minus the trims the header stated is the kept audio by definition.
//   - A capped track, meaning one that signals a trim or states an
//     authoritative length: the declared length, clamped to what the raw run
//     can supply, with the remaining raw tail as the padding. On an intact
//     source the clamp does nothing and the padding is whatever the run holds
//     past the audio; on a truncated one the length shrinks to what the
//     packets hold.
//   - A bare count with no cap: the raw run, since nothing trims the delivery
//     and the walk has counted what the header only claimed.
//
// SamplesExact belongs in the capped arm and not the bare one, which is the
// case a reading of "trimmed" alone gets wrong. An Ogg Vorbis track states
// its playable length in the final page granule with no Delay and no Padding
// beside it, so the run decodes past it; treating that as a bare count would
// lengthen every remux of one to the raw run and emit the encoder's tail as
// audio. A FLAC or WAV leaves SamplesExact false precisely because its total
// can lie, so it stays in the bare arm and never grows a padding out of a
// container's own inconsistency.
//
// Delay is never touched: a run shorter than the front trim delivers nothing
// either way, and the trim is the codec's, not the run's.
func SettleLength(t Track, raw int64) Track {
	switch {
	case t.Samples < 0 || t.SamplesAdvisory:
		t.Samples = max(raw-t.Delay-t.Padding, 0)
	case t.SamplesExact || t.Delay > 0 || t.Padding > 0:
		t.Samples = max(min(t.Samples, raw-t.Delay), 0)
		t.Padding = max(raw-t.Delay-t.Samples, 0)
	default:
		t.Samples = raw
	}
	t.SamplesExact, t.SamplesAdvisory = true, false
	return t
}
