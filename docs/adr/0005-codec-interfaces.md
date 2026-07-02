# ADR-0005: Codec and container interface shape

Status: Accepted (2026-07-02)

## Context

Six encoders, eight decoders, and eight containers meet in one pipeline.
Each codec appears in at least two containers, so any per-pair glue
multiplies. These interfaces are the contract every codec implements
against; changing them mid-stream re-opens finished work.

## Decision

**Everything is demuxer -> packets -> packet-decoder**, even self-framing
formats (`mpa`, `adts`, `flacn`): one uniform packet model instead of NxM
glue.

```go
package codec
type ID string  // "pcm","flac","alac","mp3","aac-lc","opus","vorbis"
type Packet struct { Data []byte; PTS, Dur int64; Sync bool }  // PTS/Dur in samples
type Trailer struct { Samples, Delay, Padding int64 }          // gapless finalization
type Decoder interface {
    Decode(pkt []byte, emit func(*audio.Buffer) error) error   // 0..n buffers per packet
    Drain(emit func(*audio.Buffer) error) error                // flush latency at EOF
    Reset()                                                    // after seeks
}
type Encoder interface {
    InputFormat() audio.Format
    FrameSize() int
    Encode(src *audio.Buffer, emit func(Packet) error) error
    Finish(emit func(Packet) error) (Trailer, error)
    CodecConfig() []byte   // ASC / OpusHead / STREAMINFO / magic cookie
}
```

- **Emitted buffers are borrowed**: valid only during the `emit` callback;
  the caller owns pooling. Symmetric for Decoder and Encoder.
- Import DAG, no cycles: `audio` <- `codec` <- `container` <- `format`.
  Compressed-domain types live in `codec`; `container` wraps them with track
  routing; `codec` never imports `container`.
- `container.Source` is `io.ReaderAt + Size()`; uploads and pipes spool
  first (moov-at-end reality).
- Muxers are **single-audio-track by design** (track selection happens
  upstream); `End` takes that track's `Trailer`. Muxers needing back-patch
  declare `NeedsSeek()` and receive an `io.WriteSeeker`; the engine gives
  jobs a file and refuses live streams.
- Gapless metadata flows in as `Track.Delay/Padding`, out via
  `Encoder.Finish -> Trailer -> Muxer.End`.
- Every demuxer/parser obeys the hostile-input invariants: bounded nesting
  depth, size-validate-before-allocate, metadata allocation caps, and a
  strict progress guarantee (every parse-loop iteration consumes input).
  Demuxers default tolerant (bounded resync, capped skip-unknown, structured
  warnings); strict mode exists for conformance tests.

## Consequences

- Positions and durations are int64 sample counts everywhere (see
  ADR-0006); float seconds exist only at the HTTP boundary.
- The uniform packet model makes the direct-play/transmux/transcode ladder
  nearly free: transmux is demux -> re-mux with no PCM materialized.
- Interface changes after the first codec lands require a superseding ADR
  and a sweep of every landed codec.
