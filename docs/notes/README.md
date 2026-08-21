# Analysis notes

Black-box artifacts from ADR-0001 analysis passes.

A Tier B reference (LGPL/GPL: ffmpeg, faad, LAME, Shine) is never open while
the corresponding component is being implemented. When one is the only
available description of a format, a **separate** session studies it and
writes down what it found, and the implementation session consumes only what
is written here. That session order is the mitigation, so it is kept visible
in history: the notes land first, in their own change, before implementation
starts.

What belongs here: behavioural descriptions of a format, parameter tables that
are too large to carry in prose (those go to `codec/*/tables_*.go` as data-only
files, described here), oracle recipes, and measurements taken by running a
reference binary. What does not: anything copied from a reference's source, and
anything an implementation could paraphrase back into that source's structure.

Each file states which component it serves and what an implementation session
is allowed to assume from it. `MAINTENANCE.md` carries the reference ledger
row for the same pass, and `THIRD-PARTY-NOTICES.md` carries the attribution.

| File | Component | Pass |
|---|---|---|
| `wma-bitstream.md` | `codec/wma` | 2026-08-20 |
| `wma-oracle-corpus.md` | `codec/wma` | 2026-08-20 |
