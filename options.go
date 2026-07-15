package waxflow

import (
	"log/slog"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
)

// Option configures an Engine.
type Option func(*Engine)

// WithLogger sets the Engine's logger. Nil (and the default) discards.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// IndexCache persists demuxer-built source indexes across sessions (the
// cacheDir/idx sidecar): MP3 frame tables today, seek tables for later
// formats. The engine restores a cached index when it opens a source
// whose demuxer can use one, and saves fresh snapshots on close. Keying
// blobs by source identity is the implementation's job (the server keys
// by ref plus size plus mtime); the engine stays identity-agnostic.
type IndexCache interface {
	// Load returns the saved index blob for src, or nil.
	Load(src container.Source) []byte
	// Save persists a fresh snapshot for src. Best effort: failures are
	// the implementation's to swallow (a lost sidecar only costs a
	// rebuild).
	Save(src container.Source, blob []byte)
	// Drop removes src's saved blob. The engine calls it when a demuxer
	// rejects a loaded blob, so an invalid one stops being served (and
	// LRU-refreshed) forever.
	Drop(src container.Source)
}

// WithIndexCache wires an index sidecar cache into the Engine.
func WithIndexCache(c IndexCache) Option {
	return func(e *Engine) {
		e.idx = c
	}
}

// TranscodeOptions selects the Transcode output, with the DSP chain
// (resample, mix, gain, dither) between decode and encode. Zero values keep
// the source's properties, so the zero options add no DSP stage and the
// decoder's samples reach the encoder unaltered: a bit-exact container
// rewrite for a lossless source to a lossless output. A lossy source is
// decoded and re-encoded even so, which costs a generation.
//
// Remux is what removes that generation, and the options it accepts are exactly
// the ones described above: zero everywhere but Format and Container. It moves
// the source's own packets rather than decoding them, so it is a bit-exact
// container rewrite for a lossy source too, but only where the codec survives
// the trip (the output format's codec must already be the source's). PlanRemux
// answers whether a given request is one of those, and the server's ladder asks
// it before reaching for a transcode.
type TranscodeOptions struct {
	// Format is the output format name: "wav", "aiff", "flac", "mp3",
	// "alac", "aac", "opus", or "vorbis".
	Format string
	// Container overrides the format's default container where the
	// format defines an alternative; empty selects the default. Today
	// only aac has one: "adts" replaces the progressive fragmented MP4
	// with the raw ADTS elementary stream, a legacy opt-out that
	// sacrifices gapless signaling (ADTS has none).
	Container string
	// Rate resamples to this sample rate in Hz; 0 keeps the source rate.
	Rate int
	// Channels converts the channel count (downmix to 1 or 2, or mono
	// duplication to stereo); 0 keeps the source layout.
	Channels int
	// BitDepth forces integer output at this depth, dithered when
	// reducing; 0 keeps the source domain and depth.
	BitDepth int
	// GainDB applies a scalar gain, finite within +-120 dB. Positive
	// gain engages the true-peak limiter; tighter policy clamps (the
	// HTTP +12 dB bound) live at the API boundary, not here.
	GainDB float64
	// Dynamics applies a dynamics-processing preset to the post-gain
	// signal: gain.PresetOff (the zero value) applies none, gain.PresetVoice
	// the spoken-word leveller. It is a closed vocabulary rather than raw
	// compressor parameters; see gain.Preset for why.
	//
	// It composes with GainDB rather than replacing it, and the order is
	// load-bearing: the preset's curve has a fixed threshold, so the caller
	// levels the signal to a known point with GainDB first and the preset
	// then shapes it. A caller with a measured loudness (an analyze job's)
	// sends the exact dB alongside the preset. WaxFlow cannot measure a
	// live stream, so it cannot do this for the caller: two-pass is
	// jobs-only.
	//
	// A preset always engages the true-peak limiter.
	Dynamics gain.Preset
	// FromSample starts output at this source-timeline sample, seeking
	// sample-exact before the first chunk. Seconds convert to samples at
	// the API boundary (ADR-0006); 0 starts at the beginning.
	FromSample int64
	// FLACLevel selects the FLAC compression level for flac output: 1
	// through 8 literally, FLACLevelDefault (the zero value) for the
	// encoder default, and FLACLevelFastest for level 0, which needs a
	// sentinel because the zero value cannot mean it without stealing
	// the default. Levels trade encode speed for size and never affect
	// decoded audio.
	FLACLevel int
	// MP3Bitrate selects the constant bit rate in bits per second for mp3
	// output; the zero value uses the encoder default (128000). It must be
	// a legal Layer III CBR rate for the output sample rate. Under MP3VBR
	// it anchors the quality level instead.
	MP3Bitrate int
	// MP3VBR selects variable bit rate for mp3 output: each frame carries
	// the smallest legal bit-rate index that holds its psychoacoustic
	// demand, anchored at MP3Bitrate. The zero value is constant bit rate.
	MP3VBR bool
	// OpusBitrate selects the target bit rate in bits per second for opus
	// output; the zero value uses the encoder default (96000).
	OpusBitrate int
	// AACBitrate selects the target bit rate in bits per second for aac
	// output; the zero value uses the encoder default (128000). AAC
	// frames are variable-size, so the encoder holds the long-term mean
	// at the target with a bit reservoir.
	AACBitrate int
	// OpusComplexity gates the Opus encoder's analysis depth: 1 through 10
	// literally, OpusComplexityDefault (the zero value) for the encoder
	// default (5), and OpusComplexityLowest for complexity 0, which needs a
	// sentinel because the zero value cannot mean it without stealing the
	// default. Higher is slower and higher quality.
	OpusComplexity int
	// OpusVBR selects variable bit rate for opus output, sizing each frame to its
	// content around OpusBitrate. The zero value is constant bit rate.
	OpusVBR bool
	// OpusSignal hints the opus encoder about the content type: "voice"
	// biases the speech/music mode decision toward SILK/hybrid (audiobooks,
	// podcasts), "music" toward CELT. The zero value ("" or "auto") lets the
	// encoder's analyser decide per frame.
	OpusSignal string
	// VorbisQuality selects VBR quality for vorbis output on libvorbis's -q
	// scale (-1..10); higher is larger and better. The zero value uses the
	// encoder default (3.0). Vorbis is natively quality-driven, so this is the
	// primary knob; a small nonzero value near 0 reaches the lowest qualities
	// (the zero value cannot, matching the "0 means default" idiom).
	VorbisQuality float64
	// VorbisBitrate is a reserved ABR target in bits per second for vorbis
	// output. ABR rate control is not implemented, so a nonzero value is
	// rejected at plan time rather than silently ignored; leave it 0 for
	// quality-driven VBR.
	VorbisBitrate int
	// Shaping selects the dither strategy for quantization; the default
	// is flat TPDF.
	Shaping dither.Shaping
	// ResampleProfile selects resampler quality; empty means resample.HQ.
	ResampleProfile resample.Profile
	// Tags embeds canonical metadata fields (TITLE, ARTIST, ...) in the
	// output where the muxer can represent them in its stream form: Ogg
	// OpusTags, a FLAC VORBIS_COMMENT block, an MP3 ID3v2 tag, MP4 ilst
	// atoms. Formats without stream-form tagging (WAV, AIFF, ADTS)
	// ignore them; a finished file gets full metadata from the mapping
	// post-pass instead. Tags never change the plan: callers keying
	// cached bytes must fold the tag values into their own key.
	Tags []container.Tag
	// Chapters embeds chapter markers. Only the MP4 muxer represents
	// them (Nero chpl); the mapping post-pass covers finished files of
	// the other formats.
	Chapters []container.Chapter
	// Art embeds cover art. Only the MP4 muxer represents it (the ilst
	// covr atom); art inflates the pre-audio init header, so live
	// streams should leave it nil.
	Art *container.Picture
	// Progress, when non-nil, is called after each encoded chunk with
	// the encoder-input samples consumed so far and the projected total
	// (-1 unknown). It runs on the transcoding goroutine, so blocking it
	// pauses the pipeline; the job runner's yield-to-live-streams check
	// rides on exactly that.
	Progress func(done, total int64)
}

// FLACLevel spellings whose meaning the zero value cannot carry.
const (
	// FLACLevelDefault keeps the encoder's default compression level.
	FLACLevelDefault = 0
	// FLACLevelFastest selects FLAC level 0.
	FLACLevelFastest = -1
)

// OpusComplexity spellings whose meaning the zero value cannot carry.
const (
	// OpusComplexityDefault keeps the encoder's default complexity.
	OpusComplexityDefault = 0
	// OpusComplexityLowest selects complexity 0.
	OpusComplexityLowest = -1
)

// ProbeOptions configures Engine.Probe.
type ProbeOptions struct {
	// Strict turns tolerated input damage into errors.
	Strict bool
}
