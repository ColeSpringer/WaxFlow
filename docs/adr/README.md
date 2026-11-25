# Architecture Decision Records

Invariants pinned before the code that depends on them exists. An Accepted
ADR's text is never edited beyond its Status line: a change is a dated
amendment at its end naming the sentences it supersedes, or a superseding ADR.

| # | Title | Status |
|---|---|---|
| [0001](0001-clean-room-policy.md) | Clean-room policy for codec references | Accepted |
| [0002](0002-public-api-stability.md) | Public API surface, module layout, and stability policy | Accepted |
| [0003](0003-signing-and-source-identity.md) | Signed playback URLs and source identity | Accepted |
| [0004](0004-cache-key-format.md) | Cache key format | Accepted |
| [0005](0005-codec-interfaces.md) | Codec and container interface shape | Accepted |
| [0006](0006-buffer-pos-ownership.md) | Buffer.Pos ownership | Accepted |
| [0007](0007-tls-exposure-posture.md) | TLS and network exposure posture | Accepted |
| [0008](0008-opus-encoder-quality-gate-units.md) | Opus encoder-quality gate restated in weighted-error ratios | Accepted |
| [0009](0009-multi-source-timelines.md) | Multi-source timelines and their identity | Accepted |
| [0010](0010-timeline-width.md) | A timeline is built at the width it is delivered at | Accepted |
| [0011](0011-multi-span-packet-cuts.md) | Interior joins of a multi-span packet cut snap inward | Accepted |
| [0012](0012-live-pipelines-end-with-their-audience.md) | Live pipelines end when their audience does | Accepted |
| [0013](0013-copy-rungs.md) | The copy rungs | Accepted |
| [0014](0014-priming-a-segmented-run.md) | Priming a segmented run | Accepted |
