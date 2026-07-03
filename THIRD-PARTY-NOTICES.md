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
