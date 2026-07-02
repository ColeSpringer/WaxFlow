// Package codec defines the compressed-domain types and the Decoder and
// Encoder interfaces every WaxFlow codec implements (ADR-0005). Everything
// is demuxer -> packets -> packet-decoder, even self-framing formats, so
// each codec meets each container through one uniform packet model.
//
// The package imports audio only; container wraps these types with track
// routing and codec never learns about containers.
package codec

import "github.com/colespringer/waxflow/audio"

// ID names a codec. IDs appear in probe output, /caps, and cache keys, so
// they are part of the public contract.
type ID string

const (
	PCM    ID = "pcm"
	FLAC   ID = "flac"
	ALAC   ID = "alac"
	MP3    ID = "mp3"
	AACLC  ID = "aac-lc"
	Opus   ID = "opus"
	Vorbis ID = "vorbis"
)

// Packet is one compressed unit as a codec defines it: a FLAC frame, an
// MP3 frame, an Opus packet, a run of PCM frames. PTS and Dur are sample
// counts in the codec's output timeline; Sync marks packets a decoder can
// start from after a seek.
type Packet struct {
	Data []byte
	PTS  int64
	Dur  int64
	Sync bool
}

// Trailer carries gapless finalization from an encoder to a muxer:
// Samples is the true source length, Delay the encoder priming to trim
// from the front, Padding the trailing samples to trim.
type Trailer struct {
	Samples int64
	Delay   int64
	Padding int64
}

// Decoder turns packets into PCM buffers.
//
// Emitted buffers are borrowed: they are valid only during the emit
// callback and the decoder may reuse them immediately after it returns.
// Decoders never stamp Buffer.Pos; position authority belongs to
// format.Media (ADR-0006).
type Decoder interface {
	// Decode consumes one packet's payload and emits 0..n buffers (bit
	// reservoirs and decoder delay make packet-to-buffer non-1:1).
	Decode(pkt []byte, emit func(*audio.Buffer) error) error
	// Drain flushes decoder latency at end of stream.
	Drain(emit func(*audio.Buffer) error) error
	// Reset discards internal state after a seek.
	Reset()
}

// Releaser is optionally implemented by decoders and encoders that hold
// pooled scratch buffers. Release returns them to the pool; the codec
// must not be used afterward. format.Media calls it on Close, so codecs
// that borrow from audio's pool should implement it rather than leaving
// scratch to the garbage collector.
type Releaser interface {
	Release()
}

// Encoder turns PCM buffers into packets.
//
// Emitted packets are borrowed: Data is valid only during the emit
// callback.
type Encoder interface {
	// InputFormat is the exact PCM format Encode expects; the pipeline
	// converts upstream (for example opus: 48kHz float32).
	InputFormat() audio.Format
	// FrameSize is the encoder's native frame length in samples for the
	// framer stage to re-chunk to, or 0 when any length is accepted.
	FrameSize() int
	Encode(src *audio.Buffer, emit func(Packet) error) error
	// Finish flushes delayed packets and returns the gapless Trailer for
	// Muxer.End.
	Finish(emit func(Packet) error) (Trailer, error)
	// CodecConfig returns the out-of-band configuration blob the target
	// container stores (ASC, OpusHead, STREAMINFO, magic cookie).
	CodecConfig() []byte
}
