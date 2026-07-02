# ADR-0002: Public API surface, module layout, and stability policy

Status: Accepted (2026-07-02)

## Context

The codecs are meant to be imported by anyone (WaxBin and WaxTap included),
which demands a stable, dependency-free public tree. But codec APIs will
churn heavily through v0.x, and multi-module repos tax every change with
tagging ceremony.

## Decision

- **Public packages**: root facade, `waxerr`, `audio`, `dsp/...`,
  `codec/...`, `container/...`, `format`, `source`, `server`, `client`.
  Everything else lives under `internal/`.
- **The public tree is stdlib-only.** `make depcheck` runs `go list -deps`
  over the public packages and fails CI on any non-stdlib import. Runtime
  third-party dependencies are confined to `cmd/` and `internal/`
  (`spf13/cobra`; later `colespringer/waxlabel`).
- **Single module through v1.0.** External importers of, say,
  `waxflow/codec/flac` inherit cobra in their module *graph* (never their
  binaries); that is acceptable during v0.x churn.
- **Structural extraction at v1.0**: when codec APIs freeze, the stdlib-only
  tree graduates to a nested module with an empty `require` block, so the
  guarantee becomes structural instead of script-enforced.
- **No compatibility promise before v1.0.0.** Tagged v0.x releases are
  usable snapshots, not contracts. At v1.0.0 every exported identifier in
  the public tree is semver-locked, preceded by a strict surface audit.
- Unfinished codecs compile and test but stay **unregistered** in `format`
  and `/caps`; the service never advertises what it cannot do.

## Consequences

- `depcheck`'s package list grows as public packages land; forgetting to add a
  new public package there is the main failure mode, so the Makefile derives
  the list from a single variable reviewed in PRs.
- `resolver/` is the one intentional nested module planned before v1.0,
  keeping `modernc.org/sqlite` out of the main module graph.
