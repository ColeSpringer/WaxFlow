# Third-party notices

Attributions for permissively licensed (Tier A) code studied closely or
ported into WaxFlow, per [ADR-0001](docs/adr/0001-clean-room-policy.md).
Module dependencies (e.g. spf13/cobra) carry their own licenses in the
module cache and are not vendored here.

Entries follow this format:

> **component**: derived from *project* (license), upstream URL, commit.

> **codec/flac encoder strategy**: the analysis design (Tukey
> apodization defaults, precision-15 coefficient quantization with error
> feedback, mean-based Rice parameter estimation, and the level presets'
> block/order/partition shape) was studied from *libFLAC* (BSD-3-Clause),
> https://github.com/xiph/flac, as permitted for Tier A sources. The
> implementation is original code written against RFC 9639; no source
> was ported line-by-line.

> **codec/mp3 decoder**: the granule pipeline structure and the ported
> table data (the Huffman tree tables in huffman.go and the ISO Table
> B.3 synthesis window in synthwin.go) derive from the public-domain
> PDMP3 via *hajimehoshi/go-mp3* (Apache-2.0),
> https://github.com/hajimehoshi/go-mp3, v0.3.4. The low sampling
> frequency handling (MPEG-2/2.5 scalefactor decoding, intensity stereo,
> band tables, and band-edge behavior) was ported from *minimp3* (CC0),
> https://github.com/lieff/minimp3. go-mp3 also serves as a test-only
> differential oracle per the testing policy; it is never imported by
> the public tree or the runtime pipeline.

> **codec/mp3 encoder**: original code written against ISO 11172-3 /
> 13818-3 and textbook filterbank/MDCT theory. It introduces no new
> third-party data: the forward Huffman tables are derived at init from
> the decoder's tree tables (attributed above), and the polyphase
> analysis window is derived from the synthesis window (attributed
> above). *Shine* (LGPL, Tier B) is used only as a black-box quality
> oracle through `ffmpeg -c:a libshine`; its source was not consulted.
